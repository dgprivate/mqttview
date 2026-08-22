package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mqttview/mqttview/internal/secrets"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	dir := t.TempDir()
	key, err := secrets.LoadOrCreateKey("", dir)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	st, err := Open(filepath.Join(dir, "test.db"), box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestUser(t *testing.T, st *Store) User {
	t.Helper()
	u, err := st.CreateUser(User{
		ID:           uuid.NewString(),
		Email:        "person@example.com",
		PasswordHash: "hash",
		Role:         RoleAdmin,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestTOTPSecretLifecycle(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if u.TwoFactorEnabled() {
		t.Fatal("a new account should not have two-factor")
	}

	if err := st.SetTOTPSecret(u.ID, "ciphertext"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TOTPSecretEnc != "ciphertext" || got.TOTPConfirmedAt != nil {
		t.Fatalf("a freshly issued secret should be unconfirmed: %+v", got)
	}
	if got.TwoFactorEnabled() {
		t.Fatal("an unconfirmed secret must not gate sign-in")
	}

	if err := st.ConfirmTOTP(u.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetUser(u.ID)
	if !got.TwoFactorEnabled() {
		t.Fatal("a confirmed secret should enable two-factor")
	}

	// Confirming again changes nothing: the guard is what stops a replayed
	// confirmation from re-enabling something that was turned off.
	if err := st.ConfirmTOTP(u.ID, time.Now()); err == nil {
		t.Error("confirming twice should report that nothing was pending")
	}

	if err := st.DisableTOTP(u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetUser(u.ID)
	if got.TwoFactorEnabled() || got.TOTPSecretEnc != "" {
		t.Fatalf("disabling should clear the secret: %+v", got)
	}
}

func TestConfirmingWithoutASecretIsRefused(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if err := st.ConfirmTOTP(u.ID, time.Now()); err == nil {
		t.Fatal("confirmed two-factor on an account with no pending secret")
	}
	got, _ := st.GetUser(u.ID)
	if got.TwoFactorEnabled() {
		t.Fatal("two-factor was enabled with nothing to verify against")
	}
}

func TestReEnrollingDiscardsTheOldRecoveryCodes(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if err := st.SetTOTPSecret(u.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRecoveryCodes(u.ID, []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}

	// A new secret means a new authenticator, so codes minted for the old one
	// must not survive.
	if err := st.SetTOTPSecret(u.ID, "second"); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountUnusedRecoveryCodes(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d recovery codes survived re-enrolment", n)
	}
}

func TestRecoveryCodeIsSpentExactlyOnce(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if err := st.ReplaceRecoveryCodes(u.ID, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	hashes, err := st.RecoveryCodeHashes(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 2 {
		t.Fatalf("got %d hashes, want 2", len(hashes))
	}

	var id string
	for k := range hashes {
		id = k
		break
	}
	if err := st.UseRecoveryCode(id, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The second attempt is what a replay looks like, and it must fail rather
	// than silently succeed against an already-used row.
	if err := st.UseRecoveryCode(id, time.Now()); err == nil {
		t.Fatal("the same recovery code was spent twice")
	}

	n, _ := st.CountUnusedRecoveryCodes(u.ID)
	if n != 1 {
		t.Fatalf("%d codes left, want 1", n)
	}
	hashes, _ = st.RecoveryCodeHashes(u.ID)
	if len(hashes) != 1 {
		t.Fatalf("a used code is still listed as available")
	}
}

func TestDeletingAUserTakesItsRecoveryCodes(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if err := st.ReplaceRecoveryCodes(u.ID, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}

	// The foreign key cascades; leaving orphans would let a recreated account
	// with the same ID inherit somebody else's way in.
	n, err := st.CountUnusedRecoveryCodes(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d recovery codes outlived the user", n)
	}
}

func TestSessionUserCarriesTwoFactorState(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if err := st.SetTOTPSecret(u.ID, "ciphertext"); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmTOTP(u.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	sess := Session{
		ID:        "hashed-token",
		UserID:    u.ID,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}

	// SessionUser has a column list of its own, so it can fall out of step with
	// the one GetUser uses. Every request reads the user through this path: if
	// it drops the two-factor columns, the second factor silently stops
	// applying to everything a signed-in user does.
	_, got, err := st.SessionUser(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TwoFactorEnabled() {
		t.Fatal("the session-loaded user lost its two-factor state")
	}
	if got.TOTPSecretEnc != "ciphertext" {
		t.Fatalf("the session-loaded user lost its secret: %q", got.TOTPSecretEnc)
	}
}
