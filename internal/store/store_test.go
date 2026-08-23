package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mqttview/mqttview/internal/mqttc"
)

func TestCreateUserValidatesTheRole(t *testing.T) {
	st := newTestStore(t)

	// A role is what every permission check reads, so an unrecognised one is
	// refused at the door rather than stored and interpreted later.
	if _, err := st.CreateUser(User{ID: uuid.NewString(), Email: "a@example.com", Role: "wizard"}); err == nil {
		t.Fatal("an unknown role was accepted")
	}
	if _, err := st.CreateUser(User{ID: uuid.NewString(), Email: "b@example.com"}); err == nil {
		t.Fatal("an empty role was accepted")
	}
}

func TestUserCRUD(t *testing.T) {
	st := newTestStore(t)

	u, err := st.CreateUser(User{
		ID:           uuid.NewString(),
		Email:        "  Person@Example.COM ",
		Name:         "Person",
		PasswordHash: "hash",
		Role:         RoleOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Addresses are normalised on the way in, or the same person could have
	// two accounts that differ only by case.
	if u.Email != "person@example.com" {
		t.Errorf("email = %q, want it normalised", u.Email)
	}
	if u.Provider != ProviderLocal {
		t.Errorf("provider = %q, want the local default", u.Provider)
	}

	byID, err := st.GetUser(u.ID)
	if err != nil || byID.Email != u.Email {
		t.Fatalf("GetUser: %+v %v", byID, err)
	}
	byEmail, err := st.GetUserByEmail("PERSON@example.com")
	if err != nil || byEmail.ID != u.ID {
		t.Fatalf("GetUserByEmail did not normalise: %+v %v", byEmail, err)
	}

	u.Name = "Renamed"
	u.Role = RoleAdmin
	if err := st.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetUser(u.ID)
	if got.Name != "Renamed" || got.Role != RoleAdmin {
		t.Errorf("update did not take: %+v", got)
	}

	if err := st.SetPasswordHash(u.ID, "newhash"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetUser(u.ID)
	if got.PasswordHash != "newhash" {
		t.Error("the password hash was not updated")
	}

	users, err := st.ListUsers()
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers gave %d users, %v", len(users), err)
	}

	if err := st.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetUser(u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete GetUser gave %v, want ErrNotFound", err)
	}
}

func TestDuplicateEmailIsAConflict(t *testing.T) {
	st := newTestStore(t)
	newTestUser(t, st)

	_, err := st.CreateUser(User{ID: uuid.NewString(), Email: "person@example.com", Role: RoleViewer})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a duplicate address gave %v, want ErrConflict", err)
	}
}

func TestCountUsersAndAdmins(t *testing.T) {
	st := newTestStore(t)

	if n, _ := st.CountUsers(); n != 0 {
		t.Fatalf("a fresh database has %d users", n)
	}

	admin := newTestUser(t, st)
	if n, _ := st.CountAdmins(); n != 1 {
		t.Fatalf("CountAdmins = %d, want 1", n)
	}

	// The count is what stops the last administrator being demoted or deleted.
	admin.Role = RoleViewer
	if err := st.UpdateUser(admin); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountAdmins(); n != 0 {
		t.Errorf("CountAdmins = %d after demotion, want 0", n)
	}
	if n, _ := st.CountUsers(); n != 1 {
		t.Errorf("CountUsers = %d, want 1", n)
	}
}

func TestProviderLinking(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if _, err := st.GetUserByProviderSubject("idp", "subject-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unlinked subject gave %v", err)
	}

	if err := st.LinkProvider(u.ID, "idp", "subject-1"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUserByProviderSubject("idp", "subject-1")
	if err != nil || got.ID != u.ID {
		t.Fatalf("lookup by subject: %+v %v", got, err)
	}
	if got.Provider != "idp" {
		t.Errorf("provider = %q after linking", got.Provider)
	}
}

func TestTouchLoginRecordsTheTime(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if u.LastLoginAt != nil {
		t.Fatal("a new account has already signed in?")
	}
	if err := st.TouchLogin(u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetUser(u.ID)
	if got.LastLoginAt == nil {
		t.Fatal("the login time was not recorded")
	}
	if time.Since(*got.LastLoginAt) > time.Minute {
		t.Errorf("login time is %v, which is not now", got.LastLoginAt)
	}
}

func TestSessionLifecycle(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	sess := Session{
		ID: "hash-1", UserID: u.ID,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		UserAgent: "test", IP: "10.0.0.1",
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}

	gotSess, gotUser, err := st.SessionUser("hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotUser.ID != u.ID || gotSess.IP != "10.0.0.1" {
		t.Fatalf("session load: %+v %+v", gotSess, gotUser)
	}

	if err := st.DeleteSession("hash-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser("hash-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted session still resolved: %v", err)
	}
}

func TestAnExpiredSessionDoesNotResolve(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if err := st.CreateSession(Session{
		ID: "old", UserID: u.ID,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Expiry is enforced on read, not only by the sweeper, so a session cannot
	// outlive its lifetime just because the sweep has not run yet.
	if _, _, err := st.SessionUser("old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session resolved: %v", err)
	}
}

func TestADisabledUserCannotResolveASession(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	if err := st.CreateSession(Session{
		ID: "s", UserID: u.ID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	u.Disabled = true
	if err := st.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	// Disabling an account has to take effect on the sessions it already has,
	// or it does nothing until they expire.
	if _, _, err := st.SessionUser("s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a disabled user's session resolved: %v", err)
	}
}

func TestDeleteUserSessionsAndPurge(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	for _, id := range []string{"a", "b"} {
		if err := st.CreateSession(Session{
			ID: id, UserID: u.ID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Changing a password signs the other browsers out; this is that.
	if err := st.DeleteUserSessions(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser("a"); !errors.Is(err, ErrNotFound) {
		t.Error("a session survived DeleteUserSessions")
	}

	if err := st.CreateSession(Session{
		ID: "expired", UserID: u.ID,
		CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.PurgeExpiredSessions()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d sessions, want 1", n)
	}
}

func TestConnectionCRUD(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	spec := mqttc.ConnectionSpec{
		ID:       uuid.NewString(),
		Name:     "broker",
		URL:      "mqtts://broker.example.com:8883",
		Version:  mqttc.V5,
		Username: "user",
		Password: "hunter2",
		TLS: mqttc.TLSSpec{
			InsecureSkipVerify: true,
			ClientKeyPEM:       "-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----",
			ClientCertPEM:      "-----BEGIN CERTIFICATE-----\ny\n-----END CERTIFICATE-----",
		},
		Subscriptions: []mqttc.Subscription{{Filter: "a/#", QoS: 1}},
		AutoConnect:   true,
	}
	if err := st.SaveConnection(ConnectionRecord{Spec: spec, CreatedBy: u.ID}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetConnection(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The password comes back decrypted, which is the whole point of the box.
	if got.Spec.Password != "hunter2" {
		t.Errorf("password = %q", got.Spec.Password)
	}
	if got.Spec.TLS.ClientKeyPEM != spec.TLS.ClientKeyPEM || !got.Spec.TLS.InsecureSkipVerify {
		t.Errorf("TLS settings did not round trip: %+v", got.Spec.TLS)
	}
	if len(got.Spec.Subscriptions) != 1 || got.Spec.Subscriptions[0].Filter != "a/#" {
		t.Errorf("subscriptions did not round trip: %+v", got.Spec.Subscriptions)
	}

	list, err := st.ListConnections()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListConnections gave %d, %v", len(list), err)
	}

	// Saving again is an upsert, not a duplicate.
	spec.Name = "renamed"
	if err := st.SaveConnection(ConnectionRecord{Spec: spec}); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListConnections()
	if len(list) != 1 || list[0].Spec.Name != "renamed" {
		t.Fatalf("upsert produced %d rows: %+v", len(list), list)
	}

	if err := st.DeleteConnection(spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetConnection(spec.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted connection still loaded: %v", err)
	}
	if err := st.DeleteConnection("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting nothing gave %v", err)
	}
}

func TestTheStoredPasswordIsCiphertext(t *testing.T) {
	st := newTestStore(t)

	spec := mqttc.ConnectionSpec{
		ID: uuid.NewString(), Name: "b", URL: "mqtt://h:1883",
		Version: mqttc.V311, Password: "hunter2",
	}
	if err := st.SaveConnection(ConnectionRecord{Spec: spec}); err != nil {
		t.Fatal(err)
	}

	// Read the raw column: a database that anyone can copy must not contain
	// the broker password in the clear.
	var raw string
	if err := st.DB().QueryRow(
		`SELECT password_enc FROM connections WHERE id = ?`, spec.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || raw == "hunter2" {
		t.Fatalf("password_enc = %q, which is not ciphertext", raw)
	}
}

func TestPluginSettingsAndState(t *testing.T) {
	st := newTestStore(t)

	// Nothing saved yet.
	if _, err := st.GetPluginSettings("plug"); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading absent settings gave %v", err)
	}

	if err := st.SavePluginSettings(PluginSettings{
		PluginID: "plug", Enabled: true, Settings: map[string]any{"prefix": "plc"},
	}); err != nil {
		t.Fatal(err)
	}
	ps, err := st.GetPluginSettings("plug")
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Enabled || ps.Settings["prefix"] != "plc" {
		t.Fatalf("settings did not round trip: %+v", ps)
	}

	list, err := st.ListPluginSettings()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListPluginSettings gave %d, %v", len(list), err)
	}

	// The per-plugin key-value store is namespaced, so two plugins cannot
	// collide on a key name.
	if err := st.PluginSet("plug", "k", "v"); err != nil {
		t.Fatal(err)
	}
	v, err := st.PluginGet("plug", "k")
	if err != nil || v != "v" {
		t.Fatalf("PluginGet gave %q %v", v, err)
	}
	if v, _ := st.PluginGet("other", "k"); v != "" {
		t.Error("a key leaked between plugins")
	}

	all, err := st.PluginList("plug")
	if err != nil || len(all) != 1 {
		t.Fatalf("PluginList gave %+v %v", all, err)
	}

	if err := st.PluginDelete("plug", "k"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.PluginGet("plug", "k"); v != "" {
		t.Error("the key survived deletion")
	}
}

func TestSettingsTable(t *testing.T) {
	st := newTestStore(t)

	// An absent key is ErrNotFound rather than an empty string, so a caller can
	// tell "never set" from "set to empty".
	if _, err := st.Setting("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an absent setting gave %v, want ErrNotFound", err)
	}
	if err := st.SetSetting("k", "v"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Setting("k"); v != "v" {
		t.Errorf("Setting = %q", v)
	}
	// Setting it again replaces rather than duplicating.
	if err := st.SetSetting("k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Setting("k"); v != "v2" {
		t.Errorf("Setting = %q after overwrite", v)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	st := newTestStore(t)

	// Re-running must be a no-op: every start calls this, and an install that
	// has already migrated must not be migrated again.
	if err := st.migrate(); err != nil {
		t.Fatalf("re-running migrations failed: %v", err)
	}

	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no migrations were recorded")
	}
}

func TestSaveConnectionValidatesAndReportsFailures(t *testing.T) {
	st := newTestStore(t)

	// A spec that does not normalise is refused before it reaches the
	// database, so a row that cannot be loaded again is never written.
	for _, spec := range []mqttc.ConnectionSpec{
		{ID: "", Name: "n", URL: "mqtt://h:1883", Version: mqttc.V311},
		{ID: "a", Name: "", URL: "mqtt://h:1883", Version: mqttc.V311},
		{ID: "a", Name: "n", URL: "", Version: mqttc.V311},
		{ID: "a", Name: "n", URL: "gopher://h:70", Version: mqttc.V311},
	} {
		if err := st.SaveConnection(ConnectionRecord{Spec: spec}); err == nil {
			t.Errorf("accepted %+v", spec)
		}
	}
}

func TestUpdateAndLinkOnAMissingUser(t *testing.T) {
	st := newTestStore(t)

	missing := User{ID: "nope", Email: "a@example.com", Role: RoleViewer}
	if err := st.UpdateUser(missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateUser on a missing row gave %v", err)
	}
	if err := st.SetPasswordHash("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPasswordHash gave %v", err)
	}
	if err := st.LinkProvider("nope", "idp", "s"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LinkProvider gave %v", err)
	}
	if err := st.TouchLogin("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchLogin gave %v", err)
	}
	if err := st.DeleteUser("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteUser gave %v", err)
	}
}

func TestUpdateUserRejectsAnUnknownRole(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)

	u.Role = "wizard"
	if err := st.UpdateUser(u); err == nil {
		t.Fatal("an unknown role was accepted on update")
	}
}

func TestUpdateUserRejectsADuplicateAddress(t *testing.T) {
	st := newTestStore(t)
	first := newTestUser(t, st)

	second, err := st.CreateUser(User{
		ID: "u2", Email: "other@example.com", Role: RoleViewer, PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}

	second.Email = first.Email
	if err := st.UpdateUser(second); !errors.Is(err, ErrConflict) {
		t.Fatalf("taking another account's address gave %v, want ErrConflict", err)
	}
}

func TestPluginSettingsUpsertRatherThanDuplicate(t *testing.T) {
	st := newTestStore(t)

	for i, enabled := range []bool{true, false, true} {
		if err := st.SavePluginSettings(PluginSettings{
			PluginID: "p", Enabled: enabled, Settings: map[string]any{"n": i},
		}); err != nil {
			t.Fatal(err)
		}
	}

	list, err := st.ListPluginSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("saving three times produced %d rows", len(list))
	}
	ps, _ := st.GetPluginSettings("p")
	if !ps.Enabled {
		t.Error("the last write did not win")
	}
}

func TestPluginSettingsWithNoSettingsBlock(t *testing.T) {
	st := newTestStore(t)

	// A nil map must round-trip as an empty one, not as JSON null that fails
	// to decode on the way back.
	if err := st.SavePluginSettings(PluginSettings{PluginID: "p", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ps, err := st.GetPluginSettings("p")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Settings == nil {
		t.Error("settings came back nil rather than empty")
	}
}

// TestEveryQueryReportsAFailingDatabase closes the database and calls every
// method, so the `if err != nil` branch after each query is exercised.
//
// A store that swallows a failure and returns a zero value is worse than one
// that fails: the caller signs somebody in, or shows an empty list, believing
// it read the truth.
func TestEveryQueryReportsAFailingDatabase(t *testing.T) {
	st := newTestStore(t)
	u := newTestUser(t, st)
	if err := st.ReplaceRecoveryCodes(u.ID, []string{"h"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	checks := map[string]func() error{
		"CreateUser": func() error {
			_, err := st.CreateUser(User{ID: "x", Email: "x@example.com", Role: RoleViewer})
			return err
		},
		"GetUser":                  func() error { _, err := st.GetUser(u.ID); return err },
		"GetUserByEmail":           func() error { _, err := st.GetUserByEmail(u.Email); return err },
		"GetUserByProviderSubject": func() error { _, err := st.GetUserByProviderSubject("i", "s"); return err },
		"ListUsers":                func() error { _, err := st.ListUsers(); return err },
		"CountUsers":               func() error { _, err := st.CountUsers(); return err },
		"CountAdmins":              func() error { _, err := st.CountAdmins(); return err },
		"UpdateUser":               func() error { return st.UpdateUser(u) },
		"SetPasswordHash":          func() error { return st.SetPasswordHash(u.ID, "h") },
		"LinkProvider":             func() error { return st.LinkProvider(u.ID, "i", "s") },
		"TouchLogin":               func() error { return st.TouchLogin(u.ID) },
		"DeleteUser":               func() error { return st.DeleteUser(u.ID) },

		"CreateSession": func() error {
			return st.CreateSession(Session{ID: "s", UserID: u.ID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
		},
		"SessionUser":          func() error { _, _, err := st.SessionUser("s"); return err },
		"DeleteSession":        func() error { return st.DeleteSession("s") },
		"DeleteUserSessions":   func() error { return st.DeleteUserSessions(u.ID) },
		"PurgeExpiredSessions": func() error { _, err := st.PurgeExpiredSessions(); return err },

		"SaveConnection": func() error {
			return st.SaveConnection(ConnectionRecord{Spec: mqttc.ConnectionSpec{
				ID: "c", Name: "c", URL: "mqtt://h:1883", Version: mqttc.V311,
			}})
		},
		"GetConnection":    func() error { _, err := st.GetConnection("c"); return err },
		"ListConnections":  func() error { _, err := st.ListConnections(); return err },
		"DeleteConnection": func() error { return st.DeleteConnection("c") },

		"GetPluginSettings":  func() error { _, err := st.GetPluginSettings("p"); return err },
		"ListPluginSettings": func() error { _, err := st.ListPluginSettings(); return err },
		"SavePluginSettings": func() error { return st.SavePluginSettings(PluginSettings{PluginID: "p"}) },
		"PluginGet":          func() error { _, err := st.PluginGet("p", "k"); return err },
		"PluginSet":          func() error { return st.PluginSet("p", "k", "v") },
		"PluginDelete":       func() error { return st.PluginDelete("p", "k") },
		"PluginList":         func() error { _, err := st.PluginList("p"); return err },

		"Setting":    func() error { _, err := st.Setting("k"); return err },
		"SetSetting": func() error { return st.SetSetting("k", "v") },

		"SetTOTPSecret":            func() error { return st.SetTOTPSecret(u.ID, "x") },
		"ConfirmTOTP":              func() error { return st.ConfirmTOTP(u.ID, time.Now()) },
		"DisableTOTP":              func() error { return st.DisableTOTP(u.ID) },
		"ReplaceRecoveryCodes":     func() error { return st.ReplaceRecoveryCodes(u.ID, []string{"h"}) },
		"RecoveryCodeHashes":       func() error { _, err := st.RecoveryCodeHashes(u.ID); return err },
		"UseRecoveryCode":          func() error { return st.UseRecoveryCode("id", time.Now()) },
		"CountUnusedRecoveryCodes": func() error { _, err := st.CountUnusedRecoveryCodes(u.ID); return err },
	}

	for name, fn := range checks {
		if err := fn(); err == nil {
			t.Errorf("%s succeeded with a closed database", name)
		}
	}
}
