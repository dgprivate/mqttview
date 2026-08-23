package homeassistant_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The app's start script is the only part of mqttview written in a language
// with no compiler and no type system, and it is the part every install runs
// first. These tests run the real script with the real binary reading what it
// wrote, so a broken option name is a failing test rather than an app that
// starts with a configuration nobody asked for.

// hassStorage writes a stand-in for Home Assistant's config entry storage. The
// shape is Home Assistant's own: .storage/core.config_entries, a list of
// entries, each with a domain and a data object.
func hassStorage(t *testing.T, mqtt map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	storage := filepath.Join(dir, ".storage")
	if err := os.MkdirAll(storage, 0o755); err != nil {
		t.Fatal(err)
	}

	entries := []map[string]any{
		// Decoys, because reading "the MQTT integration" out of a file holding
		// every integration means selecting, not parsing.
		{"domain": "sun", "title": "Sun", "data": map[string]any{}},
		{"domain": "hue", "title": "Hue", "data": map[string]any{
			"host": "10.0.0.9", "api_key": "should-not-be-read",
		}},
	}
	if mqtt != nil {
		entries = append(entries, map[string]any{
			"domain": "mqtt", "title": "MQTT", "data": mqtt,
		})
	}

	raw, err := json.MarshalIndent(map[string]any{
		"version": 1, "key": "core.config_entries",
		"data": map[string]any{"entries": entries},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, "core.config_entries"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// bashioStub stands in for the Supervisor's shell client. `available` decides
// whether some other app provides a broker; the values are what
// bashio::services would print for it.
func bashioStub(available bool, values map[string]string) string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString("bashio::services.available() { ")
	if available {
		b.WriteString("return 0; }\n")
	} else {
		b.WriteString("return 1; }\n")
	}
	b.WriteString("bashio::services() {\n  case \"$2\" in\n")
	for k, v := range values {
		b.WriteString("    " + k + ") echo '" + v + "' ;;\n")
	}
	b.WriteString("    *) echo '' ;;\n  esac\n}\n")
	return b.String()
}

func TestAnAppWithNoOptionsStartsInHomeAssistantModeWithNoBroker(t *testing.T) {
	for _, app := range []string{plainApp, hassConfigApp} {
		t.Run(app, func(t *testing.T) {
			run := runApp(t, app)

			// The defaults matter more than they look: this is what somebody
			// gets who installs the app and presses Start without reading
			// anything.
			for _, want := range []string{
				"mode: ingress",
				"- 172.30.32.2",
				"default_role: operator",
				"addr: 0.0.0.0:8114",
			} {
				if !strings.Contains(run.config, want) {
					t.Errorf("the generated config has no %q:\n%s", want, run.config)
				}
			}
			if strings.Contains(run.config, "connections:") {
				t.Errorf("a broker was invented out of nothing:\n%s", run.config)
			}

			out, err := checkConfig(t, run)
			if err != nil {
				t.Fatalf("the binary rejected the config the app wrote: %v\n%s", err, out)
			}
			if !strings.Contains(out, "this configuration is valid") {
				t.Errorf("check-config said:\n%s", out)
			}
		})
	}
}

func TestAConfiguredBrokerReachesTheBinaryIntact(t *testing.T) {
	for _, app := range []string{plainApp, hassConfigApp} {
		t.Run(app, func(t *testing.T) {
			run := runApp(t, app, withOptions(map[string]any{
				"mqtt_url":      "mqtts://broker.example:8883",
				"mqtt_username": "dean",
				"mqtt_password": "p a s s", // spaces, because YAML
				"mqtt_insecure": true,
			}))

			out, err := checkConfig(t, run)
			if err != nil {
				t.Fatalf("check-config: %v\n%s", err, out)
			}
			// -check-config prints what the binary resolved, so this asserts
			// the value survived the shell, the YAML and the loader rather
			// than that the script printed a line.
			if !strings.Contains(out, "mqtts://broker.example:8883") || !strings.Contains(out, "as dean") {
				t.Errorf("the broker did not arrive:\n%s", out)
			}
			if !strings.Contains(run.config, "insecure_skip_verify: true") {
				t.Errorf("mqtt_insecure was dropped:\n%s", run.config)
			}
			if !strings.Contains(run.log, "warning") {
				t.Errorf("accepting any certificate was not warned about:\n%s", run.log)
			}
		})
	}
}

func TestTurningTheImportOffImportsNothing(t *testing.T) {
	run := runApp(t, hassConfigApp,
		withOptions(map[string]any{"import_mqtt": false}),
		withHassConfig(hassStorage(t, map[string]any{"broker": "mqtt.example", "port": 1883})),
		withBashio(bashioStub(true, map[string]string{"host": "core-mosquitto", "port": "1883"})),
	)

	if strings.Contains(run.config, "connections:") {
		t.Errorf("import_mqtt: false still added a broker:\n%s", run.config)
	}
	if !strings.Contains(run.log, "import_mqtt is off") {
		t.Errorf("nothing in the log says why:\n%s", run.log)
	}
}

func TestABrokerFromAnotherAppIsImported(t *testing.T) {
	run := runApp(t, plainApp, withBashio(bashioStub(true, map[string]string{
		"host":     "core-mosquitto",
		"port":     "1883",
		"username": "addons",
		"password": "s3cret",
		"ssl":      "false",
	})))

	if !strings.Contains(run.config, "url: mqtt://core-mosquitto:1883") {
		t.Errorf("the Mosquitto app's broker was not imported:\n%s", run.config)
	}
	if !strings.Contains(run.config, "username: addons") {
		t.Errorf("the credentials were not imported:\n%s", run.config)
	}
	if out, err := checkConfig(t, run); err != nil {
		t.Fatalf("check-config: %v\n%s", err, out)
	}
}

func TestATLSBrokerFromAnotherAppKeepsItsScheme(t *testing.T) {
	run := runApp(t, plainApp, withBashio(bashioStub(true, map[string]string{
		"host": "core-mosquitto", "port": "8883", "ssl": "true",
	})))

	if !strings.Contains(run.config, "url: mqtts://core-mosquitto:8883") {
		t.Errorf("ssl: true did not become mqtts://:\n%s", run.config)
	}
}

func TestTheMQTTIntegrationIsReadWhenTheAppMayReadIt(t *testing.T) {
	// Dean's own broker: TLS with a client certificate, which is the case that
	// has no shortcut — it cannot be retyped into a form without also moving
	// three files.
	storage := hassStorage(t, map[string]any{
		"broker":      "mqtt.black.si",
		"port":        8883,
		"username":    "dean",
		"password":    "s3cret",
		"certificate": "/ssl/ca.crt",
		"client_cert": "/ssl/client.crt",
		"client_key":  "/ssl/client.key",
	})

	run := runApp(t, hassConfigApp, withHassConfig(storage))

	for _, want := range []string{
		"url: mqtts://mqtt.black.si:8883",
		"name: mqtt.black.si",
		"username: dean",
		"password: s3cret",
		"ca_file: /ssl/ca.crt",
		"client_cert_file: /ssl/client.crt",
		"client_key_file: /ssl/client.key",
	} {
		if !strings.Contains(run.config, want) {
			t.Errorf("missing %q:\n%s", want, run.config)
		}
	}
	// The Hue entry sits in the same file. Reading one integration out of it
	// must not drag the rest along.
	if strings.Contains(run.config, "should-not-be-read") {
		t.Errorf("another integration's credentials leaked into the config:\n%s", run.config)
	}
}

func TestTheMQTTIntegrationIsUnreachableFromThePlainApp(t *testing.T) {
	// The same fixture, the same script, a different manifest. This is the
	// whole difference between the two apps, and the reason the second one
	// exists rather than an option on the first.
	storage := hassStorage(t, map[string]any{"broker": "mqtt.black.si", "port": 8883})

	run := runApp(t, plainApp, withHassConfig(storage))

	if strings.Contains(run.config, "connections:") {
		t.Fatalf("the plain app read Home Assistant's configuration:\n%s", run.config)
	}
	if !strings.Contains(run.log, "no access to Home Assistant's configuration") {
		t.Errorf("the log does not say why nothing was imported:\n%s", run.log)
	}
	// And it says what to do instead, because "nothing happened" is not an
	// answer somebody can act on.
	if !strings.Contains(run.log, "mqtt_url") {
		t.Errorf("the log offers no way forward:\n%s", run.log)
	}
}

func TestAnExplicitBrokerBeatsTheIntegration(t *testing.T) {
	storage := hassStorage(t, map[string]any{"broker": "from-storage", "port": 1883})

	run := runApp(t, hassConfigApp,
		withOptions(map[string]any{"mqtt_url": "mqtt://typed-in:1883"}),
		withHassConfig(storage),
		withBashio(bashioStub(true, map[string]string{"host": "core-mosquitto", "port": "1883"})),
	)

	if strings.Contains(run.config, "from-storage") || strings.Contains(run.config, "core-mosquitto") {
		t.Errorf("something overrode what somebody typed:\n%s", run.config)
	}
	if strings.Count(run.config, "- name:") != 1 {
		t.Errorf("more than one broker was added:\n%s", run.config)
	}
}

func TestNoMQTTIntegrationIsNotAnError(t *testing.T) {
	run := runApp(t, hassConfigApp, withHassConfig(hassStorage(t, nil)))

	if strings.Contains(run.config, "connections:") {
		t.Errorf("a broker appeared from a Home Assistant that has none:\n%s", run.config)
	}
	if !strings.Contains(run.log, "no MQTT integration") {
		t.Errorf("the log does not say what was found:\n%s", run.log)
	}
	if out, err := checkConfig(t, run); err != nil {
		t.Fatalf("check-config: %v\n%s", err, out)
	}
}

func TestTheSupervisorHavingNoBrokerFallsBackToTheIntegration(t *testing.T) {
	// The real sequence on Dean's install: bashio is there, the Supervisor has
	// no MQTT service because no app provides one, and the broker is in the
	// integration. Every step of that has to be walked before anything is
	// found.
	storage := hassStorage(t, map[string]any{"broker": "mqtt.black.si", "port": 8883})

	run := runApp(t, hassConfigApp,
		withHassConfig(storage),
		withBashio(bashioStub(false, nil)),
	)

	if !strings.Contains(run.config, "url: mqtts://mqtt.black.si:8883") {
		t.Errorf("the fallback did not run:\n%s", run.config)
	}
	if !strings.Contains(run.log, "no MQTT service to share") {
		t.Errorf("the log does not explain the fallback:\n%s", run.log)
	}
}

func TestAnIntegrationWithoutTLSStaysPlain(t *testing.T) {
	run := runApp(t, hassConfigApp, withHassConfig(hassStorage(t, map[string]any{
		"broker": "10.0.0.5", "port": 1883, "username": "ha",
	})))

	if !strings.Contains(run.config, "url: mqtt://10.0.0.5:1883") {
		t.Errorf("a plain broker was promoted to TLS:\n%s", run.config)
	}
	if strings.Contains(run.config, "ca_file") {
		t.Errorf("a certificate was invented:\n%s", run.config)
	}
}

func TestHomeAssistantsOwnCertificateBundleIsNotAFilePath(t *testing.T) {
	// The integration writes "auto" when it means the system trust store. Kept
	// as a path, mqttview would look for a file called auto and refuse to
	// start — which is exactly how an import turns into a broken install.
	run := runApp(t, hassConfigApp, withHassConfig(hassStorage(t, map[string]any{
		"broker": "mqtt.example", "port": 8883, "certificate": "auto",
	})))

	if strings.Contains(run.config, "ca_file") {
		t.Errorf(`"auto" was written out as a file:\n%s`, run.config)
	}
	if !strings.Contains(run.config, "url: mqtts://mqtt.example:8883") {
		t.Errorf("the scheme was not inferred from the certificate:\n%s", run.config)
	}
	if out, err := checkConfig(t, run); err != nil {
		t.Fatalf("the binary refused a config built from a real integration: %v\n%s", err, out)
	}
}

func TestAdminUsersAndFrameAncestorsAreListsInTheConfig(t *testing.T) {
	run := runApp(t, plainApp, withOptions(map[string]any{
		"admin_users":     []string{"dean", "9f3c1e"},
		"frame_ancestors": []string{"https://ha.example"},
		"fallback_user":   "shared",
		"default_role":    "viewer",
		"log_level":       "debug",
	}))

	for _, want := range []string{
		"admin_users:",
		"- dean",
		"- 9f3c1e",
		"frame_ancestors:",
		"- https://ha.example",
		"fallback_user: shared",
		"default_role: viewer",
	} {
		if !strings.Contains(run.config, want) {
			t.Errorf("missing %q:\n%s", want, run.config)
		}
	}
	if out, err := checkConfig(t, run); err != nil {
		t.Fatalf("check-config: %v\n%s", err, out)
	}
}

func TestTheAppLogsTheVersionOfTheBinaryItIsAboutToStart(t *testing.T) {
	// A wrapper around a cached image was invisible until this line existed:
	// the app said it had updated and the binary inside it was four commits
	// old. The log is where somebody looks first, so the version goes there.
	run := runApp(t, plainApp)

	var versionLine string
	for _, line := range scanLines(run.log) {
		if strings.Contains(line, "binary:") {
			versionLine = line
		}
	}
	if versionLine == "" {
		t.Fatalf("nothing in the log says which binary this is:\n%s", run.log)
	}
	if strings.Contains(versionLine, "unknown") {
		t.Errorf("the binary would not say what it is: %q", versionLine)
	}
}
