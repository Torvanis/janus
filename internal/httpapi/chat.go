package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Janus Chat — Phase 0 spike.
//
// The browser never holds an API credential. It talks to /api/v1/chat with
// its session; the server runs each generation through the SAME proxy
// pipeline API clients use (handleProxy, in process), as the user, with a
// hidden per-(user, team) chat token so grants, quotas, the Security
// Gateway, routing, fallbacks, personal subscriptions and metering all
// apply unchanged and the request log shows client_app=janus-chat.
//
// A generation is a server-owned run: it does not stop when the tab closes.
// The owning pod flushes the partial reply to the database and heart-beats
// the run; any replica serves the event stream from the database, and a run
// whose owner dies is reaped as interrupted.

const (
	chatClientApp = "janus-chat"
	// chatMaxCompare is how many models one message can be compared across.
	chatMaxCompare       = 3
	chatFlushEvery       = 500 * time.Millisecond
	chatRunStaleAfter    = 30 * time.Second
	chatReapEvery        = 15 * time.Second
	chatLocalPollEvery   = 80 * time.Millisecond
	chatRemotePollEvery  = 400 * time.Millisecond
	chatMaxMessageChars  = 200_000
	chatUsageWaitTimeout = 5 * time.Second
)

// chatRuntime is the per-process run registry.
type chatRuntime struct {
	mu   sync.Mutex
	runs map[string]*chatLiveRun
	once sync.Once
	host string
}

type chatLiveRun struct {
	mu        sync.Mutex
	content   strings.Builder
	reasoning strings.Builder
	// extras collects the provider's search activity and cited sources.
	extras chatExtras
	status string
	cancel context.CancelFunc
	msg    *store.ChatMessage
}

func (r *chatLiveRun) snapshot() (content, reasoning, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.content.String(), r.reasoning.String(), r.status
}

func (s *Server) chatRT() *chatRuntime {
	s.chatOnce.Do(func() {
		host, _ := os.Hostname()
		s.chat = &chatRuntime{runs: map[string]*chatLiveRun{}, host: host}
	})
	return s.chat
}

func (s *Server) chatEnabled() bool { return s.Config != nil && s.Config.ChatPreview }

func (s *Server) mountChatRoutes(pr chi.Router) {
	pr.Route("/chat", func(cr chi.Router) {
		cr.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !s.chatEnabled() {
					WriteError(w, r, ErrNotFoundf("chat"))
					return
				}
				if SessionFrom(r.Context()) == nil {
					WriteError(w, r, ErrForbidden("Janus Chat is used from the web app; API clients call /v1/chat/completions directly."))
					return
				}
				if ok, why := s.chatAllowedFor(r.Context(), UserFrom(r.Context())); !ok &&
					!(s.chatAdmin(r.Context()).Enabled && chatShareRouteOpen(r)) {
					WriteError(w, r, ErrForbidden(why))
					return
				}
				s.startChatReaper()
				next.ServeHTTP(w, r)
			})
		})
		cr.Get("/catalog", s.handleChatCatalog)
		cr.Get("/search", s.handleChatSearch)
		cr.Get("/settings", s.handleChatSettingsGet)
		cr.Get("/memory", s.handleChatMemoryList)
		cr.Put("/memory", s.handleChatMemorySettings)
		cr.Post("/memory", s.handleChatMemoryCreate)
		cr.Delete("/memory", s.handleChatMemoryDeleteAll)
		cr.Patch("/memory/{id}", s.handleChatMemoryUpdate)
		cr.Delete("/memory/{id}", s.handleChatMemoryDelete)
		cr.Put("/settings", s.handleChatSettingsPut)
		cr.Get("/prompts", s.handleChatPromptList)
		cr.Post("/prompts", s.handleChatPromptCreate)
		cr.Patch("/prompts/{id}", s.handleChatPromptUpdate)
		cr.Delete("/prompts/{id}", s.handleChatPromptDelete)
		cr.Get("/conversations", s.handleChatList)
		cr.Post("/conversations", s.handleChatCreate)
		cr.Get("/conversations/{id}", s.handleChatGet)
		cr.Patch("/conversations/{id}", s.handleChatUpdate)
		cr.Delete("/conversations/{id}", s.handleChatDelete)
		cr.Get("/conversations/{id}/export", s.handleChatExport)
		cr.Get("/conversations/{id}/shares", s.handleChatShareList)
		cr.Post("/conversations/{id}/shares", s.handleChatShareCreate)
		cr.Delete("/conversations/{id}/shares/{share}", s.handleChatShareRevoke)
		cr.Get("/shared/{share}", s.handleChatShareGet)
		cr.Post("/conversations/{id}/messages", s.handleChatSend)
		cr.Get("/runs/{id}/events", s.handleChatEvents)
		cr.Post("/attachments", s.handleChatUpload)
		cr.Get("/attachments/{id}/content", s.handleChatAttachmentContent)
		cr.Delete("/attachments/{id}", s.handleChatAttachmentDelete)
		cr.Post("/runs/{id}/stop", s.handleChatStop)
	})
}

func (s *Server) startChatReaper() {
	rt := s.chatRT()
	rt.once.Do(func() {
		go func() {
			t := time.NewTicker(chatReapEvery)
			defer t.Stop()
			for range t.C {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if n, err := s.Store.ReapChatRuns(ctx, time.Now().Add(-chatRunStaleAfter)); err != nil {
					s.Logger.Warn("reap chat runs", "error", err.Error())
				} else if n > 0 {
					s.Logger.Info("reaped interrupted chat runs", "count", n)
				}
				cancel()
				// Uploads that were never sent are purged after a day.
				ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
				s.chatSweepAttachments(ctx)
				cancel()
				// Temporary chats go once idle for a day.
				ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
				s.chatSweepTemporary(ctx, time.Now())
				cancel()
				// Chats that went quiet get their name.
				s.chatSweepNaming(time.Now())
			}
		}()
	})
}

// --- wire shapes -------------------------------------------------------------

