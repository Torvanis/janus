package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SecgwCipher is the subset of crypto.Cipher the term-list and violation
// stores need for encryption at rest. An interface so the store package
// does not import crypto and tests can run without a key.
type SecgwCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(envelope string) (string, error)
}

// --- policies -----------------------------------------------------------------

const secgwPolicyColumns = `id, name, description, enabled, mandatory, checks, capture, synthetic_refusal, refusal_text, created_by, created_at, updated_at`

func scanSecgwPolicy(scan func(...any) error) (*SecgwPolicy, error) {
	var p SecgwPolicy
	var enabled, mandatory, synth int
	var checks, capture, created, updated string
	if err := scan(&p.ID, &p.Name, &p.Description, &enabled, &mandatory, &checks, &capture, &synth, &p.RefusalText, &p.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	p.Enabled, p.Mandatory, p.SyntheticRefusal = enabled == 1, mandatory == 1, synth == 1
	if err := json.Unmarshal([]byte(checks), &p.Checks); err != nil {
		return nil, fmt.Errorf("decode policy checks: %w", err)
	}
	if capture != "" {
		if err := json.Unmarshal([]byte(capture), &p.Capture); err != nil {
			return nil, fmt.Errorf("decode policy capture: %w", err)
		}
	}
	p.CreatedAt, p.UpdatedAt = ParseTime(created), ParseTime(updated)
	return &p, nil
}

// ListSecgwPolicies returns every policy, name order, with binding counts.
func (s *Store) ListSecgwPolicies(ctx context.Context) ([]*SecgwPolicy, error) {
	rows, err := s.query(ctx, `SELECT `+secgwPolicyColumns+` FROM secgw_policy ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SecgwPolicy{}
	for rows.Next() {
		p, err := scanSecgwPolicy(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan policy: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range out {
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM secgw_binding WHERE policy_id = ?`, p.ID).Scan(&p.BindingCount); err != nil {
			return nil, fmt.Errorf("count bindings: %w", err)
		}
	}
	return out, nil
}

