# Feishu Setup

1. Create a Feishu app and enable bot capability.
2. Enable event subscription through WebSocket mode. No public callback URL is required.
3. Subscribe to message receive events and card action callbacks.
4. Copy the app id into `feishu.app_id`.
5. Store the app secret in an environment variable and set `feishu.app_secret_env`.
6. Copy the bot open id into `feishu.bot_open_id`.
7. Add your user open id to `security.allowed_open_ids`.
8. If the network requires a proxy, set `feishu.proxy_url` to an `http://` proxy URL. Omit it for direct Feishu REST and WebSocket connections.

Private-chat plain text starts a task. Use `@backend fix the failing router test` to choose a configured project. Group chat works only for allowlisted users and requires mentioning the bot, for example `@Codex @backend fix the failing router test`. If the bot is mentioned without a project, the bridge returns a project selection card.

Send `/sessions` in a private chat to select and attach a Codex Desktop thread. The attached task card accepts follow-up text and can stop a running turn. Every turn runs with fixed full access and does not show an approval card.

Run:

```bash
export FEISHU_APP_SECRET=...
codex-feishu-bridge doctor --config ~/.codex-feishu-bridge/config.yaml
codex-feishu-bridge serve --config ~/.codex-feishu-bridge/config.yaml
```
