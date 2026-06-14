# codex-feishu-bridge Design

## Status

Approved for design by user on 2026-06-14.

## Goal

Build an open-source local daemon that lets a user control Codex from Feishu without depending on Hermes or Codex Desktop. The daemon runs on the user's machine, receives Feishu messages through a WebSocket connection, starts Codex background tasks, and continues existing Codex sessions through `codex exec resume`.

The core product loop is:

1. User sends `/codex ...` in Feishu.
2. Local daemon creates a task and runs `codex exec --json`.
3. Daemon sends a Feishu result card with a short task id.
4. User replies to that card or submits a card form.
5. Daemon resolves the card message to a task and runs `codex exec --json resume <session_id> "<reply>"`.

## Non-Goals

- Do not depend on Hermes.
- Do not inject messages into the current Codex Desktop conversation.
- Do not support image or file input in MVP.
- Do not stream Codex output into Feishu cards in MVP.
- Do not implement team permissions or multi-user role management in MVP.
- Do not implement task locks or workspace conflict protection in MVP.
- Do not implement remote server deployment or public HTTP webhook mode in MVP.
- Do not implement non-Feishu transports in MVP.
- Do not implement force-stop for running Codex processes in MVP.

## Architecture

The project is a Go single binary named `codex-feishu-bridge`.

Main modules:

- `transport`: inbound and outbound messaging abstraction. MVP implements Feishu WebSocket only. The interface is intentionally small so future transports can reuse the same contracts without adding non-Feishu work to MVP.
- `router`: parses inbound events, validates users, creates tasks for `/codex ...`, and routes card replies/forms back to existing tasks.
- `taskstore`: SQLite persistence for tasks, message routes, runs, and allowlisted users.
- `codexrunner`: executes `codex exec --json` and `codex exec --json resume`, parses JSONL events, and stores raw logs.
- `notifier`: sends Feishu start, success, and failure cards.
- `config`: loads local YAML config and merges project-level defaults.

High-level flow:

```text
Feishu /codex request
  -> Feishu WebSocket transport
  -> router validates command and user
  -> taskstore creates task_id
  -> notifier sends start card
  -> codexrunner runs codex exec --json
  -> taskstore stores codex_session_id and run result
  -> notifier sends completion/failure card
  -> taskstore maps Feishu result message id to task_id

Feishu reply or card form submit
  -> transport receives event
  -> router resolves Feishu message id to task_id
  -> codexrunner runs codex exec --json resume <session_id> <reply>
  -> taskstore stores new run
  -> notifier sends completion/failure card
```

## MVP Contracts

The first implementation should use explicit package-level contracts instead of passing Feishu SDK objects or raw Codex events across module boundaries.

### Transport

Inbound transport emits `InboundEvent`:

- `dedup_key`: stable id used for replay protection.
- `kind`: `new_task`, `reply`, or `card_action`.
- `chat_id`.
- `sender_open_id`.
- `message_id`: current Feishu message or callback message id when available.
- `root_message_id`: replied-to/root/open card message id used for route lookup.
- `action_id`: card action id, empty for text messages.
- `text`: normalized prompt or follow-up text.
- `raw_received_at`.

Outbound transport accepts `OutboundMessage`:

- `chat_id`.
- `reply_to_message_id`, nullable.
- `card_kind`: `start`, `success`, `failure`, or `routing_error`.
- `task_id`, nullable for unauthorized/routing errors.
- `status`.
- `title`.
- `body_markdown`.
- `actions`: MVP supports only the `continue_submit` form action.

Outbound send returns `SentMessage` with `message_id`. A missing `message_id` is treated as a send failure for routeable task cards because continuation would be impossible.

### Router

Router owns command parsing, authorization, route lookup, and same-task run admission. It calls the store through transactional methods:

- `CreateTaskWithRun(...)`.
- `StartResumeRun(...)`.
- `FinishRun(...)`.
- `InsertMessageRoute(...)`.
- `ResolveMessageRoute(...)`.
- `RecordDedup(...)`.

### Codex Runner

Runner returns `RunResult`:

- `codex_session_id`: the Codex thread id used by `codex exec resume`.
- `final_text`.
- `exit_code`.
- `stderr_tail`.
- `log_path`.
- `started_at`.
- `finished_at`.

Runner never sends Feishu messages directly. It only returns structured results and persisted log paths.

## Feishu Interaction

New tasks require explicit `/codex` prefix to avoid accidental execution.

Supported MVP command forms:

```text
/codex <prompt>
/codex @backend <prompt>
```

Project aliases are configured locally. If no alias is provided, the default workspace is used.

Task continuation should not require the user to type a task id. The daemon records Feishu message ids for task cards and uses Feishu reply context or card callback context to route the reply to the correct task.

MVP routing rules:

- A direct text reply to a task start card or result card is valid.
- A card callback from a task start card or result card is valid.
- Route lookup uses Feishu `context.open_message_id` for card callbacks and the replied-to/root message id for message replies. If the Feishu SDK exposes both message id and parent/root message id, the parent/root id wins.
- Every sent start/result card message id must be inserted into `message_routes`.
- If a reply event cannot be mapped to a sent card message id, the daemon must not fall back to "latest task"; it should send a short routing error.
- MVP is creator-only: only the original `tasks.created_by` user may resume a task, even if the card appears in a group chat. Future versions may add chat allowlists or shared task ownership.

Feishu cards should show:

- Short task id, for example `cx_8f3a2c`.
- Status.
- Project alias and working directory label.
- Short summary or final Codex response.
- A multiline form input for custom follow-up text.
- A submit action with action id `continue_submit`.

Full Codex session ids are internal and should not be displayed in normal cards.

MVP card action rules:

- `continue_submit` payload must contain a string field named `text`.
- Empty or whitespace-only `text` is rejected with a short card response and does not create a run.
- Text replies and card form submissions share the same routing path after normalization.
- MVP does not implement extra action buttons such as stop, retry, or open logs.
- Start/result card body text sent to Feishu is capped at 4,000 characters after redaction. Failure summaries keep the stricter 2,000-character cap from Error Handling.
- If Feishu returns a message id after sending a task card, the daemon must insert that id into `message_routes` before considering the notification complete.

## Persistence

Use SQLite for local state at `~/.codex-feishu-bridge/state.db`.

Schema changes are managed with a `schema_migrations` table:

- `version`: integer primary key.
- `applied_at`.

Startup must run pending migrations before opening the daemon for events. If migration fails, `serve` exits before connecting to Feishu.

### `tasks`

- `id`: short task id, primary key.
- `codex_session_id`: Codex `thread_id` parsed from JSONL, nullable until known.
- `status`: `queued`, `running`, `succeeded`, `failed`.
- `project_alias`: selected project alias.
- `cwd`: execution directory.
- `created_by`: Feishu open id.
- `chat_id`: Feishu chat id.
- `root_message_id`: first task card message id.
- `effective_codex_command`: command selected at task creation.
- `effective_sandbox`: sandbox selected at task creation.
- `effective_model`: model selected at task creation, nullable.
- `effective_approval`: approval policy selected at task creation.
- `effective_approval_flag_supported`: whether the local Codex binary accepted approval flags at task creation.
- `effective_extra_args_json`: JSON array of extra args selected at task creation.
- `created_at`.
- `updated_at`.

### `message_routes`

- `feishu_message_id`: primary key.
- `task_id`: foreign key to `tasks.id`.
- `route_type`: `start_card`, `result_card`, `manual_reply`.
- `created_at`.

### `runs`

- `id`: primary key.
- `task_id`: foreign key to `tasks.id`.
- `kind`: `exec` or `resume`.
- `status`: `running`, `succeeded`, or `failed`.
- `prompt`.
- `codex_session_id`.
- `exit_code`.
- `started_at`.
- `finished_at`.
- `log_path`.
- `final_text`.

### `event_dedup`

- `dedup_key`: primary key. Use Feishu event id or callback token when available. Fallback to `message:<message_id>` for message events, or `card:<open_message_id>:<sender_open_id>:<action_id>:<payload_hash>` for card callbacks.
- `received_at`.
- `source`: `message` or `card_callback`.
- `state`: `processing`, `completed`, or `failed`.
- `task_id`: nullable foreign key to `tasks.id`.
- `run_id`: nullable foreign key to `runs.id`.
- `completed_at`, nullable.
- `last_error`, nullable.

The daemon should insert `dedup_key` before dispatching work in the same transaction that creates or admits the task/run. If a completed key already exists, the event is a replay and must be acknowledged without starting Codex. If a key is stuck in `processing` with no run id past a short startup recovery window, mark it `failed` and allow the user to retry by sending a new Feishu message instead of silently dropping future work.

### `users`

- `feishu_open_id`.
- `role`: MVP uses `owner`.
- `enabled`.

SQLite constraints and indexes:

- `tasks.status` and `runs.status` use check constraints.
- `runs.kind` uses a check constraint for `exec` or `resume`.
- `message_routes.task_id`, `runs.task_id`, `event_dedup.task_id`, and `event_dedup.run_id` use foreign keys.
- Add indexes on `tasks.codex_session_id`, `runs.task_id`, `runs.status`, and `message_routes.task_id`.
- Add a partial unique index `runs_one_active_per_task` on `runs(task_id)` where `status = 'running'`.

Status transition rules:

- New task creation inserts `tasks.status = 'running'` and an initial `runs.status = 'running'` in one transaction.
- A successful run sets `runs.status = 'succeeded'` and `tasks.status = 'succeeded'`.
- A failed run sets `runs.status = 'failed'` and `tasks.status = 'failed'`.
- A resume run may start from a task whose latest status is `succeeded` or `failed`, but not while another run for that task is `running`.
- `queued` is reserved for a future queue mode and should not be emitted by MVP unless a migration later adds queuing.

## Configuration

Default config path:

```text
~/.codex-feishu-bridge/config.yaml
```

Example:

```yaml
feishu:
  app_id: cli_xxx
  app_secret_env: FEISHU_APP_SECRET
  connection: websocket
security:
  allowed_open_ids:
    - ou_xxx
codex:
  command: codex
  default_model: ""
  sandbox: workspace-write
  approval: never
  extra_args: []
  log_retention_days: 14
workspace:
  default: /path/to/default/repo
projects:
  backend:
    cwd: /path/to/backend
    model: ""
    sandbox: workspace-write
    approval: never
```

Security defaults:

- Only allow configured Feishu open ids.
- Default sandbox is `workspace-write`.
- Default approval mode is `never`.
- Default execution directory must be configured.
- `danger-full-access` must be explicitly configured and should not be the generated default.
- Verify inbound events belong to the configured Feishu app. MVP WebSocket mode should rely on the official SDK token validation where available.
- Private chats are the recommended MVP deployment mode. Group chats are allowed only when the sender open id is allowlisted and task continuation is creator-only.
- Never execute Codex for unknown users, unknown project aliases, or unroutable replies.

Authorization source of truth:

- `security.allowed_open_ids` is the source of truth in MVP.
- The `users` table is a local audit/cache table refreshed at startup: configured ids are upserted as `enabled = true`, and ids no longer present in config are set to `enabled = false`.
- Runtime authorization checks use the loaded config plus the refreshed `users.enabled` value. If they disagree, config wins and the daemon logs the mismatch.
- Unauthorized users always receive a short rejection in private chats. In group chats, unauthorized events are ignored to avoid noisy group responses. In both cases, Codex is never invoked.

## Codex Runner

New task command:

```bash
<effective_codex_command> exec --json -C <cwd> -s <sandbox> [-m <model>] [extra_args...] "<prompt>"
```

Resume command:

```bash
<effective_codex_command> exec --json -C <cwd> -s <sandbox> [-m <model>] [extra_args...] resume <session_id> "<reply>"
```

Argument construction:

- `cwd` comes from selected project alias, then `workspace.default`.
- `sandbox` comes from project config, then `codex.sandbox`, then `workspace-write`.
- `model` comes from project config, then `codex.default_model`; omit `-m` when empty.
- `extra_args` from `codex.extra_args` are Codex exec global args only. They are appended after common flags and before the prompt or `resume` subcommand.
- The config key `approval = "never"` is retained for future compatibility, but the current `codex exec` version may not accept `-a`; the runner must only pass approval flags after `doctor` confirms the local `codex exec --help` supports them. If unsupported, `doctor` should warn and runtime should omit the flag.
- New task creation must snapshot effective command, cwd, sandbox, model, approval, approval-flag support decision, and extra args into `tasks`.
- Resume must use the same effective command, cwd, sandbox, model, approval behavior, and extra args stored on the task at creation time unless the user explicitly changes task config in a future feature. This prevents an old task from changing behavior because the config file changed after the task was created.

Codex capability detection:

- `doctor` runs `<effective_codex_command> exec --help` and verifies `--json`, `resume`, `-C`/`--cd`, `-s`/`--sandbox`, and `-m`/`--model` support.
- `doctor` runs `<effective_codex_command> exec resume --help` and verifies resume accepts a session id and prompt.
- Missing required capabilities fail `doctor`.
- Approval flag support is optional. If `-a`/`--approval` is not present, `effective_approval_flag_supported` is false and runtime omits approval flags while still storing the configured approval value.

