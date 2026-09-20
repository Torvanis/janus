package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const modelColumns = `m.id, m.upstream_id, m.name, m.display_name, m.status, m.modalities, m.rate_in_nanousd, m.rate_out_nanousd, m.rate_cached_nanousd, m.rate_cache_write_5m_nanousd, m.rate_cache_write_1h_nanousd, m.context_window, m.rate_effective_from, m.discovered_at, m.classifier_role, m.metadata_json`

func scanModel(scan func(...any) error) (*Model, error) {
	var m Model
	var modalities, effective, discovered, metadata string
	if err := scan(&m.ID, &m.UpstreamID, &m.Name, &m.DisplayName, &m.Status, &modalities, &m.RateInNano, &m.RateOutNano, &m.RateCachedNano, &m.RateCacheWrite5mNano, &m.RateCacheWrite1hNano, &m.ContextWindow, &effective, &discovered, &m.ClassifierRole, &metadata, &m.UpstreamName, &m.AdapterType); err != nil {
		return nil, err
	}
	if err := m.readMetadata(metadata); err != nil {
		return nil, err
	}
	m.Modalities = splitCSV(modalities)
	m.RateEffectiveFrom = ParseTime(effective)
	m.DiscoveredAt = ParseTime(discovered)
	return &m, nil
}

