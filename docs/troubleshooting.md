# Troubleshooting

Run `doctor` first:

```bash
codex-feishu-bridge doctor --config ~/.codex-feishu-bridge/config.toml
```

Common failures:

- `feishu.app_secret`: export the environment variable named by `[feishu].app_secret_env`.
- `workspace.default`: create the directory or fix the path.
- `codex.command`: install Codex or set `[codex].command`.
- `codex.exec.*`: update Codex CLI if required flags are missing.
- SQLite errors: check `~/.codex-feishu-bridge/state.db` parent directory permissions.
- Missing result card continuation: inspect `message_routes` and confirm Feishu returned a message id.
- Codex run failures: inspect private JSONL logs under `~/.codex-feishu-bridge/logs`.

After a daemon restart, running tasks are marked failed. Tasks are resumable only if a Codex session id had already been recorded.
