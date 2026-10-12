package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/config"
)

func TestTheSAMLKeypairSurvivesARestart(t *testing.T) {
	first := newSAMLTestService(t)
	before, err := first.spKeypair()
	if err != nil {
		t.Fatal(err)
	}

	// A new process over the same data directory. Generating a fresh keypair
	// here would silently invalidate the metadata every identity provider was
	// registered with, and every SAML login would start failing.
	restarted := &Service{dataDir: first.dataDir, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	after, err := restarted.spKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Certificate[0]) != string(before.Certificate[0]) {
		t.Fatal("a restart produced a different service provider certificate")
	}
}

func TestASAMLKeypairThatCannotBeWrittenIsReported(t *testing.T) {
	s := newSAMLTestService(t)
	s.dataDir = filepath.Join(s.dataDir, "does-not-exist")

	if _, err := s.spKeypair(); err == nil {
		t.Error("a keypair was reported as kept when it could not be written")
	}
	// And nothing downstream pretends otherwise.
	if _, err := s.SAMLMetadata(t.Context(), "work"); err == nil {
		t.Error("metadata was produced without a keypair")
	}
}

func TestAMetadataURLThatIsNotAURLIsReported(t *testing.T) {
	s := newSAMLTestService(t)
	if _, err := s.fetchIDPMetadata(t.Context(),
		config.SAMLProviderConfig{MetadataURL: "http://idp.example.com/\x7f"}); err == nil {
		t.Error("an unparseable metadata URL was accepted")
	}
}

func TestASAMLStateCookieThatIsNotAFreshOneOfOursIsRefused(t *testing.T) {
	s := newSAMLTestService(t)
	live := time.Now().Add(time.Minute).Unix()

	sealedText := func(plain string) *http.Cookie {
		sealed, err := s.box.Seal(plain)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: samlCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(sealed))}
	}

	post := func(id string, cookie *http.Cookie, body string) error {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/saml/"+id+"/acs", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		_, _, err := s.CompleteSAML(httptest.NewRecorder(), r, id)
		return err
	}
	form := url.Values{"SAMLResponse": {"x"}}.Encode()

	if err := post("work", &http.Cookie{Name: samlCookie, Value: "!!!"}, form); err == nil {
		t.Error("a cookie that is not base64 was accepted")
	}
	if err := post("work", sealedText("not json"), form); err == nil {
		t.Error("a sealed value that is not a login state was accepted")
	}
	// A genuine state for a provider that has since been removed.
	if err := post("gone", sealedState(t, s, samlState{Provider: "gone", RequestID: "r", Expires: live}), form); err == nil {
		t.Error("a login for an unconfigured provider was completed")
	}
	// A body that is not a form at all.
	if err := post("work", sealedState(t, s, samlState{Provider: "work", RequestID: "r", Expires: live}), "%zz"); err == nil {
		t.Error("an unparseable body was accepted")
	}
}

func TestSSOCannotSignIntoADisabledAccountByItsAddress(t *testing.T) {
	svc, db := newTestService(t, func(c *config.Config) { c.Auth.AllowSignup = true })
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")
	u.Disabled = true
	if err := db.UpdateUser(u); err != nil {
		t.Fatal(err)
	}

	// Never linked to this identity, so the match is by address. Disabling an
	// account has to hold whichever door somebody comes in by.
	if _, err := svc.resolveFederatedUser("idp", "new-subject", "person@example.com", "", nil); err == nil {
		t.Fatal("a disabled local account was signed into through SSO")
	}
	if got, _ := db.GetUser(u.ID); got.ProviderSubject != "" {
		t.Errorf("the disabled account was linked to %q anyway", got.ProviderSubject)
	}
}

func TestATOTPCheckAtTheEpochDoesNotWrapAround(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	// At counter zero there is no previous step. Subtracting one from an
	// unsigned counter would check a step at the far end of time instead.
	epoch := time.Unix(0, 0)
	code, err := TOTPCodeAt(secret, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTOTP(secret, code, epoch); err != nil {
		t.Errorf("the current code at the epoch was refused: %v", err)
	}
	wrapped, err := totpCode(secret, ^uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	if wrapped != code && VerifyTOTP(secret, wrapped, epoch) == nil {
		t.Error("a code from the wrapped-around counter was accepted")
	}
}

func TestASixLetterCodeIsTreatedAsARecoveryCode(t *testing.T) {
	svc, db := newTestService(t)
	if _, _, err := svc.BootstrapAdmin("person@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	u, _ := db.GetUserByEmail("person@example.com")
	enrolment, err := svc.BeginTwoFactorEnrolment(u)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)
	code, _ := TOTPCodeAt(enrolment.Secret, time.Now())
	if _, err := svc.ConfirmTwoFactorEnrolment(u, code); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.ID)

	// Six characters but not six digits is not a TOTP code, and must not be
	// compared as one.
	if err := svc.VerifySecondFactor(context.Background(), u, "abcdef"); !errors.Is(err, ErrRecoveryCodeInvalid) {
		t.Errorf("got %v, want ErrRecoveryCodeInvalid", err)
	}
}

func TestNewWithoutALoggerStillLogs(t *testing.T) {
	svc := New(nil, config.Default(), nil, nil)
	if svc.log == nil {
		t.Fatal("a nil logger was kept, and the first warning would panic")
	}
}
