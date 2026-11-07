package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

const askArgs = `{"questions":[{"question":"How often should backups run?","options":[{"label":"Nightly"},{"label":"Hourly","description":"More storage"},{"label":"Nightly"}],"allow_other":true},{"question":"Where should they go?","options":[{"label":"S3"},{"label":"NFS"}],"multi_select":true}]}`

// The model asks; the reply ends with the question (saved on the message,
// so a reload shows it), no further model round is made; the answer is the
// next message and the model sees question, choices and answer.
func TestChatModelAsksTheUser(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeToolUpstream(t, h)
	f.narrate = "I need two details first."
	f.call = []struct{ name, args string }{{store.ChatToolAsk, askArgs}}
	f.reply = "SHOULD NOT BE REACHED"

	conv, done := toolChat(t, h, sess, map[string]any{"content": "plan my backups"})
	if done["status"] != store.ChatStatusComplete || !strings.Contains(done["content"].(string), "I need two details first.") || strings.Contains(done["content"].(string), "SHOULD NOT") {
		t.Fatalf("done = %v", done)
	}
	if n := len(f.chats()); n != 1 {
		t.Fatalf("the reply must stop after asking: %d model requests", n)
	}
	q, _ := done["question"].(map[string]any)
	qs, _ := q["questions"].([]any)
	if len(qs) != 2 {
		t.Fatalf("question = %v", done["question"])
	}
	first := qs[0].(map[string]any)
	if opts := first["options"].([]any); len(opts) != 2 || first["allow_other"] != true {
		t.Fatalf("duplicate choice not dropped / allow_other lost: %v", first)
	}
	if qs[1].(map[string]any)["multi_select"] != true {
		t.Fatalf("multi_select lost: %v", qs[1])
	}

	// Reload: the question is on the stored message.
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv, nil)
	if !strings.Contains(rec.Body.String(), "How often should backups run?") {
		t.Fatalf("question not stored: %s", rec.Body.String())
	}

	// The answer is the next message; the model sees what it asked.
	f.mu.Lock()
	f.call, f.narrate, f.reply = nil, "", "Nightly to S3 it is."
	f.mu.Unlock()
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "How often: Nightly\nWhere: S3, NFS"})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["content"] != "Nightly to S3 it is." {
		t.Fatalf("answer turn = %v", done)
	}
	last := f.lastChat()
	for _, want := range []string{"You asked the user", "How often should backups run? Choices: Nightly / Hourly", "Where should they go? Choices: S3 / NFS (several allowed)", "Their next message is the answer", "How often: Nightly"} {
		if !strings.Contains(last, want) {
			t.Fatalf("history lacks %q: %s", want, last)
		}
	}
}

// Guardrails: ask_user alone; bad calls are refused to the model (which
// carries on); not offered in Compare or deep research; can be turned off.
func TestChatAskGuardrails(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeToolUpstream(t, h)

	// A bad call: the model is told why and the reply goes on.
	f.call = []struct{ name, args string }{{store.ChatToolAsk, `{"questions":[{"question":"Pick","options":[{"label":"Only one"}]}]}`}}
	f.reply = "Going ahead without asking."
	_, done := toolChat(t, h, sess, map[string]any{"content": "do it"})
	if done["question"] != nil || done["content"] != "Going ahead without asking." || !strings.Contains(f.lastChat(), "Not asked: question 1 needs at least 2 different choices") {
		t.Fatalf("bad call: %v / %s", done, f.lastChat())
	}

	// Deep research: not offered.
	f.call = nil
	conv := chatConversationFor(t, h, sess, "test-model")
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "research it", "research": true})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if offered(f.lastChat(), store.ChatToolAsk) {
		t.Fatal("ask_user offered in deep research")
	}
	// A normal chat: offered.
	toolChat(t, h, sess, map[string]any{"content": "hi"})
	if !offered(f.lastChat(), store.ChatToolAsk) {
		t.Fatal("ask_user not offered in a normal chat")
	}
	// Off for one chat.
	conv = chatConversationFor(t, h, sess, "test-model")
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+conv, map[string]any{"tools_off": []string{store.ChatToolAsk}}); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "hi"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if offered(f.lastChat(), store.ChatToolAsk) {
		t.Fatal("ask_user offered after being turned off")
	}
}

func TestParseChatQuestion(t *testing.T) {
	for _, bad := range []string{`nope`, `{"questions":[]}`, `{"questions":[{"question":"","options":[{"label":"a"},{"label":"b"}]}]}`,
		`{"questions":[{"question":"q","options":[{"label":"a"},{"label":"A"}]}]}`,
		`{"questions":[{"question":"1","options":[{"label":"a"},{"label":"b"}]},{"question":"2","options":[{"label":"a"},{"label":"b"}]},{"question":"3","options":[{"label":"a"},{"label":"b"}]},{"question":"4","options":[{"label":"a"},{"label":"b"}]},{"question":"5","options":[{"label":"a"},{"label":"b"}]}]}`} {
		if _, err := parseChatQuestion(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	q, err := parseChatQuestion(`{"questions":[{"question":"q","options":[{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"}]},{"question":"free","options":[],"allow_other":true}]}`)
	if err != nil || len(q.Questions[0].Options) != 4 || len(q.Questions[1].Options) != 0 {
		t.Fatalf("parse: %v %+v", err, q)
	}
	b, _ := json.Marshal(q)
	if strings.Contains(string(b), `"multi_select"`) {
		t.Fatalf("zero values serialized: %s", b)
	}
}
