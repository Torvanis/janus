package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// TokenPrefix is the human-recognisable marker on every issued credential.
const TokenPrefix = "janus_"

// GenerateToken mints a new downstream credential and returns the plaintext
// value (shown exactly once) alongside its storage digest.
//
// Digest choice: tokens are 256 bits of CSPRNG output, so a plain SHA-256 digest
// is preimage-safe and — unlike a salted password hash — supports the indexed,
// sub-5ms equality lookup the proxy hot path requires.
func GenerateToken() (plaintext, digest, prefix string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("generate token: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	plaintext = TokenPrefix + body
	digest = HashToken(plaintext)
	prefix = plaintext[:len(TokenPrefix)+6]
	return plaintext, digest, prefix, nil
}

// HashToken computes the storage digest for a presented credential.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plaintext)))
	return hex.EncodeToString(sum[:])
}

const tokenColumns = `id, user_id, prefix, description, created_at, last_used_at, revoked_at, team_id, kind`

func scanToken(scan func(...any) error) (*Token, error) {
	var t Token
	var created, lastUsed, revoked string
	if err := scan(&t.ID, &t.UserID, &t.Prefix, &t.Description, &created, &lastUsed, &revoked, &t.TeamID, &t.Kind); err != nil {
		return nil, err
	}
	t.CreatedAt = ParseTime(created)
	t.LastUsedAt = ParseTime(lastUsed)
	t.RevokedAt = ParseTime(revoked)
	return &t, nil
}

// CreateToken persists a new credential for the user.
func (s *Store) CreateToken(ctx context.Context, userID, description string) (*Token, string, error) {
	return s.CreateTeamToken(ctx, userID, description, "")
}

// CreateTeamToken assigns a credential to an explicit personal or team context.
func (s *Store) CreateTeamToken(ctx context.Context, userID, description, teamID string) (*Token, string, error) {
	if teamID != "" {
		if err := s.ValidateTeamMember(ctx, teamID, userID); err != nil {
			return nil, "", err
		}
	}
	plaintext, digest, prefix, err := GenerateToken()
	if err != nil {
		return nil, "", err
	}
	t := &Token{ID: NewID(), UserID: userID, Prefix: prefix, Description: description, TeamID: teamID, CreatedAt: nowUTC()}
	if err := s.exec(ctx,
		`INSERT INTO api_token (id, user_id, token_digest, prefix, description, created_at, last_used_at, revoked_at, team_id)
		 VALUES (?,?,?,?,?,?,'','',?)`,
		t.ID, t.UserID, digest, t.Prefix, t.Description, FormatTime(t.CreatedAt), t.TeamID); err != nil {
		return nil, "", err
	}
	return t, plaintext, nil
}

// ListTokens returns the user's credentials, newest first. Hidden
// gateway-owned credentials (Janus Chat's per-team attribution tokens) are
// never listed: the user did not create them and cannot present them.
func (s *Store) ListTokens(ctx context.Context, userID string) ([]*Token, error) {
	rows, err := s.query(ctx, `SELECT `+tokenColumns+` FROM api_token WHERE user_id = ? AND kind = '' ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Token{}
	for rows.Next() {
		t, err := scanToken(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenByID loads one credential.
func (s *Store) TokenByID(ctx context.Context, id string) (*Token, error) {
	t, err := scanToken(s.queryRow(ctx, `SELECT `+tokenColumns+` FROM api_token WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}
	return t, nil
}

// TokenByDigest resolves a presented credential to its record. Revoked tokens
// are returned so the caller can distinguish "unknown" from "withdrawn".
func (s *Store) TokenByDigest(ctx context.Context, digest string) (*Token, error) {
	t, err := scanToken(s.queryRow(ctx, `SELECT `+tokenColumns+` FROM api_token WHERE token_digest = ?`, digest).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load token by digest: %w", err)
	}
	return t, nil
}

// RevokeToken withdraws a credential immediately.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE api_token SET revoked_at = ? WHERE id = ? AND revoked_at = ''`, FormatTime(nowUTC()), id)
}

// RevokeTokensForUser withdraws every credential a user holds.
func (s *Store) RevokeTokensForUser(ctx context.Context, userID string) error {
	return s.exec(ctx, `UPDATE api_token SET revoked_at = ? WHERE user_id = ? AND revoked_at = ''`, FormatTime(nowUTC()), userID)
}

// TouchToken records last use. It is called off the request's critical path.
func (s *Store) TouchToken(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE api_token SET last_used_at = ? WHERE id = ?`, FormatTime(nowUTC()), id)
}

// ReissueResult is what ReissueToken did: the new credential (with its
// plaintext, shown once), the token it replaces as it was before the call, and
// whether that old token was revoked by this call.
type ReissueResult struct {
	Token      *Token
	Plaintext  string
	Old        *Token
	RevokedOld bool
}

// ReissueToken mints a new credential with the same name and context as an
// existing one owned by ownerID, optionally revoking the old one in the same
// transaction so there is never a moment with neither or (when asked) both.
// A user token's settings are its description and team context; both are
// copied. Team membership is re-checked as on create. Gateway-owned tokens and
// other users' tokens are ErrNotFound, so callers cannot probe for them.
func (s *Store) ReissueToken(ctx context.Context, tokenID, ownerID string, revokeOld bool) (*ReissueResult, error) {
	var result *ReissueResult
	err := s.modelTx(ctx, func(tx *Store) error {
		// Same lock order as ChangeTokenTeam: membership first, then the
		// token, so a concurrent revoke or team change cannot interleave.
		if err := tx.lockTeamMembership(ctx); err != nil {
			return err
		}
		if err := tx.exec(ctx, `UPDATE api_token SET id=id WHERE id=?`, tokenID); err != nil {
			return err
		}
		old, err := tx.TokenByID(ctx, tokenID)
		if err != nil {
			return err
		}
		if old.UserID != ownerID || old.Kind != "" {
			return ErrNotFound
		}
		created, plaintext, err := tx.CreateTeamToken(ctx, ownerID, old.Description, old.TeamID)
		if err != nil {
			return err
		}
		revoked := false
		if revokeOld && old.RevokedAt.IsZero() {
			if err := tx.RevokeToken(ctx, old.ID); err != nil {
				return err
			}
			revoked = true
		}
		result = &ReissueResult{Token: created, Plaintext: plaintext, Old: old, RevokedOld: revoked}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