func splitCSV(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

const modelJoin = ` FROM model m JOIN upstream u ON u.id = m.upstream_id `
const modelSelect = `SELECT ` + modelColumns + `, u.name, u.adapter_type` + modelJoin

// ModelFilter narrows an admin model listing.
type ModelFilter struct {
	UpstreamID string
	Status     string
	Search     string
	Sort       string
	// Servable excludes models carrying a classifier_role: guard
	// classifiers are never offered to callers, resolvable by name on the
	// proxy, or grantable. Admin listings leave this false so the model
	// stays visible for configuration.
	Servable bool
	// ClassifierOnly lists only guard classifiers (the admin "which models
	// can back a check" picker).
	ClassifierOnly bool
}

// ListModels returns curated models across upstreams.
func (s *Store) ListModels(ctx context.Context, f ModelFilter) ([]*Model, error) {
	where := []string{"u.deleted_at = ''"}
	args := []any{}
	if f.UpstreamID != "" {
		where = append(where, "m.upstream_id = ?")
		args = append(args, f.UpstreamID)
	}
	if f.Status != "" {
		where = append(where, "m.status = ?")
		args = append(args, f.Status)
	}
	if f.Servable {
		where = append(where, "m.classifier_role = ''")
	}
	if f.ClassifierOnly {
		where = append(where, "m.classifier_role <> ''")
	}
	if f.Search != "" {
		where = append(where, "(LOWER(m.name) LIKE ? OR LOWER(m.display_name) LIKE ?)")
		needle := "%" + strings.ToLower(f.Search) + "%"
		args = append(args, needle, needle)
	}
	order := "u.name ASC, m.name ASC"
	switch f.Sort {
	case "name":
		order = "m.name ASC"
	case "cost":
		order = "m.rate_in_nanousd DESC"
	case "discovered":
		order = "m.discovered_at DESC"
	}
	rows, err := s.query(ctx, modelSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Model{}
	for rows.Next() {
		m, err := scanModel(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range out {
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM model_grant WHERE model_id = ?`, m.ID).Scan(&m.GrantCount); err != nil {
			return nil, fmt.Errorf("count grants: %w", err)
		}
	}
	return out, nil
}

// ModelByID loads one model with its upstream context.
func (s *Store) ModelByID(ctx context.Context, id string) (*Model, error) {
	m, err := scanModel(s.queryRow(ctx, modelSelect+` WHERE m.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load model: %w", err)
	}
	return m, nil
}

// ResolvedModel is the outcome of resolving a caller-supplied model string.
// It always carries the real Model that will actually serve the request; when
// the caller addressed a managed alias, Managed is non-nil and describes the
// indirection so the proxy can meter and audit it correctly.
type ResolvedModel struct {
	Model   *Model
	Managed *ManagedModel
	// Fallback is the alias's configured fallback when it exists and is
	// enabled — loaded alongside the target so the proxy can switch to it on
	// a runtime trigger without a second lookup. nil when the alias has no
	// usable fallback.
	Fallback *Model
	// FellBack reports that Model IS the fallback: the target was
	// unavailable for the reason in FallbackReason (a FallbackTrigger*
	// value) and the fallback was substituted. PrimaryModel is the bypassed
	// target when it still exists.
	FellBack       bool
	FallbackReason string
	PrimaryModel   *Model
}

// WithFallback returns a copy of the resolution switched to its fallback for
// the given trigger. Callers must have checked Fallback != nil.
func (r ResolvedModel) WithFallback(trigger string) ResolvedModel {
	out := r
	out.PrimaryModel = r.Model
	out.Model = r.Fallback
	out.FellBack = true
	out.FallbackReason = trigger
	return out
}

// ViaManagedModel reports whether the caller reached the model through an alias.
func (r ResolvedModel) ViaManagedModel() bool { return r.Managed != nil }

// RequestedName is the name the caller actually sent: the alias when one was
// used, otherwise the model's own public name.
func (r ResolvedModel) RequestedName() string {
	if r.Managed != nil {
		return r.Managed.Name
	}
	if r.Model != nil {
		return r.Model.PublicName()
	}
	return ""
}

// ErrManagedModelBroken reports that a caller addressed a managed alias whose
// underlying model is missing or not enabled. It is distinct from ErrNotFound
// because the two need different operator-facing messages: "no such model"
// versus "this alias is misconfigured, tell your administrator".
var ErrManagedModelBroken = errors.New("managed model target is unavailable")

// ErrManagedModelFallbackExhausted reports that a managed alias's target is
// unavailable AND the fallback the admin configured for exactly that case
// cannot take over either (missing or disabled). It is its own error so the
// caller — and the admin reading the request log — can tell "no safety net was
// configured" from "the safety net is broken too".
var ErrManagedModelFallbackExhausted = errors.New("managed model target and fallback are both unavailable")

// ResolveModelForRequest resolves a caller-supplied model name to the model
// that will serve the request.
//
// Managed aliases are tried FIRST. An admin who creates the alias
// "current-best" intends every request for that name to go through the
// indirection; letting a same-named real model shadow it would silently defeat
// the feature. Name uniqueness is enforced on write (assertManagedNameAvailable),
// so in practice the two namespaces do not overlap — this ordering just makes
// the intent explicit and safe if a real model is later discovered with a
// colliding name.
//
// A disabled alias does not resolve at all (it behaves as though it does not
// exist). An enabled alias whose target has gone missing or been disabled
// resolves to ErrManagedModelBroken so the caller gets an accurate message
// rather than a misleading "model not found".
func (s *Store) ResolveModelForRequest(ctx context.Context, name string) (ResolvedModel, error) {
	managed, err := s.ManagedModelByName(ctx, name)
	switch {
	case err == nil && managed.Status == ManagedModelEnabled:
		resolved := ResolvedModel{Managed: managed}
		if managed.FallbackModelID != "" && !managed.FallbackBroken {
			if fb, err := s.ModelByID(ctx, managed.FallbackModelID); err == nil && fb.Status == ModelEnabled {
				resolved.Fallback = fb
			}
		}
		target, err := s.ModelByID(ctx, managed.TargetModelID)
		if managed.Broken || err != nil {
			// The target is gone or disabled: a configuration-level
			// unavailability, deterministic for every caller. Serve the
			// fallback if one is configured for this failure mode; otherwise
			// the alias is broken — and if a fallback WAS configured but is
			// itself unusable, say so distinctly.
			if err == nil {
				resolved.PrimaryModel = target
			}
			if resolved.Fallback != nil && managed.HasFallbackTrigger(FallbackTriggerTargetUnavailable) {
				resolved.Model = resolved.Fallback
				resolved.FellBack = true
				resolved.FallbackReason = FallbackTriggerTargetUnavailable
				return resolved, nil
			}
			if managed.FallbackModelID != "" && managed.HasFallbackTrigger(FallbackTriggerTargetUnavailable) {
				return resolved, ErrManagedModelFallbackExhausted
			}
			return resolved, ErrManagedModelBroken
		}
		resolved.Model = target
		return resolved, nil
	case err == nil:
		// The alias exists but is disabled: fall through so a real model of
		// the same name can still answer, and otherwise report not-found.
	case errors.Is(err, ErrNotFound):
		// No alias by that name; fall through to the real catalog.
	default:
		return ResolvedModel{}, err
	}

	model, err := s.ModelByName(ctx, name)
	if err != nil {
		return ResolvedModel{}, err
	}
	return ResolvedModel{Model: model}, nil
}

// ModelByUpstreamAndName loads one model by its owning upstream and native
// name — the natural key discovery works in, where a bare name may repeat
// across upstreams. Status is not filtered: discovery updates pending and
// disabled rows too.
func (s *Store) ModelByUpstreamAndName(ctx context.Context, upstreamID, name string) (*Model, error) {
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.UpstreamID == upstreamID && m.Name == name {
			return m, nil
		}
	}
	return nil, ErrNotFound
}

// maxRoutableNameLength bounds an admin-set display name. Native model names
// from upstreams are not bounded by this — only names an admin types.
const maxRoutableNameLength = 200

// ModelByName resolves a caller-supplied model name against enabled models.
// Display names win over native names (a rename is the caller-facing alias),
// and within each tier the name is matched exactly first, then as
// "upstream/model" so operators can disambiguate collisions. Native names keep
// resolving after a rename, so existing client configurations never break.
//
// Matching is case-insensitive throughout. New display names are stored
// lowercase, but native upstream names are whatever the provider reports, and
// names saved before that rule existed may carry capitals or spaces — those
// keep resolving here rather than breaking live traffic on upgrade. The
// "upstream/model" tier was always case-insensitive, so this also removes an
// inconsistency where the qualified form matched but the bare form did not.
//
// Managed-model aliases are NOT resolved here — see ResolveModelForRequest,
// which layers alias resolution on top of this function.
func (s *Store) ModelByName(ctx context.Context, name string) (*Model, error) {
	models, err := s.ListModels(ctx, ModelFilter{Status: ModelEnabled, Servable: true})
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.DisplayName != "" && strings.EqualFold(m.DisplayName, name) {
			return m, nil
		}
	}
	for _, m := range models {
		if m.DisplayName != "" && strings.EqualFold(m.UpstreamName+"/"+m.DisplayName, name) {
			return m, nil
		}
	}
	for _, m := range models {
		if strings.EqualFold(m.Name, name) {
			return m, nil
		}
	}
	for _, m := range models {
		if strings.EqualFold(m.UpstreamName+"/"+m.Name, name) {
			return m, nil
		}
	}
	return nil, ErrNotFound
}

// SetModelDisplayName sets or clears the caller-facing alias of a model. An
// empty (or whitespace-only) displayName clears the alias so the native name
// is presented again.
//
// The value is normalised and validated by NormalizeRoutableName — the same
// rule managed-model aliases use, because callers address both through the
// request body's "model" field. Names are lowercased, and spaces are refused:
// routing matches exactly, so a name with a space could be saved but never
// called, and a capitalised name only resolved if the caller reproduced the
// capitalisation byte for byte.
//
// A non-empty value must not collide with any OTHER enabled model's native
// name or display name — the exact-match comparison ModelByName uses — nor
// with any managed-model name. Either collision returns a *ValidationError on
// field "display_name" so the API layer can reject the write as a 400.
// Disabled, stale, and pending models do not participate in routing and
// therefore do not block a rename. Returns ErrNotFound when the model id does
// not exist.
func (s *Store) SetModelDisplayName(ctx context.Context, id, displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if _, err := s.ModelByID(ctx, id); err != nil {
		return err
	}
	if displayName != "" {
		normalized, err := NormalizeRoutableName(displayName, "display_name", maxRoutableNameLength)
		if err != nil {
			return err
		}
		displayName = normalized
		enabled, err := s.ListModels(ctx, ModelFilter{Status: ModelEnabled})
		if err != nil {
			return err
		}
		for _, m := range enabled {
			if m.ID == id {
				continue
			}
			if strings.EqualFold(m.Name, displayName) || (m.DisplayName != "" && strings.EqualFold(m.DisplayName, displayName)) {
				return &ValidationError{
					Field:   "display_name",
					Message: fmt.Sprintf("display name %q is already in use by enabled model %q on upstream %q", displayName, m.Name, m.UpstreamName),
				}
			}
		}
		// The managed-model namespace is shared with model names: an alias
		// named "current-best" and a model renamed to "current-best" would
		// make ResolveModelForRequest ambiguous. Block the rename rather than
		// letting the alias silently win at request time.
		managed, err := s.ManagedModelNames(ctx)
		if err != nil {
			return err
		}
		if _, taken := managed[strings.ToLower(displayName)]; taken {
			return &ValidationError{
				Field:   "display_name",
				Message: fmt.Sprintf("display name %q is already in use by a managed model", displayName),
			}
		}
	}
	return s.exec(ctx, `UPDATE model SET display_name = ? WHERE id = ?`, displayName, id)
}

// UpsertDiscoveredModel records a model reported by an upstream. Existing rows
// keep their curation status; brand new models land as pending_approval.
func (s *Store) UpsertDiscoveredModel(ctx context.Context, upstreamID, name string, modalities []string) (created bool, err error) {
	var id, status string
	err = s.queryRow(ctx, `SELECT id, status FROM model WHERE upstream_id = ? AND name = ?`, upstreamID, name).Scan(&id, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		now := FormatTime(nowUTC())
		return true, s.exec(ctx,
			`INSERT INTO model (id, upstream_id, name, status, modalities, rate_in_nanousd, rate_out_nanousd, rate_cached_nanousd, rate_effective_from, discovered_at, created_at)
			 VALUES (?,?,?,?,?,0,0,0,'',?,?)`,
			NewID(), upstreamID, name, ModelPending, strings.Join(modalities, ","), now, now)
	case err != nil:
		return false, fmt.Errorf("lookup discovered model: %w", err)
	default:
		next := status
		if status == ModelStale {
			next = ModelPending
		}
		return false, s.exec(ctx, `UPDATE model SET discovered_at = ?, modalities = ?, status = ? WHERE id = ?`,
			FormatTime(nowUTC()), strings.Join(modalities, ","), next, id)
	}
}

// MarkStaleModels flags models that an upstream stopped reporting. Rows are
// never deleted: an admin decides what to do with them.
func (s *Store) MarkStaleModels(ctx context.Context, upstreamID string, seen []string) (int, error) {
	query := `UPDATE model SET status = ? WHERE upstream_id = ? AND status <> ?`
	args := []any{ModelStale, upstreamID, ModelStale}
	if len(seen) > 0 {
		query += ` AND name NOT IN (` + placeholders(len(seen)) + `)`
		args = append(args, toArgs(seen)...)
	}
	res, err := s.db.ExecContext(ctx, s.rebind(query), args...)
	if err != nil {
		return 0, fmt.Errorf("mark stale models: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 && s.modelRevision != nil {
		s.modelRevision.Add(1)
	}
	return int(n), nil
}

// SetModelStatus curates a model.
func (s *Store) SetModelStatus(ctx context.Context, id, status string) error {
	return s.exec(ctx, `UPDATE model SET status = ? WHERE id = ?`, status, id)
}

// SetModelContextWindow records the model's context window size in tokens.
// Zero is a valid write — it clears the value back to "unknown". Negative
// token counts are rejected as a *ValidationError on field "context_window"
// so the API layer maps them to a 400 instead of a generic 500. Returns
// ErrNotFound when the model id does not exist.
func (s *Store) SetModelContextWindow(ctx context.Context, id string, tokens int64) error {
	return s.PatchModel(ctx, id, ModelPatch{Overrides: map[string]*int64{"context_window": &tokens}})
}

// SetModelRates appends a new rate-card version carrying all five billing
// dimensions and points the model at it. Historical usage keeps the rate that
// was effective when it was recorded. Every dimension must be >= 0; zero is a
// valid, explicitly saved price. Both writes happen in one transaction so the
// model row and the version history can never disagree.
func (s *Store) SetModelRates(ctx context.Context, id string, rc RateCard) error {
	for _, v := range []int64{rc.RateInNano, rc.RateOutNano, rc.RateCachedNano, rc.RateCacheWrite5mNano, rc.RateCacheWrite1hNano} {
		if v < 0 {
			return ErrNegativeRate
		}
	}
	return s.PatchModel(ctx, id, ModelPatch{EffectiveFrom: rc.EffectiveFrom, Overrides: map[string]*int64{
		"rate_in_nanousd": &rc.RateInNano, "rate_out_nanousd": &rc.RateOutNano, "rate_cached_nanousd": &rc.RateCachedNano, "rate_cache_write_5m_nanousd": &rc.RateCacheWrite5mNano, "rate_cache_write_1h_nanousd": &rc.RateCacheWrite1hNano,
	}})
}

// RateCardAt resolves the full rate card in force at a given instant, falling
// back to the model's current rates when no historical version covers the
// timestamp. Versions written before migration 0011 read their cache-write
// rates as 0 — exactly what was billed at the time.
func (s *Store) RateCardAt(ctx context.Context, modelID string, at time.Time) (*RateCard, error) {
	var rc RateCard
	var effective string
	row := s.queryRow(ctx,
		`SELECT rate_in_nanousd, rate_out_nanousd, rate_cached_nanousd, rate_cache_write_5m_nanousd, rate_cache_write_1h_nanousd, effective_from FROM rate_card_version
		 WHERE model_id = ? AND effective_from <= ? ORDER BY effective_from DESC LIMIT 1`,
		modelID, FormatTime(at))
	err := row.Scan(&rc.RateInNano, &rc.RateOutNano, &rc.RateCachedNano, &rc.RateCacheWrite5mNano, &rc.RateCacheWrite1hNano, &effective)
	if errors.Is(err, sql.ErrNoRows) {
		m, mErr := s.ModelByID(ctx, modelID)
		if mErr != nil {
			return nil, mErr
		}
		return &RateCard{
			EffectiveFrom:        m.RateEffectiveFrom,
			RateInNano:           m.RateInNano,
			RateOutNano:          m.RateOutNano,
			RateCachedNano:       m.RateCachedNano,
			RateCacheWrite5mNano: m.RateCacheWrite5mNano,
			RateCacheWrite1hNano: m.RateCacheWrite1hNano,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load rate card: %w", err)
	}
	rc.EffectiveFrom = ParseTime(effective)
	return &rc, nil
}

// ModelHealthStats rolls up usage events recorded since `since`, keyed by
// model_id, for the model catalog's health badges.
//
// Only requests that reached — or tried to reach — the upstream count. A
// request the gateway refused on policy grounds (quota, blocking rule, grant,
// rate limit: error_code "policy.*") says nothing about whether the model is
// working, so it is excluded from both numerator and denominator. An error is
// an upstream-attributable failure: any 5xx (which includes the 503 Janus
// records when the provider is unreachable) or an "upstream.*" error code
// (the provider rate-limiting the gateway). Models with no qualifying traffic
// are simply absent from the map.
func (s *Store) ModelHealthStats(ctx context.Context, since time.Time) (map[string]ModelHealthStats, error) {
	rows, err := s.query(ctx, `SELECT model_id, COUNT(*),
		COALESCE(SUM(CASE WHEN http_status >= 500 OR error_code LIKE 'upstream.%' THEN 1 ELSE 0 END), 0)
		FROM usage_event
		WHERE created_at >= ? AND model_id <> '' AND error_code NOT LIKE 'policy.%'
		GROUP BY model_id`, FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("model health stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ModelHealthStats{}
	for rows.Next() {
		var id string
		var h ModelHealthStats
		if err := rows.Scan(&id, &h.Requests, &h.Errors); err != nil {
			return nil, fmt.Errorf("scan model health stats: %w", err)
		}
		out[id] = h
	}
	return out, rows.Err()
}

// CountModelsByStatus reports how many models sit in each curation state.
func (s *Store) CountModelsByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.query(ctx, `SELECT m.status, COUNT(*) FROM model m JOIN upstream u ON u.id = m.upstream_id WHERE u.deleted_at = '' GROUP BY m.status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan model status: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}
