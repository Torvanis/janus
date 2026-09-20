package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/secgw"
	"github.com/torvanis/janus/internal/secgw/classify"
	"github.com/torvanis/janus/internal/store"
)

// Security Gateway administration: policies, bindings, term lists,
// violations, the effective-policy explainer and a dry-run endpoint. Every
// write flushes the config cache so the proxy sees it at once on this
// replica; other replicas converge within the cache TTL.
//
// Two reads are deliberately gated beyond ordinary admin: the full contents
// of a term list (the list is itself confidential) and the decrypted text
// of a violation. Both write an audit row on every read.

func (s *Server) mountSecgwRoutes(r chi.Router) {
	r.Get("/admin/secgw/overview", s.handleSecgwOverview)
	r.Get("/admin/secgw/policies", s.handleListSecgwPolicies)
	r.Post("/admin/secgw/policies", s.gateCreate("security policy", s.handleCreateSecgwPolicy))
	r.Get("/admin/secgw/policies/{id}", s.handleGetSecgwPolicy)
	r.Put("/admin/secgw/policies/{id}", s.handleUpdateSecgwPolicy)
	r.Delete("/admin/secgw/policies/{id}", s.handleDeleteSecgwPolicy)
	r.Get("/admin/secgw/bindings", s.handleListSecgwBindings)
	r.Post("/admin/secgw/bindings", s.gateCreate("policy binding", s.handleCreateSecgwBinding))
	r.Delete("/admin/secgw/bindings/{id}", s.handleDeleteSecgwBinding)
	r.Get("/admin/secgw/term-lists", s.handleListSecgwTermLists)
	r.Post("/admin/secgw/term-lists", s.gateCreate("term list", s.handleCreateSecgwTermList))
	r.Get("/admin/secgw/term-lists/{id}", s.handleGetSecgwTermList)
	r.Put("/admin/secgw/term-lists/{id}", s.handleUpdateSecgwTermList)
	r.Delete("/admin/secgw/term-lists/{id}", s.handleDeleteSecgwTermList)
	r.Post("/admin/secgw/term-lists/{id}/import", s.handleImportSecgwTermList)
	r.Get("/admin/secgw/term-lists/{id}/export", s.handleExportSecgwTermList)
	r.Get("/admin/secgw/violations", s.handleListSecgwViolations)
	r.Get("/admin/secgw/violations/{id}", s.handleGetSecgwViolation)
	r.Get("/admin/secgw/effective", s.handleSecgwEffective)
	r.Post("/admin/secgw/dry-run", s.handleSecgwDryRun)
	r.Get("/admin/secgw/classifiers", s.handleListSecgwClassifiers)
	r.Put("/admin/secgw/classifiers/{modelID}", s.handleSetSecgwClassifier)
	r.Get("/admin/secgw/rules", s.handleListSecgwRules)
}

// --- overview -----------------------------------------------------------------

func (s *Server) handleSecgwOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	since := time.Now().UTC().Add(-24 * time.Hour)
	counts, err := s.Store.SecgwViolationCounts(ctx, since)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	policies, err := s.Store.ListSecgwPolicies(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	bindings, err := s.Store.ListSecgwBindings(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	classifiers, err := s.Store.ListModels(ctx, store.ModelFilter{ClassifierOnly: true})
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	enabled := 0
	for _, p := range policies {
		if p.Enabled {
			enabled++
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":          len(bindings) > 0,
		"policies":         len(policies),
		"policies_enabled": enabled,
		"bindings":         len(bindings),
		"classifiers":      len(classifiers),
		"violations_24h":   counts,
		"protocols":        classify.Protocols(),
		"check_kinds":      store.SecgwCheckKinds(),
	})
}

// --- policies -----------------------------------------------------------------

func (s *Server) handleListSecgwPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := s.Store.ListSecgwPolicies(r.Context())
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

func (s *Server) handleGetSecgwPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.SecgwPolicyByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"policy": p})
}

