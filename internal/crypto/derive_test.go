package crypto

import (
	"bytes"
	"testing"
)

func TestDeriveKeyIsDeterministicAndLabelSeparated(t *testing.T) {
	a, err := New(bytes.Repeat([]byte("a"), 32))
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := New(bytes.Repeat([]byte("a"), 32))
	b, _ := New(bytes.Repeat([]byte("b"), 32))

	k1 := a.DeriveKey("chat-search")
	if len(k1) != 32 {
		t.Fatalf("len = %d", len(k1))
	}
	if !bytes.Equal(k1, a.DeriveKey("chat-search")) || !bytes.Equal(k1, a2.DeriveKey("chat-search")) {
		t.Fatal("same key and label gave different subkeys")
	}
	if bytes.Equal(k1, a.DeriveKey("other")) {
		t.Fatal("different labels gave the same subkey")
	}
	if bytes.Equal(k1, b.DeriveKey("chat-search")) {
		t.Fatal("different instance keys gave the same subkey")
	}
	if bytes.Equal(k1, bytes.Repeat([]byte("a"), 32)) {
		t.Fatal("subkey equals the instance key")
	}
	// Callers may not mutate the instance key through a returned slice.
	k1[0] ^= 0xff
	if bytes.Equal(k1, a.DeriveKey("chat-search")) {
		t.Fatal("DeriveKey returned shared memory")
	}
}