// SecgwPolicyByID loads one policy.
func (s *Store) SecgwPolicyByID(ctx context.Context, id string) (*SecgwPolicy, error) {
	p, err := scanSecgwPolicy(s.queryRow(ctx, `SELECT `+secgwPolicyColumns+` FROM secgw_policy WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load policy: %w", err)
	}
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM secgw_binding WHERE policy_id = ?`, p.ID).Scan(&p.BindingCount); err != nil {
		return nil, fmt.Errorf("count bindings: %w", err)
	}
	return p, nil
}

// CreateSecgwPolicy validates and persists a policy.
func (s *Store) CreateSecgwPolicy(ctx context.Context, p *SecgwPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := s.secgwPolicyNameFree(ctx, p.Name, ""); err != nil {
		return err
	}
	p.ID = NewID()
	p.CreatedAt = nowUTC()
	p.UpdatedAt = p.CreatedAt
	checks, capture, err := secgwEncodePolicy(p)
	if err != nil {
		return err
	}
	return s.exec(ctx, `INSERT INTO secgw_policy (`+secgwPolicyColumns+`) VALUES (`+placeholders(12)+`)`,
		p.ID, p.Name, p.Description, boolInt(p.Enabled), boolInt(p.Mandatory), checks, capture,
		boolInt(p.SyntheticRefusal), p.RefusalText, p.CreatedBy, FormatTime(p.CreatedAt), FormatTime(p.UpdatedAt))
}

// UpdateSecgwPolicy replaces a policy's editable fields in place, so its
// identity (and every binding on it) survives the edit.
func (s *Store) UpdateSecgwPolicy(ctx context.Context, p *SecgwPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := s.SecgwPolicyByID(ctx, p.ID); err != nil {
		return err
	}
	if err := s.secgwPolicyNameFree(ctx, p.Name, p.ID); err != nil {
		return err
	}
	p.UpdatedAt = nowUTC()
	checks, capture, err := secgwEncodePolicy(p)
	if err != nil {
		return err
	}
	return s.exec(ctx, `UPDATE secgw_policy SET name = ?, description = ?, enabled = ?, mandatory = ?, checks = ?, capture = ?, synthetic_refusal = ?, refusal_text = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Description, boolInt(p.Enabled), boolInt(p.Mandatory), checks, capture,
		boolInt(p.SyntheticRefusal), p.RefusalText, FormatTime(p.UpdatedAt), p.ID)
}

// DeleteSecgwPolicy removes a policy. It refuses while bindings exist so
// unbinding is a deliberate step rather than a cascade an admin did not see.
func (s *Store) DeleteSecgwPolicy(ctx context.Context, id string) error {
	p, err := s.SecgwPolicyByID(ctx, id)
	if err != nil {
		return err
	}
	if p.BindingCount > 0 {
		return ErrSecgwPolicyInUse
	}
	return s.exec(ctx, `DELETE FROM secgw_policy WHERE id = ?`, id)
}

func (s *Store) secgwPolicyNameFree(ctx context.Context, name, exceptID string) error {
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM secgw_policy WHERE LOWER(name) = LOWER(?) AND id <> ?`, name, exceptID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return &ValidationError{Field: "name", Message: "a policy with that name already exists"}
	}
	return nil
}

func secgwEncodePolicy(p *SecgwPolicy) (checks, capture string, err error) {
	c, err := json.Marshal(p.Checks)
	if err != nil {
		return "", "", fmt.Errorf("encode checks: %w", err)
	}
	cap, err := json.Marshal(p.Capture)
	if err != nil {
		return "", "", fmt.Errorf("encode capture: %w", err)
	}
	return string(c), string(cap), nil
}

// --- bindings -----------------------------------------------------------------

const secgwBindingColumns = `b.id, b.policy_id, b.scope_type, b.scope_id, b.created_by, b.created_at, p.name`

func scanSecgwBinding(scan func(...any) error) (*SecgwBinding, error) {
	var b SecgwBinding
	var created string
	if err := scan(&b.ID, &b.PolicyID, &b.ScopeType, &b.ScopeID, &b.CreatedBy, &created, &b.PolicyName); err != nil {
		return nil, err
	}
	b.CreatedAt = ParseTime(created)
	return &b, nil
}

// ListSecgwBindings returns every binding with its policy name and a
// resolved scope label.
func (s *Store) ListSecgwBindings(ctx context.Context) ([]*SecgwBinding, error) {
	rows, err := s.query(ctx, `SELECT `+secgwBindingColumns+` FROM secgw_binding b JOIN secgw_policy p ON p.id = b.policy_id ORDER BY b.scope_type ASC, b.created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SecgwBinding{}
	for rows.Next() {
		b, err := scanSecgwBinding(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, b := range out {
		b.ScopeName = s.secgwScopeName(ctx, b.ScopeType, b.ScopeID)
	}
	return out, nil
}

func (s *Store) secgwScopeName(ctx context.Context, scopeType, scopeID string) string {
	switch scopeType {
	case SecgwScopeOrg:
		return "Everyone"
	case SecgwScopeGroup:
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM user_group WHERE id = ?`, scopeID).Scan(&name); err == nil {
			return name
		}
	case SecgwScopeUpstream:
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM upstream WHERE id = ?`, scopeID).Scan(&name); err == nil {
			return name
		}
	case SecgwScopeServiceToken:
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM service_token WHERE id = ?`, scopeID).Scan(&name); err == nil {
			return name
		}
	case SecgwScopeModel:
		if m, err := s.ModelByID(ctx, scopeID); err == nil {
			return m.PublicName()
		}
	case SecgwScopeManagedModel:
		var name string
		if err := s.queryRow(ctx, `SELECT name FROM managed_model WHERE id = ?`, scopeID).Scan(&name); err == nil {
			return name
		}
	}
	return scopeID
}