func (s *Server) handleCreateSecgwPolicy(w http.ResponseWriter, r *http.Request) {
	var p store.SecgwPolicy
	if err := decodeJSON(r, &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if err := s.requireEnforceLicensed(&p); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.validateSecgwClassifierRefs(r.Context(), &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if u := UserFrom(r.Context()); u != nil {
		p.CreatedBy = u.ID
	}
	if err := s.Store.CreateSecgwPolicy(r.Context(), &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_policy_created", "secgw_policy", p.ID, nil, secgwPolicyAudit(&p))
	WriteJSON(w, http.StatusCreated, map[string]any{"policy": p})
}

func (s *Server) handleUpdateSecgwPolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.SecgwPolicyByID(r.Context(), id)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	var p store.SecgwPolicy
	if err := decodeJSON(r, &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	p.ID = id
	p.CreatedBy, p.CreatedAt = existing.CreatedBy, existing.CreatedAt
	if err := s.requireEnforceLicensed(&p); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.validateSecgwClassifierRefs(r.Context(), &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if err := s.Store.UpdateSecgwPolicy(r.Context(), &p); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_policy_updated", "secgw_policy", id, secgwPolicyAudit(existing), secgwPolicyAudit(&p))
	WriteJSON(w, http.StatusOK, map[string]any{"policy": p})
}

func (s *Server) handleDeleteSecgwPolicy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.SecgwPolicyByID(r.Context(), id)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if err := s.Store.DeleteSecgwPolicy(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrSecgwPolicyInUse) {
			WriteError(w, r, ErrInvalidRequest("This policy is still bound to one or more scopes. Remove its bindings first.").WithParam("id"))
			return
		}
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_policy_deleted", "secgw_policy", id, secgwPolicyAudit(existing), nil)
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func secgwPolicyAudit(p *store.SecgwPolicy) map[string]any {
	kinds := make([]string, 0, len(p.Checks))
	for _, c := range p.Checks {
		if c.Enabled {
			kinds = append(kinds, string(c.Kind)+":"+c.Mode)
		}
	}
	return map[string]any{"name": p.Name, "enabled": p.Enabled, "mandatory": p.Mandatory, "checks": kinds, "synthetic_refusal": p.SyntheticRefusal}
}

// validateSecgwClassifierRefs ensures every model-backed check names a
// model that carries a classifier role. Enforced at write time so the
// proxy never discovers a misconfiguration under load.
func (s *Server) validateSecgwClassifierRefs(ctx context.Context, p *store.SecgwPolicy) error {
	for i, c := range p.Checks {
		if c.Kind == store.SecgwCheckContentSafety && len(c.Options) > 0 {
			// The taxonomy lives beside the guard protocol, not in store,
			// so unknown codes are refused here rather than at scan time.
			var o secgw.ContentSafetyOptions
			if err := json.Unmarshal(c.Options, &o); err != nil {
				return ErrInvalidRequest("checks[" + strconv.Itoa(i) + "].options: " + err.Error()).WithParam("checks")
			}
			for _, code := range o.Categories {
				if !classify.KnownCategory(code) {
					return ErrInvalidRequest("checks[" + strconv.Itoa(i) + "].options.categories: unknown category " + code).WithParam("checks")
				}
			}
		}
		if c.ClassifierModelID == "" {
			continue
		}
		m, err := s.Store.ModelByID(ctx, c.ClassifierModelID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidRequest("checks[" + strconv.Itoa(i) + "].classifier_model_id: model not found").WithParam("checks")
		}
		if err != nil {
			return err
		}
		if m.ClassifierRole == "" {
			return ErrInvalidRequest("checks[" + strconv.Itoa(i) + "].classifier_model_id: " + m.PublicName() + " is not marked as a classifier. Set its classifier role first.").WithParam("checks")
		}
		// The check kind dictates the protocol. Binding a Prompt Guard
		// encoder to content_safety (or Llama Guard to prompt_injection)
		// would only fail at request time, inside the breaker, as a
		// generic "classifier failed". Refuse it here with the reason.
		if want := store.ClassifierRoleForKind(c.Kind); want != "" && m.ClassifierRole != want {
			return ErrInvalidRequest("checks[" + strconv.Itoa(i) + "].classifier_model_id: " + m.PublicName() + " is a " + m.ClassifierRole + " classifier; " + string(c.Kind) + " needs a " + want + " model.").WithParam("checks")
		}
	}
	return nil
}

// --- bindings -----------------------------------------------------------------

func (s *Server) handleListSecgwBindings(w http.ResponseWriter, r *http.Request) {
	bindings, err := s.Store.ListSecgwBindings(r.Context())
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

func (s *Server) handleCreateSecgwBinding(w http.ResponseWriter, r *http.Request) {
	var b store.SecgwBinding
	if err := decodeJSON(r, &b); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if u := UserFrom(r.Context()); u != nil {
		b.CreatedBy = u.ID
	}
	if err := s.Store.CreateSecgwBinding(r.Context(), &b); err != nil {
		if errors.Is(err, store.ErrSecgwDuplicateBinding) {
			WriteError(w, r, ErrInvalidRequest("A policy is already bound to that scope. Remove it first: precedence is decided by scope, never by having two policies compete.").WithParam("scope_id"))
			return
		}
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_binding_created", "secgw_binding", b.ID, nil, map[string]any{"policy_id": b.PolicyID, "scope_type": b.ScopeType, "scope_id": b.ScopeID})
	WriteJSON(w, http.StatusCreated, map[string]any{"binding": b})
}

func (s *Server) handleDeleteSecgwBinding(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.Store.DeleteSecgwBinding(r.Context(), id); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_binding_deleted", "secgw_binding", id, nil, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// --- term lists ---------------------------------------------------------------

func (s *Server) handleListSecgwTermLists(w http.ResponseWriter, r *http.Request) {
	lists, err := s.Store.ListSecgwTermLists(r.Context())
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"term_lists": lists})
}

// handleGetSecgwTermList returns the list WITH its terms. That read is
// audited: the list is confidential and knowing who looked at it matters.
func (s *Server) handleGetSecgwTermList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, err := s.Store.SecgwTermListByID(r.Context(), id, s.Cipher, true)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.audit(r, "secgw_term_list_read", "secgw_term_list", id, nil, map[string]any{"name": l.Name, "term_count": l.TermCount})
	WriteJSON(w, http.StatusOK, map[string]any{"term_list": l})
}

func (s *Server) handleCreateSecgwTermList(w http.ResponseWriter, r *http.Request) {
	var l store.SecgwTermList
	if err := decodeJSON(r, &l); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if u := UserFrom(r.Context()); u != nil {
		l.CreatedBy = u.ID
	}
	if err := s.Store.CreateSecgwTermList(r.Context(), &l, s.Cipher); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_term_list_created", "secgw_term_list", l.ID, nil, map[string]any{"name": l.Name, "match_mode": l.MatchMode, "term_count": l.TermCount})
	// Never echo the terms back on create/update; the caller has them.
	l.Terms, l.Allow = nil, nil
	WriteJSON(w, http.StatusCreated, map[string]any{"term_list": l})
}

func (s *Server) handleUpdateSecgwTermList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.SecgwTermListByID(r.Context(), id, s.Cipher, true)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	var l store.SecgwTermList
	if err := decodeJSON(r, &l); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	l.ID = id
	l.CreatedBy, l.CreatedAt = existing.CreatedBy, existing.CreatedAt
	// Terms omitted on update = keep existing contents (metadata-only edit).
	if l.Terms == nil {
		l.Terms = existing.Terms
	}
	if l.Allow == nil {
		l.Allow = existing.Allow
	}
	if err := s.Store.UpdateSecgwTermList(r.Context(), &l, s.Cipher); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_term_list_updated", "secgw_term_list", id,
		map[string]any{"name": existing.Name, "match_mode": existing.MatchMode, "term_count": existing.TermCount},
		map[string]any{"name": l.Name, "match_mode": l.MatchMode, "term_count": l.TermCount})
	l.Terms, l.Allow = nil, nil
	WriteJSON(w, http.StatusOK, map[string]any{"term_list": l})
}

func (s *Server) handleDeleteSecgwTermList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.SecgwTermListByID(r.Context(), id, nil, false)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if err := s.Store.DeleteSecgwTermList(r.Context(), id); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_term_list_deleted", "secgw_term_list", id, map[string]any{"name": existing.Name, "term_count": existing.TermCount}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleImportSecgwTermList accepts text/plain or text/csv (one term per
// line; a CSV takes the first column) and replaces or appends. Nobody
// hand-types 400 codenames into a form.
func (s *Server) handleImportSecgwTermList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.Store.SecgwTermListByID(r.Context(), id, s.Cipher, true)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "append"
	}
	if mode != "append" && mode != "replace" {
		WriteError(w, r, ErrInvalidRequest("mode must be append or replace").WithParam("mode"))
		return
	}
	terms, err := readTermLines(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("could not read the term file: "+err.Error()))
		return
	}
	if mode == "replace" {
		existing.Terms = terms
	} else {
		existing.Terms = append(existing.Terms, terms...)
	}
	before := existing.TermCount
	if err := s.Store.UpdateSecgwTermList(r.Context(), existing, s.Cipher); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_term_list_imported", "secgw_term_list", id, map[string]any{"term_count": before}, map[string]any{"term_count": existing.TermCount, "mode": mode, "imported": len(terms)})
	WriteJSON(w, http.StatusOK, map[string]any{"term_count": existing.TermCount, "imported": len(terms)})
}

