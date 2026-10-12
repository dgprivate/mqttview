package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// These tests make one kind of write fail and check what the API says about
// it. Dropping a table (TestHandlersReportAFailingQuery) breaks reads and
// writes alike, and the users and sessions tables cannot be dropped without
// breaking authentication before a handler runs. A trigger refuses exactly one
// statement and leaves everything else working, which is the shape of a real
// partial failure: a constraint added by a later migration, a disk that filled
// between two writes.
//
// Two properties recur. A write that failed is reported as a failure and has
// not happened. A write that is a side effect of something that already
// happened — the history of a message that was sent — is logged and does not
// turn that success into a reported failure.

// refuse makes every statement of the given kind on a table fail. op is what
// follows BEFORE in a trigger: "INSERT", "DELETE", "UPDATE OF password_hash".
func refuse(t *testing.T, ts *testServer, op, table string) {
	t.Helper()
	name := "refuse_" + strings.NewReplacer(" ", "_", ",", "").Replace(strings.ToLower(op)) + "_" + table
	if _, err := ts.db.DB().Exec(
		"CREATE TRIGGER " + name + " BEFORE " + op + " ON " + table +
			" BEGIN SELECT RAISE(ABORT, 'refused by the test'); END"); err != nil {
		t.Fatalf("refusing %s on %s: %v", op, table, err)
	}
	t.Cleanup(func() { _, _ = ts.db.DB().Exec("DROP TRIGGER IF EXISTS " + name) })
}

func allow(t *testing.T, ts *testServer, op, table string) {
	t.Helper()
	name := "refuse_" + strings.NewReplacer(" ", "_", ",", "").Replace(strings.ToLower(op)) + "_" + table
	if _, err := ts.db.DB().Exec("DROP TRIGGER " + name); err != nil {
		t.Fatalf("allowing %s on %s again: %v", op, table, err)
	}
}

func dropTable(t *testing.T, ts *testServer, table string) {
	t.Helper()
	if _, err := ts.db.DB().Exec("DROP TABLE " + table); err != nil {
		t.Fatalf("dropping %s: %v", table, err)
	}
}

// stranger is a second client against the same server with no cookies, for
// checking what a sign-in would do without disturbing the first client's
// session.
func stranger(t *testing.T, ts *testServer) *testServer {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{t: t, http: ts.http, client: &http.Client{Jar: jar}, db: ts.db, mqtt: ts.mqtt, opts: ts.opts}
}

// signsIn reports whether a fresh client gets a session with these
// credentials.
func signsIn(t *testing.T, ts *testServer, email, password string) bool {
	t.Helper()
	return stranger(t, ts).status(http.MethodPost, "/api/auth/login",
		map[string]string{"email": email, "password": password}) == http.StatusOK
}

// --- publishing -------------------------------------------------------------

func TestAPublishGoesOutEvenWhenItsHistoryCannotBeWritten(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)
	dropTable(t, ts, "publish_history")

	// A 200, because the broker has the message: reporting an error would
	// invite the user to send it again.
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/publish",
		map[string]any{"topic": "history/lost", "payload": "sent"}), http.StatusOK, nil)
	waitForTopic(t, ts, connID, "history/lost")
}

func TestThePublishHistoryReportsAFailingQuery(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := deadConnection(t, ts)
	dropTable(t, ts, "publish_history")

	base := "/api/connections/" + connID + "/publishes"
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodDelete, base},
		// A failing lookup is not "no such entry": a 404 would tell the user
		// the message was never sent.
		{http.MethodPost, base + "/1/republish"},
	} {
		if got := ts.status(ep.method, ep.path, nil); got != http.StatusInternalServerError {
			t.Errorf("%s %s gave %d, want 500", ep.method, ep.path, got)
		}
	}
}

