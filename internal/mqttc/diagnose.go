package mqttc

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ConnectError wraps a raw connect failure with a hint about what to change.
// Broker errors arrive as bare strings from three different stacks (Go's TLS,
// the OS dialer, the MQTT CONNACK), and "context deadline exceeded" on its own
// tells a user nothing about which of them gave up.
type ConnectError struct {
	// URL is the broker the attempt was made against.
	URL string
	// Err is the underlying failure, kept for errors.Is/As.
	Err error
	// Hint is a human-readable next step, empty when we have nothing to add.
	Hint string
}

func (e *ConnectError) Error() string {
	msg := fmt.Sprintf("connect to %s: %v", e.URL, e.Err)
	if e.Hint != "" {
		msg += " — " + e.Hint
	}
	return msg
}

func (e *ConnectError) Unwrap() error { return e.Err }

// explainConnectError annotates a failed connect with the most likely cause.
// It never hides the original error: the hint is appended, not substituted.
func explainConnectError(spec ConnectionSpec, err error) error {
	if err == nil {
		return nil
	}
	var already *ConnectError
	if errors.As(err, &already) {
		return err
	}
	return &ConnectError{URL: spec.URL, Err: err, Hint: connectHint(spec, err)}
}

func connectHint(spec ConnectionSpec, err error) string {
	if h := tlsHint(spec, err); h != "" {
		return h
	}
	if h := networkHint(spec, err); h != "" {
		return h
	}
	return connackHint(err)
}

func tlsHint(spec ConnectionSpec, err error) string {
	// Checked before the typed cases below: Go reports a certificate with no
	// SAN as an x509.HostnameError, whose generic "set a server name" advice
	// is wrong here — no server name makes a CN-only certificate verify.
	if strings.Contains(err.Error(), "legacy Common Name field") {
		return "the broker's certificate has no Subject Alternative Name, only a Common Name, and Go refuses those " +
			"even with the right CA; reissue the certificate with a SAN, or enable Skip certificate verification"
	}

	var unknownCA x509.UnknownAuthorityError
	if errors.As(err, &unknownCA) {
		return "the broker's certificate is not signed by a CA this server trusts; " +
			"paste the CA certificate under TLS → CA certificate, or enable Skip certificate verification"
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return fmt.Sprintf("the certificate is not valid for %q; set TLS → Server name to a name the certificate covers, "+
			"or enable Skip certificate verification", hostErr.Host)
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
		return "the broker's certificate has expired; renew it on the broker"
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "tls: certificate required"):
		return "the broker requires a client certificate (mutual TLS); supply one under TLS → Client certificate and key"
	case strings.Contains(msg, "tls: bad certificate"), strings.Contains(msg, "tls: unknown certificate"):
		return "the broker rejected our client certificate; check it is signed by the CA the broker trusts and has not expired"
	case strings.Contains(msg, "first record does not look like a TLS handshake"):
		return "that port speaks plain MQTT, not TLS; use an mqtt:// URL or point mqtts:// at the TLS port (usually 8883)"
	case strings.Contains(msg, "protocol version not supported"), strings.Contains(msg, "handshake failure"):
		return "TLS handshake was refused; the broker may require a different TLS version or cipher"
	case spec.UsesTLS() && strings.Contains(msg, "connection reset by peer"):
		// A broker with require_certificate often drops the socket rather than
		// sending a TLS alert, so the reset is all we get to work with.
		return "the broker closed the TLS connection without an alert; requiring a client certificate (mutual TLS) " +
			"is the usual reason, so try supplying one under TLS → Client certificate and key"
	case !spec.UsesTLS() && (strings.Contains(msg, "connection reset by peer") || strings.Contains(msg, "EOF")):
		return "the broker closed the connection immediately, which usually means it expects TLS; try mqtts:// on port 8883"
	}
	return ""
}

func networkHint(spec ConnectionSpec, err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return fmt.Sprintf("hostname %q did not resolve; check the name and this server's DNS", dns.Name)
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Sprintf("nothing is listening on %s; check the port and that the broker's listener is bound to an "+
			"address reachable from this server", hostPort(spec.URL))
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return fmt.Sprintf("%s is filtered or unroutable from this server; a firewall between the two is the usual cause", hostPort(spec.URL))
	case strings.Contains(msg, "i/o timeout"), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("no reply from %s within the connect timeout; the packets are most likely being dropped by a firewall", hostPort(spec.URL))
	}
	return ""
}

func connackHint(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "bad user name or password"), strings.Contains(msg, "bad username or password"):
		return "the broker rejected the credentials; check the username and password"
	case strings.Contains(msg, "not authorized"), strings.Contains(msg, "not authorised"):
		return "the broker accepted the connection but refused this client; check its ACL for the username or client ID"
	case strings.Contains(msg, "identifier rejected"):
		return "the broker rejected the client ID; try a different one"
	case strings.Contains(msg, "server unavailable"):
		return "the broker is up but not accepting connections yet"
	}
	return ""
}

// hostPort renders the broker address for a message, falling back to the whole
// URL when it cannot be parsed.
func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}
