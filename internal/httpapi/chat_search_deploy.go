package httpapi

import (
	"context"

	"github.com/torvanis/janus/internal/store"
)

// ApplyDeploymentChatSearch wires Chat web search to a SearXNG installed
// with this deployment (JANUS_CHAT_SEARCH_SEARXNG_URL).
//
//   - A new deployment address turns search on with it, once. An
//     administrator who then turns search off or picks another service is
//     not overridden on the next restart.
//   - A search service an administrator already uses (e.g. a paid Brave or
//     Tavily key) is never replaced: the bundled SearXNG is recorded and
//     offered in the console, but their choice stays in use.
//   - When the deployment no longer provides SearXNG and the settings still
//     point at the address it provided, search is turned off rather than
//     left calling a service that is gone.
//
// Replicas starting together write the same result, so the race is benign.
func (s *Server) ApplyDeploymentChatSearch(ctx context.Context) {
	if s.Config == nil || s.Store == nil {
		return
	}
	want := s.Config.ChatSearchSearXNGURL
	before, err := s.Store.ChatAdminSettings(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "chat search: read settings", "error", err.Error())
		return
	}
	next := before
	next.Search.Keys = before.Search.Keys
	switch {
	case want != "" && before.Search.Deployed != want:
		ownInUse := before.Search.Ready() && !(before.Search.Provider == store.SearchProviderSearXNG && before.Search.URL == before.Search.Deployed)
		if !ownInUse {
			next.Search.Enabled = true
			next.Search.Provider = store.SearchProviderSearXNG
			next.Search.URL = want
		}
		next.Search.Deployed = want
	case want == "" && before.Search.Deployed != "":
		if before.Search.Provider == store.SearchProviderSearXNG && before.Search.URL == before.Search.Deployed {
			next.Search.Enabled = false
			next.Search.URL = ""
		}
		next.Search.Deployed = ""
	default:
		return
	}
	if err := s.Store.SetChatAdminSettings(ctx, next); err != nil {
		s.Logger.WarnContext(ctx, "chat search: apply deployment SearXNG", "error", err.Error())
		return
	}
	s.invalidateChatAdmin()
	entry := &store.AuditEntry{
		ActorLabel: "deployment", Action: "chat_settings_updated", ResourceType: "chat_settings", ResourceID: store.SettingChat,
		OldValue: encodeAuditValue(auditChatSettings(before)), NewValue: encodeAuditValue(auditChatSettings(next)),
	}
	if err := s.Store.AppendAudit(ctx, entry); err != nil {
		s.Logger.WarnContext(ctx, "chat search: audit", "error", err.Error())
	}
	switch {
	case want != "" && next.Search.URL == want:
		s.Logger.InfoContext(ctx, "chat web search turned on with the deployment's SearXNG", "url", want)
	case want != "":
		s.Logger.InfoContext(ctx, "deployment SearXNG available; chat keeps the search service an administrator chose", "url", want, "provider", before.Search.Provider)
	default:
		s.Logger.InfoContext(ctx, "chat web search turned off: the deployment no longer provides SearXNG")
	}
}
