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
	go run ./cmd/codex-feishu-bridge doctor --config config.example.yaml
