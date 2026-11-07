package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/torvanis/janus/internal/store"
)

// Files made earlier in a chat stay usable by the model:
//
//   - history: an earlier reply that made files is sent as its text plus
//     each file by its real kind (a file, an image, a video, audio). Text
//     files it handed over are included in full, up to
//     chatGeneratedInlineTokens per reply, so "check the files you made"
//     works with no extra step;
//   - read_file: opens any text file on the branch (uploads and generated
//     files) by name, in parts: larger files, and files from turns that
//     were trimmed to fit the context window.

const (
	// chatGeneratedInlineTokens bounds the generated file text one earlier
	// reply puts back into the history.
	chatGeneratedInlineTokens = 16000
	// chatReadFileMaxChars bounds one read_file result.
	chatReadFileMaxChars = 60000
	chatReadFileTool     = "read_file"
)

var chatReadFileToolDef = fnTool(chatReadFileTool,
	"Read a text file from this conversation: a file you (or another model) made earlier in this chat, or a file the user attached. "+
		"Use it whenever the user asks about, wants you to check, fix or change a file from earlier in the chat and its full contents are not already in front of you. "+
		"Never guess a file's contents. Long files are read in parts: pass offset (a line number) to continue.",
	map[string]any{
		"filename": map[string]any{"type": "string", "description": "The file's name as shown in the chat, e.g. wg-setup.sh."},
		"offset":   map[string]any{"type": "integer", "description": "First line to read, from 1 (optional)."},
		"limit":    map[string]any{"type": "integer", "description": "How many lines to read (optional)."},
	}, "filename")

// chatFileKind describes an attachment for the model.
func chatFileKind(a *store.ChatAttachment) string {
	switch a.Kind {
	case store.ChatAttachmentImage:
		return "image"
	case store.ChatAttachmentVideo:
		return "video"
	case store.ChatAttachmentAudio:
		return "audio"
	}
	return "file"
}

// chatTextAttachment reports an attachment whose text read_file can open:
// a generated text file, or an upload with extracted text.
func chatTextAttachment(a *store.ChatAttachment) bool {
	if a.Status != "" && a.Status != store.ChatAttachmentReady {
		return false
	}
	switch a.Kind {
	case store.ChatAttachmentFile:
		return a.StorageRef != "" && chatTextMIME(a.MIME)
	case store.ChatAttachmentDocument:
		return a.TextRef != ""
	}
	return false
}

func chatTextMIME(m string) bool {
	m = strings.ToLower(strings.TrimSpace(strings.SplitN(m, ";", 2)[0]))
	return strings.HasPrefix(m, "text/") || m == "application/json" || m == "application/xml" || m == "image/svg+xml" ||
		m == "application/javascript" || m == "application/x-sh" || m == "application/yaml" || m == "application/toml" ||
		m == "application/sql" || strings.HasSuffix(m, "+json") || strings.HasSuffix(m, "+xml")
}

// chatReadableFiles keeps the branch's text files, oldest first.
func chatReadableFiles(all []*store.ChatAttachment) []*store.ChatAttachment {
	var out []*store.ChatAttachment
	for _, a := range all {
		if chatTextAttachment(a) {
			out = append(out, a)
		}
	}
	return out
}

func chatFileRef(a *store.ChatAttachment) string {
	if a.Kind == store.ChatAttachmentDocument {
		return a.TextRef
	}
	return a.StorageRef
}

