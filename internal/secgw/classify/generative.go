package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProtocolGenerativeGuard is the Llama Guard wire shape: an OpenAI chat
// completion whose reply is a verdict, not prose. Verified against Llama
// Guard 4 on vLLM 0.29:
//
//	POST {base}/v1/chat/completions
//	  {"model": ..., "messages": [...], "max_tokens": 20, "temperature": 0}
//	→ "\n\nsafe"            or
//	→ "\n\nunsafe\nS9"      (one or more codes, comma-separated)
//
// The model's own chat template injects the hazard taxonomy; the gateway
// sends the conversation and nothing else. Unlike an encoder classifier
// this is judged per CONVERSATION TURN, not per text chunk: the last
// message is the one under judgement and the earlier ones are its context
// (Llama Guard moderates an assistant reply differently when it can see
// the user prompt it answers). Chunking therefore does not apply.
//
// This is how Llama Guard is served, and it is a different workload class
// from Prompt Guard: a 12B generative model on vLLM, not a DeBERTa encoder
// on TEI. Both are "a classifier" to the admin; only the protocol differs.
const ProtocolGenerativeGuard = "generative_guard"

func init() {
	Register(ProtocolGenerativeGuard, newGenerativeGuard)
}

// Turn is one message of a conversation handed to a conversation guard.
type Turn struct {
	Role    string
	Content string
}

// ConversationGuard judges a conversation whose LAST turn is the one under
// judgement. Verdict.Categories is empty when the turn is safe.
//
// It is a second, narrower interface rather than a widening of Classifier
// because the two backends genuinely take different inputs (a bag of
// chunks vs. an ordered conversation); pretending otherwise would force
// every encoder backend to carry a no-op. The engine type-asserts.
type ConversationGuard interface {
	Classifier
	Judge(ctx context.Context, turns []Turn) (Verdict, error)
}

// Verdict is a conversation guard's answer.
type Verdict struct {
	Safe       bool
	Categories []string // S-codes, in the order the model emitted them
	Raw        string   // the model's literal reply, for the audit row
}

type generativeGuard struct {
	up Upstream
}

func newGenerativeGuard(up Upstream) (Classifier, error) {
	if strings.TrimSpace(up.BaseURL) == "" {
		return nil, fmt.Errorf("classifier %s has no base URL", up.ModelName)
	}
	if strings.TrimSpace(up.ModelName) == "" {
		return nil, fmt.Errorf("generative guard needs the served model name")
	}
	if up.Client == nil {
		up.Client = http.DefaultClient
	}
	return &generativeGuard{up: up}, nil
}

func (g *generativeGuard) Protocol() string { return ProtocolGenerativeGuard }

// Classify satisfies Classifier by judging each segment as a lone user
// turn. It exists so a generative guard can be wired anywhere a Classifier
// is expected; the engine prefers Judge for conversation context.
func (g *generativeGuard) Classify(ctx context.Context, in []Segment) ([]Score, error) {
	out := make([]Score, 0, len(in))
	for _, s := range in {
		v, err := g.Judge(ctx, []Turn{{Role: "user", Content: s.Text}})
		if err != nil {
			return nil, err
		}
		sc := Score{Index: s.Index, Label: "safe", Score: 1}
		if !v.Safe {
			sc.Label = strings.Join(v.Categories, ",")
			sc.Malicious = true
		}
		out = append(out, sc)
	}
	return out, nil
}

// Judge sends the conversation and parses the verdict.
func (g *generativeGuard) Judge(ctx context.Context, turns []Turn) (Verdict, error) {
	if len(turns) == 0 {
		return Verdict{Safe: true}, nil
	}
	msgs := make([]map[string]string, 0, len(turns))
	chars := 0
	for _, t := range turns {
		msgs = append(msgs, map[string]string{"role": t.Role, "content": t.Content})
		chars += len(t.Content)
	}
	payload, _ := json.Marshal(map[string]any{
		"model": g.up.ModelName, "messages": msgs,
		// A verdict is at most "unsafe" + a short code list. 20 tokens is
		// headroom for every category at once; anything longer is not a
		// verdict and is rejected by the parser below.
		"max_tokens": 20, "temperature": 0,
	})
	url := strings.TrimRight(g.up.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.up.APIKey)
	}
	started := time.Now()
	resp, err := g.up.Client.Do(req)
	if g.up.Meter != nil {
		defer func() { g.up.Meter(ctx, time.Since(started), chars, err) }()
	}
	if err != nil {
		return Verdict{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		err = readErr
		return Verdict{}, err
	}
	if resp.StatusCode/100 != 2 {
		err = fmt.Errorf("guard returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		return Verdict{}, err
	}
	var cc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if jerr := json.Unmarshal(body, &cc); jerr != nil || len(cc.Choices) == 0 {
		err = fmt.Errorf("guard response is not a chat completion: %s", truncate(string(body), 200))
		return Verdict{}, err
	}
	v, perr := ParseGuardVerdict(cc.Choices[0].Message.Content)
	if perr != nil {
		err = perr
		return Verdict{}, err
	}
	return v, nil
}

// ParseGuardVerdict turns Llama Guard's reply into a Verdict. It is strict:
// anything that is not exactly `safe` or `unsafe` + known codes is an
// error, never a pass. A guard that starts answering in prose (wrong
// model bound, template drift, truncation) must trip the breaker, not
// quietly wave everything through.
func ParseGuardVerdict(raw string) (Verdict, error) {
	v := Verdict{Raw: raw}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	head := strings.ToLower(strings.TrimSpace(lines[0]))
	switch head {
	case "safe":
		if len(lines) > 1 && strings.TrimSpace(strings.Join(lines[1:], "")) != "" {
			return v, fmt.Errorf("guard said safe but kept talking: %q", truncate(raw, 80))
		}
		v.Safe = true
		return v, nil
	case "unsafe":
		if len(lines) < 2 {
			return v, fmt.Errorf("guard said unsafe with no category: %q", truncate(raw, 80))
		}
		for _, code := range strings.Split(lines[1], ",") {
			code = strings.ToUpper(strings.TrimSpace(code))
			if code == "" {
				continue
			}
			if !KnownCategory(code) {
				return v, fmt.Errorf("guard returned unknown category %q", code)
			}
			v.Categories = append(v.Categories, code)
		}
		if len(v.Categories) == 0 {
			return v, fmt.Errorf("guard said unsafe with no category: %q", truncate(raw, 80))
		}
		return v, nil
	}
	return v, fmt.Errorf("guard reply is not a verdict: %q", truncate(raw, 80))
}
