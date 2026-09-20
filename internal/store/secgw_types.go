package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Security Gateway types. A Policy is a named bundle of checks; a
// PolicyBinding attaches a policy to a scope (the whole org, a group, a
// service token, a model or a managed model). Bindings are a sibling of
// Grant, never a field on it: grants answer "may this caller use this model",
// policies answer "what rules apply to this traffic", and conflating the two
// makes "block secrets org-wide" a per-(model × grantee) edit with a
// service-token bypass built into the data model.

// SecgwCheckKind names a check family.
type SecgwCheckKind string

const (
	SecgwCheckSecrets         SecgwCheckKind = "secrets"
	SecgwCheckPII             SecgwCheckKind = "pii"
	SecgwCheckTerms           SecgwCheckKind = "terms"
	SecgwCheckShape           SecgwCheckKind = "shape"
	SecgwCheckPromptInjection SecgwCheckKind = "prompt_injection"
	// SecgwCheckContentSafety is the Llama Guard style hazard check: a
	// generative guard judges each conversation turn against the MLCommons
	// taxonomy and the policy names which categories to enforce.
	SecgwCheckContentSafety SecgwCheckKind = "content_safety"
)

// SecgwCheckKinds lists every kind in a stable order for UIs and validation.
func SecgwCheckKinds() []SecgwCheckKind {
	return []SecgwCheckKind{SecgwCheckShape, SecgwCheckSecrets, SecgwCheckPII, SecgwCheckTerms, SecgwCheckPromptInjection, SecgwCheckContentSafety}
}

// Modes, fail stances, directions, scopes and actions.
const (
	SecgwModeObserve = "observe"
	SecgwModeRedact  = "redact"
	SecgwModeBlock   = "block"

	SecgwFailClosed = "closed"
	SecgwFailOpen   = "open"

	SecgwDirectionIngress = "ingress"
	SecgwDirectionEgress  = "egress"
	SecgwDirectionBoth    = "both"

	SecgwScopeOrg          = "org"
	SecgwScopeGroup        = "group"
	SecgwScopeServiceToken = "service_token"
	SecgwScopeUpstream     = "upstream"
	SecgwScopeModel        = "model"
	SecgwScopeManagedModel = "managed_model"

	// SecgwActionChecked records that a policy applied and every check
	// passed. Without it a clean request is indistinguishable from one no
	// policy covered (both have no violations and no action), so the request
	// log cannot answer "was this inspected?" — the question an auditor
	// actually asks. Ranked below observed: any real finding overwrites it.
	SecgwActionChecked   = "checked"
	SecgwActionObserved  = "observed"
	SecgwActionRedacted  = "redacted"
	SecgwActionBlocked   = "blocked"
	SecgwActionStreamCut = "stream_cut"

	// SecgwDefaultHoldBytes is the streaming egress hold-back window: the
	// trailing plaintext withheld from the client so a match can be caught
	// before the bytes leave. 256 bytes is a couple of words of lag and is
	// enough for every deterministic rule.
	SecgwDefaultHoldBytes = 256

	// ClassifierRoleTextClassification marks a catalog model as a
	// text-classification guard (Prompt Guard style). Any non-empty
	// classifier_role removes the model from the servable catalog and from
	// grant targeting: a caller who can invoke the classifier directly has
	// free oracle access to the thing judging them.
	ClassifierRoleTextClassification = "text_classification"
	// ClassifierRoleGenerativeGuard marks a catalog model as a
	// conversation guard (Llama Guard style): a chat model whose reply is
	// a safe/unsafe verdict with hazard categories.
	ClassifierRoleGenerativeGuard = "generative_guard"
)

// ClassifierRoles lists every role a model may carry, for validation and
// the admin UI.
func ClassifierRoles() []string {
	return []string{ClassifierRoleTextClassification, ClassifierRoleGenerativeGuard}
}

// ClassifierRoleForKind names which classifier protocol a model-backed
// check requires. Empty for deterministic kinds.
func ClassifierRoleForKind(k SecgwCheckKind) string {
	switch k {
	case SecgwCheckPromptInjection:
		return ClassifierRoleTextClassification
	case SecgwCheckContentSafety:
		return ClassifierRoleGenerativeGuard
	}
	return ""
}

