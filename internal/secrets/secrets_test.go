package secrets

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestBox(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k"), true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	n, ct, err := b.Seal([]byte("s3cr3t"), "conn/source")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("s3cr3t")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if p, err := b.Open(n, ct, "conn/source"); err != nil || string(p) != "s3cr3t" {
		t.Fatalf("open: %q %v", p, err)
	}
	// A ciphertext is bound to its name: it cannot be moved to another secret.
	if _, err := b.Open(n, ct, "conn/target"); err == nil {
		t.Fatal("opened under a different name")
	}
	other, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "k2"), true)
	b2, _ := New(other)
	if _, err := b2.Open(n, ct, "conn/source"); err == nil {
		t.Fatal("opened with the wrong key")
	}
	if _, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "missing"), false); err == nil {
		t.Fatal("a missing key was not reported")
	}
}
