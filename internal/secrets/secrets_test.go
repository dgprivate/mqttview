package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This package holds the key that every broker password and TLS private key in
// the database is encrypted with. The properties below are the ones that make
// the encryption worth having, so each is asserted rather than assumed.

func newBox(t *testing.T) *Box {
	t.Helper()
	key, err := LoadOrCreateKey("", t.TempDir())
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	box, err := New(key)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return box
}

func TestSealOpenRoundTrip(t *testing.T) {
	box := newBox(t)

	for _, plaintext := range []string{
		"", "hunter2", strings.Repeat("x", 10000),
		"-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----",
		"unicode: šumnik ž č", "\x00\x01\xff",
	} {
		sealed, err := box.Seal(plaintext)
		if err != nil {
			t.Fatalf("seal %q: %v", truncate(plaintext), err)
		}
		if plaintext != "" && strings.Contains(sealed, plaintext) {
			t.Fatalf("the ciphertext contains the plaintext: %q", truncate(sealed))
		}
		got, err := box.Open(sealed)
		if err != nil {
			t.Fatalf("open %q: %v", truncate(plaintext), err)
		}
		if got != plaintext {
			t.Fatalf("round trip changed the value: %q -> %q", truncate(plaintext), truncate(got))
		}
	}
}

func TestSealIsNotDeterministic(t *testing.T) {
	box := newBox(t)

	// AES-GCM with a fresh nonce each time. Identical ciphertexts would tell an
	// attacker with database access which brokers share a password.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		sealed, err := box.Seal("same-password")
		if err != nil {
			t.Fatal(err)
		}
		if seen[sealed] {
			t.Fatal("sealing the same value twice produced the same ciphertext")
		}
		seen[sealed] = true
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}

	// Flip a character. GCM authenticates, so this must fail rather than
	// return something that merely looks wrong.
	flipped := []byte(sealed)
	if flipped[len(flipped)-2] == 'A' {
		flipped[len(flipped)-2] = 'B'
	} else {
		flipped[len(flipped)-2] = 'A'
	}
	if _, err := box.Open(string(flipped)); err == nil {
		t.Fatal("a tampered ciphertext was accepted")
	}

	for _, bad := range []string{"not base64!", "AAAA", sealed[:len(sealed)/2]} {
		if _, err := box.Open(bad); err == nil {
			t.Errorf("accepted %q", truncate(bad))
		}
	}

	// An empty stored value means "nothing was ever set" — an account with no
	// password, a connection with no TLS key — and must read back as empty
	// rather than as an error.
	got, err := box.Open("")
	if err != nil || got != "" {
		t.Errorf("empty ciphertext gave (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestADifferentKeyCannotOpenIt(t *testing.T) {
	a, b := newBox(t), newBox(t)

	sealed, err := a.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("a box with a different key opened the ciphertext")
	}
}

func TestLoadOrCreateKeyPersistsAndReuses(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("no key was generated")
	}

	// A second call must return the same key: generating a new one would make
	// every stored credential unreadable on the next restart.
	second, err := LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("a second call generated a different key")
	}

	path := filepath.Join(dir, "secret.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key file was not written: %v", err)
	}
	// It is a credential on disk; group and world have no business reading it.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("secret.key is mode %o, want no group or world access", perm)
	}
}

func TestAnExplicitKeyWins(t *testing.T) {
	dir := t.TempDir()
	explicit, err := LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}

	// Configured through the environment, so nothing should be read from or
	// written to the data directory.
	other := t.TempDir()
	got, err := LoadOrCreateKey(explicit, other)
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatal("the configured key was not used")
	}
	if _, err := os.Stat(filepath.Join(other, "secret.key")); err == nil {
		t.Error("a key file was written even though one was configured")
	}
}

func TestNewRejectsUnusableKeys(t *testing.T) {
	for _, bad := range []string{"", "short", "not base64 or hex!", strings.Repeat("a", 10)} {
		if _, err := New(bad); err == nil {
			t.Errorf("accepted %q as a key", bad)
		}
	}
}

func truncate(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "…"
}
