package secgw

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// 2026-11 security review, DATA-04: the stored hash of matched text was a
// plain SHA-256. What gets matched is often short (a phone number, a term),
// so hashing every candidate recovers it. The hash is keyed now.
func TestKeyedHash(t *testing.T) {
	key, otherKey := []byte("0123456789abcdef0123456789abcdef"), []byte("fedcba9876543210fedcba9876543210")
	v := Violation{Text: "555-867-5309"}
	got := v.KeyedHash(key)
	plain := sha256.Sum256([]byte(v.Text))
	if !strings.HasPrefix(got, MatchHashPrefix) || strings.Contains(got, hex.EncodeToString(plain[:])) {
		t.Fatalf("hash %q is not keyed", got)
	}
	if got != (Violation{Text: "555-867-5309", Offset: 40, RuleID: "other"}).KeyedHash(key) {
		t.Fatal("the same text gave two values: repeats could not be linked")
	}
	if got == (Violation{Text: "555-867-5310"}).KeyedHash(key) {
		t.Fatal("different text gave the same value")
	}
	if got == v.KeyedHash(otherKey) {
		t.Fatal("the value does not depend on the key")
	}
	if (Violation{}).KeyedHash(key) != "" || v.KeyedHash(nil) != "" {
		t.Fatal("no text or no key must give no hash, never an unkeyed one")
	}
	if row := v.ToStore(store.SecgwCaptureConfig{}, nil, key); row.MatchHash != got {
		t.Fatalf("stored hash %q", row.MatchHash)
	}
}
