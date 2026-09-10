package mqttc

import (
	"testing"
	"time"
)

func sysValue(topic, payload string, at time.Time) TopicValue {
	return TopicValue{Topic: topic, Payload: []byte(payload), UpdatedAt: at}
}

// A broker with no clients and a broker that will not say are different
// facts, and a page of zeroes cannot tell them apart.
func TestABrokerThatPublishesNothingIsReportedAsUnavailableNotAsZero(t *testing.T) {
	stats := brokerStatsFrom(nil)

	if stats.Available {
		t.Error("no $SYS values at all was reported as available")
	}
	if stats.Clients.Connected != nil {
		t.Errorf("a client count of %d was invented out of no data", *stats.Clients.Connected)
	}
}

func TestTheCommonMosquittoLayoutIsUnderstood(t *testing.T) {
	now := time.Now()
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/version", "mosquitto version 2.0.22", now),
		sysValue("$SYS/broker/uptime", "98765 seconds", now),
		sysValue("$SYS/broker/clients/connected", "7", now),
		sysValue("$SYS/broker/clients/total", "9", now),
		sysValue("$SYS/broker/clients/maximum", "12", now),
		sysValue("$SYS/broker/messages/received", "1000", now),
		sysValue("$SYS/broker/messages/sent", "2000", now),
		sysValue("$SYS/broker/messages/stored", "44", now),
		sysValue("$SYS/broker/bytes/received", "555", now),
		sysValue("$SYS/broker/bytes/sent", "666", now),
		sysValue("$SYS/broker/subscriptions/count", "31", now),
		sysValue("$SYS/broker/retained messages/count", "17", now),
		sysValue("$SYS/broker/heap/current", "4096", now),
	})

	if !stats.Available {
		t.Fatal("values arrived but the stats are marked unavailable")
	}
	if stats.Version != "mosquitto version 2.0.22" {
		t.Errorf("version = %q", stats.Version)
	}
	if !stats.HasUptime || stats.Uptime != 98765 {
		t.Errorf("uptime = %d (present: %v), want 98765 — it is published as %q, not a bare number",
			stats.Uptime, stats.HasUptime, "98765 seconds")
	}

	for _, c := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"clients connected", stats.Clients.Connected, 7},
		{"clients total", stats.Clients.Total, 9},
		{"clients maximum", stats.Clients.Maximum, 12},
		{"messages received", stats.Messages.Received, 1000},
		{"messages sent", stats.Messages.Sent, 2000},
		{"messages stored", stats.Messages.Stored, 44},
		{"bytes received", stats.Bytes.Received, 555},
		{"bytes sent", stats.Bytes.Sent, 666},
		{"subscriptions", stats.Subscriptions, 31},
		{"retained", stats.Retained, 17},
		{"heap current", stats.Heap.Current, 4096},
	} {
		if c.got == nil {
			t.Errorf("%s is absent", c.name)
			continue
		}
		if *c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, *c.got, c.want)
		}
	}
}

// The load averages are the broker's own. mqttview reports them rather than
// computing rates of its own to sit beside them.
func TestTheBrokersOwnLoadAveragesAreCarriedThroughUnchanged(t *testing.T) {
	now := time.Now()
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/load/messages/received/1min", "12.50", now),
		sysValue("$SYS/broker/load/messages/received/5min", "10.25", now),
		sysValue("$SYS/broker/load/bytes/sent/15min", "900", now),
	})

	if got := stats.Load["messages/received/1min"]; got != 12.5 {
		t.Errorf("1min load = %v, want 12.5", got)
	}
	if got := stats.Load["messages/received/5min"]; got != 10.25 {
		t.Errorf("5min load = %v, want 10.25", got)
	}
	if got := stats.Load["bytes/sent/15min"]; got != 900 {
		t.Errorf("15min bytes = %v, want 900", got)
	}
}

// Nothing is hidden behind the typed fields: a broker with its own names still
// shows what it published.
func TestEveryValueIsAlsoCarriedRawSoAnUnknownBrokerStillShowsSomething(t *testing.T) {
	now := time.Now()
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/connection/mybridge/state", "1", now),
		sysValue("$SYS/some-other-broker/queue/depth", "5", now),
	})

	if !stats.Available {
		t.Fatal("values arrived but the stats are marked unavailable")
	}
	if got := stats.Raw["broker/connection/mybridge/state"]; got != "1" {
		t.Errorf("raw bridge state = %q, want %q", got, "1")
	}
	if got := stats.Raw["some-other-broker/queue/depth"]; got != "5" {
		t.Errorf("raw value from an unrecognised layout = %q, want %q", got, "5")
	}
}

// A confident zero is a lie about a broker that published something else.
func TestAValueThatIsNotANumberLeavesTheFieldAbsentRatherThanZero(t *testing.T) {
	now := time.Now()
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/clients/connected", "unknown", now),
		sysValue("$SYS/broker/uptime", "a while", now),
	})

	if stats.Clients.Connected != nil {
		t.Errorf("client count = %d, want absent: the broker published %q", *stats.Clients.Connected, "unknown")
	}
	if stats.HasUptime {
		t.Errorf("uptime = %d, want absent: the broker published %q", stats.Uptime, "a while")
	}
	// But it is still visible as published.
	if got := stats.Raw["broker/clients/connected"]; got != "unknown" {
		t.Errorf("raw value = %q, want it kept verbatim", got)
	}
}

func TestACounterPublishedAsAFloatIsStillRead(t *testing.T) {
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/messages/received", "1024.0", time.Now()),
	})

	if stats.Messages.Received == nil {
		t.Fatal("a counter published as a float was dropped")
	}
	if *stats.Messages.Received != 1024 {
		t.Errorf("messages received = %d, want 1024", *stats.Messages.Received)
	}
}

