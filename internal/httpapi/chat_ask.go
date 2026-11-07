package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/torvanis/janus/internal/store"
)

// ask_user: the model stops and asks the person something it needs, with
// choices, instead of guessing. The reply ends with a question card (saved
// on the message, so it survives reloads and restarts and is answered on
// any replica); the person's answer is their next message, and the model
// answers that in a new reply. Not offered in Compare (several models
// would each stop) or deep research (which is told not to ask).

const (
	chatAskMaxQuestions = 4
	chatAskMaxOptions   = 4
)

type chatQuestion struct {
	Questions []chatQuestionItem `json:"questions"`
}

type chatQuestionItem struct {
	Question string            `json:"question"`
	Options  []chatQuestionOpt `json:"options"`
	Multi    bool              `json:"multi_select,omitempty"`
	Other    bool              `json:"allow_other,omitempty"`
}

type chatQuestionOpt struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

var chatAskToolDef = fnTool(store.ChatToolAsk,
	"Ask the user a question when you need a detail to give a good answer and guessing would likely be wrong: "+
		"for example which environment, how often, which of two approaches. The reply stops and the user sees a card with your choices; "+
		"their answer comes as their next message. Offer 2 to 4 short choices per question, best one first (it is shown as recommended). "+
		"Ask at most 4 questions at once, only when it matters, and call this tool alone (no other tool in the same turn). "+
		"Say in one short sentence before calling it why you are asking.",
	map[string]any{
		"questions": map[string]any{
			"type": "array", "minItems": 1, "maxItems": chatAskMaxQuestions,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "description": "The question, one sentence."},
					"options": map[string]any{
						"type": "array", "minItems": 2, "maxItems": chatAskMaxOptions,
						"items": map[string]any{"type": "object", "properties": map[string]any{
							"label":       map[string]any{"type": "string", "description": "A short choice, a few words."},
							"description": map[string]any{"type": "string", "description": "Optional: what this choice means."},
						}, "required": []string{"label"}},
					},
					"multi_select": map[string]any{"type": "boolean", "description": "true if the user may pick several."},
					"allow_other":  map[string]any{"type": "boolean", "description": "true to let the user type their own answer."},
				},
				"required": []string{"question", "options"},
			},
		},
	}, "questions")

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// parseChatQuestion checks an ask_user call; the error is told to the model.
func parseChatQuestion(args string) (*chatQuestion, error) {
	var q chatQuestion
	if json.Unmarshal([]byte(args), &q) != nil {
		return nil, fmt.Errorf("the arguments were not valid JSON")
	}
	if len(q.Questions) == 0 || len(q.Questions) > chatAskMaxQuestions {
		return nil, fmt.Errorf("ask 1 to %d questions", chatAskMaxQuestions)
	}
	for i := range q.Questions {
		it := &q.Questions[i]
		it.Question = clip(it.Question, 300)
		if it.Question == "" {
			return nil, fmt.Errorf("question %d is empty", i+1)
		}
		opts := it.Options[:0]
		seen := map[string]bool{}
		for _, o := range it.Options {
			o.Label, o.Description = clip(o.Label, 80), clip(o.Description, 200)
			if o.Label == "" || seen[strings.ToLower(o.Label)] {
				continue
			}
			seen[strings.ToLower(o.Label)] = true
			opts = append(opts, o)
		}
		it.Options = opts
		if len(it.Options) < 2 && !it.Other {
			return nil, fmt.Errorf("question %d needs at least 2 different choices (or allow_other)", i+1)
		}
		if len(it.Options) > chatAskMaxOptions {
			it.Options = it.Options[:chatAskMaxOptions]
		}
	}
	return &q, nil
}

// ask runs the ask_user call. alone is false when the model made other
// calls in the same round.
func (b *chatToolbox) ask(args string, alone bool) string {
	if !alone {
		return "Not asked: call ask_user alone, after the other tools have finished. Finish those first."
	}
	if b.asked {
		return "Not asked: only one ask_user per reply."
	}
	q, err := parseChatQuestion(args)
	if err != nil {
		return "Not asked: " + err.Error() + ". Fix the call and try again."
	}
	b.asked = true
	b.live.extras.mu.Lock()
	b.live.extras.Question = q
	b.live.extras.mu.Unlock()
	return "Asked. The reply ends here; the user's answer will be their next message."
}

// chatQuestionNote is a question as later history shows it to the model.
func chatQuestionNote(q *chatQuestion) string {
	var sb strings.Builder
	sb.WriteString("[You asked the user")
	for i, it := range q.Questions {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, it.Question)
		labels := make([]string, 0, len(it.Options))
		for _, o := range it.Options {
			labels = append(labels, o.Label)
		}
		if len(labels) > 0 {
			sb.WriteString(" Choices: " + strings.Join(labels, " / "))
		}
		if it.Multi {
			sb.WriteString(" (several allowed)")
		}
	}
	sb.WriteString("\nTheir next message is the answer.]")
	return sb.String()
}