func readTermLines(rd io.Reader) ([]string, error) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var out []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// CSV: first column, honouring a simple quoted field.
		if i := strings.IndexByte(line, ','); i >= 0 && !strings.HasPrefix(line, "\"") {
			line = strings.TrimSpace(line[:i])
		} else if strings.HasPrefix(line, "\"") {
			if j := strings.Index(line[1:], "\""); j >= 0 {
				line = line[1 : j+1]
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func (s *Server) handleExportSecgwTermList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, err := s.Store.SecgwTermListByID(r.Context(), id, s.Cipher, true)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.audit(r, "secgw_term_list_exported", "secgw_term_list", id, nil, map[string]any{"name": l.Name, "term_count": l.TermCount})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(l.Name, "\"", "")+".txt\"")
	w.WriteHeader(http.StatusOK)
	for _, t := range l.Terms {
		_, _ = io.WriteString(w, t+"\n")
	}
}

// --- violations ---------------------------------------------------------------

func (s *Server) handleListSecgwViolations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SecgwViolationFilter{Kind: q.Get("kind"), Action: q.Get("action"), UserID: q.Get("user_id"), ModelName: q.Get("model")}
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = t
		} else if d, err := time.ParseDuration(v); err == nil {
			f.Since = time.Now().UTC().Add(-d)
		} else {
			WriteError(w, r, ErrInvalidRequest("since must be an RFC 3339 timestamp or a duration like 24h").WithParam("since"))
			return
		}
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	f.Offset = max(0, f.Offset)
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	vs, err := s.Store.ListSecgwViolations(r.Context(), f)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	nextFilter := f
	nextFilter.Offset += f.Limit
	nextFilter.Limit = 1
	next, err := s.Store.ListSecgwViolations(r.Context(), nextFilter)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"violations": vs, "has_more": len(next) > 0, "offset": f.Offset, "limit": f.Limit})
}

