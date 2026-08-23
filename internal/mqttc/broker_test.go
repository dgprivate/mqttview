package mqttc_test

import (
	"testing"

	"github.com/dgprivate/mqttview/internal/testutil"
)

// startBroker runs a broker for a test and returns its URL.
//
// This used to be a second copy of testutil's broker, and the copy carried the
// same bug: a readiness probe that dialled the port, handing the broker a
// connection to establish and tear down, which raced with a fast test closing
// it. Fixing testutil did nothing for the copy. One implementation, so a fix
// applies once.
func startBroker(t *testing.T) string {
	t.Helper()
	return testutil.StartBroker(t).URL
}

// brokerFor is startBroker for tests that need more than the URL — waiting for
// a subscription to land, say, rather than sleeping and hoping it has.
func brokerFor(t *testing.T) *testutil.Broker {
	t.Helper()
	return testutil.StartBroker(t)
}
