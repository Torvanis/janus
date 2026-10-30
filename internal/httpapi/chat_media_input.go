package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/chatfile"
	"github.com/torvanis/janus/internal/store"
)

// Sound and video people attach in Chat.
//
// A chat model that hears (or watches) gets the file itself: sound as an
// OpenAI input_audio part, video as a video_url data URL (the vLLM / Qwen VL
// shape; the Gemini adapter turns both into native inlineData). A model that
// does not gets text instead, made once per file and kept on the attachment
// (its TextRef) so later turns never pay again:
//
//   - sound: a transcript from a speech-to-text model;
//   - video: a description from a model that watches video, else the
//     transcript of its sound track.
//
// With neither available the model is told the file's name only, and the
// send response says so, as for images.

const (
	chatMediaInline     = "inline"
	chatMediaTranscribe = "transcribe"
	chatMediaDescribe   = "describe"

	// chatDescribePrompt asks a watching model for everything a model that
	// cannot watch would need to answer about the video.
	chatDescribePrompt = "Describe this video for someone who cannot see or hear it, so they can answer questions about it. " +
		"Cover, in order: what happens (with rough timestamps), the people, objects and setting, any text on screen, " +
		"what is said (quote it), music and other sounds. Plain prose, no preamble."
)

var (
	// chatAudioNameHints / chatVideoNameHints: model families that take
	// sound / video input through chat completions.
	chatAudioNameHints = []string{"gpt-audio", "gpt-4o-audio", "gpt-4o-mini-audio", "gemini", "voxtral", "omni", "phi-4-multimodal"}
	chatVideoNameHints = []string{"gemini", "qwen2-vl", "qwen2.5-vl", "qwen3-vl", "qwen-vl", "omni", "internvl", "llava-video", "minicpm-v"}
	// chatMediaNonChat are image-, sound- and video-making variants inside
	// those families (they take a prompt, not a file).
	chatMediaNonChat = []string{"embed", "tts", "transcribe", "realtime", "whisper", "image", "imagen", "lyria", "veo", "live"}
)

func chatNameHint(name string, hints []string) bool {
	n := strings.ToLower(name)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	for _, bad := range chatMediaNonChat {
		if strings.Contains(n, bad) {
			return false
		}
	}
	for _, h := range hints {
		if strings.Contains(n, h) {
			return true
		}
	}
	return false
}

// chatTakes reports whether a chat model takes attached sound (kind audio)
// or video: the admin's word (Admin → Chat → Models), else the name.
func (s *Server) chatTakes(ctx context.Context, model, kind string) bool {
	set := s.chatAdmin(ctx).Models[model]
	say, hints := set.Audio, chatAudioNameHints
	if kind == store.ChatAttachmentVideo {
		say, hints = set.Video, chatVideoNameHints
	}
	switch say {
	case "yes":
		return true
	case "no":
		return false
	}
	if chatNameHint(model, hints) {
		return true
	}
	if resolved, err := s.Store.ResolveModelForRequest(ctx, model); err == nil {
		if resolved.Model != nil && chatNameHint(resolved.Model.Name, hints) {
			return true
		}
		if resolved.Managed != nil && chatNameHint(resolved.Managed.TargetName, hints) {
			return true
		}
	}
	return false
}

// chatTranscribeModel is the speech-to-text model Chat uses for this
// person: the admin's pick while they may use it, else the first granted.
func (s *Server) chatTranscribeModel(ctx context.Context, user *store.User) string {
	catalog, err := s.grantedCatalog(ctx, user, nil)
	if err != nil {
		return ""
	}
	s.childFilterCatalog(ctx, user, &catalog, nil)
	var avail []string
	for _, m := range catalog.Models {
		if slices.Contains(m.Modalities, adapter.ModalitySTT) && !strings.Contains(strings.ToLower(m.Name), "realtime") &&
			!strings.Contains(strings.ToLower(m.Name), "live") && !strings.Contains(strings.ToLower(m.Name), "diarize") {
			avail = append(avail, m.PublicName())
		}
	}
	return chatPick(s.chatAdmin(ctx).TranscribeModel, avail)
}

// chatDescribeModel is the model that watches videos for chat models that
// cannot: the admin's pick while the person may use it, else the first
// granted chat model that watches video (never the asking model itself).
func (s *Server) chatDescribeModel(ctx context.Context, user *store.User, asking string) string {
	catalog, err := s.grantedCatalog(ctx, user, nil)
	if err != nil {
		return ""
	}
	s.childFilterCatalog(ctx, user, &catalog, nil)
	var avail []string
	for _, m := range catalog.Models {
		name := m.PublicName()
		if name != asking && slices.Contains(m.Modalities, adapter.ModalityChat) && s.chatTakes(ctx, name, store.ChatAttachmentVideo) {
			avail = append(avail, name)
		}
	}
	for _, mm := range catalog.Managed {
		if mm.Name != asking && s.chatTakes(ctx, mm.Name, store.ChatAttachmentVideo) {
			avail = append(avail, mm.Name)
		}
	}
	return chatPick(s.chatAdmin(ctx).DescribeModel, avail)
}

