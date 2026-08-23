package api_test

import (
	"net/http"
	"testing"

	"github.com/mqttview/mqttview/internal/config"
)

func TestPasswordLoginCanBeSwitchedOff(t *testing.T) {
	ts := newTestServer(t, func(c *config.Config) { c.Auth.AllowLocal = false })

	// 403, not 401: the password may well be right, and telling somebody their
	// credentials are wrong when the server simply does not accept passwords
	// sends them to reset a password that will not help.
	code := ts.status(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword})
	if code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", code)
	}
}

func TestRepeatedFailuresAreRateLimited(t *testing.T) {
	ts := newTestServer(t)

	// The limiter allows ten attempts per address and window. Well past that,
	// the answer has to change from "wrong password" to "stop", or the six
	// digits of a second factor could be ground down at leisure.
	var sawLimit bool
	for i := 0; i < 15; i++ {
		code := ts.status(http.MethodPost, "/api/auth/login",
			map[string]string{"email": adminEmail, "password": "not-the-password"})
		if code == http.StatusTooManyRequests {
			sawLimit = true
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d gave %d", i, code)
		}
	}
	if !sawLimit {
		t.Fatal("fifteen wrong passwords in a row were never rate limited")
	}

	// And the right password does not get through either while the limiter is
	// armed: otherwise the limit would only slow down the wrong guesses.
	if code := ts.status(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword}); code != http.StatusTooManyRequests {
		t.Errorf("the correct password gave %d while limited, want 429", code)
	}
}

func TestChangingAPasswordThatIsNotThere(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	// An account created without one signs in through SSO, and there is
	// nothing for the current-password check to compare against.
	newUser := func(email string) {
		t.Helper()
		ts.decode(ts.do(http.MethodPost, "/api/users", map[string]any{
			"email": email, "role": "operator",
		}), http.StatusCreated, nil)
	}
	newUser("sso-only@example.com")

	// Signing in as that account is impossible by design, so the 409 is checked
	// through the administrator's own account after clearing its hash.
	if err := ts.db.SetPasswordHash(mustUserID(t, ts, adminEmail), ""); err != nil {
		t.Fatal(err)
	}
	code := ts.status(http.MethodPost, "/api/auth/password", map[string]string{
		"currentPassword": adminPassword, "newPassword": "another-good-passphrase",
	})
	if code != http.StatusConflict {
		t.Errorf("status = %d, want 409", code)
	}
}

func TestChangingAPasswordWithTheWrongCurrentOne(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	code := ts.status(http.MethodPost, "/api/auth/password", map[string]string{
		"currentPassword": "not-it", "newPassword": "another-good-passphrase",
	})
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}

	// The old password still works, which is the point of refusing.
	if code := ts.status(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword}); code != http.StatusOK {
		t.Errorf("the account was damaged by a failed change: %d", code)
	}
}

// mustUserID looks an account up by address for tests that need its id.
func mustUserID(t *testing.T, ts *testServer, email string) string {
	t.Helper()
	u, err := ts.db.GetUserByEmail(email)
	if err != nil {
		t.Fatalf("looking up %s: %v", email, err)
	}
	return u.ID
}
