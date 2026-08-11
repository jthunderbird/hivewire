# OpenCode Provider Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add live and historical OpenCode child-session viewing and publish the completed feature as hivewire `v1.0.0`.

**Architecture:** A new provider reads OpenCode's SQLite database through `modernc.org/sqlite` in read-only, poll-scoped snapshots and normalizes child sessions into existing agent/event models. Provider state fingerprints mutable rows to emit persisted text/reasoning once and tool invocation/result phases once; replay uses the same normalizer. Small provider-neutral changes let resumed agents regain panes and preserve native IDs in history.

**Tech Stack:** Go 1.26, `database/sql`, `modernc.org/sqlite`, Bubble Tea, embedded HTTP assets, GitHub Actions.

**Design Spec:** `docs/superpowers/specs/2026-08-11-opencode-provider-design.md`

---

## File Map

- Create `internal/provider/opencode/db.go`: read-only SQLite connection and consistent snapshot queries.
- Create `internal/provider/opencode/normalize.go`: OpenCode JSON decoding, deterministic metadata/event/lifecycle normalization.
- Create `internal/provider/opencode/opencode.go`: provider discovery, per-session emission state, backlog/live polling.
- Create `internal/provider/opencode/replay.go`: full history replay through the shared snapshot normalizer.
- Create `internal/provider/opencode/opencode_test.go`: fixture database and provider/replay/lifecycle tests.
- Create `internal/config/config_test.go`: OpenCode database default and flag/config precedence tests.
- Modify `internal/config/config.go`: `OpenCodeDB` setting and flag.
- Modify `internal/model/model.go`: provider comments only.
- Modify `internal/store/store.go`: persist `NativeID` in history records.
- Modify `internal/store/store_test.go`: native-ID persistence/reload coverage.
- Modify `internal/hub/hub.go`: assign an existing agent when it transitions back to live.
- Modify `internal/hub/hub_test.go`: resumed backlog/evicted slot tests.
- Modify `internal/web/web.go`: OpenCode replay dispatch.
- Modify `internal/web/web_test.go`: OpenCode replay and overflow confinement tests.
- Modify `main.go`: register OpenCode provider and exact tool-output root.
- Create `main_test.go`: runtime provider registration and overflow-root derivation tests.
- Modify `README.md`: OpenCode behavior, paths, metadata, history, configuration, and security.
- Modify `.github/workflows/release.yml`: stable SemVer tag-triggered release.
- Modify `go.mod` and `go.sum`: pure-Go SQLite dependency.

## Chunk 1: Provider-Neutral Foundations

### Task 1: Add OpenCode Configuration

**Files:**
- Create: `internal/config/config_test.go`
- Modify: `internal/config/config.go`

- [ ] **Step 1: Write failing configuration tests**

Create tests that isolate `HOME` and XDG variables and assert:

```go
func TestDefaultOpenCodeDatabase(t *testing.T) {
    t.Setenv("HOME", "/home/tester")
    t.Setenv("XDG_DATA_HOME", "")
    cfg := Default()
    want := filepath.Join("/home/tester", ".local", "share", "opencode", "opencode.db")
    if cfg.OpenCodeDB != want {
        t.Fatalf("OpenCodeDB = %q, want %q", cfg.OpenCodeDB, want)
    }
}

func TestOpenCodeDatabaseFlagOverridesDefault(t *testing.T) {
    cfg, err := Load([]string{"--opencode-db", "/tmp/custom.db"})
    if err != nil {
        t.Fatal(err)
    }
    if cfg.OpenCodeDB != "/tmp/custom.db" {
        t.Fatalf("OpenCodeDB = %q", cfg.OpenCodeDB)
    }
}
```

Also test `XDG_DATA_HOME=/var/data` resolves `/var/data/opencode/opencode.db`, and test `opencode_db` from a temporary TOML selected through `HIVEWIRE_CONFIG`. OpenCode itself uses the `xdg-basedir` data path on every supported platform, so the matching rule is `XDG_DATA_HOME` when set and `~/.local/share` otherwise; do not substitute macOS Application Support or Windows LocalAppData.

- [ ] **Step 2: Run the tests and verify failure**

Run: `go test ./internal/config -run OpenCode -v`

Expected: FAIL because `Config.OpenCodeDB` does not exist.

- [ ] **Step 3: Implement configuration**

Add:

```go
OpenCodeDB string `toml:"opencode_db"`
```

