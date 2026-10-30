package httpapi

import (
	"bytes"
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
	"slices"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/chatfile"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Image generation in Janus Chat.
//
// Picking an image model (catalog modality "image" without "chat") turns a
// message into an /v1/images/generations request through the normal
// in-process proxy, so grants, quotas, Security Gateway, Family safety and
// metering apply exactly as for API traffic. The prompt is the user's
// message. The images that come back (base64, or a short-lived provider URL
// fetched once) are stored like uploads — encrypted in file storage,
// owner-only — and attached to the assistant message, so they survive the
// provider's URL expiring, show again when the chat is reopened, and are
// purged with the chat.
//
// v1 makes new images from a description; editing an attached image
// (/v1/images/edits) is not built.

const (
	// chatImageFetchTimeout bounds downloading one provider-hosted image.
	chatImageFetchTimeout = 60 * time.Second
	// chatImageMaxBytes bounds one generated image (the largest current
	// models produce ~5 MB PNGs).
	chatImageMaxBytes = 32 << 20
)

// chatPlanImageModel reports a personal plan model that makes images (a
// plan's models have no catalog row: judged by name, as discovery does).
// Chat uses them through the create_image tool.
func chatPlanImageModel(model string) bool {
	_, native, ok := subscription.ParseModel(model)
	return ok && slices.Contains(adapter.GuessModalities(native), adapter.ModalityImage)
}

// chatImageModel reports whether model (as the proxy resolves it) makes
// images rather than chat replies: modality "image" without "chat". A
// vision chat model lists both and stays a chat model.
func (s *Server) chatImageModel(ctx context.Context, model string) bool {
	if strings.HasPrefix(model, "my/") {
		return false
	}
	resolved, err := s.Store.ResolveModelForRequest(ctx, model)
	if err != nil {
		return false
	}
	var mods []string
	switch {
	case resolved.Managed != nil:
		mods = resolved.Managed.Modalities
	case resolved.Model != nil:
		mods = resolved.Model.Modalities
	}
	image, chat := false, false
	for _, m := range mods {
		switch strings.ToLower(m) {
		case "image":
			image = true
		case "chat":
			chat = true
		}
	}
	return image && !chat
}

// chatImageResponse is the OpenAI images response (and xAI's, which adds
// mime_type).
type chatImageResponse struct {
	Data []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
		MIMEType      string `json:"mime_type"`
	} `json:"data"`
}

// runChatImage is runChat for an image model.
func (s *Server) runChatImage(ctx context.Context, run *store.ChatRun, live *chatLiveRun, user *store.User, token *store.Token,
	model, prompt, requestID string) {
	rt := s.chatRT()
	defer func() {
		rt.mu.Lock()
		delete(rt.runs, run.ID)
		rt.mu.Unlock()
		live.cancel()
	}()
	out := s.chatGenerateImage(ctx, run, live, user, token, model, prompt, requestID, 1)
	status := out.status
	m := live.msg
	if status == store.ChatStatusBlocked || status == store.ChatStatusError {
		m.ErrorCode, m.ErrorMessage = out.errCd, out.errMsg
		if m.ErrorMessage == "" {
			m.ErrorMessage = "The model did not make an image."
		}
	}
	live.mu.Lock()
	live.status = status
	live.mu.Unlock()
	s.chatAttachUsage(ctx, m, requestID)
	s.chatPersist(live, out.caption, "", status)
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Store.FinishChatRun(bg, run.ID, status)
	if status == store.ChatStatusComplete && out.caption != "" {
		s.chatIndex(bg, user.ID, run.ConversationID, m.ID, out.caption)
	}
}

// chatImageResult is one image generation's outcome.
type chatImageResult struct {
	status        string
	stored        int
	caption       string
	errCd, errMsg string
}

