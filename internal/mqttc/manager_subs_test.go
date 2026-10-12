package mqttc_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/testutil"
)

// connected brings a connection up against a fresh broker and waits until the
// broker holds wantFilter, so what follows is measured against a settled
// session rather than one still subscribing.
func connected(t *testing.T, spec mqttc.ConnectionSpec, wantFilter string) (*testutil.Broker, *mqttc.Manager, *mqttc.Conn, context.Context) {
	t.Helper()
	broker := brokerFor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	mgr := mqttc.NewManager(nil)
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })

	spec.URL = broker.URL
	conn, err := mgr.Upsert(ctx, spec)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mgr.Connect(ctx, spec.ID); err != nil {
		t.Fatalf("connect: %v", err)
	}
	testutil.WaitFor(t, 10*time.Second, "the broker to hold "+wantFilter, func() bool {
		return broker.HasSubscription(wantFilter)
	})
	return broker, mgr, conn, ctx
}

// A plugin that subscribed to a filter the user also subscribed to, and then
// lets go of it, must not take the user's subscription away with it. The
// broker holds one subscription per filter, so unsubscribing it on the
// plugin's behalf would silently stop the user's messages too.
func TestReleasingAPluginFilterKeepsTheUsersOwnSubscription(t *testing.T) {
	for _, version := range []mqttc.Version{mqttc.V311, mqttc.V5} {
		t.Run(fmt.Sprintf("mqtt %d", version), func(t *testing.T) {
			broker, _, conn, ctx := connected(t, mqttc.ConnectionSpec{
				ID: "c", Name: "c", Version: version, CleanStart: true,
				Subscriptions: []mqttc.Subscription{{Filter: "shared/#", QoS: 0}},
			}, "shared/#")

			if err := conn.SubscribeEphemeral(ctx, []mqttc.Subscription{
				{Filter: "shared/#", QoS: 0}, {Filter: "plugin/#", QoS: 0},
			}); err != nil {
				t.Fatalf("ephemeral subscribe: %v", err)
			}
			testutil.WaitFor(t, 10*time.Second, "the plugin's filter to reach the broker", func() bool {
				return broker.HasSubscription("plugin/#")
			})

			if err := conn.UnsubscribeEphemeral(ctx, []string{"shared/#", "plugin/#"}); err != nil {
				t.Fatalf("ephemeral unsubscribe: %v", err)
			}

			// Unsubscribe waits for the broker's acknowledgement, so by now
			// the broker has acted on it.
			if broker.HasSubscription("plugin/#") {
				t.Error("the plugin's own filter is still subscribed after it let go of it")
			}
			if !broker.HasSubscription("shared/#") {
				t.Error("releasing the plugin's filter took the user's identical subscription with it")
			}
		})
	}
}

// Editing the subscriptions of a connection that is up applies them to the
// live session: the person who added a filter expects its messages now, not
// after the next reconnect.
func TestAddingASubscriptionToALiveConnectionTakesEffect(t *testing.T) {
	for _, version := range []mqttc.Version{mqttc.V311, mqttc.V5} {
		t.Run(fmt.Sprintf("mqtt %d", version), func(t *testing.T) {
			spec := mqttc.ConnectionSpec{
				ID: "c", Name: "c", Version: version, CleanStart: true,
				Subscriptions: []mqttc.Subscription{{Filter: "first/#", QoS: 0}},
			}
			broker, mgr, _, ctx := connected(t, spec, "first/#")

			spec.URL = broker.URL
			spec.Subscriptions = append(spec.Subscriptions, mqttc.Subscription{Filter: "second/#", QoS: 0})
			if _, err := mgr.Upsert(ctx, spec); err != nil {
				t.Fatalf("upsert: %v", err)
			}

			if !broker.HasSubscription("second/#") {
				t.Error("a filter added to a live connection was not subscribed")
			}
		})
	}
}

// Switching the broker statistics off has to reach the broker. Subscribing
// only ever adds, so without an explicit unsubscribe $SYS keeps arriving and
// the page that was switched off carries on filling the topic tree.
func TestTurningBrokerStatisticsOffUnsubscribesFromSYS(t *testing.T) {
	for _, version := range []mqttc.Version{mqttc.V311, mqttc.V5} {
		t.Run(fmt.Sprintf("mqtt %d", version), func(t *testing.T) {
			spec := mqttc.ConnectionSpec{
				ID: "c", Name: "c", Version: version, CleanStart: true, SysStats: true,
				Subscriptions: []mqttc.Subscription{{Filter: "user/#", QoS: 0}},
			}
			broker, mgr, _, ctx := connected(t, spec, mqttc.SysFilter)

			spec.URL = broker.URL
			spec.SysStats = false
			if _, err := mgr.Upsert(ctx, spec); err != nil {
				t.Fatalf("upsert: %v", err)
			}

			if broker.HasSubscription(mqttc.SysFilter) {
				t.Error("$SYS is still subscribed after broker statistics were switched off")
			}
			if !broker.HasSubscription("user/#") {
				t.Error("switching statistics off dropped the user's own subscription")
			}
		})
	}
}

// A user who subscribed to $SYS themselves keeps it when the statistics page
// is switched off: it is their subscription, not the toggle's.
func TestAUsersOwnSYSSubscriptionSurvivesTheStatisticsToggle(t *testing.T) {
	spec := mqttc.ConnectionSpec{
		ID: "c", Name: "c", Version: mqttc.V5, CleanStart: true, SysStats: true,
		Subscriptions: []mqttc.Subscription{{Filter: mqttc.SysFilter, QoS: 0}},
	}
	broker, mgr, _, ctx := connected(t, spec, mqttc.SysFilter)

	spec.URL = broker.URL
	spec.SysStats = false
	if _, err := mgr.Upsert(ctx, spec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if !broker.HasSubscription(mqttc.SysFilter) {
		t.Error("the user's own $SYS subscription was dropped with the statistics toggle")
	}
}
