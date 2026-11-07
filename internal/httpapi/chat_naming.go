package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// Chat naming: once a chat has a few replies, or has gone idle, Janus asks a
// model for a short title in the background, once per chat.
//
// Which model, and what it sees:
//   - Same model (the default): the model that wrote the chat's last reply.
//     It has already seen the whole conversation, so naming sends nothing
//     anywhere new; it gets the first question, the first answer and the
//     latest question, each trimmed.
//   - A model the administrator chose: a different destination, so less goes
//     there. It is a system task, so the person needs no grant for it (the
//     administrator chose where naming goes; the call is still metered to
//     the chat's billing context). A chat where the Security Gateway had any
//     finding (a reply blocked, a redaction, a finding it only observed) is
//     never sent; the model gets only what the person typed (the first
//     question and the latest one, trimmed), never replies, reasoning, files
//     or instructions. The naming request still passes through the Security
//     Gateway.
//
// Naming asks for no reasoning (reasoning_effort "none"): a title needs
// none, and a reasoning model otherwise spends its output on thinking.
//   - Off: the first message stays the title.
//
// A chat a person renamed is never named. Temporary chats are never named.

const (
	// chatNamingAfterReplies: a chat is named right after this many replies
	// (a one-question chat is named once idle instead).
	chatNamingAfterReplies = 3
	// chatNamingIdle: a chat with at least one reply is named once idle
	// this long.
	chatNamingIdle = 10 * time.Minute
	// chatNamingLookback: chats idle longer than this are left alone (no
	// sweep through months of history after an upgrade).
	chatNamingLookback = 7 * 24 * time.Hour
	chatNamingBatch    = 20
	// What a model chosen by the administrator sees, per message and in all.
	chatNamingOtherRunes      = 400
	chatNamingOtherTotalRunes = 800
)

// chatNamingPlan is what one naming call sends, and to which model.
type chatNamingPlan struct {
	model string
	// adminChosen: the administrator's naming model, called as a system
	// task without the person's grant.
	adminChosen bool
	// messages is the conversation excerpt, before the instruction.
	messages []map[string]string
}

// chatNamingMode is the admin's choice; an unset choice follows
// JANUS_CHAT_AUTO_TITLE (on by default).
func (s *Server) chatNamingMode(ctx context.Context) (mode, model string) {
	cfg := s.chatAdmin(ctx)
	switch cfg.Naming {
	case store.ChatNamingOff:
		return store.ChatNamingOff, ""
	case store.ChatNamingModel:
		if m := strings.TrimSpace(cfg.NamingModel); m != "" {
			return store.ChatNamingModel, m
		}
	}
	if cfg.Naming == "" && !s.chatAutoTitleEnabled() {
		return store.ChatNamingOff, ""
	}
	return store.ChatNamingSameModel, ""
}

