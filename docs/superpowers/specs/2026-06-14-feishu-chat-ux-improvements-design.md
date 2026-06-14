# Feishu Chat UX Improvements Design

## Status

Approved in design discussion on 2026-06-14.

## Goal

Improve the Feishu user experience for `codex-feishu-bridge` so the bot feels like a practical chat assistant instead of a command wrapper. The first version should reduce command syntax, make task state visible immediately, improve card readability, and provide a small set of high-frequency shortcut actions without changing the core local Codex execution model.

The product goals are:

1. A private chat user can send a normal message and get a Codex task without typing `/codex`.
2. Group chats require an explicit bot mention to avoid accidental execution.
3. The bot replies immediately with a compact status card so users know Codex is running.
4. Cards expose common actions such as continue, summarize, explain error, run tests, and generate an MR description.
5. Reply routing remains explicit: old task continuation still happens through task card replies or card actions, not through guessing from ordinary chat messages.

## Non-Goals

- Do not introduce heuristic project auto-selection in the first version.
- Do not queue tasks by default.
- Do not inject follow-up text into a currently running `codex exec` process.
- Do not infer that an ordinary root message belongs to the latest task.
- Do not keep `/codex` as a supported command entry point.
- Do not implement streaming token-by-token output into Feishu.
- Do not implement stop/cancel execution in the first version unless it already exists as a safe runner capability.
- Do not add a web dashboard or non-Feishu UI.

## User Decisions

- Entry model: progressive enhancement of the existing task/run/message-route model.
- Private chat: normal text starts a new task.
- Project selection: configuration-driven only. `@project prompt` selects a configured project alias.
- Group chat: only `@bot @project prompt` starts a task directly.
- Group chat without project: return a project selection card.
- Running conflict: if the same chat/user already has a running task, return a running status card instead of queuing or merging input.
- Card style: compact status card.
- Shortcut behavior: low-risk shortcuts execute immediately; higher-cost shortcuts require confirmation.

## Interaction Model

### New Task Entry

Private chat root messages are new task intents:

```text
fix the failing router test
@backend fix the failing router test
```

Rules:

- If the message starts with `@project`, the parser resolves `project` against configured project aliases.
- If no project is specified in private chat, the default project is used.
- If the project alias is unknown, the bot sends a project selection or unknown-project card and does not run Codex.
- If the message starts with `/codex`, the bot sends a migration hint saying commands are no longer needed and does not run Codex. This avoids silently executing stale command syntax as user prompt text.

Group chat root messages only become new task intents when the bot is explicitly mentioned:

```text
@Codex @backend fix the failing router test
```

Rules:

- `@Codex @project prompt` starts a task for the selected configured project.
- `@Codex prompt` returns a project selection card and does not run Codex.
- Plain group chat messages are ignored.
- Unknown project aliases return a project selection or unknown-project card and do not run Codex.

### Existing Task Continuation

Continuation remains route-based:

- A text reply to a start/result card resumes that task.
- A card action from a start/result card resumes that task or opens a confirmation flow.
- The router must not fall back to latest task when route lookup misses.
- Creator-only continuation remains unchanged unless future requirements explicitly add shared task ownership.

### Running Task Conflict

Before creating a new task, the router checks whether the same chat and same creator already has a running task.

If there is an active task:

- Do not create a new task.
- Do not queue the message.
- Do not pass the message to the running Codex process.
- Send a compact running status card that shows task id, project, elapsed time if available, and a short explanation.

For group chats, this response is only sent when the incoming message explicitly mentioned the bot.

## Card Design

Use compact Feishu interactive cards with clear hierarchy:

- Colored header for status.
- Short task id and project alias near the top.
- Status label: received, running, succeeded, failed, needs project, blocked by running task.
- Prompt or result summary in one short markdown section.
- Metadata fields: project, elapsed time, run kind, workspace label after redaction.
- A single action row for high-frequency commands.
- A multiline follow-up input where free-form continuation is appropriate.

Card kinds:

1. Running card
   - Sent immediately after task admission and before Codex finishes.
   - Shows "Codex is running" and the prompt summary.
   - Has a free-form Continue input only when resume is valid; otherwise shows actions that make sense while running.

2. Success card
   - Shows final answer summary.
   - Includes shortcut actions: summarize, run tests, generate MR description, continue.

3. Failure card
   - Shows error summary and stderr tail after redaction.
   - Includes shortcut actions: explain error, run tests, continue.

4. Running conflict card
   - Shows the currently running task and tells the user to wait for completion before sending a new task.
   - May include "view current task" if the current card can route back to the task. "Remind me" is reserved for a later feature.

5. Project selection card
   - Used when group chat mentions the bot without `@project`, or when a project alias is unknown.
   - Shows configured project aliases as buttons.
   - Button click starts the original pending prompt with the selected project.

## Shortcut Actions

Shortcut actions use card callbacks and map to predefined prompts.

Immediate actions:

- Summarize: resume with a prompt such as "Summarize the current task result and next steps."
- Explain error: resume with a prompt such as "Explain the latest error and suggest the next debugging step."

Confirmed actions:

- Run tests.
- Generate MR description.

Feishu buttons should not be designed as if they can directly fill a sibling input client-side. For confirmed actions, the bot should send a confirmation card or update flow that shows the generated prompt and asks the user to execute or cancel. This keeps the behavior implementable with Feishu card callbacks and avoids accidental expensive runs.

Free-form continuation:

- The compact task card keeps a multiline input and submit button for user-written continuation.
- Empty input is rejected before creating a resume run.

## Architecture

### Intent Parser

Add a small parser layer that converts normalized Feishu events into task intents. It should be independent of Feishu SDK types and easy to unit test.

