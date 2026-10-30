package adapter

import (
	"encoding/json"
	"net/url"
	"strings"
)

// Hosted (provider-run) tools on chat completions.
//
// OpenAI and xAI run web search (and xAI also X search) on their own
// servers, but only through the Responses API; chat completions refuses the
// tool types. A chat request that asks for them is therefore sent through
// the Responses translation (OpenAIResponses / Codex), and the provider's
// search activity and citations come back on the chat chunks as two Janus
// extensions next to "content":
//
//	"janus_activity": {"type":"web_search","status":"searching"|"done","query":"…","sources":["https://…"]}
//	"annotations":    [{"type":"url_citation","url_citation":{"url":"…","title":"…","start_index":0,"end_index":0}}]
//
// Clients that do not know them ignore them; the reply text is unchanged.

// HostedToolTypes are the generic provider-run tool types Janus maps onto
// each provider (never run by Janus itself):
//
//	web_search      OpenAI, xAI, Anthropic, Gemini (Google Search)
//	x_search        xAI
//	web_fetch       Anthropic (web_fetch), Gemini (url_context)
//	code_execution  OpenAI + xAI (code_interpreter), Anthropic, Gemini
var HostedToolTypes = map[string]bool{"web_search": true, "x_search": true, "web_fetch": true, "code_execution": true}

// HostedTools reports which hosted tool types a chat request asks for.
func HostedTools(body []byte) []string {
	var in struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &in) != nil {
		return nil
	}
	var out []string
	for _, t := range in.Tools {
		if typ := genericHostedType(t.Type); typ != "" {
			out = append(out, typ)
		}
	}
	return out
}

// genericHostedType normalises a tool type to its generic hosted name
// (code_interpreter is the OpenAI/xAI name for code_execution).
func genericHostedType(t string) string {
	if t == "code_interpreter" {
		return "code_execution"
	}
	if HostedToolTypes[t] {
		return t
	}
	return ""
}

// HostedToolsFor is which hosted tools an upstream runs, by adapter type
// and API host. Self-hosted servers run none.
func HostedToolsFor(adapterType, baseURL string) map[string]bool {
	switch adapterType {
	case "anthropic":
		return AnthropicHostedTools()
	case "gemini":
		return GeminiHostedTools()
	case "openai_compatible", "codex":
		return HostedToolSupport(baseURL)
	}
	return nil
}

// HostedToolSupport is which hosted tools a provider runs, by its API base
// URL: api.openai.com and the ChatGPT plan backend run web search; api.x.ai
// runs web and X search. Anything else runs none (Janus never fakes them).
func HostedToolSupport(baseURL string) map[string]bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if tools, ok := HostedToolHosts[host]; ok {
		return tools
	}
	if strings.HasSuffix(host, ".chatgpt.com") {
		return HostedToolHosts["chatgpt.com"]
	}
	return nil
}

// HostedToolHosts is which provider API hosts run which hosted tools,
// keyed by host name.
var HostedToolHosts = map[string]map[string]bool{
	"api.x.ai":       {"web_search": true, "x_search": true, "code_execution": true},
	"api.openai.com": {"web_search": true, "code_execution": true},
	// The ChatGPT plan backend (Codex) runs web search only.
	"chatgpt.com": {"web_search": true},
}

// hostedTool returns the Responses form of a hosted tool (passed through
// as the caller wrote it; filters such as allowed_domains ride along).
func hostedTool(raw json.RawMessage) (map[string]any, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	t, _ := m["type"].(string)
	switch genericHostedType(t) {
	case "":
		return nil, false
	case "code_execution":
		// OpenAI and xAI call it code_interpreter; OpenAI needs a
		// container (an automatic one is created per request).
		m["type"] = "code_interpreter"
		if _, ok := m["container"]; !ok {
			m["container"] = map[string]any{"type": "auto"}
		}
	}
	return m, true
}

// searchAction is a hosted search call's action on the output item.
type searchAction struct {
	Type    string   `json:"type"`
	Query   string   `json:"query"`
	Queries []string `json:"queries"`
	Sources []struct {
		URL string `json:"url"`
	} `json:"sources"`
}

// hostedCallKind maps a Responses output item type to the activity type
// shown to the user ("" = not a hosted search call).
func hostedCallKind(itemType string) string {
	switch itemType {
	case "web_search_call":
		return "web_search"
	case "x_search_call", "x_semantic_search_call", "x_keyword_search_call":
		return "x_search"
	case "code_interpreter_call":
		return "code_execution"
	}
	if strings.HasSuffix(itemType, "_search_call") {
		return strings.TrimSuffix(itemType, "_call")
	}
	return ""
}

func activityDelta(kind, status string, a *searchAction) map[string]any {
	act := map[string]any{"type": kind, "status": status}
	if a != nil {
		q := a.Query
		if q == "" && len(a.Queries) > 0 {
			q = strings.Join(a.Queries, " · ")
		}
		if q != "" {
			act["query"] = q
		}
		var src []string
		for _, s := range a.Sources {
			if s.URL != "" {
				src = append(src, s.URL)
			}
		}
		if len(src) > 0 {
			act["sources"] = src
		}
	}
	return map[string]any{"janus_activity": act}
}

// codeActivity adds a code_interpreter_call's program and output to its
// activity. xAI wraps the logs as {"stdout":…,"stderr":…}; OpenAI sends
// them plain.
func codeActivity(d map[string]any, code string, outputs []struct {
	Type string `json:"type"`
	Logs string `json:"logs"`
}) {
	act := d["janus_activity"].(map[string]any)
	if code = strings.TrimSpace(code); code != "" {
		if len(code) > 2000 {
			code = code[:2000] + "…"
		}
		act["code"] = code
	}
	var out []string
	for _, o := range outputs {
		logs := o.Logs
		var x struct {
			Stdout *string `json:"stdout"`
			Stderr string  `json:"stderr"`
		}
		if json.Unmarshal([]byte(logs), &x) == nil && x.Stdout != nil {
			logs = *x.Stdout
			if strings.TrimSpace(logs) == "" {
				logs = x.Stderr
			}
		}
		if logs = strings.TrimSpace(logs); logs != "" {
			out = append(out, logs)
		}
	}
	if o := strings.Join(out, "\n"); o != "" {
		if len(o) > 2000 {
			o = o[:2000] + "…"
		}
		act["output"] = o
	}
}
