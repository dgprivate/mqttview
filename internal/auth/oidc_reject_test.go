package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/config"
	"github.com/dgprivate/mqttview/internal/store"
)

// Everything on the way back from an OIDC provider that is not a valid,
// signed ID token for this login must end in a refusal with no session. The
// happy path is in sso_flow_test.go; these are the ways it must not succeed.

// callback replays the callback and reports whether a session cookie was set,
// which is the thing an attacker is after.
func callback(t *testing.T, svc *Service, id string, cookie *http.Cookie, q url.Values) (store.User, string, bool, error) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/sso/"+id+"/callback?"+q.Encode(), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	user, next, err := svc.CompleteSSO(rec, req, id)
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			return user, next, true, err
		}
	}
	return user, next, false, err
}

func TestAnIDTokenThatDoesNotProveItselfIsRefused(t *testing.T) {
	impostor, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	for name, spoil := range map[string]func(*fakeIDP){
		"signed by a key the provider does not publish": func(i *fakeIDP) { i.signWith = impostor },
		"issued for another client":                     func(i *fakeIDP) { i.claims["aud"] = "someone-else" },
		"issued by another provider":                    func(i *fakeIDP) { i.claims["iss"] = "https://evil.example.com" },
		"expired":                                       func(i *fakeIDP) { i.claims["exp"] = time.Now().Add(-time.Hour).Unix() },
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			svc, db := newTestService(t, withProvider(idp))

			cookie, q := startSSO(t, svc, "")
			idp.nonce = q.Get("nonce")
			idp.claims = map[string]any{"email": "admin@example.com", "email_verified": true}
			spoil(idp)

			_, _, session, err := callback(t, svc, "test", cookie, url.Values{
				"state": {q.Get("state")}, "code": {"c"},
			})
			if err == nil || !strings.Contains(err.Error(), "verification failed") {
				t.Fatalf("error = %v, want a verification failure", err)
			}
			if session {
				t.Error("a session was issued")
			}
			if _, err := db.GetUserByEmail("admin@example.com"); err == nil {
				t.Error("an account was created from an unverified token")
			}
		})
	}
}

func TestTheCodeExchangeMustYieldAnIDToken(t *testing.T) {
	for name, spoil := range map[string]func(*fakeIDP){
		"the provider refuses the code":  func(i *fakeIDP) { i.tokenStatus = http.StatusBadRequest },
		"the provider sends no ID token": func(i *fakeIDP) { i.omitIDToken = true },
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			svc, _ := newTestService(t, withProvider(idp))

			cookie, q := startSSO(t, svc, "")
			idp.nonce = q.Get("nonce")
			spoil(idp)

			if _, _, session, err := callback(t, svc, "test", cookie, url.Values{
				"state": {q.Get("state")}, "code": {"c"},
			}); err == nil || session {
				t.Fatalf("signed in without an ID token: %v", err)
			}
		})
	}
}

func TestAnIDTokenMustNameSomebody(t *testing.T) {
	for name, tc := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"no subject": {map[string]any{"sub": "", "email": "p@example.com", "email_verified": true}, "no subject"},
		"no email":   {map[string]any{"email_verified": true}, "did not return an email"},
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			svc, _ := newTestService(t, withProvider(idp))

			cookie, q := startSSO(t, svc, "")
			idp.nonce = q.Get("nonce")
			idp.claims = tc.claims

			_, _, session, err := callback(t, svc, "test", cookie, url.Values{
				"state": {q.Get("state")}, "code": {"c"},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one about %q", err, tc.want)
			}
			if session {
				t.Error("a session was issued")
			}
		})
	}
}

// sealedOIDCState builds a state cookie the service will open as its own.
func sealedOIDCState(t *testing.T, svc *Service, st oidcState) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return sealedRaw(t, svc, string(payload))
}

func sealedRaw(t *testing.T, svc *Service, plain string) *http.Cookie {
	t.Helper()
	sealed, err := svc.box.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: oidcCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(sealed))}
}

