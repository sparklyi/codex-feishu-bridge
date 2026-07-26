# codex-feishu-bridge

[English](README.md) | [简体中文](README.zh-CN.md)

`codex-feishu-bridge` is a personal local daemon for using Codex from Feishu. It uses Codex `app-server` over local stdio, so it can create new work or take over a thread already visible in Codex Desktop.

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

In group chats, mention the bot and include a project:

```text
@Codex @backend fix the failing test
```

To continue an existing Codex Desktop thread, send `/sessions` in a private chat, select a thread, then use the attached task card to send a follow-up. The bridge keeps the Codex thread id locally and resumes it through app-server.

While a turn is running, its task card streams progress and exposes Stop. Card actions are acknowledged immediately, so Stop does not wait for the app-server interrupt to finish.

## Commands

```bash
codex-feishu-bridge init-config [--config path] [--force]
codex-feishu-bridge doctor [--config path]
codex-feishu-bridge serve [--config path]
codex-feishu-bridge tasks list [--config path]
codex-feishu-bridge tasks show [--config path] <task_id>
```

## Security Model

Only `security.allowed_open_ids` can use the bridge. Private-chat unauthorized requests receive a rejection; group-chat unauthorized requests are ignored. Task continuation and stopping are creator-only.

Feishu cards redact local absolute paths, secrets, proxy credentials, and full Codex thread ids. SQLite stores the local task-to-thread and task-to-turn mapping under `~/.codex-feishu-bridge/state.db`.

## Local Permissions

The bridge has one execution path: the configured local `app_server` command and the app-server protocol. It keeps task state only, not raw execution transcripts. Every turn is fixed to `danger-full-access` and `approvalPolicy: never`; there is no project-level permission override or Feishu approval card.

## License

MIT