func chatPick(want string, avail []string) string {
	if want != "" && slices.Contains(avail, want) {
		return want
	}
	if len(avail) > 0 {
		return avail[0]
	}
	return ""
}

// chatMediaEstTokens is the planning estimate for a sound or video file
// before it is known what it becomes (providers bill ~32 tokens per second
// of sound and a few hundred per second of video; bytes stand in for time).
func chatMediaEstTokens(kind string, size int64) int64 {
	if kind == store.ChatAttachmentVideo {
		return max(500, size/900)
	}
	return max(200, size/500)
}

// chatAudioFormat is input_audio's format for a stored sound type.
func chatAudioFormat(mime string) string {
	switch mime {
	case "audio/mpeg":
		return "mp3"
	case "audio/wav":
		return "wav"
	case "audio/mp4":
		return "m4a"
	case "audio/ogg":
		return "ogg"
	case "audio/flac":
		return "flac"
	case "audio/aac":
		return "aac"
	}
	return strings.TrimPrefix(mime, "audio/")
}

// mediaPart is how one sound or video file goes into this model's request.
// Cached text (an earlier transcript or description) is used straight away.
func (b *chatHistoryBuild) mediaPart(ctx context.Context, a *store.ChatAttachment, name string) chatPart {
	what := "sound"
	if a.Kind == store.ChatAttachmentVideo {
		what = "video"
	}
	child := b.s.childProfileFor(ctx, b.user)
	if child != nil && !familyMediaAllowed(child, familyMediaTranscription) {
		// Child Safety cannot check sound or video itself, and this age
		// range may not turn it into text: the file is not sent.
		b.warn(childNoMediaWarning)
		return chatPart{Type: "text", Text: "[" + what + " " + name + " not sent: sound and video files are not available on this account]"}
	}
	// A child's sound and video always go through text first, so Child
	// Safety reads what is in them before the model does.
	if child == nil && b.s.chatTakes(ctx, b.model, a.Kind) {
		return chatPart{Type: "text", media: a, mode: chatMediaInline, est: a.EstTokens}
	}
	if a.TextRef != "" {
		if body, err := b.text(ctx, a.TextRef); err == nil {
			return chatMediaTextPart(a, name, body)
		}
	}
	if a.Kind == store.ChatAttachmentVideo {
		if b.describe == nil {
			m := b.s.chatDescribeModel(ctx, b.user, b.model)
			b.describe = &m
		}
		if *b.describe != "" {
			b.warn(fmt.Sprintf("This model does not watch video, so %s describes attached videos for it first.", *b.describe))
			return chatPart{Type: "text", media: a, mode: chatMediaDescribe, est: a.EstTokens}
		}
	}
	if b.transcribe == nil {
		m := b.s.chatTranscribeModel(ctx, b.user)
		b.transcribe = &m
	}
	canTranscribe := a.Kind == store.ChatAttachmentAudio || a.MIME == "video/mp4" || a.MIME == "video/webm"
	if *b.transcribe != "" && canTranscribe {
		if a.Kind == store.ChatAttachmentVideo {
			b.warn(fmt.Sprintf("This model does not watch video and no model that does is available, so %s transcribes the sound of attached videos for it.", *b.transcribe))
		} else {
			b.warn(fmt.Sprintf("This model does not hear sound, so %s transcribes attached sound for it first.", *b.transcribe))
		}
		return chatPart{Type: "text", media: a, mode: chatMediaTranscribe, est: a.EstTokens}
	}
	b.warn(fmt.Sprintf("This model does not take %s and no model is available to turn it into text, so attached %s files were described to it by name only.", what, what))
	return chatPart{Type: "text", Text: "[" + what + " " + name + " omitted: this model does not take " + what + "]"}
}

func chatMediaTextPart(a *store.ChatAttachment, name, body string) chatPart {
	kind := "transcript"
	if a.Kind == store.ChatAttachmentVideo && strings.HasPrefix(body, "Description") {
		kind = "description"
	}
	return chatPart{Type: "text", Text: "<file name=\"" + name + "\" kind=\"" + kind + "\">\n" + body + "\n</file>",
		est: int64(len(body))/chatfile.CharsPerToken + 1}
}

