# Feishu Setup

1. Create a Feishu app and enable bot capability.
2. Enable event subscription through WebSocket mode. No public callback URL is required.
3. Subscribe to message receive events and card action callbacks.
4. Copy the app id into `[feishu].app_id`.
5. Store the app secret in an environment variable and set `[feishu].app_secret_env`.
6. Add your user open id to `[security].allowed_open_ids`.

Private chat is recommended for MVP. Group chat works only for allowlisted users, and task continuation remains creator-only.

Run:

```bash
export FEISHU_APP_SECRET=...
codex-feishu-bridge doctor --config ~/.codex-feishu-bridge/config.toml
codex-feishu-bridge serve --config ~/.codex-feishu-bridge/config.toml
```
