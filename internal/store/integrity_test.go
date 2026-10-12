package store

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgprivate/mqttview/internal/mqttc"
	"github.com/dgprivate/mqttview/internal/secrets"
)

// rawDB opens a database file without the store, so a test can set up a state
// the store itself would never produce.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	key, err := secrets.LoadOrCreateKey("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestOpenNamesTheDirectoryItCouldNotCreate(t *testing.T) {
	// A file where the directory should be: MkdirAll cannot get past it.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "data")

	_, err := Open(filepath.Join(dir, "mqttview.db"), testBox(t))
	if err == nil {
		t.Fatal("a database was opened under a path that cannot be a directory")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not say which directory", err)
	}
}

// Whatever is at the path is somebody's file. Refusing to open it is the only
// acceptable outcome; "migrating" it would destroy it.
func TestAFileThatIsNotADatabaseIsRefusedAndLeftAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mqttview.db")
	original := bytes.Repeat([]byte("this is a notes file, not SQLite\n"), 200)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if st, err := Open(path, testBox(t)); err == nil {
		_ = st.Close()
		t.Fatal("a file that is not a database was opened")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Error("opening a file that is not a database changed it")
	}
}

// A migration that fails part-way must leave nothing behind and no record that
// it ran, so the next start tries it again from the beginning instead of
// running on a half-built schema.
func TestAFailedMigrationLeavesNoTraceAndIsNotRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mqttview.db")
	db := rawDB(t, path)
	// sessions is created after users in the first migration, so users being
	// absent afterwards proves the earlier statements were rolled back too.
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT)`); err != nil {
		t.Fatal(err)
	}

	if st, err := Open(path, testBox(t)); err == nil {
		_ = st.Close()
		t.Fatal("a migration that could not apply was reported as success")
	} else if !strings.Contains(err.Error(), migrations[0].name) {
		t.Errorf("error %q does not name the migration that failed", err)
	}

	if tableExists(t, db, "users") {
		t.Error("a table from the failed migration was left behind")
	}
	var recorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Errorf("%d migrations recorded as applied after a failure", recorded)
	}
}

// The schema change and the row saying it happened are one transaction. If the
// record cannot be written, the change must not stay either — otherwise the
// next start runs CREATE TABLE again on a table that exists and never boots.
func TestAMigrationAndItsRecordCommitTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mqttview.db")
	db := rawDB(t, path)
	// A column the store does not fill, so recording any migration fails.
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
        name TEXT PRIMARY KEY, applied_at TEXT NOT NULL, required TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	if st, err := Open(path, testBox(t)); err == nil {
		_ = st.Close()
		t.Fatal("a migration whose record could not be written was reported as success")
	}
	if tableExists(t, db, "users") {
		t.Error("the migration stayed applied although its record was not written")
	}
}

// Without a key there is nothing to encrypt a credential with. The store must
// refuse to write the connection rather than fall back to keeping it readable.
func TestWithoutAKeyNoConnectionIsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mqttview.db")
	st, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	err = st.SaveConnection(ConnectionRecord{Spec: mqttc.ConnectionSpec{
		ID: "c", Name: "broker", URL: "mqtt://h:1883", Version: mqttc.V311, Password: "hunter2",
	}})
	if !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("saving without a key gave %v, want ErrNoKey", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("hunter2")) {
		t.Error("the password reached the database file")
	}
	if list, err := st.ListConnections(); err != nil || len(list) != 0 {
		t.Errorf("connections after a refused save: %v, %v", list, err)
	}
}

func TestASavedMessageNeedsAnIDANameAndATopic(t *testing.T) {
	st := newTestStore(t)

	for _, msg := range []SavedMessage{
		{ID: " ", Name: "n", Topic: "t"},
		{ID: "a", Name: " ", Topic: "t"},
		{ID: "a", Name: "n", Topic: " "},
	} {
		if err := st.SaveMessage(msg); err == nil {
			t.Errorf("accepted %+v", msg)
		}
	}
	if list, err := st.SavedMessages(""); err != nil || len(list) != 0 {
		t.Errorf("refused messages were stored anyway: %v, %v", list, err)
	}
}

// A batch is written in one transaction so it is cheap, and that also makes it
// atomic: one bad row must not leave the rows before it recorded, or the
// history has a gap nobody can explain.
func TestARecordedBatchIsWrittenWhollyOrNotAtAll(t *testing.T) {
	st := newTestStore(t)
	if err := st.SaveConnection(ConnectionRecord{Spec: mqttc.ConnectionSpec{
		ID: "c", Name: "c", URL: "mqtt://h:1883", Version: mqttc.V311,
	}}); err != nil {
		t.Fatal(err)
	}

	err := st.AppendRecorded([]RecordedMessage{
		{ConnectionID: "c", Topic: "a", Payload: []byte("1")},
		{ConnectionID: "c", Topic: "b", Payload: []byte("2")},
		// No such connection: the foreign key refuses it.
		{ConnectionID: "gone", Topic: "c", Payload: []byte("3")},
	})
	if err == nil {
		t.Fatal("a batch with a row for an unknown connection was accepted")
	}

	got, err := st.Recorded(RecordedQuery{ConnectionID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("%d rows of a failed batch were kept", len(got))
	}
}
