package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncryptBytesRoundTrip(t *testing.T) {
	c := testKey(t, 3)
	for _, plain := range [][]byte{{}, []byte("x"), bytes.Repeat([]byte{0, 1, 2, 0xff}, 4096), []byte("unicode ✓ 秘密")} {
		env, err := c.EncryptBytes(plain)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(env, []byte{'J', 'B', 0x01}) {
			t.Fatalf("missing versioned header: % x", env[:3])
		}
		if want := len(plain) + 3 + 12 + 16; len(env) != want {
			t.Fatalf("envelope length %d, want %d", len(env), want)
		}
		if len(plain) > 4 && bytes.Contains(env, plain) {
			t.Fatal("ciphertext contains the plaintext")
		}
		got, err := c.DecryptBytes(env)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round trip mismatch: %q vs %q", got, plain)
		}
	}
}

func TestEncryptBytesFreshNonce(t *testing.T) {
	c := testKey(t, 3)
	a, _ := c.EncryptBytes([]byte("same"))
	b, _ := c.EncryptBytes([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext are identical; nonce reused")
	}
}

func TestDecryptBytesRejects(t *testing.T) {
	c := testKey(t, 3)
	env, _ := c.EncryptBytes([]byte("secret payload"))

	if _, err := testKey(t, 9).DecryptBytes(env); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong key: got %v, want ErrDecrypt", err)
	}
	if _, err := c.DecryptBytes([]byte("plain old bytes")); !errors.Is(err, ErrBytesFormat) {
		t.Fatalf("foreign data: got %v, want ErrBytesFormat", err)
	}
	if _, err := c.DecryptBytes(env[:10]); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("truncated: got %v, want ErrDecrypt", err)
	}
	tampered := append([]byte{}, env...)
	tampered[len(tampered)-1] ^= 1
	if _, err := c.DecryptBytes(tampered); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered tag: got %v, want ErrDecrypt", err)
	}
	// A future format version must not be accepted as v1.
	future := append([]byte{}, env...)
	future[2] = 0x02
	if _, err := c.DecryptBytes(future); !errors.Is(err, ErrBytesFormat) {
		t.Fatalf("future version: got %v, want ErrBytesFormat", err)
	}
}
