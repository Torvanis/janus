package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/chatsearch"
	"github.com/torvanis/janus/internal/store"
)

// Janus Chat: encrypted search, automatic titles and context trimming.
//
// Search is a keyed blind index (see internal/chatsearch and
// store/chat_search.go): message text stays encrypted; only
// HMAC(searchKey, userID, stemmed word) values are stored. The search key is
// derived from the instance encryption key with HKDF under its own label,
// so it never doubles as the encryption key, and rotating the encryption key
// means rebuilding the index (POST /api/v1/admin/chat/reindex). Matching is
// whole word / stem only: substring search is intentionally unsupported,
// per the product ruling that content stays encrypted at rest.

const (
	chatSearchKeyLabel    = "chat-search-v1"
	chatSearchMaxTerms    = 16
	chatSearchLimit       = 20
	chatSnippetRunes      = 160
	chatReindexBatch      = 500
	chatTitleClientApp    = "janus-chat-title"
	chatTitleMaxTokens    = 24
	chatTitleMaxWords     = 6
	chatTitleInputRunes   = 2000
	chatTitleTimeout      = 60 * time.Second
	chatContextReserve    = 1024 // tokens kept free for template/estimate error
	chatContextMinReply   = 512  // tokens kept free for the reply, at least
	chatContextPerMessage = 4    // per-message framing overhead estimate
)

func (s *Server) chatSearchKey() []byte { return s.Cipher.DeriveKey(chatSearchKeyLabel) }

// chatIndex replaces the search terms of one message (or a title row) with
// those of text. Failures are logged, never surfaced: search is an index,
// and the admin reindex rebuilds it.
func (s *Server) chatIndex(ctx context.Context, userID, conversationID, messageID, text string) {
	key := s.chatSearchKey()
	hashes := chatsearch.Hashes(key, userID, chatsearch.Terms(text))
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.IndexChatMessage(bg, userID, conversationID, messageID, hashes); err != nil {
		s.Logger.Warn("index chat message", "error", err.Error(), "message_id", messageID)
	}
}

// chatIndexTitle indexes a conversation title under its title row.
func (s *Server) chatIndexTitle(ctx context.Context, c *store.ChatConversation) {
	s.chatIndex(ctx, c.UserID, c.ID, store.ChatTitleMessageID(c.ID), c.Title)
}

// chatIndexable reports whether a message's body belongs in the index:
// user messages, and assistant replies that finished (complete or stopped).
// Streaming flushes are never indexed.
func chatIndexable(m *store.ChatMessage) bool {
	if m.Role == store.ChatRoleUser {
		return true
	}
	return m.Status == store.ChatStatusComplete || m.Status == store.ChatStatusStopped
}

func (s *Server) mountChatAdminRoutes(ar chi.Router) {
	ar.Post("/admin/chat/reindex", s.handleChatReindex)
}

type chatSearchResult struct {
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
	UpdatedAt      string `json:"updated_at"`
	Snippet        string `json:"snippet"`
}