func TestSavedMessagesReportAFailingQuery(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := deadConnection(t, ts)
	dropTable(t, ts, "saved_messages")

	base := "/api/connections/" + connID + "/saved"
	body := map[string]any{"name": "n", "topic": "a/b", "payload": "x"}

	if got := ts.status(http.MethodGet, base, nil); got != http.StatusInternalServerError {
		t.Errorf("listing gave %d, want 500", got)
	}
	if got := ts.status(http.MethodPost, base, body); got < 400 {
		t.Errorf("saving into a missing table gave %d, want a refusal", got)
	}
	// A failed lookup must not be mistaken for "not yours" or "not there".
	for _, ep := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPut, base + "/x", body},
		{http.MethodDelete, base + "/x", nil},
		{http.MethodPost, base + "/x/publish", nil},
	} {
		if got := ts.status(ep.method, ep.path, ep.body); got != http.StatusInternalServerError {
			t.Errorf("%s %s gave %d, want 500", ep.method, ep.path, got)
		}
	}
}

func TestDeletingASavedMessageThatCannotBeDeletedLeavesItInPlace(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := deadConnection(t, ts)

	var created savedEntry
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/saved",
		map[string]any{"name": "keep", "topic": "a/b", "payload": "x"}), http.StatusCreated, &created)

	refuse(t, ts, "DELETE", "saved_messages")
	if got := ts.status(http.MethodDelete, "/api/connections/"+connID+"/saved/"+created.ID, nil); got != http.StatusInternalServerError {
		t.Errorf("delete gave %d, want 500", got)
	}

	var list []savedEntry
	ts.decode(ts.do(http.MethodGet, "/api/connections/"+connID+"/saved", nil), http.StatusOK, &list)
	if len(list) != 1 {
		t.Errorf("the collection has %d messages after a failed delete, want it untouched", len(list))
	}
}

// --- retained messages and the audit log ------------------------------------

func TestClearingARetainedValueIsReportedDoneEvenIfTheAuditEntryIsNot(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	connID := subscribedBroker(t, ts)
	refuse(t, ts, "INSERT", "audit_log")

	// The broker has already forgotten the value. A 500 here would say nothing
	// happened, and somebody would go looking for a value that is gone.
	ts.decode(ts.do(http.MethodPost, "/api/connections/"+connID+"/retained/clear",
		map[string]any{"topic": "audit/missing"}), http.StatusOK, nil)
}

func TestTheAuditLogReportsAFailingQuery(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	dropTable(t, ts, "audit_log")

	if got := ts.status(http.MethodGet, "/api/audit", nil); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 rather than an empty log", got)
	}
}

// --- users --------------------------------------------------------------------

func TestAUserThatCouldNotBeStoredDoesNotExist(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	refuse(t, ts, "INSERT", "users")

	if got := ts.status(http.MethodPost, "/api/users", map[string]any{
		"email": "ghost@example.com", "password": "correct-horse-battery", "role": "viewer",
	}); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	if _, err := ts.db.GetUserByEmail("ghost@example.com"); err == nil {
		t.Error("the account exists although creating it was reported as failed")
	}
}

func TestAUserUpdateThatCouldNotBeStoredChangesNothing(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "steady@example.com", "viewer")
	refuse(t, ts, "UPDATE OF role", "users")

	if got := ts.status(http.MethodPut, "/api/users/"+id, map[string]any{
		"email": "steady@example.com", "role": "admin",
	}); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	u, err := ts.db.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != "viewer" {
		t.Errorf("role is %q after a failed update, want it unchanged", u.Role)
	}
}

func TestAPasswordThatCouldNotBeStoredIsNotChanged(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "reset@example.com", "viewer")
	refuse(t, ts, "UPDATE OF password_hash", "users")

	// An administrator's reset of somebody else's password...
	if got := ts.status(http.MethodPut, "/api/users/"+id+"/password",
		map[string]string{"password": "a-brand-new-password"}); got != http.StatusInternalServerError {
		t.Errorf("reset gave %d, want 500", got)
	}
	// ...and a change of one's own.
	if got := ts.status(http.MethodPost, "/api/auth/password", map[string]string{
		"currentPassword": adminPassword, "newPassword": "a-brand-new-password",
	}); got != http.StatusInternalServerError {
		t.Errorf("change gave %d, want 500", got)
	}

	if !signsIn(t, ts, "reset@example.com", "correct-horse-battery") {
		t.Error("the old password stopped working although the reset failed")
	}
	if !signsIn(t, ts, adminEmail, adminPassword) {
		t.Error("the administrator's old password stopped working although the change failed")
	}
}