// The UI says how stale the page is rather than implying it is live.
func TestUpdatedAtIsTheNewestValueOnThePage(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/clients/connected", "1", base),
		sysValue("$SYS/broker/messages/sent", "2", base.Add(30*time.Second)),
		sysValue("$SYS/broker/messages/received", "3", base.Add(10*time.Second)),
	})

	if !stats.UpdatedAt.Equal(base.Add(30 * time.Second)) {
		t.Errorf("updatedAt = %v, want the newest value's time %v", stats.UpdatedAt, base.Add(30*time.Second))
	}
}

func TestTheAlternativeSpellingsBrokersUseAreAccepted(t *testing.T) {
	now := time.Now()
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/clients/active", "4", now),
		sysValue("$SYS/broker/clients/inactive", "2", now),
		sysValue("$SYS/broker/store/messages/count", "8", now),
		sysValue("$SYS/broker/publish/messages/dropped", "3", now),
		sysValue("$SYS/broker/heap/maximum size", "9000", now),
	})

	for _, c := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"clients/active as connected", stats.Clients.Connected, 4},
		{"clients/inactive as disconnected", stats.Clients.Disconnected, 2},
		{"store/messages/count as stored", stats.Messages.Stored, 8},
		{"publish/messages/dropped as dropped", stats.Messages.Dropped, 3},
		{"heap/maximum size", stats.Heap.Maximum, 9000},
	} {
		if c.got == nil {
			t.Errorf("%s was not recognised", c.name)
			continue
		}
		if *c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, *c.got, c.want)
		}
	}
}

// $SYS is a reserved namespace that a plain wildcard does not reach, which is
// exactly why the subscription has to be asked for by name.
func TestAPlainWildcardDoesNotReachTheReservedNamespace(t *testing.T) {
	if MatchFilter("#", "$SYS/broker/uptime") {
		t.Error(`"#" matched a $SYS topic, so the statistics would arrive without anybody asking`)
	}
	if !MatchFilter(SysFilter, "$SYS/broker/uptime") {
		t.Errorf("%q did not match a $SYS topic, so the statistics would never arrive", SysFilter)
	}
}

func TestTheSysSubscriptionIsHeldOnlyWhenItIsWanted(t *testing.T) {
	c := newConn(NewManager(nil), ConnectionSpec{
		ID:            "c1",
		Subscriptions: []Subscription{{Filter: "home/#", QoS: 0}},
	})

	if hasFilter(c.wantedSubscriptions(), SysFilter) {
		t.Error("$SYS is subscribed to even though the statistics are off")
	}
	if c.wantsSys() {
		t.Error("wantsSys is true with the toggle off and no user subscription")
	}

	c.setSpec(ConnectionSpec{
		ID:            "c1",
		Subscriptions: []Subscription{{Filter: "home/#", QoS: 0}},
		SysStats:      true,
	})
	if !hasFilter(c.wantedSubscriptions(), SysFilter) {
		t.Error("the statistics are on but $SYS is not subscribed to")
	}
}

// Turning the toggle off must not drop a subscription the user asked for
// themselves: it is theirs, and the statistics page is not the only reason to
// want the namespace.
func TestAUserOwnedSysSubscriptionSurvivesTheToggleGoingOff(t *testing.T) {
	c := newConn(NewManager(nil), ConnectionSpec{
		ID:            "c1",
		Subscriptions: []Subscription{{Filter: SysFilter, QoS: 0}},
		SysStats:      false,
	})

	if !c.wantsSys() {
		t.Error("a subscription the user wrote themselves was treated as droppable")
	}
}

func hasFilter(subs []Subscription, filter string) bool {
	for _, s := range subs {
		if s.Filter == filter {
			return true
		}
	}
	return false
}

// Found against a real Mosquitto, not in a fixture. Mosquitto publishes
// clients/connected and clients/active as separate topics, and around a
// connection their values differ for an instant. Treating them as synonyms and
// taking whichever was walked last reported nought clients connected while the
// test that found this held a connection open.
func TestWhenABrokerPublishesBothNamesTheCanonicalOneWins(t *testing.T) {
	now := time.Now()

	// Both orderings, because the bug was that the order decided the answer.
	for _, name := range []string{"canonical first", "alias first"} {
		t.Run(name, func(t *testing.T) {
			values := []TopicValue{
				sysValue("$SYS/broker/clients/connected", "1", now),
				sysValue("$SYS/broker/clients/active", "0", now),
			}
			if name == "alias first" {
				values[0], values[1] = values[1], values[0]
			}

			stats := brokerStatsFrom(values)
			if stats.Clients.Connected == nil {
				t.Fatal("no client count at all")
			}
			if *stats.Clients.Connected != 1 {
				t.Errorf("clients connected = %d, want 1 from clients/connected regardless of walk order",
					*stats.Clients.Connected)
			}
		})
	}
}

// A key that is present but unparseable must not shadow a later one that
// works, or a broker publishing an empty canonical topic hides a good value.
func TestAnUnreadableNameFallsThroughToTheNextCandidate(t *testing.T) {
	now := time.Now()
	stats := brokerStatsFrom([]TopicValue{
		sysValue("$SYS/broker/messages/stored", "", now),
		sysValue("$SYS/broker/store/messages/count", "12", now),
	})

	if stats.Messages.Stored == nil {
		t.Fatal("an empty canonical topic hid the usable alternative")
	}
	if *stats.Messages.Stored != 12 {
		t.Errorf("messages stored = %d, want 12", *stats.Messages.Stored)
	}
}
