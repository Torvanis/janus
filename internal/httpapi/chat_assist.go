package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
	"github.com/torvanis/janus/internal/websearch"
)

// Janus Chat assistant tools (round 10): function tools Janus itself runs
// for any chat model that can call functions —
//
//   - create_file: the model hands over a real file (stored on the reply)
//     instead of pasting hundreds of lines of code;
//   - create_video / create_music: media made with the chat's video/music
//     model through the gateway (each a normal metered request);
//   - remember / forget: the person's memory across chats;
//   - read_page: reads a web page (public web only): a link the person
//     gives, or a source the model found. Offered when the model's
//     provider does not read pages itself (Claude and Gemini do), and in
//     deep research.
//
// They share one tool loop with the Janus-run web search and create_image
// (runChat): each round runs the calls the model made, appends the
// assistant tool-call turn and one tool result per call, and asks again,
// until the model answers or the round limit is reached.

// chatAssist is what a run may use besides the model's own tools.
type chatAssist struct {
	File       bool
	Memory     bool
	VideoModel string
	MusicModel string
	// ReadPage: Janus-run page reading (the "Read web pages" tool for
	// models whose provider does not read pages itself; deep research).
	ReadPage bool
}

// chatRunPlan is everything a reply run needs.
type chatRunPlan struct {
	Model, Effort string
	// Tools: provider-run tools, Janus-run web_search and create_image
	// (chatToolsFor).
	Tools      []string
	ImageModel string
	Assist     chatAssist
	Research   bool
	History    []chatWireMessage
	RequestID  string
	// Question is the user's message (deep research names its report).
	Question string
}

const (
	// chatMaxToolRounds bounds Janus-run tool rounds per reply.
	chatMaxToolRounds = 8
	// Deep research searches and reads far more before it writes.
	chatResearchSearchRounds = 20
	chatResearchToolRounds   = 32
	// chatMaxFilesPerReply / chatMaxFileBytes bound create_file.
	chatMaxFilesPerReply = 10
	chatMaxFileBytes     = 2 << 20
	// chatMediaWait bounds a video job.
	chatMediaWait     = 10 * time.Minute
	chatMediaMaxBytes = 64 << 20
	// chatMemoryPromptChars bounds the memories put in the system message.
	chatMemoryPromptChars = 8000
)

// chatToolsLimitNote ends the last allowed tool round's results.
const chatToolsLimitNote = "\n\n[That was the last tool call allowed for this reply. Answer the user now with what you have; do not call tools again.]"

// --- definitions -----------------------------------------------------------

func fnTool(name, description string, props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": props, "required": required},
	}}
}

var chatFileToolDef = fnTool(store.ChatToolFile,
	"Give the user a file: a complete program, web page, game, document, data file (CSV, JSON) or anything they will save, open or run. "+
		"The file is shown to them as a card with Preview and Download, so do NOT also paste its contents in your reply — say briefly what it is and how to use it. "+
		"Use it for anything longer than a short snippet that the user wants as a file. Calling it again with the same filename replaces that file.",
	map[string]any{
		"filename": map[string]any{"type": "string", "description": "File name with an extension, e.g. tetris.html, report.md, data.csv. No folders."},
		"content":  map[string]any{"type": "string", "description": "The complete contents of the file (text)."},
	}, "filename", "content")

var chatRememberToolDef = fnTool("remember",
	"Save a lasting fact about the user to their memory, which you will see in later chats: their name, role, preferences, ongoing projects, how they like answers. "+
		"Use it when they tell you something worth keeping or ask you to remember something. Never save passwords, keys, one-off details, or sensitive personal data (health, finances, ID numbers) unless they ask you to.",
	map[string]any{"fact": map[string]any{"type": "string", "description": "One short fact, written about the user (e.g. \"Prefers metric units\")."}}, "fact")

var chatForgetToolDef = fnTool("forget",
	"Remove something from the user's memory when they ask you to forget it or it is no longer true.",
	map[string]any{"memory": map[string]any{"type": "string", "description": "The memory's id (shown in brackets in your saved memories) or its text."}}, "memory")

var chatVideoToolDef = fnTool(store.ChatToolVideo,
	"Make a short video from a detailed description. Use it when the user asks for a video, clip or animation. It takes a minute or more; the video is shown to the user when it is ready. Afterwards reply briefly about it.",
	map[string]any{
		"prompt":  map[string]any{"type": "string", "description": "A detailed description of the video: subject, action, setting, camera, style."},
		"seconds": map[string]any{"type": "integer", "description": "Length in seconds (optional, 4 to 10)."},
	}, "prompt")

