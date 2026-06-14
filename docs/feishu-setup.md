# Feishu Setup

1. Create a Feishu app and enable bot capability.
2. Enable event subscription through WebSocket mode. No public callback URL is required.
3. Subscribe to message receive events and card action callbacks.
4. Copy the app id into `feishu.app_id`.
5. Store the app secret in an environment variable and set `feishu.app_secret_env`.
6. Copy the bot open id into `feishu.bot_open_id`.
7. Add your user open id to `security.allowed_open_ids`.

Private chat plain text starts a new Codex task. Use `@backend fix the failing router test` to choose a configured project. Group chat works only for allowlisted users and requires mentioning the bot, for example `@Codex @backend fix the failing router test`. If the bot is mentioned without a project, the bridge returns a project selection card.

`/codex` is no longer the task entry point and returns a migration hint.

Task cards include Continue, Summarize, Explain error, Run tests, and MR description actions. Run tests and MR description require confirmation before resuming Codex.

Run:

```bash
export FEISHU_APP_SECRET=...
codex-feishu-bridge doctor --config ~/.codex-feishu-bridge/config.yaml
codex-feishu-bridge serve --config ~/.codex-feishu-bridge/config.yaml
```
