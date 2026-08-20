#!/usr/bin/env bash
#
# deploy-wsl.sh - Deploy CLIProxyAPI to the local WSL Docker engine.
#
# Mirrors the GitHub Actions workflow `.github/workflows/deploy-server.yml`
# (which deploys to a remote server) but targets the LOCAL WSL instead:
#
#   server (CI):  buildx build -> scp image tar -> ssh: docker load + docker run
#   WSL (local):  sync source  -> docker build  ->        docker run (same mounts)
#
# Where it runs:
#   * From Windows (Git Bash): auto-detects the WSL distro, converts the repo
#     path, then re-invokes itself INSIDE WSL. This is the "Windows script".
#   * From inside WSL: executes the deployment directly.
#
# Deployment layout inside WSL (native filesystem, never /mnt/*, for speed and
# correct permission semantics):
#
#   ~/cli-proxy-api/
#   ├── src/            clean synced source tree (runtime data excluded)
#   ├── config/         config.yaml - bootstrapped from config.example.yaml
#   ├── logs/           -> /CLIProxyAPI/logs
#   ├── plugins/        -> /CLIProxyAPI/plugins   (plugin binaries survive)
#   ├── auths/          -> /root/.cli-proxy-api   (auth records / OAuth tokens)
#   ├── static/         -> /CLIProxyAPI/static
#   ├── pgstore/        -> /CLIProxyAPI/pgstore   (only when PGSTORE_DSN set)
#   ├── gitstore/       -> /CLIProxyAPI/gitstore  (only when GITSTORE_GIT_URL set)
#   ├── objectstore/    -> /CLIProxyAPI/objectstore (only when OBJECTSTORE_ENDPOINT set)
#   └── archive/        reserved for image tar backups (docker save)
#
# Usage (run from the repo root on Windows Git Bash, or inside WSL):
#   ./deploy-wsl.sh [--distro NAME] [--refresh-models] [--help]
#
# Options:
#   --distro NAME     WSL distro to deploy into (default: $WSL_DISTRO env, else
#                     the first non-Docker distro reported by `wsl -l -q`).
#   --refresh-models  Also refresh the model catalogs, like CI does. Off by
#                     default because it modifies tracked files in the repo
#                     and requires Go inside WSL.
#   --help            Show this help and exit.
#
# Environment:
#   WSL_DISTRO        Preferred WSL distro (overrides auto-detection).
#   DEPLOY_ROOT       WSL-side deploy root (default: $HOME/cli-proxy-api).
#   CLI_PROXY_IMAGE   Image tag (default: cli-proxy-api:latest).
#   GOPROXY           Go module proxy for the docker build (default:
#                     https://goproxy.cn,direct - set to the official proxy
#                     if you are not behind a mainland-China network).
#
# Rollback:
#   Every deploy first tags the image backing the running container as
#   cli-proxy-api:previous, so you can roll back by re-running the same
#   `docker run` command (see the deploy summary) with cli-proxy-api:previous.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- defaults ---
IMAGE="${CLI_PROXY_IMAGE:-cli-proxy-api:latest}"
PREV_IMAGE="cli-proxy-api:previous"
CONTAINER="cli-proxy-api"
REFRESH_MODELS=0
INSIDE=0
ARGS=()

usage() {
  sed -n '1,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//' | sed '/^$/d'
}

# --- parse arguments (works on both Windows and WSL sides) ---
while [[ $# -gt 0 ]]; do
  case "$1" in
    --distro)
      [[ $# -ge 2 ]] || { echo "Error: --distro requires a value." >&2; exit 1; }
      WSL_DISTRO="$2"; shift 2
      ;;
    --distro=*)
      WSL_DISTRO="${1#*=}"; shift
      ;;
    --refresh-models)
      REFRESH_MODELS=1; shift
      ;;
    --inside)
      INSIDE=1; shift
      ;;
    --help|-h)
      usage; exit 0
      ;;
    *)
      ARGS+=("$1"); shift
      ;;
  esac
done

