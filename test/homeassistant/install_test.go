package homeassistant_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The install procedure, asserted rather than written down.
//
// Adding this repository to Home Assistant fails with "is not a valid app
// repository" and no further explanation if the layout is wrong, and an app
// whose options do not match its start script fails later and quieter. Both
// are shape questions, which is to say both are testable without a Home
// Assistant.

// apps returns the app directories the Supervisor would find: first-level
// directories holding a config.yaml. Discovered rather than listed, so a third
// app is covered by these tests the day it appears.
func apps(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(repoRoot(t), e.Name(), "config.yaml")); err == nil {
			found = append(found, e.Name())
		}
	}
	sort.Strings(found)
	return found
}

func TestTheRepositoryIsShapedTheWayTheSupervisorReadsIt(t *testing.T) {
	root := repoRoot(t)

	// In the root, not a directory down. The Supervisor does not look deeper,
	// and this cost an evening to find out.
	var repo struct {
		Name       string `yaml:"name"`
		URL        string `yaml:"url"`
		Maintainer string `yaml:"maintainer"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, "repository.yaml"))), &repo); err != nil {
		t.Fatalf("repository.yaml: %v", err)
	}
	if repo.Name == "" || repo.URL == "" || repo.Maintainer == "" {
		t.Errorf("repository.yaml is missing something the store shows: %+v", repo)
	}

	found := apps(t)
	if len(found) < 2 {
		t.Fatalf("apps found: %v — both the plain app and the one with config access should be here", found)
	}

	for _, app := range found {
		m := manifest(t, app)
		if m.Slug == "" || m.Name == "" || m.Version == "" {
			t.Errorf("%s/config.yaml is missing a slug, name or version", app)
		}
		if m.URL != repo.URL {
			t.Errorf("%s points at %q rather than the repository's own %q", app, m.URL, repo.URL)
		}
		// Every app has to have its own start script and image wrapper next to
		// its manifest; the Supervisor builds each directory on its own.
		for _, f := range []string{"run.sh", "Dockerfile", "build.yaml", "DOCS.md"} {
			if _, err := os.Stat(filepath.Join(root, app, f)); err != nil {
				t.Errorf("%s/%s is missing", app, f)
			}
		}
	}
}

func TestTheTwoAppsDifferOnlyInWhatTheyMayRead(t *testing.T) {
	// One binary, one script, one set of options, offered twice with different
	// permissions. Anything else drifting apart is a feature one household has
	// and the other does not, discovered by whoever installed the wrong one.
	root := repoRoot(t)
	for _, f := range []string{"run.sh", "Dockerfile", "build.yaml", "translations/en.yaml"} {
		plain := readFile(t, filepath.Join(root, plainApp, f))
		variant := readFile(t, filepath.Join(root, hassConfigApp, f))
		if plain != variant {
			t.Errorf("%s differs between the two apps", f)
		}
	}

	a, b := manifest(t, plainApp), manifest(t, hassConfigApp)
	if a.Slug == b.Slug {
		t.Error("both apps have the same slug, so only one of them can be installed")
	}
	if a.Version != b.Version {
		t.Errorf("the apps are at different versions (%s, %s) for the same binary", a.Version, b.Version)
	}
	if !sameStrings(a.Arch, b.Arch) || !sameStrings(a.Services, b.Services) {
		t.Error("the apps disagree about architectures or services")
	}
	if a.HassioAPI != b.HassioAPI || a.Ingress != b.Ingress || a.IngressPort != b.IngressPort {
		t.Error("the apps disagree about how they are reached")
	}

	// The options and their schema are the user-visible surface. Identical, or
	// the same script is reading two different forms.
	if !sameYAML(t, a.Options, b.Options) {
		t.Error("the apps offer different options")
	}
	if !sameYAML(t, a.Schema, b.Schema) {
		t.Error("the apps validate their options differently")
	}

	// And the difference that is the point of the second app.
	if mapsHassConfig(t, plainApp) {
		t.Error("the plain app maps Home Assistant's configuration directory, which is the thing it is meant not to do")
	}
	if !mapsHassConfig(t, hassConfigApp) {
		t.Error("the variant does not map the configuration directory, so it is the plain app with a longer name")
	}
	for _, m := range b.Map {
		// Read-only, everywhere except the app's own configuration folder.
		// Write access to Home Assistant's configuration is not needed for
		// anything mqttview does, and asking for it would be asking for the
		// ability to edit somebody's house.
		if strings.HasPrefix(m, "homeassistant_config") && !strings.HasSuffix(m, ":ro") {
			t.Errorf("the variant asks for %q rather than read-only access", m)
		}
	}
}

var (
	// Calls, not prose: every read is a command substitution, and matching the
	// bare word finds the word "option" in the comments too.
	optionCall = regexp.MustCompile(`\$\(option(?:_list)?\s+([a-z_]+)`)
	// The schema entries the script is not expected to read, because the
	// server reads them instead. None so far; kept so adding one is a
	// deliberate line in this list rather than a silent gap.
	notReadByTheScript = map[string]bool{}
)

func TestEveryOptionIsRealInBothDirections(t *testing.T) {
	for _, app := range apps(t) {
		t.Run(app, func(t *testing.T) {
			m := manifest(t, app)
			script := readFile(t, filepath.Join(repoRoot(t), app, "run.sh"))

			read := map[string]bool{}
			for _, match := range optionCall.FindAllStringSubmatch(script, -1) {
				read[match[1]] = true
			}

			// An option the script never reads does nothing, however carefully
			// somebody fills it in.
			for key := range m.Schema {
				if !read[key] && !notReadByTheScript[key] {
					t.Errorf("the schema offers %q and the start script never reads it", key)
				}
			}
			// And an option the script reads but the schema does not declare
			// is silently always empty: the Supervisor drops what it cannot
			// validate.
			for key := range read {
				if _, ok := m.Schema[key]; !ok {
					t.Errorf("the start script reads %q, which no schema entry declares", key)
				}
			}
			// Defaults exist for what has one, so the form is filled in rather
			// than blank on first install.
			for key := range m.Options {
				if _, ok := m.Schema[key]; !ok {
					t.Errorf("%q has a default and no schema entry", key)
				}
			}
		})
	}
}

func TestABooleanOptionCanActuallyBeFalse(t *testing.T) {
	// jq's `//` treats false as absent, so `.[$key] // empty` read every
	// boolean set to false as unset — import_mqtt: false went on importing.
	// The property, rather than the fix: a boolean option set to false is read
	// as false.
	for _, app := range apps(t) {
		for _, line := range scanLines(readFile(t, filepath.Join(repoRoot(t), app, "run.sh"))) {
			// Comments are where the mistake is described; code is where it
			// would be repeated.
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if strings.Contains(line, `.[$key] // empty`) {
				t.Errorf("%s/run.sh reads options with jq's alternative operator, which cannot express false", app)
			}
		}
	}

	run := runApp(t, plainApp, withOptions(map[string]any{
		"import_mqtt":   false,
		"mqtt_insecure": false,
		"mqtt_url":      "mqtt://broker.example:1883",
	}))
	if strings.Contains(run.config, "connections:") {
		t.Errorf("import_mqtt: false was ignored:\n%s", run.config)
	}
	if strings.Contains(run.config, "insecure_skip_verify") {
		t.Errorf("mqtt_insecure: false turned into a TLS setting:\n%s", run.config)
	}
}

func TestTheAppsBuildFromAPinnedImage(t *testing.T) {
	// A tag is mutable and Docker will not re-pull one it already has, so an
	// app that says FROM hausbit/mqttview:latest can update itself into a
	// wrapper around a months-old binary. Seen in the log on a real install:
	// the app was current, the binary inside it was four commits behind.
	digest := regexp.MustCompile(`@sha256:[0-9a-f]{64}`)

	for _, app := range apps(t) {
		dockerfile := readFile(t, filepath.Join(repoRoot(t), app, "Dockerfile"))
		build := readFile(t, filepath.Join(repoRoot(t), app, "build.yaml"))

		fromDockerfile := regexp.MustCompile(`ARG MQTTVIEW_IMAGE=(\S+)`).FindStringSubmatch(dockerfile)
		fromBuild := regexp.MustCompile(`MQTTVIEW_IMAGE:\s*(\S+)`).FindStringSubmatch(build)
		if fromDockerfile == nil || fromBuild == nil {
			t.Fatalf("%s: no MQTTVIEW_IMAGE in the Dockerfile (%v) or build.yaml (%v)",
				app, fromDockerfile != nil, fromBuild != nil)
		}
		if !digest.MatchString(fromDockerfile[1]) {
			t.Errorf("%s/Dockerfile builds from %q, which is a tag and can change under it",
				app, fromDockerfile[1])
		}
		if fromDockerfile[1] != fromBuild[1] {
			t.Errorf("%s builds from %q locally and %q in build.yaml",
				app, fromDockerfile[1], fromBuild[1])
		}
	}
}

func TestEveryArchitectureTheAppsClaimIsPublished(t *testing.T) {
	// Home Assistant's names for architectures are not Docker's, and an app
	// offering one the image does not carry installs and then fails to start.
	published := map[string]string{
		"aarch64": "linux/arm64",
		"amd64":   "linux/amd64",
		"armv7":   "linux/arm/v7",
		"armhf":   "linux/arm/v6",
		"i386":    "linux/386",
	}

	workflow := readFile(t, filepath.Join(repoRoot(t), ".github", "workflows", "publish-image.yml"))
	platforms := regexp.MustCompile(`platforms:\s*(\S+)`).FindStringSubmatch(workflow)
	if platforms == nil {
		t.Fatal("the publish workflow names no platforms")
	}
	built := map[string]bool{}
	for _, p := range strings.Split(platforms[1], ",") {
		built[strings.TrimSpace(p)] = true
	}

	for _, app := range apps(t) {
		for _, arch := range manifest(t, app).Arch {
			platform, known := published[arch]
			if !known {
				t.Errorf("%s claims %q, which is not an architecture this maps", app, arch)
				continue
			}
			if !built[platform] {
				t.Errorf("%s offers %s but the image is not built for %s", app, arch, platform)
			}
		}
	}
}

func TestTheAppsAskForTheSupervisorAPITheyNeed(t *testing.T) {
	for _, app := range apps(t) {
		m := manifest(t, app)
		// Declaring `services` is not what causes SUPERVISOR_TOKEN to be
		// injected; hassio_api is. Without it the import silently found no
		// token and skipped, which is exactly what the first install did.
		if len(m.Services) > 0 && !m.HassioAPI {
			t.Errorf("%s asks for %v without hassio_api, so it will have no token to ask with", app, m.Services)
		}
		for _, s := range m.Services {
			// "want", not "need": an app that refuses to start without a broker
			// is an app somebody cannot install first and configure second.
			if s == "mqtt:need" {
				t.Errorf("%s requires an MQTT service to start", app)
			}
		}
	}
}

func TestTheDocumentedInstallStepsNameThingsThatExist(t *testing.T) {
	docs := readFile(t, filepath.Join(repoRoot(t), "docs", "HOME_ASSISTANT.md"))
	root := repoRoot(t)

	var repo struct {
		URL string `yaml:"url"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, "repository.yaml"))), &repo); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(docs, repo.URL) {
		t.Errorf("the instructions never give the repository URL %q somebody has to paste", repo.URL)
	}
	// Both apps are offered, so both have to be described where somebody
	// chooses between them.
	for _, app := range apps(t) {
		if !strings.Contains(docs, manifest(t, app).Slug) {
			t.Errorf("the instructions do not mention the %s app", app)
		}
	}
	// Add-ons were renamed to apps, and the old menu path no longer exists.
	if strings.Contains(docs, "/hassio/store") {
		t.Error("the instructions send people to /hassio/store, which Home Assistant removed")
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameYAML(t *testing.T, a, b any) bool {
	t.Helper()
	ra, err := yaml.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ra) == string(rb)
}