// generated is an earlier reply that made files, as the model sees it.
func (b *chatHistoryBuild) generated(ctx context.Context, m *store.ChatMessage, text string, atts []*store.ChatAttachment) chatWireMessage {
	by := firstNonEmpty(m.ServedModel, m.ModelName)
	var sb strings.Builder
	if strings.TrimSpace(text) != "" {
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(&sb, "[With this reply %s made %s:", by, chatCountFiles(atts))
	budget := int64(chatGeneratedInlineTokens)
	var bodies strings.Builder
	var items []string
	for _, a := range atts {
		name := b.s.chatDecrypt(a.FilenameEnc)
		kind := chatFileKind(a)
		if !chatTextAttachment(a) {
			items = append(items, fmt.Sprintf(" %s %q", kind, name))
			continue
		}
		items = append(items, fmt.Sprintf(" %s %q (%s)", kind, name, humanBytes(a.SizeBytes)))
		body, err := b.text(ctx, chatFileRef(a))
		if err != nil {
			b.s.Logger.Warn("chat history: read generated file", "error", err.Error(), "attachment_id", a.ID)
			fmt.Fprintf(&bodies, "\n<file name=%q>[could not be read from file storage]</file>", name)
			continue
		}
		if est := int64(len(body))/4 + 1; est <= budget {
			budget -= est
			fmt.Fprintf(&bodies, "\n<file name=%q>\n%s\n</file>", name, body)
		} else {
			fmt.Fprintf(&bodies, "\n<file name=%q>[%d lines, not shown here: open it with read_file]</file>", name, strings.Count(strings.TrimSuffix(body, "\n"), "\n")+1)
		}
	}
	out := sb.String() + strings.Join(items, ";") + "]" + bodies.String()
	return chatWireMessage{Role: store.ChatRoleAssistant, Content: out}
}

func chatCountFiles(atts []*store.ChatAttachment) string {
	counts := map[string]int{}
	order := []string{}
	for _, a := range atts {
		k := chatFileKind(a)
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		if n := counts[k]; n == 1 {
			parts = append(parts, "1 "+k)
		} else {
			parts = append(parts, fmt.Sprintf("%d %ss", n, k))
		}
	}
	return strings.Join(parts, ", ")
}

// readFile is the read_file tool: a text file on this branch, by name.
func (b *chatToolbox) readFile(ctx context.Context, args string) string {
	var in struct {
		Filename string `json:"filename"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if json.Unmarshal([]byte(args), &in) != nil || strings.TrimSpace(in.Filename) == "" {
		return "The file was not read: give filename (and optionally offset and limit)."
	}
	want := strings.TrimSpace(path.Base(strings.ReplaceAll(in.Filename, "\\", "/")))
	b.live.extras.addActivity(chatActivity{Type: chatReadFileTool, Status: "searching", Query: want})
	done := func(msg string) string {
		b.live.extras.addActivity(chatActivity{Type: chatReadFileTool, Status: "done", Query: want})
		return msg
	}
	// The newest file with that name wins (a file a later reply replaced).
	var hit *store.ChatAttachment
	var names []string
	seen := map[string]bool{}
	for i := len(b.plan.Assist.Files) - 1; i >= 0; i-- {
		a := b.plan.Assist.Files[i]
		n := b.s.chatDecrypt(a.FilenameEnc)
		if hit == nil && strings.EqualFold(n, want) {
			hit = a
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if hit == nil {
		return done(fmt.Sprintf("There is no text file named %q in this conversation. Files you can read: %s.", want, strings.Join(names, ", ")))
	}
	name := b.s.chatDecrypt(hit.FilenameEnc)
	data, err := b.s.chatReadBlob(ctx, chatFileRef(hit))
	if err != nil {
		b.s.Logger.Warn("chat read_file", "error", err.Error(), "attachment_id", hit.ID)
		return done("The file could not be read from file storage. Tell the user.")
	}
	if !utf8.Valid(data) {
		return done(name + " is not a text file, so it cannot be read.")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	start := max(in.Offset, 1)
	if start > len(lines) {
		return done(fmt.Sprintf("%s has %d lines; offset %d is past the end.", name, len(lines), in.Offset))
	}
	end := len(lines)
	if in.Limit > 0 {
		end = min(end, start-1+in.Limit)
	}
	var sb strings.Builder
	shown := start - 1
	for i := start - 1; i < end; i++ {
		if i > start-1 && sb.Len()+len(lines[i]) > chatReadFileMaxChars {
			break
		}
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
		shown = i + 1
	}
	out := fmt.Sprintf("<file name=%q lines=\"%d-%d of %d\">\n%s</file>", name, start, shown, len(lines), sb.String())
	if shown < len(lines) {
		out += fmt.Sprintf("\n[More follows: call read_file with offset %d to continue.]", shown+1)
	}
	return done(b.judge.judge(ctx, "the file "+name, name, out))
}

// chatEchoArguments is a tool call's arguments as sent back to the model
// in the next round. create_file's content is replaced by a short note:
// the file is stored (the tool result says so, and read_file opens it),
// and echoing up to 2 MB on every later round of the reply would cost
// that many input tokens each time.
func chatEchoArguments(c chatToolCall) string {
	args := firstNonEmpty(c.Arguments, "{}")
	if c.Name != store.ChatToolFile || len(args) < 2048 {
		return args
	}
	var in map[string]any
	if json.Unmarshal([]byte(args), &in) != nil {
		return args
	}
	content, ok := in["content"].(string)
	if !ok {
		return args
	}
	in["content"] = fmt.Sprintf("[%s of file content, not repeated here; read_file opens the saved file]", humanBytes(int64(len(content))))
	out, err := json.Marshal(in)
	if err != nil {
		return args
	}
	return string(out)
}
