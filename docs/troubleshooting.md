# Troubleshooting

Run `doctor` first:

```bash
codex-feishu-bridge doctor --config ~/.codex-feishu-bridge/config.yaml
```

Common failures:

- `feishu.app_secret`: export the environment variable named by `feishu.app_secret_env`.
- `workspace.default`: create the directory or fix the path.
- `codex.command`: install Codex or set `codex.command`.
- `codex.exec.*`: update Codex CLI if required flags are missing.
- SQLite errors: check `~/.codex-feishu-bridge/state.db` parent directory permissions.
- Missing result card continuation: inspect `message_routes` and confirm Feishu returned a message id.
- Plain group message ignored: mention the bot and include a project, for example `@Codex @backend fix the failing router test`.
- `/codex` returns a hint: send plain text in private chat, or `@project prompt` when selecting a configured project.
- Project selection card does not appear: confirm `feishu.bot_open_id` is configured and group message mention metadata is available.
- Shortcut button does not run: Run tests and MR description require confirming the second card; Summarize and Explain error run immediately.
- Codex run failures: inspect private JSONL logs under `~/.codex-feishu-bridge/logs`.

After a daemon restart, running tasks are marked failed. Tasks are resumable only if a Codex session id had already been recorded.
