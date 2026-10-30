package secgw

import (
	"context"
	"regexp"

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

// chatMediaTextRe is a transcript or description Chat made of an attached
// sound or video file and put in the person's message.
var chatMediaTextRe = regexp.MustCompile(`(?s)<file name="[^"\n]*" kind="(?:transcript|description)">\n.*?\n</file>`)

// userWordsOnly is msgs with the text of tool output blanked (indexes and
// roles kept, so the new turn starts where it did): every check that
// skips empty text then judges the person's words alone. With media, the
// transcripts and descriptions Chat made (and judged) of attached files
// are taken out too — only for a caller that judged them
// (WithToolOutputJudged), or anyone could hide words in such a block.
func userWordsOnly(msgs []Message, media bool) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		switch {
		case out[i].Tool:
			out[i].Text = ""
		case media && !isAssistant(out[i].Role):
			out[i].Text = chatMediaTextRe.ReplaceAllString(out[i].Text, "")
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
