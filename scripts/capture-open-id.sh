#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  export FEISHU_APP_SECRET=<your Feishu app secret>
  scripts/capture-open-id.sh --app-id cli_xxx

Options:
  --app-id cli_xxx                 Feishu app id.
  --app-secret-env FEISHU_APP_SECRET
                                   Environment variable that stores app secret.
  --timeout 120s                   How long to wait for one incoming /codex message.

After the listener starts, send `/codex ping` to the bot in Feishu. The script
prints the sender open_id, chat id, and chat type. It never prints the app secret.
USAGE
}

APP_ID=""
APP_SECRET_ENV="FEISHU_APP_SECRET"
TIMEOUT="120s"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --app-id)
      APP_ID="$2"
      shift 2
      ;;
    --app-secret-env)
      APP_SECRET_ENV="$2"
      shift 2
      ;;
    --timeout)
      TIMEOUT="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$APP_ID" ]]; then
  echo "--app-id is required" >&2
  exit 2
fi
if [[ -z "${!APP_SECRET_ENV:-}" ]]; then
  echo "environment variable $APP_SECRET_ENV is required" >&2
  exit 2
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$ROOT_DIR/.tmp"
CAPTURE_DIR="$(mktemp -d "$ROOT_DIR/.tmp/feishu-openid-capture.XXXXXX")"
trap 'rm -rf "$CAPTURE_DIR"' EXIT

cat >"$CAPTURE_DIR/main.go" <<'GO'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sparklyi/codex-feishu-bridge/internal/contracts"
	"github.com/sparklyi/codex-feishu-bridge/internal/transport/feishu"
)

func main() {
	appID := flag.String("app-id", "", "Feishu app id")
	appSecretEnv := flag.String("app-secret-env", "FEISHU_APP_SECRET", "environment variable that stores app secret")
	timeout := flag.Duration("timeout", 120*time.Second, "wait timeout")
	flag.Parse()
	if *appID == "" {
		fmt.Fprintln(os.Stderr, "--app-id is required")
		os.Exit(2)
	}
	secret := os.Getenv(*appSecretEnv)
	if secret == "" {
		fmt.Fprintf(os.Stderr, "environment variable %s is required\n", *appSecretEnv)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	captured := errors.New("captured")
	source := feishu.NewSDKEventSource(*appID, secret, "")
	receiver := feishu.Receiver{Source: source, Verify: feishu.VerifyOptions{AppID: *appID}}
	fmt.Fprintln(os.Stderr, "Waiting for a Feishu message. Send `/codex ping` to the bot now.")
	err := receiver.Receive(ctx, func(ctx context.Context, ev contracts.InboundEvent) error {
		fmt.Printf("open_id=%s\n", ev.SenderOpenID)
		fmt.Printf("chat_id=%s\n", ev.ChatID)
		fmt.Printf("chat_type=%s\n", ev.ChatType)
		fmt.Printf("message_id=%s\n", ev.MessageID)
		return captured
	})
	if errors.Is(err, captured) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintln(os.Stderr, "timed out waiting for /codex message")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
GO

REL_DIR="${CAPTURE_DIR#"$ROOT_DIR/"}"
(
  cd "$ROOT_DIR"
  go run "./$REL_DIR" --app-id "$APP_ID" --app-secret-env "$APP_SECRET_ENV" --timeout "$TIMEOUT"
)
