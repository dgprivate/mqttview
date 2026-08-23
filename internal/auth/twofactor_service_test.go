package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mqttview/mqttview/internal/config"
	"github.com/mqttview/mqttview/internal/store"
)

// enrolled returns a user with two-factor turned on, plus the secret and the
// recovery codes, so the tests below start from the state that matters.
func enrolled(t *testing.T, svc *Service, db *store.Store) (store.User, string, []string) {
	t.Helper()

	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, err := db.GetUserByEmail("person@example.com")
	if err != nil {
		t.Fatal(err)
	}

	enrolment, err := svc.BeginTwoFactorEnrolment(u)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)

	code, err := TOTPCodeAt(enrolment.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	codes, err := svc.ConfirmTwoFactorEnrolment(u, code)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)
	return u, enrolment.Secret, codes
}

func TestEnrolmentIssuesAScannableSecret(t *testing.T) {
	svc, db := newTestService(t, func(c *config.Config) { c.Auth.TwoFactorIssuer = "Example" })
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	enrolment, err := svc.BeginTwoFactorEnrolment(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enrolment.URI, "issuer=Example") {
		t.Errorf("the configured issuer is not in the URI: %s", enrolment.URI)
	}
	if !strings.Contains(enrolment.URI, "person@example.com") {
		t.Errorf("the account is not in the URI: %s", enrolment.URI)
	}

	// The stored secret is ciphertext; the plaintext is shown once and never
	// written down.
	stored, _ := db.GetUser(u.ID)
	if stored.TOTPSecretEnc == "" || stored.TOTPSecretEnc == enrolment.Secret {
		t.Fatalf("the secret was stored in the clear: %q", stored.TOTPSecretEnc)
	}
}

func TestConfirmingNeedsARealCode(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	// Nothing pending yet.
	if _, err := svc.ConfirmTwoFactorEnrolment(u, "000000"); !errors.Is(err, ErrTwoFactorNotPending) {
		t.Fatalf("confirming with nothing pending gave %v", err)
	}

	if _, err := svc.BeginTwoFactorEnrolment(u); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)

	if _, err := svc.ConfirmTwoFactorEnrolment(u, "000000"); !errors.Is(err, ErrInvalidTOTP) {
		t.Fatalf("a wrong code gave %v", err)
	}
	// Still not enabled, so the account is not locked out by a failed attempt.
	after, _ := db.GetUser(u.ID)
	if after.TwoFactorEnabled() {
		t.Fatal("a failed confirmation enabled two-factor")
	}
}

func TestEnrollingWhileAlreadyOnIsRefused(t *testing.T) {
	svc, db := newTestService(t)
	u, _, _ := enrolled(t, svc, db)

	if _, err := svc.BeginTwoFactorEnrolment(u); !errors.Is(err, ErrTwoFactorAlreadyOn) {
		t.Fatalf("got %v, want ErrTwoFactorAlreadyOn", err)
	}
	if _, err := svc.ConfirmTwoFactorEnrolment(u, "000000"); !errors.Is(err, ErrTwoFactorAlreadyOn) {
		t.Fatalf("got %v, want ErrTwoFactorAlreadyOn", err)
	}
}

func TestStatusReflectsEachStage(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	status, err := svc.TwoFactorStatusFor(u)
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.Pending {
		t.Fatalf("a fresh account: %+v", status)
	}

	if _, err := svc.BeginTwoFactorEnrolment(u); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)
	status, _ = svc.TwoFactorStatusFor(u)
	if status.Enabled || !status.Pending {
		t.Fatalf("mid-enrolment: %+v", status)
	}

	u, _, _ = enrolled(t, svc, db)
	status, _ = svc.TwoFactorStatusFor(u)
	if !status.Enabled || status.RecoveryCodesLeft != 10 {
		t.Fatalf("after enrolling: %+v", status)
	}
}