// CreateSecgwBinding attaches a policy to a scope. One binding per scope:
// precedence is decided at write time, never by a runtime tie-break.
func (s *Store) CreateSecgwBinding(ctx context.Context, b *SecgwBinding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if _, err := s.SecgwPolicyByID(ctx, b.PolicyID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return &ValidationError{Field: "policy_id", Message: "policy not found"}
		}
		return err
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM secgw_binding WHERE scope_type = ? AND scope_id = ?`, b.ScopeType, b.ScopeID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrSecgwDuplicateBinding
	}
	b.ID = NewID()
	b.CreatedAt = nowUTC()
	return s.exec(ctx, `INSERT INTO secgw_binding (id, policy_id, scope_type, scope_id, created_by, created_at) VALUES (?,?,?,?,?,?)`,
		b.ID, b.PolicyID, b.ScopeType, b.ScopeID, b.CreatedBy, FormatTime(b.CreatedAt))
}

// DeleteSecgwBinding removes a binding.
func (s *Store) DeleteSecgwBinding(ctx context.Context, id string) error {
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM secgw_binding WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return s.exec(ctx, `DELETE FROM secgw_binding WHERE id = ?`, id)
}

// SecgwSnapshot is the immutable policy state the proxy hot path resolves
// against: every enabled policy and every binding, loaded together so a
// request never sees a binding whose policy it cannot find.
type SecgwSnapshot struct {
	Policies map[string]*SecgwPolicy
	Bindings []*SecgwBinding
	// TermLists is populated by the caller that holds the cipher.
	TermLists []*SecgwTermList
	LoadedAt  time.Time
}

// Empty reports whether no binding exists at all — the common case in a
// deployment that has not enabled the feature, and the fast path.
func (snap *SecgwSnapshot) Empty() bool { return snap == nil || len(snap.Bindings) == 0 }

// LoadSecgwSnapshot reads policies and bindings for the resolver.
func (s *Store) LoadSecgwSnapshot(ctx context.Context) (*SecgwSnapshot, error) {
	policies, err := s.ListSecgwPolicies(ctx)
	if err != nil {
		return nil, err
	}
	snap := &SecgwSnapshot{Policies: map[string]*SecgwPolicy{}, LoadedAt: nowUTC()}
	for _, p := range policies {
		snap.Policies[p.ID] = p
	}
	rows, err := s.query(ctx, `SELECT `+secgwBindingColumns+` FROM secgw_binding b JOIN secgw_policy p ON p.id = b.policy_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		b, err := scanSecgwBinding(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		snap.Bindings = append(snap.Bindings, b)
	}
	return snap, rows.Err()
}

// --- term lists ---------------------------------------------------------------

const secgwTermListColumns = `id, name, match_mode, severity, terms_encrypted, allow_encrypted, term_count, created_by, created_at, updated_at`

func (s *Store) scanSecgwTermList(scan func(...any) error, cipher SecgwCipher, withTerms bool) (*SecgwTermList, error) {
	var l SecgwTermList
	var termsEnc, allowEnc, created, updated string
	if err := scan(&l.ID, &l.Name, &l.MatchMode, &l.Severity, &termsEnc, &allowEnc, &l.TermCount, &l.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	l.CreatedAt, l.UpdatedAt = ParseTime(created), ParseTime(updated)
	if withTerms {
		terms, err := secgwDecryptTerms(cipher, termsEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt terms for list %s: %w", l.ID, err)
		}
		allow, err := secgwDecryptTerms(cipher, allowEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt allowlist for list %s: %w", l.ID, err)
		}
		l.Terms, l.Allow = terms, allow
	}
	return &l, nil
}

func secgwEncryptTerms(cipher SecgwCipher, terms []string) (string, error) {
	if len(terms) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(terms)
	if err != nil {
		return "", err
	}
	if cipher == nil {
		return "", errors.New("term lists require the gateway encryption key (JANUS_ENCRYPTION_KEY)")
	}
	return cipher.Encrypt(string(raw))
}

func secgwDecryptTerms(cipher SecgwCipher, envelope string) ([]string, error) {
	if envelope == "" {
		return nil, nil
	}
	if cipher == nil {
		return nil, errors.New("no cipher configured")
	}
	raw, err := cipher.Decrypt(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSecgwUndecryptable, err)
	}
	var terms []string
	if err := json.Unmarshal([]byte(raw), &terms); err != nil {
		return nil, err
	}
	return terms, nil
}