func TestAUserThatCouldNotBeDeletedIsStillThere(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "sticky@example.com", "viewer")
	refuse(t, ts, "DELETE", "users")

	if got := ts.status(http.MethodDelete, "/api/users/"+id, nil); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	if _, err := ts.db.GetUser(id); err != nil {
		t.Errorf("the account is gone although deleting it was reported as failed: %v", err)
	}
}

// Disabling an account clears its sessions, but the clearing is not what
// locks the account out: the session query refuses a disabled account on its
// own. So when the clearing fails the account is still locked out, and that
// is why the handler can report success.
func TestADisabledAccountIsLockedOutEvenWhenItsSessionsCannotBeCleared(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "leaver@example.com", "viewer")
	leaver := ts.asUser(t, "leaver@example.com", "correct-horse-battery")

	refuse(t, ts, "DELETE", "sessions")
	ts.decode(ts.do(http.MethodPut, "/api/users/"+id, map[string]any{
		"email": "leaver@example.com", "role": "viewer", "disabled": true,
	}), http.StatusOK, nil)

	if got := leaver.status(http.MethodGet, "/api/auth/me", nil); got != http.StatusUnauthorized {
		t.Errorf("the disabled account's session still answers with %d", got)
	}
}

// After a password reset or change the new password is what counts, whether
// or not the other sessions could be cleared and whether or not the caller's
// own session could be re-issued.
func TestAPasswordChangeTakesEffectWhenSessionBookkeepingFails(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := newUser(t, ts, "rotate@example.com", "viewer")

	refuse(t, ts, "DELETE", "sessions")
	refuse(t, ts, "INSERT", "sessions")

	ts.decode(ts.do(http.MethodPut, "/api/users/"+id+"/password",
		map[string]string{"password": "rotated-password-one"}), http.StatusOK, nil)
	ts.decode(ts.do(http.MethodPost, "/api/auth/password", map[string]string{
		"currentPassword": adminPassword, "newPassword": "rotated-password-two",
	}), http.StatusOK, nil)

	allow(t, ts, "INSERT", "sessions")
	if !signsIn(t, ts, "rotate@example.com", "rotated-password-one") {
		t.Error("the reset password does not sign in")
	}
	if signsIn(t, ts, adminEmail, adminPassword) {
		t.Error("the administrator's old password still signs in after a change")
	}
	if !signsIn(t, ts, adminEmail, "rotated-password-two") {
		t.Error("the administrator's new password does not sign in")
	}
}

func TestASignInWhoseSessionCannotBeStoredIsNotASignIn(t *testing.T) {
	ts := newTestServer(t)
	refuse(t, ts, "INSERT", "sessions")

	if got := ts.status(http.MethodPost, "/api/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword}); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	if got := ts.status(http.MethodGet, "/api/auth/me", nil); got != http.StatusUnauthorized {
		t.Errorf("after a failed sign-in /me answered %d, want 401", got)
	}
}

// --- two-factor -----------------------------------------------------------------

func TestTwoFactorEnrolmentThatCannotBeStoredIsNotStarted(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	refuse(t, ts, "UPDATE OF totp_secret_enc", "users")

	if got := ts.status(http.MethodPost, "/api/auth/2fa/enrol", nil); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — a secret that was not stored must not be shown", got)
	}
}

