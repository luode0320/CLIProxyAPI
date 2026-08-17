#!/usr/bin/env bash
#
# backup.sh - Backup/restore CLIProxyAPI persistent data.
#
# Persistent data lives on the host via compose bind mounts. Backing it up is a
# plain archive of the host directories/files; restoring unpacks back into the
# project root (or the directory the CLI_PROXY_*_PATH variables point at).
#
# Usage:
#   ./backup.sh [--cluster] [--output <dir>]          # create a backup archive
#   ./backup.sh --restore <backup-file> [--cluster]   # restore from an archive
#
# Backup contents (standalone): config.yaml, plugins/, auths/, logs/, static/, pgstore/, gitstore/, objectstore/
# Backup contents (cluster):    home/, logs/, plugins/, static/, pgstore/, gitstore/, objectstore/
#
# Tip: schedule with cron/systemd timer, e.g. daily:
#   0 3 * * * cd /opt/cli-proxy-api && ./backup.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

BACKUP_DIR="${CLI_PROXY_BACKUP_DIR:-backups}"
CLUSTER=0
ACTION="backup"
RESTORE_FILE=""

usage() {
  echo "Usage:"
  echo "  ./backup.sh [--cluster] [--output <dir>]         # create a backup"
  echo "  ./backup.sh --restore <backup-file> [--cluster]  # restore from a backup"
  echo ""
  echo "Backup contents (standalone): config.yaml, plugins/, auths/, logs/"
  echo "Backup contents (cluster):    home/, logs/, plugins/"
  echo ""
  echo "Options:"
  echo "  --cluster       include cluster-mode paths (home/ instead of config+auths)"
  echo "  --output <dir>  backup directory (default: backups/, or \$CLI_PROXY_BACKUP_DIR)"
  echo "  --restore <f>   restore mode: unpack <f> into the project root"
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster) CLUSTER=1 ;;
    --output) BACKUP_DIR="${2:-}"; shift ;;
    --restore) ACTION="restore"; RESTORE_FILE="${2:-}"; shift ;;
    -h|--help) usage ;;
    *) echo "Error: unknown option: $1" >&2; usage ;;
  esac
  shift
done

if [[ "$ACTION" == "restore" ]]; then
  if [[ -z "$RESTORE_FILE" || ! -f "$RESTORE_FILE" ]]; then
    echo "Error: backup file not found: ${RESTORE_FILE:-<empty>}" >&2
    exit 1
  fi
  echo "Restoring from $RESTORE_FILE ..."
  tar -xzf "$RESTORE_FILE"
  echo "Restore done. Run ./deploy-check.sh before starting the container."
  exit 0
fi

mkdir -p "$BACKUP_DIR"

if [[ $CLUSTER -eq 1 ]]; then
  ARGS=(home logs plugins static pgstore gitstore objectstore)
else
  ARGS=(config.yaml plugins auths logs static pgstore gitstore objectstore)
fi

INCLUDE=()
for p in "${ARGS[@]}"; do
  if [[ -e "$p" ]]; then
    INCLUDE+=("$p")
  else
    echo "Note: skipping '${p}' (not present in project root)."
  fi
done

if [[ ${#INCLUDE[@]} -eq 0 ]]; then
  echo "Error: nothing to back up — none of (${ARGS[*]}) exist in $(pwd)." >&2
  exit 1
fi

STAMP="$(date +%Y%m%d-%H%M%S)"
OUT="${BACKUP_DIR}/cliproxy-backup-${STAMP}.tar.gz"

echo "Creating backup: $OUT"
tar -czf "$OUT" "${INCLUDE[@]}"
echo ""
echo "Backup complete."
echo "  Archive: $OUT"
echo "  Contents: ${INCLUDE[*]}"
echo "  To restore later: ./backup.sh --restore $OUT"
