# Security

`codex-feishu-bridge` is a local control plane for Codex. It does not proxy through Hermes and does not inject messages into Codex Desktop.

Controls:

- User allowlist: `[security].allowed_open_ids`.
- Unknown users never invoke Codex.
- Private unauthorized requests get a short rejection.
- Group unauthorized requests are silent.
- Continuations require the original task creator.
- Card replies route only through stored `message_routes`; there is no fallback to latest task.
- Feishu-visible text is redacted and truncated.
- Raw Codex JSONL logs stay local with `0600` file permissions.

The daemon stores full Codex session ids in SQLite so `codex exec resume` can work, but normal Feishu cards display only short task ids.