func TestPolicyAppliesToLocalAccountsOnly(t *testing.T) {
	svc, db := newTestService(t, func(c *config.Config) {
		c.Auth.RequireTwoFactor = true
		c.Auth.AllowSignup = true
	})

	local, err := db.CreateUser(store.User{
		ID: "l1", Email: "local@example.com", Role: store.RoleViewer, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !svc.TwoFactorRequired(local) {
		t.Error("the policy does not apply to a local account")
	}

	// An SSO account authenticates at the identity provider, which is where a
	// second factor belongs; requiring one here as well buys nothing.
	federated, err := svc.resolveFederatedUser("idp", "s1", "sso@example.com", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if svc.TwoFactorRequired(federated) {
		t.Error("the policy was applied to an SSO account")
	}

	status, _ := svc.TwoFactorStatusFor(local)
	if !status.RequiredByPolicy || !status.RequiredForThisUser {
		t.Errorf("status does not report the policy: %+v", status)
	}
}

func TestVerifySecondFactorTakesEitherKindOfCode(t *testing.T) {
	svc, db := newTestService(t)
	u, secret, codes := enrolled(t, svc, db)
	ctx := context.Background()

	code, _ := TOTPCodeAt(secret, time.Now())
	if err := svc.VerifySecondFactor(ctx, u, code); err != nil {
		t.Fatalf("a valid TOTP code was refused: %v", err)
	}
	if err := svc.VerifySecondFactor(ctx, u, "000000"); !errors.Is(err, ErrInvalidTOTP) {
		t.Errorf("a wrong TOTP code gave %v", err)
	}

	// A recovery code is told from a TOTP code by shape, not by asking.
	if err := svc.VerifySecondFactor(ctx, u, codes[0]); err != nil {
		t.Fatalf("a recovery code was refused: %v", err)
	}
	if err := svc.VerifySecondFactor(ctx, u, codes[0]); !errors.Is(err, ErrRecoveryCodeInvalid) {
		t.Errorf("a spent recovery code gave %v", err)
	}
	if err := svc.VerifySecondFactor(ctx, u, "aaaa-bbbb-cccc"); !errors.Is(err, ErrRecoveryCodeInvalid) {
		t.Errorf("an invented recovery code gave %v", err)
	}
}

func TestRecoveryCodesAreCaseAndPunctuationInsensitive(t *testing.T) {
	svc, db := newTestService(t)
	u, _, codes := enrolled(t, svc, db)

	// Somebody reading one off paper will type it differently.
	typed := strings.ToUpper(strings.ReplaceAll(codes[0], "-", " "))
	if err := svc.VerifySecondFactor(context.Background(), u, typed); err != nil {
		t.Fatalf("a code typed as %q was refused: %v", typed, err)
	}
}

func TestVerifyingAgainstAnAccountWithoutTwoFactor(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	if err := svc.VerifySecondFactor(context.Background(), u, "000000"); !errors.Is(err, ErrTwoFactorNotPending) {
		t.Fatalf("got %v", err)
	}
}

func TestRegeneratingNeedsTwoFactorToBeOn(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")

	if _, err := svc.RegenerateRecoveryCodes(u); !errors.Is(err, ErrTwoFactorNotPending) {
		t.Fatalf("got %v", err)
	}

	u, _, old := enrolled(t, svc, db)
	fresh, err := svc.RegenerateRecoveryCodes(u)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 10 {
		t.Fatalf("got %d codes", len(fresh))
	}
	if err := svc.VerifySecondFactor(context.Background(), u, old[0]); err == nil {
		t.Error("an old recovery code survived regeneration")
	}
}

func TestCompleteLoginRequiresBothFactors(t *testing.T) {
	svc, db := newTestService(t)
	u, secret, _ := enrolled(t, svc, db)
	ctx := context.Background()

	// The password alone stops at the second factor.
	if _, err := svc.Login(ctx, u.Email, testPassword, "10.0.0.1"); !errors.Is(err, ErrTwoFactorRequired) {
		t.Fatalf("Login gave %v, want ErrTwoFactorRequired", err)
	}

	if _, err := svc.CompleteLogin(ctx, u.Email, testPassword, "000000", "10.0.0.1"); err == nil {
		t.Error("a wrong code completed the login")
	}
	if _, err := svc.CompleteLogin(ctx, u.Email, "wrong", "000000", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("a wrong password should fail as a credential error")
	}

	code, _ := TOTPCodeAt(secret, time.Now())
	got, err := svc.CompleteLogin(ctx, u.Email, testPassword, code, "10.0.0.1")
	if err != nil {
		t.Fatalf("the right password and code failed: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("signed in as %s", got.Email)
	}

	// An account without two-factor completes on the password alone.
	if err := svc.DisableTwoFactor(u); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteLogin(ctx, u.Email, testPassword, "", "10.0.0.1"); err != nil {
		t.Fatalf("after disabling: %v", err)
	}
	after, _ := db.GetUser(u.ID)
	if after.TwoFactorEnabled() {
		t.Error("two-factor is still on after being disabled")
	}
}

func TestClientIPTrustsHeadersOnlyWhenTold(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.1")

	svc, _ := newTestService(t)
	// The default: the header is a claim anyone can make, and it keys the
	// sign-in rate limit.
	if got := svc.ClientIP(req); got != "192.0.2.10" {
		t.Errorf("ClientIP = %q, want the peer address", got)
	}

	trusting, _ := newTestService(t, func(c *config.Config) { c.Auth.TrustProxyHeaders = true })
	if got := trusting.ClientIP(req); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the first forwarded entry", got)
	}

	// With trust on but no header, the peer address is still the answer.
	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	bare.RemoteAddr = "192.0.2.10:5555"
	if got := trusting.ClientIP(bare); got != "192.0.2.10" {
		t.Errorf("ClientIP = %q", got)
	}

	// A RemoteAddr with no port is used as-is rather than dropped.
	odd := httptest.NewRequest(http.MethodGet, "/", nil)
	odd.RemoteAddr = "unix-socket"
	if got := svc.ClientIP(odd); got != "unix-socket" {
		t.Errorf("ClientIP = %q", got)
	}
}
