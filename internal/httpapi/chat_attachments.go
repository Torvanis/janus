package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/chatfile"
	"github.com/torvanis/janus/internal/store"
)

// Janus Chat attachments.
//
// A file is uploaded on its own (POST /chat/attachments), checked and, for
// documents, reduced to text (internal/chatfile). The original and the
// extracted text go to file storage (internal/blobstore), which encrypts
// them; the row keeps the encrypted filename and a reference to each
// object. Sending a message binds the uploads to it. Each send rebuilds the
// branch's attachments into the request: document text in full as a text
// part, images as data URLs when the model accepts images and as a short
// placeholder otherwise. There is no retrieval step: the whole document is
// context, and the composer shows the token cost before sending.

const (
	chatMaxAttachmentsPerMessage = 10
	chatDefaultMaxUploadBytes    = 20 << 20
	chatExtractTimeout           = 15 * time.Second
	// chatImageTokens is the flat estimate per image (providers bill images
	// by tiles; this is a planning figure, the request log has the truth).
	chatImageTokens = 1000
	// chatUnboundTTL is how long an upload that was never sent is kept.
	chatUnboundTTL = 24 * time.Hour
	// chatSweepBatch bounds the unbound uploads purged per reaper pass.
	chatSweepBatch = 200
	// chatRequestHeadroom is kept free under maxInMemoryBody for the JSON
	// envelope when images are inlined.
	chatRequestHeadroom  = 256 << 10
	chatMaxFilenameRunes = 255
)

