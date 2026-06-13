# Development

Prerequisites:

- Go 1.22+
- Codex CLI available on `PATH`
- A Feishu app for manual E2E testing

Run tests:

```bash
go test ./...
```

Build:

```bash
go build -o bin/codex-feishu-bridge ./cmd/codex-feishu-bridge
```

The test suite uses fake Feishu transports and fake Codex binaries for deterministic coverage. To refresh a Codex JSONL fixture manually:

```bash
codex exec --skip-git-repo-check -s read-only --json 'Reply with exactly OK.'
```

Keep stdout JSONL event lines only.
