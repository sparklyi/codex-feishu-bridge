.PHONY: test build tidy lint doctor

test:
	go test ./...
	scripts/test-init-local-config.sh

build:
	go build -o bin/codex-feishu-bridge ./cmd/codex-feishu-bridge

tidy:
	go mod tidy

lint:
	go vet ./...

doctor:
	@test -n "$(CONFIG)" || { echo "usage: make doctor CONFIG=/path/to/config.yaml" >&2; exit 2; }
	go run ./cmd/codex-feishu-bridge doctor --config "$(CONFIG)"
