package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const managedModelColumns = `id, name, description, target_model_id, status, created_by_user_id, created_at, updated_at, fallback_model_id, fallback_triggers`

func scanManagedModel(scan func(...any) error) (*ManagedModel, error) {
	var m ManagedModel
	var created, updated, triggers string
	if err := scan(&m.ID, &m.Name, &m.Description, &m.TargetModelID, &m.Status, &m.CreatedBy, &created, &updated,
		&m.FallbackModelID, &triggers); err != nil {
		return nil, err
	}
	m.CreatedAt = ParseTime(created)
	m.UpdatedAt = ParseTime(updated)
	m.FallbackTriggers = decodeFallbackTriggers(m.FallbackModelID, triggers)
	return &m, nil
}

// decodeFallbackTriggers turns the stored JSON array into the in-memory list.
// A fallback with no stored triggers (older rows, or an admin who never chose)
// gets the defaults; no fallback means no triggers whatever is stored.
func decodeFallbackTriggers(fallbackModelID, raw string) []string {
	if fallbackModelID == "" {
		return []string{}
	}
	var out []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	out = normalizeFallbackTriggers(out)
	if len(out) == 0 {
		return append([]string{}, DefaultFallbackTriggers...)
	}
	return out
}

// normalizeFallbackTriggers drops blanks and duplicates and orders the list
// canonically (AllFallbackTriggers order). Unknown values are kept so the
// validator can name them.
func normalizeFallbackTriggers(in []string) []string {
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			seen[t] = true
		}
	}
	out := make([]string, 0, len(seen))
	for _, t := range AllFallbackTriggers {
		if seen[t] {
			out = append(out, t)
			delete(seen, t)
		}
	}
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out[len(out)-len(seen):])
	return out
}

// validateFallbackTriggers rejects unknown failure modes.
func validateFallbackTriggers(triggers []string) error {
	known := map[string]bool{}
	for _, t := range AllFallbackTriggers {
		known[t] = true
	}
	for _, t := range triggers {
		if !known[t] {
			return &ValidationError{Field: "fallback_triggers", Message: fmt.Sprintf("unknown failure mode %q; choose from %s", t, strings.Join(AllFallbackTriggers, ", "))}
		}
	}
	return nil
}

// assertValidFallback checks a fallback selection: it must be a real,
// existing catalog model (never a managed model — aliases do not chain, and a
// fallback that is itself an alias with a fallback would be a chain) and it
// must differ from the target, since falling back to the model that just
// failed is not a fallback.
func (s *Store) assertValidFallback(ctx context.Context, targetModelID, fallbackModelID string) error {
	fallbackModelID = strings.TrimSpace(fallbackModelID)
	if fallbackModelID == "" {
		return nil
	}
	if fallbackModelID == targetModelID {
		return &ValidationError{Field: "fallback_model_id", Message: "the fallback must be a different model from the target"}
	}
	if _, err := s.ManagedModelByID(ctx, fallbackModelID); err == nil {
		return &ValidationError{Field: "fallback_model_id", Message: "the fallback must be a real catalog model, not another managed model"}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, err := s.ModelByID(ctx, fallbackModelID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return &ValidationError{Field: "fallback_model_id", Message: "that fallback model does not exist"}
		}
		return err
	}
	return nil
}