// handleChatSearch finds the caller's conversations containing every query
// word (whole words after light stemming). Snippets are decrypted server
// side from the best matching message.
func (s *Server) handleChatSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	terms := chatsearch.Terms(r.URL.Query().Get("q"))
	if len(terms) > chatSearchMaxTerms {
		terms = terms[:chatSearchMaxTerms]
	}
	results := []chatSearchResult{}
	if len(terms) == 0 {
		WriteJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}
	hits, err := s.Store.SearchChat(ctx, user.ID, chatsearch.Hashes(s.chatSearchKey(), user.ID, terms), chatSearchLimit)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	for _, h := range hits {
		snippet := ""
		if h.BestMessageID != "" && !store.IsChatTitleMessageID(h.BestMessageID) {
			// The hit is already user-scoped; the conversation check keeps a
			// stray index row from ever reading another thread.
			if m, err := s.Store.ChatMessage(ctx, h.BestMessageID); err == nil && m.ConversationID == h.ConversationID {
				snippet = chatsearch.Snippet(s.chatDecrypt(m.ContentEnc), terms, chatSnippetRunes)
			}
		}
		if snippet == "" {
			snippet = chatsearch.Snippet(h.Title, terms, chatSnippetRunes)
		}
		results = append(results, chatSearchResult{
			ConversationID: h.ConversationID, Title: h.Title, UpdatedAt: store.FormatTime(h.UpdatedAt), Snippet: snippet,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleChatReindex rebuilds the whole search index from decrypted content,
// in batches of 500. Needed after an encryption-key rotation or an upgrade
// that changes tokenization. Admins trigger it but never see content.
func (s *Server) handleChatReindex(w http.ResponseWriter, r *http.Request) {
	if !s.chatEnabled() {
		WriteError(w, r, ErrNotFoundf("chat"))
		return
	}
	ctx := r.Context()
	key := s.chatSearchKey()
	var messages, cleared, titles, unreadable int
	for after := ""; ; {
		batch, err := s.Store.ChatMessagesForReindex(ctx, after, chatReindexBatch)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		msgs := make([]*store.ChatMessage, 0, len(batch))
		for _, it := range batch {
			msgs = append(msgs, it.Message)
		}
		atts, err := s.chatAttachmentsByMessage(ctx, msgs)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		for _, it := range batch {
			m := it.Message
			after = m.ID
			var hashes []string
			if chatIndexable(m) {
				text, err := s.Cipher.Decrypt(m.ContentEnc)
				if err != nil && m.ContentEnc != "" {
					unreadable++
					continue
				}
				text += s.chatAttachmentSearchText(ctx, atts[m.ID])
				hashes = chatsearch.Hashes(key, it.UserID, chatsearch.Terms(text))
			}
			if err := s.Store.IndexChatMessage(ctx, it.UserID, m.ConversationID, m.ID, hashes); err != nil {
				WriteError(w, r, err)
				return
			}
			if len(hashes) > 0 {
				messages++
			} else {
				cleared++
			}
		}
		if len(batch) < chatReindexBatch {
			break
		}
	}
	for after := ""; ; {
		batch, err := s.Store.ChatConversationsForReindex(ctx, after, chatReindexBatch)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		for _, c := range batch {
			after = c.ID
			hashes := chatsearch.Hashes(key, c.UserID, chatsearch.Terms(c.Title))
			if err := s.Store.IndexChatMessage(ctx, c.UserID, c.ID, store.ChatTitleMessageID(c.ID), hashes); err != nil {
				WriteError(w, r, err)
				return
			}
			titles++
		}
		if len(batch) < chatReindexBatch {
			break
		}
	}
	pruned, err := s.Store.PruneChatSearchIndex(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := map[string]any{
		"messages_indexed": messages, "messages_cleared": cleared, "titles_indexed": titles,
		"unreadable": unreadable, "pruned_rows": pruned,
	}
	s.audit(r, "chat_search_reindexed", "chat_search", "", nil, out)
	WriteJSON(w, http.StatusOK, out)
}

// --- in-process proxy calls ----------------------------------------------------

// chatProxyRequest builds the in-process /v1/chat/completions request every
// chat call uses: the user's hidden chat token (which fixes the billing
// team), the user, and a request id, so grants, quotas, the Security
// Gateway and metering apply as for API traffic. agent names the caller in
// the request log.
func chatProxyRequest(ctx context.Context, user *store.User, token *store.Token, requestID, agent string, payload []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Janus-Agent", agent)
	req.Header.Set("User-Agent", "Janus Chat")
	rctx := context.WithValue(req.Context(), ctxRequestID, requestID)
	rctx = context.WithValue(rctx, ctxUser, user)
	rctx = context.WithValue(rctx, ctxToken, token)
	return stampChatOrigin(req.WithContext(rctx), requestID), nil
}

// --- automatic titles ----------------------------------------------------------

func (s *Server) chatAutoTitleEnabled() bool { return s.Config != nil && s.Config.ChatAutoTitle }

// chatMaybeAutoTitle runs after a reply finished. When it was the first
// successful reply and the title is still the truncated first message, it
// asks the same model (same billing token) for a short title in the
// background. Any failure silently keeps the truncated title.
func (s *Server) chatMaybeAutoTitle(origin *chatOrigin, run *store.ChatRun, user *store.User, token *store.Token, model string) {
	if !s.chatAutoTitleEnabled() {
		return
	}
	// Not tracked by Drain: a title is cosmetic, and losing one at shutdown
	// just leaves the truncated title.
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.Logger.Error("chat title panicked", "panic", p)
			}
		}()
		ctx, cancel := context.WithTimeout(withChatOrigin(context.Background(), origin), chatTitleTimeout)
		defer cancel()
		c, err := s.Store.ChatConversation(ctx, user.ID, run.ConversationID)
		if err != nil {
			return
		}
		msgs, err := s.Store.ListChatMessages(ctx, c.ID)
		if err != nil {
			return
		}
		var firstUser, reply *store.ChatMessage
		completed := 0
		for _, m := range msgs {
			if m.Role == store.ChatRoleUser && firstUser == nil {
				firstUser = m
			}
			if m.Role == store.ChatRoleAssistant && m.Status == store.ChatStatusComplete {
				completed++
				if m.ID == run.MessageID {
					reply = m
				}
			}
		}
		if firstUser == nil || reply == nil || completed != 1 {
			return
		}
		question := s.chatDecrypt(firstUser.ContentEnc)
		if c.Title != chatTitle(question) {
			return // the user (or an earlier run) already named it
		}
		title := s.chatGenerateTitle(ctx, user, token, model, question, s.chatDecrypt(reply.ContentEnc))
		// The title is part of what the reply cost: add it to the footer.
		s.chatRefreshUsage(ctx, reply, origin)
		if title == "" {
			return
		}
		ok, err := s.Store.SetChatConversationTitleIf(ctx, user.ID, c.ID, c.Title, title)
		if err != nil || !ok {
			return
		}
		c.Title = title
		s.chatIndexTitle(ctx, c)
	}()
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// chatGenerateTitle asks the model for a title, non-streaming, through the
// proxy with X-Janus-Agent janus-chat-title (its own request-log row, billed
// like the chat). Returns "" on any failure.
func (s *Server) chatGenerateTitle(ctx context.Context, user *store.User, token *store.Token, model, question, answer string) string {
	payload, _ := json.Marshal(map[string]any{
		"model": model, "stream": false, "max_tokens": chatTitleMaxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": "Write a short title of at most 6 words for this conversation. Reply with the title only: no quotes, no punctuation at the end."},
			{"role": "user", "content": truncateRunes(question, chatTitleInputRunes)},
			{"role": "assistant", "content": truncateRunes(answer, chatTitleInputRunes)},
			{"role": "user", "content": "Title for the conversation above:"},
		},
	})
	req, err := chatProxyRequest(ctx, user, token, store.NewID(), chatTitleClientApp, payload)
	if err != nil {
		return ""
	}
	sink := &chatSink{header: http.Header{}, live: &chatLiveRun{}}
	func() {
		defer func() { _ = recover() }()
		s.handleProxy(sink, req)
	}()
	if sink.sse {
		return "" // asked for a buffered reply; ignore an unexpected stream
	}
	sink.finishRaw()
	if sink.errCd != "" || sink.status >= 400 {
		return ""
	}
	content, _, _ := sink.live.snapshot()
	return cleanChatTitle(content)
}

// cleanChatTitle keeps the first non-empty line, drops a "Title:" label,
// quotes and surrounding punctuation, and caps it at six words.
func cleanChatTitle(raw string) string {
	line := ""
	for _, l := range strings.Split(raw, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	if i := strings.Index(strings.ToLower(line), "title:"); i >= 0 && i < 4 {
		line = line[i+len("title:"):]
	}
	line = strings.Map(func(r rune) rune {
		switch r {
		case '"', '\'', '“', '”', '‘', '’', '«', '»', '`', '*', '#':
			return -1
		}
		return r
	}, line)
	words := strings.Fields(line)
	if len(words) > chatTitleMaxWords {
		words = words[:chatTitleMaxWords]
	}
	t := strings.TrimFunc(strings.Join(words, " "), func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
	return truncateRunes(t, 80)
}

// --- context trimming ------------------------------------------------------------

// chatContextWindow resolves the model name the way the proxy does (managed
// alias first, then the catalog) and returns its context window in tokens;
// 0 means unknown, and unknown means no trimming.
func (s *Server) chatContextWindow(ctx context.Context, model string) int64 {
	resolved, err := s.Store.ResolveModelForRequest(ctx, model)
	if err != nil {
		return 0
	}
	if resolved.Managed != nil {
		return resolved.Managed.ContextWindow
	}
	if resolved.Model != nil {
		return resolved.Model.ContextWindow
	}
	return 0
}

// chatEstimateTokens is the rough prompt size: chars/4 plus framing per
// message; a document part counts its recorded estimate, an image a flat
// chatImageTokens.
func chatEstimateTokens(history []chatWireMessage) int64 {
	var n int64
	for _, m := range history {
		n += chatMessageTokens(m)
	}
	return n
}

func chatMessageTokens(m chatWireMessage) int64 {
	n := int64(chatContextPerMessage)
	switch c := m.Content.(type) {
	case string:
		n += int64(len(c)) / 4
	case []chatPart:
		for _, p := range c {
			switch {
			case p.img != nil || p.ImageURL != nil:
				n += chatImageTokens
			case p.est > 0:
				n += p.est + chatContextPerMessage // + the <file> wrapper
			default:
				n += int64(len(p.Text)) / 4
			}
		}
	}
	return n
}

// trimChatHistory drops the oldest messages (after any leading system
// messages) until the estimate fits the window minus a fixed reserve
// (1024 tokens) and room for the reply (max(512, window/8) tokens). The
// latest message (the user's question) is always kept, even if it alone is
// too big — the proxy then reports the real error. window <= 0 = unknown:
// nothing is dropped. Returns the history to send and how many were dropped.
func trimChatHistory(history []chatWireMessage, window int64) ([]chatWireMessage, int) {
	if window <= 0 || len(history) <= 1 {
		return history, 0
	}
	budget := window - chatContextReserve - max(chatContextMinReply, window/8)
	if chatEstimateTokens(history) <= budget {
		return history, 0
	}
	head := 0
	for head < len(history)-1 && history[head].Role == "system" {
		head++
	}
	total := chatEstimateTokens(history)
	drop := 0
	for head+drop < len(history)-1 && total > budget {
		total -= chatMessageTokens(history[head+drop])
		drop++
	}
	if drop == 0 {
		return history, 0
	}
	out := make([]chatWireMessage, 0, len(history)-drop)
	out = append(out, history[:head]...)
	out = append(out, history[head+drop:]...)
	return out, drop
}

// chatRefreshUsage re-sums a finished reply's metering after a later
// sub-call (the auto-title) and saves the new totals on the message.
func (s *Server) chatRefreshUsage(ctx context.Context, m *store.ChatMessage, origin *chatOrigin) {
	if origin == nil {
		return
	}
	want := int(origin.calls.Load())
	deadline := time.Now().Add(chatUsageWaitTimeout)
	for {
		tot, err := s.Store.ChatMessageUsage(ctx, m.ID)
		if err == nil && (tot.Events >= want || !time.Now().Before(deadline)) {
			if tot.Events == 0 {
				return
			}
			m.TokensIn, m.TokensOut, m.TokensCached, m.CostNano = tot.TokensIn, tot.TokensOut, tot.TokensCached, tot.CostNano
			if err := s.Store.UpdateChatMessage(ctx, m); err != nil {
				s.Logger.Warn("chat title: update reply usage", "error", err.Error(), "message_id", m.ID)
			}
			return
		}
		if err != nil || ctx.Err() != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
