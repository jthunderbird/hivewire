package opencode

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const reducedDDL = `
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  parent_id TEXT,
  directory TEXT NOT NULL,
  title TEXT NOT NULL,
  version TEXT NOT NULL,
  agent TEXT,
  model TEXT,
  tokens_input INTEGER DEFAULT 0 NOT NULL,
  tokens_output INTEGER DEFAULT 0 NOT NULL,
  tokens_reasoning INTEGER DEFAULT 0 NOT NULL,
  tokens_cache_read INTEGER DEFAULT 0 NOT NULL,
  tokens_cache_write INTEGER DEFAULT 0 NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL
);
CREATE INDEX session_parent_idx ON session(parent_id);
CREATE TABLE message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX message_session_time_created_id_idx ON message(session_id,time_created,id);
CREATE TABLE part (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX part_message_id_id_idx ON part(message_id,id);
CREATE INDEX part_session_idx ON part(session_id);
`

func fixtureDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	dsn := sqliteFileURL(abs)
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(reducedDDL); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertSession(t *testing.T, db *sql.DB, id, parent string, created int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO session (
		id,project_id,parent_id,directory,title,version,agent,model,time_created,time_updated
	) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, "project", parent, "/work", id, "1.15.7", "build", "gpt-5", created, created+1); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteFileURI(t *testing.T) {
	tests := []struct {
		name, path, prefix string
	}{
		{"POSIX", "/tmp/OpenCode #1?.db", "file:///tmp/OpenCode%20%231%3F.db?"},
		{"Windows drive", `C:\Users\Alice\OpenCode #1?.db`, "file:///C:/Users/Alice/OpenCode%20%231%3F.db?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sqliteFileURI(tt.path)
			if !strings.HasPrefix(got, tt.prefix) {
				t.Fatalf("sqliteFileURI(%q) = %q, want prefix %q", tt.path, got, tt.prefix)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			if u.Scheme != "file" || u.Host != "" {
				t.Fatalf("URI scheme/host = %q/%q, want file with no authority", u.Scheme, u.Host)
			}
			if u.Query().Get("mode") != "ro" {
				t.Fatalf("mode = %q, want ro", u.Query().Get("mode"))
			}
			if got := u.Query()["_pragma"]; !reflect.DeepEqual(got, []string{"query_only(1)", "busy_timeout(50)"}) {
				t.Fatalf("pragmas = %v", got)
			}
		})
	}
}

