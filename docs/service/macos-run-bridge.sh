#!/bin/zsh

env_file="$HOME/.codex/private/agent-env.local"
if [[ ! -r "$env_file" ]]; then
  print -u2 "missing bridge environment file: $env_file"
  exit 1
fi

set -a
source "$env_file"
set +a

unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
export CODEX_FEISHU_BRIDGE_SUPERVISED=1

bridge="$HOME/GoProject/codex-feishu-bridge/bin/codex-feishu-bridge"
config="$HOME/.codex-feishu-bridge/config.yaml"
child_pid=""

stop_child() {
  if [[ -n "$child_pid" ]]; then
    kill -TERM "$child_pid" 2>/dev/null || true
    wait "$child_pid" 2>/dev/null || true
  fi
  exit 0
}

trap stop_child INT TERM HUP

while true; do
  "$bridge" serve --config "$config" &
  child_pid=$!
  wait "$child_pid"
  exit_status=$?
  child_pid=""

  if (( exit_status != 75 )); then
    exit "$exit_status"
  fi
  sleep 1
done
