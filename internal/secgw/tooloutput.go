package secgw

import (
	"context"
	"strings"

	"github.com/torvanis/janus/internal/store"
)

// Tool output is what a tool fed the model — a fetched page, search
// results — never what the person wrote. A role "tool" (or legacy
// "function") message carries Message.Tool; the checks that read the
// person's turn skip it, and a violation found in it is recorded with the
// direction store.SecgwDirectionToolOutput, never as the person's words.

type toolOutputKey struct{}

// WithToolOutputJudged marks a request whose tool output its caller has
// already judged when the tool produced it (Chat does, with the person's
// effective policy, and withholds a flagged result from the model). The
// model-backed ingress checks then judge only the person's own words.
// Only an in-process caller can set it; a request from outside never has
// it.
func WithToolOutputJudged(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolOutputKey{}, true)
}

// ToolOutputJudged reports WithToolOutputJudged.
func ToolOutputJudged(ctx context.Context) bool {
	v, _ := ctx.Value(toolOutputKey{}).(bool)
	return v
}

type judgedTextKey struct{}

// WithJudgedText names the exact texts the caller made and judged itself
// and put in the person's message (Chat's transcripts and descriptions of
// attached sound and video). The model-backed checks leave those out of
// the person's words. The texts are matched whole: a block the person
// typed to look like one is not on the list, so it is judged as theirs.
func WithJudgedText(ctx context.Context, texts []string) context.Context {
	return context.WithValue(ctx, judgedTextKey{}, texts)
}

// JudgedText reports WithJudgedText, for a caller that judged its tool
// output (WithToolOutputJudged); nil otherwise.
func JudgedText(ctx context.Context) []string {
	if !ToolOutputJudged(ctx) {
		return nil
	}
	v, _ := ctx.Value(judgedTextKey{}).([]string)
	return v
}

// userWordsOnly is msgs with the text of tool output blanked (indexes and
// roles kept, so the new turn starts where it did): every check that
// skips empty text then judges the person's words alone. The texts in
// judged (see WithJudgedText) are taken out as well.
func userWordsOnly(msgs []Message, judged []string) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		switch {
		case out[i].Tool:
			out[i].Text = ""
		case !isAssistant(out[i].Role):
			for _, block := range judged {
				if block != "" {
					out[i].Text = strings.ReplaceAll(out[i].Text, block, "")
				}
			}
		}
	}
	return out
}

// ingressDirection is the direction a request-side violation in m is
// recorded with: tool output says so; anything else is the request.
func ingressDirection(m Message) string {
	if m.Tool {
		return store.SecgwDirectionToolOutput
	}
	return store.SecgwDirectionIngress
}
