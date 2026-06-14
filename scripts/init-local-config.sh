#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  scripts/init-local-config.sh \
    --app-id cli_xxx \
    --allowed-open-id ou_xxx \
    --workspace /path/to/repo \
    [--config ~/.codex-feishu-bridge/config.yaml] \
    [--app-secret-env FEISHU_APP_SECRET] \
    [--state-db ~/.codex-feishu-bridge/state.db] \
    [--log-dir ~/.codex-feishu-bridge/logs] \
    [--project-alias default] \
    [--codex-command codex] \
    [--sandbox workspace-write] \
    [--approval never] \
    [--model gpt-5] \
    [--create-workspace] \
    [--force]

The app secret is never written to config.yaml. Export the environment variable
named by --app-secret-env before running doctor or serve.
USAGE
}

yaml_quote() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

expand_path() {
  local value="$1"
  if [[ "$value" == "~/"* ]]; then
    printf '%s/%s' "$HOME" "${value#~/}"
  elif [[ "$value" == "." ]]; then
    pwd
  elif [[ "$value" == ./* ]]; then
    printf '%s/%s' "$(pwd)" "${value#./}"
  elif [[ "$value" == /* ]]; then
    printf '%s' "$value"
  else
    printf '%s/%s' "$(pwd)" "$value"
  fi
}

CONFIG_PATH="${HOME}/.codex-feishu-bridge/config.yaml"
APP_ID=""
APP_SECRET_ENV="FEISHU_APP_SECRET"
ALLOWED_OPEN_IDS=()
WORKSPACE=""
STATE_DB="${HOME}/.codex-feishu-bridge/state.db"
LOG_DIR="${HOME}/.codex-feishu-bridge/logs"
PROJECT_ALIAS="default"
CODEX_COMMAND="codex"
SANDBOX="workspace-write"
APPROVAL="never"
MODEL=""
CREATE_WORKSPACE=0
FORCE=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --config)
      CONFIG_PATH="$2"
      shift 2
      ;;
    --app-id)
      APP_ID="$2"
      shift 2
      ;;
    --app-secret-env)
      APP_SECRET_ENV="$2"
      shift 2
      ;;
    --allowed-open-id)
      ALLOWED_OPEN_IDS+=("$2")
      shift 2
      ;;
    --workspace)
      WORKSPACE="$2"
      shift 2
      ;;
    --state-db)
      STATE_DB="$2"
      shift 2
      ;;
    --log-dir)
      LOG_DIR="$2"
      shift 2
      ;;
    --project-alias)
      PROJECT_ALIAS="$2"
      shift 2
      ;;
    --codex-command)
      CODEX_COMMAND="$2"
      shift 2
      ;;
    --sandbox)
      SANDBOX="$2"
      shift 2
      ;;
    --approval)
      APPROVAL="$2"
      shift 2
      ;;
    --model)
      MODEL="$2"
      shift 2
      ;;
    --create-workspace)
      CREATE_WORKSPACE=1
      shift
      ;;
    --force)
      FORCE=1
      shift
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
if [[ ${#ALLOWED_OPEN_IDS[@]} -eq 0 ]]; then
  echo "--allowed-open-id is required" >&2
  exit 2
fi
if [[ -z "$WORKSPACE" ]]; then
  echo "--workspace is required" >&2
  exit 2
fi

CONFIG_PATH="$(expand_path "$CONFIG_PATH")"
WORKSPACE="$(expand_path "$WORKSPACE")"
STATE_DB="$(expand_path "$STATE_DB")"
LOG_DIR="$(expand_path "$LOG_DIR")"

if [[ -e "$CONFIG_PATH" && "$FORCE" -ne 1 ]]; then
  echo "config already exists: $CONFIG_PATH (use --force to overwrite)" >&2
  exit 1
fi

mkdir -p "$(dirname "$CONFIG_PATH")" "$(dirname "$STATE_DB")" "$LOG_DIR"
chmod 700 "$(dirname "$CONFIG_PATH")" "$(dirname "$STATE_DB")" "$LOG_DIR"
if [[ "$CREATE_WORKSPACE" -eq 1 ]]; then
  mkdir -p "$WORKSPACE"
fi

tmp="$(mktemp "${CONFIG_PATH}.tmp.XXXXXX")"
chmod 600 "$tmp"
{
  printf 'feishu:\n'
  printf '  app_id: %s\n' "$(yaml_quote "$APP_ID")"
  printf '  app_secret_env: %s\n' "$(yaml_quote "$APP_SECRET_ENV")"
  printf '  connection: websocket\n'
  printf 'security:\n'
  printf '  allowed_open_ids:\n'
  for open_id in "${ALLOWED_OPEN_IDS[@]}"; do
    printf '    - %s\n' "$(yaml_quote "$open_id")"
  done
  printf 'codex:\n'
  printf '  command: %s\n' "$(yaml_quote "$CODEX_COMMAND")"
  printf '  default_model: %s\n' "$(yaml_quote "$MODEL")"
  printf '  sandbox: %s\n' "$(yaml_quote "$SANDBOX")"
  printf '  approval: %s\n' "$(yaml_quote "$APPROVAL")"
  printf '  extra_args:\n'
  printf '    - --skip-git-repo-check\n'
  printf '  log_retention_days: 14\n'
  printf 'workspace:\n'
  printf '  default: %s\n' "$(yaml_quote "$WORKSPACE")"
  printf 'paths:\n'
  printf '  state_db: %s\n' "$(yaml_quote "$STATE_DB")"
  printf '  log_dir: %s\n' "$(yaml_quote "$LOG_DIR")"
  printf 'projects:\n'
  printf '  %s:\n' "$PROJECT_ALIAS"
  printf '    cwd: %s\n' "$(yaml_quote "$WORKSPACE")"
  printf '    model: %s\n' "$(yaml_quote "$MODEL")"
  printf '    sandbox: %s\n' "$(yaml_quote "$SANDBOX")"
  printf '    approval: %s\n' "$(yaml_quote "$APPROVAL")"
} >"$tmp"
mv "$tmp" "$CONFIG_PATH"
chmod 600 "$CONFIG_PATH"

cat <<EOF
Created $CONFIG_PATH

Next:
  export ${APP_SECRET_ENV}=<your Feishu app secret>
  codex-feishu-bridge doctor --config "$CONFIG_PATH"
  codex-feishu-bridge serve --config "$CONFIG_PATH"
EOF
