package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
)

// Janus Chat export: the conversation's current branch (what the thread
// shows) as Markdown or JSON, decrypted for its owner. Attachments are
// listed by name; their bytes are not embedded. Failed and empty replies
// are left out, like they are from the model's context.

type chatExportMessage struct {
	Role        string   `json:"role"`
	Content     string   `json:"content"`
	Reasoning   string   `json:"reasoning,omitempty"`
	Model       string   `json:"model,omitempty"`
	Status      string   `json:"status,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
	TokensIn    int64    `json:"tokens_in,omitempty"`
	TokensOut   int64    `json:"tokens_out,omitempty"`
	CostUSD     float64  `json:"cost_usd,omitempty"`
	CreatedAt   string   `json:"created_at"`
}

type chatExport struct {
	Format       string              `json:"format"`
	Title        string              `json:"title"`
	Model        string              `json:"model"`
	Instructions string              `json:"instructions,omitempty"`
	ExportedAt   string              `json:"exported_at"`
	CostUSD      float64             `json:"cost_usd,omitempty"`
	Messages     []chatExportMessage `json:"messages"`
}

var chatFilenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9 ._-]+`)

func chatExportFilename(title, ext string) string {
	name := strings.TrimSpace(chatFilenameUnsafe.ReplaceAllString(title, ""))
	name = strings.Join(strings.Fields(name), "-")
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "chat"
	}
	return name + "." + ext
}

func (s *Server) handleChatExport(w http.ResponseWriter, r *http.Request) {
	if !s.chatAdmin(r.Context()).ExportEnabled {
		WriteError(w, r, ErrForbidden("Export is turned off for Janus Chat on this instance."))
		return
	}
	ctx := r.Context()
	user := UserFrom(ctx)
	c, err := s.Store.ChatConversation(ctx, user.ID, chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, chatNotFound(err))
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "json" {
		WriteError(w, r, ErrInvalidRequest("format must be markdown or json").WithParam("format"))
		return
	}
	out, err := s.chatBuildExport(ctx, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	localOnly := s.Config != nil && s.Config.LocalOnly
	if localOnly {
		// Local-only mode tracks no cost: leave it out, as the page does.
		out.CostUSD = 0
		for i := range out.Messages {
			out.Messages[i].CostUSD = 0
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	if format == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+chatExportFilename(out.Title, "json")+`"`)
		WriteJSON(w, http.StatusOK, out)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+chatExportFilename(out.Title, "md")+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(chatExportMarkdown(out, !localOnly)))
}

// chatBuildExport collects the current branch of c, decrypted.
func (s *Server) chatBuildExport(ctx context.Context, c *store.ChatConversation) (chatExport, error) {
	out := chatExport{Format: "janus-chat-v1", Title: c.Title, Model: c.ModelName,
		Instructions: s.chatDecrypt(c.InstructionsEnc), ExportedAt: time.Now().UTC().Format(time.RFC3339), Messages: []chatExportMessage{}}
	if out.Title == "" {
		out.Title = "Chat"
	}
	msgs, err := s.Store.ListChatMessages(ctx, c.ID)
	if err != nil {
		return out, err
	}
	atts, err := s.chatAttachmentsByMessage(ctx, msgs)
	if err != nil {
		return out, err
	}
	byID := map[string]*store.ChatMessage{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	var chain []*store.ChatMessage
	for id := c.CurrentLeafID; id != ""; {
		m := byID[id]
		if m == nil {
			break
		}
		chain = append(chain, m)
		id = m.ParentID
	}
	for i := len(chain) - 1; i >= 0; i-- {
		m := chain[i]
		text := s.chatDecrypt(m.ContentEnc)
		if m.Role == store.ChatRoleAssistant && strings.TrimSpace(text) == "" {
			continue
		}
		em := chatExportMessage{Role: m.Role, Content: text, CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339)}
		if m.Role == store.ChatRoleAssistant {
			em.Reasoning = s.chatDecrypt(m.ReasoningEnc)
			em.Model = m.ServedModel
			if em.Model == "" {
				em.Model = m.ModelName
			}
			if m.Status != store.ChatStatusComplete {
				em.Status = m.Status
			}
			em.TokensIn, em.TokensOut, em.CostUSD = m.TokensIn, m.TokensOut, float64(m.CostNano)/1e9
			out.CostUSD += em.CostUSD
		}
		for _, a := range atts[m.ID] {
			em.Attachments = append(em.Attachments, s.chatDecrypt(a.FilenameEnc))
		}
		out.Messages = append(out.Messages, em)
	}
	return out, nil
}

// chatExportMarkdown renders the export as a readable document: a heading
// per turn, reasoning folded into a <details> block, model and cost under
// each reply.
// withCost false (local-only mode, where Janus tracks no cost) leaves every
// cost out.
func chatExportMarkdown(e chatExport, withCost bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", e.Title)
	fmt.Fprintf(&b, "_Exported from Janus Chat on %s", e.ExportedAt[:10])
	if withCost && e.CostUSD > 0 {
		fmt.Fprintf(&b, " · total cost $%.5f", e.CostUSD)
	}
	b.WriteString("_\n\n")
	if e.Instructions != "" {
		b.WriteString("> **Instructions for this chat**\n")
		for _, line := range strings.Split(e.Instructions, "\n") {
			b.WriteString("> " + line + "\n")
		}
		b.WriteString("\n")
	}
	for _, m := range e.Messages {
		b.WriteString("---\n\n")
		if m.Role == store.ChatRoleUser {
			b.WriteString("### You\n\n")
		} else {
			fmt.Fprintf(&b, "### %s\n\n", firstNonEmpty(m.Model, "Assistant"))
		}
		for _, a := range m.Attachments {
			fmt.Fprintf(&b, "📎 %s\n\n", a)
		}
		if m.Reasoning != "" {
			b.WriteString("<details><summary>Thinking</summary>\n\n" + m.Reasoning + "\n\n</details>\n\n")
		}
		b.WriteString(strings.TrimRight(m.Content, "\n") + "\n\n")
		if m.Status != "" {
			fmt.Fprintf(&b, "_(%s)_\n\n", m.Status)
		}
		if m.Role == store.ChatRoleAssistant && withCost && (m.TokensIn > 0 || m.CostUSD > 0) {
			fmt.Fprintf(&b, "<sub>%d in / %d out · $%.5f</sub>\n\n", m.TokensIn, m.TokensOut, m.CostUSD)
		} else if m.Role == store.ChatRoleAssistant && m.TokensIn > 0 {
			fmt.Fprintf(&b, "<sub>%d in / %d out</sub>\n\n", m.TokensIn, m.TokensOut)
		}
	}
	return b.String()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
