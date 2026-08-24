package mosquitto_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// Every transport and protocol version mqttview offers in the connection form,
// against the broker most people point it at. If one of these fails, somebody
// picked that option in the UI and it did not work.

var versions = []struct {
	name string
	v    mqttc.Version
}{
	{"MQTT 3.1", mqttc.V31},
	{"MQTT 3.1.1", mqttc.V311},
	{"MQTT 5.0", mqttc.V5},
}

// roundTrip is the same proof of life for every mode: subscribe, publish,
// receive what was published. Anything less does not show the transport works
// in both directions.
func roundTrip(t *testing.T, spec mqttc.ConnectionSpec) {
	t.Helper()
	topic := "mqttview/test/" + strings.ReplaceAll(t.Name(), "/", "-")
	spec.CleanStart = true
	spec.Subscriptions = []mqttc.Subscription{{Filter: topic, QoS: 1}}

	s := connect(t, spec)
	s.publish(t, mqttc.PublishRequest{Topic: topic, Payload: []byte("hello"), QoS: 1})

	if got := string(s.await(t, topic, 15*time.Second).Payload); got != "hello" {
		t.Errorf("payload = %q", got)
	}
}

func TestPlainTCP(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		b := start(t, config{})
		for _, v := range versions {
			t.Run(v.name, func(t *testing.T) {
				roundTrip(t, mqttc.ConnectionSpec{URL: b.url("mqtt"), Version: v.v})
			})
		}
	})
}

func TestWebSockets(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		// The transport people reach for when a broker is behind a reverse proxy
		// that only speaks HTTP.
		b := start(t, config{websockets: true})
		for _, v := range versions {
			t.Run(v.name, func(t *testing.T) {
				roundTrip(t, mqttc.ConnectionSpec{URL: b.url("ws"), Version: v.v})
			})
		}
	})
}

func TestUnixSocket(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		// A broker on the same machine, with no port open at all. mqttview offers
		// the scheme, so it has to work.
		b := start(t, config{unixSocket: true})
		roundTrip(t, mqttc.ConnectionSpec{URL: b.url("unix"), Version: mqttc.V311})
	})
}

func TestUsernameAndPassword(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		b := start(t, config{users: map[string]string{"dean": "s3cret"}})

		for _, v := range versions {
			t.Run(v.name, func(t *testing.T) {
				roundTrip(t, mqttc.ConnectionSpec{
					URL:      b.url("mqtt"),
					Version:  v.v,
					Username: "dean",
					Password: "s3cret",
				})
			})
		}

		t.Run("a wrong password is refused", func(t *testing.T) {
			err := tryConnect(t, mqttc.ConnectionSpec{
				ID: "wrong", URL: b.url("mqtt"), Version: mqttc.V311,
				Username: "dean", Password: "not-it",
			})
			if err == nil {
				t.Fatal("the broker accepted a wrong password")
			}
			// The error is what somebody sees in the UI, and "connection refused"
			// for a bad password sends them to check their firewall.
			if !mentionsAny(err.Error(), "auth", "credential", "password", "not authorized", "bad user") {
				t.Errorf("the error does not say the credentials were wrong: %v", err)
			}
		})

		t.Run("no credentials at all is refused", func(t *testing.T) {
			if err := tryConnect(t, mqttc.ConnectionSpec{
				ID: "anon", URL: b.url("mqtt"), Version: mqttc.V311,
			}); err == nil {
				t.Fatal("a broker with allow_anonymous false accepted an anonymous client")
			}
		})
	})
}

