# Troubleshooting

Run `doctor` first:

```bash
codex-feishu-bridge doctor --config ~/.codex-feishu-bridge/config.yaml
```

Common failures:

- `feishu.app_secret`: export the environment variable named by `feishu.app_secret_env`.
- `workspace.default`: create the directory or fix the path.
- `app_server.command`: install a Codex CLI with app-server support or set `app_server.command` to its executable path.
- `app_server.probe`: ensure `app_server.command` points to the standalone Codex CLI installed by the official installer. The executable bundled inside Codex Desktop is not the supported app-server interface for this bridge.
- Feishu WebSocket reconnects or card updates time out: set `feishu.proxy_url` to an `http://` proxy URL when the network requires a proxy. Leave it unset to use direct connections; environment proxy variables are ignored.
- SQLite errors: check `~/.codex-feishu-bridge/state.db` and its parent directory permissions.
- Missing continuation: confirm Feishu returned a task-card message id and reply to that card.
- `/sessions` is empty: open or create a Codex Desktop thread first, then retry from a private chat.
- Group messages are ignored: mention the bot and include a project, for example `@Codex @backend fix the failing router test`.
- Configuration rejects an old permission field: remove `sandbox`, `approval`, `force_full_access`, and `approval_timeout_seconds`; those settings are no longer supported.
- State database is unsupported: stop the bridge and recreate `~/.codex-feishu-bridge/state.db`; the old approval-state schema is intentionally not migrated.

After a bridge restart, in-flight tasks are marked failed. Attached tasks remain resumable because their Codex thread id is retained in SQLite.