type chatMessageView struct {
	ID                 string  `json:"id"`
	ParentID           string  `json:"parent_id"`
	Role               string  `json:"role"`
	Content            string  `json:"content"`
	Reasoning          string  `json:"reasoning,omitempty"`
	Model              string  `json:"model,omitempty"`
	ServedModel        string  `json:"served_model,omitempty"`
	TeamID             string  `json:"team_id"`
	RequestID          string  `json:"request_id,omitempty"`
	Status             string  `json:"status"`
	ErrorCode          string  `json:"error_code,omitempty"`
	ErrorMessage       string  `json:"error_message,omitempty"`
	ContextDropped     int     `json:"context_dropped"`
	TokensIn           int64   `json:"tokens_in"`
	TokensOut          int64   `json:"tokens_out"`
	TokensCached       int64   `json:"tokens_cached"`
	CostUSD            float64 `json:"cost_usd"`
	TokensOutPerSecond float64 `json:"tokens_out_per_second"`
	LatencyMs          int     `json:"latency_ms"`
	TTFBMs             int     `json:"ttfb_ms"`
	CreatedAt          string  `json:"created_at"`
	// Attachments are the files sent with a user message.
	Attachments []chatAttachmentView `json:"attachments,omitempty"`
	// Notes (assistant replies) come from the request log: how the reply
	// was routed and what the Security Gateway did. Omitted when nothing
	// noteworthy happened.
	Notes *chatReplyNotes `json:"notes,omitempty"`
	// Activity and Sources: what the provider searched and the pages it
	// cited (replies that used a search tool).
	Activity []chatActivity `json:"activity,omitempty"`
	Sources  []chatSource   `json:"sources,omitempty"`
	// Question: the model asked the person this (ask_user).
	Question *chatQuestion `json:"question,omitempty"`
	// CompareGroup is shared by replies made side by side (Compare).
	CompareGroup string `json:"compare_group,omitempty"`
}

// chatReplyNotes is the request-log context shown under a reply.
type chatReplyNotes struct {
	RequestedModel      string `json:"requested_model,omitempty"`
	RouteReason         string `json:"route_reason,omitempty"`
	FallbackReason      string `json:"fallback_reason,omitempty"`
	ReasoningAdjustment string `json:"reasoning_adjustment,omitempty"`
	SecgwAction         string `json:"secgw_action,omitempty"`
	SecgwViolations     int    `json:"secgw_violations,omitempty"`
}

func chatNotesFrom(n store.ChatRequestNote) *chatReplyNotes {
	out := &chatReplyNotes{RouteReason: n.RouteReason, FallbackReason: n.FallbackReason, ReasoningAdjustment: n.ReasoningAdjustment}
	if n.RequestedModel != "" && n.ServedModel != "" && n.RequestedModel != n.ServedModel {
		out.RequestedModel = n.RequestedModel
	}
	// "checked" (inspected, clean) is not news to the reader.
	if n.SecgwAction != "" && n.SecgwAction != store.SecgwActionChecked {
		out.SecgwAction, out.SecgwViolations = n.SecgwAction, n.SecgwViolations
	}
	if *out == (chatReplyNotes{}) {
		return nil
	}
	return out
}

// chatAttachNotes fills Notes on assistant views from the request log.
func (s *Server) chatAttachNotes(ctx context.Context, views []chatMessageView) {
	var ids []string
	for _, v := range views {
		if v.Role == store.ChatRoleAssistant && v.RequestID != "" && v.Status != store.ChatStatusStreaming {
			ids = append(ids, v.RequestID)
		}
	}
	if len(ids) == 0 {
		return
	}
	notes, err := s.Store.ChatRequestNotes(ctx, ids)
	if err != nil {
		s.Logger.Warn("chat request notes", "error", err.Error())
		return
	}
	for i := range views {
		if n, ok := notes[views[i].RequestID]; ok && views[i].Role == store.ChatRoleAssistant {
			views[i].Notes = chatNotesFrom(n)
		}
	}
}

func (s *Server) chatView(m *store.ChatMessage) chatMessageView {
	v := chatMessageView{
		ID: m.ID, ParentID: m.ParentID, Role: m.Role,
		Content: s.chatDecrypt(m.ContentEnc), Reasoning: s.chatDecrypt(m.ReasoningEnc),
		Model: m.ModelName, ServedModel: m.ServedModel, TeamID: m.TeamID, RequestID: m.RequestID,
		Status: m.Status, ErrorCode: m.ErrorCode, ErrorMessage: m.ErrorMessage, ContextDropped: m.ContextDropped,
		TokensIn: m.TokensIn, TokensOut: m.TokensOut, TokensCached: m.TokensCached,
		CostUSD: usage.USD(m.CostNano), TokensOutPerSecond: m.TokensOutPerSecond,
		LatencyMs: m.LatencyMs, TTFBMs: m.TTFBMs, CreatedAt: store.FormatTime(m.CreatedAt),
	}
	if e := s.chatDecodeExtras(m.ExtrasEnc); e != nil {
		v.Activity, v.Sources, v.Question = e.Activity, e.Sources, e.Question
	}
	return v
}

// chatViewWith is chatView plus the message's attachments.
func (s *Server) chatViewWith(m *store.ChatMessage, atts []*store.ChatAttachment) chatMessageView {
	v := s.chatView(m)
	for _, a := range atts {
		v.Attachments = append(v.Attachments, s.chatAttachmentView(a))
	}
	return v
}

func (s *Server) chatEncrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return s.Cipher.Encrypt(plain)
}

func (s *Server) chatDecrypt(env string) string {
	if env == "" {
		return ""
	}
	out, err := s.Cipher.Decrypt(env)
	if err != nil {
		return ""
	}
	return out
}

// --- conversations -----------------------------------------------------------

func (s *Server) handleChatList(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	list, err := s.Store.ListChatConversations(r.Context(), user.ID, 200)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	views := make([]chatConversationView, 0, len(list))
	for _, c := range list {
		views = append(views, s.chatConvView(c))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"conversations": views})
}

type chatConversationBody struct {
	Title  *string `json:"title"`
	Model  *string `json:"model"`
	LeafID *string `json:"current_leaf_id"`
	Pinned *bool   `json:"pinned"`
	// Instructions apply to this conversation only (after the user's own).
	Instructions *string `json:"instructions"`
	// ReasoningEffort: "" (model default), low, medium or high.
	ReasoningEffort *string `json:"reasoning_effort"`
	// Temporary is set at creation only: never listed, never searchable,
	// removed once idle for a day.
	Temporary *bool `json:"temporary"`
	// ToolsOff lists tools turned off for this conversation (on by default).
	ToolsOff *[]string `json:"tools_off"`
	// ImageModel is the model create_image uses in this chat ("" = automatic).
	ImageModel *string `json:"image_model"`
}