func TestAnSSOStateCookieThatIsNotAFreshOneOfOursIsRefused(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, withProvider(idp))
	q := url.Values{"state": {"s"}, "code": {"c"}}
	live := time.Now().Add(time.Minute).Unix()

	for name, cookie := range map[string]*http.Cookie{
		"not base64":                  {Name: oidcCookie, Value: "!!!"},
		"sealed by somebody else":     {Name: oidcCookie, Value: base64.RawURLEncoding.EncodeToString([]byte("forged"))},
		"ours, but not a login state": sealedRaw(t, svc, "not json"),
		"expired": sealedOIDCState(t, svc, oidcState{
			Provider: "test", State: "s", Expires: time.Now().Add(-time.Minute).Unix(),
		}),
		"issued for another provider": sealedOIDCState(t, svc, oidcState{
			Provider: "other", State: "s", Expires: live,
		}),
	} {
		if _, _, session, err := callback(t, svc, "test", cookie, q); err == nil || session {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestAProviderRefusalWithoutADescriptionIsStillReported(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, withProvider(idp))

	cookie, q := startSSO(t, svc, "")
	_, _, session, err := callback(t, svc, "test", cookie, url.Values{
		"state": {q.Get("state")}, "error": {"access_denied"},
	})
	if err == nil || !strings.Contains(err.Error(), "access_denied") || session {
		t.Fatalf("error = %v, session = %v", err, session)
	}
}

func TestAnSSOProviderThatCannotBeUsedIsRefused(t *testing.T) {
	svc, _ := newTestService(t, func(c *config.Config) {
		c.Auth.Providers = map[string]config.ProviderConfig{
			"down": {Enabled: true, Issuer: "http://127.0.0.1:1", ClientID: "c"},
		}
	})

	// Discovery fails: there is nothing to redirect to.
	if _, err := svc.StartSSO(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil), "down", ""); err == nil {
		t.Error("a provider whose discovery fails started a login")
	}

	// A callback carrying a genuine state for a provider that is not
	// configured — removed between the two legs, say — has nothing to verify
	// a token against.
	cookie := sealedOIDCState(t, svc, oidcState{
		Provider: "gone", State: "s", Expires: time.Now().Add(time.Minute).Unix(),
	})
	if _, _, session, err := callback(t, svc, "gone", cookie, url.Values{
		"state": {"s"}, "code": {"c"},
	}); err == nil || session {
		t.Errorf("a callback for an unconfigured provider was accepted: %v", err)
	}
}

func TestSSODoesNotCreateAccountsWhenSignupIsOff(t *testing.T) {
	idp := newFakeIDP(t)
	svc, db := newTestService(t, withProvider(idp), func(c *config.Config) { c.Auth.AllowSignup = false })

	cookie, q := startSSO(t, svc, "")
	idp.nonce = q.Get("nonce")
	idp.claims = map[string]any{"email": "stranger@example.com", "email_verified": true}

	if _, _, session, err := callback(t, svc, "test", cookie, url.Values{
		"state": {q.Get("state")}, "code": {"c"},
	}); err == nil || session {
		t.Fatalf("an unknown person was signed in with signup off: %v", err)
	}
	if _, err := db.GetUserByEmail("stranger@example.com"); err == nil {
		t.Error("an account was created with signup off")
	}
}

func TestAnSSOLoginWithNoDestinationLandsOnTheHomePage(t *testing.T) {
	idp := newFakeIDP(t)
	svc, _ := newTestService(t, withProvider(idp))

	cookie, q := startSSO(t, svc, "")
	idp.nonce = q.Get("nonce")
	idp.claims = map[string]any{"email": "person@example.com", "email_verified": true}

	_, next, session, err := callback(t, svc, "test", cookie, url.Values{
		"state": {q.Get("state")}, "code": {"c"},
	})
	if err != nil || !session {
		t.Fatalf("sign-in failed: %v", err)
	}
	if next != "/" {
		t.Errorf("next = %q, want /", next)
	}
}