func TestReducedSessionSchemaMatchesQueriedProductionSubset(t *testing.T) {
	db := fixtureDB(t, filepath.Join(t.TempDir(), "opencode.db"))
	rows, err := db.Query(`PRAGMA table_info(session)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var columns []string
	projectRequired := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
		if name == "project_id" {
			projectRequired = notNull == 1
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"id", "project_id", "parent_id", "directory", "title", "version", "agent", "model",
		"tokens_input", "tokens_output", "tokens_reasoning", "tokens_cache_read", "tokens_cache_write",
		"time_created", "time_updated",
	}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("session columns = %v, want %v", columns, want)
	}
	if !projectRequired {
		t.Fatal("project_id must be NOT NULL")
	}
}

func TestSnapshotReadsChildRowsInStableOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	w := fixtureDB(t, path)
	insertSession(t, w, "parent", "", 1)
	insertSession(t, w, "child-b", "parent", 20)
	insertSession(t, w, "child-a", "parent", 20)
	if _, err := w.Exec(`INSERT INTO message VALUES
		('msg-b','child-a',30,31,'{"role":"assistant"}'),
		('msg-a','child-a',30,32,'{"role":"user"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`INSERT INTO part VALUES
		('part-b','msg-a','child-a',40,41,'{"type":"text","text":"b"}'),
		('part-a','msg-a','child-a',40,42,'{"type":"text","text":"a"}')`); err != nil {
		t.Fatal(err)
	}

	db := newDatabase(path)
	snap, err := db.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := rowIDs(snap.sessions); got != "child-a,child-b" {
		t.Fatalf("session order = %q", got)
	}
	if got := messageIDs(snap.messages["child-a"]); got != "msg-a,msg-b" {
		t.Fatalf("message order = %q", got)
	}
	if got := partIDs(snap.parts["child-a"]); got != "part-a,part-b" {
		t.Fatalf("part order = %q", got)
	}
	if snap.sessions[0].parentID != "parent" || snap.sessions[0].agent != "build" || snap.sessions[0].model != "gpt-5" {
		t.Fatalf("reduced session row not populated: %+v", snap.sessions[0])
	}
}

func TestSnapshotReleasesDatabaseForRenameAndDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	w := fixtureDB(t, path)
	insertSession(t, w, "child", "parent", 1)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	db := newDatabase(path)
	if _, err := db.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "old.db")
	if err := os.Rename(path, oldPath); err != nil {
		t.Fatalf("rename after snapshot: %v", err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("delete after snapshot: %v", err)
	}
}

func TestMissingDatabaseIsNoOpWithoutArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing # database?.db")
	db := newDatabase(path)

	snap, err := db.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.sessions) != 0 || len(snap.messages) != 0 || len(snap.parts) != 0 {
		t.Fatalf("missing database returned data: %+v", snap)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("missing database created artifacts: %v", entries)
	}
}

func TestNonWALSnapshotCannotWriteOrCreateSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "OpenCode #1?.db")
	w := fixtureDB(t, path)
	insertSession(t, w, "child", "parent", 1)

	db := newDatabase(path)
	if _, err := db.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle, exists, err := openDatabase(context.Background(), path)
	if err != nil || !exists {
		t.Fatalf("open = %v, %v", exists, err)
	}
	defer handle.Close()
	var queryOnly, busyTimeout int
	if err := handle.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	if err := handle.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if queryOnly != 1 || busyTimeout != 50 {
		t.Fatalf("connection pragmas query_only=%d busy_timeout=%d", queryOnly, busyTimeout)
	}
	if max := handle.Stats().MaxOpenConnections; max != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", max)
	}
	if _, err := handle.Exec(`INSERT INTO session (
		id,project_id,parent_id,directory,title,version,time_created,time_updated
	) VALUES ('written','project','parent','/work','bad','1',1,1)`); err == nil {
		t.Fatal("read-only connection accepted a write")
	}

	var count int
	if err := w.QueryRow(`SELECT count(*) FROM session`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("logical session count = %d, want 1", count)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !reflect.DeepEqual(names, []string{filepath.Base(path)}) {
		t.Fatalf("read created filesystem artifacts: %v", names)
	}
}

func TestActiveWALSnapshotSeesCommittedRowsWithoutLogicalWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	w := fixtureDB(t, path)
	if _, err := w.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	insertSession(t, w, "child", "parent", 1)

	db := newDatabase(path)
	first, err := db.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.messages["child"]) != 0 {
		t.Fatalf("initial messages = %v", first.messages["child"])
	}
	if _, err := w.Exec(`INSERT INTO message VALUES ('committed','child',2,2,'{}')`); err != nil {
		t.Fatal(err)
	}

	second, err := db.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := messageIDs(second.messages["child"]); got != "committed" {
		t.Fatalf("committed WAL messages = %q", got)
	}
	handle, exists, err := openDatabase(context.Background(), path)
	if err != nil || !exists {
		t.Fatalf("open = %v, %v", exists, err)
	}
	defer handle.Close()
	if _, err := handle.Exec(`DELETE FROM message`); err == nil {
		t.Fatal("WAL reader accepted a logical write")
	}
	var count int
	if err := w.QueryRow(`SELECT count(*) FROM message`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("logical message count = %d, want 1", count)
	}
}

func TestIncompatibleSchemaReturnsErrorAndClearsConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	w, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	db := newDatabase(path)
	_, err = db.snapshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "parent_id") {
		t.Fatalf("incompatible schema error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove incompatible database after snapshot error: %v", err)
	}
}

func TestReplacedDatabaseIsReopened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	w := fixtureDB(t, path)
	insertSession(t, w, "old", "parent", 1)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(dir, "replacement.db")
	replacement := fixtureDB(t, replacementPath)
	insertSession(t, replacement, "new", "parent", 2)
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}

	db := newDatabase(path)
	if snap, err := db.snapshot(context.Background()); err != nil || rowIDs(snap.sessions) != "old" {
		t.Fatalf("initial snapshot = %q, %v", rowIDs(snap.sessions), err)
	}
	oldPath := filepath.Join(dir, "old.db")
	if err := os.Rename(path, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}

	snap, err := db.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := rowIDs(snap.sessions); got != "new" {
		t.Fatalf("replacement snapshot = %q, want new", got)
	}
}

func TestQueryHelpersShareOneTransactionSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	w := fixtureDB(t, path)
	if _, err := w.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	insertSession(t, w, "child", "parent", 1)
	if _, err := w.Exec(`INSERT INTO message VALUES ('before','child',1,1,'{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`INSERT INTO part VALUES ('part-before','before','child',1,1,'{}')`); err != nil {
		t.Fatal(err)
	}

	db := newDatabase(path)
	handle, exists, err := openDatabase(context.Background(), path)
	if err != nil || !exists {
		t.Fatalf("open = %v, %v", exists, err)
	}
	defer handle.Close()
	tx, err := handle.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := querySessions(context.Background(), tx); err != nil {
		t.Fatal(err)
	}

	committed := make(chan error, 1)
	go func() {
		_, err := w.Exec(`
			INSERT INTO message VALUES ('after','child',2,2,'{}');
			INSERT INTO part VALUES ('part-after','after','child',2,2,'{}');`)
		committed <- err
	}()
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	rows, err := queryMessages(context.Background(), tx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if got := messageIDs(rows); got != "before" {
		t.Fatalf("transaction mixed snapshots: %q", got)
	}
	parts, err := queryParts(context.Background(), tx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if got := partIDs(parts); got != "part-before" {
		t.Fatalf("transaction mixed part snapshots: %q", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	snap, err := db.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := messageIDs(snap.messages["child"]); got != "before,after" {
		t.Fatalf("next transaction did not see commit: %q", got)
	}
	if got := partIDs(snap.parts["child"]); got != "part-before,part-after" {
		t.Fatalf("next transaction did not see committed part: %q", got)
	}
}

func TestMissingDatabaseAfterRemovalIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	w := fixtureDB(t, path)
	insertSession(t, w, "child", "parent", 1)
	db := newDatabase(path)
	if _, err := db.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := db.snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func rowIDs(rows []sessionRow) string {
	var result string
	for _, row := range rows {
		if result != "" {
			result += ","
		}
		result += row.id
	}
	return result
}

func messageIDs(rows []messageRow) string {
	var result string
	for _, row := range rows {
		if result != "" {
			result += ","
		}
		result += row.id
	}
	return result
}

func partIDs(rows []partRow) string {
	var result string
	for _, row := range rows {
		if result != "" {
			result += ","
		}
		result += row.id
	}
	return result
}