# --- normalize `wsl -l -q` output (UTF-16LE on modern Windows, ANSI on older).
#     iconv must run inside the pipe: storing UTF-16 in a bash variable would
#     strip the NUL bytes and corrupt the text. ---
normalize_wsl_list() {
  if command -v iconv >/dev/null 2>&1; then
    iconv -f UTF-16LE -t UTF-8 2>/dev/null || cat
  else
    cat
  fi
}

# --- pick a WSL distro: $WSL_DISTRO > first non-Docker distro from `wsl -l -q` ---
detect_distro() {
  local list line
  list="$(wsl.exe -l -q 2>/dev/null | normalize_wsl_list | tr -d '\r' | sed 's/^[[:space:]*]*//' | grep -v '^$')" || return 1
  while IFS= read -r line; do
    case "$line" in
      docker-desktop|docker-desktop-data) continue ;;
    esac
    printf '%s\n' "$line"
    return 0
  done <<< "$list"
  return 1
}

# --- Windows side: re-invoke this script inside WSL ---
bridge_to_wsl() {
  command -v wsl.exe >/dev/null 2>&1 || {
    echo "ERROR: wsl.exe not found. This script requires WSL." >&2
    exit 1
  }

  local distro="${WSL_DISTRO:-}"
  if [[ -z "$distro" ]]; then
    distro="$(detect_distro)" || {
      echo "ERROR: no WSL distro found. Install one (wsl --install) or set WSL_DISTRO." >&2
      exit 1
    }
  fi
  echo "Using WSL distro: ${distro}"
  export WSL_DISTRO="$distro"

  # Convert the script's own directory to a WSL path (works from any cwd).
  # Use cygpath -m (forward slashes): MSYS argument conversion would otherwise
  # eat the backslashes of a `cygpath -w` output when passing it to wsl.exe.
  local win_dir wsl_dir script
  win_dir="$(cygpath -m "$SCRIPT_DIR" 2>/dev/null || echo "$SCRIPT_DIR")"
  wsl_dir="$(wsl.exe -d "$distro" -- wslpath -u "$win_dir" 2>/dev/null | tr -d '\r')"
  if [[ -z "$wsl_dir" ]]; then
    # Fallback when wslpath is unavailable: naive drive-letter mapping.
    wsl_dir="$(printf '%s' "$win_dir" | sed -E 's|^([A-Za-z]):|/mnt/\L\1|; s|\\|/|g')"
  fi
  script="${wsl_dir%/}/$(basename "$0")"
  # Existence must be checked INSIDE WSL: on the Windows side, `/mnt/f/...`
  # is not a valid path (Git Bash uses /f/...), so a plain [[ -f ]] would
  # always fail here.
  if ! wsl.exe -d "$distro" -- test -f "$script" >/dev/null 2>&1; then
    echo "ERROR: cannot locate script inside WSL at ${script}." >&2
    echo "Run this script from the repository root." >&2
    exit 1
  fi

  echo "Re-invoking inside WSL: ${script}"
  exec wsl.exe -d "$distro" -- bash "$script" --inside "${ARGS[@]}"
}

# --- copy the source tree to the native WSL filesystem ---
sync_source() {
  local src="$1" dst="$2"
  echo "--- Syncing source to ${dst} ---"
  mkdir -p "$dst"
  if command -v rsync >/dev/null 2>&1; then
    rsync -a --delete \
      --exclude=.git --exclude=.github --exclude=.workbuddy --exclude=.zcode \
      --exclude=auths --exclude=logs --exclude=plugins --exclude=config.yaml \
      --exclude=.env --exclude=deploy-wsl.sh --exclude=backups \
      "${src%/}/" "${dst%/}/"
  else
    echo "INFO: rsync not found; falling back to tar (slower)."
    tar -C "$src" \
      --exclude='./.git' --exclude='./.github' --exclude='./.workbuddy' \
      --exclude='./auths' --exclude='./logs' --exclude='./plugins' \
      --exclude='./config.yaml' --exclude='./.env' --exclude='./deploy-wsl.sh' \
      -cf - . | tar -C "$dst" -xf -
  fi
}