// handleGetSecgwViolation returns one violation with its decrypted match
// text when the row carries one. Every such read is audited: the person
// who can read captured jailbreak prompts is not automatically the person
// who administers rate cards, and an auditor will ask who looked.
func (s *Server) handleGetSecgwViolation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	v, err := s.Store.SecgwViolationByID(r.Context(), id, s.Cipher)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if v.HasMatchText {
		s.audit(r, "secgw_violation_text_read", "secgw_violation", id, nil, map[string]any{"kind": v.Kind, "request_id": v.RequestID})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"violation": v})
}

// --- effective / dry-run / classifiers ------------------------------------------

// handleSecgwEffective renders the merged check set for a (caller, model)
// pair with the trace that produced it. Precedence you cannot see is
// precedence you cannot audit.
func (s *Server) handleSecgwEffective(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub, err := s.secgwSubjectFromQuery(ctx, r)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	snap, err := s.Store.LoadSecgwSnapshot(ctx)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	eff := secgw.Resolve(snap, sub)
	checks := make([]secgw.ResolvedCheck, 0, len(eff.Checks))
	for _, k := range store.SecgwCheckKinds() {
		if c, ok := eff.Checks[k]; ok {
			checks = append(checks, c)
		}
	}
	// Never emit null for a list: with no bindings the trace is empty, and an
	// empty array is what every client is written against.
	trace := eff.Trace
	if trace == nil {
		trace = []secgw.TraceEntry{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"subject": sub, "checks": checks, "trace": trace})
}

