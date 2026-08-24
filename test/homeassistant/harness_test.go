// Package homeassistant_test holds component tests for the ways mqttview is
// installed, as opposed to the unit tests that live beside the code.
//
// What these exercise is everything between the code and a working panel: the
// shell script that turns an app's options into a configuration file, the
// binary reading that file, and the requests arriving the way the Supervisor
// sends them. Each of those has broken at least once on a real Home Assistant
// while every unit test passed, because none of them is Go and none of them was
// covered by anything.
//
// They are deliberately end-to-end and deliberately slow. The binary is built
// once and started for real; `go test -short` skips the lot.
//
// The one thing faked is the network position. Ingress is trusted because the
// request came from 172.30.32.2, and a test cannot come from there — so the
// walkthrough moves that address to 127.0.0.1 through the environment, and a
// separate test starts an instance with the file exactly as written to prove
// the check is doing something.
package homeassistant_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The apps this repository offers, which is what "every mode" means: one that
// can see nothing of Home Assistant beyond who you are, and one that may also
// read the configuration directory.
const (
	plainApp      = "addon"
	hassConfigApp = "addon-hassconfig"
)

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error

	repoRootOnce sync.Once
	repoRootDir  string
)

// repoRoot is the directory holding go.mod, found by walking up. Tests read
// addon/config.yaml and friends, and the working directory is this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	repoRootOnce.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			return
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				repoRootDir = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				return
			}
			dir = parent
		}
	})
	if repoRootDir == "" {
		t.Fatal("go.mod is nowhere above the working directory")
	}
	return repoRootDir
}

// binary builds cmd/mqttview once for the whole package. The real binary
// rather than an in-process server: a config file the binary refuses to load
// is the failure these tests exist to catch, and an in-process server would be
// wired up by the test rather than by main.
func binary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("component tests build and start the binary")
	}
	binaryOnce.Do(func() {
		out := filepath.Join(os.TempDir(), fmt.Sprintf("mqttview-component-%d", os.Getpid()))
		cmd := exec.Command("go", "build", "-o", out, "./cmd/mqttview")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if raw, err := cmd.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("go build: %w\n%s", err, raw)
			return
		}
		binaryPath = out
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed, and the app's run script needs it", tool)
		}
	}
}

// --------------------------------------------------------------------------
// Running an app's start script
// --------------------------------------------------------------------------

// appRun is what one start of an app produced.
type appRun struct {
	config     string // the generated mqttview.yaml, verbatim
	configPath string
	log        string // what the app would have written to the Home Assistant log
	dataDir    string
}

type runSetup struct {
	options map[string]any
	// hass is a directory standing in for Home Assistant's own configuration
	// directory. It is only reachable from an app whose manifest maps it,
	// which the harness enforces by reading the manifest rather than by being
	// told — the point of the second app is the mapping, so a test that
	// asserted the mapping itself would be asserting its own setup.
	hass string
	// bashio, when set, is a stub of the Supervisor's shell client, standing in
	// for an MQTT broker that some other app provides.
	bashio string
}

type runOption func(*runSetup)

func withOptions(o map[string]any) runOption { return func(s *runSetup) { s.options = o } }
func withHassConfig(dir string) runOption    { return func(s *runSetup) { s.hass = dir } }
func withBashio(script string) runOption     { return func(s *runSetup) { s.bashio = script } }

