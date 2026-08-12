package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
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

func insertMessage(t *testing.T, db *sql.DB, sessionID, id string, created, updated int64, data string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES (?,?,?,?,?)`, id, sessionID, created, updated, data); err != nil {
		t.Fatal(err)
	}
}

func insertPart(t *testing.T, db *sql.DB, sessionID, messageID, id string, created, updated int64, data string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO part (id,message_id,session_id,time_created,time_updated,data) VALUES (?,?,?,?,?,?)`, id, messageID, sessionID, created, updated, data); err != nil {
		t.Fatal(err)
	}
}

func updatesByNativeID(updates []provider.Update) map[string]provider.Update {
	result := make(map[string]provider.Update, len(updates))
	for _, update := range updates {
		result[update.Agent.NativeID] = update
	}
	return result
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

func TestNormalizeRepresentativeOpenCodeRows(t *testing.T) {
	sessions := []sessionRow{
		{id: "ses_parent", parentID: "ses_root"},
		{
			id: "ses_child", parentID: "ses_parent", directory: "/work/tree",
			title: "Audit parser (@general subagent)", version: "1.15.7", agent: "general",
			model: "gpt-5.6-sol", timeCreated: 900, timeUpdated: 1700,
			tokensInput: 100, tokensOutput: 20, tokensReasoning: 5,
			tokensCacheRead: 50, tokensCacheWrite: 2,
		},
	}
	messages := []messageRow{
		{id: "msg_user", sessionID: "ses_child", timeCreated: 1000, timeUpdated: 1001, data: `{"role":"user","time":{"created":1000},"agent":"general","model":{"providerID":"openai","modelID":"gpt-5.6-sol"},"future":true}`},
		{id: "msg_assistant", sessionID: "ses_child", timeCreated: 1100, timeUpdated: 1900, data: `{"role":"assistant","time":{"created":1100,"completed":1900},"parentID":"msg_user","providerID":"openai","modelID":"gpt-5.6-sol","agent":"general","finish":"stop","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":50,"write":0}},"unknown":{"x":1}}`},
	}
	parts := []partRow{
		{id: "part_user", messageID: "msg_user", sessionID: "ses_child", timeCreated: 1000, timeUpdated: 1001, data: `{"type":"text","text":"audit the parser","time":{"start":1000,"end":1001}}`},
		{id: "part_synthetic", messageID: "msg_user", sessionID: "ses_child", timeCreated: 1002, timeUpdated: 1003, data: `{"type":"text","text":"hidden context","synthetic":true,"time":{"start":1002,"end":1003}}`},
		{id: "part_reasoning", messageID: "msg_assistant", sessionID: "ses_child", timeCreated: 1200, timeUpdated: 1250, data: `{"type":"reasoning","text":"inspect rows","time":{"start":1200,"end":1250}}`},
		{id: "part_tool", messageID: "msg_assistant", sessionID: "ses_child", timeCreated: 1300, timeUpdated: 1500, data: `{"type":"tool","callID":"call_1","tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok\nFull output saved to: /data/tool-output/call_1","title":"Run tests","metadata":{},"time":{"start":1300,"end":1500}}}`},
		{id: "part_unknown", messageID: "msg_assistant", sessionID: "ses_child", timeCreated: 1400, timeUpdated: 1400, data: `{"type":"snapshot","future":"field"}`},
		{id: "part_text", messageID: "msg_assistant", sessionID: "ses_child", timeCreated: 1600, timeUpdated: 1800, data: `{"type":"text","text":"All checks pass.","time":{"start":1600,"end":1800}}`},
	}

	got, err := normalizeSession(sessions[1], sessions, messages, parts, "/data/opencode.db", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := got.agent
	if a.ID != "opencode:ses_child" || a.NativeID != "ses_child" || a.Provider != "opencode" {
		t.Fatalf("agent IDs = %q/%q/%q", a.ID, a.NativeID, a.Provider)
	}
	if a.Name != "general" || a.Title != "Audit parser" || a.Parent != "ses_parent" || a.Depth != 2 {
		t.Fatalf("agent identity mapping = %+v", a)
	}
	if a.Cwd != "/work/tree" || a.CLIVersion != "1.15.7" || a.Model != "gpt-5.6-sol" || a.Source != "/data/opencode.db" {
		t.Fatalf("agent source mapping = %+v", a)
	}
	if a.Prompt != "audit the parser" || a.ToolCount != 1 {
		t.Fatalf("prompt/tool count = %q/%d", a.Prompt, a.ToolCount)
	}
	wantTokens := model.Tokens{In: 100, Out: 20, Reasoning: 5, CacheRead: 50, CacheWrite: 2, Total: 120}
	if a.Tokens != wantTokens {
		t.Fatalf("tokens = %+v, want %+v", a.Tokens, wantTokens)
	}
	if !a.Started.Equal(time.UnixMilli(900)) || !a.Updated.Equal(time.UnixMilli(1900)) {
		t.Fatalf("agent times = %v/%v", a.Started, a.Updated)
	}
	wantKinds := []model.EventKind{model.EvUser, model.EvReasoning, model.EvToolUse, model.EvToolResult, model.EvText}
	if gotKinds := normalizedKinds(got.events); !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", gotKinds, wantKinds)
	}
	for _, event := range got.events {
		if event.AgentID != a.ID || event.Header == "" {
			t.Fatalf("incomplete event: %+v", event)
		}
	}
	toolUse, toolResult := got.events[2], got.events[3]
	if toolUse.Tool != "bash" || !strings.Contains(toolUse.Header, "bash") || !strings.Contains(toolUse.Body, "go test ./...") {
		t.Fatalf("tool use = %+v", toolUse)
	}
	if toolResult.Err || toolResult.Lines != 2 || toolResult.Overflow == nil || toolResult.Overflow.Path != "/data/tool-output/call_1" {
		t.Fatalf("tool result = %+v", toolResult)
	}
	if got.status != model.StatusDone || !got.statusTime.Equal(time.UnixMilli(1900)) {
		t.Fatalf("authoritative status = %q at %v", got.status, got.statusTime)
	}
}

func TestNormalizeAgentFallbackTitleAndDepthErrors(t *testing.T) {
	session := sessionRow{id: "child", parentID: "missing", title: "Task (@general subagent) extra", model: "exact-model", timeCreated: 1}
	messages := []messageRow{{id: "m", timeCreated: 2, data: `{"role":"assistant","agent":"general","modelID":"fallback-model"}`}}
	got, err := normalizeSession(session, []sessionRow{session}, messages, nil, "db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.agent.Name != "general" || got.agent.Title != session.title || got.agent.Depth != 1 || got.agent.Model != "exact-model" {
		t.Fatalf("fallback/exact mapping = %+v", got.agent)
	}

	cycle := []sessionRow{{id: "a", parentID: "b"}, {id: "b", parentID: "a"}}
	if _, err := normalizeSession(cycle[0], cycle, nil, nil, "db", nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestNormalizeOrderingFallbacksAndAtomicTerminalTool(t *testing.T) {
	session := sessionRow{id: "child", parentID: "parent", timeCreated: 1}
	messages := []messageRow{
		{id: "u", timeCreated: 2, data: `{"role":"user"}`},
		{id: "a", timeCreated: 3, data: `{"role":"assistant"}`},
	}
	parts := []partRow{
		{id: "z-user", messageID: "u", timeCreated: 100, timeUpdated: 101, data: `{"type":"text","text":"user"}`},
		{id: "b-reasoning", messageID: "a", timeCreated: 200, timeUpdated: 201, data: `{"type":"reasoning","text":"reason","time":{"end":201}}`},
		{id: "a-text", messageID: "a", timeCreated: 200, timeUpdated: 201, data: `{"type":"text","text":"text","time":{"end":201}}`},
		{id: "tool", messageID: "a", timeCreated: 300, timeUpdated: 500, data: `{"type":"tool","tool":"bash","state":{"status":"completed","input":{},"output":"done"}}`},
		{id: "between", messageID: "a", timeCreated: 400, timeUpdated: 401, data: `{"type":"text","text":"between","time":{"start":400,"end":401}}`},
	}
	got, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantBodies := []string{"user", "text", "reason", "{}", "done", "between"}
	if bodies := normalizedBodies(got.events); !reflect.DeepEqual(bodies, wantBodies) {
		t.Fatalf("ordered bodies = %#v, want %#v", bodies, wantBodies)
	}
	wantTimes := []int64{100, 200, 200, 300, 500, 400}
	for i, want := range wantTimes {
		if got.events[i].TS.UnixMilli() != want {
			t.Fatalf("event %d time = %d, want %d", i, got.events[i].TS.UnixMilli(), want)
		}
	}
}

func TestNormalizeIncompleteMutationAndRepeatedState(t *testing.T) {
	session := sessionRow{id: "child", parentID: "parent", timeCreated: 1}
	messages := []messageRow{
		{id: "u", timeCreated: 2, data: `{"role":"user"}`},
		{id: "a", timeCreated: 3, data: `{"role":"assistant"}`},
	}
	parts := []partRow{
		{id: "user", messageID: "u", timeCreated: 10, timeUpdated: 10, data: `{"type":"text","text":"prompt"}`},
		{id: "text", messageID: "a", timeCreated: 20, timeUpdated: 20, data: `{"type":"text","text":"draft","time":{"start":20}}`},
		{id: "reason", messageID: "a", timeCreated: 30, timeUpdated: 30, data: `{"type":"reasoning","text":"draft","time":{"start":30}}`},
		{id: "tool", messageID: "a", timeCreated: 40, timeUpdated: 40, data: `{"type":"tool","tool":"read","state":{"status":"running","input":{"file_path":"a"},"time":{"start":40}}}`},
	}
	first, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if kinds := normalizedKinds(first.events); !reflect.DeepEqual(kinds, []model.EventKind{model.EvUser, model.EvToolUse}) {
		t.Fatalf("initial kinds = %v", kinds)
	}
	if first.agent.ToolCount != 1 || first.state["text"].textDone || first.state["reason"].reasoningDone {
		t.Fatalf("initial state = %+v, agent = %+v", first.state, first.agent)
	}

	repeated, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", first.state)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.events) != 0 || repeated.agent.ToolCount != 1 {
		t.Fatalf("repeated normalization emitted %+v", repeated)
	}

	parts[1].data = `{"type":"text","text":"final","time":{"start":20,"end":25}}`
	parts[2].data = `{"type":"reasoning","text":"final reason","time":{"start":30,"end":35}}`
	parts[3].data = `{"type":"tool","tool":"read","state":{"status":"error","input":{"file_path":"a"},"error":"permission denied","time":{"start":40,"end":45}}}`
	completed, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", repeated.state)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []model.EventKind{model.EvText, model.EvReasoning, model.EvToolResult}
	if kinds := normalizedKinds(completed.events); !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("completion kinds = %v", kinds)
	}
	if !completed.events[2].Err || completed.events[2].Body != "permission denied" || completed.agent.ToolCount != 1 {
		t.Fatalf("errored tool transition = %+v, count %d", completed.events[2], completed.agent.ToolCount)
	}
}

func TestNormalizeFingerprintChangesPreserveEmittedPhases(t *testing.T) {
	session := sessionRow{id: "child", parentID: "parent", timeCreated: 1}
	messages := []messageRow{
		{id: "u", timeCreated: 2, data: `{"role":"user"}`},
		{id: "a", timeCreated: 3, data: `{"role":"assistant"}`},
	}
	parts := []partRow{
		{id: "user", messageID: "u", timeCreated: 10, data: `{"type":"text","text":"prompt"}`},
		{id: "text", messageID: "a", timeCreated: 20, data: `{"type":"text","text":"answer","time":{"end":21}}`},
		{id: "reason", messageID: "a", timeCreated: 30, data: `{"type":"reasoning","text":"thought","time":{"end":31}}`},
		{id: "tool", messageID: "a", timeCreated: 40, data: `{"type":"tool","tool":"read","state":{"status":"completed","input":{"file_path":"a"},"output":"old"}}`},
	}
	first, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if kinds := normalizedKinds(first.events); !reflect.DeepEqual(kinds, []model.EventKind{
		model.EvUser, model.EvText, model.EvReasoning, model.EvToolUse, model.EvToolResult,
	}) {
		t.Fatalf("initial kinds = %v", kinds)
	}

	parts[0].data = `{"type":"text","text":"changed prompt"}`
	parts[1].data = `{"type":"text","text":"changed answer","time":{"end":21}}`
	parts[2].data = `{"type":"reasoning","text":"changed thought","time":{"end":31}}`
	parts[3].data = `{"type":"tool","tool":"read","state":{"status":"completed","input":{"file_path":"b"},"output":"new"}}`
	changed, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", first.state)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed.events) != 0 {
		t.Fatalf("fingerprint-only changes repeated emitted phases: %+v", changed.events)
	}
	if state := changed.state["user"]; !state.user {
		t.Fatalf("user phase was cleared: %+v", state)
	}
	if state := changed.state["text"]; !state.textDone {
		t.Fatalf("text phase was cleared: %+v", state)
	}
	if state := changed.state["reason"]; !state.reasoningDone {
		t.Fatalf("reasoning phase was cleared: %+v", state)
	}
	if state := changed.state["tool"]; !state.toolUse || !state.toolResult {
		t.Fatalf("tool phases were cleared: %+v", state)
	}
}

