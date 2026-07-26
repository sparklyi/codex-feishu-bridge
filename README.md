# codex-feishu-bridge

[English](README.md) | [简体中文](README.zh-CN.md)

`codex-feishu-bridge` is a personal local daemon for controlling Codex from a trusted Feishu private chat. It uses Codex `app-server` over local stdio, so it can create new work or take over a thread already visible in Codex Desktop. Events from non-private chats are ignored.

## Quick Start

```bash
go install github.com/sparklyi/codex-feishu-bridge/cmd/codex-feishu-bridge@latest
codex-feishu-bridge init-config
```

Edit `~/.codex-feishu-bridge/config.yaml`, then run:

```bash
export FEISHU_APP_SECRET=...
codex-feishu-bridge doctor
codex-feishu-bridge serve
```

`doctor` starts a local app-server process and verifies that desktop threads can be listed. `serve` performs the same probe before accepting Feishu events.

Set `app_server.command` to the standalone Codex CLI installed by the official installer. Do not point it at the executable bundled inside Codex Desktop; the standalone CLI provides the supported app-server interface for this bridge.

When the network requires a proxy, set `feishu.proxy_url` to an `http://` proxy URL. Leave it unset for direct Feishu REST and WebSocket connections; environment proxy variables are ignored by the bridge.

## Feishu Workflow

For a screenshot-based setup guide, see [Feishu Bot Quickstart (Chinese)](docs/feishu-quickstart.zh-CN.md).

Start a new task in a private chat:

```text
explain this repository
@backend fix the failing test
```

Use `@backend` only when `projects.backend` is configured; omit the prefix to use `workspace.default`.

To continue an existing Codex Desktop thread, send `/sessions` in a private chat, select a thread, then use the attached task card to send a follow-up. The bridge keeps the Codex thread id locally and resumes it through app-server.

While a turn is running, one task card remains attached to that turn. It shows the current phase and key milestones from app-server item events, without exposing reasoning, commands, or command output. Use **Add to current turn** to steer the active Codex turn with an extra constraint; it does not create another task or card. Stop is acknowledged immediately and does not wait for the app-server interrupt to finish.

When the turn completes, the same card becomes a compact result with conclusion, changes, and verification. Select **View details** to open a separate paged card containing the final agent reply.

`feishu.card_display_mode` defaults to `concise`, which only displays phases and milestones. Set it to `preview` to additionally show a throttled reply draft while the turn is running. This mode never exposes raw reasoning or tool output.

## Commands

```bash
codex-feishu-bridge init-config [--config path] [--force]
codex-feishu-bridge doctor [--config path]
codex-feishu-bridge serve [--config path]
codex-feishu-bridge tasks list [--config path]
codex-feishu-bridge tasks show [--config path] <task_id>
```

## Security Model

Only `security.allowed_open_ids` can use the bridge. Unauthorized private-chat requests receive a rejection, and all non-private events are ignored. Task continuation and stopping are creator-only.

Feishu cards are sent only to private chats and redact local absolute paths, secrets, proxy credentials, and full Codex thread ids. SQLite stores the local task-to-thread and task-to-turn mapping under `~/.codex-feishu-bridge/state.db`.

## Local Permissions

The bridge has one execution path: the configured local `app_server` command and the app-server protocol. It keeps task state only, not raw execution transcripts. Every turn is fixed to `danger-full-access` and `approvalPolicy: never`; there is no project-level permission override or Feishu approval card.

## License

MIT
