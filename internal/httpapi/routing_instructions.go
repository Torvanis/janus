package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/torvanis/janus/internal/routing"
	"github.com/torvanis/janus/internal/store"
)

// Router instructions: admin-written text a router (and each of its routes,
// and its default) adds to the system prompt of the requests it serves.
//
// They are added once, to the final body, after the Security Gateway's
// ingress passes and the routing classifier have read the caller's text:
// guards and the classifier judge only what the caller sent, and the
// instructions themselves are not scanned (an admin wrote them). They go
// before the caller's own system prompt, are sent to the model and counted
// in its input tokens, and callers never see them. Callers resend history
// without them, so they are added again on every turn, sticky turns too.

const headerRouteInstructions = "X-Janus-Route-Instructions"

// Values recorded for router instructions.
const (
	instructionsRouter      = "router"
	instructionsRoute       = "route"
	instructionsRouterRoute = "router+route"
	// instructionsNotApplied: the router or route has instructions, but the
	// endpoint has no system prompt to add them to.
	instructionsNotApplied = "not_applied"
)

// routeInstructions joins the router-wide instructions with those of the
// route that serves (or the default's), separated by a blank line. The
// label says which parts were present: router, route, router+route, or ""
// when there is nothing to add.
func routeInstructions(cfg store.RouterConfig, d routing.Decision) (string, string) {
	router := strings.TrimSpace(cfg.Instructions)
	route := ""
	switch {
	case d.IsDefault():
		route = cfg.Default.Instructions
	case d.Index >= 0 && d.Index < len(cfg.Routes) && cfg.Routes[d.Index].ID == d.RouteID:
		route = cfg.Routes[d.Index].Instructions
	}
	route = strings.TrimSpace(route)
	switch {
	case router != "" && route != "":
		return router + "\n\n" + route, instructionsRouterRoute
	case router != "":
		return router, instructionsRouter
	case route != "":
		return route, instructionsRoute
	}
	return "", ""
}

// injectInstructions adds text before the caller's system prompt, in the
// shape the endpoint's wire format carries it. It reports false (body
// unchanged) when the endpoint has no system prompt or the body cannot be
// read.
//
//   - /v1/chat/completions: a first system or developer message with text
//     content gets the text prepended; with an array of parts, a text part
//     is inserted first; otherwise a system message is inserted first.
//   - /v1/messages (Anthropic): system as text is prepended to; as blocks, a
//     text block is inserted first; absent, it is set.
//   - /v1/responses: instructions are prepended to, or set.
func injectInstructions(path string, body []byte, text string) ([]byte, bool) {
	if text == "" {
		return body, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body, false
	}
	var ok bool
	switch strings.TrimSuffix(path, "/") {
	case "/v1/chat/completions":
		ok = injectChatInstructions(m, text)
	case "/v1/messages":
		ok = injectSystemField(m, "system", text, true)
	case "/v1/responses":
		ok = injectSystemField(m, "instructions", text, false)
	}
	if !ok {
		return body, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

func isNull(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

// prependText puts text and a blank line before existing text.
func prependText(text, existing string) string {
	if strings.TrimSpace(existing) == "" {
		return text
	}
	return text + "\n\n" + existing
}

func injectChatInstructions(m map[string]json.RawMessage, text string) bool {
	var msgs []json.RawMessage
	if raw, has := m["messages"]; !has || json.Unmarshal(raw, &msgs) != nil {
		return false
	}
	if len(msgs) > 0 {
		var first map[string]json.RawMessage
		if json.Unmarshal(msgs[0], &first) == nil && first != nil {
			var role string
			_ = json.Unmarshal(first["role"], &role)
			if role == "system" || role == "developer" {
				if content, ok := withInstructions(first["content"], text, true); ok {
					first["content"] = content
					if b, err := json.Marshal(first); err == nil {
						msgs[0] = b
						m["messages"], _ = json.Marshal(msgs)
						return true
					}
				}
			}
		}
	}
	sys, _ := json.Marshal(map[string]string{"role": "system", "content": text})
	m["messages"], _ = json.Marshal(append([]json.RawMessage{sys}, msgs...))
	return true
}

// withInstructions returns content with text put first: prepended to a
// string, or as a new first text part of an array (when parts are allowed).
// Absent or null content becomes the text itself.
func withInstructions(content json.RawMessage, text string, parts bool) (json.RawMessage, bool) {
	if isNull(content) {
		b, _ := json.Marshal(text)
		return b, true
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		b, _ := json.Marshal(prependText(text, s))
		return b, true
	}
	if !parts {
		return nil, false
	}
	var arr []json.RawMessage
	if json.Unmarshal(content, &arr) != nil {
		return nil, false
	}
	part, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	b, err := json.Marshal(append([]json.RawMessage{part}, arr...))
	if err != nil {
		return nil, false
	}
	return b, true
}

func injectSystemField(m map[string]json.RawMessage, key, text string, blocks bool) bool {
	v, ok := withInstructions(m[key], text, blocks)
	if !ok {
		return false
	}
	m[key] = v
	return true
}

// applyRouteInstructions adds the serving route's instructions to the final
// body (once, after the guards), records what was added on the event's
// route reason, and discloses it when the router tells callers how it
// routed. It returns the body to forward.
func (s *Server) applyRouteInstructions(w http.ResponseWriter, pr *pendingRoute, event *store.UsageEvent, body []byte) []byte {
	if pr == nil {
		return body
	}
	text, label := routeInstructions(pr.cfg, pr.decision)
	pr.instructions = ""
	if label == "" {
		return body
	}
	out, ok := injectInstructions(event.EndpointPath, body, text)
	if !ok {
		pr.instructions = instructionsNotApplied
		event.RouteReason = withInstructionsNote(event.RouteReason, instructionsNotApplied)
		return body
	}
	pr.instructions = label
	event.RouteReason = withInstructionsNote(event.RouteReason, label)
	if pr.cfg.Disclose {
		w.Header().Set(headerRouteInstructions, label)
	}
	return out
}

// withInstructionsNote appends what router instructions did to a stored
// route reason ("matched:legal;instructions=router+route"). The route
// reason is the decision record the request log reads per request.
func withInstructionsNote(reason, label string) string {
	if i := strings.Index(reason, ";instructions="); i >= 0 {
		reason = reason[:i]
	}
	return reason + ";instructions=" + label
}

// defaultInstructions is what the router's default adds: the router-wide
// text plus the default's own. Evaluation replays the caller's request to
// the default with it, so the judge compares like with like.
func defaultInstructions(cfg store.RouterConfig) string {
	text, _ := routeInstructions(cfg, routing.Default(cfg, 0, store.RouteReasonDefault, nil))
	return text
}
