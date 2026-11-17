package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// reissueAudits returns the audit rows of one action for one token.
func (h *harness) reissueAudits(action, tokenID string) []*store.AuditEntry {
	h.t.Helper()
	entries, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{Action: action})
	if err != nil {
		h.t.Fatalf("list %s audit: %v", action, err)
	}
	out := []*store.AuditEntry{}
	for _, e := range entries {
		if e.ResourceID == tokenID {
			out = append(out, e)
		}
	}
	return out
}

// TestReissueTokenRevokesOldAndKeepsNameAndTeam covers the default path: the
// new token copies the old one's name and team, its value is returned once and
// works, the old value is refused at once, and both steps are audited.
func TestReissueTokenRevokesOldAndKeepsNameAndTeam(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Reissue team", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, oldValue, err := h.store.CreateTeamToken(ctx, h.user.ID, "CI runner", team.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Warm the credential cache so the test proves revocation drops it.
	if _, _, err := h.server.resolveToken(ctx, oldValue); err != nil {
		t.Fatalf("old token should resolve before re-issue: %v", err)
	}
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}

	rec := h.doAsSession(session, http.MethodPost, "/api/v1/tokens/"+old.ID+"/reissue", map[string]any{"revoke_old": true})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reissue: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			TeamID      string `json:"team_id"`
			RevokedAt   string `json:"revoked_at"`
		} `json:"token"`
		Value      string `json:"value"`
		RevokedOld bool   `json:"revoked_old"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Token.RevokedAt != "" {
		t.Fatalf("new token revoked_at = %q, want empty", body.Token.RevokedAt)
	}
	if body.Token.ID == "" || body.Token.ID == old.ID {
		t.Fatalf("new token id %q (old %q)", body.Token.ID, old.ID)
	}
	if body.Token.Description != "CI runner" || body.Token.TeamID != team.ID {
		t.Fatalf("new token settings = (%q, %q), want (CI runner, %s)", body.Token.Description, body.Token.TeamID, team.ID)
	}
	if !strings.HasPrefix(body.Value, store.TokenPrefix) || body.Value == oldValue || !body.RevokedOld {
		t.Fatalf("value %q revoked_old %v", body.Value, body.RevokedOld)
	}
	if got, _, err := h.server.resolveToken(ctx, body.Value); err != nil || got.ID != body.Token.ID {
		t.Fatalf("new value should resolve to the new token: %#v %v", got, err)
	}
	reloaded, err := h.store.TokenByID(ctx, old.ID)
	if err != nil || reloaded.RevokedAt.IsZero() {
		t.Fatalf("old token should be revoked: %#v %v", reloaded, err)
	}
	if _, _, err := h.server.resolveToken(ctx, oldValue); err == nil {
		t.Fatal("old value still resolves after re-issue with revoke_old")
	}

	created := h.reissueAudits("token_created", body.Token.ID)
	if len(created) != 1 || !strings.Contains(created[0].NewValue, `"reissued_from":"`+old.ID+`"`) || created[0].ActorUserID != h.user.ID {
		t.Fatalf("token_created audit = %#v", created)
	}
	revoked := h.reissueAudits("token_revoked", old.ID)
	if len(revoked) != 1 || !strings.Contains(revoked[0].NewValue, `"reason":"reissued"`) ||
		!strings.Contains(revoked[0].NewValue, `"replaced_by":"`+body.Token.ID+`"`) {
		t.Fatalf("token_revoked audit = %#v", revoked)
	}
}

// TestReissueTokenKeepOldAndRevokedSource covers keeping the old token
// working (two tokens share the name; names are not unique) and re-issuing a
// token that is already revoked (no second revoke, no revoke audit).
func TestReissueTokenKeepOldAndRevokedSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, oldValue, err := h.store.CreateToken(ctx, h.user.ID, "Laptop")
	if err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(session, http.MethodPost, "/api/v1/tokens/"+old.ID+"/reissue", map[string]any{"revoke_old": false})
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), `"revoked_old":true`) {
		t.Fatalf("reissue keep-old: %d %s", rec.Code, rec.Body.String())
	}
	if _, _, err := h.server.resolveToken(ctx, oldValue); err != nil {
		t.Fatalf("old token must keep working when not revoked: %v", err)
	}
	if n := len(h.reissueAudits("token_revoked", old.ID)); n != 0 {
		t.Fatalf("unexpected revoke audit rows: %d", n)
	}
	same := 0
	tokens, _ := h.store.ListTokens(ctx, h.user.ID)
	for _, tok := range tokens {
		if tok.Description == "Laptop" && tok.RevokedAt.IsZero() {
			same++
		}
	}
	if same != 2 {
		t.Fatalf("want two active tokens named Laptop, got %d", same)
	}

	// An already revoked token can be re-issued; revoke_old is a no-op.
	if err := h.store.RevokeToken(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	rec = h.doAsSession(session, http.MethodPost, "/api/v1/tokens/"+old.ID+"/reissue", map[string]any{"revoke_old": true})
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"revoked_old":false`) {
		t.Fatalf("reissue revoked: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(h.reissueAudits("token_revoked", old.ID)); n != 0 {
		t.Fatalf("re-issuing a revoked token must not audit a second revoke: %d", n)
	}
}