func (s *Server) secgwSubjectFromQuery(ctx context.Context, r *http.Request) (secgw.Subject, error) {
	q := r.URL.Query()
	sub := secgw.Subject{UserID: q.Get("user_id"), ServiceTokenID: q.Get("service_token_id"), ModelID: q.Get("model_id"), ManagedModelID: q.Get("managed_model_id"), UpstreamID: q.Get("upstream_id")}
	if sub.UserID == "" && sub.ServiceTokenID == "" {
		return sub, ErrInvalidRequest("provide user_id or service_token_id").WithParam("user_id")
	}
	if sub.UserID != "" {
		groups, err := s.Store.GroupIDsForUser(ctx, sub.UserID)
		if err != nil {
			return sub, err
		}
		sub.GroupIDs = groups
	}
	if name := q.Get("model"); name != "" && sub.ModelID == "" {
		resolved, err := s.Store.ResolveModelForRequest(ctx, name)
		if err == nil && resolved.Model != nil {
			sub.ModelID = resolved.Model.ID
			sub.UpstreamID = resolved.Model.UpstreamID
			if resolved.Managed != nil {
				sub.ManagedModelID = resolved.Managed.ID
			}
		}
	}
	return sub, nil
}

type secgwDryRunPayload struct {
	Body           json.RawMessage `json:"body"`
	UserID         string          `json:"user_id"`
	ServiceTokenID string          `json:"service_token_id"`
	Model          string          `json:"model"`
	// PolicyID, when set, evaluates the request against ONE policy as if
	// it were bound org-wide — for testing a draft before binding it.
	PolicyID string `json:"policy_id"`
}

// handleSecgwDryRun evaluates a request body without side effects: no
// upstream call, no usage event, no violation rows. Classifier calls DO
// happen (that is the point of testing) and are metered as overhead.
func (s *Server) handleSecgwDryRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in secgwDryRunPayload
	if err := decodeJSON(r, &in); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if len(in.Body) == 0 {
		WriteError(w, r, ErrInvalidRequest("body is required").WithParam("body"))
		return
	}
	var eng *secgw.Engine
	var eff *secgw.Effective
	if in.PolicyID != "" {
		p, err := s.Store.SecgwPolicyByID(ctx, in.PolicyID)
		if err != nil {
			writeSecgwError(w, r, err)
			return
		}
		lists, err := s.Store.LoadSecgwTermLists(ctx, s.Cipher)
		if err != nil {
			writeSecgwError(w, r, err)
			return
		}
		snap := &store.SecgwSnapshot{Policies: map[string]*store.SecgwPolicy{p.ID: p},
			Bindings: []*store.SecgwBinding{{ID: "dry-run", PolicyID: p.ID, ScopeType: store.SecgwScopeOrg}}, TermLists: lists}
		eng, err = secgw.NewEngine(snap, s.secgwClassifiers(), s.Logger)
		if err != nil {
			WriteError(w, r, ErrInvalidRequest("policy does not compile: "+err.Error()))
			return
		}
		eff = eng.Resolve(secgw.Subject{UserID: "dry-run"})
	} else {
		var err error
		eng, err = s.secgwEngine(ctx)
		if err != nil {
			writeSecgwError(w, r, err)
			return
		}
		q := r.URL.Query()
		q.Set("user_id", in.UserID)
		q.Set("service_token_id", in.ServiceTokenID)
		q.Set("model", in.Model)
		r.URL.RawQuery = q.Encode()
		sub, err := s.secgwSubjectFromQuery(ctx, r)
		if err != nil {
			writeSecgwError(w, r, err)
			return
		}
		eff = eng.Resolve(sub)
	}
	dec, err := eng.Ingress(ctx, eff, secgw.Request{Body: in.Body, IsServiceToken: in.ServiceTokenID != ""})
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	type shown struct {
		Kind         string  `json:"kind"`
		RuleID       string  `json:"rule_id"`
		Severity     string  `json:"severity"`
		Action       string  `json:"action"`
		MessageIndex int     `json:"message_index"`
		Offset       int     `json:"offset"`
		Length       int     `json:"length"`
		Score        float64 `json:"classifier_score,omitempty"`
	}
	out := make([]shown, 0, len(dec.Violations))
	for _, v := range dec.Violations {
		out = append(out, shown{Kind: string(v.Kind), RuleID: v.RuleID, Severity: v.Severity, Action: v.Action, MessageIndex: v.MessageIndex, Offset: v.Offset, Length: v.Length, Score: v.ClassifierScore})
	}
	resp := map[string]any{"action": dec.Action, "violations": out, "redactions": dec.Redactions, "block_kind": dec.BlockKind, "classifier_failed": dec.ClassifierFailed}
	if dec.Action == store.SecgwActionRedacted {
		resp["redacted_body"] = json.RawMessage(dec.Body)
	}
	WriteJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListSecgwClassifiers(w http.ResponseWriter, r *http.Request) {
	models, err := s.Store.ListModels(r.Context(), store.ModelFilter{ClassifierOnly: true})
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"classifiers": models, "protocols": classify.Protocols(), "taxonomy": classify.Taxonomy, "default_categories": classify.DefaultCategories()})
}