Codex JSONL parser contract:

- Parse stdout as newline-delimited JSON. Stderr is captured separately and never parsed as JSON.
- Session id comes from the first event shaped like `{"type":"thread.started","thread_id":"..."}`. Store this value in `tasks.codex_session_id` and `runs.codex_session_id`. For compatibility, also accept a top-level string field named `session_id`.
- Final assistant text comes from the last event shaped like `{"type":"item.completed","item":{"type":"agent_message","text":"..."}}`.
- `{"type":"turn.completed","usage":...}` marks normal Codex turn completion, but process exit code remains authoritative.
- If stdout contains malformed JSON, no thread id is found, or no final assistant text is found, mark the run failed with a redacted parse error.
- Parser tests must use fixtures captured from the local `codex exec --json` version plus fake failure fixtures.

The runner must:

- Capture stdout JSONL and stderr.
- Persist raw JSONL to a run log with `0600` file permissions under `~/.codex-feishu-bridge/logs`.
- Parse session id from JSONL events.
- Parse final assistant output.
- Preserve exit code and error text.
- Update `tasks.codex_session_id` as soon as a session id is known.
- Redact secrets and local absolute paths before sending any error, log tail, or final text to Feishu. Local logs may keep full raw JSONL, but Feishu-facing text must be truncated and redacted.
- Keep a configurable log retention window. MVP default is 14 days from `codex.log_retention_days`.
- Log pruning runs at daemon startup and then once per day while `serve` is running. Pruning deletes run log files older than the retention window and may keep their SQLite `runs` rows with `log_path` marked missing.

If session id cannot be parsed, mark the run failed and do not allow resume for that task.

## Concurrency

MVP allows fully parallel tasks. The daemon does not serialize globally or per project.

This is intentional: users are responsible for avoiding concurrent edits in the same workspace. The daemon only records `cwd`, prompt, run logs, status, and exit code so conflicts can be traced.

One constraint still applies: a single task/session may have only one active run at a time. If a second resume request arrives while that task is already running, MVP rejects it with a "task is already running" card instead of queuing or launching a second resume against the same Codex session.

Atomic enforcement:

- `StartResumeRun` opens an immediate SQLite transaction.
- It resolves the task and verifies the requester is `tasks.created_by`.
- It attempts to insert a `runs.status = 'running'` row. The partial unique index `runs_one_active_per_task` rejects concurrent active runs for the same task.
- On unique constraint failure, the router sends the "task is already running" card and does not invoke Codex.
- On success, it updates `tasks.status = 'running'` and commits before starting the Codex process.
- `FinishRun` updates the run and task status in one transaction.

Future versions may add optional locks:

- global lock,
- per-project lock,
- per-task queue,
- explicit `--parallel=false`.

## Error Handling

- Feishu WebSocket disconnect: reconnect automatically.
- Unauthorized user in private chat: send a short rejection and do not run Codex.
- Unauthorized user in group chat: ignore and do not run Codex.
- Unknown project alias: send config error card, do not run Codex.
- Missing default workspace: fail `doctor` and reject tasks.
- Codex executable missing: `doctor` reports failure; runtime sends failure card.
- Codex non-zero exit: mark run failed and send failure card with exit code and log tail.
- No session id parsed: mark task failed and block resume.
- Reply cannot be routed: send "reply from task card or start a new /codex task".
- SQLite write failure: fail fast for the current event and log locally.
- Feishu-facing error summaries must be capped at 2,000 characters after redaction.
- Never include Feishu app secrets, Codex credentials, proxy URLs with credentials, or local absolute paths in Feishu cards.
- Duplicate or replayed Feishu events are acknowledged and ignored before any task or run is created.
- Feishu rate limits: respect `Retry-After` when available, otherwise use exponential backoff with a small capped retry count.
- Start card send failure: mark the task/run failed and do not start Codex, because no routeable Feishu message exists for continuation.
- Completion/failure card send failure: keep the persisted run result, log the notification failure locally, and keep existing start-card routes valid for later continuation if `codex_session_id` exists.
- Missing Feishu message id after sending a routeable card: treat as send failure.
- Route insertion failure after card send: retry the SQLite insert. If insertion still fails, log a critical local error. For start cards, fail the task and do not run Codex. For result cards, keep the run result and rely on older task routes if they exist.
- Daemon restart recovery: on startup, mark any `runs.status = 'running'` rows as `failed` with a restart reason, update affected task status to `failed` if that run was the latest run, and do not try to reattach to orphaned Codex processes in MVP.
- A task whose latest run failed after restart may be resumed only if `tasks.codex_session_id` is already known.

