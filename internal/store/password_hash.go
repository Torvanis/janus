package store

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Password hashing.
//
// New hashes use PBKDF2-HMAC-SHA-512 (stdlib crypto/pbkdf2, a FIPS 140-3
// approved KDF that runs under GODEBUG=fips140=only) in a self-describing,
// PHC-style string:
//
//	$pbkdf2-sha512$i=<iterations>$<salt, base64 std no padding>$<key, base64 std no padding>
//
// The iteration count is 400,000: about twice the OWASP Password Storage
// Cheat Sheet minimum for PBKDF2-HMAC-SHA-512 (210,000), chosen so one
// verification costs about the same as the bcrypt cost-12 compare it replaces
// (~300 ms on a 2.1 GHz Xeon core). Keeping the costs level means sign-in
// latency does not change for users, and a wrong password against an account
// still on a legacy bcrypt hash takes as long as one against a migrated
// account or an unknown email, so timing cannot tell them apart while
// accounts migrate. The count is stored in every hash, so raising the
// constant later upgrades accounts at their next sign-in without a migration.
//
// Hashes written by earlier releases are bcrypt ("$2a$…", cost 12). They keep
// verifying; a successful sign-in rewrites them as PBKDF2 (see CheckPassword).
const (
	pbkdf2Scheme     = "pbkdf2-sha512"
	pbkdf2Iterations = 400_000
	pbkdf2SaltLen    = 16 // 128 bits, the FIPS 140-only minimum
	pbkdf2KeyLen     = 64 // full SHA-512 output
	// pbkdf2MaxIterations bounds the work a corrupted or hostile stored hash
	// can force on a sign-in request.
	pbkdf2MaxIterations = 10_000_000
)

var errUnknownHash = errors.New("unrecognized password hash format")

func pbkdf2Derive(pw string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha512.New, pw, salt, iter, pbkdf2KeyLen)
}

// hashPassword returns a new PBKDF2-HMAC-SHA-512 hash string for pw.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key, err := pbkdf2Derive(pw, salt, pbkdf2Iterations)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	enc := base64.RawStdEncoding
	return "$" + pbkdf2Scheme + "$i=" + strconv.Itoa(pbkdf2Iterations) + "$" + enc.EncodeToString(salt) + "$" + enc.EncodeToString(key), nil
}

// parsePBKDF2 splits a PBKDF2 hash string into its parameters.
func parsePBKDF2(h string) (iter int, salt, key []byte, err error) {
	parts := strings.Split(h, "$")
	// "", scheme, "i=N", salt, key
	if len(parts) != 5 || parts[0] != "" || parts[1] != pbkdf2Scheme || !strings.HasPrefix(parts[2], "i=") {
		return 0, nil, nil, errUnknownHash
	}
	iter, err = strconv.Atoi(strings.TrimPrefix(parts[2], "i="))
	if err != nil || iter < 1 || iter > pbkdf2MaxIterations {
		return 0, nil, nil, errUnknownHash
	}
	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(parts[3]); err != nil || len(salt) < pbkdf2SaltLen {
		return 0, nil, nil, errUnknownHash
	}
	if key, err = enc.DecodeString(parts[4]); err != nil || len(key) < 16 || len(key) > pbkdf2KeyLen {
		return 0, nil, nil, errUnknownHash
	}
	return iter, salt, key, nil
}

func isBcryptHash(h string) bool { return strings.HasPrefix(h, "$2") }

// verifyPassword checks pw against a stored hash of either format. ok is the
// verdict; needsRehash reports a correct password whose hash is legacy
// (bcrypt) or weaker than the current PBKDF2 parameters.
func verifyPassword(stored, pw string) (ok, needsRehash bool) {
	if isBcryptHash(stored) {
		if !legacyBcryptCompiled {
			// The FIPS edition does not compile bcrypt in (not an approved
			// algorithm). Spend a full verification so the answer takes as
			// long as for any other account, then refuse: an administrator
			// resets the password.
			burnPasswordTime(pw)
			return false, false
		}
		if !bcryptMatches(stored, pw) {
			return false, false
		}
		return true, true
	}
	iter, salt, key, err := parsePBKDF2(stored)
	if err != nil {
		return false, false
	}
	// Derive at the stored length so the comparison is always like for like.
	got, err := pbkdf2.Key(sha512.New, pw, salt, iter, len(key))
	if err != nil {
		return false, false
	}
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false
	}
	return true, iter < pbkdf2Iterations || len(key) < pbkdf2KeyLen || len(salt) < pbkdf2SaltLen
}

// dummyPasswordHash is a PBKDF2 hash of a random, never-disclosed password at
// the current parameters. Verifying against it costs the same as verifying a
// real account, so the response time of a sign-in for an unknown email (or a
// user without a local credential) does not reveal that the account is
// missing.
var dummyPasswordHash = sync.OnceValue(func() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("generate dummy password: %v", err))
	}
	h, err := hashPassword(base64.RawStdEncoding.EncodeToString(b))
	if err != nil {
		panic(fmt.Sprintf("build dummy password hash: %v", err))
	}
	return h
})

// burnPasswordTime performs one full-cost verification whose result is
// discarded (timing equalization for the unknown-account paths).
func burnPasswordTime(pw string) { _, _ = verifyPassword(dummyPasswordHash(), pw) }

// Recovery codes.
//
// New recovery codes are stored as "hmac-sha256$<base64>" =
// HMAC-SHA-256(K, userID || 0x00 || code), with K a purpose-bound subkey that
// HKDF derives from JANUS_ENCRYPTION_KEY (label "recovery-codes"). Why HMAC
// and not a slow KDF: a code is a one-time random value, not a human-chosen
// password, so dictionary attacks do not apply; the secret key makes a stolen
// database alone useless for offline guessing; and anyone who also holds the
// encryption key can already decrypt the TOTP secret stored next to the codes,
// so a slow hash would add no protection for the second factor while costing
// ten full KDF runs per recovery attempt. Online guessing is bounded by the
// pending-login attempt limit and the per-IP sign-in rate limit. Binding the
// user ID stops a code row copied to another account from working there.
//
// Codes written by earlier releases are bcrypt (cost 6). They are still
// accepted; a used code is consumed, so it is never stored again, and the
// next set the user generates is HMAC.
const (
	recoveryScheme   = "hmac-sha256$"
	recoveryKeyLabel = "recovery-codes"
)

func recoveryCodeMAC(key []byte, userID, code string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(userID))
	m.Write([]byte{0})
	m.Write([]byte(code))
	return m.Sum(nil)
}

func hashRecoveryCode(key []byte, userID, code string) string {
	return recoveryScheme + base64.RawStdEncoding.EncodeToString(recoveryCodeMAC(key, userID, code))
}

// recoveryCodeMatches reports whether code matches one stored line (either
// format), comparing in constant time.
func recoveryCodeMatches(key []byte, userID, stored, code string) bool {
	switch {
	case strings.HasPrefix(stored, recoveryScheme):
		want, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, recoveryScheme))
		if err != nil {
			return false
		}
		return hmac.Equal(recoveryCodeMAC(key, userID, code), want)
	case isBcryptHash(stored):
		return legacyBcryptCompiled && bcryptMatches(stored, code)
	default:
		return false
	}
}
