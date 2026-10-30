package classify

import "strings"

// Conversation context for classifiers that decide what a request IS
// (auto-routing, Usage insights, Child Safety's request check). A short
// follow-up ("Keep going", "What about Ohio?") means nothing on its own:
// judged alone it routes as trivial or as a quick fact. Measured on hosted
// Jev over 20 follow-up and topic-switch conversations: the latest message
// alone got the category right 8/20 and the difficulty 4/11; the earlier
// turns as marked context, with the latest message named as the one to
// judge, got 17/20 and 10/11 and kept topic switches apart (an unmarked
// transcript dragged "unrelated quick one: ..." to the earlier, harder
// topic). Single-turn requests are judged exactly as before.
const (
	// ContextTurns is how many earlier messages (user and assistant) are
	// given as context: the last three exchanges.
	ContextTurns = 6
	// ContextMaxChars caps the context part; the newest lines are kept.
	ContextMaxChars = 4000
	// Per-message caps: a reply's gist is in its start, and it is mostly
	// the model's own words.
	contextUserChars  = 1000
	contextReplyChars = 300

	contextHeader = "Earlier in this conversation (context only):"
	contextLatest = "The latest message, which is the request to judge:"
)

// Turn is one message of a conversation.
type Turn struct {
	Role    string
	Content string
}

// WithContext frames latest (the new turn) with the earlier conversation
// as context. earlier is the conversation before the new turn, oldest
// first; only its user and assistant messages are used. With no earlier
// user or assistant text it returns latest unchanged. maxChars bounds the
// whole text (0 = latest is taken as is and only the context is capped);
// latest keeps its tail and the context gives way first.
func WithContext(earlier []Turn, latest string, maxChars int) string {
	latest = strings.TrimSpace(latest)
	if maxChars > 0 {
		latest = TrimText(latest, maxChars)
	}
	if latest == "" {
		return ""
	}
	ctx := earlierLines(earlier)
	if ctx == "" {
		return latest
	}
	room := ContextMaxChars
	frame := len([]rune(contextHeader)) + len([]rune(contextLatest)) + len("\n\n\nUser: ") + 1
	if maxChars > 0 {
		if left := maxChars - len([]rune(latest)) - frame; left < room {
			room = left
		}
	}
	if room < 200 {
		// No useful room left: the new turn alone.
		return latest
	}
	return contextHeader + "\n" + keepNewestLines(ctx, room) + "\n\n" + contextLatest + "\nUser: " + latest
}

// earlierLines renders the last ContextTurns user and assistant messages,
// one per line (whitespace folded), each capped.
func earlierLines(turns []Turn) string {
	var lines []string
	for i := len(turns) - 1; i >= 0 && len(lines) < ContextTurns; i-- {
		t := strings.Join(strings.Fields(turns[i].Content), " ")
		if t == "" {
			continue
		}
		switch role := turns[i].Role; {
		case role == "assistant" || strings.HasPrefix(role, "assistant."):
			lines = append(lines, "Assistant: "+headText(t, contextReplyChars))
		case role == "user":
			lines = append(lines, "User: "+headText(t, contextUserChars))
		}
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n")
}

// headText keeps the start of t (rune-safe), marking a cut.
func headText(t string, n int) string {
	r := []rune(t)
	if len(r) <= n {
		return t
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// keepNewestLines keeps the newest whole lines within n runes; a single
// line longer than n keeps its tail.
func keepNewestLines(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	lines := strings.Split(s, "\n")
	var kept []string
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		l := len([]rune(lines[i])) + 1
		if used+l > n {
			if len(kept) == 0 {
				kept = append(kept, TrimText(lines[i], n))
			}
			break
		}
		kept = append([]string{lines[i]}, kept...)
		used += l
	}
	return strings.Join(kept, "\n")
}

// ParseHistory reads an admin-typed conversation ("User: ..." and
// "Assistant: ..." lines; unprefixed lines continue the previous message,
// or start a user message) into turns. Used by the Try boxes.
func ParseHistory(s string) []Turn {
	var out []Turn
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		low := strings.ToLower(t)
		switch {
		case strings.HasPrefix(low, "user:"):
			out = append(out, Turn{Role: "user", Content: strings.TrimSpace(t[5:])})
		case strings.HasPrefix(low, "assistant:"):
			out = append(out, Turn{Role: "assistant", Content: strings.TrimSpace(t[10:])})
		case len(out) > 0:
			out[len(out)-1].Content += "\n" + t
		default:
			out = append(out, Turn{Role: "user", Content: t})
		}
	}
	return out
}