var chatMusicToolDef = fnTool(store.ChatToolMusic,
	"Make a music clip from a description. Use it when the user asks for music, a song, a jingle or a soundtrack. The track is shown to the user with a player. Afterwards reply briefly about it.",
	map[string]any{"prompt": map[string]any{"type": "string", "description": "Genre, mood, instruments, tempo, and lyrics or \"instrumental\"."}}, "prompt")

var chatReadPageToolDef = fnTool("read_page",
	"Open a web page (or PDF) on the public internet and read its text. Use it whenever the user gives a link or asks about a page, "+
		"and to read the most relevant search results instead of relying on snippets. Read the page before answering about it; never guess what a page says.",
	map[string]any{"url": map[string]any{"type": "string", "description": "The full http(s) address."}}, "url")

// chatResearchPrompt is added to the system message in deep research.
func chatResearchPrompt(readPage bool) string {
	read := ""
	if readPage {
		read = " Open and read the most relevant sources with read_page rather than relying on snippets."
	}
	return "Deep research mode. Investigate the question thoroughly before answering. " +
		"Break it into sub-questions, then search widely — many searches with different phrasings and angles (at least 6–10)." + read +
		" Cross-check important facts across independent sources and note where they disagree. " +
		"Then write a well-structured report in Markdown: a short summary first, then sections with headings, then a \"Sources\" list. " +
		"Cite sources inline as [n](url). Say clearly what you could not confirm. Do not ask the user questions; make reasonable assumptions and state them."
}

// --- the toolbox (one per run) ------------------------------------------------

type chatToolbox struct {
	s          *Server
	run        *store.ChatRun
	live       *chatLiveRun
	user       *store.User
	token      *store.Token
	plan       chatRunPlan
	search     bool // Janus runs web_search
	image      bool // create_image offered
	maxSearch  int
	searches   int
	imagesMade int
	files      map[string]*store.ChatAttachment
	media      int
	// functionsOff: the model refused function tools (400): none sent.
	functionsOff bool
	// judge judges tool output before the model sees it (nil: nothing to).
	judge *chatToolJudge
}

func (b *chatToolbox) defs() []any {
	if b.functionsOff {
		return nil
	}
	var list []any
	if b.image {
		list = append(list, chatImageToolDef)
	}
	if b.search && b.searches < b.maxSearch {
		list = append(list, chatSearchToolDef)
	}
	a := b.plan.Assist
	if a.File {
		list = append(list, chatFileToolDef)
	}
	if a.VideoModel != "" {
		list = append(list, chatVideoToolDef)
	}
	if a.MusicModel != "" {
		list = append(list, chatMusicToolDef)
	}
	if a.Memory {
		list = append(list, chatRememberToolDef, chatForgetToolDef)
	}
	if a.ReadPage {
		list = append(list, chatReadPageToolDef)
	}
	return list
}

// offersFunctions reports whether any Janus-run function tool is sent.
func (b *chatToolbox) offersFunctions() bool { return len(b.defs()) > 0 }

// ours keeps the calls Janus runs (in order).
func (b *chatToolbox) ours(calls []chatToolCall) []chatToolCall {
	var out []chatToolCall
	for _, c := range calls {
		if b.handles(c.Name) {
			out = append(out, c)
		}
	}
	return out
}

func (b *chatToolbox) handles(name string) bool {
	a := b.plan.Assist
	switch name {
	case store.ChatToolImage:
		return b.image
	case store.ChatToolWebSearch:
		return b.search
	case store.ChatToolFile:
		return a.File
	case store.ChatToolVideo:
		return a.VideoModel != ""
	case store.ChatToolMusic:
		return a.MusicModel != ""
	case "remember", "forget":
		return a.Memory
	case "read_page":
		return a.ReadPage
	}
	return false
}