func TestTLS(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		b := start(t, config{tls: true})
		pki := testPKI(t)

		for _, v := range versions {
			t.Run(v.name, func(t *testing.T) {
				roundTrip(t, mqttc.ConnectionSpec{
					URL:     b.url("mqtts"),
					Version: v.v,
					TLS:     mqttc.TLSSpec{CAPEM: pki.caPEM},
				})
			})
		}

		t.Run("an unknown CA is refused", func(t *testing.T) {
			// The whole value of TLS is here. A client that trusts a certificate
			// signed by nobody it knows is a client with a decorative padlock.
			err := tryConnect(t, mqttc.ConnectionSpec{
				ID: "otherca", URL: b.url("mqtts"), Version: mqttc.V311,
				TLS: mqttc.TLSSpec{CAPEM: pki.otherCAPEM},
			})
			if err == nil {
				t.Fatal("a certificate from an unrelated CA was accepted")
			}
			if !mentionsAny(err.Error(), "certificate", "x509", "tls", "verif") {
				t.Errorf("the error does not point at the certificate: %v", err)
			}
		})

		t.Run("the system trust store does not include our test CA", func(t *testing.T) {
			// No CAPEM at all: this must fail, or the CAPEM cases above prove
			// nothing.
			if err := tryConnect(t, mqttc.ConnectionSpec{
				ID: "noca", URL: b.url("mqtts"), Version: mqttc.V311,
			}); err == nil {
				t.Fatal("a self-issued certificate was accepted against the system roots")
			}
		})

		t.Run("insecure_skip_verify connects anyway", func(t *testing.T) {
			// The option exists because home brokers with self-signed certificates
			// are the common case, and the UI marks a connection using it.
			roundTrip(t, mqttc.ConnectionSpec{
				ID: "insecure", URL: b.url("mqtts"), Version: mqttc.V311,
				TLS: mqttc.TLSSpec{InsecureSkipVerify: true},
			})
		})

		t.Run("a name that is not in the certificate is refused", func(t *testing.T) {
			// The certificate carries this broker's own address and localhost.
			// Reaching it by another name has to fail, or hostname
			// verification is not happening.
			err := tryConnect(t, mqttc.ConnectionSpec{
				ID: "sni", URL: b.url("mqtts"), Version: mqttc.V311,
				TLS: mqttc.TLSSpec{CAPEM: pki.caPEM, ServerName: "not-the-broker.example"},
			})
			if err == nil {
				t.Fatal("a certificate for a different name was accepted")
			}
		})

		t.Run("ServerName lets a certificate be verified by its own name", func(t *testing.T) {
			// The reason the option exists: a broker reached at an address its
			// certificate does not carry, but a name it does.
			roundTrip(t, mqttc.ConnectionSpec{
				ID: "sni-ok", URL: b.url("mqtts"), Version: mqttc.V311,
				TLS: mqttc.TLSSpec{CAPEM: pki.caPEM, ServerName: "localhost"},
			})
		})

		t.Run("TLS 1.3 only", func(t *testing.T) {
			roundTrip(t, mqttc.ConnectionSpec{
				ID: "tls13", URL: b.url("mqtts"), Version: mqttc.V311,
				TLS: mqttc.TLSSpec{CAPEM: pki.caPEM, MinVersion: "1.3"},
			})
		})
	})
}

func TestMutualTLS(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		// Dean's own broker wants a client certificate, and so do most brokers
		// belonging to anybody who set one up on purpose.
		b := start(t, config{tls: true, requireCert: true})
		pki := testPKI(t)

		t.Run("with a client certificate", func(t *testing.T) {
			roundTrip(t, mqttc.ConnectionSpec{
				URL: b.url("mqtts"), Version: mqttc.V5,
				TLS: mqttc.TLSSpec{
					CAPEM:         pki.caPEM,
					ClientCertPEM: pki.clientCertPEM,
					ClientKeyPEM:  pki.clientKeyPEM,
				},
			})
		})

		t.Run("without one, the broker hangs up", func(t *testing.T) {
			if err := tryConnect(t, mqttc.ConnectionSpec{
				ID: "nocert", URL: b.url("mqtts"), Version: mqttc.V311,
				TLS: mqttc.TLSSpec{CAPEM: pki.caPEM},
			}); err == nil {
				t.Fatal("a broker requiring a client certificate accepted a client without one")
			}
		})
	})
}

func TestSecureWebSockets(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		b := start(t, config{websockets: true, tls: true})
		pki := testPKI(t)

		roundTrip(t, mqttc.ConnectionSpec{
			URL: b.url("wss"), Version: mqttc.V5,
			TLS: mqttc.TLSSpec{CAPEM: pki.caPEM},
		})
	})
}

func mentionsAny(s string, words ...string) bool {
	s = strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(s, strings.ToLower(w)) {
			return true
		}
	}
	return false
}
