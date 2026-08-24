package homeassistant_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mqttview ships in two shapes and the difference is who decides who you are:
// standalone, where it decides, and Home Assistant, where it has already been
// decided before the request arrives. Both are walked here against the real
// binary, because the second one is a header away from being an MQTT console
// that lets the caller pick their own username.

// standaloneConfig writes the smallest configuration somebody running the
// binary or the container would have.
func standaloneConfig(t *testing.T) (configPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mqttview.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  mode: standalone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

var passwordLine = regexp.MustCompile(`password:\s+(\S+)`)

func TestStandaloneWantsALoginAndPrintsOneOnce(t *testing.T) {
	configPath, dataDir := standaloneConfig(t)
	inst := start(t, configPath, dataDir)

	// Nothing without a session. This is the mode where mqttview is the only
	// thing standing between a browser and somebody's house.
	resp, err := http.Get(inst.url + "/api/connections")
	if err != nil {
		t.Fatal(err)
	}
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusUnauthorized {
		t.Fatalf("the connection list answered %d without a login", status)
	}

	out := waitForLine(t, inst, "created the first administrator")
	m := passwordLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no password was printed for the account that was created:\n%s", out)
	}

	login, err := json.Marshal(map[string]string{"email": "admin@localhost", "password": m[1]})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Post(inst.url+"/api/auth/login", "application/json", strings.NewReader(string(login)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the printed password does not work: %d %s", resp.StatusCode, body(t, resp))
	}
}

// ingressInstance starts the binary on the configuration an app's run.sh
// actually wrote, with the trusted proxy moved to this host — the one thing a
// test cannot reproduce, since 172.30.32.2 is the Supervisor's address on Home
// Assistant's own network. That the address in the file is load-bearing is
// asserted separately, below.
func ingressInstance(t *testing.T, app string, opts ...runOption) (*instance, appRun) {
	t.Helper()
	run := runApp(t, app, opts...)
	inst := start(t, run.configPath, run.dataDir,
		"MQTTVIEW_INGRESS_TRUSTED_PROXIES=127.0.0.1")
	return inst, run
}

func TestNothingButTheSupervisorGetsIn(t *testing.T) {
	// The configuration exactly as the app wrote it, with no override: the
	// Supervisor is at an address nothing in this test can send from.
	run := runApp(t, plainApp)
	inst := start(t, run.configPath, run.dataDir)

	req, err := http.NewRequest(http.MethodGet, inst.url+"/api/auth/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Everything an attacker would send, which is everything the Supervisor
	// sends. The only thing they cannot forge is where the packet came from.
	req.Header.Set("X-Remote-User-Id", "attacker")
	req.Header.Set("X-Remote-User-Name", "dean")
	req.Header.Set("X-Forwarded-For", "172.30.32.2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("forged identity headers were accepted: %s", body(t, resp))
	}
}

func TestThePanelNeedsNoLogin(t *testing.T) {
	inst, _ := ingressInstance(t, plainApp)
	ha := fakeSupervisor(t, inst, "53379db9_mqttview", "9f3c1e", "dean")

	var me struct {
		Email, Name, Provider, Role string
	}
	decode(t, ha.do(t, http.MethodGet, "/api/auth/me", nil), &me)

	if me.Provider != "homeassistant" {
		t.Errorf("signed in through %q rather than Home Assistant: %+v", me.Provider, me)
	}
	if me.Name != "dean" {
		t.Errorf("the panel does not know who this is: %+v", me)
	}
	// Whoever opens the panel first is the administrator. Home Assistant does
	// not say who its administrators are, and an install where nobody can add
	// a broker is an install that does nothing.
	if me.Role != "admin" {
		t.Errorf("the first person to open the panel is not an administrator: %+v", me)
	}
}

func TestTheSecondPersonThroughIsNotAnAdministrator(t *testing.T) {
	inst, _ := ingressInstance(t, plainApp)

	first := fakeSupervisor(t, inst, "mqttview", "9f3c1e", "dean")
	firstResp := first.do(t, http.MethodGet, "/api/auth/me", nil)
	firstResp.Body.Close()

	second := fakeSupervisor(t, inst, "mqttview", "aa11bb", "guest")
	var me struct{ Name, Role string }
	decode(t, second.do(t, http.MethodGet, "/api/auth/me", nil), &me)

	if me.Name != "guest" {
		t.Fatalf("the second person was not recognised: %+v", me)
	}
	// default_role in the app's options, which is what the generated
	// configuration says and what every subsequent household member gets.
	if me.Role != "operator" {
		t.Errorf("the second person got %q rather than the default role", me.Role)
	}
}

func TestWritingWorksOverPlainHTTP(t *testing.T) {
	// Home Assistant is reached over http at home as often as over https from
	// outside, and a Secure cookie set over one is not sent over the other. A
	// panel that accepts writes only on whichever address happened to come
	// first is the bug this asserts against; it was reported from Dean's own
	// install as "CSRF token missing or invalid".
	inst, _ := ingressInstance(t, plainApp)
	ha := fakeSupervisor(t, inst, "mqttview", "9f3c1e", "dean")

	resp := ha.do(t, http.MethodGet, "/api/auth/me", nil)
	resp.Body.Close()

	cookie := ha.cookie("mqttview_csrf")
	if cookie == nil {
		t.Fatal("no CSRF cookie was set, so nothing can ever be written")
	}
	if cookie.Secure {
		t.Error("the CSRF cookie is Secure, so it will not come back over plain http")
	}

	// With the token: accepted. The body is nonsense on purpose — this asserts
	// the request got past CSRF, not that it made a connection.
	write := ha.do(t, http.MethodPost, "/api/connections", strings.NewReader(`{}`))
	status := write.StatusCode
	write.Body.Close()
	if status == http.StatusForbidden {
		t.Errorf("a write carrying the CSRF token was refused: %d", status)
	}

	// Without it: refused. Otherwise any page in any tab could post here, and
	// the identity is a header the browser adds for free.
	req, err := http.NewRequest(http.MethodPost, ha.URL+ha.prefix+"/api/connections", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	bare, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Body.Close()
	if bare.StatusCode != http.StatusForbidden {
		t.Errorf("a write without the CSRF header was not refused: %d", bare.StatusCode)
	}
}

func TestThePanelIsServedUnderTheIngressPath(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot(t), "web", "dist", "index.html")); err != nil {
		t.Skip("the frontend has not been built, so there is no page to serve")
	}

	inst, _ := ingressInstance(t, plainApp)
	slug := "53379db9_mqttview"
	ha := fakeSupervisor(t, inst, slug, "9f3c1e", "dean")

	page := body(t, ha.do(t, http.MethodGet, "/", nil))
	// Without this the browser resolves every asset against the Home Assistant
	// root and the panel is a blank frame — which is what an ingress app looks
	// like when it has not been told where it lives.
	if !strings.Contains(page, `<base href="/`+slug+`/"`) {
		t.Errorf("the page has no base tag for the panel path:\n%s", firstLines(page, 20))
	}

	// A route the router owns rather than a file, which the server has to
	// answer with the page rather than a 404.
	deep := ha.do(t, http.MethodGet, "/connections", nil)
	status := deep.StatusCode
	deep.Body.Close()
	if status != http.StatusOK {
		t.Errorf("a link straight to a panel route answered %d", status)
	}
}

func TestTheRunningVersionIsWhatTheAppSaidItStarted(t *testing.T) {
	// "Is this actually the version I just installed?" was unanswerable until
	// both of these existed. They come from different places — the app's log
	// line runs the binary with -version, the panel reads it over HTTP — so
	// they agree only if it really is one binary.
	inst, run := ingressInstance(t, plainApp)
	ha := fakeSupervisor(t, inst, "mqttview", "9f3c1e", "dean")

	var health struct{ Version string }
	decode(t, ha.do(t, http.MethodGet, "/api/health", nil), &health)
	if health.Version == "" {
		t.Fatal("the panel cannot say which version it is")
	}
	if !strings.Contains(run.log, health.Version) {
		t.Errorf("the app logged a different version than the panel reports (%q):\n%s",
			health.Version, run.log)
	}
}

func TestAnImportedBrokerAppearsOnceAndStaysThatWayAcrossRestarts(t *testing.T) {
	// Seeding runs on every start. Adding the same broker again on each one, or
	// overwriting an edit somebody made in the panel, are both worse than not
	// importing at all.
	// Seeded the only way there is now: from the broker Home Assistant itself
	// provides, which is what the Supervisor tells the app about.
	run := runApp(t, plainApp, withBashio(bashioStub(true, map[string]string{
		"host": "core-mosquitto", "port": "1883", "username": "addons", "ssl": "false",
	})))

	count := func() int {
		inst := start(t, run.configPath, run.dataDir, "MQTTVIEW_INGRESS_TRUSTED_PROXIES=127.0.0.1")
		ha := fakeSupervisor(t, inst, "mqttview", "9f3c1e", "dean")
		var list []struct{ Name, URL string }
		decode(t, ha.do(t, http.MethodGet, "/api/connections", nil), &list)
		for _, c := range list {
			// tcp:// rather than mqtt://: the store keeps the scheme the client
			// library uses, and the two mean the same broker.
			if !strings.Contains(c.URL, "core-mosquitto:1883") {
				t.Errorf("an unexpected connection: %+v", c)
			}
		}
		inst.stop(t)
		return len(list)
	}

	if got := count(); got != 1 {
		t.Fatalf("the first start seeded %d connections, want 1", got)
	}
	if got := count(); got != 1 {
		t.Fatalf("a restart left %d connections, want 1", got)
	}
}

func TestTheAppNeverPublishesAPort(t *testing.T) {
	// Ingress is the only door. A published port would be a second one, held
	// shut by nothing but the source check, forever.
	for _, app := range []string{plainApp, hassConfigApp} {
		m := manifest(t, app)
		if len(m.Ports) != 0 {
			t.Errorf("%s publishes ports: %v", app, m.Ports)
		}
		if !m.Ingress {
			t.Errorf("%s is not an ingress app, so it would need a login", app)
		}
	}
}

func firstLines(s string, n int) string {
	lines := scanLines(s)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
