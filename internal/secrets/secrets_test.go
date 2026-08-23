package secrets

import (
	"encoding/base64"
	"encoding/hex"
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

// The key file is the whole security boundary for everything at rest, so its
// handling has to be exact rather than approximately right.

func TestTheKeyFileIsCreatedOnceAndReused(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	// A new key on every start would make every stored password unreadable.
	if first != second {
		t.Fatal("a second call generated a different key")
	}

	info, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret.key is mode %o, want 600", perm)
	}
}

func TestAnUnusableKeyFileIsReportedRatherThanReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(path, []byte("this is not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Overwriting it would silently destroy every secret already encrypted
	// with the real key, so the only safe answer is to refuse to start.
	_, err := LoadOrCreateKey("", dir)
	if err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "this is not a key\n" {
		t.Error("the unusable key file was overwritten")
	}
}

func TestAConfiguredKeyIsValidatedBeforeUse(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadOrCreateKey("too short", dir); err == nil {
		t.Error("a short key was accepted")
	}
	// A configured key must not cause a file to be written: the operator has
	// chosen to keep it somewhere else.
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); !os.IsNotExist(err) {
		t.Error("a key file was written even though a key was configured")
	}

	// Both encodings are accepted, because both appear in the wild.
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	for name, encoded := range map[string]string{
		"hex":    hex.EncodeToString(raw),
		"base64": base64.StdEncoding.EncodeToString(raw),
	} {
		got, err := LoadOrCreateKey(encoded, dir)
		if err != nil || got != encoded {
			t.Errorf("%s key: %q %v", name, got, err)
		}
	}
}

func TestCiphertextThatCannotBeOpened(t *testing.T) {
	box := newBox(t)

	for name, input := range map[string]string{
		"not base64":    "!!!!",
		"too short":     base64.StdEncoding.EncodeToString([]byte("short")),
		"a wrong nonce": base64.StdEncoding.EncodeToString(make([]byte, 40)),
	} {
		if _, err := box.Open(input); err == nil {
			t.Errorf("%s was opened", name)
		}
	}
}

func TestAValueSealedWithAnotherKeyIsNotSilentlyEmpty(t *testing.T) {
	a := newBox(t)
	b := newBox(t)

	sealed, err := a.Seal("broker-password")
	if err != nil {
		t.Fatal(err)
	}
	// Returning "" here would look to a caller like an account with no
	// password, and it would try to connect without one.
	if got, err := b.Open(sealed); err == nil {
		t.Fatalf("another key opened it and returned %q", got)
	}
}