// ListSecgwTermLists returns metadata for every list. Terms are never
// included here: the list endpoint is visible to any admin and the list is
// itself confidential.
func (s *Store) ListSecgwTermLists(ctx context.Context) ([]*SecgwTermList, error) {
	rows, err := s.query(ctx, `SELECT `+secgwTermListColumns+` FROM secgw_term_list ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SecgwTermList{}
	for rows.Next() {
		l, err := s.scanSecgwTermList(rows.Scan, nil, false)
		if err != nil {
			return nil, fmt.Errorf("scan term list: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LoadSecgwTermLists returns every list WITH decrypted terms, for the
// resolver snapshot and the terms-read capability.
func (s *Store) LoadSecgwTermLists(ctx context.Context, cipher SecgwCipher) ([]*SecgwTermList, error) {
	rows, err := s.query(ctx, `SELECT `+secgwTermListColumns+` FROM secgw_term_list ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SecgwTermList{}
	for rows.Next() {
		l, err := s.scanSecgwTermList(rows.Scan, cipher, true)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SecgwTermListByID loads one list; withTerms decides whether the decrypted
// terms ride along (callers gate that on the terms-read capability).
func (s *Store) SecgwTermListByID(ctx context.Context, id string, cipher SecgwCipher, withTerms bool) (*SecgwTermList, error) {
	l, err := s.scanSecgwTermList(s.queryRow(ctx, `SELECT `+secgwTermListColumns+` FROM secgw_term_list WHERE id = ?`, id).Scan, cipher, withTerms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// CreateSecgwTermList validates, encrypts and persists a list.
func (s *Store) CreateSecgwTermList(ctx context.Context, l *SecgwTermList, cipher SecgwCipher) error {
	if err := l.Validate(); err != nil {
		return err
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM secgw_term_list WHERE LOWER(name) = LOWER(?)`, l.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return &ValidationError{Field: "name", Message: "a term list with that name already exists"}
	}
	termsEnc, err := secgwEncryptTerms(cipher, l.Terms)
	if err != nil {
		return err
	}
	allowEnc, err := secgwEncryptTerms(cipher, l.Allow)
	if err != nil {
		return err
	}
	l.ID = NewID()
	l.CreatedAt = nowUTC()
	l.UpdatedAt = l.CreatedAt
	return s.exec(ctx, `INSERT INTO secgw_term_list (`+secgwTermListColumns+`) VALUES (`+placeholders(10)+`)`,
		l.ID, l.Name, l.MatchMode, l.Severity, termsEnc, allowEnc, l.TermCount, l.CreatedBy, FormatTime(l.CreatedAt), FormatTime(l.UpdatedAt))
}

// UpdateSecgwTermList replaces a list's contents in place.
func (s *Store) UpdateSecgwTermList(ctx context.Context, l *SecgwTermList, cipher SecgwCipher) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if _, err := s.SecgwTermListByID(ctx, l.ID, nil, false); err != nil {
		return err
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM secgw_term_list WHERE LOWER(name) = LOWER(?) AND id <> ?`, l.Name, l.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return &ValidationError{Field: "name", Message: "a term list with that name already exists"}
	}
	termsEnc, err := secgwEncryptTerms(cipher, l.Terms)
	if err != nil {
		return err
	}
	allowEnc, err := secgwEncryptTerms(cipher, l.Allow)
	if err != nil {
		return err
	}
	l.UpdatedAt = nowUTC()
	return s.exec(ctx, `UPDATE secgw_term_list SET name = ?, match_mode = ?, severity = ?, terms_encrypted = ?, allow_encrypted = ?, term_count = ?, updated_at = ? WHERE id = ?`,
		l.Name, l.MatchMode, l.Severity, termsEnc, allowEnc, l.TermCount, FormatTime(l.UpdatedAt), l.ID)
}

// DeleteSecgwTermList removes a list.
func (s *Store) DeleteSecgwTermList(ctx context.Context, id string) error {
	if _, err := s.SecgwTermListByID(ctx, id, nil, false); err != nil {
		return err
	}
	return s.exec(ctx, `DELETE FROM secgw_term_list WHERE id = ?`, id)
}

// --- violations ---------------------------------------------------------------

const secgwViolationColumns = `id, request_id, usage_event_id, user_id, service_token_id, model_name, policy_id, binding_id, kind, rule_id, severity, direction, action, match_offset, match_length, match_hash, match_text_encrypted, classifier_score, classifier_model, created_at`

func scanSecgwViolation(scan func(...any) error) (*SecgwViolation, error) {
	var v SecgwViolation
	var created string
	var matchText sql.NullString
	if err := scan(&v.ID, &v.RequestID, &v.UsageEventID, &v.UserID, &v.ServiceTokenID, &v.ModelName, &v.PolicyID, &v.BindingID,
		&v.Kind, &v.RuleID, &v.Severity, &v.Direction, &v.Action, &v.MatchOffset, &v.MatchLength, &v.MatchHash, &matchText,
		&v.ClassifierScore, &v.ClassifierModel, &created); err != nil {
		return nil, err
	}
	v.CreatedAt = ParseTime(created)
	if matchText.Valid {
		v.SetMatchTextEnvelope(matchText.String)
	}
	return &v, nil
}

// InsertSecgwViolations appends violation rows. Never returns an error to the
// proxy path's caller semantics: the caller logs and moves on, because
// recording a violation must not affect the traffic it observes.
func (s *Store) InsertSecgwViolations(ctx context.Context, vs []*SecgwViolation) error {
	for _, v := range vs {
		if v.ID == "" {
			v.ID = NewID()
		}
		if v.CreatedAt.IsZero() {
			v.CreatedAt = nowUTC()
		}
		var matchText any
		if env := v.MatchTextEnvelope(); env != "" {
			matchText = env
		}
		if err := s.exec(ctx, `INSERT INTO secgw_violation (`+secgwViolationColumns+`) VALUES (`+placeholders(20)+`)`,
			v.ID, v.RequestID, v.UsageEventID, v.UserID, v.ServiceTokenID, v.ModelName, v.PolicyID, v.BindingID,
			v.Kind, v.RuleID, v.Severity, v.Direction, v.Action, v.MatchOffset, v.MatchLength, v.MatchHash, matchText,
			v.ClassifierScore, v.ClassifierModel, FormatTime(v.CreatedAt)); err != nil {
			return err
		}
	}
	return nil
}

// ListSecgwViolations returns violation metadata, newest first. Match text
// is never decrypted here.
func (s *Store) ListSecgwViolations(ctx context.Context, f SecgwViolationFilter) ([]*SecgwViolation, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.UserID != "" {
		where = append(where, "(user_id = ? OR service_token_id = ?)")
		args = append(args, f.UserID, f.UserID)
	}
	if f.RequestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, f.RequestID)
	}
	if f.ModelName != "" {
		where = append(where, "model_name = ?")
		args = append(args, f.ModelName)
	}
	if !f.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, FormatTime(f.Since))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit, f.Offset)
	rows, err := s.query(ctx, `SELECT `+secgwViolationColumns+` FROM secgw_violation WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SecgwViolation{}
	for rows.Next() {
		v, err := scanSecgwViolation(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan violation: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.resolveSecgwViolationLabels(ctx, out)
	return out, nil
}

// resolveSecgwViolationLabels fills the display names on a page of rows
// with one query per referenced table. Labels are for humans reading the
// list; a missing label (deleted user, deleted policy) is simply empty.
func (s *Store) resolveSecgwViolationLabels(ctx context.Context, vs []*SecgwViolation) {
	users, tokens, policies, lists, models := map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
	for _, v := range vs {
		if v.ClassifierModel != "" {
			models[v.ClassifierModel] = ""
		}
		if v.UserID != "" {
			users[v.UserID] = ""
		}
		if v.ServiceTokenID != "" {
			tokens[v.ServiceTokenID] = ""
		}
		if v.PolicyID != "" {
			policies[v.PolicyID] = ""
		}
		if v.Kind == string(SecgwCheckTerms) && v.RuleID != "" {
			lists[v.RuleID] = ""
		}
	}
	fill := func(m map[string]string, query string) {
		for id := range m {
			var label string
			if err := s.queryRow(ctx, query, id).Scan(&label); err == nil {
				m[id] = label
			}
		}
	}
	fill(users, `SELECT CASE WHEN name <> '' THEN name ELSE email END FROM app_user WHERE id = ?`)
	fill(tokens, `SELECT name FROM service_token WHERE id = ?`)
	fill(policies, `SELECT name FROM secgw_policy WHERE id = ?`)
	fill(lists, `SELECT name FROM secgw_term_list WHERE id = ?`)
	fill(models, `SELECT COALESCE(NULLIF(display_name, ''), name) FROM model WHERE id = ?`)
	// Whether each owning policy captures matched text. Only prompt_injection
	// may capture at all, so any other kind is reported as capture-disabled:
	// its text was never eligible to be stored.
	captures := map[string]bool{}
	for _, v := range vs {
		if v.PolicyID == "" {
			continue
		}
		if _, seen := captures[v.PolicyID]; seen {
			continue
		}
		var raw string
		if err := s.queryRow(ctx, `SELECT capture FROM secgw_policy WHERE id = ?`, v.PolicyID).Scan(&raw); err != nil {
			continue
		}
		var cfg SecgwCaptureConfig
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &cfg)
		}
		captures[v.PolicyID] = cfg.PromptInjectionBodiesEnabled()
	}
	for _, v := range vs {
		if label := models[v.ClassifierModel]; label != "" {
			v.ClassifierModel = label
		}
		v.UserLabel = users[v.UserID]
		v.ServiceTokenName = tokens[v.ServiceTokenID]
		v.PolicyName = policies[v.PolicyID]
		v.TermListName = lists[v.RuleID]
		if !v.HasMatchText {
			v.CaptureDisabled = v.Kind != string(SecgwCheckPromptInjection) || !captures[v.PolicyID]
		}
	}
}

// SecgwViolationByID loads one violation; when cipher is non-nil and the row
// carries text, MatchText is decrypted. Callers gate this on the
// violations-read capability and write an ops audit row.
func (s *Store) SecgwViolationByID(ctx context.Context, id string, cipher SecgwCipher) (*SecgwViolation, error) {
	v, err := scanSecgwViolation(s.queryRow(ctx, `SELECT `+secgwViolationColumns+` FROM secgw_violation WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.resolveSecgwViolationLabels(ctx, []*SecgwViolation{v})
	if cipher != nil && v.HasMatchText {
		text, err := cipher.Decrypt(v.MatchTextEnvelope())
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSecgwUndecryptable, err)
		}
		v.MatchText = text
	}
	return v, nil
}

