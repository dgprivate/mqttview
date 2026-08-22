package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 specifies HMAC-SHA1; authenticator apps implement that
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP as specified in RFC 6238, written out rather than pulled in.
//
// It is sixty lines of well-specified arithmetic, and the alternative is a
// third-party dependency on the one code path that decides whether somebody is
// who they say they are. SHA-1 is not a choice here: RFC 6238 names it and
// every authenticator app implements it, so a different hash would produce
// codes no phone can generate.

const (
	// totpDigits is what every authenticator app expects.
	totpDigits = 6
	// totpPeriod is the RFC 6238 default step.
	totpPeriod = 30 * time.Second
	// totpSkew is how many steps either side of now are accepted. One step
	// covers a phone whose clock is off by up to thirty seconds, which is
	// common; more than that starts widening the window an attacker has.
	totpSkew = 1
	// totpSecretBytes is 160 bits, the size RFC 4226 recommends for the key.
	totpSecretBytes = 20
)

// ErrInvalidTOTP is returned for a code that does not match.
var ErrInvalidTOTP = errors.New("auth: the six-digit code is not valid")

// base32NoPad is the alphabet authenticator apps read, without padding: some
// readers reject the "=" characters.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh base32 secret to hand to an authenticator app.
func NewTOTPSecret() (string, error) {
	buf := make([]byte, totpSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate TOTP secret: %w", err)
	}
	return base32NoPad.EncodeToString(buf), nil
}

// totpCode computes the code for one time step.
func totpCode(secret string, counter uint64) (string, error) {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("auth: TOTP secret is not valid base32: %w", err)
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// RFC 4226 dynamic truncation: the low nibble of the last byte picks where
	// to read the four-byte value from.
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, value%mod), nil
}

// TOTPCodeAt returns the code a correctly configured authenticator would show
// for this secret at this instant.
//
// Exported so a test can act as the phone, and so an operator can check an
// enrolment without one. It grants nothing that holding the secret does not.
func TOTPCodeAt(secret string, at time.Time) (string, error) {
	return totpCode(secret, uint64(at.Unix())/uint64(totpPeriod.Seconds()))
}

// VerifyTOTP reports whether code is valid for secret at the given time.
//
// The comparison is constant-time. A code is six digits, so a timing oracle
// would narrow a million possibilities to very few.
func VerifyTOTP(secret, code string, now time.Time) error {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return ErrInvalidTOTP
	}

	counter := uint64(now.Unix()) / uint64(totpPeriod.Seconds())
	for skew := -totpSkew; skew <= totpSkew; skew++ {
		c := counter
		switch {
		case skew < 0:
			step := uint64(-skew)
			if c < step {
				continue
			}
			c -= step
		case skew > 0:
			c += uint64(skew)
		}

		want, err := totpCode(secret, c)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return nil
		}
	}
	return ErrInvalidTOTP
}

// TOTPURI builds the otpauth:// URI an authenticator app scans.
//
// The issuer appears both in the label and as a parameter, which is what the
// apps that disagree about where to look each need.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(int(totpPeriod.Seconds())))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
