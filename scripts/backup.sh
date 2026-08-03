#!/usr/bin/env bash
# Online backup of the tutor-mcp SQLite database. Safe to run while the
# server is writing — uses the SQLite "VACUUM INTO" / ".backup" path,
# which acquires the appropriate locks transparently.
#
# Usage:   scripts/backup.sh [<backup_dir>]
# Default: ${BACKUP_DIR:-/home/ubuntu/backups/tutor-mcp}
#
# Retention: deletes daily backups older than ${BACKUP_RETENTION_DAYS:-14}.
#
# Exit codes:
#   0  backup written and pruning succeeded
#   1  source database missing or unreadable
#   2  backup write failed
#   3  pruning failed (backup itself succeeded)

set -euo pipefail
umask 077

DB_PATH="${DB_PATH:-/home/ubuntu/mcp/data/runtime.db}"
BACKUP_DIR="${1:-${BACKUP_DIR:-/home/ubuntu/backups/tutor-mcp}}"
RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"

if [ ! -r "$DB_PATH" ]; then
  echo "backup.sh: source database not readable at $DB_PATH" >&2
  exit 1
fi

mkdir -p "$BACKUP_DIR"
chmod 0700 "$BACKUP_DIR"

stamp="$(date -u +%Y-%m-%dT%H-%M-%SZ)"
target="$BACKUP_DIR/runtime-$stamp.db"
tmp="$target.partial"

# Use SQLite's online backup. Writing to .partial first means a crash
# mid-backup leaves no half-baked file at the canonical name.
if ! sqlite3 "$DB_PATH" ".backup '$tmp'"; then
  rm -f "$tmp"
  echo "backup.sh: sqlite3 .backup failed" >&2
  exit 2
fi
mv "$tmp" "$target"
chmod 0600 "$target"
echo "backup.sh: wrote $target ($(stat -c%s "$target") bytes)"

# Prune old daily backups. -mtime +N means modified more than N*24h ago.
if ! find "$BACKUP_DIR" -maxdepth 1 -type f -name 'runtime-*.db' -mtime "+${RETENTION_DAYS}" -print -delete; then
  echo "backup.sh: pruning failed (kept new backup)" >&2
  exit 3
fi