Suggested outputs:

- `StartTaskIntent`
  - `Prompt`
  - `ProjectAlias`
  - `RequiresProjectSelection`
  - `UnknownProjectAlias`
  - `IsBotMentioned`
  - `ChatType`
- `ContinuationIntent`
  - existing reply or card route data
- `IgnoredIntent`
  - plain group messages or unsupported events
- `MigrationHintIntent`
  - `/codex` syntax was used and should produce guidance instead of execution

The parser should know only the configured project aliases and chat context. It should not inspect the filesystem or infer a project from prompt text.

### Transport Normalization

Extend Feishu normalization to expose bot mention information for group messages.

Preferred approach:

- Parse Feishu message mention metadata when available.
- Compare mention identity against a configured bot identifier or a transport-provided bot identity.
- Strip the bot mention from the prompt before intent parsing.

If bot identity cannot be reliably derived, require a config value for the bot mention identity and have `doctor` warn when group-chat triggering is configured but the identity is missing.

### Router

Router remains the orchestration layer:

- Authorize the sender.
- Convert inbound event to intent.
- Reject or ignore non-executable intents.
- Check running task conflict for new task intents.
- Resolve project selection card callbacks.
- Admit new task or resume run through the store.
- Send the appropriate card via notifier.

Router should not contain Feishu card layout details.

### Store

Reuse existing `tasks`, `runs`, `message_routes`, and `event_dedup` where possible.

Add only the minimum persistence needed for project selection cards:

- `pending_intents`
  - pending id.
  - chat id.
  - creator open id.
  - prompt.
  - available project aliases.
  - status: pending, consumed, expired.
  - created at and expires at.

Project selection callbacks consume a pending intent exactly once. Expired pending intents should not start tasks.

Add a query for active tasks by chat and creator:

- private chat conflict: same chat id and sender.
- group chat conflict: same chat id and sender, only after explicit bot mention.

This keeps users from blocking each other in shared group chats while still preventing one user's accidental concurrent runs.

### Notifier

Notifier owns bridge-level card content:

- Status titles.
- Prompt/result summaries.
- Shortcut button definitions.
- Redacted workspace labels.
- Project selection card content.
- Running conflict card content.

Notifier should emit structured `OutboundMessage` data that the Feishu sender can render. It should not build raw Feishu JSON directly.

### Feishu Sender

The Feishu sender owns rendering `OutboundMessage` into interactive card JSON.

It should support:

- Header color/template per status.
- Markdown blocks.
- Field-like metadata blocks.
- Multiline input and submit action.
- Multiple buttons with callback payloads.
- Confirmation buttons for expensive shortcut actions.

The renderer must continue to preserve redaction guarantees and message id handling.

## Data Flow

### Private Chat New Task

```text
Feishu root message
  -> normalizer extracts text and chat context
  -> intent parser returns StartTaskIntent
  -> router checks authorization and active task conflict
  -> router resolves default or explicit project
  -> store admits task/run
  -> notifier sends running card
  -> runner executes Codex
  -> store finishes run
  -> notifier sends success/failure card
```

### Group Chat Missing Project

```text
Feishu root message with bot mention
  -> normalizer marks bot mention
  -> intent parser sees no project alias
  -> router stores pending intent
  -> notifier sends project selection card
  -> user clicks project
  -> router consumes pending intent
  -> normal new task flow starts
```

### Shortcut Action

```text
Card callback
  -> normalizer extracts action id and value
  -> router resolves message route or pending intent
  -> low-risk shortcut starts resume run directly
  -> high-cost shortcut sends confirmation card first
  -> confirmed callback starts resume run
```

## Error Handling

- Unauthorized private messages receive a compact rejection card.
- Unauthorized group messages remain silent unless the bot was explicitly mentioned; explicit mentions receive a compact rejection card.
- Unknown project aliases return a project selection card with available aliases.
- Missing project in group returns a project selection card.
- `/codex` messages return a migration hint card and do not run.
- Route misses return a routing error card; no latest-task fallback.
- Running conflicts return a running status card.
- Empty card input is rejected before creating a run.
- Failed Codex runs continue to produce failure cards with redacted stderr tail.

## Testing Plan

Parser tests:

- Private plain text becomes a start task intent.
- Private `@project prompt` selects a configured project.
- Private unknown `@project` returns unknown project.
- `/codex prompt` returns migration hint.
- Group plain text is ignored.
- Group bot mention without project requires project selection.
- Group bot mention with `@project` starts task.

Router tests:

- Private plain text creates a task.
- Group mention without project sends project selection and does not run.
- Project selection callback starts the pending task exactly once.
- Running task conflict sends running conflict card and does not create a second task.
- Shortcut immediate action creates a resume run with the configured prompt.
- Shortcut confirmation action sends confirmation first and runs only after confirmation.

Notifier and sender tests:

- Compact cards include status, task id, project, summary, and action payloads.
- Project selection card includes configured aliases.
- Confirmation card contains the preset prompt and execute/cancel buttons.
- Card JSON remains valid Feishu interactive card JSON.
- Redaction still hides local paths, secrets, proxy credentials, and Codex session ids.

Integration tests:

- End-to-end private plain message to running card to success card.
- End-to-end group missing project to selection card to task run.
- Reply to task card still resumes the right task.
- Route miss never falls back to latest task.

## Documentation Updates

Update user-facing docs after implementation:

- Replace `/codex` quickstart examples with plain private chat examples.
- Document `@project prompt` project selection.
- Document group usage as `@bot @project prompt`.
- Document project selection cards for group mentions without project.
- Document shortcut buttons and which ones require confirmation.