type chatAttachmentView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Filename  string `json:"filename"`
	MIME      string `json:"mime"`
	SizeBytes int64  `json:"size_bytes"`
	EstTokens int64  `json:"est_tokens"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (s *Server) chatAttachmentView(a *store.ChatAttachment) chatAttachmentView {
	return chatAttachmentView{ID: a.ID, Kind: a.Kind, Filename: s.chatDecrypt(a.FilenameEnc), MIME: a.MIME,
		SizeBytes: a.SizeBytes, EstTokens: a.EstTokens, Width: a.Width, Height: a.Height, Truncated: a.Truncated}
}

// chatMaxUploadBytes is the per-file limit: the smaller of the chat upload
// limit and file storage's per-object limit.
func (s *Server) chatMaxUploadBytes() int64 {
	limit := int64(chatDefaultMaxUploadBytes)
	if s.Config != nil && s.Config.ChatMaxUploadBytes > 0 {
		limit = s.Config.ChatMaxUploadBytes
	}
	return min(limit, s.blobs().MaxObjectBytes())
}

func errUploadTooLarge(limit int64) *APIError {
	return newError(http.StatusRequestEntityTooLarge, CodeInvalidRequest, "invalid_request_error",
		"That file is larger than the "+chatFormatBytes(limit)+" upload limit.")
}

func chatFormatBytes(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return strconv.FormatInt(n>>20, 10) + " MB"
	}
	if n >= 1<<20 {
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	}
	return strconv.FormatInt(n>>10, 10) + " KB"
}

func chatAttachmentKey(userID, id, part string) string {
	return "chat/" + userID + "/" + id + "/" + part
}

// cleanFilename keeps the last path segment, drops control characters and
// caps the length; the name is only ever shown to its owner and the model.
func cleanFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	name = strings.TrimSpace(name)
	if name == "." || name == "/" {
		return ""
	}
	if utf8.RuneCountInString(name) > chatMaxFilenameRunes {
		ext := path.Ext(name)
		r := []rune(strings.TrimSuffix(name, ext))
		name = string(r[:max(1, chatMaxFilenameRunes-utf8.RuneCountInString(ext))]) + ext
	}
	return name
}

// handleChatUpload accepts one multipart file (field "file").
func (s *Server) handleChatUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	limit := s.chatMaxUploadBytes()
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20)) // room for the multipart framing
	mr, err := r.MultipartReader()
	if err != nil {
		WriteError(w, r, ErrInvalidRequest("Upload the file as multipart/form-data in a field named \"file\"."))
		return
	}
	var filename string
	var data []byte
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				WriteError(w, r, errUploadTooLarge(limit))
				return
			}
			WriteError(w, r, ErrInvalidRequest("The upload could not be read."))
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		filename = cleanFilename(part.FileName())
		data, err = io.ReadAll(io.LimitReader(part, limit+1))
		_ = part.Close()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				WriteError(w, r, errUploadTooLarge(limit))
				return
			}
			WriteError(w, r, ErrInvalidRequest("The upload could not be read."))
			return
		}
		if int64(len(data)) > limit {
			WriteError(w, r, errUploadTooLarge(limit))
			return
		}
		break
	}
	if data == nil {
		WriteError(w, r, ErrInvalidRequest("Upload the file as multipart/form-data in a field named \"file\"."))
		return
	}
	if filename == "" {
		WriteError(w, r, ErrInvalidRequest("The file needs a name."))
		return
	}

	xctx, cancel := context.WithTimeout(ctx, chatExtractTimeout)
	res, err := chatfile.Process(xctx, filename, data)
	cancel()
	if err != nil {
		if chatfile.IsReject(err) {
			WriteError(w, r, ErrInvalidRequest(err.Error()))
			return
		}
		WriteError(w, r, err)
		return
	}
	if (res.Kind == store.ChatAttachmentAudio || res.Kind == store.ChatAttachmentVideo) && s.childMediaOff(ctx, user, familyMediaTranscription) {
		// Child Safety cannot check sound or video, and this age range
		// may not turn it into text: such files are not accepted at all.
		WriteError(w, r, errFamily(childNoMediaUpload))
		return
	}

	st, err := s.blobs().Active(ctx)
	if err != nil {
		s.Logger.Warn("chat upload: file storage unavailable", "error", err.Error())
		WriteError(w, r, newError(http.StatusServiceUnavailable, CodeServerError, "server_error",
			"File storage is not available right now. Ask an administrator to check Settings → File storage."))
		return
	}
	id := store.NewID()
	sum := sha256.Sum256(data)
	a := &store.ChatAttachment{ID: id, UserID: user.ID, Kind: res.Kind, MIME: res.MIME, SizeBytes: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), TextChars: res.TextChars, Truncated: res.Truncated,
		Width: res.Width, Height: res.Height, Status: store.ChatAttachmentReady}
	switch res.Kind {
	case chatfile.KindDocument:
		a.EstTokens = res.EstTokens()
	case chatfile.KindAudio, chatfile.KindVideo:
		a.EstTokens = chatMediaEstTokens(res.Kind, a.SizeBytes)
	default:
		a.EstTokens = chatImageTokens
	}
	if a.FilenameEnc, err = s.chatEncrypt(filename); err != nil {
		WriteError(w, r, err)
		return
	}
	origKey := chatAttachmentKey(user.ID, id, "original")
	if err := st.Put(ctx, origKey, data, res.MIME); err != nil {
		s.chatStoreError(w, r, err, limit)
		return
	}
	a.StorageRef = blobstore.Ref(st.Kind(), origKey)
	if res.Kind == chatfile.KindDocument {
		textKey := chatAttachmentKey(user.ID, id, "text")
		if err := st.Put(ctx, textKey, []byte(res.Text), "text/plain; charset=utf-8"); err != nil {
			s.chatDeleteBlobs(ctx, a.StorageRef)
			s.chatStoreError(w, r, err, limit)
			return
		}
		a.TextRef = blobstore.Ref(st.Kind(), textKey)
	}
	if err := s.Store.InsertChatAttachment(ctx, a); err != nil {
		s.chatDeleteBlobs(ctx, a.StorageRef, a.TextRef)
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, s.chatAttachmentView(a))
}

func (s *Server) chatStoreError(w http.ResponseWriter, r *http.Request, err error, limit int64) {
	if errors.Is(err, blobstore.ErrTooLarge) {
		WriteError(w, r, errUploadTooLarge(min(limit, s.blobs().MaxObjectBytes())))
		return
	}
	s.Logger.Warn("chat upload: store object", "error", err.Error())
	WriteError(w, r, newError(http.StatusServiceUnavailable, CodeServerError, "server_error",
		"The file could not be saved to file storage. Try again, or ask an administrator to check Settings → File storage."))
}

func chatAttachmentNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFoundf("attachment")
	}
	return err
}

// handleChatAttachmentContent streams the original file to its owner.
// Only allow-listed image types are served with their own content type;
// everything else is a download. The sandbox CSP and nosniff make sure a
// file can never run as a page on the gateway's origin.
func (s *Server) handleChatAttachmentContent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	a, err := s.Store.ChatAttachment(ctx, user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatAttachmentNotFound(err))
		return
	}
	data, err := s.chatReadBlob(ctx, a.StorageRef)
	if err != nil {
		s.Logger.Warn("chat attachment: read original", "error", err.Error(), "attachment_id", a.ID)
		WriteError(w, r, ErrNotFoundf("attachment content"))
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	name := s.chatDecrypt(a.FilenameEnc)
	disposition := func(kind string) string {
		if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
			return v
		}
		return kind
	}
	if (a.Kind == store.ChatAttachmentImage && chatfile.ImageMIMEs[a.MIME]) ||
		((a.Kind == store.ChatAttachmentAudio || a.Kind == store.ChatAttachmentVideo) && chatfile.MediaMIMEs[a.MIME]) {
		h.Set("Content-Type", a.MIME)
		h.Set("Content-Disposition", disposition("inline"))
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", disposition("attachment"))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleChatAttachmentDelete removes an upload that was not sent yet.
func (s *Server) handleChatAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	a, err := s.Store.ChatAttachment(ctx, user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatAttachmentNotFound(err))
		return
	}
	if a.MessageID != "" {
		WriteError(w, r, ErrConflict("That file was already sent with a message. Delete the conversation to remove it."))
		return
	}
	ok, err := s.Store.DeleteChatAttachment(ctx, user.ID, a.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !ok {
		WriteError(w, r, ErrConflict("That file was already sent with a message. Delete the conversation to remove it."))
		return
	}
	s.chatDeleteBlobs(ctx, a.StorageRef, a.TextRef)
	w.WriteHeader(http.StatusNoContent)
}

// chatReadBlob reads (and decrypts) one object by its ref.
func (s *Server) chatReadBlob(ctx context.Context, ref string) ([]byte, error) {
	kind, key, ok := blobstore.ParseRef(ref)
	if !ok {
		return nil, errors.New("chat attachment: bad storage ref")
	}
	st, err := s.blobs().For(ctx, kind)
	if err != nil {
		return nil, err
	}
	return st.Get(ctx, key)
}

// chatDeleteBlobs removes objects best effort; failures are logged (an
// orphaned object is encrypted and harmless, a failed chat delete is not).
func (s *Server) chatDeleteBlobs(ctx context.Context, refs ...string) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		kind, key, ok := blobstore.ParseRef(ref)
		if !ok {
			continue
		}
		st, err := s.blobs().For(bg, kind)
		if err == nil {
			err = st.Delete(bg, key)
		}
		if err != nil {
			s.Logger.Warn("chat attachment: delete object", "error", err.Error(), "ref", ref)
		}
	}
}

// chatDeleteAttachments removes rows and their objects. Several rows can
// share objects (a file re-sent on an edited branch), so refs are deduped
// and only deleted once no surviving row uses them — callers pass every row
// of the conversation at once.
func (s *Server) chatDeleteAttachments(ctx context.Context, atts []*store.ChatAttachment) {
	if len(atts) == 0 {
		return
	}
	seen := map[string]bool{}
	var refs []string
	ids := make([]string, 0, len(atts))
	for _, a := range atts {
		ids = append(ids, a.ID)
		for _, ref := range []string{a.StorageRef, a.TextRef} {
			if ref != "" && !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
	}
	s.chatDeleteBlobs(ctx, refs...)
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.Store.DeleteChatAttachmentRows(bg, ids); err != nil {
		s.Logger.Warn("chat attachment: delete rows", "error", err.Error())
	}
}

// chatSweepAttachments purges uploads never sent with a message.
func (s *Server) chatSweepAttachments(ctx context.Context) {
	stale, err := s.Store.StaleUnboundChatAttachments(ctx, time.Now().Add(-chatUnboundTTL), chatSweepBatch)
	if err != nil {
		s.Logger.Warn("sweep chat attachments", "error", err.Error())
		return
	}
	if len(stale) > 0 {
		s.chatDeleteAttachments(ctx, stale)
		s.Logger.Info("purged unsent chat attachments", "count", len(stale))
	}
}

// --- send-time binding ----------------------------------------------------------

// chatResolveAttachments validates a send's attachment_ids: at most ten, all
// the caller's, each unbound or already in this conversation. Unknown or
// foreign ids are 404 (never revealing another user's file).
func (s *Server) chatResolveAttachments(ctx context.Context, userID, conversationID string, ids []string) ([]*store.ChatAttachment, error) {
	uniq := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) > chatMaxAttachmentsPerMessage {
		return nil, ErrInvalidRequest("Attach at most " + strconv.Itoa(chatMaxAttachmentsPerMessage) + " files to one message.")
	}
	if len(uniq) == 0 {
		return nil, nil
	}
	atts, err := s.Store.ChatAttachmentsByIDs(ctx, userID, uniq)
	if err != nil {
		return nil, err
	}
	if len(atts) != len(uniq) {
		return nil, ErrNotFoundf("attachment")
	}
	for _, a := range atts {
		if a.ConversationID != "" && a.ConversationID != conversationID {
			return nil, ErrNotFoundf("attachment")
		}
		if a.Status != store.ChatAttachmentReady {
			return nil, ErrInvalidRequest("One of the files could not be processed; remove it and try again.")
		}
	}
	return atts, nil
}

// chatBindAttachments binds uploads to the new user message. An upload
// already sent with an earlier message of this conversation (editing a
// message keeps its files) gets a new row pointing at the same objects.
func (s *Server) chatBindAttachments(ctx context.Context, userID, conversationID, messageID string, atts []*store.ChatAttachment) ([]*store.ChatAttachment, error) {
	var unbound []string
	out := make([]*store.ChatAttachment, 0, len(atts))
	for _, a := range atts {
		if a.MessageID == "" {
			unbound = append(unbound, a.ID)
			b := *a
			b.ConversationID, b.MessageID = conversationID, messageID
			out = append(out, &b)
			continue
		}
		cp := *a
		cp.ID, cp.ConversationID, cp.MessageID, cp.CreatedAt = store.NewID(), conversationID, messageID, time.Time{}
		if err := s.Store.InsertChatAttachment(ctx, &cp); err != nil {
			return nil, err
		}
		out = append(out, &cp)
	}
	if _, err := s.Store.BindChatAttachments(ctx, userID, conversationID, messageID, unbound); err != nil {
		return nil, err
	}
	return out, nil
}

// chatAttachmentsByMessage loads the attachments of the given messages.
func (s *Server) chatAttachmentsByMessage(ctx context.Context, msgs []*store.ChatMessage) (map[string][]*store.ChatAttachment, error) {
	// User messages carry uploads; assistant messages carry generated images.
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	atts, err := s.Store.ChatAttachmentsForMessages(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[string][]*store.ChatAttachment{}
	for _, a := range atts {
		out[a.MessageID] = append(out[a.MessageID], a)
	}
	return out, nil
}

// chatAttachmentSearchText is what the search index learns from a
// message's files: their names and the extracted document text.
func (s *Server) chatAttachmentSearchText(ctx context.Context, atts []*store.ChatAttachment) string {
	var b strings.Builder
	for _, a := range atts {
		b.WriteString("\n")
		b.WriteString(s.chatDecrypt(a.FilenameEnc))
		if a.TextRef != "" {
			if text, err := s.chatReadBlob(ctx, a.TextRef); err == nil {
				b.WriteString("\n")
				b.Write(text)
			} else {
				s.Logger.Warn("chat attachment: read text for index", "error", err.Error(), "attachment_id", a.ID)
			}
		}
	}
	return b.String()
}

// --- vision capability ------------------------------------------------------------

// chatVisionNameHints are model-name fragments of families that accept
// image input. TODO: this is a stopgap; the real fix is model metadata (an
// "image input" capability recorded by discovery or set by the admin).
var chatVisionNameHints = []string{
	"gpt-4o", "gpt-4.1", "gpt-5", "gpt-6", "chatgpt-4o", "o3", "o4-mini", "claude", "gemini", "grok-2-vision", "grok-vision",
	"grok-4", "llava", "bakllava", "qwen2-vl", "qwen2.5-vl", "qwen3-vl", "qwen-vl", "pixtral", "gemma3", "gemma-3",
	"minicpm-v", "llama3.2-vision", "internvl", "moondream",
}

// chatVisionNonChat are name fragments of image-blind variants inside
// otherwise vision-capable families.
var chatVisionNonChat = []string{"embed", "tts", "audio", "transcribe", "realtime", "whisper", "dall-e", "image-gen", "imagen"}

// chatModelAcceptsImages decides whether a chat model takes image input:
// a "vision" modality, or "image" next to "chat", wins; otherwise the name
// is matched against known vision families. ("image" on its own is an
// image-GENERATION model in Janus's modality list, not image input.)
func chatModelAcceptsImages(name string, modalities []string) bool {
	hasChat, hasImage := false, false
	for _, m := range modalities {
		switch strings.ToLower(m) {
		case "vision":
			return true
		case "chat":
			hasChat = true
		case "image":
			hasImage = true
		}
	}
	if hasChat && hasImage {
		return true
	}
	n := strings.ToLower(name)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	for _, bad := range chatVisionNonChat {
		if strings.Contains(n, bad) {
			return false
		}
	}
	if strings.HasPrefix(n, "llama-3.2-") && strings.Contains(n, "vision") {
		return true
	}
	for _, hint := range chatVisionNameHints {
		if hint == "o3" || hint == "o4-mini" {
			if n == hint || strings.HasPrefix(n, hint+"-") {
				return true
			}
			continue
		}
		if strings.Contains(n, hint) {
			return true
		}
	}
	return false
}

// chatVision resolves the model the way the proxy does and applies
// chatModelAcceptsImages to the served model (and the requested name).
func (s *Server) chatVision(ctx context.Context, model string) bool {
	// The admin's word wins (Admin → Chat → Models → Sees images).
	switch s.chatAdmin(ctx).Models[model].Images {
	case "yes":
		return true
	case "no":
		return false
	}
	resolved, err := s.Store.ResolveModelForRequest(ctx, model)
	if err == nil && resolved.Model != nil {
		if chatModelAcceptsImages(resolved.Model.Name, resolved.Model.Modalities) {
			return true
		}
	}
	if err == nil && resolved.Managed != nil && chatModelAcceptsImages(resolved.Managed.TargetName, resolved.Managed.Modalities) {
		return true
	}
	return chatModelAcceptsImages(model, nil)
}

// --- history with content parts ----------------------------------------------------

// chatWireMessage is one OpenAI chat message: Content is a string, or a
// []chatPart when the message carries files.
type chatWireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	// ToolCalls / ToolCallID carry a tool round within one reply (the
	// model's create_image call and Janus's answer to it).
	ToolCalls  []any  `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// chatPart is one content part. est is the planning token count of a
