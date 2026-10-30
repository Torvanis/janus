package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// Janus Chat settings: per-conversation (pin, instructions, thinking
// effort, temporary) and the user's own preferences (instructions for
// every chat, default model). Instructions become the leading system
// message of every request; trimming never drops them.

// chatMaxInstructionChars bounds both instruction fields.
const chatMaxInstructionChars = 8000

// chatConversationView is a conversation as the API shows it: the stored
// fields plus the decrypted instructions.
type chatConversationView struct {
	*store.ChatConversation
	Instructions string `json:"instructions"`
}

func (s *Server) chatConvView(c *store.ChatConversation) chatConversationView {
	return chatConversationView{ChatConversation: c, Instructions: s.chatDecrypt(c.InstructionsEnc)}
}

func validChatEffort(v string) bool { return slices.Contains(store.ChatReasoningEfforts, v) }

// chatSystemPrompt is what goes first in a request: the organization's
// instructions (Admin → Chat), the user's own, then the conversation's.
// Empty = no system message.
func (s *Server) chatSystemPrompt(r *http.Request, c *store.ChatConversation) string {
	var parts []string
	// The organization's instructions lead, then the user's, then the chat's.
	if v := s.chatOrgInstructions(r.Context()); v != "" {
		parts = append(parts, v)
	}
	if st, err := s.Store.ChatUserSettings(r.Context(), c.UserID); err == nil {
		if v := strings.TrimSpace(s.chatDecrypt(st.InstructionsEnc)); v != "" {
			parts = append(parts, v)
		}
	}
	if v := strings.TrimSpace(s.chatDecrypt(c.InstructionsEnc)); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, "\n\n")
}

type chatSettingsBody struct {
	Instructions *string `json:"instructions"`
	DefaultModel *string `json:"default_model"`
	ImageModel   *string `json:"image_model"`
	VideoModel   *string `json:"video_model"`
	MusicModel   *string `json:"music_model"`
}

// chatSettingsView: the person's chat preferences. image/video/music_model
// are their own models for those functions ("" = automatic), used in every
// chat when they are still available to them.
type chatSettingsView struct {
	Instructions string `json:"instructions"`
	DefaultModel string `json:"default_model"`
	ImageModel   string `json:"image_model"`
	VideoModel   string `json:"video_model"`
	MusicModel   string `json:"music_model"`
}

func (s *Server) chatSettingsView(st *store.ChatUserSetting) chatSettingsView {
	return chatSettingsView{Instructions: s.chatDecrypt(st.InstructionsEnc), DefaultModel: st.DefaultModel,
		ImageModel: st.ImageModel, VideoModel: st.VideoModel, MusicModel: st.MusicModel}
}

// chatUserModel is the person's own model for a function (store.ChatTool*:
// image, video, music), "" when automatic or unreadable.
func (s *Server) chatUserModel(ctx context.Context, userID, kind string) string {
	st, err := s.Store.ChatUserSettings(ctx, userID)
	if err != nil {
		return ""
	}
	switch kind {
	case store.ChatToolImage:
		return st.ImageModel
	case store.ChatToolVideo:
		return st.VideoModel
	case store.ChatToolMusic:
		return st.MusicModel
	}
	return ""
}

func (s *Server) handleChatSettingsGet(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	st, err := s.Store.ChatUserSettings(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.chatSettingsView(st))
}

func (s *Server) handleChatSettingsPut(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	var body chatSettingsBody
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	st, err := s.Store.ChatUserSettings(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Instructions != nil {
		v := strings.TrimSpace(*body.Instructions)
		if len(v) > chatMaxInstructionChars {
			WriteError(w, r, ErrInvalidRequest("Instructions can be at most 8,000 characters").WithParam("instructions"))
			return
		}
		if st.InstructionsEnc, err = s.chatEncrypt(v); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if body.DefaultModel != nil {
		st.DefaultModel = strings.TrimSpace(*body.DefaultModel)
	}
	for _, f := range []struct {
		in    *string
		out   *string
		param string
	}{{body.ImageModel, &st.ImageModel, "image_model"}, {body.VideoModel, &st.VideoModel, "video_model"}, {body.MusicModel, &st.MusicModel, "music_model"}} {
		if f.in == nil {
			continue
		}
		v := strings.TrimSpace(*f.in)
		if len(v) > 200 {
			WriteError(w, r, ErrInvalidRequest("That model name is too long.").WithParam(f.param))
			return
		}
		*f.out = v
	}
	if err := s.Store.SaveChatUserSettings(r.Context(), st); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.chatSettingsView(st))
}

// chatSweepTemporary purges temporary conversations idle since before
// now-ChatTemporaryTTL (files first, then rows). Returns how many went.
func (s *Server) chatSweepTemporary(ctx context.Context, now time.Time) int {
	stale, err := s.Store.StaleTemporaryChats(ctx, now.Add(-s.chatTemporaryTTL(ctx)), chatSweepBatch)
	if err != nil {
		s.Logger.Warn("sweep temporary chats", "error", err.Error())
		return 0
	}
	n := 0
	for _, c := range stale {
		if atts, err := s.Store.ChatAttachmentsForConversation(ctx, c.UserID, c.ID); err == nil {
			s.chatDeleteAttachments(ctx, atts)
		}
		if err := s.Store.PurgeChatConversation(ctx, c.UserID, c.ID); err != nil {
			s.Logger.Warn("purge temporary chat", "error", err.Error(), "conversation_id", c.ID)
			continue
		}
		n++
	}
	if n > 0 {
		s.Logger.Info("purged temporary chats", "count", n)
	}
	return n
}