## CLI

MVP binary subcommands:

```text
codex-feishu-bridge serve
codex-feishu-bridge init-config
codex-feishu-bridge doctor
codex-feishu-bridge tasks list
codex-feishu-bridge tasks show <task_id>
```

`doctor` should validate:

- config exists and parses,
- Feishu app id is present,
- app secret env var is set,
- Codex executable is available,
- required `codex exec` capabilities are present,
- configured approval behavior is either supported by the local Codex binary or will be safely omitted with a warning,
- default workspace exists,
- project aliases exist,
- SQLite database is writable and migrations can run,
- log directory exists or can be created with private permissions.

## Testing

Unit tests:

- `/codex` command parsing.
- Project alias parsing.
- `InboundEvent`, `OutboundMessage`, `RunResult`, and taskstore transaction contract behavior.
- User allowlist validation.
- Feishu event normalization.
- Feishu card `continue_submit` payload parsing, empty text rejection, and card body truncation.
- Feishu message id to task route lookup.
- Codex JSONL parsing for success, failure, and missing session id.
- Codex JSONL parsing for `thread.started.thread_id`, `item.completed` agent text, malformed stdout JSON, stderr separation, and non-zero exit.
- Codex capability detection for `--json`, `resume`, `-C`, `-s`, `-m`, and optional approval support.
- SQLite store creation, updates, and recovery.
- SQLite migrations, primary/foreign keys, partial unique active-run index, and status transition checks.
- Effective Codex argument precedence for default workspace, project alias, sandbox, model, and extra args.
- Feishu-facing redaction for secrets and local absolute paths.
- Unauthorized users cannot create or resume tasks.
- Creator-only continuation rejects another allowlisted user's reply to the same card.
- Same-task concurrent resume is rejected while other tasks may still run in parallel.
- Task creation snapshots effective command, cwd, sandbox, model, approval, approval-flag support decision, and extra args.
- Resume uses the task's stored runner config even if the config file has changed.
- Config changes to approval after task creation do not affect resume args for that task.
- Config changes to `codex.command` and project/default `cwd` after task creation do not affect resume command or working directory for that task.
- Event verification rejects wrong app id, invalid token/signature, and malformed callback payloads.
- Event dedup rejects repeated event ids or callback tokens without invoking Codex, and handles fallback dedup keys.
- Startup recovery marks stale running runs failed and keeps resumable tasks only when a Codex session id is known.
- Feishu send failures, rate limits, missing message ids, and route insertion failures follow the defined failure policy.
- Log pruning respects default and custom retention windows.
- Run logs are created with `0600` permissions.

Integration tests:

- fake transport sends `/codex`.
- fake Codex binary emits JSONL.
- daemon creates task, stores session id, and sends completion card.
- fake card reply triggers `codex exec resume`.
- fake unauthorized user event is rejected before Codex runner is invoked.
- fake reconnect sequence preserves existing SQLite state and can resume an existing task.
- fake Codex non-zero exit produces a redacted failure card and a run record.
- fake config change between task creation and resume mutates `codex.command` and project/default `cwd`, then verifies resume still uses the task's stored command and working directory.
- fake duplicate Feishu event is acknowledged once and does not create a second run.
- fake result card send failure persists the run result and leaves existing start-card route usable.
- fake daemon restart marks an in-flight run failed and allows resume only when the stored Codex session id exists.

Manual E2E:

- configure real Feishu app WebSocket.
- send `/codex ...` in private chat.
- receive start and completion cards.
- reply to completion card with follow-up.
- confirm `resume` runs and sends a new result.
- inspect SQLite and run logs.

## Open Source Packaging

Repository should include:

- `README.md`
- `config.example.yaml`
- Feishu app setup guide
- macOS LaunchAgent example
- Linux systemd example
- local development guide
- security model documentation
- troubleshooting guide

The README should clearly state that the tool runs Codex locally with the user's local Codex auth and local filesystem permissions.

## Initial MVP Acceptance Criteria

- A user can run `init-config`, edit config, and pass `doctor`.
- A user can start `serve` locally.
- Feishu WebSocket connects without requiring a public callback URL.
- `/codex <prompt>` starts a Codex task.
- Daemon sends a start card and a final result card.
- Result card reply resumes the same Codex session.
- SQLite records tasks, message routes, and runs.
- Logs contain enough detail to debug failed Codex runs.