// document part (0 = use the text length); img marks an image to inline
// late (after trimming) so dropped turns never load their images.
type chatPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *chatPartImage `json:"image_url,omitempty"`
	Audio    *chatPartAudio `json:"input_audio,omitempty"`
	Video    *chatPartVideo `json:"video_url,omitempty"`

	est int64
	img *store.ChatAttachment
	// media is a sound or video file still to place: mode inline (the
	// file itself, after trimming), transcribe or describe (text first).
	media *store.ChatAttachment
	mode  string
}

// chatPartAudio is OpenAI's input_audio part.
type chatPartAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// chatPartVideo is the video_url part (vLLM / Qwen VL; Gemini via Janus).
type chatPartVideo struct {
	URL string `json:"url"`
}

type chatPartImage struct {
	URL string `json:"url"`
}

// chatHistoryBuild turns the branch into wire messages, expanding files.
type chatHistoryBuild struct {
	s      *Server
	vision bool
	// model / user: who answers and for whom, for sound and video files
	// (chat_media_input.go); describe / transcribe are looked up once.
	model                string
	user                 *store.User
	describe, transcribe *string
	texts                map[string]string // text ref -> text, cached per send
	warnings             []string
	// files is every file on this branch (uploads and files replies made),
	// oldest first: what read_file can open.
	files []*store.ChatAttachment
}

