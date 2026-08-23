package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dgprivate/mqttview/internal/store"
)

var (
	// ErrTwoFactorRequired is returned by Login when the password was right but
	// a second factor is still needed.
	ErrTwoFactorRequired = errors.New("auth: a second factor is required")
	// ErrTwoFactorNotPending is returned when confirming an enrolment that was
	// never started.
	ErrTwoFactorNotPending = errors.New("auth: no two-factor enrolment is in progress")
	// ErrTwoFactorAlreadyOn is returned when enrolling an account that has it.
	ErrTwoFactorAlreadyOn = errors.New("auth: two-factor is already enabled; turn it off first")
	// ErrRecoveryCodeInvalid is returned for a code that does not match an
	// unused one.
	ErrRecoveryCodeInvalid = errors.New("auth: that recovery code is not valid")
)

const (
	// recoveryCodeCount is how many are issued at enrolment.
	recoveryCodeCount = 10
	// recoveryCodeBytes gives 80 bits per code, which is far beyond guessing
	// and still short enough to write down.
	recoveryCodeBytes = 10
)

// recoveryAlphabet is Crockford-style base32 without the characters people
// misread when copying a code off paper.
var recoveryAlphabet = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

// TwoFactorStatus is what the account page shows.
type TwoFactorStatus struct {
	Enabled bool `json:"enabled"`
	// Pending is true when a secret has been issued but not yet confirmed.
	Pending             bool       `json:"pending"`
	ConfirmedAt         *time.Time `json:"confirmedAt,omitempty"`
	RecoveryCodesLeft   int        `json:"recoveryCodesLeft"`
	RequiredByPolicy    bool       `json:"requiredByPolicy"`
	RequiredForThisUser bool       `json:"requiredForThisUser"`
}

// Enrolment is handed back once, when a secret is issued.
type Enrolment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

// TwoFactorStatusFor describes where an account stands.
func (s *Service) TwoFactorStatusFor(u store.User) (TwoFactorStatus, error) {
	left, err := s.store.CountUnusedRecoveryCodes(u.ID)
	if err != nil {
		return TwoFactorStatus{}, err
	}
	return TwoFactorStatus{
		Enabled:             u.TwoFactorEnabled(),
		Pending:             u.TOTPSecretEnc != "" && u.TOTPConfirmedAt == nil,
		ConfirmedAt:         u.TOTPConfirmedAt,
		RecoveryCodesLeft:   left,
		RequiredByPolicy:    s.cfg.RequireTwoFactor,
		RequiredForThisUser: s.TwoFactorRequired(u),
	}, nil
}

// TwoFactorRequired reports whether this account must have a second factor.
//
// The policy applies to local accounts only. An SSO account authenticates at
// the identity provider, which is where a second factor belongs for it;
// demanding one here as well would mean enrolling every SSO user in mqttview
// for no additional assurance.
func (s *Service) TwoFactorRequired(u store.User) bool {
	return s.cfg.RequireTwoFactor && u.Provider == store.ProviderLocal
}

// BeginTwoFactorEnrolment issues a secret and returns what an authenticator
// needs. Nothing is enforced until ConfirmTwoFactorEnrolment succeeds, so a
// person who closes the page halfway is not locked out.
func (s *Service) BeginTwoFactorEnrolment(u store.User) (Enrolment, error) {
	if u.TwoFactorEnabled() {
		return Enrolment{}, ErrTwoFactorAlreadyOn
	}

	secret, err := NewTOTPSecret()
	if err != nil {
		return Enrolment{}, err
	}
	enc, err := s.box.Seal(secret)
	if err != nil {
		return Enrolment{}, fmt.Errorf("auth: encrypt TOTP secret: %w", err)
	}
	if err := s.store.SetTOTPSecret(u.ID, enc); err != nil {
		return Enrolment{}, err
	}

	return Enrolment{Secret: secret, URI: TOTPURI(s.issuerName(), u.Email, secret)}, nil
}

// ConfirmTwoFactorEnrolment checks a code against the pending secret and, if it
// matches, turns two-factor on and issues recovery codes. The plaintext codes
// are returned once and only their hashes are kept.
func (s *Service) ConfirmTwoFactorEnrolment(u store.User, code string) ([]string, error) {
	if u.TwoFactorEnabled() {
		return nil, ErrTwoFactorAlreadyOn
	}
	if u.TOTPSecretEnc == "" {
		return nil, ErrTwoFactorNotPending
	}

	secret, err := s.box.Open(u.TOTPSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("auth: decrypt TOTP secret: %w", err)
	}
	if err := VerifyTOTP(secret, code, time.Now()); err != nil {
		return nil, err
	}
	if err := s.store.ConfirmTOTP(u.ID, time.Now()); err != nil {
		return nil, err
	}

	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := s.store.ReplaceRecoveryCodes(u.ID, hashes); err != nil {
		return nil, err
	}
	s.log.Info("two-factor enabled", "user", u.Email)
	return codes, nil
}