// SecgwScopeRank orders scopes from least to most specific. Resolution walks
// this order and later scopes override earlier ones (subject to the
// mandatory floor).
var SecgwScopeRank = map[string]int{
	SecgwScopeOrg: 0, SecgwScopeGroup: 1, SecgwScopeUpstream: 2, SecgwScopeModel: 3, SecgwScopeManagedModel: 4, SecgwScopeServiceToken: 5,
}

// SecgwCheck is one check inside a policy.
type SecgwCheck struct {
	Kind      SecgwCheckKind `json:"kind"`
	Enabled   bool           `json:"enabled"`
	Mode      string         `json:"mode"`
	Fail      string         `json:"fail,omitempty"`
	Direction string         `json:"direction"`
	// Options is kind-specific configuration, validated by the detector or
	// classifier that owns the kind (secrets: disable_rules/extra_rules_toml;
	// pii: classes/allow_bare_ssn; terms: term_list_ids; shape: limits;
	// prompt_injection: threshold/chunk_tokens/timeout_ms;
	// content_safety: categories/timeout_ms).
	Options json.RawMessage `json:"options,omitempty"`
	// ClassifierModelID names the catalog model backing a model-backed
	// check. Empty for deterministic kinds.
	ClassifierModelID string `json:"classifier_model_id,omitempty"`
	// HoldBytes overrides SecgwDefaultHoldBytes for streaming egress. 0 =
	// default. A more specific binding may raise it, never lower it.
	HoldBytes int `json:"hold_bytes,omitempty"`
}

// SecgwCaptureConfig controls what a violation row may retain, per kind.
// The capture matrix is fixed for the kinds where the evidence IS the harm:
// secrets and terms never store the matched text and that is enforced in
// Validate, not left to the UI. Only prompt_injection may capture bodies.
type SecgwCaptureConfig struct {
	// PromptInjectionBodies stores the full matched message(s) for
	// prompt_injection violations. Default true: the attack text is the
	// intelligence, and the corpus is what tunes the classifier.
	PromptInjectionBodies *bool `json:"prompt_injection_bodies,omitempty"`
}

// PromptInjectionBodiesEnabled resolves the default.
func (c SecgwCaptureConfig) PromptInjectionBodiesEnabled() bool {
	return c.PromptInjectionBodies == nil || *c.PromptInjectionBodies
}

