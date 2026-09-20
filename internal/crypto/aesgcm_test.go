package crypto

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T, seed byte) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	c, err := New(key)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	return c
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 24, 31, 33, 64} {
		if _, err := New(make([]byte, size)); err == nil {
			t.Errorf("New accepted a %d-byte key; only 32 bytes is valid for AES-256", size)
		}
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := testKey(t, 1)
	for _, plaintext := range []string{
		"sk-test-credential",
		"a",
		strings.Repeat("x", 4096),
		"unicode ✓ ключ 秘密",
		`{"type":"service_account","private_key_id":"abc"}`, // vertex-style JSON keys
	} {
		envelope, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plaintext[:min(8, len(plaintext))], err)
		}
		if !strings.HasPrefix(envelope, "janus.v1.") {
			t.Fatalf("envelope %q missing janus.v1. version prefix", envelope[:min(16, len(envelope))])
		}
		// A single character can appear in base64 output by chance; only
		// meaningful substrings prove a leak.
		if len(plaintext) >= 8 && strings.Contains(envelope, plaintext) {
			t.Fatal("envelope leaks the plaintext")
		}
		got, err := c.Decrypt(envelope)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != plaintext {
			t.Fatalf("round trip mismatch: got %q want %q", got, plaintext)
		}
	}
}

func TestEmptyPlaintextRoundTripsAsEmpty(t *testing.T) {
	c := testKey(t, 2)
	envelope, err := c.Encrypt("")
	if err != nil || envelope != "" {
		t.Fatalf("Encrypt(\"\") = (%q, %v), want (\"\", nil): 'no credential' must round-trip cleanly", envelope, err)
	}
	plain, err := c.Decrypt("")
	if err != nil || plain != "" {
		t.Fatalf("Decrypt(\"\") = (%q, %v), want (\"\", nil)", plain, err)
	}
}

func TestNoncesAreUnique(t *testing.T) {
	c := testKey(t, 3)
	a, err := c.Encrypt("same secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	b, err := c.Encrypt("same secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if a == b {
		t.Fatal("two encryptions of the same plaintext produced identical envelopes — nonce reuse breaks GCM")
	}
}

func TestTamperedCiphertextIsRejected(t *testing.T) {
	c := testKey(t, 4)
	envelope, err := c.Encrypt("sk-live-credential")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(envelope, "janus.v1."))
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	// Flip one bit in every position class: nonce, body, and auth tag.
	for _, idx := range []int{0, len(raw) / 2, len(raw) - 1} {
		mutated := make([]byte, len(raw))
		copy(mutated, raw)
		mutated[idx] ^= 0x01
		tampered := "janus.v1." + base64.StdEncoding.EncodeToString(mutated)
		if _, err := c.Decrypt(tampered); !errors.Is(err, ErrDecrypt) {
			t.Errorf("tamper at byte %d: err = %v, want ErrDecrypt", idx, err)
		}
	}
}

func TestWrongKeyIsAnExplicitError(t *testing.T) {
	right := testKey(t, 5)
	wrong := testKey(t, 6)
	envelope, err := right.Encrypt("sk-live-credential")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	plain, err := wrong.Decrypt(envelope)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong-key decrypt err = %v, want ErrDecrypt", err)
	}
	if plain != "" {
		t.Fatalf("wrong-key decrypt leaked %q; must return the empty string", plain)
	}
	if !strings.Contains(err.Error(), "JANUS_ENCRYPTION_KEY") {
		t.Fatalf("error %q does not point the operator at JANUS_ENCRYPTION_KEY", err)
	}
}

func TestMalformedEnvelopesAreRejected(t *testing.T) {
	c := testKey(t, 7)
	cases := map[string]string{
		"missing prefix":      base64.StdEncoding.EncodeToString([]byte("not an envelope")),
		"foreign prefix":      "janus.v2." + base64.StdEncoding.EncodeToString([]byte("future version")),
		"invalid base64":      "janus.v1.%%%not-base64%%%",
		"shorter than nonce":  "janus.v1." + base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}),
		"empty after prefix":  "janus.v1.",
		"plaintext masquerad": "sk-live-credential",
	}
	for name, envelope := range cases {
		if _, err := c.Decrypt(envelope); err == nil {
			t.Errorf("%s: Decrypt(%q) succeeded, want an error", name, envelope)
		}
	}
}

func TestMaskShowsAtMostFirstAndLastFour(t *testing.T) {
	if got := Mask(""); got != "" {
		t.Errorf("Mask(\"\") = %q, want empty", got)
	}
	// At 8 characters or fewer, first4+last4 would be the whole secret.
	for _, short := range []string{"a", "12345678"} {
		got := Mask(short)
		if strings.ContainsAny(got, short) {
			t.Errorf("Mask(%q) = %q leaks characters of a short secret", short, got)
		}
		if len([]rune(got)) != len(short) {
			t.Errorf("Mask(%q) = %q, want %d mask runes", short, got, len(short))
		}
	}
	got := Mask("sk-live-abcdef1234")
	if got != "sk-l…1234" {
		t.Errorf("Mask(long) = %q, want %q", got, "sk-l…1234")
	}
}
