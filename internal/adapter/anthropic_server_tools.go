package adapter

import (
	"encoding/json"
	"strings"
)

// Anthropic server tools (run on Anthropic's infrastructure) on the chat
// completions translation.
//
// Callers (and Janus Chat) ask for them generically — {"type":"web_search"},
// {"type":"web_fetch"}, {"type":"code_execution"} — and Janus sends
// Anthropic's versioned definitions. Anthropic-native definitions
// ({"type":"web_search_20250305","name":"web_search",…}) still pass through
// as written. The reply's server tool blocks become the same Janus
// extensions the Responses translation emits: janus_activity for the
// search/fetch/run, annotations for the cited pages.

// anthropicServerTools maps a generic hosted tool type to its definition.
var anthropicServerTools = map[string]map[string]any{
	"web_search":     {"type": "web_search_20250305", "name": "web_search", "max_uses": 5},
	"web_fetch":      {"type": "web_fetch_20250910", "name": "web_fetch", "max_uses": 5, "citations": map[string]any{"enabled": true}},
	"code_execution": {"type": "code_execution_20250825", "name": "code_execution"},
}

// Anthropic's price per web search (it reports the count, not the cost):
// $10 per 1,000 = 10,000,000 nano-USD each. Fetches cost only tokens; code
// execution is billed by container hour against a large free allowance,
// which the response does not report, so it is not estimated here.
const anthropicWebSearchFeeNano = 10_000_000

// anthropicGenericTool returns the Anthropic definition for a generic
// hosted tool type, carrying over max_uses / domain filters if given.
func anthropicGenericTool(raw json.RawMessage, typ string) (map[string]any, bool) {
	def, ok := anthropicServerTools[typ]
	if !ok {
		return nil, false
	}
	out := map[string]any{}
	for k, v := range def {
		out[k] = v
	}
	var extra map[string]any
	if json.Unmarshal(raw, &extra) == nil {
		for _, k := range []string{"max_uses", "allowed_domains", "blocked_domains", "user_location"} {
			if v, ok := extra[k]; ok {
				out[k] = v
			}
		}
		// OpenAI-style filters: {"filters":{"allowed_domains":[…]}}.
		if f, ok := extra["filters"].(map[string]any); ok {
			if v, ok := f["allowed_domains"]; ok {
				out["allowed_domains"] = v
			}
		}
	}
	return out, true
}

// AnthropicHostedTools are the generic hosted tool types the Anthropic
// adapter can run.
func AnthropicHostedTools() map[string]bool {
	out := map[string]bool{}
	for k := range anthropicServerTools {
		out[k] = true
	}
	return out
}

// anthropicServerBlock is a server tool block in a Messages response.
type anthropicServerBlock struct {
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	// Citations on a text block.
	Citations []anthropicCitation `json:"citations"`
}

type anthropicCitation struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// serverToolKind maps a server_tool_use name to its activity type.
func serverToolKind(name string) string {
	switch name {
	case "web_search":
		return "web_search"
	case "web_fetch":
		return "web_fetch"
	case "code_execution", "bash_code_execution", "text_editor_code_execution":
		return "code_execution"
	}
	return ""
}

// serverToolActivity turns a server_tool_use block's input into the
// activity shown to the user (what is searched, fetched or run).
func serverToolActivity(name string, input json.RawMessage, status string) map[string]any {
	kind := serverToolKind(name)
	if kind == "" {
		return nil
	}
	var in struct {
		Query   string `json:"query"`
		URL     string `json:"url"`
		Code    string `json:"code"`
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	a := &searchAction{Query: firstNonEmptyStr(in.Query, in.URL)}
	act := activityDelta(kind, status, a)
	if code := firstNonEmptyStr(in.Code, in.Command); code != "" && kind == "code_execution" {
		if len(code) > 2000 {
			code = code[:2000] + "…"
		}
		act["janus_activity"].(map[string]any)["code"] = code
	}
	return act
}

// serverResultActivity reads a *_tool_result block: search results become
// the activity's sources; code output rides along for display.
func serverResultActivity(b anthropicServerBlock) (map[string]any, []string) {
	switch b.Type {
	case "web_search_tool_result":
		var results []struct {
			Type  string `json:"type"`
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		var urls []string
		if json.Unmarshal(b.Content, &results) == nil {
			for _, r := range results {
				if r.URL != "" {
					urls = append(urls, r.URL)
				}
			}
		}
		a := &searchAction{}
		for _, u := range urls {
			a.Sources = append(a.Sources, struct {
				URL string `json:"url"`
			}{u})
		}
		return activityDelta("web_search", "done", a), urls
	case "web_fetch_tool_result":
		var r struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(b.Content, &r)
		a := &searchAction{Query: r.URL}
		if r.URL != "" {
			a.Sources = append(a.Sources, struct {
				URL string `json:"url"`
			}{r.URL})
		}
		return activityDelta("web_fetch", "done", a), nil
	case "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result":
		var r struct {
			Stdout     string `json:"stdout"`
			Stderr     string `json:"stderr"`
			ReturnCode *int   `json:"return_code"`
		}
		_ = json.Unmarshal(b.Content, &r)
		act := activityDelta("code_execution", "done", nil)
		out := strings.TrimSpace(r.Stdout)
		if out == "" {
			out = strings.TrimSpace(r.Stderr)
		}
		if len(out) > 2000 {
			out = out[:2000] + "…"
		}
		if out != "" {
			act["janus_activity"].(map[string]any)["output"] = out
		}
		return act, nil
	}
	return nil, nil
}

// citationAnnotations renders text-block citations as chat annotations.
func citationAnnotations(cs []anthropicCitation) []any {
	var out []any
	for _, c := range cs {
		if c.URL == "" {
			continue
		}
		out = append(out, map[string]any{"type": "url_citation",
			"url_citation": map[string]any{"url": c.URL, "title": c.Title, "start_index": 0, "end_index": 0}})
	}
	return out
}

// anthropicServerUsage is usage.server_tool_use.
type anthropicServerUsage struct {
	WebSearchRequests int64 `json:"web_search_requests"`
	WebFetchRequests  int64 `json:"web_fetch_requests"`
}

func (s *anthropicServerUsage) apply(u *Usage) {
	if s == nil {
		return
	}
	if s.WebSearchRequests > 0 || s.WebFetchRequests > 0 {
		u.ToolCalls = map[string]int64{}
		if s.WebSearchRequests > 0 {
			u.ToolCalls["web_search"] = s.WebSearchRequests
		}
		if s.WebFetchRequests > 0 {
			u.ToolCalls["web_fetch"] = s.WebFetchRequests
		}
	}
	u.ToolFeeNano = s.WebSearchRequests * anthropicWebSearchFeeNano
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
