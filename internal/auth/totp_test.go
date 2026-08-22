package auth

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B publishes test vectors. They use a twenty-byte ASCII
// secret and eight digits; mqttview issues six, so the expected values here are
// the last six digits of the published ones — the truncation is the same
// arithmetic either way.
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	// "12345678901234567890" in base32, which is the RFC's SHA-1 seed.
	secret := base32NoPad.EncodeToString([]byte("12345678901234567890"))

	tests := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}

	for _, tt := range tests {
		counter := uint64(tt.unix) / uint64(totpPeriod.Seconds())
		got, err := totpCode(secret, counter)
		if err != nil {
			t.Fatalf("unix %d: %v", tt.unix, err)
		}
		if got != tt.want {
			t.Errorf("unix %d: got %s, want %s", tt.unix, got, tt.want)
		}
	}
}

func TestVerifyTOTPAcceptsTheCurrentCode(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)

	code, err := totpCode(secret, uint64(now.Unix())/uint64(totpPeriod.Seconds()))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTOTP(secret, code, now); err != nil {
		t.Fatalf("the current code was rejected: %v", err)
	}
	// Padding and stray whitespace are what a person actually types.
	if err := VerifyTOTP(secret, " "+code+" ", now); err != nil {
		t.Errorf("a code with whitespace was rejected: %v", err)
	}
}

func TestVerifyTOTPToleratesOneStepOfClockDrift(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1700000000, 0)

	for _, drift := range []time.Duration{-totpPeriod, 0, totpPeriod} {
		at := base.Add(drift)
		code, err := totpCode(secret, uint64(at.Unix())/uint64(totpPeriod.Seconds()))
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyTOTP(secret, code, base); err != nil {
			t.Errorf("a code %v out was rejected: %v", drift, err)
		}
	}

	// Two steps is outside the window: that is the point of having one.
	at := base.Add(2 * totpPeriod)
	code, err := totpCode(secret, uint64(at.Unix())/uint64(totpPeriod.Seconds()))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTOTP(secret, code, base); err == nil {
		t.Error("a code two steps out was accepted")
	}
}

func TestVerifyTOTPRejectsRubbish(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)

	for _, code := range []string{"", "12345", "1234567", "abcdef", "000000", "      "} {
		if err := VerifyTOTP(secret, code, now); err == nil {
			t.Errorf("accepted %q", code)
		}
	}
	if err := VerifyTOTP("not base32!", "123456", now); err == nil {
		t.Error("accepted a code against a malformed secret")
	}
}

func TestTOTPURIIsScannable(t *testing.T) {
	uri := TOTPURI("mqttview", "dean@example.com", "JBSWY3DPEHPK3PXP")

	for _, want := range []string{
		"otpauth://totp/",
		"secret=JBSWY3DPEHPK3PXP",
		"issuer=mqttview",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI is missing %q: %s", want, uri)
		}
	}
	// The label is "issuer:account". The colon is the separator the Key URI
	// format defines, so it stays literal; only characters inside either part
	// are escaped.
	if !strings.Contains(uri, "totp/mqttview:dean@example.com?") {
		t.Errorf("label is not the issuer:account form: %s", uri)
	}
}

func TestNewTOTPSecretIsUniqueAndUsable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := NewTOTPSecret()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatal("generated the same secret twice")
		}
		seen[s] = true

		if _, err := base32NoPad.DecodeString(s); err != nil {
			t.Fatalf("secret is not decodable base32: %v", err)
		}
		if _, err := totpCode(s, 1); err != nil {
			t.Fatalf("secret does not produce a code: %v", err)
		}
	}
}