func TestNormalizeDeferredUserTextEmitsWhenEligible(t *testing.T) {
	tests := []struct {
		name, initial, eligible string
	}{
		{
			name:     "synthetic becomes eligible",
			initial:  `{"type":"text","text":"prompt","synthetic":true,"time":{"start":10,"end":11}}`,
			eligible: `{"type":"text","text":"prompt","synthetic":false,"time":{"start":10,"end":11}}`,
		},
		{
			name:     "empty becomes populated",
			initial:  `{"type":"text","text":"","time":{"start":10,"end":11}}`,
			eligible: `{"type":"text","text":"prompt","time":{"start":10,"end":11}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := sessionRow{id: "child", parentID: "parent", timeCreated: 1}
			message := messageRow{id: "user-message", timeCreated: 2, data: `{"role":"user"}`}
			part := partRow{id: "user-part", messageID: message.id, timeCreated: 10, timeUpdated: 11, data: tt.initial}

			first, err := normalizeSession(session, []sessionRow{session}, []messageRow{message}, []partRow{part}, "db", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.events) != 0 || first.state[part.id].user {
				t.Fatalf("ineligible row emitted or completed user phase: events=%+v state=%+v", first.events, first.state[part.id])
			}

			part.data = tt.eligible
			second, err := normalizeSession(session, []sessionRow{session}, []messageRow{message}, []partRow{part}, "db", first.state)
			if err != nil {
				t.Fatal(err)
			}
			if len(second.events) != 1 || second.events[0].Kind != model.EvUser || second.events[0].Body != "prompt" {
				t.Fatalf("eligible transition events = %+v", second.events)
			}
			if !second.state[part.id].user {
				t.Fatalf("emitted user phase not recorded: %+v", second.state[part.id])
			}

			third, err := normalizeSession(session, []sessionRow{session}, []messageRow{message}, []partRow{part}, "db", second.state)
			if err != nil {
				t.Fatal(err)
			}
			if len(third.events) != 0 {
				t.Fatalf("eligible user event repeated: %+v", third.events)
			}
		})
	}
}

func TestNormalizeUnsupportedPartIgnoresConflictingSupportedFields(t *testing.T) {
	session := sessionRow{id: "child", parentID: "parent", timeCreated: 1}
	message := messageRow{id: "a", timeCreated: 2, data: `{"role":"assistant"}`}
	part := partRow{
		id: "unsupported", messageID: "a", timeCreated: 3,
		data: `{"type":"snapshot","text":42,"synthetic":"wrong","state":{"input":"wrong","output":false}}`,
	}
	got, err := normalizeSession(session, []sessionRow{session}, []messageRow{message}, []partRow{part}, "db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.events) != 0 || got.state[part.id].malformed != "" {
		t.Fatalf("unsupported valid part was decoded as supported: events=%+v state=%+v", got.events, got.state[part.id])
	}
}

func TestNormalizeMalformedNewestMessageOverridesOlderTerminal(t *testing.T) {
	session := sessionRow{id: "child", parentID: "parent", timeCreated: 1}
	messages := []messageRow{
		{id: "a-done", timeCreated: 100, timeUpdated: 110, data: `{"role":"assistant","time":{"completed":110},"finish":"stop"}`},
		{id: "z-malformed", timeCreated: 100, timeUpdated: 120, data: `{"role":`},
	}
	first, err := normalizeSession(session, []sessionRow{session}, messages, nil, "db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.status != model.StatusLive || !first.statusTime.Equal(time.UnixMilli(120)) {
		t.Fatalf("malformed newest authority = %q at %v, want live at 120ms", first.status, first.statusTime)
	}
	if len(first.events) != 1 || first.events[0].Kind != model.EvNotice || !first.events[0].Err {
		t.Fatalf("malformed newest notice = %+v", first.events)
	}
	second, err := normalizeSession(session, []sessionRow{session}, messages, nil, "db", first.state)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.events) != 0 || second.status != model.StatusLive {
		t.Fatalf("malformed newest repeated result = %+v", second)
	}
}

func TestNormalizeAssistantErrorPrecedenceAndMalformedDedup(t *testing.T) {
	session := sessionRow{id: "child", parentID: "parent", timeCreated: 1}
	messages := []messageRow{
		{id: "bad-message", timeCreated: 10, timeUpdated: 11, data: `{"role":`},
		{id: "error", timeCreated: 20, timeUpdated: 21, data: `{"role":"assistant","time":{"completed":25},"finish":"error","error":{"name":"FallbackName","message":"fallback message","data":{"message":"preferred detail"},"code":500}}`},
	}
	parts := []partRow{
		{id: "bad-part", messageID: "error", timeCreated: 30, timeUpdated: 31, data: `{"type":"text","text":42}`},
		{id: "unknown", messageID: "error", timeCreated: 32, timeUpdated: 32, data: `{"type":"future","value":42}`},
	}
	first, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.events) != 3 {
		t.Fatalf("events = %+v", first.events)
	}
	if first.events[0].Kind != model.EvNotice || !first.events[0].Err || !strings.Contains(first.events[0].Body, "malformed") {
		t.Fatalf("malformed message notice = %+v", first.events[0])
	}
	if first.events[1].Body != "preferred detail" || !first.events[1].Err {
		t.Fatalf("assistant error notice = %+v", first.events[1])
	}
	if first.events[2].Kind != model.EvNotice || !strings.Contains(first.events[2].Body, "malformed") {
		t.Fatalf("malformed part notice = %+v", first.events[2])
	}
	if first.status != model.StatusError || first.statusTime.UnixMilli() != 25 {
		t.Fatalf("error status = %q at %v", first.status, first.statusTime)
	}

	second, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", first.state)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.events) != 0 {
		t.Fatalf("malformed/error notices repeated: %+v", second.events)
	}
	parts[0].data = `{"type":"text","text":false}`
	changed, err := normalizeSession(session, []sessionRow{session}, messages, parts, "db", second.state)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed.events) != 1 || changed.events[0].Kind != model.EvNotice {
		t.Fatalf("changed malformed row was not detected: %+v", changed.events)
	}
}

func TestNormalizeAssistantErrorMappingPrecedence(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"data message", `{"data":{"message":"data"},"message":"message","name":"name"}`, "data"},
		{"message", `{"message":"message","name":"name"}`, "message"},
		{"name", `{"name":"name","code":500}`, "name"},
		{"formatted JSON", `{"code":500}`, "{\n  \"code\": 500\n}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assistantError(json.RawMessage(tt.raw)); got != tt.want {
				t.Fatalf("assistantError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPollAdoptsChildrenWithDepthAndStrictBacklogActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db := fixtureDB(t, path)
	since := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	cutoff := since.UnixMilli()
	insertSession(t, db, "root", "", cutoff-100)
	insertSession(t, db, "before", "root", cutoff-100)
	insertSession(t, db, "equal", "root", cutoff-100)
	insertSession(t, db, "nested", "equal", cutoff-100)
	insertSession(t, db, "post-activity", "root", cutoff-100)
	if _, err := db.Exec(`UPDATE session SET time_updated=? WHERE id='before'`, cutoff-1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE session SET time_updated=? WHERE id IN ('equal','nested')`, cutoff); err != nil {
		t.Fatal(err)
	}
	insertMessage(t, db, "post-activity", "post-message", cutoff+1, cutoff+1, `{"role":"user"}`)

	p := New(path, 0, since)
	if p.Name() != Name || Name != "opencode" {
		t.Fatalf("provider name = %q, constant = %q", p.Name(), Name)
	}
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	got := updatesByNativeID(updates)
	if len(got) != 4 {
		t.Fatalf("adopted IDs = %v, want four children and no root", reflect.ValueOf(got).MapKeys())
	}
	if !got["before"].Agent.Backlog || got["before"].Agent.Status != model.StatusDone || len(got["before"].Events) != 0 {
		t.Fatalf("before cutoff = %+v", got["before"])
	}
	if got["before"].Agent.Updated.UnixMilli() != cutoff-1 || got["before"].Agent.DurationMS != 99 || got["before"].Agent.EventCount != 0 || got["before"].Agent.ToolCount != 0 {
		t.Fatalf("deterministic backlog counts/times = %+v", got["before"].Agent)
	}
	for _, id := range []string{"equal", "nested", "post-activity"} {
		if got[id].Agent.Backlog {
			t.Fatalf("%s was incorrectly backlog: %+v", id, got[id].Agent)
		}
	}
	if got["equal"].Agent.Depth != 1 || got["nested"].Agent.Depth != 2 {
		t.Fatalf("depths direct/nested = %d/%d", got["equal"].Agent.Depth, got["nested"].Agent.Depth)
	}
}

func TestPollStreamsMutablePhasesAndLifecycleOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db := fixtureDB(t, path)
	base := time.Now().Add(-time.Second).Truncate(time.Millisecond).UnixMilli()
	insertSession(t, db, "child", "parent", base)
	insertMessage(t, db, "child", "user", base+10, base+10, `{"role":"user","time":{"created":1}}`)
	insertMessage(t, db, "child", "assistant", base+20, base+20, `{"role":"assistant","finish":"tool-calls"}`)
	insertPart(t, db, "child", "user", "user-part", base+10, base+10, `{"type":"text","text":"run checks"}`)
	insertPart(t, db, "child", "assistant", "text", base+30, base+30, fmt.Sprintf(`{"type":"text","text":"draft","time":{"start":%d}}`, base+30))
	insertPart(t, db, "child", "assistant", "reason", base+35, base+35, fmt.Sprintf(`{"type":"reasoning","text":"thinking","time":{"start":%d}}`, base+35))
	insertPart(t, db, "child", "assistant", "tool", base+40, base+40, fmt.Sprintf(`{"type":"tool","tool":"bash","state":{"status":"running","input":{"command":"go test ./..."},"time":{"start":%d}}}`, base+40))

	p := New(path, 0, time.UnixMilli(base-1))
	first, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || !reflect.DeepEqual(normalizedKinds(first[0].Events), []model.EventKind{model.EvUser, model.EvToolUse}) {
		t.Fatalf("first poll = %+v", first)
	}
	if first[0].Agent.Status != model.StatusLive || first[0].Agent.ToolCount != 1 || first[0].Agent.EventCount != 2 {
		t.Fatalf("first agent counts/status = %+v", first[0].Agent)
	}

	completed := base + 100
	if _, err := db.Exec(`UPDATE part SET data=? WHERE id='text'`, fmt.Sprintf(`{"type":"text","text":"final","time":{"start":%d,"end":%d}}`, base+30, base+60)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE part SET data=? WHERE id='reason'`, fmt.Sprintf(`{"type":"reasoning","text":"thought","time":{"start":%d,"end":%d}}`, base+35, base+65)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE part SET data=? WHERE id='tool'`, fmt.Sprintf(`{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok","time":{"start":%d,"end":%d}}}`, base+40, base+70)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE message SET data=? WHERE id='assistant'`, fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"stop"}`, completed)); err != nil {
		t.Fatal(err)
	}
	second, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []model.EventKind{model.EvText, model.EvReasoning, model.EvToolResult, model.EvStatus}
	if len(second) != 1 || !reflect.DeepEqual(normalizedKinds(second[0].Events), wantKinds) {
		t.Fatalf("completion poll kinds = %v, want %v", normalizedKinds(second[0].Events), wantKinds)
	}
	statusEvent := second[0].Events[len(second[0].Events)-1]
	if statusEvent.TS.UnixMilli() != completed || second[0].Agent.Status != model.StatusDone || second[0].Agent.EventCount != 6 {
		t.Fatalf("completion status/count = %+v, event=%+v", second[0].Agent, statusEvent)
	}
	if second[0].Agent.Updated.UnixMilli() != base+40 || second[0].Agent.DurationMS != 40 {
		t.Fatalf("deterministic updated/duration = %v/%d", second[0].Agent.Updated, second[0].Agent.DurationMS)
	}
	third, err := p.Poll()
	if err != nil || len(third) != 0 {
		t.Fatalf("repeated poll = %+v, %v", third, err)
	}

	insertMessage(t, db, "child", "resumed-user", base+200, base+200, `{"role":"user"}`)
	insertPart(t, db, "child", "resumed-user", "resumed-text", base+200, base+200, `{"type":"text","text":"continue"}`)
	resumed, err := p.Poll()
	if err != nil || len(resumed) != 1 {
		t.Fatalf("resumed poll = %+v, %v", resumed, err)
	}
	if resumed[0].Agent.Status != model.StatusLive || resumed[0].Agent.Backlog {
		t.Fatalf("resumed agent = %+v", resumed[0].Agent)
	}
	if kinds := normalizedKinds(resumed[0].Events); !reflect.DeepEqual(kinds, []model.EventKind{model.EvUser, model.EvStatus}) {
		t.Fatalf("resumed events = %v", kinds)
	}
}

func TestPollNewestMessageLifecycleVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db := fixtureDB(t, path)
	base := time.Now().Add(-time.Second).Truncate(time.Millisecond).UnixMilli()
	tests := []struct {
		id, data string
		want     model.Status
	}{
		{"stop", fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"stop"}`, base+100), model.StatusDone},
		{"length", fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"length"}`, base+101), model.StatusDone},
		{"unknown", fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"unknown"}`, base+102), model.StatusDone},
		{"finish-error", fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"error"}`, base+103), model.StatusError},
		{"filter", fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"content-filter"}`, base+104), model.StatusError},
		{"object-error", fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"stop","error":{"message":"bad"}}`, base+105), model.StatusError},
		{"tool-calls", `{"role":"assistant","finish":"tool-calls"}`, model.StatusLive},
		{"user", `{"role":"user"}`, model.StatusLive},
		{"incomplete", `{"role":"assistant"}`, model.StatusLive},
		{"malformed", `{"role":`, model.StatusLive},
	}
	for i, tt := range tests {
		created := base + int64(i)
		insertSession(t, db, tt.id, "parent", base)
		insertMessage(t, db, tt.id, tt.id+"-old", created-1, created-1, fmt.Sprintf(`{"role":"assistant","time":{"completed":%d},"finish":"stop"}`, base+50))
		insertMessage(t, db, tt.id, tt.id+"-new", created, created, tt.data)
	}

	updates, err := New(path, 0, time.UnixMilli(base-1)).Poll()
	if err != nil {
		t.Fatal(err)
	}
	got := updatesByNativeID(updates)
	for _, tt := range tests {
		u := got[tt.id]
		if u.Agent.Status != tt.want {
			t.Errorf("%s status = %q, want %q", tt.id, u.Agent.Status, tt.want)
		}
		statusCount := 0
		for _, event := range u.Events {
			if event.Kind == model.EvStatus {
				statusCount++
			}
		}
		wantStatusEvents := 0
		if tt.want != model.StatusLive {
			wantStatusEvents = 1
		}
		if statusCount != wantStatusEvents {
			t.Errorf("%s status events = %d, want %d: %+v", tt.id, statusCount, wantStatusEvents, u.Events)
		}
	}
}

func TestBacklogResumesWithoutReplayingExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db := fixtureDB(t, path)
	since := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	old := since.Add(-time.Second).UnixMilli()
	insertSession(t, db, "child", "parent", old)
	if _, err := db.Exec(`UPDATE session SET time_updated=? WHERE id='child'`, old); err != nil {
		t.Fatal(err)
	}
	insertMessage(t, db, "child", "old-user", old-2, old-2, `{"role":"user","agent":"build","model":{"modelID":"gpt-5"}}`)
	insertPart(t, db, "child", "old-user", "old-prompt", old-2, old-2, `{"type":"text","text":"historical prompt"}`)
	insertMessage(t, db, "child", "old-message", old, old, `{"role":"assistant"}`)
	insertPart(t, db, "child", "old-message", "old-text", old, old, fmt.Sprintf(`{"type":"text","text":"old draft","time":{"start":%d}}`, old))
	insertPart(t, db, "child", "old-message", "old-tool", old, old, fmt.Sprintf(`{"type":"tool","tool":"read","state":{"status":"completed","input":{"file_path":"a"},"output":"ok","time":{"start":%d,"end":%d}}}`, old, old))

	p := New(path, 0, since)
	first, err := p.Poll()
	if err != nil || len(first) != 1 || !first[0].Agent.Backlog || len(first[0].Events) != 0 {
		t.Fatalf("initial backlog = %+v, %v", first, err)
	}
	if first[0].Agent.Prompt != "historical prompt" || first[0].Agent.Model != "gpt-5" || first[0].Agent.ToolCount != 1 || first[0].Agent.EventCount != 3 {
		t.Fatalf("backlog metadata/counts = %+v", first[0].Agent)
	}
	unchanged, err := p.Poll()
	if err != nil || len(unchanged) != 0 {
		t.Fatalf("unchanged backlog poll = %+v, %v", unchanged, err)
	}
	if _, err := db.Exec(`UPDATE part SET time_updated=?,data=? WHERE id='old-text'`, since.UnixMilli()+1, fmt.Sprintf(`{"type":"text","text":"old final","time":{"start":%d,"end":%d}}`, old, since.UnixMilli()+1)); err != nil {
		t.Fatal(err)
	}
	insertMessage(t, db, "child", "new-user", since.UnixMilli()+2, since.UnixMilli()+2, `{"role":"user"}`)
	insertPart(t, db, "child", "new-user", "new-text", since.UnixMilli()+2, since.UnixMilli()+2, `{"type":"text","text":"continue"}`)
	second, err := p.Poll()
	if err != nil || len(second) != 1 {
		t.Fatalf("resume = %+v, %v", second, err)
	}
	if second[0].Agent.Backlog || second[0].Agent.Status != model.StatusLive {
		t.Fatalf("resumed agent = %+v", second[0].Agent)
	}
	if second[0].Agent.ToolCount != 1 || second[0].Agent.EventCount != 5 {
		t.Fatalf("resumed counts = %+v", second[0].Agent)
	}
	if kinds := normalizedKinds(second[0].Events); !reflect.DeepEqual(kinds, []model.EventKind{model.EvUser, model.EvStatus}) {
		t.Fatalf("resume events = %v, want only new user and live status", kinds)
	}
}