// runApp runs an app's run.sh the way the Supervisor would, with every
// absolute path it touches redirected into a temporary directory, and returns
// the configuration it wrote.
func runApp(t *testing.T, app string, opts ...runOption) appRun {
	t.Helper()
	requireTools(t, "bash", "jq")
	bin := binary(t)
	root := repoRoot(t)

	var setup runSetup
	for _, opt := range opts {
		opt(&setup)
	}

	work := t.TempDir()
	data := filepath.Join(work, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}

	options := setup.options
	if options == nil {
		options = map[string]any{}
	}
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "options.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Whether the configuration directory is reachable is decided by the
	// manifest, exactly as it is on a real install: a mapping is granted at
	// install time, and no option can add one.
	hass := filepath.Join(work, "not-mapped")
	if setup.hass != "" && mapsHassConfig(t, app) {
		hass = setup.hass
	}

	bashio := filepath.Join(work, "no-bashio.sh")
	if setup.bashio != "" {
		bashio = filepath.Join(work, "bashio.sh")
		if err := os.WriteFile(bashio, []byte(setup.bashio), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// One substitution per absolute path the script uses. `exec` becomes `:`,
	// the null command, so the last line is parsed and its arguments expanded —
	// a typo in them is still a failure — without starting a server the test
	// did not ask for.
	script := readFile(t, filepath.Join(root, app, "run.sh"))
	for from, to := range map[string]string{
		"/data/":                    data + "/",
		"/homeassistant/.storage":   filepath.Join(hass, ".storage"),
		"/config/.storage":          filepath.Join(hass, ".storage"),
		"/usr/lib/bashio/bashio.sh": bashio,
		"/usr/local/bin/mqttview":   bin,
	} {
		script = strings.ReplaceAll(script, from, to)
	}
	script = strings.Replace(script, "\nexec "+bin, "\n: "+bin, 1)

	path := filepath.Join(work, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", path)
	cmd.Dir = work
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s/run.sh failed: %v\n%s", app, err, stderr.String())
	}

	configPath := filepath.Join(data, "mqttview.yaml")
	return appRun{
		config:     readFile(t, configPath),
		configPath: configPath,
		log:        stderr.String(),
		dataDir:    data,
	}
}

// mapsHassConfig reports whether the app asks for read access to Home
// Assistant's configuration directory.
func mapsHassConfig(t *testing.T, app string) bool {
	t.Helper()
	for _, m := range manifest(t, app).Map {
		if strings.HasPrefix(m, "homeassistant_config") || strings.HasPrefix(m, "config:") {
			return true
		}
	}
	return false
}

// checkConfig runs the binary's own configuration check over a generated file,
// which is the same load path a start takes.
func checkConfig(t *testing.T, run appRun) (string, error) {
	t.Helper()
	cmd := exec.Command(binary(t), "-config", run.configPath, "-check-config")
	cmd.Env = append(os.Environ(), "MQTTVIEW_DATA_DIR="+run.dataDir)
	raw, err := cmd.CombinedOutput()
	return string(raw), err
}

// --------------------------------------------------------------------------
// Running the server
// --------------------------------------------------------------------------

type instance struct {
	addr    string
	url     string
	stdout  *lockedBuffer
	cmd     *exec.Cmd
	stopped sync.Once
}

// stop ends the process and waits for it, so a test that restarts against the
// same data directory is not racing the previous instance for the SQLite file.
func (i *instance) stop(t *testing.T) {
	t.Helper()
	i.stopped.Do(func() {
		_ = i.cmd.Process.Kill()
		_, _ = i.cmd.Process.Wait()
	})
}

// start runs the binary against a configuration file and waits for it to
// answer. Anything it printed is available afterwards, which is how the
// generated administrator password is read — the same way somebody reads it
// out of a container log.
func start(t *testing.T, configPath, dataDir string, env ...string) *instance {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	cmd := exec.Command(binary(t), "-config", configPath, "-log-level", "debug")
	cmd.Env = append(os.Environ(),
		"MQTTVIEW_ADDR="+addr,
		"MQTTVIEW_DATA_DIR="+dataDir,
		"MQTTVIEW_BASE_URL=http://"+addr,
	)
	cmd.Env = append(cmd.Env, env...)

	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	inst := &instance{addr: addr, url: "http://" + addr, stdout: out, cmd: cmd}
	t.Cleanup(func() {
		inst.stop(t)
		if t.Failed() {
			t.Logf("server log:\n%s", out.String())
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(inst.url + "/api/health")
		if err == nil {
			resp.Body.Close()
			return inst
		}
		if cmd.ProcessState != nil {
			t.Fatalf("the server exited before it listened:\n%s", out.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the server never answered on %s:\n%s", addr, out.String())
	return nil
}

// supervisor is a stand-in for Home Assistant's ingress proxy: it strips the
// panel prefix, adds the identity headers and forwards. Everything it does
// here it does on a real install, and nothing more — the point is that
// mqttview is talking to something that behaves like the Supervisor rather
// than to a hand-written request.
type supervisor struct {
	*httptest.Server
	prefix string
	client *http.Client
}

func fakeSupervisor(t *testing.T, inst *instance, slug, userID, userName string) *supervisor {
	t.Helper()
	target, err := url.Parse(inst.url)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/" + slug

	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, prefix)
		if r.Out.URL.Path == "" {
			r.Out.URL.Path = "/"
		}
		// What the Supervisor actually sends, and the reason the panel needs no
		// login: it has already decided who this is. Set on the outbound
		// request only — Rewrite starts from a copy with the inbound hop
		// headers already dropped, which is the same guarantee the Supervisor
		// gives: a header the caller sent cannot survive into the app.
		r.Out.Header.Set("X-Ingress-Path", prefix)
		r.Out.Header.Set("X-Remote-User-Id", userID)
		r.Out.Header.Set("X-Remote-User-Name", userName)
		r.Out.Header.Set("X-Remote-User-Display-Name", userName)
	}}

	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)

	jar := newJar()
	return &supervisor{Server: srv, prefix: prefix, client: &http.Client{Jar: jar}}
}

// do sends a request the way the browser inside the Home Assistant frame would:
// through the proxy, under the panel prefix, carrying the CSRF token from the
// cookie for anything that writes.
func (s *supervisor) do(t *testing.T, method, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+s.prefix+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := s.csrf(); token != "" && method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (s *supervisor) cookie(name string) *http.Cookie {
	u, _ := url.Parse(s.URL)
	for _, c := range s.client.Jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (s *supervisor) csrf() string {
	if c := s.cookie("mqttview_csrf"); c != nil {
		return c.Value
	}
	return ""
}

// --------------------------------------------------------------------------
// Manifests
// --------------------------------------------------------------------------

type appManifest struct {
	Name        string         `yaml:"name"`
	Version     string         `yaml:"version"`
	Slug        string         `yaml:"slug"`
	Description string         `yaml:"description"`
	URL         string         `yaml:"url"`
	Arch        []string       `yaml:"arch"`
	Ingress     bool           `yaml:"ingress"`
	IngressPort int            `yaml:"ingress_port"`
	Ports       map[string]any `yaml:"ports"`
	Image       string         `yaml:"image"`
	HassioAPI   bool           `yaml:"hassio_api"`
	Services    []string       `yaml:"services"`
	Map         []string       `yaml:"map"`
	Options     map[string]any `yaml:"options"`
	Schema      map[string]any `yaml:"schema"`
}

func manifest(t *testing.T, app string) appManifest {
	t.Helper()
	var m appManifest
	raw := readFile(t, filepath.Join(repoRoot(t), app, "config.yaml"))
	if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("%s/config.yaml: %v", app, err)
	}
	return m
}

// --------------------------------------------------------------------------
// Small helpers
// --------------------------------------------------------------------------

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var (
	portMu    sync.Mutex
	portsUsed = map[int]bool{}
)

// freePort asks the operating system for a port and refuses to hand out one
// this process has already given away. A port closed a moment ago is one the
// kernel will offer again, and two servers on one number is a test asserting
// about somebody else's process. See the same helper in test/mosquitto, where
// it cost an evening to work that out.
func freePort(t *testing.T) int {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()

	for range 100 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if !portsUsed[port] {
			portsUsed[port] = true
			return port
		}
	}
	t.Fatal("no unused port after a hundred tries")
	return 0
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decoding %s: %v\n%s", resp.Request.URL, err, raw)
	}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// waitForLine waits for something to appear in a running server's output.
// Startup work carries on after the health endpoint answers, so a line about a
// seeded connection may not be there yet when the first request succeeds.
func waitForLine(t *testing.T, inst *instance, substr string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if out := inst.stdout.String(); strings.Contains(out, substr) {
			return out
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the server never logged %q:\n%s", substr, inst.stdout.String())
	return ""
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newJar is a cookie jar that keeps whatever it is given, including cookies a
// stricter jar would drop. The panel is reached over plain HTTP as often as
// over HTTPS, and a cookie the browser would not store here is a bug the tests
// have already caught once.
func newJar() http.CookieJar { return &simpleJar{cookies: map[string]*http.Cookie{}} }

type simpleJar struct {
	mu      sync.Mutex
	cookies map[string]*http.Cookie
}

func (j *simpleJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cs {
		if c.MaxAge < 0 {
			delete(j.cookies, c.Name)
			continue
		}
		j.cookies[c.Name] = c
	}
}

func (j *simpleJar) Cookies(*url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*http.Cookie, 0, len(j.cookies))
	for _, c := range j.cookies {
		out = append(out, c)
	}
	return out
}

// scanLines is used by the tests that read something out of a log.
func scanLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}
