# Security

`codex-feishu-bridge` is a local control plane for a personal Codex installation. It starts a local app-server process over stdio; it does not expose a public service endpoint.

Controls:

- User allowlist: `security.allowed_open_ids`.
- Unknown users never invoke Codex.
- Private unauthorized requests get a short rejection.
- Group unauthorized requests are silent.
- Continuations and stop requests require the original task creator.
- Card replies route through stored `message_routes`; card actions validate task ownership.
- Feishu-visible text is redacted and truncated.
- SQLite stores only bridge task state and the corresponding Codex thread and turn identifiers.

Every bridge turn is deliberately fixed to unattended `danger-full-access` with `approvalPolicy: never`. Permission settings are not configurable and no approval card is created. Keep the allowlist limited to accounts that may operate the local machine with those permissions.