func (s *Server) handleSetSecgwClassifier(w http.ResponseWriter, r *http.Request) {
	modelID := chi.URLParam(r, "modelID")
	var body struct {
		Role string `json:"classifier_role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	before, err := s.Store.ModelByID(r.Context(), modelID)
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	if err := s.Store.SetModelClassifierRole(r.Context(), modelID, body.Role); err != nil {
		writeSecgwError(w, r, err)
		return
	}
	s.InvalidateConfigCache()
	s.audit(r, "secgw_classifier_role_set", "model", modelID, map[string]any{"classifier_role": before.ClassifierRole}, map[string]any{"classifier_role": body.Role, "model": before.PublicName()})
	after, _ := s.Store.ModelByID(r.Context(), modelID)
	WriteJSON(w, http.StatusOK, map[string]any{"model": after})
}

// handleListSecgwRules lists the built-in secret rules so an admin can pick
// which to disable per policy.
func (s *Server) handleListSecgwRules(w http.ResponseWriter, r *http.Request) {
	rules, err := secgw.BuiltinSecretRules()
	if err != nil {
		writeSecgwError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"secret_rules": rules, "pii_classes": secgw.PIIClasses()})
}

// writeSecgwError maps store validation and not-found errors onto the API
// envelope; anything else is reported as an internal error.
func writeSecgwError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *store.ValidationError
	switch {
	case errors.As(err, &ve):
		WriteError(w, r, ErrInvalidRequest(ve.Message).WithParam(ve.Field))
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, r, ErrNotFoundf("Security Gateway resource"))
	case errors.Is(err, store.ErrSecgwUndecryptable):
		// Not a 500: the row is fine, the operator rotated JANUS_ENCRYPTION_KEY
		// without migrating. Say so; a generic error sends them to the logs.
		WriteError(w, r, newError(http.StatusConflict, CodeInvalidRequest, "invalid_request_error",
			"This record was encrypted with a different JANUS_ENCRYPTION_KEY than the gateway currently holds. Restore the original key, or delete and recreate the record."))
	default:
		WriteError(w, r, err)
	}
}

// requireEnforceLicensed: Community guardrails are observe-only. A policy
// with any check in redact or block mode needs guardrails_enforce. Policies
// that already enforce keep working after a downgrade (configured things
// keep running); only saving a new or changed one is refused.
func (s *Server) requireEnforceLicensed(p *store.SecgwPolicy) error {
	for _, c := range p.Checks {
		if c.Mode == store.SecgwModeRedact || c.Mode == store.SecgwModeBlock {
			return s.requireFeature("guardrails_enforce")
		}
	}
	return nil
}
