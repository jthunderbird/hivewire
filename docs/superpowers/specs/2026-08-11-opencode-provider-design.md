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
- observed message and part IDs;
- emitted tool phases;
- latest persisted activity time;
- lifecycle state.

Each `Poll` discovers child sessions and queries changed messages and parts. It emits only new normalized events or lifecycle updates. Per-row identities and observed states prevent duplicate events across repeated polls.

Claude Code and Codex providers remain unchanged. The hub, store, TUI, and web event stream continue consuming provider-neutral updates.

## Configuration And Wiring

Add `opencode_db` to TOML configuration and `--opencode-db` to command-line flags. The default is `~/.local/share/opencode/opencode.db`, resolved from the user's data directory.

Register the OpenCode provider in `main.go` alongside Claude Code and Codex. Add the OpenCode data directory to the web server's allowed overflow roots because OpenCode can persist oversized tool output under that directory and place a `Full output saved to:` marker in the stored tool result.

Generalize comments and documentation that currently enumerate only Claude Code and Codex.

## Agent Mapping

For each child session:

- `ID`: `opencode:<session id>`
- `NativeID`: OpenCode session ID
- `Provider`: `opencode`
- `Name`: session agent name, with message agent as fallback
- `Title`: session title, removing only OpenCode's known redundant `(@<agent> subagent)` suffix when present
- `Prompt`: first non-synthetic user text, clipped using the existing search limit
- `Depth`: computed by following parent session links
- `Parent`: immediate parent session ID
- `Cwd`: session directory
- `Model`: model ID, with provider ID included only when needed to disambiguate
- `CLIVersion`: session version
- `Started` and `Updated`: session/message/part timestamps
- `Tokens`: session aggregate input, output, reasoning, cache-read, and cache-write columns
- `Source`: configured OpenCode database path

The provider computes the normalized total consistently from the available aggregate token categories.

## Event Mapping

Events remain complete and replayable:

- A non-synthetic user text part becomes `EvUser`; the first one supplies `Prompt`.
- A completed assistant text part becomes `EvText`.
- A completed reasoning part becomes `EvReasoning`.
- A tool part first observed as pending or running emits one `EvToolUse` and increments `ToolCount` once.
- A tool part reaching completed emits one `EvToolResult` with its full output.
- A tool part reaching error emits one errored `EvToolResult` with its error body.
- Step, snapshot, patch, compaction, and file parts do not create stream events in this initial implementation.

Tool headers reuse `provider.ToolHeader` when input is structured. OpenCode tool names are preserved rather than translated to Claude-specific capitalization. Tool output uses existing line counting, folding, and overflow detection.

Events are ordered by persisted creation time and stable row ID. Polling state ensures a mutable tool row can emit its invocation and result at different times without duplicating either.

## Lifecycle

- A child session discovered after hivewire starts begins `live`.
- New persisted activity reopens a previously completed child.
- An assistant message containing an error marks the child `error` unless newer activity resumes it.
- A completed assistant message with a terminal finish reason such as `stop` marks the child `done`.
- Intermediate `tool-calls` finishes do not complete the child.
- A failed tool result marks that event as errored but does not fail the child because OpenCode may recover.
- The configured idle timeout remains a fallback when durable records provide no terminal signal.

Sessions whose database activity predates hivewire startup are backlog. They are sniffed for searchable metadata and aggregate counts, indexed as done, and not replayed into live panes.

## History Replay

Add `opencode.Replay(databasePath, sessionID)`. It queries all messages and parts for the indexed child session, rebuilds metadata and events in stable order, assigns sequence numbers, and returns the complete normalized run.

The web replay switch adds an OpenCode case and obtains the native session ID from the indexed `opencode:<session id>` record. The history index continues storing metadata only; it does not copy transcript content.

## Database Safety And Errors

- Open lazily so hivewire starts normally when OpenCode is not installed and can discover a database created later.
- Use read-only and query-only settings with a bounded busy timeout compatible with OpenCode's WAL writes.
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
- missing database behavior;
- malformed JSON and incompatible schema handling;
- concurrent read-only polling while another connection updates a WAL database.

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

Change the GitHub workflow from releasing every push to `main` to releasing version tags matching `v*`. The workflow derives the binary and release version from `github.ref_name`, retains tests and static cross-builds, and publishes a normal release without `--prerelease`.

After all implementation and verification work is complete, create annotated tag `v1.0.0` and perform one final push containing both local `main` and the tag. The resulting assets will be named like `hivewire_v1.0.0_linux_amd64`, and the GitHub tag, release title, and release version will all be exactly `v1.0.0`.
