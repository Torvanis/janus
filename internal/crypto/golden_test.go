package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// goldenVectors were produced by the pre-hardening code (commit fc50f8f,
// cipher.NewGCM with an explicit crypto/rand nonce and golang.org/x/crypto/hkdf).
// They stand in for every envelope already stored in customer databases: the
// current code must keep opening all of them and keep deriving the same
// subkeys, or upgraded instances would lose their upstream credentials, TOTP
// secrets, encrypted blobs and chat search index.
type goldenVectors struct {
	Strings []struct {
		Key       string `json:"key_hex"`
		Plaintext string `json:"plaintext"`
		Envelope  string `json:"envelope"`
	} `json:"strings"`
	Bytes []struct {
		Key       string `json:"key_hex"`
		Plaintext string `json:"plaintext_b64"`
		Sealed    string `json:"sealed_b64"`
	} `json:"bytes"`
	Derive []struct {
		Key    string `json:"key_hex"`
		Label  string `json:"label"`
		Subkey string `json:"subkey_hex"`
	} `json:"derive"`
}

func loadGolden(t *testing.T) goldenVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden_fc50f8f.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenVectors
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Strings) == 0 || len(g.Bytes) == 0 || len(g.Derive) == 0 {
		t.Fatal("golden file is missing vectors")
	}
	return g
}

func cipherFromHex(t *testing.T, k string) *Cipher {
	t.Helper()
	key, err := hex.DecodeString(k)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLegacyEnvelopesStillDecrypt(t *testing.T) {
	g := loadGolden(t)
	for i, v := range g.Strings {
		got, err := cipherFromHex(t, v.Key).Decrypt(v.Envelope)
		if err != nil {
			t.Fatalf("vector %d: legacy janus.v1 envelope no longer decrypts: %v", i, err)
		}
		if got != v.Plaintext {
			t.Fatalf("vector %d: got %q want %q", i, got, v.Plaintext)
		}
	}
}

func TestLegacyBytesEnvelopesStillDecrypt(t *testing.T) {
	g := loadGolden(t)
	for i, v := range g.Bytes {
		sealed, _ := base64.StdEncoding.DecodeString(v.Sealed)
		want, _ := base64.StdEncoding.DecodeString(v.Plaintext)
		got, err := cipherFromHex(t, v.Key).DecryptBytes(sealed)
		if err != nil {
			t.Fatalf("vector %d: legacy JB\\x01 object no longer decrypts: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("vector %d: plaintext mismatch", i)
		}
	}
}

func TestDeriveKeyMatchesLegacyHKDF(t *testing.T) {
	g := loadGolden(t)
	for i, v := range g.Derive {
		got := hex.EncodeToString(cipherFromHex(t, v.Key).DeriveKey(v.Label))
		if got != v.Subkey {
			t.Fatalf("vector %d (label %q): stdlib crypto/hkdf gives %s, legacy x/crypto/hkdf gave %s", i, v.Label, got, v.Subkey)
		}
	}
}

// TestNewEnvelopeLayoutIsUnchanged proves new output keeps the legacy wire
// format (prefix, 12-byte nonce, ciphertext, 16-byte tag), so no envelope
// version bump is needed and an older binary (rollback) can read new rows.
func TestNewEnvelopeLayoutIsUnchanged(t *testing.T) {
	c := testKey(t, 9)
	env, err := c.Encrypt("sk-abc")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(env[len(envelopePrefix):])
	if err != nil {
		t.Fatal(err)
	}
	if want := 12 + len("sk-abc") + 16; len(raw) != want {
		t.Fatalf("sealed length %d, want %d (nonce||ct||tag)", len(raw), want)
	}
	obj, err := c.EncryptBytes([]byte("xyz"))
	if err != nil {
		t.Fatal(err)
	}
	if want := 3 + 12 + 3 + 16; len(obj) != want {
		t.Fatalf("object length %d, want %d (header||nonce||ct||tag, +31 bytes)", len(obj), want)
	}
}

// TestLegacyReaderOpensNewEnvelopes replays the pre-hardening decrypt path
// (cipher.NewGCM, nonce taken from the first 12 bytes) on new output, which
// is what a rolled-back binary would do. That construction is refused under
// GODEBUG=fips140=only, so the test skips there; the layout test above still
// covers the format in that mode.
func TestLegacyReaderOpensNewEnvelopes(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := cipher.NewGCM(block)
	if err != nil {
		t.Skipf("legacy GCM unavailable in this mode: %v", err)
	}
	env, err := c.Encrypt("rollback-safe")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(env[len(envelopePrefix):])
	plain, err := legacy.Open(nil, raw[:12], raw[12:], nil)
	if err != nil || string(plain) != "rollback-safe" {
		t.Fatalf("legacy reader: %q %v", plain, err)
	}
	obj, err := c.EncryptBytes([]byte("blob"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err = legacy.Open(nil, obj[3:15], obj[15:], bytesMagic)
	if err != nil || string(plain) != "blob" {
		t.Fatalf("legacy object reader: %q %v", plain, err)
	}
}