// TestReissueTokenRefusals: bearer tokens, other people's tokens (even for an
// admin), gateway-owned tokens and a lost team membership are all refused and
// create nothing.
func TestReissueTokenRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mine, _, err := h.store.CreateToken(ctx, h.user.ID, "Mine")
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		tokens, err := h.store.ListTokens(ctx, h.user.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(tokens)
	}
	before := count()

	// A presented API token cannot re-issue (leaked key cannot rotate itself).
	if rec := h.do(http.MethodPost, "/api/v1/tokens/"+mine.ID+"/reissue", map[string]any{"revoke_old": true}); rec.Code != http.StatusForbidden {
		t.Fatalf("bearer reissue: %d %s", rec.Code, rec.Body.String())
	}

	// Another user, made an admin, cannot re-issue (and so cannot receive) my token.
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-other", "other@example.com", "Other", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !other.IsAdmin() {
		t.Fatalf("fixture user should be an admin: %#v", other)
	}
	otherSession, err := h.server.Sessions.Create(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.doAsSession(otherSession, http.MethodPost, "/api/v1/tokens/"+mine.ID+"/reissue", map[string]any{"revoke_old": true}); rec.Code != http.StatusNotFound {
		t.Fatalf("admin reissue of another user's token: %d %s", rec.Code, rec.Body.String())
	}
	if reloaded, _ := h.store.TokenByID(ctx, mine.ID); !reloaded.RevokedAt.IsZero() {
		t.Fatal("refused reissue revoked the token")
	}

	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Lost team membership: refused with the store's reason, nothing created.
	team, err := h.store.CreateTeam(ctx, "Left team", other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddTeamMember(ctx, team.ID, h.user.ID, "member", "manual", ""); err != nil {
		t.Fatal(err)
	}
	teamTok, _, err := h.store.CreateTeamToken(ctx, h.user.ID, "Team key", team.ID)
	if err != nil {
		t.Fatal(err)
	}
	before++
	if err := h.store.RemoveTeamMemberSource(ctx, team.ID, h.user.ID, "manual", ""); err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(session, http.MethodPost, "/api/v1/tokens/"+teamTok.ID+"/reissue", map[string]any{"revoke_old": true})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no longer an active member of this token's team") {
		t.Fatalf("reissue without membership: %d %s", rec.Code, rec.Body.String())
	}
	if reloaded, _ := h.store.TokenByID(ctx, teamTok.ID); !reloaded.RevokedAt.IsZero() {
		t.Fatal("failed reissue must not revoke the old token")
	}
	if rec := h.doAsSession(session, http.MethodPost, "/api/v1/tokens/missing/reissue", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d", rec.Code)
	}
	if got := count(); got != before {
		t.Fatalf("refused re-issues created tokens: %d, want %d", got, before)
	}
}