# --- WSL side: the actual deploy (mirrors deploy-server.yml's SSH script) ---
main() {
  local deploy_root="${DEPLOY_ROOT:-$HOME/cli-proxy-api}"
  local repo_dir="$SCRIPT_DIR"

  # When invoked directly from inside WSL, $WSL_DISTRO may be unset; resolve it
  # so the summary can print correct `wsl -d ...` hint commands.
  if [[ -z "${WSL_DISTRO:-}" ]]; then
    WSL_DISTRO="$(detect_distro 2>/dev/null || true)"
  fi

  # Preflight: docker must be reachable inside WSL.
  if ! command -v docker >/dev/null 2>&1; then
    echo "ERROR: 'docker' not found inside the WSL distro." >&2
    echo "Install Docker Engine in WSL, or enable Docker Desktop's" >&2
    echo "'Use the WSL 2 based engine' and set WSL_DISTRO=docker-desktop." >&2
    exit 1
  fi
  if ! docker info >/dev/null 2>&1; then
    echo "ERROR: Docker daemon is not running inside WSL." >&2
    echo "Start it: sudo service docker start   (or launch Docker Desktop)." >&2
    exit 1
  fi

  # Optional: refresh model catalogs (CI parity). Off by default because it
  # modifies tracked files in the repo; requires Go inside WSL.
  if [[ $REFRESH_MODELS -eq 1 ]]; then
    echo "--- Refreshing model catalogs (modifies tracked files in the repo) ---"
    if command -v go >/dev/null 2>&1; then
      (cd "$repo_dir" && bash .github/scripts/refresh-model-catalogs.sh)
    else
      echo "WARNING: 'go' not found inside WSL; skipping model catalog refresh." >&2
    fi
  fi

  # Build metadata (same env the CI workflow injects).
  local version commit build_date
  version="$(cd "$repo_dir" && git describe --tags --always --dirty 2>/dev/null || echo dev)"
  commit="$(cd "$repo_dir" && git rev-parse --short HEAD 2>/dev/null || echo none)"
  build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  # Persistent directories (all survive container recreation).
  local config_dir="$deploy_root/config"
  mkdir -p "$config_dir" "$deploy_root/logs" "$deploy_root/plugins" \
    "$deploy_root/auths" "$deploy_root/static" "$deploy_root/pgstore" \
    "$deploy_root/gitstore" "$deploy_root/objectstore" "$deploy_root/archive"

  # Build from a copy on the native filesystem (fast, correct permissions).
  sync_source "$repo_dir" "$deploy_root/src"

  echo "--- Building ${IMAGE} ---"
  echo "  VERSION=${version} COMMIT=${commit} BUILD_DATE=${build_date}"
  echo "  GOPROXY=${GOPROXY:-https://goproxy.cn,direct} (override with GOPROXY env)"
  docker build \
    --build-arg VERSION="$version" \
    --build-arg COMMIT="$commit" \
    --build-arg BUILD_DATE="$build_date" \
    --build-arg GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
    -t "$IMAGE" \
    "$deploy_root/src"

  # Save a rollback point: tag the image backing the running container.
  # Check container existence via `docker ps -aq` — a bare `docker inspect
  # $CONTAINER` would match the IMAGE of the same name when the container is
  # gone, and the image's JSON has no `.Image` key (template error).
  local cid
  cid="$(docker ps -aq --filter "name=^/${CONTAINER}$" | head -1 || true)"
  if [[ -n "$cid" ]]; then
    docker tag "$(docker inspect -f '{{.Image}}' "$cid")" "$PREV_IMAGE" >/dev/null 2>&1 || true
    echo "Rollback point saved: ${PREV_IMAGE}"
  fi

  # Bootstrap config.yaml (same guard as the CI workflow: config.yaml must be
  # a regular FILE; a directory at this path would shadow the container file).
  if [[ -e "$config_dir/config.yaml" ]] && [[ ! -f "$config_dir/config.yaml" ]]; then
    echo "FATAL: ${config_dir}/config.yaml exists but is NOT a regular file (likely a directory)." >&2
    echo "Remove it, then re-run this script to bootstrap from config.example.yaml." >&2
    exit 1
  fi
  if [[ ! -f "$config_dir/config.yaml" ]]; then
    echo "No config.yaml found; bootstrapping from config.example.yaml"
    echo "WARNING: example API keys will enable safe mode; edit ${config_dir}/config.yaml before use"
    docker run --rm \
      -v "$config_dir":/config \
      --entrypoint cp \
      "$IMAGE" \
      /CLIProxyAPI/config.example.yaml /config/config.yaml
  fi

  # Recreate the container. Persistent bind mounts - all survive recreation:
  #   config.yaml -> /CLIProxyAPI/config.yaml  (FILE, bootstrapped above)
  #   logs        -> /CLIProxyAPI/logs
  #   plugins     -> /CLIProxyAPI/plugins      (plugin binaries; were lost on every deploy)
  #   auths       -> /root/.cli-proxy-api      (auth records / OAuth tokens)
  #   static      -> /CLIProxyAPI/static       (management panel asset)
  #   pgstore     -> /CLIProxyAPI/pgstore      (Postgres store spool; only with PGSTORE_DSN)
  #   gitstore    -> /CLIProxyAPI/gitstore     (Git store local copy; only with GITSTORE_GIT_URL)
  #   objectstore -> /CLIProxyAPI/objectstore  (Object store spool; only with OBJECTSTORE_ENDPOINT)
  echo "--- Recreating container ${CONTAINER} ---"
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

  local run_args=(
    -d --network host --restart=always
    -v /etc/hosts:/etc/hosts
    -v "$config_dir/config.yaml":/CLIProxyAPI/config.yaml
    -v "$deploy_root/logs":/CLIProxyAPI/logs
    -v "$deploy_root/plugins":/CLIProxyAPI/plugins
    -v "$deploy_root/auths":/root/.cli-proxy-api
    -v "$deploy_root/static":/CLIProxyAPI/static
    -v "$deploy_root/pgstore":/CLIProxyAPI/pgstore
    -v "$deploy_root/gitstore":/CLIProxyAPI/gitstore
    -v "$deploy_root/objectstore":/CLIProxyAPI/objectstore
    -e APP_ENV=docker
    --name "$CONTAINER"
    "$IMAGE"
  )
  # Optional: a .env at the deploy root is mounted so PGSTORE_DSN / GITSTORE_GIT_URL
  # / OBJECTSTORE_ENDPOINT take effect (the program auto-loads .env from /CLIProxyAPI).
  if [[ -f "$deploy_root/.env" ]]; then
    run_args+=(-v "$deploy_root/.env":/CLIProxyAPI/.env)
  fi

  docker run "${run_args[@]}"

  # --- summary ---
  echo ""
  echo "=== Deploy complete ==="
  docker ps --filter "name=$CONTAINER" --format 'table {{.Names}}\t{{.Status}}'
  echo ""
  echo "Mount summary (host -> container):"
  echo "  ${config_dir}/config.yaml  -> /CLIProxyAPI/config.yaml"
  echo "  ${deploy_root}/logs        -> /CLIProxyAPI/logs"
  echo "  ${deploy_root}/plugins     -> /CLIProxyAPI/plugins"
  echo "  ${deploy_root}/auths       -> /root/.cli-proxy-api"
  echo "  ${deploy_root}/static      -> /CLIProxyAPI/static"
  echo "  ${deploy_root}/pgstore     -> /CLIProxyAPI/pgstore"
  echo "  ${deploy_root}/gitstore    -> /CLIProxyAPI/gitstore"
  echo "  ${deploy_root}/objectstore -> /CLIProxyAPI/objectstore"
  if [[ -f "$deploy_root/.env" ]]; then
    echo "  ${deploy_root}/.env        -> /CLIProxyAPI/.env"
  fi
  echo ""
  echo "Logs:   wsl -d ${WSL_DISTRO:-?} -- docker logs -f ${CONTAINER}"
  echo "Shell:  wsl -d ${WSL_DISTRO:-?} -- docker exec -it ${CONTAINER} sh"
  echo "Rollback: start the previous image manually with the same docker run"
  echo "          flags above but the image tag ${PREV_IMAGE}"
}

# --- entry: inside WSL -> deploy; on Windows -> bridge into WSL ---
if [[ $INSIDE -eq 1 ]] || [[ -n "${WSL_INTEROP:-}" ]]; then
  main
else
  bridge_to_wsl
fi