// round runs one round of calls and returns the assistant turn and the
// tool results, in call order.
func (b *chatToolbox) round(ctx context.Context, calls []chatToolCall) (chatWireMessage, []chatWireMessage) {
	turn := chatWireMessage{Role: "assistant", Content: ""}
	var results []chatWireMessage
	searched, imaged := false, false
	for _, c := range calls {
		if c.ID == "" {
			c.ID = "call_" + store.NewID()
		}
		turn.ToolCalls = append(turn.ToolCalls, map[string]any{"id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": firstNonEmpty(c.Arguments, "{}")}})
		var out string
		switch {
		case ctx.Err() != nil:
			out = "Stopped by the user."
		case c.Name == store.ChatToolWebSearch:
			searched = true
			q := chatSearchCallQuery(c.Arguments)
			out = b.judge.judge(ctx, "the search results for “"+q+"”", q, b.s.chatRunSearch(ctx, b.live, q))
		case c.Name == store.ChatToolImage:
			imaged = true
			out = b.s.chatImageCall(ctx, b.run, b.live, b.user, b.token, b.plan.ImageModel, c, &b.imagesMade)
		case c.Name == store.ChatToolFile:
			out = b.createFile(ctx, c.Arguments)
		case c.Name == store.ChatToolVideo:
			out = b.createVideo(ctx, c.Arguments)
		case c.Name == store.ChatToolMusic:
			out = b.createMusic(ctx, c.Arguments)
		case c.Name == "remember":
			out = b.remember(ctx, c.Arguments)
		case c.Name == "forget":
			out = b.forget(ctx, c.Arguments)
		case c.Name == "read_page":
			out = b.readPage(ctx, c.Arguments)
		}
		results = append(results, chatWireMessage{Role: "tool", ToolCallID: c.ID, Content: out})
	}
	if searched {
		b.searches++
		if b.searches == b.maxSearch && len(results) > 0 {
			// Last search round: the search tool is withdrawn from the
			// next request. Say so, or the model keeps reaching for it
			// and ends with no answer at all. (In the tool result: some
			// chat templates refuse a system message mid-conversation.)
			appendToolNote(&results[len(results)-1], chatSearchLimitNote)
		}
	}
	if imaged {
		// One round of pictures per reply; then the model finishes.
		b.image = false
	}
	return turn, results
}

func appendToolNote(m *chatWireMessage, note string) {
	text, _ := m.Content.(string)
	m.Content = text + note
}

func argString(args, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func argInt(args, key string) int {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case string:
		return atoiDefault(v, 0)
	}
	return 0
}

// --- create_file -----------------------------------------------------------------

var chatFileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,118}$`)

// chatFileExts are the text files create_file makes, with the type they
// are served as (always as a download; previews run sandboxed).
var chatFileExts = map[string]string{
	".html": "text/html", ".htm": "text/html", ".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript",
	".ts": "text/plain", ".tsx": "text/plain", ".jsx": "text/plain", ".json": "application/json", ".md": "text/markdown",
	".txt": "text/plain", ".csv": "text/csv", ".tsv": "text/tab-separated-values", ".xml": "application/xml", ".svg": "image/svg+xml",
	".yaml": "text/plain", ".yml": "text/plain", ".toml": "text/plain", ".ini": "text/plain", ".env": "text/plain",
	".py": "text/plain", ".go": "text/plain", ".rs": "text/plain", ".java": "text/plain", ".kt": "text/plain", ".c": "text/plain",
	".h": "text/plain", ".cpp": "text/plain", ".hpp": "text/plain", ".cs": "text/plain", ".rb": "text/plain", ".php": "text/plain",
	".swift": "text/plain", ".sh": "text/plain", ".ps1": "text/plain", ".bat": "text/plain", ".sql": "text/plain", ".lua": "text/plain",
	".r": "text/plain", ".dart": "text/plain", ".scala": "text/plain", ".vue": "text/plain", ".svelte": "text/plain", ".tex": "text/plain",
	".ipynb": "application/json", ".srt": "text/plain", ".vtt": "text/vtt", ".ics": "text/calendar", ".log": "text/plain",
}

// chatFileMIME is the type of a file create_file may make ("" = refused).
func chatFileMIME(name string) string {
	if n := strings.ToLower(name); n == "dockerfile" || n == "makefile" {
		return "text/plain"
	}
	return chatFileExts[strings.ToLower(path.Ext(name))]
}

func (b *chatToolbox) createFile(ctx context.Context, args string) string {
	var in struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if json.Unmarshal([]byte(args), &in) != nil {
		return "The file was not saved: the call's arguments were not valid JSON. Try again with filename and content."
	}
	name := strings.TrimSpace(path.Base(strings.ReplaceAll(in.Filename, "\\", "/")))
	b.live.extras.addActivity(chatActivity{Type: store.ChatToolFile, Status: "searching", Query: name})
	done := func(msg string) string {
		b.live.extras.addActivity(chatActivity{Type: store.ChatToolFile, Status: "done", Query: name, Size: int64(len(in.Content))})
		return msg
	}
	mime := chatFileMIME(name)
	switch {
	case !chatFileNameRe.MatchString(name):
		return done("The file was not saved: use a simple file name with an extension, like game.html or notes.md.")
	case mime == "":
		return done(fmt.Sprintf("The file was not saved: %s files are not supported. Text files are (code, HTML, Markdown, CSV, JSON, SVG and similar).", path.Ext(name)))
	case in.Content == "":
		return done("The file was not saved: it was empty.")
	case len(in.Content) > chatMaxFileBytes:
		return done("The file was not saved: it is larger than 2 MB.")
	case !utf8.ValidString(in.Content):
		return done("The file was not saved: only text files can be made.")
	}
	if b.files == nil {
		b.files = map[string]*store.ChatAttachment{}
	}
	prev := b.files[strings.ToLower(name)]
	if prev == nil && len(b.files) >= chatMaxFilesPerReply {
		return done("The file was not saved: a reply can make at most 10 files.")
	}
	a, err := b.s.chatStoreMedia(ctx, b.user.ID, b.run.ConversationID, b.live.msg.ID, name, store.ChatAttachmentFile, mime, []byte(in.Content))
	if err != nil {
		b.s.Logger.Warn("chat create_file: store", "error", err.Error())
		return done("The file could not be saved to file storage. Tell the user.")
	}
	if prev != nil {
		b.s.chatDropAttachment(ctx, prev)
	}
	b.files[strings.ToLower(name)] = a
	return done(fmt.Sprintf("Saved %s (%s). It is shown to the user as a file card with Preview and Download; do not repeat its contents. Reply briefly: what it is and how to use it.",
		name, humanBytes(int64(len(in.Content)))))
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// chatStoreMedia stores bytes made for a reply as an attachment on it.
func (s *Server) chatStoreMedia(ctx context.Context, userID, conversationID, messageID, filename, kind, mime string, data []byte) (*store.ChatAttachment, error) {
	st, err := s.blobs().Active(ctx)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.blobs().MaxObjectBytes() {
		return nil, errors.New("larger than file storage allows")
	}
	id := store.NewID()
	sum := sha256.Sum256(data)
	a := &store.ChatAttachment{ID: id, UserID: userID, ConversationID: conversationID, MessageID: messageID,
		Kind: kind, MIME: mime, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Status: store.ChatAttachmentReady}
	if a.FilenameEnc, err = s.chatEncrypt(filename); err != nil {
		return nil, err
	}
	key := chatAttachmentKey(userID, id, "original")
	if err := st.Put(ctx, key, data, mime); err != nil {
		return nil, err
	}
	a.StorageRef = blobstore.Ref(st.Kind(), key)
	if err := s.Store.InsertChatAttachment(ctx, a); err != nil {
		s.chatDeleteBlobs(ctx, a.StorageRef)
		return nil, err
	}
	return a, nil
}

// chatDropAttachment removes a file a reply replaced.
func (s *Server) chatDropAttachment(ctx context.Context, a *store.ChatAttachment) {
	if err := s.Store.DeleteBoundChatAttachment(ctx, a.UserID, a.ID); err != nil {
		s.Logger.Warn("chat: drop replaced file", "error", err.Error())
		return
	}
	s.chatDeleteBlobs(ctx, a.StorageRef)
}

// --- memory --------------------------------------------------------------------

func (b *chatToolbox) remember(ctx context.Context, args string) string {
	fact := argString(args, "fact")
	if fact == "" {
		return "Nothing was saved: the call had no fact."
	}
	if len(fact) > store.ChatMaxMemoryChars {
		fact = fact[:store.ChatMaxMemoryChars]
	}
	enc, err := b.s.chatEncrypt(fact)
	if err != nil {
		return "The memory could not be saved."
	}
	m := &store.ChatMemory{UserID: b.user.ID, ContentEnc: enc, SourceConversationID: b.run.ConversationID}
	switch err := b.s.Store.InsertChatMemory(ctx, m); {
	case errors.Is(err, store.ErrChatMemoryFull):
		return "The memory is full (200 memories). Tell the user they can remove old ones in Settings → Memory."
	case err != nil:
		return "The memory could not be saved."
	}
	b.live.extras.addActivity(chatActivity{Type: "remember", Status: "done", Query: fact})
	return "Saved to the user's memory. Mention it in a few words only if it helps."
}

func (b *chatToolbox) forget(ctx context.Context, args string) string {
	want := strings.ToLower(strings.Trim(argString(args, "memory"), "[] "))
	if want == "" {
		return "Nothing was removed: say which memory."
	}
	mems, err := b.s.Store.ChatMemories(ctx, b.user.ID)
	if err != nil {
		return "The memory could not be read."
	}
	for _, m := range mems {
		text := b.s.chatDecrypt(m.ContentEnc)
		if strings.HasPrefix(m.ID, want) || strings.ToLower(text) == want || (len(want) > 12 && strings.Contains(strings.ToLower(text), want)) {
			if err := b.s.Store.DeleteChatMemory(ctx, b.user.ID, m.ID); err != nil {
				return "The memory could not be removed."
			}
			b.live.extras.addActivity(chatActivity{Type: "forget", Status: "done", Query: text})
			return "Removed from the user's memory: " + text
		}
	}
	return "No saved memory matched that. Nothing was removed."
}

// chatMemoryPrompt is the system-message block of a person's memories
// ("" = none or memory off for this chat).
func (s *Server) chatMemoryPrompt(ctx context.Context, userID string) string {
	mems, err := s.Store.ChatMemories(ctx, userID)
	if err != nil || len(mems) == 0 {
		return ""
	}
	var lines []string
	used := 0
	for i := len(mems) - 1; i >= 0; i-- { // newest first within the budget
		text := strings.ReplaceAll(s.chatDecrypt(mems[i].ContentEnc), "\n", " ")
		line := fmt.Sprintf("- [%s] %s", mems[i].ID[:8], text)
		if used+len(line) > chatMemoryPromptChars {
			break
		}
		used += len(line)
		lines = append(lines, line)
	}
	slices.Reverse(lines)
	return "What you remember about this person from earlier chats (use it when it helps; do not recite it; to change one, forget it by id and remember the new version):\n" +
		strings.Join(lines, "\n")
}

// --- media models ------------------------------------------------------------------

// chatMusicModelName reports a music generation model (Google Lyria).
func chatMusicModelName(name string) bool {
	n := strings.ToLower(name)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return strings.HasPrefix(n, "lyria") && !strings.Contains(n, "realtime")
}

// chatMediaModels lists the video or music models this person may use:
// their plan's first (video only), then granted ones not hidden in Chat.
func (s *Server) chatMediaModels(ctx context.Context, user *store.User, kind string) []string {
	var out []string
	cfg := s.chatAdmin(ctx)
	// A child whose age range may not make videos (or music) has none.
	child := s.childProfileFor(ctx, user)
	mediaKind := familyMediaVideo
	if kind == store.ChatToolMusic {
		mediaKind = familyMediaMusic
	}
	if child != nil && !familyMediaAllowed(child, mediaKind) {
		return nil
	}
	if kind == store.ChatToolVideo && cfg.PersonalPlans {
		if names, err := s.personalModelNames(ctx, user); err == nil {
			for _, n := range names {
				if _, native, ok := subscription.ParseModel(n); ok && slices.Contains(adapter.GuessModalities(native), adapter.ModalityVideo) && !child.Blocks(n) {
					out = append(out, n)
				}
			}
		}
	}
	catalog, err := s.grantedCatalog(ctx, user, nil)
	if err != nil {
		return out
	}
	s.childFilterCatalog(ctx, user, &catalog, nil)
	for _, m := range catalog.Models {
		name := m.PublicName()
		if cfg.Models[name].Hidden {
			continue
		}
		switch kind {
		case store.ChatToolVideo:
			if slices.Contains(m.Modalities, adapter.ModalityVideo) {
				out = append(out, name)
			}
		case store.ChatToolMusic:
			if chatMusicModelName(m.Name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// chatMediaModelFor picks the video/music model for a chat: the plan's own
// when the chat runs on that plan, then the admin's default, then the first.
func (s *Server) chatMediaModelFor(ctx context.Context, user *store.User, chatModel, kind string) string {
	avail := s.chatMediaModels(ctx, user, kind)
	if len(avail) == 0 {
		return ""
	}
	// The person's own choice (chat tray → Models), when still available.
	if mine := s.chatUserModel(ctx, user.ID, kind); mine != "" && slices.Contains(avail, mine) {
		return mine
	}
	if provider, _, ok := subscription.ParseModel(chatModel); ok {
		for _, m := range avail {
			if p, _, ok := subscription.ParseModel(m); ok && p == provider {
				return m
			}
		}
	}
	def := s.chatAdmin(ctx).VideoModel
	if kind == store.ChatToolMusic {
		def = s.chatAdmin(ctx).MusicModel
	}
	if def != "" && slices.Contains(avail, def) {
		return def
	}
	for _, m := range avail {
		if !subscription.IsPersonalModel(m) {
			return m
		}
	}
	return avail[0]
}

// chatAssistFor decides the assistant tools of a run.
func (s *Server) chatAssistFor(ctx context.Context, user *store.User, c *store.ChatConversation, model string, research bool) chatAssist {
	var a chatAssist
	off := func(id string) bool {
		return slices.Contains(s.chatAdmin(ctx).ToolsOff, id) || (c != nil && slices.Contains(c.ToolsOff, id))
	}
	a.File = !off(store.ChatToolFile)
	if !off(store.ChatToolMemory) && (c == nil || !c.Temporary) {
		if memOff, err := s.Store.ChatMemoryOff(ctx, user.ID); err == nil && !memOff {
			a.Memory = true
		}
	}
	if !off(store.ChatToolVideo) {
		a.VideoModel = s.chatMediaModelFor(ctx, user, model, store.ChatToolVideo)
	}
	if !off(store.ChatToolMusic) {
		a.MusicModel = s.chatMediaModelFor(ctx, user, model, store.ChatToolMusic)
	}
	// Read web pages: the provider's own reader where it has one (Claude,
	// Gemini: a hosted tool), Janus's for every other model. Deep research
	// always reads its sources.
	a.ReadPage = (research || !off(store.ChatToolWebFetch)) && !s.chatModelTools(ctx, model)[store.ChatToolWebFetch]
	return a
}

// --- proxied media calls -----------------------------------------------------------

// chatProxyCall is an in-process gateway request of any method/path.
func chatProxyCall(ctx context.Context, user *store.User, token *store.Token, requestID, method, p string, payload []byte, hdr map[string]string) (*http.Request, error) {
	var body io.Reader = http.NoBody
	if payload != nil {
		body = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, p, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(payload))
	}
	req.Header.Set("X-Janus-Agent", chatMediaClientApp)
	req.Header.Set("User-Agent", "Janus Chat")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rctx := context.WithValue(req.Context(), ctxRequestID, requestID)
	rctx = context.WithValue(rctx, ctxUser, user)
	rctx = context.WithValue(rctx, ctxToken, token)
	return stampChatOrigin(req.WithContext(rctx), requestID), nil
}

func (b *chatToolbox) call(ctx context.Context, method, p string, payload []byte, hdr map[string]string) *chatSink {
	req, err := chatProxyCall(ctx, b.user, b.token, store.NewID(), method, p, payload, hdr)
	if err != nil {
		return &chatSink{errCd: "chat.internal", errMsg: "the request could not be started"}
	}
	sink := &chatSink{header: http.Header{}, live: &chatLiveRun{}}
	b.s.chatDriveProxy(b.run, b.live, sink, req, RequestIDFrom(req.Context()))
	sink.finishRaw()
	return sink
}

func sinkFailed(k *chatSink) string {
	if k.errCd != "" || k.status >= 400 || k.status == 0 {
		return firstNonEmpty(k.errMsg, fmt.Sprintf("the model answered %d", k.status))
	}
	return ""
}

// keepAlive heartbeats the run while it waits between media polls (the
// reaper would otherwise take a quiet run for a dead one) and stops it
// when the user asked.
func (b *chatToolbox) keepAlive() {
	bg, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if stop, err := b.s.Store.HeartbeatChatRun(bg, b.run.ID, time.Now().UnixMilli()); err == nil && stop {
		b.live.cancel()
	}
}

func (b *chatToolbox) wait(ctx context.Context, d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		b.keepAlive()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(min(2*time.Second, time.Until(end))):
		}
	}
	return ctx.Err() == nil
}

// videoFlavor: xAI's job API (/v1/videos/generations, poll by request id,
// a URL when done) or OpenAI's, which Google's Veo endpoint also speaks
// (/v1/videos, poll status, /content).
func (s *Server) videoIsXAI(ctx context.Context, model string) bool {
	if provider, _, ok := subscription.ParseModel(model); ok {
		return provider == "xai"
	}
	resolved, err := s.Store.ResolveModelForRequest(ctx, model)
	if err != nil || resolved.Model == nil {
		return false
	}
	up, err := s.cachedUpstreamByID(ctx, resolved.Model.UpstreamID)
	if err != nil || up == nil {
		return false
	}
	u, err := url.Parse(up.BaseURL)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), "x.ai")
}

func (b *chatToolbox) createVideo(ctx context.Context, args string) string {
	prompt := argString(args, "prompt")
	model := b.plan.Assist.VideoModel
	if prompt == "" {
		return "No video was made: the call had no prompt."
	}
	if b.media >= 2 {
		return "No video was made: a reply can make at most two videos or tracks."
	}
	b.media++
	b.live.extras.addActivity(chatActivity{Type: store.ChatToolVideo, Status: "searching", Query: prompt})
	done := func(msg string) string {
		b.live.extras.addActivity(chatActivity{Type: store.ChatToolVideo, Status: "done", Query: prompt})
		return msg
	}
	data, err := b.makeVideo(ctx, model, prompt, argInt(args, "seconds"))
	if err != nil {
		if ctx.Err() != nil {
			return done("Stopped by the user.")
		}
		return done("The video could not be made: " + err.Error() + ". Tell the user.")
	}
	n := len(b.files) + b.media
	if _, err := b.s.chatStoreMedia(ctx, b.user.ID, b.run.ConversationID, b.live.msg.ID, fmt.Sprintf("video-%d.mp4", n), store.ChatAttachmentVideo, "video/mp4", data); err != nil {
		b.s.Logger.Warn("chat create_video: store", "error", err.Error())
		return done("The video was made but could not be saved. Tell the user.")
	}
	return done(fmt.Sprintf("Created the video with %s; it is shown to the user with a player above your reply.", model))
}

func (b *chatToolbox) makeVideo(ctx context.Context, model, prompt string, seconds int) ([]byte, error) {
	hdr := map[string]string{"X-Janus-Model": model}
	xai := b.s.videoIsXAI(ctx, model)
	body := map[string]any{"model": model, "prompt": prompt}
	createPath := "/v1/videos"
	if xai {
		createPath = "/v1/videos/generations"
		if seconds < 1 || seconds > 15 {
			seconds = 6
		}
		body["duration"] = seconds
	}
	payload, _ := json.Marshal(body)
	k := b.call(ctx, http.MethodPost, createPath, payload, nil)
	if msg := sinkFailed(k); msg != "" {
		return nil, errors.New(msg)
	}
	var job struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(k.raw.Bytes(), &job)
	id := firstNonEmpty(job.RequestID, job.ID)
	if id == "" {
		return nil, errors.New("the video model did not start a job")
	}
	// Poll soon, then every 8 s: short clips are often ready in seconds.
	deadline := time.Now().Add(chatMediaWait)
	for every := time.Second; time.Now().Before(deadline); every = min(every*2, 8*time.Second) {
		if !b.wait(ctx, every) {
			return nil, ctx.Err()
		}
		k := b.call(ctx, http.MethodGet, "/v1/videos/"+url.PathEscape(id), nil, hdr)
		if k.status != http.StatusOK && k.status != http.StatusAccepted {
			return nil, errors.New("checking the job failed: " + firstNonEmpty(k.errMsg, fmt.Sprint(k.status)))
		}
		var st struct {
			Status string `json:"status"`
			Error  any    `json:"error"`
			Video  struct {
				URL string `json:"url"`
			} `json:"video"`
		}
		_ = json.Unmarshal(k.raw.Bytes(), &st)
		switch strings.ToLower(st.Status) {
		case "failed", "expired", "cancelled", "error":
			return nil, fmt.Errorf("the video job %s", strings.ToLower(st.Status))
		}
		if st.Video.URL != "" {
			return b.s.chatFetchMedia(ctx, st.Video.URL)
		}
		if strings.EqualFold(st.Status, "completed") || strings.EqualFold(st.Status, "succeeded") {
			c := b.call(ctx, http.MethodGet, "/v1/videos/"+url.PathEscape(id)+"/content", nil, hdr)
			if msg := sinkFailed(c); msg != "" {
				return nil, errors.New("download failed: " + msg)
			}
			data := c.raw.Bytes()
			if len(data) < 12 || string(data[4:8]) != "ftyp" {
				return nil, errors.New("the download was not a video")
			}
			return slices.Clone(data), nil
		}
	}
	return nil, errors.New("it took longer than 10 minutes")
}

// chatFetchMedia downloads a provider-hosted result once (https only,
// bounded). xAI's file host refuses requests without a browser agent.
func (s *Server) chatFetchMedia(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the video address is not https")
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Janus Chat)")
	resp, err := s.upstreamHTTPClient(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the video download answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, chatMediaMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > chatMediaMaxBytes {
		return nil, errors.New("the video is too large")
	}
	return data, nil
}

func (b *chatToolbox) createMusic(ctx context.Context, args string) string {
	prompt := argString(args, "prompt")
	model := b.plan.Assist.MusicModel
	if prompt == "" {
		return "No music was made: the call had no prompt."
	}
	if b.media >= 2 {
		return "No music was made: a reply can make at most two videos or tracks."
	}
	b.media++
	b.live.extras.addActivity(chatActivity{Type: store.ChatToolMusic, Status: "searching", Query: prompt})
	done := func(msg string) string {
		b.live.extras.addActivity(chatActivity{Type: store.ChatToolMusic, Status: "done", Query: prompt})
		return msg
	}
	payload, _ := json.Marshal(map[string]any{"model": model, "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": prompt}}})
	k := b.call(ctx, http.MethodPost, "/v1/chat/completions", payload, nil)
	if msg := sinkFailed(k); msg != "" {
		return done("The music could not be made: " + msg + ". Tell the user.")
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Audio   struct {
					Data   string `json:"data"`
					Format string `json:"format"`
				} `json:"audio"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(k.raw.Bytes(), &resp) != nil || len(resp.Choices) == 0 || resp.Choices[0].Message.Audio.Data == "" {
		return done("The music model finished without a track. Tell the user.")
	}
	msg := resp.Choices[0].Message
	data, err := base64.StdEncoding.DecodeString(msg.Audio.Data)
	if err != nil || len(data) == 0 {
		return done("The music model sent a track Janus could not read. Tell the user.")
	}
	ext, mime := ".mp3", "audio/mpeg"
	if strings.EqualFold(msg.Audio.Format, "wav") {
		ext, mime = ".wav", "audio/wav"
	}
	n := len(b.files) + b.media
	if _, err := b.s.chatStoreMedia(ctx, b.user.ID, b.run.ConversationID, b.live.msg.ID, fmt.Sprintf("music-%d%s", n, ext), store.ChatAttachmentAudio, mime, data); err != nil {
		b.s.Logger.Warn("chat create_music: store", "error", err.Error())
		return done("The track was made but could not be saved. Tell the user.")
	}
	out := fmt.Sprintf("Created the track with %s; it is shown to the user with a player above your reply.", model)
	if lyrics := strings.TrimSpace(msg.Content); lyrics != "" && lyrics != "<instrumental>" {
		out += " The music model's lyrics and structure: " + clipText(lyrics, 2000)
	} else {
		out += " It is instrumental."
	}
	return done(out)
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- read_page -------------------------------------------------------

