# Development

Prerequisites:

- Go 1.26
- Standalone Codex CLI with `app-server` support on `PATH`; do not point `app_server.command` at the executable bundled inside Codex Desktop
- A Feishu app for manual end-to-end testing

Run the test suite:

```bash
go test ./...
go vet ./...
scripts/test-init-local-config.sh
```

Build:

```bash
go build -o bin/codex-feishu-bridge ./cmd/codex-feishu-bridge
```

The test suite uses fake Feishu transports and a fake app-server API. For a real local protocol probe, configure an accessible workspace and run:

```bash
codex-feishu-bridge doctor --config ~/.codex-feishu-bridge/config.yaml
```

The bridge starts its own local app-server process; do not start a second bridge instance for normal manual testing.
