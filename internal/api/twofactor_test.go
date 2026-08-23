package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/auth"
)

// currentCode produces what an authenticator app would show right now.
func currentCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := auth.TOTPCodeAt(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	return code
}

// enrol takes the signed-in account through the whole enrolment and returns the
// secret and the recovery codes.
func enrol(t *testing.T, ts *testServer) (string, []string) {
	t.Helper()

	var enrolment struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/auth/2fa/enrol", nil), http.StatusOK, &enrolment)
	if enrolment.Secret == "" || !strings.HasPrefix(enrolment.URI, "otpauth://totp/") {
		t.Fatalf("enrolment looks wrong: %+v", enrolment)
	}

	var confirmed struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/auth/2fa/confirm",
		map[string]string{"code": currentCode(t, enrolment.Secret)}), http.StatusOK, &confirmed)

	if len(confirmed.RecoveryCodes) != 10 {
		t.Fatalf("got %d recovery codes, want 10", len(confirmed.RecoveryCodes))
	}
	return enrolment.Secret, confirmed.RecoveryCodes
}

func TestTwoFactorEnrolmentAndLogin(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var before struct {
		Enabled bool `json:"enabled"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/2fa", nil), http.StatusOK, &before)
	if before.Enabled {
		t.Fatal("two-factor should start off")
	}

	secret, _ := enrol(t, ts)

	var after struct {
		Enabled           bool `json:"enabled"`
		RecoveryCodesLeft int  `json:"recoveryCodesLeft"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/2fa", nil), http.StatusOK, &after)
	if !after.Enabled || after.RecoveryCodesLeft != 10 {
		t.Fatalf("unexpected status after enrolling: %+v", after)
	}

	// From here the password alone must not be enough.
	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)

	var refused struct {
		Error             string `json:"error"`
		TwoFactorRequired bool   `json:"twoFactorRequired"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword}),
		http.StatusUnauthorized, &refused)
	if !refused.TwoFactorRequired {
		t.Fatalf("password alone should have asked for a second factor: %+v", refused)
	}

	// A wrong code is refused, and says so without blaming the password.
	var wrong struct {
		TwoFactorRequired bool `json:"twoFactorRequired"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": adminPassword, "code": "000000",
	}), http.StatusUnauthorized, &wrong)
	if !wrong.TwoFactorRequired {
		t.Fatal("a wrong code should still report that a second factor is wanted")
	}

	// The right one gets in.
	ts.decode(ts.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": adminPassword, "code": currentCode(t, secret),
	}), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodGet, "/api/auth/me", nil), http.StatusOK, nil)
}

func TestRecoveryCodeSignsInOnceOnly(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	_, codes := enrol(t, ts)
	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)

	// A recovery code stands in for the authenticator.
	ts.decode(ts.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": adminPassword, "code": codes[0],
	}), http.StatusOK, nil)

	var status struct {
		RecoveryCodesLeft int `json:"recoveryCodesLeft"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/2fa", nil), http.StatusOK, &status)
	if status.RecoveryCodesLeft != 9 {
		t.Fatalf("a used code should be spent, %d left", status.RecoveryCodesLeft)
	}

	// The same one must not work twice.
	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)
	resp := ts.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": adminPassword, "code": codes[0],
	})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a recovery code was accepted a second time")
	}
}

func TestRegeneratingRecoveryCodesInvalidatesTheOldOnes(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	secret, old := enrol(t, ts)

	var fresh struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/auth/2fa/recovery-codes",
		map[string]string{"code": currentCode(t, secret)}), http.StatusOK, &fresh)

	if len(fresh.RecoveryCodes) != 10 {
		t.Fatalf("got %d fresh codes", len(fresh.RecoveryCodes))
	}
	for _, c := range fresh.RecoveryCodes {
		for _, o := range old {
			if c == o {
				t.Fatal("a regenerated set repeated an old code")
			}
		}
	}

	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)
	resp := ts.do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": adminPassword, "code": old[0],
	})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an old recovery code still worked after regenerating")
	}
}

func TestDisablingTwoFactorNeedsThePasswordAndACode(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	secret, _ := enrol(t, ts)

	// Neither half on its own.
	for _, body := range []map[string]string{
		{"password": adminPassword},
		{"code": currentCode(t, secret)},
		{"password": "wrong", "code": currentCode(t, secret)},
	} {
		resp := ts.do(http.MethodPost, "/api/auth/2fa/disable", body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("two-factor was turned off with %v", body)
		}
	}

	ts.decode(ts.do(http.MethodPost, "/api/auth/2fa/disable", map[string]string{
		"password": adminPassword, "code": currentCode(t, secret),
	}), http.StatusOK, nil)

	var status struct {
		Enabled bool `json:"enabled"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/2fa", nil), http.StatusOK, &status)
	if status.Enabled {
		t.Fatal("two-factor is still on after being disabled")
	}

	// And the password alone signs in again.
	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword}), http.StatusOK, nil)
}

func TestEnrolmentIsNotEnforcedUntilConfirmed(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// Start enrolling and then walk away, which is what closing the tab does.
	var enrolment struct {
		Secret string `json:"secret"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/auth/2fa/enrol", nil), http.StatusOK, &enrolment)

	var status struct {
		Enabled bool `json:"enabled"`
		Pending bool `json:"pending"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/auth/2fa", nil), http.StatusOK, &status)
	if status.Enabled || !status.Pending {
		t.Fatalf("an unconfirmed secret should be pending, not enabled: %+v", status)
	}

	// The account must still be reachable with the password alone.
	ts.decode(ts.do(http.MethodPost, "/api/auth/logout", nil), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword}), http.StatusOK, nil)
}

func TestEnrollingTwiceIsRefused(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	enrol(t, ts)

	resp := ts.do(http.MethodPost, "/api/auth/2fa/enrol", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-enrolling returned %d, want 409", resp.StatusCode)
	}
}
