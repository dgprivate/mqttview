package mqttc

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// The manager's bookkeeping — what counts as a change worth reconnecting for,
// what a disconnect does to intent, what the history ring keeps — is decided
// without a broker, so it is tested without one.

func specFor(url string) ConnectionSpec {
	return ConnectionSpec{ID: "c1", Name: "c1", URL: url, Version: V311}
}

func TestNeedsRestartOnlyForChangesThatAffectTheSession(t *testing.T) {
	base := specFor("mqtt://broker:1883")
	base.Username = "user"
	base.Password = "pass"
	base.KeepAlive = 30
	if err := base.Normalize(); err != nil {
		t.Fatal(err)
	}

	m := NewManager(nil)
	c, err := m.Upsert(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}

	cosmetic := base
	cosmetic.Name = "renamed"
	cosmetic.AutoConnect = !base.AutoConnect
	cosmetic.HistorySize = base.HistorySize + 10
	if c.needsRestart(cosmetic) {
		t.Error("renaming a connection would drop the session")
	}

	// Subscriptions change without a reconnect: that is the whole reason the
	// distinction exists.
	subs := base
	subs.Subscriptions = []Subscription{{Filter: "a/#", QoS: 1}}
	if c.needsRestart(subs) {
		t.Error("adding a subscription would reconnect")
	}

	for _, tt := range []struct {
		name string
		fix  func(*ConnectionSpec)
	}{
		{"url", func(s *ConnectionSpec) { s.URL = "mqtt://elsewhere:1883" }},
		{"version", func(s *ConnectionSpec) { s.Version = V5 }},
		{"username", func(s *ConnectionSpec) { s.Username = "other" }},
		{"password", func(s *ConnectionSpec) { s.Password = "other" }},
		{"client id", func(s *ConnectionSpec) { s.ClientID = "other" }},
		{"keep alive", func(s *ConnectionSpec) { s.KeepAlive = 60 }},
		{"clean start", func(s *ConnectionSpec) { s.CleanStart = !s.CleanStart }},
		{"tls verification", func(s *ConnectionSpec) { s.TLS.InsecureSkipVerify = true }},
		{"tls ca", func(s *ConnectionSpec) { s.TLS.CAPEM = "x" }},
		{"tls client cert", func(s *ConnectionSpec) { s.TLS.ClientCertPEM = "x" }},
		{"server name", func(s *ConnectionSpec) { s.TLS.ServerName = "other" }},
		{"will", func(s *ConnectionSpec) { s.Will = &Will{Topic: "bye", Payload: "x"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			tt.fix(&changed)
			if !c.needsRestart(changed) {
				t.Errorf("changing the %s would not reconnect, so it would not take effect", tt.name)
			}
		})
	}
}

func TestWillChangedComparesContentNotPointers(t *testing.T) {
	a := &Will{Topic: "t", Payload: "p", QoS: 1, Retain: true}
	b := &Will{Topic: "t", Payload: "p", QoS: 1, Retain: true}

	if willChanged(a, b) {
		t.Error("two identical wills compared as different")
	}
	if willChanged(nil, nil) {
		t.Error("no will on either side compared as a change")
	}
	if !willChanged(a, nil) || !willChanged(nil, a) {
		t.Error("adding or removing a will is a change")
	}
	c := &Will{Topic: "other", Payload: "p", QoS: 1, Retain: true}
	if !willChanged(a, c) {
		t.Error("a different topic is a change")
	}
}

func TestDisconnectClearsTheIntentToBeConnected(t *testing.T) {
	m := NewManager(nil)
	spec := specFor("mqtt://127.0.0.1:1")
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := m.Upsert(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	if c.wantsConnection() {
		t.Fatal("a connection that has never been asked to connect wants to be")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Nothing is listening, so this fails — but the intent is recorded, which
	// is what the auto-connect supervisor reads.
	_ = m.Connect(ctx, "c1")
	if !c.wantsConnection() {
		t.Error("a failed connect did not record the intent to retry")
	}

	if err := m.Disconnect(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if c.wantsConnection() {
		t.Error("disconnecting by hand did not stop the retry loop")
	}
}

func TestManagerLookupsAndRemoval(t *testing.T) {
	m := NewManager(nil)
	spec := specFor("mqtt://broker:1883")
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	if _, ok := m.Get("c1"); !ok {
		t.Fatal("the connection is not registered")
	}
	if len(m.List()) != 1 {
		t.Fatalf("List = %d", len(m.List()))
	}
	if _, ok := m.Get("nope"); ok {
		t.Error("an unknown id resolved")
	}

	// Operations on an unknown connection report it rather than panicking.
	if err := m.Connect(context.Background(), "nope"); err == nil {
		t.Error("connecting an unknown id succeeded")
	}
	if err := m.Disconnect(context.Background(), "nope"); err == nil {
		t.Error("disconnecting an unknown id succeeded")
	}
	if err := m.Publish(context.Background(), "nope", PublishRequest{Topic: "a"}); err == nil {
		t.Error("publishing to an unknown id succeeded")
	}

	if err := m.Remove(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("c1"); ok {
		t.Error("the connection survived removal")
	}
}

func TestUpsertRejectsAnUnusableSpec(t *testing.T) {
	m := NewManager(nil)

	for _, spec := range []ConnectionSpec{
		{ID: "a", Name: "", URL: "mqtt://h:1883"},
		{ID: "a", Name: "n", URL: ""},
		{ID: "a", Name: "n", URL: "gopher://h:70"},
	} {
		if _, err := m.Upsert(context.Background(), spec); err == nil {
			t.Errorf("accepted %+v", spec)
		}
	}
}

func TestObserversSeeMessagesAndStopWhenRemoved(t *testing.T) {
	m := NewManager(nil)

	var seen int
	remove := m.AddObserver(Observer{
		OnMessage: func(Message) { seen++ },
	})
	m.emitMessage(Message{Topic: "a"})
	if seen != 1 {
		t.Fatalf("the observer saw %d messages", seen)
	}

	remove()
	m.emitMessage(Message{Topic: "b"})
	if seen != 1 {
		t.Errorf("a removed observer still saw %d", seen)
	}
}

func TestHistoryKeepsTheNewestAndClears(t *testing.T) {
	h := NewHistory(3)

	for i := 0; i < 5; i++ {
		h.Add(Message{Topic: "a", Seq: uint64(i)})
	}
	got := h.Recent(10, "")
	if len(got) != 3 {
		t.Fatalf("history holds %d, want 3", len(got))
	}
	// A ring keeps the newest and hands them back newest first, which is the
	// order a live stream is read in.
	if got[0].Seq != 4 {
		t.Errorf("the first message is %d, want the newest", got[0].Seq)
	}
	for _, m := range got {
		if m.Seq < 2 {
			t.Errorf("message %d should have been evicted", m.Seq)
		}
	}

	h.Clear()
	if len(h.Recent(10, "")) != 0 {
		t.Error("history survived a clear")
	}
}

func TestConnStatusStartsDisconnected(t *testing.T) {
	m := NewManager(nil)
	spec := specFor("mqtt://broker:1883")
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := m.Upsert(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	st := c.Status()
	if st.State != StateDisconnected || st.ConnectionID != "c1" {
		t.Fatalf("status = %+v", st)
	}
	if st.Version != V311.String() {
		t.Errorf("version = %q", st.Version)
	}
}

func TestSubscribeValidatesFilters(t *testing.T) {
	m := NewManager(nil)
	spec := specFor("mqtt://broker:1883")
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.Upsert(context.Background(), spec)

	if err := c.Subscribe(context.Background(), []Subscription{{Filter: "a/#/b"}}); err == nil {
		t.Error("an invalid filter was accepted")
	}
	if err := c.Subscribe(context.Background(), []Subscription{{Filter: "a/#", QoS: 9}}); err == nil {
		t.Error("an out-of-range QoS was accepted")
	}
	// Valid ones are recorded even while disconnected, and applied on connect.
	if err := c.Subscribe(context.Background(), []Subscription{{Filter: "a/#", QoS: 1}}); err != nil {
		t.Fatalf("a valid subscription was refused: %v", err)
	}
	if len(c.Spec().Subscriptions) != 1 {
		t.Error("the subscription was not persisted on the spec")
	}
}

func TestStartAutoConnectSkipsWhatIsNotAskedFor(t *testing.T) {
	m := NewManager(nil)
	spec := specFor("mqtt://127.0.0.1:1")
	spec.AutoConnect = false
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, _ := m.Upsert(context.Background(), spec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartAutoConnect(ctx)

	time.Sleep(100 * time.Millisecond)
	if c.wantsConnection() {
		t.Error("a connection with autoConnect off was connected anyway")
	}
}

// TestShutdownStopsTheRetryLoops is a regression test for a leak that took a
// long time to find: it showed up as a data race inside the test broker,
// attributed to whichever test happened to be running when a stray retry from
// an earlier one landed on a reused port.
//
// The property is simple and worth having on its own: after Shutdown returns,
// nothing is still trying to connect. In production that is the difference
// between SIGTERM meaning "stopped" and meaning "asked to stop".
func TestShutdownStopsTheRetryLoops(t *testing.T) {
	m := NewManager(nil)

	// A broker that is not there, so the supervisor keeps retrying rather than
	// succeeding and returning on its own.
	spec := ConnectionSpec{
		ID: "gone", Name: "gone", URL: "mqtt://127.0.0.1:1", Version: V311,
		AutoConnect: true,
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	m.StartAutoConnect(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m.Shutdown(ctx)

	// Shutdown waits, so this is a statement about what has already happened
	// rather than a poll with a sleep in it.
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Errorf("goroutines went from %d to %d across StartAutoConnect and Shutdown", before, after)
	}
}

func TestStartingAutoConnectTwiceDoesNotDoubleUp(t *testing.T) {
	m := NewManager(nil)

	spec := ConnectionSpec{
		ID: "gone", Name: "gone", URL: "mqtt://127.0.0.1:1", Version: V311,
		AutoConnect: true,
	}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upsert(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	// Reloading the configuration calls this again. The second call has to
	// replace the first, not run alongside it: two loops dialling the same
	// broker halve the effective backoff every reload.
	m.StartAutoConnect(context.Background())
	m.StartAutoConnect(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m.Shutdown(ctx)

	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines went from %d to %d", before, after)
	}
}
