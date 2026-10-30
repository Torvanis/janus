package secgw

import (
	dec "github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/store"
)

// ContextText is the text a classifier that decides what a request IS
// (auto-routing, Child Safety's request check) judges: the new turn exactly
// as the guards read it, preceded by the earlier conversation as marked
// context when there is any (see classify.WithContext). A first turn is
// exactly NewTurnText, so it can still share a guard's call. maxChars
// bounds the whole text (0 = the guards' own limit for the new turn). It
// returns "" when the body carries no chat messages.
//
// Security guards keep judging the new turn alone: earlier assistant text
// would only add false findings there, and they have a whole-conversation
// coverage option of their own.
func ContextText(body []byte, maxChars int) string {
	msgs, ok := parseMessages(body)
	if !ok {
		return ""
	}
	return contextText(msgs, maxChars)
}

func contextText(msgs []Message, maxChars int) string {
	latest := scopeText(msgs, store.SecgwJudgeNewTurn)
	if maxChars <= 0 {
		latest = dec.TrimText(latest, guardTextMaxChars)
	}
	return dec.WithContext(earlierTurns(msgs), latest, maxChars)
}

// earlierTurns is the conversation before the new turn.
func earlierTurns(msgs []Message) []dec.Turn {
	start := guardTurnStart(msgs)
	var out []dec.Turn
	for _, m := range msgs {
		if m.Index >= start {
			break
		}
		out = append(out, dec.Turn{Role: m.Role, Content: m.Text})
	}
	return out
}
