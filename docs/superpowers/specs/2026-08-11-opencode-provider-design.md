# OpenCode Provider Design

## Goal

Add first-class OpenCode subagent support to hivewire while preserving its existing operating model: one static binary, no hooks, no agent configuration, live TUI and web views, and history replay from data already owned by the agent CLI.

The first stable hivewire release will be tagged and published as `v1.0.0` after the feature is verified. Development remains directly on `main`, with all commits kept local until one final push sends `main` and the release tag together.

## Scope

Hivewire will display only OpenCode child sessions, identified by a non-empty `session.parent_id`. Top-level OpenCode conversations remain excluded. Current OpenCode SQLite storage is in scope; legacy pre-SQLite JSON storage and opt-in developer traces are not.

No TUI or web redesign is required. OpenCode data will be normalized through the existing `model.Agent` and `model.Event` types used by Claude Code and Codex.

## Verified OpenCode Storage Model

OpenCode 1.15.7 persists child sessions in `~/.local/share/opencode/opencode.db`:

- `session` stores identity, parent linkage, directory, title, agent, model, CLI version, timestamps, and aggregate token usage.
- `message` stores user and assistant message metadata, including model, finish reason, completion time, and errors.
- `part` stores text, reasoning, tool state, step boundaries, and other message content.

The official Task implementation creates a durable child session with `parentID`. OpenCode does contain a JSONL writer, but it is a process-wide developer trace enabled only by `OPENCODE_DIRECT_TRACE`; it is not a normal per-subagent transcript and is unsuitable as hivewire's source.

Text and reasoning deltas are published on OpenCode's in-process event bus and are not durably written for each token. The database persists an initial part and then its completed content. Hivewire will therefore stream at durable event boundaries, consistent with reliable history replay, rather than promise character-level deltas.

## Architecture

Add `internal/provider/opencode`, implementing the existing `provider.Provider` interface. It will lazily open the configured database through a pure-Go SQLite driver in read-only/query-only mode. No external `sqlite3` executable or CGO runtime is required.

The provider keeps one in-memory tail state per child session:

- normalized agent metadata;
- observed message and part fingerprints, including mutable completion state;
- emitted text/reasoning completion and tool invocation/result phases;
- latest persisted activity time;
- lifecycle state.

Each `Poll` discovers child sessions and queries messages and parts inside one short read transaction, yielding a consistent snapshot without pinning WAL checkpoints between polls. It emits only new normalized events or lifecycle updates. State comparison is keyed by row ID and relevant content/state, not timestamps alone, so equal-timestamp updates and incomplete rows are not lost. Malformed-row notices use the same fingerprint deduplication.

Claude Code and Codex providers remain unchanged. The store, TUI, and web event stream continue consuming provider-neutral updates. The hub gains one provider-neutral fix: when an existing agent transitions from non-live to live and is not backlog, `Apply` runs normal slot assignment again. This lets a resumed OpenCode or Codex session regain an evicted pane without displacing another live agent. Resumed backlog agents clear `Backlog` before that transition.

## Configuration And Wiring

Add `opencode_db` to TOML configuration and `--opencode-db` to command-line flags. The default is `~/.local/share/opencode/opencode.db`, resolved from the user's data directory.

Register the OpenCode provider in `main.go` alongside Claude Code and Codex. Add only `filepath.Join(filepath.Dir(cfg.OpenCodeDB), "tool-output")` to the web server's allowed overflow roots because OpenCode persists oversized tool output beside its database in that directory. This derivation follows `--opencode-db` and non-default data locations without separately exposing the database directory, which may contain credentials such as `auth.json`.

Generalize comments and documentation that currently enumerate only Claude Code and Codex.

## Agent Mapping

For each child session:

- `ID`: `opencode:<session id>`
- `NativeID`: OpenCode session ID
- `Provider`: `opencode`
- `Name`: session agent name, with message agent as fallback
- `Title`: session title, removing only the exact suffix ` (@<agent> subagent)` for the mapped agent when present
- `Prompt`: first non-synthetic user text, clipped using the existing search limit
- `Depth`: number of parent links, so a direct child is depth 1; a missing parent stops traversal, while a cycle is a poll error
- `Parent`: immediate parent session ID
- `Cwd`: session directory
- `Model`: exact OpenCode model ID; the model provider is not prepended
- `CLIVersion`: session version
- `Started` and `Updated`: session/message/part timestamps
- `Tokens`: session aggregate input, output, reasoning, cache-read, and cache-write columns
- `Source`: configured OpenCode database path

OpenCode's reasoning and cache counts are breakdowns of output and input rather than additional tokens. The normalized `Total` is therefore `In + Out`, matching OpenCode's aggregate semantics without double counting reasoning or cache tokens.

All OpenCode numeric times are Unix milliseconds. Agent `Started` is `session.time_created`; agent `Updated` is the maximum relevant session, message, or part update time. User/text/reasoning event time is part `time.start` when present, otherwise part `time_created`. Tool-use time is state `time.start` then part creation as fallback; tool-result time is state `time.end` then part update as fallback.

## Event Mapping

Events remain complete and replayable:

- A user-message text part becomes `EvUser` unless its part data has `synthetic: true`; the first eligible one supplies `Prompt`.
- An assistant text part emits one `EvText` only after `time.end` is present.
- A reasoning part emits one `EvReasoning` only after `time.end` is present.
- A tool part first observed in any state emits one `EvToolUse` and increments `ToolCount` once.
- A tool part observed completed emits its invocation first when needed, followed by one `EvToolResult` with full output.
- A tool part observed errored emits its invocation first when needed, followed by one errored `EvToolResult`. Its body is the state's string error; structured assistant errors prefer `data.message`, then `message`, then `name`, then formatted JSON.
- Step, snapshot, patch, compaction, and file parts do not create stream events in this initial implementation.