// SecgwPolicy is a named bundle of checks.
type SecgwPolicy struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// Mandatory: when bound at org scope, more specific bindings may add
	// checks or tighten a check's mode/fail, never relax or remove one.
	Mandatory bool               `json:"mandatory"`
	Checks    []SecgwCheck       `json:"checks"`
	Capture   SecgwCaptureConfig `json:"capture"`
	// SyntheticRefusal, when true, answers a blocked request with HTTP 200
	// and a well-formed completion whose finish_reason is content_filter
	// instead of a 403 error object — for clients that render errors badly.
	SyntheticRefusal bool      `json:"synthetic_refusal"`
	RefusalText      string    `json:"refusal_text,omitempty"`
	CreatedBy        string    `json:"created_by_user_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// BindingCount is resolved at read time for the admin list.
	BindingCount int `json:"binding_count"`
}

// secgwNameRE admits what people actually type into a name field: letters
// in any script, digits, spaces, and the ordinary punctuation of a title
// (. , : ; ' & / ( ) plus hyphen, en/em dash and underscore). Control
// characters and anything that reads as markup are out.
var secgwNameRE = regexp.MustCompile(`^[\pL\pN][\pL\pN .,:;'&/()_\-–—]{0,79}$`)

// Validate normalises defaults and rejects impossible policies.
func (p *SecgwPolicy) Validate() error {
	p.Name = strings.TrimSpace(p.Name)
	if !secgwNameRE.MatchString(p.Name) {
		return &ValidationError{Field: "name", Message: "give the policy a name of 1–80 characters: letters, digits, spaces and ordinary punctuation"}
	}
	if len(p.Checks) == 0 {
		return &ValidationError{Field: "checks", Message: "a policy needs at least one check"}
	}
	seen := map[SecgwCheckKind]bool{}
	for i := range p.Checks {
		c := &p.Checks[i]
		if !secgwKnownKind(c.Kind) {
			return &ValidationError{Field: fmt.Sprintf("checks[%d].kind", i), Message: "unknown check kind " + string(c.Kind)}
		}
		if seen[c.Kind] {
			return &ValidationError{Field: fmt.Sprintf("checks[%d].kind", i), Message: "check kind " + string(c.Kind) + " appears twice"}
		}
		seen[c.Kind] = true
		switch c.Mode {
		case "":
			c.Mode = SecgwModeObserve
		case SecgwModeObserve, SecgwModeBlock:
		case SecgwModeRedact:
			if c.Kind == SecgwCheckPromptInjection || c.Kind == SecgwCheckContentSafety || c.Kind == SecgwCheckShape {
				return &ValidationError{Field: fmt.Sprintf("checks[%d].mode", i), Message: string(c.Kind) + " cannot redact: it has no span to replace. Use observe or block."}
			}
		default:
			return &ValidationError{Field: fmt.Sprintf("checks[%d].mode", i), Message: "mode must be observe, redact or block"}
		}
		switch c.Fail {
		case "":
			c.Fail = SecgwFailClosed
		case SecgwFailClosed, SecgwFailOpen:
		default:
			return &ValidationError{Field: fmt.Sprintf("checks[%d].fail", i), Message: "fail must be closed or open"}
		}
		switch c.Direction {
		case "":
			c.Direction = SecgwDirectionIngress
		case SecgwDirectionIngress, SecgwDirectionEgress, SecgwDirectionBoth:
		default:
			return &ValidationError{Field: fmt.Sprintf("checks[%d].direction", i), Message: "direction must be ingress, egress or both"}
		}
		if c.Kind == SecgwCheckShape && c.Direction != SecgwDirectionIngress {
			return &ValidationError{Field: fmt.Sprintf("checks[%d].direction", i), Message: "shape limits apply to requests only"}
		}
		if role := ClassifierRoleForKind(c.Kind); role != "" {
			// A disabled check only has to be well-formed, not runnable: the
			// editor round-trips every kind so "off" ones keep their settings,
			// and an org with no classifier yet must still be able to save a
			// policy that blocks secrets. The classifier is required the
			// moment the check is switched on.
			if c.ClassifierModelID == "" && c.Enabled {
				return &ValidationError{Field: fmt.Sprintf("checks[%d].classifier_model_id", i), Message: string(c.Kind) + " needs a classifier model"}
			}
			switch c.Kind {
			case SecgwCheckPromptInjection:
				if c.Direction != SecgwDirectionIngress {
					return &ValidationError{Field: fmt.Sprintf("checks[%d].direction", i), Message: "prompt_injection runs on requests only in this version"}
				}
			case SecgwCheckContentSafety:
				// v1 ruling: egress content safety is observe-only. A
				// generative verdict cannot be produced inside a streaming
				// hold-back window, and blocking a buffered response while
				// streaming ones pass would be a control that only works
				// when the caller happens not to stream. Say so at save
				// time rather than let an admin believe they have it.
				if c.Mode == SecgwModeBlock && c.Direction != SecgwDirectionIngress {
					return &ValidationError{Field: fmt.Sprintf("checks[%d].mode", i), Message: "content_safety can only block on ingress in this version; egress responses are observed and logged, never blocked"}
				}
			}
		} else if c.ClassifierModelID != "" {
			return &ValidationError{Field: fmt.Sprintf("checks[%d].classifier_model_id", i), Message: string(c.Kind) + " is deterministic and takes no classifier"}
		}
		if c.HoldBytes < 0 || c.HoldBytes > 1<<20 {
			return &ValidationError{Field: fmt.Sprintf("checks[%d].hold_bytes", i), Message: "hold_bytes must be between 0 and 1048576"}
		}
		if len(c.Options) > 0 && !json.Valid(c.Options) {
			return &ValidationError{Field: fmt.Sprintf("checks[%d].options", i), Message: "options must be a JSON object"}
		}
	}
	if p.SyntheticRefusal && strings.TrimSpace(p.RefusalText) == "" {
		p.RefusalText = "This request was declined by your organisation's security policy."
	}
	if len(p.RefusalText) > 2000 {
		return &ValidationError{Field: "refusal_text", Message: "refusal_text must be at most 2000 characters"}
	}
	return nil
}

func secgwKnownKind(k SecgwCheckKind) bool {
	for _, known := range SecgwCheckKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// SecgwBinding attaches a policy to a scope.
type SecgwBinding struct {
	ID        string    `json:"id"`
	PolicyID  string    `json:"policy_id"`
	ScopeType string    `json:"scope_type"`
	ScopeID   string    `json:"scope_id"`
	CreatedBy string    `json:"created_by_user_id"`
	CreatedAt time.Time `json:"created_at"`
	// Resolved at read time for the admin list.
	PolicyName string `json:"policy_name,omitempty"`
	ScopeName  string `json:"scope_name,omitempty"`
}

// Validate checks the scope vocabulary.
func (b *SecgwBinding) Validate() error {
	if _, ok := SecgwScopeRank[b.ScopeType]; !ok {
		return &ValidationError{Field: "scope_type", Message: "scope_type must be org, group, upstream, service_token, model or managed_model"}
	}
	b.ScopeID = strings.TrimSpace(b.ScopeID)
	if b.ScopeType == SecgwScopeOrg {
		b.ScopeID = ""
	} else if b.ScopeID == "" {
		return &ValidationError{Field: "scope_id", Message: "scope_id is required for scope_type " + b.ScopeType}
	}
	if b.PolicyID == "" {
		return &ValidationError{Field: "policy_id", Message: "policy_id is required"}
	}
	return nil
}

// Term-list match modes.
const (
	SecgwTermExact     = "exact"
	SecgwTermSubstring = "substring"
	SecgwTermRegex     = "regex"
	SecgwTermFuzzy     = "fuzzy"
)

// SecgwTermList is a customer-supplied dictionary: codenames, client names,
// classification markings. It is itself confidential — Terms and Allow are
// encrypted at rest and the list endpoint never returns them in full.
type SecgwTermList struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MatchMode string    `json:"match_mode"`
	Severity  string    `json:"severity"`
	Terms     []string  `json:"terms,omitempty"`
	Allow     []string  `json:"allow,omitempty"`
	TermCount int       `json:"term_count"`
	CreatedBy string    `json:"created_by_user_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate normalises a term list.
func (l *SecgwTermList) Validate() error {
	l.Name = strings.TrimSpace(l.Name)
	if !secgwNameRE.MatchString(l.Name) {
		return &ValidationError{Field: "name", Message: "give the list a name of 1–80 characters: letters, digits, spaces and ordinary punctuation"}
	}
	switch l.MatchMode {
	case "":
		l.MatchMode = SecgwTermExact
	case SecgwTermExact, SecgwTermSubstring, SecgwTermRegex, SecgwTermFuzzy:
	default:
		return &ValidationError{Field: "match_mode", Message: "match_mode must be exact, substring, regex or fuzzy"}
	}
	switch l.Severity {
	case "":
		l.Severity = "high"
	case "low", "medium", "high", "critical":
	default:
		return &ValidationError{Field: "severity", Message: "severity must be low, medium, high or critical"}
	}
	l.Terms = secgwCleanTerms(l.Terms)
	l.Allow = secgwCleanTerms(l.Allow)
	if len(l.Terms) == 0 {
		return &ValidationError{Field: "terms", Message: "the list needs at least one term"}
	}
	if len(l.Terms) > 50_000 {
		return &ValidationError{Field: "terms", Message: "a list may hold at most 50000 terms"}
	}
	for _, t := range l.Terms {
		if len(t) > 512 {
			return &ValidationError{Field: "terms", Message: "a term may be at most 512 characters"}
		}
		if l.MatchMode == SecgwTermRegex {
			if _, err := regexp.Compile(t); err != nil {
				return &ValidationError{Field: "terms", Message: fmt.Sprintf("invalid pattern %q: %v", t, err)}
			}
		}
	}
	l.TermCount = len(l.Terms)
	return nil
}

func secgwCleanTerms(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// SecgwViolation is one matched check on one request. MatchText is NULL
// unless the kind is capture-eligible; MatchHash is always set so "same
// secret again?" is answerable without storing the secret.
type SecgwViolation struct {
	ID              string    `json:"id"`
	RequestID       string    `json:"request_id"`
	UsageEventID    string    `json:"usage_event_id"`
	UserID          string    `json:"user_id,omitempty"`
	ServiceTokenID  string    `json:"service_token_id,omitempty"`
	ModelName       string    `json:"model_name"`
	PolicyID        string    `json:"policy_id"`
	BindingID       string    `json:"binding_id"`
	Kind            string    `json:"kind"`
	RuleID          string    `json:"rule_id"`
	Severity        string    `json:"severity"`
	Direction       string    `json:"direction"`
	Action          string    `json:"action"`
	MatchOffset     int       `json:"match_offset"`
	MatchLength     int       `json:"match_length"`
	MatchHash       string    `json:"match_hash"`
	MatchText       string    `json:"match_text,omitempty"`
	HasMatchText    bool      `json:"has_match_text"`
	ClassifierScore float64   `json:"classifier_score,omitempty"`
	ClassifierModel string    `json:"classifier_model,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	// Resolved at read time for the admin list; never persisted.
	UserLabel        string `json:"user_label,omitempty"`
	ServiceTokenName string `json:"service_token_name,omitempty"`
	PolicyName       string `json:"policy_name,omitempty"`
	TermListName     string `json:"term_list_name,omitempty"`
	// CaptureDisabled says the owning policy had text capture turned off
	// when this violation was recorded, so there is no text to reveal and
	// never will be. Without it the UI can only say "no text was captured",
	// which reads as a failure rather than the configured behaviour.
	CaptureDisabled bool `json:"capture_disabled,omitempty"`

	// matchTextEncrypted is the at-rest envelope; decrypted on demand by a
	// caller holding the violations-read capability.
	matchTextEncrypted string
}

// MatchTextEnvelope exposes the ciphertext to the crypto layer only.
func (v *SecgwViolation) MatchTextEnvelope() string { return v.matchTextEncrypted }

// SetMatchTextEnvelope stores the ciphertext produced by the crypto layer.
func (v *SecgwViolation) SetMatchTextEnvelope(env string) {
	v.matchTextEncrypted = env
	v.HasMatchText = env != ""
}

// SecgwViolationFilter narrows a violations listing.
type SecgwViolationFilter struct {
	Kind      string
	Action    string
	UserID    string
	ModelName string
	// RequestID narrows to the violations raised on one request, for the
	// security section of that request's detail drawer.
	RequestID string
	Since     time.Time
	Limit     int
	Offset    int
}

// SecgwClassifierRun is one classifier call made while inspecting a single
// request: what ran, how long it took, whether it answered, and what it
// concluded. Assembled at read time by joining the gateway's own usage
// events (which carry the caller's request_id) with the violations that
// same request produced, so a request's drawer can say "these checks ran
// and this is what they found" instead of leaving classifier calls to look
// like separate requests in the log.
type SecgwClassifierRun struct {
	// Kind is the check the run served (prompt_injection, content_safety);
	// empty when the classifier answered clean and no violation names it.
	Kind string `json:"kind,omitempty"`
	// ModelName is the classifier, e.g. Llama-Prompt-Guard-2-86M.
	ModelName string `json:"model_name"`
	Direction string `json:"direction,omitempty"`
	// LatencyMs and HTTPStatus come from the classifier's own usage event.
	LatencyMs  int `json:"latency_ms"`
	HTTPStatus int `json:"http_status"`
	// Findings are the violations this classifier raised on the request.
	// Empty means it ran and found nothing: the "checked, clean" case that
	// has no violation row anywhere and is otherwise invisible.
	Findings []*SecgwViolation `json:"findings"`
}

// SecgwRetention is the per-kind retention for violation rows, reusing the
// troubleshooting bounds and their refusal of an unbounded policy.
type SecgwRetention = TroubleshootingRetention

// ErrSecgwPolicyInUse is returned when deleting a policy that still has
// bindings; the admin must unbind first so the removal is deliberate.
var ErrSecgwPolicyInUse = errors.New("policy still has bindings")

// ErrSecgwDuplicateBinding is returned when a second binding targets the
// same scope: precedence is resolved at write time, never by a runtime
// tie-break.
var ErrSecgwDuplicateBinding = errors.New("a policy is already bound to that scope")

// ErrSecgwUndecryptable means a stored ciphertext (term list, captured
// text) cannot be opened with the key this instance holds — the encryption
// key changed since the row was written. The row is intact; the key is not.
var ErrSecgwUndecryptable = errors.New("encrypted with a key this instance does not hold")