// convertMedia turns the parts that need a transcript or description into
// text before the first model call (in the run, not the send request: a
// description can take a while). Each becomes an activity in the reply.
func (b *chatToolbox) convertMedia(ctx context.Context, history []chatWireMessage) {
	for i := range history {
		parts, ok := history[i].Content.([]chatPart)
		if !ok {
			continue
		}
		for j := range parts {
			p := parts[j]
			if p.media == nil || (p.mode != chatMediaTranscribe && p.mode != chatMediaDescribe) {
				continue
			}
			a := p.media
			name := b.s.chatDecrypt(a.FilenameEnc)
			act := "transcribe"
			if p.mode == chatMediaDescribe {
				act = "watch"
			}
			b.live.extras.addActivity(chatActivity{Type: act, Status: "searching", Query: name})
			text, err := b.mediaText(ctx, a, p.mode)
			b.live.extras.addActivity(chatActivity{Type: act, Status: "done", Query: name})
			if err != nil {
				b.s.Logger.Warn("chat media: to text", "error", err.Error(), "attachment_id", a.ID)
				what := "sound"
				if a.Kind == store.ChatAttachmentVideo {
					what = "video"
				}
				parts[j] = chatPart{Type: "text", Text: fmt.Sprintf("[%s %s could not be turned into text: %s. Tell the user.]", what, name, err.Error())}
				continue
			}
			parts[j] = chatMediaTextPart(a, name, text)
		}
	}
}

// mediaText makes (and keeps) the transcript or description of one file.
func (b *chatToolbox) mediaText(ctx context.Context, a *store.ChatAttachment, mode string) (string, error) {
	data, err := b.s.chatReadBlob(ctx, a.StorageRef)
	if err != nil {
		return "", errors.New("the file could not be read")
	}
	var text string
	if mode == chatMediaDescribe {
		model := b.s.chatDescribeModel(ctx, b.user, b.plan.Model)
		if model == "" {
			return "", errors.New("no model that watches video is available")
		}
		// Streamed: a watching model reads the whole video before its
		// first word, longer than the proxy's time-to-first-byte limit for
		// a buffered reply (a 2.9 MB clip on vLLM took over 30 s).
		payload, _ := json.Marshal(map[string]any{"model": model, "stream": true, "stream_options": map[string]any{"include_usage": true}, "messages": []map[string]any{{
			"role": "user", "content": []map[string]any{
				{"type": "text", "text": chatDescribePrompt},
				{"type": "video_url", "video_url": map[string]string{"url": "data:" + a.MIME + ";base64," + base64.StdEncoding.EncodeToString(data)}},
			}}}})
		k := b.call(ctx, http.MethodPost, "/v1/chat/completions", payload, nil)
		if msg := sinkFailed(k); msg != "" {
			return "", errors.New(msg)
		}
		content, _, _ := k.live.snapshot()
		if strings.TrimSpace(content) == "" {
			return "", errors.New("the video model gave no description")
		}
		text = "Description of the video by " + model + ":\n" + strings.TrimSpace(content)
	} else {
		model := b.s.chatTranscribeModel(ctx, b.user)
		if model == "" {
			return "", errors.New("no speech-to-text model is available")
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("model", model)
		_ = mw.WriteField("response_format", "json")
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, "upload."+chatMediaExt(a.MIME)))
		h.Set("Content-Type", a.MIME)
		fw, _ := mw.CreatePart(h)
		_, _ = fw.Write(data)
		_ = mw.Close()
		k := b.call(ctx, http.MethodPost, "/v1/audio/transcriptions", buf.Bytes(), map[string]string{"Content-Type": mw.FormDataContentType()})
		if msg := sinkFailed(k); msg != "" {
			return "", errors.New(msg)
		}
		var out struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(k.raw.Bytes(), &out) != nil || strings.TrimSpace(out.Text) == "" {
			return "", errors.New("the speech-to-text model returned no text")
		}
		text = "Transcript of the sound by " + model + ":\n" + strings.TrimSpace(out.Text)
	}
	b.s.chatKeepMediaText(ctx, a, text)
	return text, nil
}

// chatKeepMediaText stores a transcript/description next to the file
// (encrypted like every chat object) so later turns reuse it.
func (s *Server) chatKeepMediaText(ctx context.Context, a *store.ChatAttachment, text string) {
	st, err := s.blobs().Active(ctx)
	if err != nil {
		return
	}
	key := chatAttachmentKey(a.UserID, a.ID, "text")
	if err := st.Put(ctx, key, []byte(text), "text/plain; charset=utf-8"); err != nil {
		s.Logger.Warn("chat media: keep text", "error", err.Error(), "attachment_id", a.ID)
		return
	}
	ref := blobstore.Ref(st.Kind(), key)
	chars := int64(len(text))
	if err := s.Store.SetChatAttachmentText(ctx, a.UserID, a.ID, ref, chars, chars/chatfile.CharsPerToken+1); err != nil {
		s.Logger.Warn("chat media: record text", "error", err.Error(), "attachment_id", a.ID)
		return
	}
	a.TextRef = ref
}

func chatMediaExt(mime string) string {
	switch mime {
	case "video/mp4":
		return "mp4"
	case "video/webm":
		return "webm"
	case "video/quicktime":
		return "mov"
	}
	return chatAudioFormat(mime)
}