Tool headers reuse `provider.ToolHeader` when input is structured. OpenCode tool names are preserved rather than translated to Claude-specific capitalization. Tool output uses existing line counting, folding, and overflow detection.

Events are ordered by effective event time, then persisted creation time, then stable row ID, with tool invocation ordered immediately before its result when both are first observed together. Polling state tracks completion separately from row discovery, so an incomplete text/reasoning row is revisited and a mutable tool row can emit invocation and result at different times without duplicating either. Live polling and replay call the same row normalizer.

## Lifecycle

- A child session discovered after hivewire starts begins `live`.
- Lifecycle is derived from the newest message by creation time and ID within the same database snapshot. A newest user message, incomplete assistant message, or assistant finish `tool-calls` is `live`.
- A completed newest assistant with `stop`, `length`, or `unknown` is `done`.
- A newest assistant with an error object or finish `error` or `content-filter` is `error`.
- Persisted activity after a prior terminal message reopens the child because the newer message becomes authoritative.
- A failed tool result marks that event as errored but does not fail the child because OpenCode may recover.
- The configured idle timeout remains a fallback when durable records provide no terminal signal.

The provider emits an `EvStatus` only when normalized status changes, using the authoritative message completion time when available. Evaluating one consistent snapshot and applying the precedence above prevents done/live flicker when tool and terminal rows arrive in one poll.

Sessions whose database activity predates hivewire startup are backlog. They are sniffed for searchable metadata and aggregate counts, indexed as done, and not replayed into live panes.

## History Replay

Add `opencode.Replay(databasePath, sessionID)`. It queries all messages and parts for the indexed child session in one read transaction, calls the same row normalizer as live polling, rebuilds metadata and events in stable order, appends the final lifecycle event when terminal, assigns sequence numbers, and returns the complete normalized run.

Add `NativeID` to `store.Record` and populate it for every provider. Existing index JSON remains readable because the new field has a zero value; no pre-feature OpenCode records require migration. The web replay switch adds an OpenCode case and passes the indexed `NativeID`, rejecting an empty value. The history index continues storing metadata only; it does not copy transcript content.

## Database Safety And Errors

- Use `modernc.org/sqlite`, which is pure Go and supports the required static cross-build targets.
- Check for the database with `os.Stat` before opening it, so missing-database polling cannot create a database, WAL, or SHM file.
- Open lazily with a `file:` URI using `mode=ro`, enforce `PRAGMA query_only=ON`, set a 50-millisecond busy timeout, and limit the pool to one connection. This bounds delay well below the default 250-millisecond provider interval because providers are currently polled sequentially and `Poll` must not block.
- Keep transactions poll-scoped. Close and reopen after database errors or when `os.SameFile` shows that the database path was replaced, allowing recovery after creation, migration, or replacement.
- Never run migrations or write OpenCode-owned data.
- Treat a missing database as no OpenCode updates, not a startup error.
- Return busy, corrupt, permission, or incompatible-schema failures from `Poll`; the main loop logs them and continues running other providers.
- Decode unknown JSON fields leniently and ignore unsupported part types.
- Surface malformed supported records as provider notices where an agent can be identified; otherwise log the poll error without terminating hivewire.

## Testing

Provider tests will create temporary SQLite databases through the same pure-Go driver and cover:

- child-only discovery and nested depth;
- session, agent, model, title, prompt, timestamp, and token mapping;
- ordered user, text, reasoning, tool-use, and tool-result events;
- pending/running/completed/error tool transitions;
- no duplicate events across repeated polls;
- terminal completion, error, activity-after-completion, and idle fallback;
- backlog indexing without event replay;
- complete history replay;
- replay after flushing and reopening the history index;
- missing database behavior;
- malformed JSON and incompatible schema handling;
- concurrent read-only polling while another connection updates a WAL database.
- first-observed terminal tool rows, incomplete-to-complete text/reasoning rows, equal-timestamp mutations, and snapshot consistency;
- resumed backlog and evicted agents regaining slots without evicting live agents;
- overflow access confined to `tool-output` and proof that polling creates no filesystem artifacts;
- bounded polling behavior with a large fixture database.

Configuration and web tests will cover the new default/override and replay dispatch. Existing provider, hub, store, TUI, and web tests must continue passing.

Final verification before release:

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- formatting checks
- `CGO_ENABLED=0` cross-builds for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64
- a read-only live smoke test against the local OpenCode 1.15.7 database

## Documentation

Update the README introduction, data-source section, pane metadata table, history behavior, configuration example, and security notes to include OpenCode. Document that OpenCode text/reasoning appears at completed persisted-part boundaries and requires no server, hooks, or external SQLite installation.

## Release

Change the GitHub workflow from releasing every push to `main` to releasing version tags matching `v*`. Remove `workflow_dispatch` so a branch name can never be mistaken for a release version. The workflow validates `github.ref_name` against stable SemVer `^v[0-9]+\.[0-9]+\.[0-9]+$`, derives binary and release versions from that tag, retains tests and static cross-builds, and publishes a normal release without `--prerelease`.

After all implementation and verification work is complete, create annotated tag `v1.0.0` and run `git push --atomic origin main v1.0.0`, ensuring neither ref advances if the other is rejected. The resulting assets will be named like `hivewire_v1.0.0_linux_amd64`, and the GitHub tag, release title, and release version will all be exactly `v1.0.0`.