// applyChatConversationBody copies the editable settings onto c.
func (s *Server) applyChatConversationBody(c *store.ChatConversation, body chatConversationBody) *APIError {
	if body.Pinned != nil {
		c.Pinned = *body.Pinned && !c.Temporary
	}
	if body.ReasoningEffort != nil {
		v := strings.ToLower(strings.TrimSpace(*body.ReasoningEffort))
		if !validChatEffort(v) {
			return ErrInvalidRequest("Thinking effort must be low, medium, high or empty for the model's default").WithParam("reasoning_effort")
		}
		c.ReasoningEffort = v
	}
	if body.ToolsOff != nil {
		for _, id := range *body.ToolsOff {
			if !store.ValidChatTool(id) {
				return ErrInvalidRequest("Unknown tool " + strconv.Quote(id) + ".").WithParam("tools_off")
			}
		}
		c.ToolsOff = store.CleanChatTools(*body.ToolsOff)
	}
	if body.ImageModel != nil {
		// Checked against what the person may use when the tool runs; an
		// unavailable choice falls back to automatic there.
		c.ImageModel = strings.TrimSpace(*body.ImageModel)
		if len(c.ImageModel) > 200 {
			return ErrInvalidRequest("That image model name is too long.").WithParam("image_model")
		}
	}
	if body.Instructions != nil {
		v := strings.TrimSpace(*body.Instructions)
		if len(v) > chatMaxInstructionChars {
			return ErrInvalidRequest("Instructions can be at most 8,000 characters").WithParam("instructions")
		}
		enc, err := s.chatEncrypt(v)
		if err != nil {
			return ErrInternal()
		}
		c.InstructionsEnc = enc
	}
	return nil
}

func (s *Server) handleChatCreate(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body chatConversationBody
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	c := &store.ChatConversation{UserID: user.ID}
	if body.Model != nil {
		c.ModelName = strings.TrimSpace(*body.Model)
	}
	if body.Title != nil {
		c.Title = strings.TrimSpace(*body.Title)
	}
	c.Temporary = body.Temporary != nil && *body.Temporary
	if c.Title != "" {
		c.TitleNamedAt = store.FormatTime(time.Now().UTC())
	}
	if apiErr := s.applyChatConversationBody(c, body); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	team, err := s.chatBillingTeam(r.Context(), user)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	c.TeamID = team
	if err := s.Store.CreateChatConversation(r.Context(), c); err != nil {
		WriteError(w, r, err)
		return
	}
	if c.Title != "" {
		s.chatIndexTitle(r.Context(), c)
	}
	WriteJSON(w, http.StatusCreated, s.chatConvView(c))
}

// chatBillingTeam is who pays for a chat message: the "Working under"
// selection at the top of the app (the session's active team, "" =
// personal), read at send time. The model picker reads /api/v1/models,
// which resolves grants for the same selection, so what is offered is
// exactly what the send may use.
func (s *Server) chatBillingTeam(ctx context.Context, user *store.User) (string, error) {
	team, err := s.selectedTeamID(ctx, user)
	if err != nil {
		return "", err
	}
	if team != "" {
		if err := s.Store.ValidateTeamMember(ctx, team, user.ID); err != nil {
			return "", teamContextError(err)
		}
	}
	return team, nil
}