func TestPollIdleFallbackAndMalformedNoticeOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db := fixtureDB(t, path)
	base := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	insertSession(t, db, "child", "parent", base.UnixMilli())
	insertMessage(t, db, "child", "bad", base.UnixMilli()+1, base.UnixMilli()+1, `{"role":`)
	p := New(path, 100*time.Millisecond, base.Add(-time.Second))
	first, err := p.Poll()
	if err != nil || len(first) != 1 {
		t.Fatalf("idle poll = %+v, %v", first, err)
	}
	if first[0].Agent.Status != model.StatusDone || first[0].Agent.DurationMS != 1 {
		t.Fatalf("idle agent = %+v", first[0].Agent)
	}
	if kinds := normalizedKinds(first[0].Events); !reflect.DeepEqual(kinds, []model.EventKind{model.EvNotice, model.EvStatus}) {
		t.Fatalf("idle/malformed events = %v", kinds)
	}
	if got := first[0].Events[1].TS; !got.Equal(base.Add(time.Millisecond + 100*time.Millisecond)) {
		t.Fatalf("idle status time = %v", got)
	}
	second, err := p.Poll()
	if err != nil || len(second) != 0 {
		t.Fatalf("repeated malformed/idle poll = %+v, %v", second, err)
	}
}

func TestPollDatabaseRecoveryReplacementAndBusyTimeout(t *testing.T) {
	t.Run("missing and corrupt recovery", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "opencode.db")
		p := New(path, 0, time.Time{})
		if updates, err := p.Poll(); err != nil || len(updates) != 0 {
			t.Fatalf("missing poll = %+v, %v", updates, err)
		}
		if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Poll(); err == nil {
			t.Fatal("corrupt database poll succeeded")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		db := fixtureDB(t, path)
		insertSession(t, db, "recovered", "parent", time.Now().UnixMilli())
		if updates, err := p.Poll(); err != nil || len(updates) != 1 || updates[0].Agent.NativeID != "recovered" {
			t.Fatalf("recovery poll = %+v, %v", updates, err)
		}
	})

	t.Run("valid open replacement", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "opencode.db")
		db := fixtureDB(t, path)
		insertSession(t, db, "old", "parent", time.Now().UnixMilli())
		p := New(path, 0, time.Time{})
		if updates, err := p.Poll(); err != nil || len(updates) != 1 {
			t.Fatalf("initial poll = %+v, %v", updates, err)
		}
		replacementPath := filepath.Join(dir, "replacement.db")
		replacement := fixtureDB(t, replacementPath)
		insertSession(t, replacement, "new", "parent", time.Now().UnixMilli())
		if err := replacement.Close(); err != nil {
			t.Fatal(err)
		}
		oldPath := filepath.Join(dir, "old.db")
		if err := os.Rename(path, oldPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacementPath, path); err != nil {
			t.Fatal(err)
		}
		if updates, err := p.Poll(); err != nil || len(updates) != 1 || updates[0].Agent.NativeID != "new" {
			t.Fatalf("replacement poll = %+v, %v", updates, err)
		}
	})

	t.Run("busy timeout recovers", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opencode.db")
		db := fixtureDB(t, path)
		insertSession(t, db, "child", "parent", time.Now().UnixMilli())
		if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
			t.Fatal(err)
		}
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), `UPDATE session SET title='locked' WHERE id='child'`); err != nil {
			t.Fatal(err)
		}
		p := New(path, 0, time.Time{})
		start := time.Now()
		if _, err := p.Poll(); err == nil {
			t.Fatal("poll under exclusive writer lock succeeded")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("busy timeout took %v", elapsed)
		}
		if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		if updates, err := p.Poll(); err != nil || len(updates) != 1 {
			t.Fatalf("post-lock recovery = %+v, %v", updates, err)
		}
	})
}

func TestLargeBacklogPollIsEventlessAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db := fixtureDB(t, path)
	old := time.Now().Add(-time.Hour).UnixMilli()
	insertSession(t, db, "child", "parent", old)
	insertMessage(t, db, "child", "assistant", old, old, `{"role":"assistant","time":{"completed":1},"finish":"stop"}`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO part (id,message_id,session_id,time_created,time_updated,data) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("part-%05d", i), "assistant", "child", old, old, `{"type":"text","text":"historical","time":{"end":1}}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct {
		updates []provider.Update
		err     error
	}, 1)
	go func() {
		updates, err := New(path, 0, time.Now()).Poll()
		done <- struct {
			updates []provider.Update
			err     error
		}{updates, err}
	}()
	select {
	case result := <-done:
		if result.err != nil || len(result.updates) != 1 || len(result.updates[0].Events) != 0 {
			t.Fatalf("large backlog = %+v, %v", result.updates, result.err)
		}
		if result.updates[0].Agent.EventCount != 5000 {
			t.Fatalf("backlog EventCount = %d, want 5000", result.updates[0].Agent.EventCount)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("large backlog poll exceeded five seconds")
	}
}

func normalizedKinds(events []model.Event) []model.EventKind {
	result := make([]model.EventKind, len(events))
	for i := range events {
		result[i] = events[i].Kind
	}
	return result
}

func normalizedBodies(events []model.Event) []string {
	result := make([]string, len(events))
	for i := range events {
		result[i] = events[i].Body
	}
	return result
}