Add a small `openCodeDataDir(home string)` helper that returns `filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode")` when set and `filepath.Join(home, ".local", "share", "opencode")` otherwise. Default the database beneath it and register:

```go
fs.StringVar(&cfg.OpenCodeDB, "opencode-db", cfg.OpenCodeDB, "OpenCode SQLite database")
```

- [ ] **Step 4: Run configuration tests**

Run: `go test ./internal/config -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: configure OpenCode database"
```

### Task 2: Preserve Native IDs In History

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing persistence test**

Add a test that opens a temporary store, upserts:

```go
model.Agent{
    ID: "opencode:ses_123", NativeID: "ses_123", Provider: "opencode",
    Started: time.Now(),
}
```

Flush, reopen, and assert `Record.NativeID == "ses_123"`.
Also write legacy index JSON without `nativeId`, open it, and assert decoding succeeds with an empty `NativeID`.

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/store -run NativeID -v`

Expected: FAIL because `Record` has no `NativeID`.

- [ ] **Step 3: Implement the index field**

Add to `Record`:

```go
NativeID string `json:"nativeId,omitempty"`
```

Populate it in `Store.Upsert`. Do not add migration logic; old JSON naturally decodes the field as empty.

- [ ] **Step 4: Verify store tests**

Run: `go test ./internal/store -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat: preserve provider native IDs in history"
```

### Task 3: Reassign Resumed Agents To Slots

**Files:**
- Modify: `internal/hub/hub.go`
- Modify: `internal/hub/hub_test.go`

- [ ] **Step 1: Write failing resumed-agent tests**

Cover both cases:

```go
func TestEvictedFinishedAgentRegainsSlotWhenResumed(t *testing.T) { /* ... */ }
func TestResumedBacklogAgentCanTakeSlot(t *testing.T) { /* ... */ }
```

For the first, fill one slot with `old`, mark it done, add live `replacement` so `old` is evicted, then update `old` to live. Assert `old` is pending while `replacement` remains in the slot. Mark `replacement` done and assert `old` is promoted.

For the second, insert a done backlog agent, then update the same ID with `Backlog=false` and `StatusLive`; assert it enters a free slot.

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/hub -run 'Resumed|Regains' -v`

Expected: FAIL because `Apply` calls `assign` only for new IDs.

- [ ] **Step 3: Implement transition-aware assignment**

Capture the prior status/backlog before replacing agent metadata. After replacement, call `assign(id)` when either the ID is new or it transitioned from non-live/backlog to non-backlog live. Keep existing `assign` protections against evicting live agents and duplicate pending IDs.

- [ ] **Step 4: Verify hub tests**

Run: `go test ./internal/hub -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/hub.go internal/hub/hub_test.go
git commit -m "fix: restore panes for resumed agents"
```

## Chunk 2: OpenCode Snapshot And Normalization

### Task 4: Build Read-Only OpenCode Snapshots

