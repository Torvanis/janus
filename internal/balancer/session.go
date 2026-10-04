package balancer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Session keys keep a conversation on the member that holds its prompt
// cache. The key must be the SAME on every turn of a conversation and on
// every gateway replica, and must differ between conversations.

// Explicit session headers, first match wins. X-Janus-Session is ours; the
// rest are what vLLM's router and common clients already send, so they keep
// their stickiness when pointed at Janus.
var sessionHeaders = []string{"X-Janus-Session", "X-Session-Id", "X-Session-ID", "X-Conversation-Id", "X-Correlation-Id"}

// SessionKey returns the affinity key for a request and where it came from
// ("header", "prompt_cache_key", "user", "derived"), or "" when the body is
// not a chat/completions shape we can derive from.
//
// caller scopes every key to the authenticated principal so two users who
// happen to send the same session id (or the same first message) are
// hashed independently and never learn anything about each other's routing.
func SessionKey(h http.Header, body []byte, caller string) (key, source string) {
	for _, name := range sessionHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return scoped(caller, "h:"+v), "header"
		}
	}
	var req struct {
		PromptCacheKey string            `json:"prompt_cache_key"`
		User           string            `json:"user"`
		SessionID      string            `json:"session_id"`
		Messages       []json.RawMessage `json:"messages"`
		Prompt         json.RawMessage   `json:"prompt"`
		Input          json.RawMessage   `json:"input"`
		Instructions   string            `json:"instructions"`
		System         json.RawMessage   `json:"system"`
		Tools          json.RawMessage   `json:"tools"`
	}
	if len(body) == 0 || json.Unmarshal(body, &req) != nil {
		return "", ""
	}
	if v := strings.TrimSpace(req.PromptCacheKey); v != "" {
		return scoped(caller, "p:"+v), "prompt_cache_key"
	}
	if v := strings.TrimSpace(req.SessionID); v != "" {
		return scoped(caller, "s:"+v), "header"
	}
	// Derived: tools + system prompt + the first user message. History is
	// append-only, so this prefix is identical on every turn; hashing the
	// whole body (what vLLM's router falls back to) changes every turn and
	// scatters the conversation. It also changes exactly when a client
	// compacts its history — the point at which the old cache is useless.
	d := sha256.New()
	d.Write([]byte("tools:"))
	d.Write(canonical(req.Tools))
	switch {
	case len(req.Messages) > 0:
		firstUser := false
		for _, raw := range req.Messages {
			var msg struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(raw, &msg) != nil {
				continue
			}
			d.Write([]byte("\x00" + msg.Role + ":"))
			d.Write(canonical(msg.Content))
			if msg.Role == "user" {
				firstUser = true
				break
			}
		}
		if !firstUser {
			return "", ""
		}
	case len(req.Prompt) > 0:
		// Legacy completions: the prompt grows each turn, so use its head.
		d.Write([]byte("\x00prompt:"))
		d.Write(head(canonical(req.Prompt), 2048))
	case len(req.Input) > 0:
		d.Write([]byte("\x00instructions:" + req.Instructions + "\x00system:"))
		d.Write(canonical(req.System))
		d.Write([]byte("\x00input:"))
		d.Write(head(canonical(req.Input), 2048))
	default:
		return "", ""
	}
	if v := strings.TrimSpace(req.User); v != "" {
		d.Write([]byte("\x00user:" + v))
	}
	return scoped(caller, "d:"+hex.EncodeToString(d.Sum(nil))), "derived"
}

func scoped(caller, key string) string {
	sum := sha256.Sum256([]byte(caller + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

// canonical re-encodes JSON so insignificant whitespace/key order from
// different client libraries (or the same client across turns) doesn't
// change the key.
func canonical(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(v) // map keys are sorted by encoding/json
	if err != nil {
		return raw
	}
	return out
}

func head(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// EstimatePromptTokens is a cheap prompt-size estimate (~4 bytes per token)
// used only to account for context this replica has sent but the member has
// not yet reported.
func EstimatePromptTokens(body []byte) float64 {
	return float64(len(body)) / 4
}
