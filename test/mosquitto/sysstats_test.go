package mosquitto_test

import (
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
)

// The broker statistics are read from a real broker rather than a fixture,
// because the thing most likely to be wrong is not the parsing: it is the
// assumption that $SYS looks the way the documentation says. It does not on
// every version, and "uptime" is a sentence rather than a number.
func TestTheBrokerStatisticsComeFromARealBroker(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		// Mosquitto republishes $SYS every sys_interval seconds, ten by
		// default, which is most of a test's patience spent waiting.
		b := start(t, config{extra: []string{"sys_interval 1"}})

		s := connect(t, mqttc.ConnectionSpec{
			ID: "sys", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
			SysStats: true,
		})

		// Waiting for the count to be right, not merely present: the first
		// retained burst arrives as this client subscribes, before the broker
		// has counted it, so "a number is there" is satisfied by a stale nought.
		stats := awaitStats(t, s.conn, 30*time.Second, func(st mqttc.BrokerStats) bool {
			return st.Available && st.Version != "" &&
				st.Clients.Connected != nil && *st.Clients.Connected >= 1
		})

		if stats.Version == "" {
			t.Error("the broker reported no version")
		}
		// This client is connected, so the count cannot be zero. It is the
		// assertion that catches a parse that "succeeded" into nothing.
		if *stats.Clients.Connected < 1 {
			t.Errorf("clients connected = %d while this test holds a connection open", *stats.Clients.Connected)
		}
		if !stats.HasUptime {
			t.Errorf("no uptime was parsed; the broker published %q", stats.Raw["broker/uptime"])
		}
		if stats.UpdatedAt.IsZero() {
			t.Error("no timestamp, so the UI cannot say how stale the page is")
		}
		if len(stats.Raw) == 0 {
			t.Error("nothing was carried raw, so a broker with unfamiliar names would show an empty page")
		}
	})
}

// A subscription to "#" must not bring $SYS with it: that is in the spec, and
// it is why the statistics are opt-in rather than always on.
func TestAWildcardSubscriptionDoesNotCollectTheBrokerStatistics(t *testing.T) {
	eachBroker(t, func(t *testing.T, start startFunc) {
		b := start(t, config{extra: []string{"sys_interval 1"}})

		s := connect(t, mqttc.ConnectionSpec{
			ID: "nosys", URL: b.url("mqtt"), Version: mqttc.V311, CleanStart: true,
			Subscriptions: []mqttc.Subscription{{Filter: "#", QoS: 0}},
			SysStats:      false,
		})

		// Long enough that several sys_intervals have passed.
		time.Sleep(3 * time.Second)

		if stats := s.conn.BrokerStats(); stats.Available {
			t.Errorf("$SYS arrived through a plain wildcard: %v", stats.Raw)
		}
	})
}

func awaitStats(t *testing.T, c *mqttc.Conn, within time.Duration, ready func(mqttc.BrokerStats) bool) mqttc.BrokerStats {
	t.Helper()

	deadline := time.Now().Add(within)
	var last mqttc.BrokerStats
	for time.Now().Before(deadline) {
		last = c.BrokerStats()
		if ready(last) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the broker statistics never arrived within %s; last saw available=%v raw=%d entries",
		within, last.Available, len(last.Raw))
	return last
}