func (b *chatHistoryBuild) warn(msg string) {
	for _, w := range b.warnings {
		if w == msg {
			return
		}
	}
	b.warnings = append(b.warnings, msg)
}

func (b *chatHistoryBuild) text(ctx context.Context, ref string) (string, error) {
	if t, ok := b.texts[ref]; ok {
		return t, nil
	}
	data, err := b.s.chatReadBlob(ctx, ref)
	if err != nil {
		return "", err
	}
	if b.texts == nil {
		b.texts = map[string]string{}
	}
	b.texts[ref] = string(data)
	return b.texts[ref], nil
}

// message builds one user message with its attachments: documents first
// (as <file> blocks), then images, then the typed text.
func (b *chatHistoryBuild) message(ctx context.Context, role, text string, atts []*store.ChatAttachment) chatWireMessage {
	if len(atts) == 0 {
		return chatWireMessage{Role: role, Content: text}
	}
	parts := make([]chatPart, 0, len(atts)+1)
	for _, a := range atts {
		name := b.s.chatDecrypt(a.FilenameEnc)
		switch a.Kind {
		case store.ChatAttachmentDocument:
			body, err := b.text(ctx, a.TextRef)
			if err != nil {
				b.s.Logger.Warn("chat attachment: read text", "error", err.Error(), "attachment_id", a.ID)
				parts = append(parts, chatPart{Type: "text", Text: "[file " + name + " could not be read]"})
				b.warn("A file could not be read from file storage and was left out.")
				continue
			}
			parts = append(parts, chatPart{Type: "text", Text: "<file name=\"" + name + "\">\n" + body + "\n</file>", est: a.EstTokens})
		case store.ChatAttachmentImage:
			if !b.vision {
				parts = append(parts, chatPart{Type: "text", Text: "[image " + name + " omitted: this model does not accept images]"})
				b.warn("This model does not accept images, so attached images were described to it by name only.")
				continue
			}
			parts = append(parts, chatPart{Type: "image_url", img: a})
		case store.ChatAttachmentAudio, store.ChatAttachmentVideo:
			parts = append(parts, b.mediaPart(ctx, a, name))
		}
	}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, chatPart{Type: "text", Text: text})
	}
	return chatWireMessage{Role: role, Content: parts}
}