func TestTwoFactorThatCannotBeConfirmedIsNotEnforced(t *testing.T) {
	ts := newTestServer(t)
	ts.login()

	var enrolment struct {
		Secret string `json:"secret"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/auth/2fa/enrol", nil), http.StatusOK, &enrolment)
	refuse(t, ts, "UPDATE OF totp_confirmed_at", "users")

	if got := ts.status(http.MethodPost, "/api/auth/2fa/confirm",
		map[string]string{"code": currentCode(t, enrolment.Secret)}); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	// Enforcing a factor whose recovery codes were never shown would lock the
	// account out the first time the phone is lost.
	if !signsIn(t, ts, adminEmail, adminPassword) {
		t.Error("the password alone no longer signs in, so the unconfirmed factor is being enforced")
	}
}

func TestTwoFactorThatCannotBeTurnedOffStaysOn(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	secret, _ := enrol(t, ts)
	refuse(t, ts, "UPDATE OF totp_secret_enc", "users")

	if got := ts.status(http.MethodPost, "/api/auth/2fa/disable", map[string]string{
		"password": adminPassword, "code": currentCode(t, secret),
	}); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	if signsIn(t, ts, adminEmail, adminPassword) {
		t.Error("the password alone signs in, so the factor was dropped although disabling it failed")
	}
}

func TestRecoveryCodesThatCannotBeReplacedStillWork(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	secret, codes := enrol(t, ts)
	refuse(t, ts, "INSERT", "recovery_codes")

	if got := ts.status(http.MethodPost, "/api/auth/2fa/recovery-codes",
		map[string]string{"code": currentCode(t, secret)}); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}

	// The replacement is one transaction. Had the old codes been deleted
	// without new ones going in, the account would have no way back.
	if got := stranger(t, ts).status(http.MethodPost, "/api/auth/login", map[string]string{
		"email": adminEmail, "password": adminPassword, "code": codes[0],
	}); got != http.StatusOK {
		t.Errorf("an old recovery code gave %d after a failed regeneration, want it to still sign in", got)
	}
}

// --- connections ----------------------------------------------------------------

func TestAConnectionUpdateThatCannotBeStoredDoesNotTakeEffect(t *testing.T) {
	for _, tt := range []struct {
		name   string
		induce func(t *testing.T, ts *testServer)
	}{
		{"the stored definition cannot be read", func(t *testing.T, ts *testServer) { dropTable(t, ts, "connections") }},
		{"the new definition cannot be written", func(t *testing.T, ts *testServer) {
			refuse(t, ts, "UPDATE", "connections")
			refuse(t, ts, "INSERT", "connections")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.login()
			id := deadConnection(t, ts)
			tt.induce(t, ts)

			if got := ts.status(http.MethodPut, "/api/connections/"+id, map[string]any{
				"name": "renamed", "url": "mqtt://127.0.0.1:1", "version": "3.1.1",
			}); got != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", got)
			}

			// The live connection is only replaced once the definition is
			// saved, so a restart cannot quietly undo what the user saw.
			var view struct {
				Name string `json:"name"`
			}
			ts.decode(ts.do(http.MethodGet, "/api/connections/"+id, nil), http.StatusOK, &view)
			if view.Name != "dead" {
				t.Errorf("the live connection is named %q after a failed update", view.Name)
			}
		})
	}
}

func TestAConnectionThatCannotBeDeletedIsReportedAsSuch(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	id := deadConnection(t, ts)
	refuse(t, ts, "DELETE", "connections")

	if got := ts.status(http.MethodDelete, "/api/connections/"+id, nil); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	if _, err := ts.db.GetConnection(id); err != nil {
		t.Errorf("the stored definition is gone although the delete was reported as failed: %v", err)
	}
}

// A subscription is changed on the live session first and saved second. When
// saving fails the live session is still right, so the change is reported as
// made; it is only a restart that would lose it, and that is logged.
func TestASubscriptionAppliesEvenWhenItCannotBeSaved(t *testing.T) {
	for _, tt := range []struct {
		name   string
		induce func(t *testing.T, ts *testServer)
	}{
		{"the stored definition cannot be read", func(t *testing.T, ts *testServer) { dropTable(t, ts, "connections") }},
		{"the new definition cannot be written", func(t *testing.T, ts *testServer) {
			refuse(t, ts, "UPDATE", "connections")
			refuse(t, ts, "INSERT", "connections")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.login()
			id := deadConnection(t, ts)
			tt.induce(t, ts)

			var view struct {
				Subscriptions []struct {
					Filter string `json:"filter"`
				} `json:"subscriptions"`
			}
			ts.decode(ts.do(http.MethodPost, "/api/connections/"+id+"/subscribe",
				map[string]any{"subscriptions": []map[string]any{{"filter": "kept/#"}}}),
				http.StatusOK, &view)
			if len(view.Subscriptions) != 1 || view.Subscriptions[0].Filter != "kept/#" {
				t.Errorf("subscriptions are %+v, want kept/#", view.Subscriptions)
			}
		})
	}
}