// RegenerateRecoveryCodes issues a fresh set, invalidating the old ones.
func (s *Service) RegenerateRecoveryCodes(u store.User) ([]string, error) {
	if !u.TwoFactorEnabled() {
		return nil, ErrTwoFactorNotPending
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := s.store.ReplaceRecoveryCodes(u.ID, hashes); err != nil {
		return nil, err
	}
	s.log.Info("recovery codes regenerated", "user", u.Email)
	return codes, nil
}

// DisableTwoFactor turns it off for a user.
//
// The caller decides who is allowed to ask: a person turning off their own
// needs their password, and an administrator resetting somebody who lost their
// phone does not.
func (s *Service) DisableTwoFactor(u store.User) error {
	if err := s.store.DisableTOTP(u.ID); err != nil {
		return err
	}
	s.log.Info("two-factor disabled", "user", u.Email)
	return nil
}

// VerifySecondFactor accepts either a TOTP code or a recovery code.
//
// A recovery code is consumed on use. The two are told apart by shape rather
// than by asking the user which they typed: a TOTP code is six digits and a
// recovery code is not.
func (s *Service) VerifySecondFactor(ctx context.Context, u store.User, code string) error {
	code = strings.TrimSpace(code)
	if !u.TwoFactorEnabled() {
		return ErrTwoFactorNotPending
	}

	if len(code) == totpDigits && isAllDigits(code) {
		secret, err := s.box.Open(u.TOTPSecretEnc)
		if err != nil {
			return fmt.Errorf("auth: decrypt TOTP secret: %w", err)
		}
		return VerifyTOTP(secret, code, time.Now())
	}
	return s.useRecoveryCode(ctx, u, code)
}

func (s *Service) useRecoveryCode(_ context.Context, u store.User, code string) error {
	hashes, err := s.store.RecoveryCodeHashes(u.ID)
	if err != nil {
		return err
	}

	want := hashRecoveryCode(code)
	// Every candidate is compared, and in constant time, so neither the number
	// of remaining codes nor which one matched is visible in the timing.
	matched := ""
	for id, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			matched = id
		}
	}
	if matched == "" {
		return ErrRecoveryCodeInvalid
	}
	if err := s.store.UseRecoveryCode(matched, time.Now()); err != nil {
		// Another request spent it first.
		return ErrRecoveryCodeInvalid
	}
	s.log.Info("recovery code used", "user", u.Email)
	return nil
}

// newRecoveryCodes returns the plaintext codes and the hashes to store.
func newRecoveryCodes() (codes []string, hashes []string, err error) {
	codes = make([]string, 0, recoveryCodeCount)
	hashes = make([]string, 0, recoveryCodeCount)

	for i := 0; i < recoveryCodeCount; i++ {
		buf := make([]byte, recoveryCodeBytes)
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, fmt.Errorf("auth: generate recovery code: %w", err)
		}
		raw := recoveryAlphabet.EncodeToString(buf)
		// Grouped for copying off a screen onto paper.
		code := raw[:4] + "-" + raw[4:8] + "-" + raw[8:]
		codes = append(codes, code)
		hashes = append(hashes, hashRecoveryCode(code))
	}
	return codes, hashes, nil
}

// hashRecoveryCode normalises and hashes a code.
//
// Plain SHA-256 rather than argon2: a recovery code is eighty bits of entropy
// this server generated, not a password a person chose, so there is nothing for
// a slow hash to defend against and a sign-in should not cost a second.
func hashRecoveryCode(code string) string {
	norm := strings.ToLower(strings.TrimSpace(code))
	norm = strings.ReplaceAll(norm, "-", "")
	norm = strings.ReplaceAll(norm, " ", "")
	sum := sha256.Sum256([]byte("mqttview-recovery:" + norm))
	return hex.EncodeToString(sum[:])
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// issuerName is what an authenticator app shows beside the account.
func (s *Service) issuerName() string {
	if s.cfg.TwoFactorIssuer != "" {
		return s.cfg.TwoFactorIssuer
	}
	return "mqttview"
}