// chatGenerateImage makes images for prompt through the gateway (a normal
// metered /v1/images/generations request under requestID) and stores them
// on the run's assistant message, numbered from first.
func (s *Server) chatGenerateImage(ctx context.Context, run *store.ChatRun, live *chatLiveRun, user *store.User, token *store.Token,
	model, prompt, requestID string, first int) chatImageResult {
	payload, _ := json.Marshal(map[string]any{"model": model, "prompt": prompt, "n": 1})
	req, err := chatProxyRequestPath(ctx, user, token, requestID, chatAgentFor(ctx, requestID, chatMediaClientApp), "/v1/images/generations", payload)
	if err != nil {
		return chatImageResult{status: store.ChatStatusError, errCd: "chat.internal", errMsg: "The image could not be started."}
	}
	// A sink of its own: the image reply is JSON, never chat text.
	sink := &chatSink{header: http.Header{}, live: &chatLiveRun{}}
	s.chatDriveProxy(run, live, sink, req, requestID)
	sink.finishRaw()
	out := chatImageResult{status: store.ChatStatusComplete}
	switch {
	case ctx.Err() != nil:
		out.status = store.ChatStatusStopped
		return out
	case sink.errCd == CodeSecurityBlocked:
		out.status, out.errCd, out.errMsg = store.ChatStatusBlocked, sink.errCd, sink.errMsg
		return out
	case sink.errCd != "" || sink.status >= 400:
		out.status, out.errCd, out.errMsg = store.ChatStatusError, sink.errCd, sink.errMsg
		return out
	}
	var resp chatImageResponse
	if err := json.Unmarshal(sink.raw.Bytes(), &resp); err != nil {
		out.status, out.errCd, out.errMsg = store.ChatStatusError, "upstream.bad_response", "The image model sent a reply Janus could not read."
		return out
	}
	for i, item := range resp.Data {
		data, err := s.chatImageBytes(ctx, item.B64JSON, item.URL)
		if err != nil {
			s.Logger.Warn("chat image: read generated image", "error", err.Error(), "request_id", requestID, "model", model)
			continue
		}
		if err := s.chatStoreGeneratedImage(ctx, user.ID, run.ConversationID, live.msg.ID, first+i, data); err != nil {
			s.Logger.Warn("chat image: store generated image", "error", err.Error(), "request_id", requestID)
			continue
		}
		out.stored++
		if out.caption == "" {
			out.caption = strings.TrimSpace(item.RevisedPrompt)
		}
	}
	if out.stored == 0 {
		out.status = store.ChatStatusError
		out.errCd, out.errMsg = "upstream.empty_reply", "The model finished without an image. Try again, or describe it differently."
		if len(resp.Data) > 0 {
			out.errCd, out.errMsg = "chat.image_unavailable", "The image was made but could not be saved. Try again."
		}
	}
	return out
}

// chatImageBytes decodes a base64 image, or downloads a provider-hosted
// one once (https only, bounded in time and size).
func (s *Server) chatImageBytes(ctx context.Context, b64, rawURL string) ([]byte, error) {
	if b64 != "" {
		if i := strings.Index(b64, ","); strings.HasPrefix(b64, "data:") && i > 0 {
			b64 = b64[i+1:]
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("decode base64: %w", err)
		}
		if len(data) > chatImageMaxBytes {
			return nil, errors.New("image too large")
		}
		return data, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("image url is not https")
	}
	fctx, cancel := context.WithTimeout(ctx, chatImageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.upstreamHTTPClient(ctx).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image url returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, chatImageMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > chatImageMaxBytes {
		return nil, errors.New("image too large")
	}
	return data, nil
}

// chatImageExt names the image by its bytes, never by what the provider
// said it was.
func chatImageExt(data []byte) string {
	switch http.DetectContentType(data) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ""
}

// chatStoreGeneratedImage checks a generated image like an upload and
// stores it, bound to the assistant message.
func (s *Server) chatStoreGeneratedImage(ctx context.Context, userID, conversationID, messageID string, n int, data []byte) error {
	ext := chatImageExt(data)
	if ext == "" {
		return errors.New("not a supported image")
	}
	filename := fmt.Sprintf("image-%d%s", n, ext)
	xctx, cancel := context.WithTimeout(ctx, chatExtractTimeout)
	res, err := chatfile.Process(xctx, filename, data)
	cancel()
	if err != nil {
		return err
	}
	if res.Kind != chatfile.KindImage {
		return errors.New("not an image")
	}
	st, err := s.blobs().Active(ctx)
	if err != nil {
		return err
	}
	id := store.NewID()
	sum := sha256.Sum256(data)
	a := &store.ChatAttachment{ID: id, UserID: userID, ConversationID: conversationID, MessageID: messageID,
		Kind: res.Kind, MIME: res.MIME, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		Width: res.Width, Height: res.Height, EstTokens: chatImageTokens, Status: store.ChatAttachmentReady}
	if a.FilenameEnc, err = s.chatEncrypt(filename); err != nil {
		return err
	}
	key := chatAttachmentKey(userID, id, "original")
	if err := st.Put(ctx, key, data, res.MIME); err != nil {
		return err
	}
	a.StorageRef = blobstore.Ref(st.Kind(), key)
	if err := s.Store.InsertChatAttachment(ctx, a); err != nil {
		s.chatDeleteBlobs(ctx, a.StorageRef)
		return err
	}
	return nil
}

// chatProxyRequestPath is chatProxyRequest for another /v1 endpoint.
func chatProxyRequestPath(ctx context.Context, user *store.User, token *store.Token, requestID, agent, path string, payload []byte) (*http.Request, error) {
	req, err := chatProxyRequest(ctx, user, token, requestID, agent, payload)
	if err != nil {
		return nil, err
	}
	req.URL.Path = path
	req.RequestURI = ""
	req.Body = io.NopCloser(bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	return req, nil
}