// SetManagedModelFallback configures or clears (empty fallbackModelID) the
// alias's fallback. Triggers are normalised; an empty list with a fallback set
// means DefaultFallbackTriggers.
func (s *Store) SetManagedModelFallback(ctx context.Context, id, fallbackModelID string, triggers []string) error {
	m, err := s.ManagedModelByID(ctx, id)
	if err != nil {
		return err
	}
	fallbackModelID = strings.TrimSpace(fallbackModelID)
	if err := s.assertValidFallback(ctx, m.TargetModelID, fallbackModelID); err != nil {
		return err
	}
	triggers = normalizeFallbackTriggers(triggers)
	if err := validateFallbackTriggers(triggers); err != nil {
		return err
	}
	encoded := ""
	if fallbackModelID == "" {
		triggers = nil
	} else {
		if len(triggers) == 0 {
			triggers = append([]string{}, DefaultFallbackTriggers...)
		}
		b, err := json.Marshal(triggers)
		if err != nil {
			return fmt.Errorf("encode fallback triggers: %w", err)
		}
		encoded = string(b)
	}
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE managed_model SET fallback_model_id = ?, fallback_triggers = ?, updated_at = ? WHERE id = ?`),
		fallbackModelID, encoded, FormatTime(nowUTC()), id)
	if err != nil {
		return fmt.Errorf("set managed model fallback: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrManagedModelNameTaken reports that a managed model's name collides with
// another managed model, or with a real model's native or display name.
//
// The namespace has to be shared: ModelByName resolves a caller-supplied model
// string against real models AND managed models, so allowing a duplicate would
// make routing ambiguous — a request for "best-coder" could mean two things.
var ErrManagedModelNameTaken = errors.New("that model name is already in use")

// NormalizeRoutableName trims, lowercases, and validates a name that API
// callers address in a request body's "model" field. Both real-model display
// names (renames) and managed-model aliases go through it, because both are
// resolved from the same field: two different rulesets for one namespace is
// how you end up able to save a name that cannot be called.
//
// The charset deliberately matches what public model catalogues actually use —
// lowercase letters, digits, and the separators . _ - : / — so a name is
// always safe to paste into a config file, a URL, or a shell command without
// quoting or escaping. Uppercase input is folded to lowercase rather than
// rejected: an admin typing "GPT-4o-Mini" means the same model as
// "gpt-4o-mini", and silently accepting-then-failing is the bug being fixed.
func NormalizeRoutableName(name, field string, maxLen int) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", &ValidationError{Field: field, Message: "give it a name; callers address it by this name"}
	}
	if len(name) > maxLen {
		return "", &ValidationError{Field: field, Message: fmt.Sprintf("names are limited to %d characters", maxLen)}
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == ':' || r == '/':
		case r == ' ':
			return "", &ValidationError{Field: field, Message: "names cannot contain spaces — use a hyphen instead, for example current-best"}
		default:
			return "", &ValidationError{Field: field, Message: "names can use lowercase letters, digits, and . _ - : / only"}
		}
	}
	return name, nil
}

// normalizeManagedModelName trims and validates an alias name. Aliases are
// addressed by API callers in the request body's "model" field, so they are
// constrained like a model identifier — see NormalizeRoutableName.
func normalizeManagedModelName(name string) (string, error) {
	return NormalizeRoutableName(name, "name", 100)
}

// assertManagedNameAvailable rejects a name that would make model routing
// ambiguous. It checks both directions of the shared namespace: other managed
// models, and the native/display names of every ENABLED real model (the same
// set ModelByName resolves against). excludeID lets an update keep its own
// name. Disabled, stale, and pending models do not participate in routing and
// therefore do not block an alias name — mirroring SetModelDisplayName.
func (s *Store) assertManagedNameAvailable(ctx context.Context, name, excludeID string) error {
	var existingID string
	err := s.queryRow(ctx, `SELECT id FROM managed_model WHERE LOWER(name) = LOWER(?)`, name).Scan(&existingID)
	if err == nil && existingID != excludeID {
		return ErrManagedModelNameTaken
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check managed model name: %w", err)
	}
	enabled, err := s.ListModels(ctx, ModelFilter{Status: ModelEnabled})
	if err != nil {
		return err
	}
	for _, m := range enabled {
		if strings.EqualFold(m.Name, name) || (m.DisplayName != "" && strings.EqualFold(m.DisplayName, name)) {
			return &ValidationError{
				Field: "name",
				Message: fmt.Sprintf("the name %q is already used by enabled model %q on upstream %q",
					name, m.Name, m.UpstreamName),
			}
		}
	}
	return nil
}

// assertValidTarget checks that a proposed target is a real, existing catalog
// model. Aliases never chain: a managed model may only point at a real model,
// so an alias can always be resolved in exactly one hop and an admin can never
// build a cycle.
func (s *Store) assertValidTarget(ctx context.Context, targetModelID string) error {
	if strings.TrimSpace(targetModelID) == "" {
		return &ValidationError{Field: "target_model_id", Message: "choose the model this alias should point at"}
	}
	if _, err := s.ModelByID(ctx, targetModelID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return &ValidationError{Field: "target_model_id", Message: "that model does not exist"}
		}
		return err
	}
	return nil
}

// CreateManagedModel defines a new alias pointing at a real catalog model.
func (s *Store) CreateManagedModel(ctx context.Context, name, description, targetModelID, createdByUserID string) (*ManagedModel, error) {
	name, err := normalizeManagedModelName(name)
	if err != nil {
		return nil, err
	}
	if len(description) > 500 {
		return nil, &ValidationError{Field: "description", Message: "descriptions are limited to 500 characters"}
	}
	if err := s.assertValidTarget(ctx, targetModelID); err != nil {
		return nil, err
	}
	if err := s.assertManagedNameAvailable(ctx, name, ""); err != nil {
		return nil, err
	}
	now := nowUTC()
	m := &ManagedModel{
		ID: NewID(), Name: name, Description: strings.TrimSpace(description),
		TargetModelID: targetModelID, Status: ManagedModelEnabled,
		CreatedBy: createdByUserID, CreatedAt: now, UpdatedAt: now,
	}
	err = s.exec(ctx, `INSERT INTO managed_model (`+managedModelColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Name, m.Description, m.TargetModelID, m.Status, m.CreatedBy, FormatTime(m.CreatedAt), FormatTime(m.UpdatedAt), "", "")
	m.FallbackTriggers = []string{}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrManagedModelNameTaken
		}
		return nil, err
	}
	return s.decorateManagedModel(ctx, m)
}

// ManagedModelFilter narrows a managed-model listing.
type ManagedModelFilter struct {
	Status string
	Search string
	// TargetModelID lists only the aliases pointing at one real model, which
	// is how the admin UI warns before disabling or deleting that model.
	TargetModelID string
	// TargetUpstreamID lists the aliases whose target lives on one upstream —
	// the blast radius of deleting that upstream, since every such alias
	// keeps resolving by name but can no longer be served once its target is
	// disabled with the upstream.
	TargetUpstreamID string
}

// ListManagedModels returns aliases with their targets resolved.
func (s *Store) ListManagedModels(ctx context.Context, f ManagedModelFilter) ([]*ManagedModel, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.TargetModelID != "" {
		where = append(where, "target_model_id = ?")
		args = append(args, f.TargetModelID)
	}
	if f.TargetUpstreamID != "" {
		where = append(where, "target_model_id IN (SELECT id FROM model WHERE upstream_id = ?)")
		args = append(args, f.TargetUpstreamID)
	}
	if f.Search != "" {
		where = append(where, "(LOWER(name) LIKE ? OR LOWER(description) LIKE ?)")
		needle := "%" + strings.ToLower(f.Search) + "%"
		args = append(args, needle, needle)
	}
	rows, err := s.query(ctx, `SELECT `+managedModelColumns+` FROM managed_model WHERE `+
		strings.Join(where, " AND ")+` ORDER BY name ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*ManagedModel{}
	for rows.Next() {
		m, err := scanManagedModel(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan managed model: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range out {
		if _, err := s.decorateManagedModel(ctx, m); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ManagedModelByID loads one alias with its target resolved.
func (s *Store) ManagedModelByID(ctx context.Context, id string) (*ManagedModel, error) {
	m, err := scanManagedModel(s.queryRow(ctx, `SELECT `+managedModelColumns+` FROM managed_model WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load managed model: %w", err)
	}
	return s.decorateManagedModel(ctx, m)
}

// ManagedModelByName resolves an alias by the name callers address it with.
func (s *Store) ManagedModelByName(ctx context.Context, name string) (*ManagedModel, error) {
	// Aliases are stored lowercase, but a caller may send any case — match
	// case-insensitively so "Current-Best" reaches "current-best".
	m, err := scanManagedModel(s.queryRow(ctx, `SELECT `+managedModelColumns+` FROM managed_model WHERE LOWER(name) = LOWER(?)`, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load managed model by name: %w", err)
	}
	return s.decorateManagedModel(ctx, m)
}

// decorateManagedModel resolves the target's presentation and health onto the
// alias. A missing or non-enabled target is not an error — it is a *broken*
// alias, which the admin UI must be able to see and repair. Nothing here is
// persisted; every field is derived at read time so a target change is
// reflected immediately.
func (s *Store) decorateManagedModel(ctx context.Context, m *ManagedModel) (*ManagedModel, error) {
	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM model_grant WHERE model_id = ? AND model_kind = ?`,
		m.ID, ModelKindManaged).Scan(&m.GrantCount); err != nil {
		return nil, fmt.Errorf("count managed model grants: %w", err)
	}
	target, err := s.ModelByID(ctx, m.TargetModelID)
	if errors.Is(err, ErrNotFound) {
		m.Broken = true
		m.BrokenReason = "the underlying model no longer exists in the catalog"
		m.Servable = false
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	m.TargetName = target.Name
	m.TargetDisplayName = target.DisplayName
	m.TargetPublicName = target.PublicName()
	m.TargetStatus = target.Status
	m.TargetUpstreamID = target.UpstreamID
	m.TargetUpstreamName = target.UpstreamName
	m.Modalities = target.Modalities
	m.ContextWindow = target.ContextWindow
	if target.Status != ModelEnabled {
		m.Broken = true
		m.BrokenReason = fmt.Sprintf("the underlying model %q is %s, so requests to this alias will fail",
			target.PublicName(), target.Status)
		if m.FallbackModelID != "" {
			m.BrokenReason = fmt.Sprintf("the underlying model %q is %s; requests to this alias are served by its fallback",
				target.PublicName(), target.Status)
		}
	}
	m.Servable = m.Status == ManagedModelEnabled && !m.Broken
	if err := s.decorateManagedModelFallback(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// decorateManagedModelFallback resolves the fallback's presentation fields.
// A usable fallback also rescues Servable when the target is broken, because
// that is precisely the situation a fallback exists for.
func (s *Store) decorateManagedModelFallback(ctx context.Context, m *ManagedModel) error {
	if m.FallbackModelID == "" {
		return nil
	}
	fb, err := s.ModelByID(ctx, m.FallbackModelID)
	if errors.Is(err, ErrNotFound) {
		m.FallbackBroken = true
		m.FallbackBrokenReason = "the fallback model no longer exists in the catalog"
		return nil
	}
	if err != nil {
		return err
	}
	m.FallbackName = fb.Name
	m.FallbackPublicName = fb.PublicName()
	m.FallbackStatus = fb.Status
	m.FallbackUpstreamID = fb.UpstreamID
	m.FallbackUpstreamName = fb.UpstreamName
	if fb.Status != ModelEnabled {
		m.FallbackBroken = true
		m.FallbackBrokenReason = fmt.Sprintf("the fallback model %q is %s and cannot take over", fb.PublicName(), fb.Status)
	}
	if m.Broken && !m.FallbackBroken && m.HasFallbackTrigger(FallbackTriggerTargetUnavailable) {
		m.Servable = m.Status == ManagedModelEnabled
	}
	return nil
}

// UpdateManagedModel changes an alias's presentation metadata. The target is
// changed separately (SetManagedModelTarget) so a repoint is always its own
// audited event rather than an incidental side effect of an edit.
func (s *Store) UpdateManagedModel(ctx context.Context, id, name, description string) error {
	name, err := normalizeManagedModelName(name)
	if err != nil {
		return err
	}
	if len(description) > 500 {
		return &ValidationError{Field: "description", Message: "descriptions are limited to 500 characters"}
	}
	if _, err := s.ManagedModelByID(ctx, id); err != nil {
		return err
	}
	if err := s.assertManagedNameAvailable(ctx, name, id); err != nil {
		return err
	}
	res, execErr := s.db.ExecContext(ctx, s.rebind(
		`UPDATE managed_model SET name = ?, description = ?, updated_at = ? WHERE id = ?`),
		name, strings.TrimSpace(description), FormatTime(nowUTC()), id)
	if execErr != nil {
		if isUniqueViolation(execErr) {
			return ErrManagedModelNameTaken
		}
		return fmt.Errorf("update managed model: %w", execErr)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetManagedModelTarget repoints an alias at a different real model. This is
// the whole point of the feature: users keep addressing the same name while
// the admin swaps what it means. In-flight requests are unaffected — they
// already resolved their model — and the next request uses the new target.
func (s *Store) SetManagedModelTarget(ctx context.Context, id, targetModelID string) error {
	if _, err := s.ManagedModelByID(ctx, id); err != nil {
		return err
	}
	if err := s.assertValidTarget(ctx, targetModelID); err != nil {
		return err
	}
	if m, err := s.ManagedModelByID(ctx, id); err == nil && m.FallbackModelID != "" && m.FallbackModelID == targetModelID {
		return &ValidationError{Field: "target_model_id", Message: "that model is this alias's fallback; choose a different target or change the fallback first"}
	}
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE managed_model SET target_model_id = ?, updated_at = ? WHERE id = ?`),
		targetModelID, FormatTime(nowUTC()), id)
	if err != nil {
		return fmt.Errorf("repoint managed model: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetManagedModelStatus enables or disables an alias. A disabled alias keeps
// its grants and history but stops being offered and stops resolving.
func (s *Store) SetManagedModelStatus(ctx context.Context, id, status string) error {
	if status != ManagedModelEnabled && status != ManagedModelDisabled {
		return &ValidationError{Field: "status", Message: "status must be enabled or disabled"}
	}
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE managed_model SET status = ?, updated_at = ? WHERE id = ?`),
		status, FormatTime(nowUTC()), id)
	if err != nil {
		return fmt.Errorf("update managed model status: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteManagedModel removes an alias and every grant attached to it.
//
// Unlike a real model, an alias carries no usage history of its own — usage is
// always recorded against the underlying model — so deleting one loses no
// metering data. Requests addressed to the deleted name stop resolving, which
// is exactly what an admin retiring an alias intends.
func (s *Store) DeleteManagedModel(ctx context.Context, id string) error {
	if _, err := s.ManagedModelByID(ctx, id); err != nil {
		return err
	}
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`DELETE FROM model_grant WHERE model_id = ? AND model_kind = ?`), id, ModelKindManaged); err != nil {
			return fmt.Errorf("delete managed model grants: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM managed_model WHERE id = ?`), id); err != nil {
			return fmt.Errorf("delete managed model: %w", err)
		}
		return nil
	})
}

// ManagedModelNames returns every alias name currently in use. The proxy's
// catalog filter uses it so alias names are never mistaken for uncatalogued
// model names on usage graphs.
func (s *Store) ManagedModelNames(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.query(ctx, `SELECT name FROM managed_model`)
	if err != nil {
		return nil, fmt.Errorf("load managed model names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan managed model name: %w", err)
		}
		// Keyed lowercase so callers can compare case-insensitively; the
		// shared namespace must not depend on how a name was typed.
		out[strings.ToLower(name)] = struct{}{}
	}
	return out, rows.Err()
}
