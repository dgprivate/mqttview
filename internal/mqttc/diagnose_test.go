package mqttc

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestExplainConnectErrorAddsHint(t *testing.T) {
	tests := []struct {
		name string
		url  string
		err  error
		want string
	}{
		{
			name: "unknown authority",
			url:  "mqtts://broker:8883",
			err:  x509.UnknownAuthorityError{},
			want: "CA certificate",
		},
		{
			name: "common name only",
			url:  "mqtts://broker:8883",
			err:  errors.New("tls: failed to verify certificate: x509: certificate relies on legacy Common Name field, use SANs instead"),
			want: "Subject Alternative Name",
		},
		{
			name: "mutual tls required",
			url:  "mqtts://broker:8883",
			err:  errors.New("network Error : remote error: tls: certificate required"),
			want: "mutual TLS",
		},
		{
			name: "tls socket reset",
			url:  "mqtts://broker:8883",
			err:  errors.New("write tcp 10.0.0.2:45038->10.0.0.1:8883: write: connection reset by peer"),
			want: "mutual TLS",
		},
		{
			name: "tls url on plaintext port",
			url:  "mqtts://broker:1883",
			err:  errors.New("tls: first record does not look like a TLS handshake"),
			want: "speaks plain MQTT",
		},
		{
			name: "connection refused",
			url:  "mqtt://broker:1883",
			err:  errors.New("dial tcp 10.0.0.1:1883: connect: connection refused"),
			want: "broker:1883",
		},
		{
			name: "filtered by firewall",
			url:  "mqtt://broker:1883",
			err:  errors.New("dial tcp 10.0.0.1:1883: connect: no route to host"),
			want: "firewall",
		},
		{
			name: "deadline exceeded",
			url:  "mqtt://broker:1883",
			err:  context.DeadlineExceeded,
			want: "connect timeout",
		},
		{
			name: "dns failure",
			url:  "mqtt://nope:1883",
			err:  &net.DNSError{Name: "nope", IsNotFound: true},
			want: "did not resolve",
		},
		{
			name: "bad credentials",
			url:  "mqtt://broker:1883",
			err:  errors.New("Connection Refused: Bad user name or password"),
			want: "username and password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ConnectionSpec{Name: "t", URL: tt.url}
			if err := spec.Normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			got := explainConnectError(spec, tt.err).Error()
			if !strings.Contains(got, tt.want) {
				t.Errorf("hint missing %q in: %s", tt.want, got)
			}
			// The raw cause must survive: the hint is additive, never a swap.
			// Normalize rewrites the scheme (mqtt -> tcp), so compare against
			// the normalized URL rather than the one that went in.
			if !strings.Contains(got, spec.URL) {
				t.Errorf("broker URL missing in: %s", got)
			}
		})
	}
}

func TestExplainConnectErrorPreservesCause(t *testing.T) {
	spec := ConnectionSpec{Name: "t", URL: "mqtt://broker:1883"}
	if err := spec.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if explainConnectError(spec, nil) != nil {
		t.Fatal("nil error must stay nil")
	}

	wrapped := explainConnectError(spec, context.DeadlineExceeded)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("errors.Is must still match the underlying cause")
	}
	// Re-explaining must not stack hints on top of each other.
	if again := explainConnectError(spec, wrapped); again.Error() != wrapped.Error() {
		t.Errorf("double explain changed the message: %s", again)
	}
}

func TestExplainConnectErrorWithoutHint(t *testing.T) {
	spec := ConnectionSpec{Name: "t", URL: "mqtt://broker:1883"}
	if err := spec.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	got := explainConnectError(spec, errors.New("something unmapped")).Error()
	if want := "connect to tcp://broker:1883: something unmapped"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
