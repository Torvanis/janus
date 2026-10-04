package adapter

import "strings"

// CopilotAdapterType is the adapter_type for a personal GitHub Copilot plan.
const CopilotAdapterType = "github_copilot"

// Copilot speaks the OpenAI wire format, but GitHub serves it without the
// version segment: <host>/chat/completions and <host>/models, while
// <host>/v1/chat/completions is a 404. Everything else (streaming, usage,
// errors) is the OpenAI-compatible adapter unchanged.
type Copilot struct{ OpenAICompatible }

func init() { Register(&Copilot{OpenAICompatible{name: CopilotAdapterType}}) }

// Type implements Adapter.
func (c *Copilot) Type() string { return CopilotAdapterType }

// PersonalOnly implements PersonalOnly: never an upstream type.
func (c *Copilot) PersonalOnly() bool { return true }

// Prepare implements Adapter: drop the client's /v1 before joining.
func (c *Copilot) Prepare(up Upstream, req *Request) (*Prepared, error) {
	r := *req
	if strings.HasPrefix(r.Path, "/v1/") {
		r.Path = strings.TrimPrefix(r.Path, "/v1")
	}
	return c.OpenAICompatible.Prepare(up, &r)
}