func (s *Server) handleChatGet(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	c, err := s.Store.ChatConversation(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	msgs, err := s.Store.ListChatMessages(r.Context(), c.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	atts, err := s.chatAttachmentsByMessage(r.Context(), msgs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	groups, err := s.Store.ChatCompareGroups(r.Context(), c.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	views := make([]chatMessageView, 0, len(msgs))
	for _, m := range msgs {
		v := s.chatViewWith(m, atts[m.ID])
		v.CompareGroup = groups[m.ID]
		views = append(views, v)
	}
	s.chatAttachNotes(r.Context(), views)
	runs, err := s.Store.StreamingChatRuns(r.Context(), c.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"conversation": s.chatConvView(c), "messages": views, "runs": runs})
}

func chatNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFoundf("conversation")
	}
	return err
}

func (s *Server) handleChatUpdate(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	c, err := s.Store.ChatConversation(r.Context(), user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	var body chatConversationBody
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	oldTitle := c.Title
	if body.Title != nil {
		c.Title = strings.TrimSpace(*body.Title)
	}
	if body.Model != nil {
		c.ModelName = strings.TrimSpace(*body.Model)
	}
	if body.LeafID != nil {
		c.CurrentLeafID = *body.LeafID
	}
	if apiErr := s.applyChatConversationBody(c, body); apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if err := s.Store.UpdateChatConversation(r.Context(), c); err != nil {
		WriteError(w, r, err)
		return
	}
	if c.Title != oldTitle {
		s.chatIndexTitle(r.Context(), c)
		// A person named it: automatic naming leaves it alone.
		_ = s.Store.MarkChatNamed(r.Context(), user.ID, c.ID)
	}
	WriteJSON(w, http.StatusOK, s.chatConvView(c))
}

func (s *Server) handleChatDelete(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	id := chi.URLParam(r, "id")
	atts, err := s.Store.ChatAttachmentsForConversation(r.Context(), user.ID, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.DeleteChatConversation(r.Context(), user.ID, id); err != nil {
		WriteError(w, r, err)
		return
	}
	// Its share links end with it.
	if err := s.Store.RevokeChatSharesForConversation(r.Context(), user.ID, id); err != nil {
		s.Logger.Warn("revoke chat shares", "error", err.Error(), "conversation_id", id)
	}
	// The conversation is gone for the user either way; its files are
	// removed best effort (failures are logged).
	s.chatDeleteAttachments(r.Context(), atts)
	w.WriteHeader(http.StatusNoContent)
}

// --- send --------------------------------------------------------------------

type chatSendBody struct {
	Content string `json:"content"`
	// ParentID is the message the new user message answers; "" = the
	// conversation's current leaf. Editing a message = sending with that
	// message's parent, which forks a sibling branch.
	ParentID *string `json:"parent_id"`
	// Regenerate re-answers ParentID (a user message) without adding a new
	// user message.
	Regenerate bool   `json:"regenerate"`
	Model      string `json:"model"`
	// AttachmentIDs are uploads (POST /chat/attachments) to send with the
	// new user message; at most ten.
	AttachmentIDs []string `json:"attachment_ids"`
	// CompareModels: up to two more models answer the same message, side
	// by side (sibling replies sharing a compare group).
	CompareModels []string `json:"compare_models"`
	// Research runs deep research: many searches and page reads, then a
	// cited report (also saved as a Markdown file on the reply).
	Research bool `json:"research"`
}

func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	c, err := s.Store.ChatConversation(ctx, user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	var body chatSendBody
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if !body.Regenerate && strings.TrimSpace(body.Content) == "" && len(body.AttachmentIDs) == 0 {
		WriteError(w, r, ErrInvalidRequest("Type a message first."))
		return
	}
	if body.Regenerate && len(body.AttachmentIDs) > 0 {
		WriteError(w, r, ErrInvalidRequest("Regenerate reuses the files already sent; attach new files to a new message."))
		return
	}
	if m := strings.TrimSpace(body.Model); m != "" && s.chatImageModel(ctx, m) && len(body.AttachmentIDs) > 0 {
		WriteError(w, r, ErrInvalidRequest("Image models make a new image from your description; attached files cannot be edited yet. Remove the files, or pick a chat model."))
		return
	}
	newAtts, err := s.chatResolveAttachments(ctx, user.ID, c.ID, body.AttachmentIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if len(body.Content) > chatMaxMessageChars {
		WriteError(w, r, ErrInvalidRequest("That message is too long to send."))
		return
	}
	if m := strings.TrimSpace(body.Model); m != "" {
		c.ModelName = m
	}
	if c.ModelName == "" {
		WriteError(w, r, ErrInvalidRequest("Pick a model first."))
		return
	}
	if err := s.chatModelAllowed(ctx, c.ModelName); err != nil {
		WriteError(w, r, err)
		return
	}
	var compare []string
	for _, m := range body.CompareModels {
		if m = strings.TrimSpace(m); m != "" && m != c.ModelName && !slices.Contains(compare, m) {
			compare = append(compare, m)
		}
	}
	if len(compare) > chatMaxCompare-1 {
		WriteError(w, r, ErrInvalidRequest("Compare up to three models at a time.").WithParam("compare_models"))
		return
	}
	if len(compare) > 0 && body.Research {
		WriteError(w, r, ErrInvalidRequest("Deep research runs on one model at a time. Turn off Compare, or deep research.").WithParam("research"))
		return
	}
	for _, m := range append([]string{c.ModelName}, compare...) {
		if len(compare) > 0 && s.chatImageModel(ctx, m) {
			WriteError(w, r, ErrInvalidRequest("Image models cannot be compared; pick chat models.").WithParam("compare_models"))
			return
		}
	}
	for _, m := range compare {
		if err := s.chatModelAllowed(ctx, m); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if body.Research && s.chatImageModel(ctx, c.ModelName) {
		WriteError(w, r, ErrInvalidRequest("Deep research needs a chat model.").WithParam("research"))
		return
	}
	team, err := s.chatBillingTeam(ctx, user)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	c.TeamID = team

	parent := c.CurrentLeafID
	if body.ParentID != nil {
		parent = *body.ParentID
	}
	all, err := s.Store.ListChatMessages(ctx, c.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	byID := map[string]*store.ChatMessage{}
	for _, m := range all {
		byID[m.ID] = m
	}
	if parent != "" && byID[parent] == nil {
		WriteError(w, r, ErrInvalidRequest("That message is not part of this conversation."))
		return
	}

	userMsgID := parent
	if !body.Regenerate {
		enc, err := s.chatEncrypt(body.Content)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		um := &store.ChatMessage{ConversationID: c.ID, ParentID: parent, Role: store.ChatRoleUser, ContentEnc: enc,
			TeamID: c.TeamID, Status: store.ChatStatusComplete}
		if err := s.Store.InsertChatMessage(ctx, um); err != nil {
			WriteError(w, r, err)
			return
		}
		byID[um.ID] = um
		userMsgID = um.ID
		if len(newAtts) > 0 {
			if newAtts, err = s.chatBindAttachments(ctx, user.ID, c.ID, um.ID, newAtts); err != nil {
				WriteError(w, r, err)
				return
			}
		}
		s.chatIndex(ctx, user.ID, c.ID, um.ID, body.Content+s.chatAttachmentSearchText(ctx, newAtts))
	} else if userMsgID == "" || byID[userMsgID].Role != store.ChatRoleUser {
		WriteError(w, r, ErrInvalidRequest("Regenerate needs the user message to answer."))
		return
	}

	branchAtts, err := s.chatAttachmentsByMessage(ctx, all)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !body.Regenerate {
		branchAtts[userMsgID] = newAtts
	}
	token, err := s.Store.ChatToken(ctx, user.ID, c.TeamID)
	if err != nil {
		WriteError(w, r, teamContextError(err))
		return
	}
	question := s.chatDecrypt(byID[userMsgID].ContentEnc)
	group := ""
	if len(compare) > 0 {
		group = store.NewID()
	}
	first, err := s.chatStartReply(r, user, token, c, c.ModelName, userMsgID, byID, branchAtts, body.Research, question, group)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	c.CurrentLeafID = first.msg.ID
	newTitle := c.Title == ""
	if newTitle {
		t := question
		if strings.TrimSpace(t) == "" && len(branchAtts[userMsgID]) > 0 {
			t = s.chatDecrypt(branchAtts[userMsgID][0].FilenameEnc)
		}
		c.Title = chatTitle(t)
	}
	if err := s.Store.UpdateChatConversation(ctx, c); err != nil {
		WriteError(w, r, err)
		return
	}
	if newTitle {
		s.chatIndexTitle(ctx, c)
	}
	// Compare: the same message answered by each other model, side by
	// side (sibling replies sharing a compare group).
	var others []map[string]any
	for _, m := range compare {
		rep, err := s.chatStartReply(r, user, token, c, m, userMsgID, byID, branchAtts, false, question, group)
		if err != nil {
			s.Logger.Warn("chat compare: start reply", "model", m, "error", err.Error())
			continue
		}
		others = append(others, map[string]any{"run_id": rep.runID, "assistant_message": rep.view, "warnings": rep.warnings})
	}

	userView := s.chatViewWith(byID[userMsgID], branchAtts[userMsgID])
	out := map[string]any{
		"run_id": first.runID, "user_message": userView, "assistant_message": first.view, "conversation": s.chatConvView(c),
		"warnings": first.warnings,
	}
	if len(others) > 0 {
		out["compare"] = others
	}
	WriteJSON(w, http.StatusAccepted, out)
}

// chatReplyStart is one reply that was started.
type chatReplyStart struct {
	runID    string
	msg      *store.ChatMessage
	view     chatMessageView
	warnings []string
}

// chatStartReply builds the request for one model answering userMsgID and
// starts its run (server-owned, detached from the request).
func (s *Server) chatStartReply(r *http.Request, user *store.User, token *store.Token, c *store.ChatConversation, model, userMsgID string,
	byID map[string]*store.ChatMessage, branchAtts map[string][]*store.ChatAttachment, research bool, question, group string) (*chatReplyStart, error) {
	ctx := r.Context()
	// Fit the branch into the model's context window: the oldest turns go
	// first, the new question always stays. The count is shown in the thread.
	build := &chatHistoryBuild{s: s, vision: s.chatVision(ctx, model), model: model, user: user}
	wire := s.chatHistory(ctx, build, byID, branchAtts, userMsgID)
	var tools []string
	var imageModel string
	var assist chatAssist
	imageRun := s.chatImageModel(ctx, model)
	if !imageRun {
		tools = s.chatToolsFor(ctx, c, model)
		if slices.Contains(tools, store.ChatToolImage) {
			if imageModel = s.chatImageModelFor(ctx, user, c); imageModel == "" {
				tools = slices.DeleteFunc(tools, func(t string) bool { return t == store.ChatToolImage })
			}
		}
		assist = s.chatAssistFor(ctx, user, c, model, research)
		if group != "" {
			assist.Ask = false // Compare: several models would each stop to ask
		}
	}
	sys := s.chatSystemPrompt(r, c)
	if assist.Memory {
		if mem := s.chatMemoryPrompt(ctx, user.ID); mem != "" {
			sys = strings.TrimSpace(sys + "\n\n" + mem)
		}
	}
	// (Janus's own page reader needs no note: its tool description says
	// when to use it, and a chat with no other tools keeps its prompt.)
	if note := chatToolsNote(tools); note != "" {
		// Tell the model it can search here, so it does instead of
		// answering "I have no live data".
		sys = strings.TrimSpace(sys + "\n\n" + note)
	}
	if research {
		sys = strings.TrimSpace(sys + "\n\n" + chatResearchPrompt(assist.ReadPage))
	}
	if sys != "" {
		wire = append([]chatWireMessage{{Role: "system", Content: sys}}, wire...)
	}
	history, dropped := trimChatHistory(wire, s.chatContextWindow(ctx, model))
	build.inlineImages(ctx, history)
	if !imageRun {
		assist.Files = chatReadableFiles(build.files)
	}

	requestID := store.NewID()
	am := &store.ChatMessage{ConversationID: c.ID, ParentID: userMsgID, Role: store.ChatRoleAssistant,
		ModelName: model, TeamID: c.TeamID, RequestID: requestID, Status: store.ChatStatusStreaming, ContextDropped: dropped}
	if err := s.Store.InsertChatMessage(ctx, am); err != nil {
		return nil, err
	}
	if group != "" {
		if err := s.Store.SetChatMessageCompareGroup(ctx, am.ID, group); err != nil {
			return nil, err
		}
	}
	run := &store.ChatRun{MessageID: am.ID, ConversationID: c.ID, UserID: user.ID, Owner: s.chatRT().host, Status: store.ChatStatusStreaming}
	if err := s.Store.InsertChatRun(ctx, run); err != nil {
		return nil, err
	}
	// Every call this reply makes carries the browser's address and
	// headers, the conversation (auto-routing stickiness) and this
	// message (the footer sums them; only the first counts as a request).
	runCtx, cancel := context.WithCancel(withChatOrigin(context.WithoutCancel(ctx), s.chatOriginFor(r, c.ID, am.ID, requestID)))
	live := &chatLiveRun{status: store.ChatStatusStreaming, cancel: cancel, msg: am}
	// The reply view is taken before the run goroutine starts: the run
	// writes am's error fields when it fails, and a fast failure would race
	// a view taken afterwards.
	view := s.chatView(am)
	rt := s.chatRT()
	rt.mu.Lock()
	rt.runs[run.ID] = live
	rt.mu.Unlock()
	if imageRun {
		// An image model answers the message itself (no history: image
		// endpoints take one description).
		go s.runChatImage(runCtx, run, live, user, token, model, question, requestID)
	} else {
		go s.runChat(runCtx, run, live, user, token, chatRunPlan{Model: model, Effort: c.ReasoningEffort, Tools: tools,
			ImageModel: imageModel, Assist: assist, Research: research, History: history, RequestID: requestID, Question: question})
	}
	view.CompareGroup = group
	warnings := build.warnings
	if warnings == nil {
		warnings = []string{}
	}
	return &chatReplyStart{runID: run.ID, msg: am, view: view, warnings: warnings}, nil
}

func chatTitle(first string) string {
	t := strings.Join(strings.Fields(first), " ")
	if r := []rune(t); len(r) > 60 {
		t = string(r[:60]) + "…"
	}
	if t == "" {
		t = "New chat"
	}
	return t
}

// chatHistory walks from leaf to root and returns the OpenAI messages in
// order. User messages with files become content-part arrays (see
// chatHistoryBuild.message); images are inlined after trimming.
func (s *Server) chatHistory(ctx context.Context, build *chatHistoryBuild, byID map[string]*store.ChatMessage,
	atts map[string][]*store.ChatAttachment, leaf string) []chatWireMessage {
	var chain []*store.ChatMessage
	for id := leaf; id != ""; {
		m := byID[id]
		if m == nil {
			break
		}
		chain = append(chain, m)
		id = m.ParentID
	}
	out := make([]chatWireMessage, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		m := chain[i]
		if m.Role == store.ChatRoleUser && i > 0 && chain[i-1].Role == store.ChatRoleAssistant && chain[i-1].Status == store.ChatStatusBlocked {
			// A message whose reply was blocked is not context either:
			// resending it would have it judged (and refused) again as
			// part of every later turn, and the user could never move on.
			continue
		}
		text := s.chatDecrypt(m.ContentEnc)
		build.files = append(build.files, atts[m.ID]...)
		if m.Role == store.ChatRoleAssistant && (m.Status == store.ChatStatusComplete || m.Status == store.ChatStatusStopped) && len(atts[m.ID]) > 0 {
			// What an earlier reply made: its text, then each file by
			// its real kind. Text files it handed over come back in full
			// (up to a budget) so the model can review or change them;
			// larger ones, and images and media, are named, and
			// read_file opens them.
			out = append(out, build.generated(ctx, m, text, atts[m.ID]))
			continue
		}
		if m.Role == store.ChatRoleAssistant && m.Status == store.ChatStatusComplete {
			if e := s.chatDecodeExtras(m.ExtrasEnc); e != nil && e.Question != nil {
				// The model asked the user something: keep the question
				// (and its choices) so the next message reads as the answer.
				out = append(out, chatWireMessage{Role: m.Role, Content: strings.TrimSpace(text + "\n\n" + chatQuestionNote(e.Question))})
				continue
			}
		}
		if m.Role == store.ChatRoleAssistant && (m.Status != store.ChatStatusComplete && m.Status != store.ChatStatusStopped || text == "") {
			continue // failed/empty replies are not context
		}
		if m.Role == store.ChatRoleUser {
			out = append(out, build.message(ctx, m.Role, text, atts[m.ID]))
			continue
		}
		out = append(out, chatWireMessage{Role: m.Role, Content: text})
	}
	return out
}

// --- the run -----------------------------------------------------------------

// chatSink is the in-process ResponseWriter handed to handleProxy. It parses
// the OpenAI SSE stream (or a buffered JSON/error reply) into the live run.
type chatSink struct {
	header http.Header
	status int
	buf    bytes.Buffer
	raw    bytes.Buffer // non-SSE body (errors, buffered replies)
	sse    bool
	live   *chatLiveRun
	errMsg string
	errCd  string
	// calls are the function tool calls the model made (by stream index).
	calls []chatToolCall
}

// chatToolCall is one function call the chat model asked Janus to run.
type chatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolCall accumulates one streamed tool_call delta.
func (k *chatSink) toolCall(index int, id, name, args string) {
	for len(k.calls) <= index {
		k.calls = append(k.calls, chatToolCall{})
	}
	c := &k.calls[index]
	if id != "" {
		c.ID = id
	}
	if name != "" {
		c.Name = name
	}
	c.Arguments += args
}

func (k *chatSink) Header() http.Header { return k.header }
func (k *chatSink) WriteHeader(code int) {
	if k.status == 0 {
		k.status = code
		k.sse = strings.Contains(k.header.Get("Content-Type"), "event-stream")
	}
}
func (k *chatSink) Flush() {}
func (k *chatSink) Write(p []byte) (int, error) {
	if k.status == 0 {
		k.WriteHeader(http.StatusOK)
	}
	if !k.sse && k.raw.Len() == 0 && k.status < 400 && bytes.HasPrefix(bytes.TrimLeft(p, " \r\n"), []byte("data:")) {
		// An event stream under the wrong Content-Type (an upstream that
		// labels SSE text/plain): read it as the stream it is, or the
		// reply is dropped and the user sees an empty answer.
		k.sse = true
	}
	if !k.sse {
		k.raw.Write(p)
		return len(p), nil
	}
	k.buf.Write(p)
	for {
		line, err := k.buf.ReadBytes('\n')
		if err != nil {
			// incomplete line: put it back
			rest := append(line, k.buf.Bytes()...)
			k.buf.Reset()
			k.buf.Write(rest)
			break
		}
		k.line(bytes.TrimSpace(line))
	}
	return len(p), nil
}

func (k *chatSink) line(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[5:])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var frame struct {
		Choices []struct {
			Delta struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
				Activity         json.RawMessage `json:"janus_activity"`
				Annotations      json.RawMessage `json:"annotations"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Error *APIError `json:"error"`
	}
	if json.Unmarshal(data, &frame) != nil {
		return
	}
	if frame.Error != nil {
		k.errCd, k.errMsg = frame.Error.Code, frame.Error.Message
		return
	}
	for _, ch := range frame.Choices {
		k.live.extras.feed(ch.Delta.Activity, ch.Delta.Annotations)
		for _, tc := range ch.Delta.ToolCalls {
			if tc.Index >= 0 && tc.Index < 16 {
				k.toolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
		}
	}
	k.live.mu.Lock()
	for _, ch := range frame.Choices {
		k.live.content.WriteString(ch.Delta.Content)
		k.live.reasoning.WriteString(ch.Delta.ReasoningContent)
		k.live.reasoning.WriteString(ch.Delta.Reasoning)
	}
	k.live.mu.Unlock()
}

// finishRaw interprets a non-streaming reply: an error envelope, or (from a
// model/adapter that ignored stream=true) a complete chat.completion.
func (k *chatSink) finishRaw() {
	if k.raw.Len() == 0 {
		return
	}
	var body struct {
		Error   *APIError `json:"error"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(k.raw.Bytes(), &body) != nil {
		if k.status >= 400 {
			k.errCd, k.errMsg = "upstream.error", strings.TrimSpace(k.raw.String())
		}
		return
	}
	if body.Error != nil {
		k.errCd, k.errMsg = body.Error.Code, body.Error.Message
		return
	}
	k.live.mu.Lock()
	for _, ch := range body.Choices {
		k.live.content.WriteString(ch.Message.Content)
		k.live.reasoning.WriteString(ch.Message.ReasoningContent)
	}
	k.live.mu.Unlock()
	for _, ch := range body.Choices {
		for i, tc := range ch.Message.ToolCalls {
			k.toolCall(i, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
	}
}

func (s *Server) runChat(ctx context.Context, run *store.ChatRun, live *chatLiveRun, user *store.User, token *store.Token, plan chatRunPlan) {
	model, effort, tools, history, requestID := plan.Model, plan.Effort, plan.Tools, plan.History, plan.RequestID
	rt := s.chatRT()
	defer func() {
		rt.mu.Lock()
		delete(rt.runs, run.ID)
		rt.mu.Unlock()
		live.cancel()
	}()
	// Janus runs the search itself when the model's provider has none.
	janusSearch := s.chatJanusSearch(ctx, model, tools)
	var hosted []string
	for _, t := range tools {
		if t == store.ChatToolImage || (t == store.ChatToolWebSearch && janusSearch) {
			continue
		}
		hosted = append(hosted, t)
	}
	box := &chatToolbox{s: s, run: run, live: live, user: user, token: token, plan: plan, search: janusSearch,
		image: plan.ImageModel != "" && slices.Contains(tools, store.ChatToolImage), maxSearch: chatMaxSearchRounds}
	box.judge = s.chatToolJudgeFor(ctx, user, plan, live)
	// Sound and video this model cannot take become text first.
	box.convertMedia(ctx, history)
	box.judge.judgeMediaText(ctx, history)
	maxRounds := chatMaxToolRounds
	if plan.Research {
		box.maxSearch, maxRounds = chatResearchSearchRounds, chatResearchToolRounds
	}
	toolRound := false
	request := func(messages []chatWireMessage, id string) (*chatSink, bool) {
		body := map[string]any{
			"model": model, "messages": messages, "stream": true,
			"stream_options": map[string]any{"include_usage": true},
		}
		if effort != "" {
			// The proxy fits this to what the model takes (strips or
			// clamps it) and records any change on the request log.
			body["reasoning_effort"] = effort
		}
		list := append(chatToolsBody(hosted), box.defs()...)
		if len(list) > 0 {
			// Provider-run tools make the proxy serve the request through
			// the provider's Responses API, which searches and cites.
			body["tools"] = list
		}
		payload, _ := json.Marshal(body)
		// Tool output was judged above when each tool made it.
		// The reply's first call (and a no-tools retry, which is the
		// answer) is janus-chat; later rounds name themselves.
		agent := chatClientApp
		if id != requestID && toolRound {
			agent = chatToolClientApp
			if plan.Research {
				agent = chatResearchClientApp
			}
		}
		req, err := chatProxyRequest(chatToolOutputJudged(ctx), user, token, id, agent, payload)
		if err != nil {
			s.chatFail(run, live, "chat.internal", "The reply could not be started.")
			return nil, false
		}
		sink := &chatSink{header: http.Header{}, live: live}
		s.chatDriveProxy(run, live, sink, req, id)
		sink.finishRaw()
		return sink, true
	}
	sink, ok := request(history, requestID)
	if !ok {
		return
	}
	if box.offersFunctions() && (sink.status == http.StatusBadRequest || sink.status == http.StatusUnprocessableEntity) &&
		strings.Contains(strings.ToLower(sink.errMsg), "tool") {
		// A model that takes no function tools: answer without Janus's
		// tools rather than fail the message.
		live.mu.Lock()
		live.content.Reset()
		live.reasoning.Reset()
		live.mu.Unlock()
		box.functionsOff = true
		if sink, ok = request(history, store.NewID()); !ok {
			return
		}
	}
	// Janus-run tools: answer each call the model made, then ask again (at
	// most maxRounds rounds; the last round's results say to answer).
	convo := append([]chatWireMessage{}, history...)
	for rounds := 0; ctx.Err() == nil && sink.errCd == "" && rounds < maxRounds; {
		calls := box.ours(sink.calls)
		if len(calls) == 0 {
			break
		}
		rounds++
		turn, results := box.round(ctx, calls)
		if rounds == maxRounds && len(results) > 0 {
			appendToolNote(&results[len(results)-1], chatToolsLimitNote)
			box.functionsOff = true
		}
		live.mu.Lock()
		said := live.content.String()
		if said != "" {
			turn.Content = said
			if plan.Research {
				// Deep research: what the model says between rounds ("let
				// me check…") is progress, not the report. Keep it with the
				// thinking; the reply is the final report only.
				live.reasoning.WriteString(strings.TrimSpace(said) + "\n\n")
				live.content.Reset()
			} else if !strings.HasSuffix(said, "\n\n") {
				live.content.WriteString("\n\n")
			}
		}
		live.mu.Unlock()
		convo = append(append(convo, turn), results...)
		if box.asked {
			// The model asked the user something: the reply ends with
			// the question card; their answer is the next message.
			break
		}
		toolRound = true
		more, ok2 := request(convo, store.NewID())
		if !ok2 {
			return
		}
		sink = more
	}

	status := store.ChatStatusComplete
	switch {
	case ctx.Err() != nil:
		status = store.ChatStatusStopped
	case sink.errCd == CodeSecurityBlocked:
		// A policy refused the message or stopped the reply: a clear,
		// calm terminal state with the gateway's notice, never "error".
		status = store.ChatStatusBlocked
	case sink.errCd != "" || sink.status >= 400:
		status = store.ChatStatusError
	}
	if status == store.ChatStatusComplete {
		// A "successful" reply with no text at all is a failure the user
		// would otherwise only see as silence: name it. (A reply that only
		// made images has its pictures; that is an answer.)
		if c, r, _ := live.snapshot(); strings.TrimSpace(c) == "" && strings.TrimSpace(r) == "" && !live.madeSomething() && !box.asked {
			status = store.ChatStatusError
			sink.errCd = "upstream.empty_reply"
			sink.errMsg = "The model finished without sending any text. Try again, or pick another model."
			s.Logger.Warn("chat reply was empty", "request_id", requestID, "model", model, "upstream_status", sink.status,
				"content_type", sink.header.Get("Content-Type"))
		}
	}
	live.mu.Lock()
	live.status = status
	live.mu.Unlock()
	content, reasoning, _ := live.snapshot()
	m := live.msg
	if status == store.ChatStatusBlocked {
		m.ErrorCode, m.ErrorMessage = sink.errCd, sink.errMsg
	}
	if status == store.ChatStatusError {
		m.ErrorCode, m.ErrorMessage = sink.errCd, sink.errMsg
		if m.ErrorMessage == "" {
			m.ErrorMessage = "The model did not answer."
		}
		s.pending.Add(1)
		go func(code, msg string) {
			defer s.pending.Done()
			s.alertChatFailure(model, code, msg)
		}(m.ErrorCode, m.ErrorMessage)
	}
	s.chatAttachUsage(ctx, m, requestID)
	s.chatPersist(live, content, reasoning, status)
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Store.FinishChatRun(bg, run.ID, status)
	// Only finished replies are searchable; streaming flushes never index.
	if status == store.ChatStatusComplete || status == store.ChatStatusStopped {
		s.chatIndex(bg, user.ID, run.ConversationID, m.ID, content)
	}
	if status == store.ChatStatusComplete && plan.Research && strings.TrimSpace(content) != "" {
		s.chatSaveResearchReport(bg, user, run, live, plan.Question, content)
	}
	if status == store.ChatStatusComplete {
		s.chatMaybeAutoTitle(chatOriginFrom(ctx), run, user)
	}
}

// chatDriveProxy runs the in-process proxy call for a run and keeps the run
// alive while it works: the partial reply is flushed, the run heartbeats
// (so the reaper on any replica knows it is alive), and a stop requested
// from any replica cancels it.
func (s *Server) chatDriveProxy(run *store.ChatRun, live *chatLiveRun, sink *chatSink, req *http.Request, requestID string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				s.Logger.Error("chat run panicked", "panic", p, "request_id", requestID)
			}
		}()
		s.handleProxy(sink, req)
	}()
	tick := time.NewTicker(chatFlushEvery)
	defer tick.Stop()
	var lastLen int
	var seq int64
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			content, reasoning, _ := live.snapshot()
			if n := len(content) + len(reasoning); n != lastLen {
				lastLen = n
				s.chatPersist(live, content, reasoning, store.ChatStatusStreaming)
			}
			seq++
			bg, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			stop, err := s.Store.HeartbeatChatRun(bg, run.ID, seq)
			cancel()
			if err == nil && stop {
				live.cancel()
			}
		}
	}
}

// chatAttachUsage copies the metering the proxy wrote (asynchronously) for
// this reply onto the message, so the footer shows what the request log and
// quotas recorded: tokens and cost are the sum over every call the reply
// made (tool and research rounds, retries, images, video, music, media
// conversion); the served model, speed and latency are the first call's.
func (s *Server) chatAttachUsage(ctx context.Context, m *store.ChatMessage, requestID string) {
	want := 1
	if o := chatOriginFrom(ctx); o != nil {
		want = max(int(o.calls.Load()), 1)
	}
	deadline := time.Now().Add(chatUsageWaitTimeout)
	var first *store.UsageEvent
	var tot store.ChatUsageTotals
	for {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if first == nil {
			if e, err := s.Store.UsageEventByRequestID(bg, requestID); err == nil {
				first = e
			}
		}
		if t, err := s.Store.ChatMessageUsage(bg, m.ID); err == nil {
			tot = t
		}
		cancel()
		if first != nil && tot.Events >= want || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if first == nil {
		s.Logger.Warn("chat message has no usage event yet", "request_id", requestID)
		return
	}
	m.TokensIn, m.TokensOut, m.TokensCached, m.CostNano = first.TokensIn, first.TokensOut, first.TokensCached, first.CostNano
	if tot.Events > 0 {
		m.TokensIn, m.TokensOut, m.TokensCached, m.CostNano = tot.TokensIn, tot.TokensOut, tot.TokensCached, tot.CostNano
	}
	m.TokensOutPerSecond, m.LatencyMs, m.TTFBMs = first.TokensOutPerSecond, first.LatencyMs, first.TTFBMs
	m.ServedModel = first.ModelName
	if m.ErrorCode == "" && first.ErrorCode != "" && first.HTTPStatus >= 400 {
		m.ErrorCode = first.ErrorCode
	}
}

func (s *Server) chatPersist(live *chatLiveRun, content, reasoning, status string) {
	m := live.msg
	var err error
	if m.ContentEnc, err = s.chatEncrypt(content); err != nil {
		s.Logger.Error("encrypt chat message", "error", err.Error())
		return
	}
	if m.ReasoningEnc, err = s.chatEncrypt(reasoning); err != nil {
		s.Logger.Error("encrypt chat reasoning", "error", err.Error())
		return
	}
	m.Status = status
	if enc := s.chatEncodeExtras(&live.extras); enc != "" {
		m.ExtrasEnc = enc
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.UpdateChatMessage(bg, m); err != nil {
		s.Logger.Warn("persist chat message", "error", err.Error(), "message_id", m.ID)
	}
}

func (s *Server) chatFail(run *store.ChatRun, live *chatLiveRun, code, msg string) {
	live.msg.ErrorCode, live.msg.ErrorMessage = code, msg
	s.chatPersist(live, "", "", store.ChatStatusError)
	_ = s.Store.FinishChatRun(context.Background(), run.ID, store.ChatStatusError)
}

// --- events ------------------------------------------------------------------

// handleChatEvents streams a run as SSE: "delta" frames carry appended text
// with UTF-8 byte offsets (content_to is what to send back as content_from
// on reconnect, so a resume never repeats or skips text), then one "done" frame with
// the final message. It works on any replica: the owning pod reads the live
// buffer, every other pod reads the flushed copy from the database.
func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	run, err := s.Store.ChatRun(ctx, user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	var sentC, sentR int
	if v := r.URL.Query().Get("content_from"); v != "" {
		sentC = atoiDefault(v, 0)
	}
	if v := r.URL.Query().Get("reasoning_from"); v != "" {
		sentR = atoiDefault(v, 0)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(event string, v any) bool {
		b, _ := json.Marshal(v)
		if _, err := w.Write([]byte("event: " + event + "\ndata: " + string(b) + "\n\n")); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	rt := s.chatRT()
	keepalive := time.Now()
	var sentExtras string
	for {
		rt.mu.Lock()
		live := rt.runs[run.ID]
		rt.mu.Unlock()
		// Searches in progress and sources found so far, as they change
		// (from the live run on this replica, the flushed row otherwise).
		var extras *chatExtras
		if live != nil {
			extras = live.extras.snapshot()
		}
		var content, reasoning, status string
		poll := chatRemotePollEvery
		if live != nil {
			content, reasoning, status = live.snapshot()
			poll = chatLocalPollEvery
		} else {
			m, err := s.Store.ChatMessage(ctx, run.MessageID)
			if err != nil {
				return
			}
			content, reasoning, status = s.chatDecrypt(m.ContentEnc), s.chatDecrypt(m.ReasoningEnc), m.Status
			extras = s.chatDecodeExtras(m.ExtrasEnc)
		}
		if extras != nil {
			if b, _ := json.Marshal(extras); string(b) != sentExtras {
				sentExtras = string(b)
				if !send("activity", extras) {
					return
				}
				keepalive = time.Now()
			}
		}
		if len(content) > sentC || len(reasoning) > sentR {
			d := map[string]any{}
			if len(content) > sentC {
				d["content"], d["content_from"], d["content_to"] = content[sentC:], sentC, len(content)
				sentC = len(content)
			}
			if len(reasoning) > sentR {
				d["reasoning"], d["reasoning_from"], d["reasoning_to"] = reasoning[sentR:], sentR, len(reasoning)
				sentR = len(reasoning)
			}
			if !send("delta", d) {
				return
			}
			keepalive = time.Now()
		}
		if status != store.ChatStatusStreaming && (live == nil || status != "") {
			// The owning pod sets live.status before its final write;
			// read the persisted row so "done" carries the usage.
			if live != nil {
				time.Sleep(chatLocalPollEvery)
				rt.mu.Lock()
				still := rt.runs[run.ID] != nil
				rt.mu.Unlock()
				if still {
					continue
				}
			}
			m, err := s.Store.ChatMessage(ctx, run.MessageID)
			if err != nil {
				return
			}
			atts, _ := s.Store.ChatAttachmentsForMessages(ctx, []string{m.ID})
			done := []chatMessageView{s.chatViewWith(m, atts)}
			// A reply made side by side keeps its group, or the page drops
			// it out of the comparison the moment it finishes.
			if groups, err := s.Store.ChatCompareGroups(ctx, m.ConversationID); err == nil {
				done[0].CompareGroup = groups[m.ID]
			}
			s.chatAttachNotes(r.Context(), done)
			send("done", done[0])
			return
		}
		if time.Since(keepalive) > 15*time.Second {
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			keepalive = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

func (s *Server) handleChatStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFrom(ctx)
	id := chi.URLParam(r, "id")
	if _, err := s.Store.ChatRun(ctx, user.ID, id); err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	if err := s.Store.RequestChatRunCancel(ctx, user.ID, id); err != nil {
		WriteError(w, r, err)
		return
	}
	rt := s.chatRT()
	rt.mu.Lock()
	if live := rt.runs[id]; live != nil {
		live.cancel()
	}
	rt.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func atoiDefault(s string, d int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return d
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return d
		}
	}
	return n
}
