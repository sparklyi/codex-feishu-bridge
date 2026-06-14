#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

CONFIG_PATH="$TMP_DIR/config.yaml"
WORKSPACE="$TMP_DIR/workspace"
STATE_DB="$TMP_DIR/state/state.db"
LOG_DIR="$TMP_DIR/logs"

"$ROOT_DIR/scripts/init-local-config.sh" \
  --config "$CONFIG_PATH" \
  --app-id cli_test \
  --allowed-open-id ou_test \
  --workspace "$WORKSPACE" \
  --state-db "$STATE_DB" \
  --log-dir "$LOG_DIR" \
  --create-workspace

test -f "$CONFIG_PATH"
test -d "$WORKSPACE"
test -d "$(dirname "$STATE_DB")"
test -d "$LOG_DIR"

grep -q 'app_id: "cli_test"' "$CONFIG_PATH"
grep -q 'app_secret_env: "FEISHU_APP_SECRET"' "$CONFIG_PATH"
grep -q 'allowed_open_ids:' "$CONFIG_PATH"
grep -q '    - "ou_test"' "$CONFIG_PATH"
grep -q "  default: \"$WORKSPACE\"" "$CONFIG_PATH"
grep -q "  state_db: \"$STATE_DB\"" "$CONFIG_PATH"
grep -q "  log_dir: \"$LOG_DIR\"" "$CONFIG_PATH"

if grep -q 'dummy-secret' "$CONFIG_PATH"; then
  echo "config must not contain app secrets" >&2
  exit 1
fi

mode="$(stat -f %Lp "$CONFIG_PATH" 2>/dev/null || stat -c %a "$CONFIG_PATH")"
if [[ "$mode" != "600" ]]; then
  echo "config mode = $mode, want 600" >&2
  exit 1
fi

cd "$ROOT_DIR"
FEISHU_APP_SECRET=dummy-secret go test ./internal/config
