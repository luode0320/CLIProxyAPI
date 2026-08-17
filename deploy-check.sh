#!/usr/bin/env bash
#
# deploy-check.sh - Pre-deploy validation for Docker Compose persistence mounts.
#
# Verifies that the host paths used by docker-compose.yml / docker-compose.cluster.yml
# exist and are of the expected type (config.yaml must be a regular FILE, never a
# directory), prints a host -> container mount summary, and exits non-zero on fatal
# issues so deploys do not silently lose data.
#
# Usage:
#   ./deploy-check.sh             # validate standalone compose mounts
#   ./deploy-check.sh --cluster   # validate cluster compose mounts
#
# Exit codes: 0 = OK (warnings possible), 1 = fatal issue, fix and re-run.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# --- Load only the CLI_PROXY_* path variables from .env (mirrors compose behavior
#     without sourcing arbitrary content). ---
if [[ -f .env ]]; then
  while IFS= read -r line; do
    [[ "$line" =~ ^[[:space:]]*CLI_PROXY_([A-Z_]+)=(.+)$ ]] || continue
    key="CLI_PROXY_${BASH_REMATCH[1]}"
    value="${BASH_REMATCH[2]}"
    # strip optional surrounding quotes
    value="${value%\"}"; value="${value#\"}"
    value="${value%\'}"; value="${value#\'}"
    export "$key=$value"
  done < .env
fi

MODE="${1:-}"
CLUSTER=0
if [[ "$MODE" == "--cluster" ]]; then
  CLUSTER=1
elif [[ -n "$MODE" ]]; then
  echo "Error: unknown option '${MODE}'." >&2
  echo "Usage: ./deploy-check.sh [--cluster]" >&2
  exit 1
fi

# --- Resolve mount paths (same defaults as the compose files) ---
CONFIG_PATH="${CLI_PROXY_CONFIG_PATH:-./config.yaml}"
AUTH_PATH="${CLI_PROXY_AUTH_PATH:-./auths}"
LOG_PATH="${CLI_PROXY_LOG_PATH:-./logs}"
PLUGIN_PATH="${CLI_PROXY_PLUGIN_PATH:-./plugins}"
HOME_PATH="${CLI_PROXY_HOME_PATH:-./home}"

fatal=0
warn=0

echo "=== CLIProxyAPI pre-deploy check ==="
echo "Mode: $([[ $CLUSTER -eq 1 ]] && echo 'cluster' || echo 'standalone')"
echo "Working directory: $SCRIPT_DIR"

# --- Fatal: config.yaml must be a regular file (standalone mode only) ---
if [[ $CLUSTER -eq 0 ]]; then
  echo ""
  echo "[config] ${CONFIG_PATH} -> /CLIProxyAPI/config.yaml"
  if [[ -f "$CONFIG_PATH" ]]; then
    echo "  OK: config.yaml is a regular file"
  else
    fatal=1
    echo "  FAIL: '${CONFIG_PATH}' is missing or not a regular file."
    echo "  Docker would auto-create a DIRECTORY at this path, which shadows the"
    echo "  container's /CLIProxyAPI/config.yaml and prevents startup."
    echo "  Fix: cp config.example.yaml ${CONFIG_PATH}"
    echo "       (or point CLI_PROXY_CONFIG_PATH at an existing config file)."
  fi
fi

# --- Directory mounts ---
check_dir() {
  local host="$1" container="$2" label="$3"
  echo ""
  echo "[${label}] ${host} -> ${container}"
  if [[ -d "$host" ]]; then
    if [[ -z "$(ls -A "$host" 2>/dev/null)" ]]; then
      warn=1
      echo "  WARN: directory exists but is EMPTY."
      echo "  Re-deploys will keep pointing at this same directory (OK), but if the"
      echo "  working directory or CLI_PROXY_*_PATH changed since the last deploy,"
      echo "  container data may be sitting in a different host directory."
    else
      echo "  OK: directory exists with content"
    fi
  elif [[ -e "$host" ]]; then
    warn=1
    echo "  WARN: path exists but is NOT a directory (a file?)."
    echo "  Mounting a file where a directory is expected can hide container data."
  else
    warn=1
    echo "  WARN: directory does not exist yet."
    echo "  Docker will auto-create an EMPTY directory here. Fine for the first"
    echo "  deploy, but confirm the compose working directory and CLI_PROXY_*_PATH"
    echo "  so re-deploys always point at the SAME host directory."
  fi
}

check_dir "$PLUGIN_PATH" "/CLIProxyAPI/plugins" "plugins"
check_dir "$LOG_PATH" "/CLIProxyAPI/logs" "logs"

if [[ $CLUSTER -eq 1 ]]; then
  check_dir "$HOME_PATH" "/root/.cli-proxy-api" "home"
else
  check_dir "$AUTH_PATH" "/root/.cli-proxy-api" "auths"
fi

# --- Mount summary ---
echo ""
echo "=== Mount summary (host -> container) ==="
if [[ $CLUSTER -eq 1 ]]; then
  echo "  ${HOME_PATH}    -> /root/.cli-proxy-api"
else
  echo "  ${CONFIG_PATH}  -> /CLIProxyAPI/config.yaml"
  echo "  ${AUTH_PATH}    -> /root/.cli-proxy-api"
fi
echo "  ${LOG_PATH}     -> /CLIProxyAPI/logs"
echo "  ${PLUGIN_PATH}  -> /CLIProxyAPI/plugins"

if [[ $fatal -eq 1 ]]; then
  echo ""
  echo "FATAL: fix the issues above before deploying. Aborting."
  exit 1
fi

if [[ $warn -eq 1 ]]; then
  echo ""
  echo "WARNINGS: review the warnings above. Deploy can continue."
else
  echo ""
  echo "All checks passed."
fi
exit 0
