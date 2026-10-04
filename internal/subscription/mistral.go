package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Mistral connects a Mistral Vibe plan (formerly Le Chat Pro / Team). Mistral
// has no third-party sign-in; instead the user creates a key for Vibe Code at
// chat.mistral.ai → Code → Vibe CLI and pastes it into Janus. Mistral bills
// that key against the plan's included usage first (then pay-as-you-go if
// the user enabled it), and documents using it from other OpenAI-compatible
// tools. The API is Chat Completions on api.mistral.ai.
const (
	mistralAPIURL  = "https://api.mistral.ai/v1"
	mistralKeysURL = "https://chat.mistral.ai/code/extensions"
)

// Mistral is the Mistral Vibe provider. APIURL exists for tests.
type Mistral struct {
	APIURL string
}

func init() { Register(&Mistral{}) }

// ID implements Provider.
func (m *Mistral) ID() string { return "mistral" }

// DisplayName implements Provider.
func (m *Mistral) DisplayName() string { return "Mistral Vibe" }

// Description implements Provider.
func (m *Mistral) Description() string {
	return "Use your own Mistral Vibe plan (Free, Pro or Team). Create a Vibe Code key at chat.mistral.ai and paste it here; it is stored encrypted and never shown again."
}

// AdminNote implements Provider.
func (m *Mistral) AdminNote() string {
	return "Mistral issues Vibe Code keys for use with its CLI and other OpenAI-compatible tools; usage counts against the user's Vibe plan and, if they enabled it, their pay-as-you-go billing."
}

// AdapterType implements Provider.
func (m *Mistral) AdapterType() string { return "openai_compatible" }

// InferenceBaseURL implements Provider.
func (m *Mistral) InferenceBaseURL() string { return pick(m.APIURL, mistralAPIURL) }

// KeyHelp implements KeyAuth.
func (m *Mistral) KeyHelp() (string, string) {
	return "Open chat.mistral.ai → Code → Vibe CLI, create a key, and paste it below.", mistralKeysURL
}

// VerifyKey implements KeyAuth: a key that can list models is live.
// Mistral keys carry no account identity, so the account is named by the
// key's last characters.
func (m *Mistral) VerifyKey(ctx context.Context, client *http.Client, key string) (*Account, error) {
	key = strings.TrimSpace(key)
	if len(key) < 16 || strings.ContainsAny(key, " \t\r\n") {
		return nil, fmt.Errorf("that does not look like a Mistral key")
	}
	if _, err := m.Models(ctx, client, key); err != nil {
		return nil, err
	}
	return m.Account(ctx, client, key)
}

// Account implements Provider.
func (m *Mistral) Account(_ context.Context, _ *http.Client, key string) (*Account, error) {
	sum := sha256.Sum256([]byte(key))
	tail := key
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return &Account{Subject: "key:" + hex.EncodeToString(sum[:8]), Email: "Vibe key …" + tail, Name: "Mistral Vibe"}, nil
}

// Models implements Provider.
func (m *Mistral) Models(ctx context.Context, client *http.Client, key string) ([]string, error) {
	infos, err := m.ListModels(ctx, client, key)
	return idsOf(infos), err
}

// ListModels implements ModelLister: chat-capable, non-deprecated models.
// capabilities.reasoning false means the model refuses reasoning_effort
// outright (even "none"); true means it takes some values, which Mistral
// names when it refuses one, so Janus learns the exact set on first use.
func (m *Mistral) ListModels(ctx context.Context, client *http.Client, key string) ([]ModelInfo, error) {
	var out struct {
		Data []struct {
			ID           string `json:"id"`
			Deprecation  string `json:"deprecation"`
			Capabilities struct {
				CompletionChat bool  `json:"completion_chat"`
				Reasoning      *bool `json:"reasoning"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	status, body, err := getJSON(ctx, client, strings.TrimRight(m.InferenceBaseURL(), "/")+"/models", key, &out)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, &ReauthError{Reason: "Mistral does not accept this key (it may have been revoked). Create a new Vibe key and connect again."}
	default:
		return nil, fmt.Errorf("Mistral model list failed (HTTP %d): %s", status, oauthDetail(body))
	}
	seen := map[string]bool{}
	models := []ModelInfo{}
	for _, d := range out.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" || seen[id] || !d.Capabilities.CompletionChat || d.Deprecation != "" {
			continue
		}
		seen[id] = true
		var r *Reasoning
		if d.Capabilities.Reasoning != nil {
			r = &Reasoning{Supported: boolPtr(*d.Capabilities.Reasoning)}
		}
		models = append(models, ModelInfo{ID: id, Reasoning: r})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

// StartDeviceAuthorization implements Provider; Mistral connects by key.
func (m *Mistral) StartDeviceAuthorization(context.Context, *http.Client) (*DeviceAuthorization, error) {
	return nil, ErrNotSupported
}

// PollDeviceAuthorization implements Provider; Mistral connects by key.
func (m *Mistral) PollDeviceAuthorization(context.Context, *http.Client, string) (*Tokens, error) {
	return nil, ErrNotSupported
}

// Refresh implements Provider. Keys do not expire on a timer.
func (m *Mistral) Refresh(context.Context, *http.Client, string) (*Tokens, error) {
	return nil, &ReauthError{Reason: "Mistral keys cannot be refreshed. Create a new Vibe key and connect again."}
}

// Revoke implements Provider. Keys are revoked in the Mistral console; the
// user is told so when they disconnect.
func (m *Mistral) Revoke(context.Context, *http.Client, string) error { return nil }