// chatBranch is the conversation's current branch, oldest first.
func chatBranch(c *store.ChatConversation, msgs []*store.ChatMessage) []*store.ChatMessage {
	byID := make(map[string]*store.ChatMessage, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	leaf := byID[c.CurrentLeafID]
	if leaf == nil && len(msgs) > 0 {
		leaf = msgs[len(msgs)-1]
	}
	var chain []*store.ChatMessage
	for m, n := leaf, 0; m != nil && n <= len(msgs); m, n = byID[m.ParentID], n+1 {
		chain = append(chain, m)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// chatPlanNaming decides whether and how to name c. ok=false means leave
// it (nothing to name from, or the chosen model may not see this chat).
func (s *Server) chatPlanNaming(ctx context.Context, c *store.ChatConversation, msgs []*store.ChatMessage) (chatNamingPlan, bool) {
	mode, chosen := s.chatNamingMode(ctx)
	if mode == store.ChatNamingOff || c.Temporary {
		return chatNamingPlan{}, false
	}
	branch := chatBranch(c, msgs)
	var firstQ, firstA, lastQ, lastModel string
	for _, m := range branch {
		text := strings.TrimSpace(s.chatDecrypt(m.ContentEnc))
		switch {
		case m.Role == store.ChatRoleUser && text != "":
			if firstQ == "" {
				firstQ = text
			} else {
				lastQ = text
			}
		case m.Role == store.ChatRoleAssistant && m.Status == store.ChatStatusComplete:
			if firstA == "" {
				firstA = text
			}
			lastModel = m.ModelName
		}
	}
	if firstQ == "" || lastModel == "" {
		return chatNamingPlan{}, false
	}
	if c.Title != chatTitle(firstQ) {
		return chatNamingPlan{}, false // a person (or an older Janus) named it
	}
	if mode == store.ChatNamingSameModel || chosen == lastModel {
		msgs := []map[string]string{
			{"role": "user", "content": truncateRunes(firstQ, chatTitleInputRunes)},
			{"role": "assistant", "content": truncateRunes(firstA, chatTitleInputRunes)},
		}
		if lastQ != "" {
			msgs = append(msgs, map[string]string{"role": "user", "content": truncateRunes(lastQ, chatTitleInputRunes/2)})
		}
		return chatNamingPlan{model: lastModel, messages: msgs}, true
	}
	// A different model: never a chat the Security Gateway had anything
	// to say about, and only what the person typed, trimmed.
	flagged, err := s.Store.ChatHadSecurityFindings(ctx, c.ID)
	if err != nil || flagged {
		return chatNamingPlan{}, false
	}
	excerpt := "First message: " + truncateRunes(firstQ, chatNamingOtherRunes)
	if lastQ != "" {
		excerpt += "\nLatest message: " + truncateRunes(lastQ, chatNamingOtherRunes)
	}
	return chatNamingPlan{model: chosen, adminChosen: true, messages: []map[string]string{
		{"role": "user", "content": truncateRunes(excerpt, chatNamingOtherTotalRunes+40)},
	}}, true
}

// chatNameConversation names c now if it has not been named. Exactly one
// caller across replicas gets to (ClaimChatNaming); a failed call keeps the
// first-message title rather than retrying a paid request.
func (s *Server) chatNameConversation(ctx context.Context, user *store.User, c *store.ChatConversation, origin *chatOrigin, reply *store.ChatMessage) {
	msgs, err := s.Store.ListChatMessages(ctx, c.ID)
	if err != nil {
		return
	}
	plan, ok := s.chatPlanNaming(ctx, c, msgs)
	if !ok {
		return
	}
	if claimed, err := s.Store.ClaimChatNaming(ctx, user.ID, c.ID); err != nil || !claimed {
		return
	}
	// The conversation's own billing context pays, as its replies did.
	token, err := s.Store.ChatToken(ctx, user.ID, c.TeamID)
	if err != nil {
		return
	}
	if plan.adminChosen {
		ctx = context.WithValue(ctx, ctxChatNamingModel, plan.model)
	}
	title := s.chatGenerateTitle(ctx, user, token, plan.model, plan.messages)
	if reply != nil {
		// Named right after a reply: the title is part of what it cost.
		s.chatRefreshUsage(ctx, reply, origin)
	}
	if title == "" {
		return
	}
	if ok, err := s.Store.SetChatConversationTitleIf(ctx, user.ID, c.ID, c.Title, title); err != nil || !ok {
		return
	}
	c.Title = title
	s.chatIndexTitle(ctx, c)
}

// chatMaybeAutoTitle runs after a reply finished: once the chat has
// chatNamingAfterReplies finished replies it is named in the background
// (shorter chats are named by the idle sweep).
func (s *Server) chatMaybeAutoTitle(origin *chatOrigin, run *store.ChatRun, user *store.User) {
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.Logger.Error("chat naming panicked", "panic", p)
			}
		}()
		ctx, cancel := context.WithTimeout(withChatOrigin(context.Background(), origin), chatTitleTimeout)
		defer cancel()
		if mode, _ := s.chatNamingMode(ctx); mode == store.ChatNamingOff {
			return
		}
		c, err := s.Store.ChatConversation(ctx, user.ID, run.ConversationID)
		if err != nil || c.TitleNamedAt != "" {
			return
		}
		msgs, err := s.Store.ListChatMessages(ctx, c.ID)
		if err != nil {
			return
		}
		var reply *store.ChatMessage
		done := 0
		for _, m := range chatBranch(c, msgs) {
			if m.Role == store.ChatRoleAssistant && m.Status == store.ChatStatusComplete {
				done++
				if m.ID == run.MessageID {
					reply = m
				}
			}
		}
		if reply == nil || done < chatNamingAfterReplies {
			return
		}
		s.chatNameConversation(ctx, user, c, origin, reply)
	}()
}

var chatNamingSweeping atomic.Bool

// chatSweepNaming names chats that went idle before reaching
// chatNamingAfterReplies replies. One sweep at a time per replica; the
// claim keeps replicas from naming the same chat twice.
func (s *Server) chatSweepNaming(now time.Time) {
	if !chatNamingSweeping.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer chatNamingSweeping.Store(false)
		defer func() {
			if p := recover(); p != nil {
				s.Logger.Error("chat naming sweep panicked", "panic", p)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		mode, _ := s.chatNamingMode(ctx)
		var due []*store.ChatConversation
		var err error
		if mode != store.ChatNamingOff {
			due, err = s.Store.ChatsToName(ctx, now.Add(-chatNamingIdle), now.Add(-chatNamingLookback), chatNamingBatch)
		}
		cancel()
		if err != nil {
			s.Logger.Warn("list chats to name", "error", err.Error())
			return
		}
		for _, c := range due {
			ctx, cancel := context.WithTimeout(context.Background(), chatTitleTimeout)
			user, err := s.Store.UserByID(ctx, c.UserID)
			if err == nil && user.IsActive && user.DeletedAt == nil {
				s.chatNameConversation(ctx, user, c, nil, nil)
			}
			// Whatever happened, do not pick this chat up again.
			_, _ = s.Store.ClaimChatNaming(ctx, c.UserID, c.ID)
			cancel()
		}
	}()
}

// ctxChatNamingModel marks a naming request to the administrator's naming
// model: the proxy serves exactly that model without the caller's grant.
const ctxChatNamingModel ctxKey = "chat_naming_model"

// chatNamingGrantFree reports whether this request is the administrator's
// naming task for model (and so needs no grant from the person).
func chatNamingGrantFree(r *http.Request, model string) bool {
	m, _ := r.Context().Value(ctxChatNamingModel).(string)
	return m != "" && m == model && r.Header.Get("X-Janus-Agent") == chatTitleClientApp
}