// SecgwViolationCounts summarises violations by kind and action since a
// time, for the admin overview.
func (s *Store) SecgwViolationCounts(ctx context.Context, since time.Time) (map[string]map[string]int, error) {
	rows, err := s.query(ctx, `SELECT kind, action, COUNT(*) FROM secgw_violation WHERE created_at >= ? GROUP BY kind, action`, FormatTime(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]int{}
	for rows.Next() {
		var kind, action string
		var n int
		if err := rows.Scan(&kind, &action, &n); err != nil {
			return nil, err
		}
		if out[kind] == nil {
			out[kind] = map[string]int{}
		}
		out[kind][action] = n
	}
	return out, rows.Err()
}

// PruneSecgwViolations applies a per-kind retention: rows older than
// MaxAgeHours, then the oldest beyond MaxCount. MaxBytes is not applied
// (violation rows are small and bounded by MaxCount).
func (s *Store) PruneSecgwViolations(ctx context.Context, kind string, r SecgwRetention) (int64, error) {
	var total int64
	if r.MaxAgeHours > 0 {
		cutoff := nowUTC().Add(-time.Duration(r.MaxAgeHours) * time.Hour)
		res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM secgw_violation WHERE kind = ? AND created_at < ?`), kind, FormatTime(cutoff))
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if r.MaxCount > 0 {
		res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM secgw_violation WHERE kind = ? AND id IN (
			SELECT id FROM secgw_violation WHERE kind = ? ORDER BY created_at DESC LIMIT -1 OFFSET ?)`), kind, kind, r.MaxCount)
		if err != nil && s.dialect == DialectPostgres {
			// Postgres has no LIMIT -1; use ALL.
			res, err = s.db.ExecContext(ctx, s.rebind(`DELETE FROM secgw_violation WHERE kind = ? AND id IN (
				SELECT id FROM secgw_violation WHERE kind = ? ORDER BY created_at DESC OFFSET ?)`), kind, kind, r.MaxCount)
		}
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}