// chatFetchPage reads a page for read_page (a seam for tests, which cannot
// serve a public address).
var chatFetchPage = websearch.FetchPage

func (b *chatToolbox) readPage(ctx context.Context, args string) string {
	raw := argString(args, "url")
	b.live.extras.addActivity(chatActivity{Type: store.ChatToolWebFetch, Status: "searching", Query: raw})
	page, err := chatFetchPage(ctx, raw)
	b.live.extras.addActivity(chatActivity{Type: store.ChatToolWebFetch, Status: "done", Query: raw})
	if err != nil {
		return "The page could not be read: " + err.Error() + ". Try another source."
	}
	b.live.extras.addSource(page.URL, page.Title)
	return b.judge.judge(ctx, "the page at "+page.URL, page.URL, websearch.FormatPageForModel(page))
}

// --- deep research report ----------------------------------------------------------------

var chatSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

var chatGenericHeading = map[string]bool{"summary": true, "executive summary": true, "overview": true, "introduction": true,
	"tl;dr": true, "tldr": true, "answer": true, "short answer": true, "key findings": true, "report": true, "research report": true}

// chatSaveResearchReport keeps a finished report as a Markdown file on the
// reply, so it can be downloaded.
func (s *Server) chatSaveResearchReport(ctx context.Context, user *store.User, run *store.ChatRun, live *chatLiveRun, question, report string) {
	// Named from the report's title (its first heading) unless that is a
	// generic section name ("Summary", "Overview"…); then from the question.
	title := question
	for _, line := range strings.Split(report, "\n") {
		if t := strings.TrimSpace(strings.TrimLeft(line, "# ")); strings.HasPrefix(line, "#") && t != "" {
			if !chatGenericHeading[strings.ToLower(strings.Trim(t, "*:. "))] {
				title = t
			}
			break
		}
	}
	slug := strings.Trim(chatSlugRe.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = "research"
	}
	if _, err := s.chatStoreMedia(ctx, user.ID, run.ConversationID, live.msg.ID, "research-"+slug+".md", store.ChatAttachmentFile, "text/markdown", []byte(report)); err != nil {
		s.Logger.Warn("chat research: save report", "error", err.Error())
	}
}