**Files:**
- Create: `internal/provider/opencode/db.go`
- Create: `internal/provider/opencode/opencode_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Add the pure-Go SQLite dependency**

Run:

```bash
go get modernc.org/sqlite
```

Expected: `go.mod` and `go.sum` include `modernc.org/sqlite`; no CGO-only driver is added.

- [ ] **Step 2: Create a fixture database helper**

In the provider test, open a temporary writable SQLite database and create this reduced production-compatible schema, retaining every queried column and production name/type/nullability:

```sql
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  parent_id TEXT,
  directory TEXT NOT NULL,
  title TEXT NOT NULL,
  version TEXT NOT NULL,
  agent TEXT,
  model TEXT,
  tokens_input INTEGER NOT NULL DEFAULT 0,
  tokens_output INTEGER NOT NULL DEFAULT 0,
  tokens_reasoning INTEGER NOT NULL DEFAULT 0,
  tokens_cache_read INTEGER NOT NULL DEFAULT 0,
  tokens_cache_write INTEGER NOT NULL DEFAULT 0,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL
);
CREATE INDEX session_parent_idx ON session(parent_id);
CREATE TABLE message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX message_session_time_created_id_idx ON message(session_id,time_created,id);
CREATE TABLE part (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL REFERENCES message(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX part_message_id_id_idx ON part(message_id,id);
CREATE INDEX part_session_idx ON part(session_id);
```

Add helpers such as:

```go
func newFixture(t *testing.T) *fixture
func (f *fixture) session(t *testing.T, row sessionFixture)
func (f *fixture) message(t *testing.T, sessionID, id string, created int64, data string)
func (f *fixture) part(t *testing.T, sessionID, messageID, id string, created, updated int64, data string)
```

Enable WAL on the writer connection.

- [ ] **Step 3: Write failing snapshot tests**

Test that `readSnapshot`:

- returns only rows requested inside one child session;
- returns messages ordered by `time_created,id` and parts by effective creation/id;
- opens an existing non-WAL database without modifying contents or creating sidecars;
- returns a missing sentinel/no-op for an absent path and creates no database, WAL, or SHM;
- reads committed WAL updates while the writer remains open;
- rejects an incompatible schema with a useful error.

Do not assert that an active WAL reader creates no `-wal`/`-shm`: SQLite may create or maintain those internal files. Instead hash logical table contents before/after and prove hivewire issues no writes.

Expose package-private query helpers accepting `*sql.Tx`. In a consistency test, begin a read transaction, execute the session query to establish the snapshot, commit coordinated message/part changes from the writer, execute message/part queries through the same transaction, and assert the first snapshot is entirely pre-commit. Assert the next snapshot is entirely post-commit.

- [ ] **Step 4: Verify failure**

Run: `go test ./internal/provider/opencode -run Snapshot -v`

Expected: FAIL because the package does not exist.

- [ ] **Step 5: Implement database rows and connection safety**

Define focused internal rows:

```go
type sessionRow struct { /* scalar session columns */ }
type messageRow struct { ID string; Created, Updated int64; Data []byte }
type partRow struct { ID, MessageID string; Created, Updated int64; Data []byte }
type snapshot struct { Sessions []sessionRow; Messages map[string][]messageRow; Parts map[string][]partRow }
```

Implement a connection manager that:

- `os.Stat`s before `sql.Open`;
- uses a URL-escaped `file:` URI with `mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(50)` so every physical connection receives both pragmas;
- limits the pool to one connection;
- begins a read-only transaction, loads child sessions plus requested messages/parts, and always rolls back/commits promptly;
- compares `os.SameFile` and reopens after replacement;
- closes and clears the handle after query errors.

Blank-import `modernc.org/sqlite` only in this package.

- [ ] **Step 6: Verify snapshot tests**

Run: `go test ./internal/provider/opencode -run Snapshot -v`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/provider/opencode/db.go internal/provider/opencode/opencode_test.go
git commit -m "feat: read OpenCode database snapshots"
```

### Task 5: Normalize OpenCode Agents And Events

**Files:**
- Create: `internal/provider/opencode/normalize.go`
- Modify: `internal/provider/opencode/opencode_test.go`

- [ ] **Step 1: Add representative fixture JSON**

Include user and assistant messages and parts matching OpenCode 1.15.7:

```json
{"role":"user","time":{"created":1000},"agent":"general","model":{"providerID":"openai","modelID":"gpt-5.6-sol"}}
{"role":"assistant","time":{"created":1100,"completed":1900},"parentID":"msg_user","providerID":"openai","modelID":"gpt-5.6-sol","agent":"general","finish":"stop","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":50,"write":0}}}
{"type":"text","text":"audit the parser","time":{"start":1000,"end":1001}}
{"type":"reasoning","text":"inspect rows","time":{"start":1200,"end":1250}}
{"type":"tool","callID":"call_1","tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok","title":"Run tests","metadata":{},"time":{"start":1300,"end":1500}}}
{"type":"text","text":"All checks pass.","time":{"start":1600,"end":1800}}
```

- [ ] **Step 2: Write failing metadata/event tests**

Assert:

- ID/provider/native ID/name/title suffix removal/parent/depth/cwd/version/model;
- `Total == In + Out`, with reasoning and cache retained separately;
- synthetic user text does not become prompt/event;
- event kinds and deterministic timestamps/order;
- first-observed completed tool emits use then result and increments once;
- failed tool emits use then errored result but agent can remain live;
- unknown parts are ignored;
- overflow markers are retained.
- timestamp fallback for user, text, reasoning, tool-use, and tool-result events;
- equal effective/creation times resolve by row ID;
- first-observed terminal tool use/result remain adjacent even when another event timestamp falls between their start/end times;
- structured assistant errors prefer `data.message`, then `message`, then `name`, then JSON;
- unknown JSON fields are tolerated;
- incomplete text/reasoning remains un-emitted;
- changed content/state with unchanged timestamps is detected from fingerprints;
- repeated normalization with prior state does not duplicate user/text/reasoning/tool events or malformed notices.

- [ ] **Step 3: Verify failure**

Run: `go test ./internal/provider/opencode -run 'Normalize|Mapping|Tool' -v`

Expected: FAIL because normalization is absent.

- [ ] **Step 4: Implement decoders and pure normalization**

Decode JSON leniently into structs that retain only required fields. Implement a pure function receiving a session row plus ordered rows and an optional emission-state map, returning normalized metadata, candidate events, fingerprints, and authoritative status.

Use:

```go
type emittedPart struct {
    fingerprint             string
    user                    bool
    textDone, reasoningDone bool
    toolUse, toolResult     bool
    malformed              string
}
```

Pass prior per-row state into normalization and return a complete next-state map alongside candidates; the caller commits state only after successful normalization. Fingerprints include relevant raw JSON and mutable phase. Do not mark incomplete text/reasoning emitted. For a terminal tool first seen, create an atomic two-event candidate group sorted at tool start, with invocation immediately before result even if another event's time falls before the result's own timestamp. Sort candidate groups by effective time, persisted creation time, and row ID, then flatten. Build tool bodies once, and reuse existing `provider.ToolHeader`, `PrettyJSON`, `CountLines`, `FirstLine`, and `DetectOverflow`.

- [ ] **Step 5: Verify normalization tests**

Run: `go test ./internal/provider/opencode -run 'Normalize|Mapping|Tool' -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/provider/opencode/normalize.go internal/provider/opencode/opencode_test.go
git commit -m "feat: normalize OpenCode session events"
```

## Chunk 3: Live Provider And Replay

### Task 6: Implement Polling, Deduplication, And Lifecycle

**Files:**
- Create: `internal/provider/opencode/opencode.go`
- Modify: `internal/provider/opencode/opencode_test.go`

- [ ] **Step 1: Write failing provider tests**

Cover:

- only `parent_id IS NOT NULL` sessions are adopted;
- direct child depth 1 and nested depth 2;
- preexisting sessions are backlog, done, metadata-sniffed, and eventless;
- incomplete text/reasoning emits nothing, then emits once after `time.end` appears;
- pending tool emits one use; completed update later emits one result;
- repeated polls emit no duplicate events;
- equal-timestamp content/state updates are detected;
- `stop`, `length`, and `unknown` complete; `error`, `content-filter`, or error object fail; `tool-calls` stays live;
- newest-message ties resolve by `time_created,id`, error objects override finish, and terminal rows plus tool activity in one snapshot do not flicker;
- status events use the authoritative assistant completion timestamp;
- a newer user/incomplete assistant message reopens a done session and clears backlog;
- idle timeout completes an otherwise ambiguous live session;
- malformed supported JSON emits one notice, not one notice per poll;
- a large backlog fixture does not load events and completes within a generous bounded test deadline.

Classify backlog from the maximum session/message/part persisted activity, not session creation. Test activity before `since`, exactly equal to `since` (not backlog), and a session created before startup with post-start activity. For a resumed backlog session, assert only rows persisted after startup emit while prior fingerprints remain suppressed.

Add Poll-level database behavior tests: absent database returns no updates; incompatible/corrupt database returns an error; after the file is atomically replaced with a valid fixture the same provider recovers; an already-open valid database atomically replaced by a different valid database is detected through `os.SameFile`; a lock exceeding the 50ms busy timeout returns promptly and a later poll succeeds. Assert deterministic `EventCount`, `ToolCount`, `Updated`, and `DurationMS` for live, backlog, and terminal agents.

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/provider/opencode -run 'Poll|Backlog|Lifecycle|Dedup|Malformed' -v`

Expected: FAIL because `Provider` is absent.

- [ ] **Step 3: Implement provider state and Poll**

Expose:

```go
const Name = "opencode"

func New(path string, idleDone time.Duration, since time.Time) *Provider
func (p *Provider) Name() string
func (p *Provider) Poll() ([]provider.Update, error)
```

Keep `map[string]*agentTail` state. Discovery loads child session metadata. Backlog sessions normalize metadata/counts but commit all row fingerprints as already observed and emit no events. Active sessions normalize each consistent snapshot and emit only state transitions. Add one `EvStatus` when status changes. Set `Agent.EventCount`, `ToolCount`, `Updated`, and duration deterministically.

- [ ] **Step 4: Verify provider tests**

Run: `go test ./internal/provider/opencode -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/opencode/opencode.go internal/provider/opencode/opencode_test.go
git commit -m "feat: stream OpenCode child sessions"
```

### Task 7: Add History Replay

**Files:**
- Create: `internal/provider/opencode/replay.go`
- Modify: `internal/provider/opencode/opencode_test.go`

- [ ] **Step 1: Write failing replay tests**

Test:

```go
a, events, err := Replay(f.path, "ses_child")
```

Assert complete metadata, every completed event, invocation-before-result ordering, monotonic sequence numbers, final terminal status event, and no unsupported parts. Add missing session and empty native ID error cases.

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/provider/opencode -run Replay -v`

Expected: FAIL because `Replay` is absent.

- [ ] **Step 3: Implement replay through shared normalization**

Load one child snapshot in a short read transaction. Normalize with empty emission state so every eligible event is emitted, assign `AgentID` and `Seq`, append one final terminal status event at authoritative completion time, and return clear errors for missing/non-child sessions.

- [ ] **Step 4: Verify replay and provider tests**

Run: `go test ./internal/provider/opencode -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/opencode/replay.go internal/provider/opencode/opencode_test.go
git commit -m "feat: replay OpenCode session history"
```

### Task 8: Wire Runtime And Web Replay

**Files:**
- Modify: `main.go`
- Create: `main_test.go`
- Modify: `internal/web/web.go`
- Modify: `internal/web/web_test.go`
- Modify: `internal/model/model.go`

- [ ] **Step 1: Write failing web replay test**

Create an OpenCode fixture database and file-backed history store, index a record with `Provider: "opencode"`, `NativeID: "ses_child"`, and `Source` set to the fixture path, flush and reopen the store, request `/api/replay?id=opencode:ses_child`, and assert returned `Agent.ID`, `NativeID`, and every `Event.AgentID`. This proves dispatch uses persisted `NativeID` rather than parsing record ID. Add a record with empty `NativeID` and assert replay returns HTTP 400.

Add an overflow resolution test proving a path under sibling `auth.json` is forbidden while a real file under derived `tool-output` is allowed.

Create `main_test.go` in this same failing-test step. Assert `providersFor` returns provider names Claude, Codex, and OpenCode and `overflowRoots` derives only the sibling `tool-output` root from a custom `OpenCodeDB` path.

- [ ] **Step 2: Verify failure**

Run: `go test . ./internal/web -run 'OpenCode|Overflow|Providers|Roots' -v`

Expected: replay FAILS with unknown provider; confinement test passes only after exact roots are supplied.

- [ ] **Step 3: Wire OpenCode**

In `main.go`:

```go
opencode.New(cfg.OpenCodeDB, idle, since)
```

Add `filepath.Join(filepath.Dir(cfg.OpenCodeDB), "tool-output")` to `Server.Roots`, not the OpenCode data directory.

Extract package-private `providersFor(cfg, idle, since)` and `overflowRoots(cfg)` helpers. Make `run` use both helpers so the failing runtime tests pass only when registration and root wiring are complete.

In `web.go`, import the provider and dispatch:

```go
case opencode.Name:
    if rec.NativeID == "" { /* bad request */ }
    a, events, err = opencode.Replay(rec.Source, rec.NativeID)
```

Generalize model comments without changing JSON behavior.

- [ ] **Step 4: Verify focused and full tests**

Run:

```bash
go test ./internal/web ./internal/config ./internal/store ./internal/hub ./internal/provider/opencode -v
go test ./...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go internal/model/model.go internal/web/web.go internal/web/web_test.go
git commit -m "feat: integrate OpenCode provider"
```

## Chunk 4: Documentation, Release, And Verification

### Task 9: Document OpenCode Support

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update supported-provider documentation**

Document:

- Claude Code, Codex, and OpenCode in the introduction;
- OpenCode database/session/message/part paths and child identification;
- completed persisted-part boundaries rather than token-level deltas;
- pane field mapping for OpenCode;
- history search/replay behavior;
- `opencode_db = "~/.local/share/opencode/opencode.db"`;
- no hooks, server, OpenCode configuration, external SQLite, or CGO;
- overflow reads confined to `tool-output`;
- unauthenticated LAN exposure includes OpenCode prompts and outputs.

- [ ] **Step 2: Check documentation references**

Run searches for stale two-provider wording such as `Claude Code and Codex`, `both CLIs`, and provider enumerations. Preserve occurrences that intentionally compare only those two providers; update generic claims.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: describe OpenCode support"
```

### Task 10: Make Releases Tag-Driven

**Files:**
- Modify: `.github/workflows/release.yml`

- [ ] **Step 1: Change the workflow trigger and validate version**

Use:

```yaml
on:
  push:
    tags: ["v*"]
```

Remove `workflow_dispatch`. Add an early shell step that rejects anything not matching `^v[0-9]+\.[0-9]+\.[0-9]+$`. Set `VERSION: ${{ github.ref_name }}` for builds and publishing.

- [ ] **Step 2: Publish a stable release**

Remove `--prerelease`, add `--verify-tag`, use stable release notes naming the OpenCode feature, and keep target `GITHUB_SHA`. Keep all existing targets, checksums, tests, vet, and `CGO_ENABLED=0`.

- [ ] **Step 3: Validate workflow text locally**

Inspect the diff and verify:

- no `v0.1.0-beta` remains;
- no branch push trigger remains;
- no `workflow_dispatch` remains;
- both build and publish use `github.ref_name`;
- `gh release create` publishes the existing pushed tag.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: publish stable tagged releases"
```

### Task 11: Verify The Complete Feature

**Files:**
- Modify only if verification reveals defects.

- [ ] **Step 1: Format and inspect changes**

Run:

```bash
gofmt -w main.go internal/config internal/hub internal/model internal/provider/opencode internal/store internal/web
git diff --check
```

Expected: no formatting or whitespace errors.

- [ ] **Step 2: Run all tests and race detector**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
```

Expected: all PASS with no race or vet findings.

- [ ] **Step 3: Cross-build the release matrix without CGO**

Verify `/tmp/opencode` exists, create `/tmp/opencode/hivewire-dist`, then run the same matrix and naming logic as CI:

```bash
rm -rf /tmp/opencode/hivewire-dist
mkdir -p /tmp/opencode/hivewire-dist
VERSION=v1.0.0
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  os="${target%/*}"
  arch="${target#*/}"
  out="/tmp/opencode/hivewire-dist/hivewire_${VERSION}_${os}_${arch}"
  if [ "$os" = windows ]; then out="${out}.exe"; fi
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$out" . || exit 1
done
(cd /tmp/opencode/hivewire-dist && sha256sum * > checksums.txt)
```

Expected: all five binaries build successfully, proving the SQLite dependency is pure Go across release targets.

- [ ] **Step 4: Run a read-only live smoke test**

Confirm `opencode --version` and `opencode db path`. Record a checksum of `auth.json` and a logical count query through `opencode db`; do not use WAL/SHM metadata as a write test because concurrent OpenCode may change it. Start hivewire bound only to localhost with temporary state and failure-safe cleanup:

```bash
CGO_ENABLED=0 go build -o /tmp/opencode/hivewire-smoke .
state="$(mktemp -d)"
log="/tmp/opencode/hivewire-smoke.log"
/tmp/opencode/hivewire-smoke --tui=false --addr 127.0.0.1 --port 18787 --state-dir "$state" --log-file "$log" &
pid=$!
trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; rm -rf "$state"' EXIT
for i in $(seq 1 50); do
  curl -fsS http://127.0.0.1:18787/api/agents >/tmp/opencode/hivewire-agents.json && break
  sleep 0.1
done
curl -fsS 'http://127.0.0.1:18787/api/history?limit=5' >/tmp/opencode/hivewire-history.json
kill "$pid"
wait "$pid" || true
trap - EXIT
rm -rf "$state"
```

Assert both responses are valid JSON, history includes `provider: opencode`, the log has no database errors, `auth.json` is unchanged, and the logical query remains valid. Automated query-only and missing-artifact tests prove hivewire itself does not write OpenCode storage.

- [ ] **Step 5: Request final code review**

Use `superpowers:requesting-code-review` against the full diff from `origin/main`, fix every blocker with tests, and rerun verification.

- [ ] **Step 6: Commit any verification fixes**

Stage only intended files and use a specific non-amended commit message.

- [ ] **Step 7: Prepare but do not push the release tag until explicitly at final release point**

After a clean worktree and successful verification:

```bash
test "$(git branch --show-current)" = main
test -z "$(git status --porcelain)"
git fetch origin main --tags
test "$(git rev-parse HEAD)" = "$(git rev-parse main)"
git log --oneline origin/main..main
git tag -a v1.0.0 -m "hivewire v1.0.0"
test "$(git rev-parse v1.0.0^{})" = "$(git rev-parse main)"
git diff --stat origin/main..v1.0.0
```

Inspect the tag target and release diff. The final network operation is:

```bash
git push --atomic origin main v1.0.0
```

Expected: `main` and `v1.0.0` advance together and the tag workflow publishes exact release `v1.0.0`.