// inlineImages loads the images that survived trimming as data URLs,
// newest message first, replacing the oldest with a placeholder once the
// request would exceed the proxy's in-memory body limit.
func (b *chatHistoryBuild) inlineImages(ctx context.Context, history []chatWireMessage) {
	textBytes := int64(0)
	for _, m := range history {
		switch c := m.Content.(type) {
		case string:
			textBytes += int64(len(c)) + 32
		case []chatPart:
			for _, p := range c {
				textBytes += int64(len(p.Text)) + 64
			}
		}
	}
	budget := maxInMemoryBody - chatRequestHeadroom - textBytes
	for i := len(history) - 1; i >= 0; i-- {
		parts, ok := history[i].Content.([]chatPart)
		if !ok {
			continue
		}
		for j := range parts {
			if m := parts[j].media; m != nil && parts[j].mode == chatMediaInline {
				name := b.s.chatDecrypt(m.FilenameEnc)
				what := map[bool]string{true: "video", false: "sound"}[m.Kind == store.ChatAttachmentVideo]
				cost := int64(base64.StdEncoding.EncodedLen(int(m.SizeBytes))) + 96
				if cost > budget {
					parts[j] = chatPart{Type: "text", Text: "[" + what + " " + name + " omitted: the request would be too large]"}
					b.warn("Some sound or video files were left out because the request would be too large.")
					continue
				}
				data, err := b.s.chatReadBlob(ctx, m.StorageRef)
				if err != nil {
					b.s.Logger.Warn("chat attachment: read media", "error", err.Error(), "attachment_id", m.ID)
					parts[j] = chatPart{Type: "text", Text: "[" + what + " " + name + " could not be read]"}
					b.warn("A file could not be read from file storage and was left out.")
					continue
				}
				budget -= cost
				enc := base64.StdEncoding.EncodeToString(data)
				if m.Kind == store.ChatAttachmentVideo {
					parts[j] = chatPart{Type: "video_url", Video: &chatPartVideo{URL: "data:" + m.MIME + ";base64," + enc}}
				} else {
					parts[j] = chatPart{Type: "input_audio", Audio: &chatPartAudio{Data: enc, Format: chatAudioFormat(m.MIME)}}
				}
				continue
			}
			a := parts[j].img
			if a == nil {
				continue
			}
			name := b.s.chatDecrypt(a.FilenameEnc)
			cost := int64(base64.StdEncoding.EncodedLen(int(a.SizeBytes))) + int64(len(a.MIME)) + 64
			if cost > budget {
				parts[j] = chatPart{Type: "text", Text: "[image " + name + " omitted: the request would be too large]"}
				b.warn("Some older images were left out because the request would be too large.")
				continue
			}
			data, err := b.s.chatReadBlob(ctx, a.StorageRef)
			if err != nil {
				b.s.Logger.Warn("chat attachment: read image", "error", err.Error(), "attachment_id", a.ID)
				parts[j] = chatPart{Type: "text", Text: "[image " + name + " could not be read]"}
				b.warn("A file could not be read from file storage and was left out.")
				continue
			}
			budget -= cost
			parts[j] = chatPart{Type: "image_url", ImageURL: &chatPartImage{URL: "data:" + a.MIME + ";base64," + base64.StdEncoding.EncodeToString(data)}}
		}
	}
}