// SetModelClassifierRole marks or clears a model as a guard classifier.
// Setting a role on a model that is currently granted is refused: the grant
// would otherwise hand a caller direct access to the classifier.
func (s *Store) SetModelClassifierRole(ctx context.Context, id, role string) error {
	switch role {
	case "", ClassifierRoleTextClassification, ClassifierRoleGenerativeGuard:
	default:
		return &ValidationError{Field: "classifier_role", Message: "classifier_role must be empty or one of " + strings.Join(ClassifierRoles(), ", ")}
	}
	m, err := s.ModelByID(ctx, id)
	if err != nil {
		return err
	}
	if role != "" && m.GrantCount == 0 {
		// GrantCount is only populated by ListModels; count directly.
		var n int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM model_grant WHERE model_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return &ValidationError{Field: "classifier_role", Message: "remove this model's grants first: a classifier must never be directly callable"}
		}
		var managed int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM managed_model WHERE target_model_id = ? OR fallback_model_id = ?`, id, id).Scan(&managed); err != nil {
			return err
		}
		if managed > 0 {
			return &ValidationError{Field: "classifier_role", Message: "a managed model points at this model; repoint it first"}
		}
	}
	return s.exec(ctx, `UPDATE model SET classifier_role = ? WHERE id = ?`, role, id)
}

// SecgwClassifierRunsForRequest assembles what the security gateway did on
// one request: every classifier call the gateway made on the caller's
// behalf, paired with whatever those classifiers found.
//
// The join is by request_id, which the gateway stamps on its own classifier
// usage events precisely so the cost and the verdict stay attributable to
// the request that caused them. A run with no findings is the important
// case: it means the classifier answered and the content was clean, which
// exists nowhere in the violation table and is what lets a request's drawer
// distinguish "checked, nothing found" from "never checked".
func (s *Store) SecgwClassifierRunsForRequest(ctx context.Context, requestID string) ([]*SecgwClassifierRun, error) {
	if requestID == "" {
		return nil, nil
	}
	rows, err := s.query(ctx,
		`SELECT model_name, latency_ms, http_status FROM usage_event
		 WHERE request_id = ? AND client_app = ? ORDER BY created_at`, requestID, InternalClientApp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []*SecgwClassifierRun{}
	for rows.Next() {
		var r SecgwClassifierRun
		if err := rows.Scan(&r.ModelName, &r.LatencyMs, &r.HTTPStatus); err != nil {
			return nil, err
		}
		r.Findings = []*SecgwViolation{}
		runs = append(runs, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return runs, nil
	}

	// Attach findings by classifier. A violation names the classifier model
	// that produced it (classifier_model holds the model ID), so resolve
	// those to names once and match on the name the run reports.
	vs, err := s.ListSecgwViolations(ctx, SecgwViolationFilter{RequestID: requestID, Limit: 200})
	if err != nil {
		return nil, err
	}
	byModel := map[string][]*SecgwViolation{}
	for _, v := range vs {
		if v.ClassifierModel == "" {
			continue
		}
		name := v.ClassifierModel
		if m, err := s.ModelByID(ctx, v.ClassifierModel); err == nil {
			name = m.PublicName()
		}
		byModel[name] = append(byModel[name], v)
	}
	for _, r := range runs {
		if found, ok := byModel[r.ModelName]; ok {
			r.Findings = found
			r.Kind = found[0].Kind
			r.Direction = found[0].Direction
		}
	}
	return runs, nil
}
