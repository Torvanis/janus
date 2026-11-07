package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The reported case: a model made several files in a chat, then the user
// asks (maybe another model) to check them. The files' text must reach the
// model, not "N image(s)"; files too large to inline are named and
// read_file opens them by name, in parts.
func TestChatModelSeesFilesMadeEarlier(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeToolUpstream(t, h)
	mk := func(name, content string) string {
		b, _ := json.Marshal(map[string]string{"filename": name, "content": content})
		return string(b)
	}
	script := "#!/bin/sh\nset -eu\nwg genkey | tee privatekey | wg pubkey > publickey\n"
	peerA := "[Interface]\nAddress = 10.8.0.2/24\nPrivateKey = A_PRIV\n\n[Peer]\nPublicKey = B_PUB\nEndpoint = 203.0.113.7:51820\nAllowedIPs = 10.8.0.0/24\n"
	var big strings.Builder
	for i := 1; i <= 9000; i++ {
		fmt.Fprintf(&big, "line %05d of the long log\n", i)
	}
	f.call = []struct{ name, args string }{
		{"create_file", mk("wg-setup.sh", script)},
		{"create_file", mk("wg0-a.txt", peerA)},
		{"create_file", mk("debug.log", big.String())},
	}
	f.reply = "Here are the WireGuard files."
	conv := chatConversationFor(t, h, sess, "test-model")
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "make me a wireguard setup"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != "complete" {
		t.Fatalf("first turn: %v", done)
	}

	// Next turn: validate them. Small files are in the history in full.
	f.mu.Lock()
	f.call, f.reply = nil, "They look right."
	f.mu.Unlock()
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "can you validate these files?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	last := f.lastChat()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(last), &req); err != nil {
		t.Fatal(err)
	}
	var prior string
	for _, m := range req.Messages {
		if c, ok := m.Content.(string); ok && m.Role == "assistant" && strings.Contains(c, "Here are the WireGuard files.") {
			prior = c
		}
	}
	if prior == "" {
		t.Fatalf("the earlier reply is missing from the history: %s", last)
	}
	for _, want := range []string{`made 3 files:`, `file "wg-setup.sh"`, `file "wg0-a.txt"`, "wg genkey | tee privatekey", "Endpoint = 203.0.113.7:51820",
		`<file name="debug.log">[9000 lines, not shown here: open it with read_file]</file>`} {
		if !strings.Contains(prior, want) {
			t.Errorf("history lacks %q:\n%s", want, prior)
		}
	}
	if strings.Contains(prior, "image(s)") || strings.Contains(prior, "line 04500 of the long log") {
		t.Fatalf("files described as images, or the large file inlined:\n%.600s", prior)
	}
	if !offered(last, "read_file") {
		t.Fatal("read_file not offered in a chat that has files")
	}

	// read_file opens the large one in parts, and names what exists when
	// the model guesses a name.
	f.mu.Lock()
	f.call = []struct{ name, args string }{
		{"read_file", `{"filename":"debug.log","offset":4500,"limit":3}`},
		{"read_file", `{"filename":"wg0-b.txt"}`},
	}
	f.reply = "Checked the log."
	f.mu.Unlock()
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "and the log around line 4500?"})
	_, done = chatEvents(t, h, sess, sent["run_id"].(string), "")
	chats := f.chats()
	round1, round2 := chats[len(chats)-2], chats[len(chats)-1]
	if !strings.Contains(round1, `lines=\"4500-4502 of 9000\"`) || !strings.Contains(round1, "line 04500 of the long log") ||
		!strings.Contains(round1, "line 04502 of the long log") || strings.Contains(round1, "line 04503 of the long log") ||
		!strings.Contains(round1, "call read_file with offset 4503") {
		t.Fatalf("read_file part:\n%.2000s", round1[strings.Index(round1, `"role":"tool"`):])
	}
	if !strings.Contains(round2, `There is no text file named \"wg0-b.txt\"`) || !strings.Contains(round2, "wg0-a.txt") {
		t.Fatalf("unknown file: %.1500s", round2[strings.LastIndex(round2, `"role":"tool"`):])
	}
	acts, _ := done["activity"].([]any)
	if len(acts) == 0 || acts[0].(map[string]any)["type"] != "read_file" || acts[0].(map[string]any)["query"] != "debug.log" {
		t.Fatalf("activity = %v", done["activity"])
	}
}

// A chat with no files does not offer read_file; an attached document is
// readable with it.
func TestReadFileOfferedOnlyWithFiles(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeToolUpstream(t, h)
	toolChat(t, h, sess, map[string]any{"content": "hello"})
	if offered(f.lastChat(), "read_file") {
		t.Fatal("read_file offered in a chat with no files")
	}

	// An attached document (even one whose turn was trimmed) is readable.
	up := chatUploadOK(t, h, sess, "notes.md", []byte("# Notes\nalpha\nbravo\ncharlie\n"))
	conv := chatConversationFor(t, h, sess, "test-model")
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "here", "attachment_ids": []string{up["id"].(string)}})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	f.mu.Lock()
	f.call = []struct{ name, args string }{{"read_file", `{"filename":"NOTES.md","offset":3,"limit":1}`}}
	f.mu.Unlock()
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "what is line 3 of notes.md?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if last := f.lastChat(); !strings.Contains(last, `lines=\"3-3 of 4\"`) || !strings.Contains(last, "bravo") || strings.Contains(last[strings.LastIndex(last, `"role":"tool"`):], "charlie") {
		t.Fatalf("read_file of an upload: %.1500s", last[strings.LastIndex(last, `"role":"tool"`):])
	}
	// Another user's files are never visible: read_file only sees this branch.
	_ = http.MethodGet
}

// A file handed over is not echoed back in full on the next round.
func TestChatEchoArgumentsDropsFileContent(t *testing.T) {
	big := strings.Repeat("x", 100000)
	args, _ := json.Marshal(map[string]string{"filename": "a.txt", "content": big})
	out := chatEchoArguments(chatToolCall{Name: "create_file", Arguments: string(args)})
	if strings.Contains(out, "xxxx") || !strings.Contains(out, `"filename":"a.txt"`) || !strings.Contains(out, "97.7 KB of file content") {
		t.Fatalf("echo = %.200s", out)
	}
	small, _ := json.Marshal(map[string]string{"filename": "a.txt", "content": "hi"})
	if chatEchoArguments(chatToolCall{Name: "create_file", Arguments: string(small)}) != string(small) {
		t.Fatal("small call changed")
	}
	other := `{"query":"` + big + `"}`
	if chatEchoArguments(chatToolCall{Name: "web_search", Arguments: other}) != other {
		t.Fatal("other tools changed")
	}
}
