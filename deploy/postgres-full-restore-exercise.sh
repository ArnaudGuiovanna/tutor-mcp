#!/usr/bin/env bash
set -Eeuo pipefail

: "${SOURCE_DATABASE_URL:?SOURCE_DATABASE_URL is required}"
: "${RESTORE_DATABASE_URL:?RESTORE_DATABASE_URL is required}"
: "${BACKUP_PATH:?BACKUP_PATH is required}"
: "${BACKUP_RECIPIENT_CERT:?BACKUP_RECIPIENT_CERT is required}"
: "${BACKUP_RECIPIENT_KEY:?BACKUP_RECIPIENT_KEY is required}"
: "${TENANT_ID:?TENANT_ID is required}"
: "${EXPECTED_CHECKSUM_PATH:?EXPECTED_CHECKSUM_PATH is required}"
: "${ALLOW_DESTRUCTIVE_RESTORE_EXERCISE:?set ALLOW_DESTRUCTIVE_RESTORE_EXERCISE=yes}"

if [[ "$ALLOW_DESTRUCTIVE_RESTORE_EXERCISE" != yes ]]; then
	echo "restore exercise approval flag must equal yes" >&2
	exit 2
fi
if [[ "$SOURCE_DATABASE_URL" == "$RESTORE_DATABASE_URL" ]]; then
	echo "source and isolated restore databases must differ" >&2
	exit 2
fi
for required_path in "$BACKUP_PATH" "$BACKUP_RECIPIENT_CERT" "$BACKUP_RECIPIENT_KEY" "$EXPECTED_CHECKSUM_PATH"; do
	case "$required_path" in
	/*) ;;
	*) echo "restore paths must be absolute" >&2; exit 2 ;;
	esac
done
if [[ -e "$EXPECTED_CHECKSUM_PATH" ]]; then
	echo "refusing to overwrite $EXPECTED_CHECKSUM_PATH" >&2
	exit 2
fi

umask 077
DATABASE_URL="$SOURCE_DATABASE_URL" go run ./cmd/tutor-tenant-verify \
	-mode snapshot -tenant "$TENANT_ID" >"$EXPECTED_CHECKSUM_PATH"
sha256sum -c "$BACKUP_PATH.sha256"

# RESTORE_DATABASE_URL must name a disposable, network-isolated database.
# Streaming also handles a newer pg_dump client emitting the optional
# transaction_timeout setting for an older server. The clear SQL never reaches
# disk and pipefail ensures that no decrypt/restore/DDL error is hidden.
decrypt_args=(cms -decrypt -binary -inform DER -in "$BACKUP_PATH" \
	-recip "$BACKUP_RECIPIENT_CERT" -inkey "$BACKUP_RECIPIENT_KEY")
if [[ -n "${BACKUP_PRIVATE_KEY_PASSIN:-}" ]]; then
	decrypt_args+=(-passin "env:BACKUP_PRIVATE_KEY_PASSIN")
fi
openssl "${decrypt_args[@]}" |
	pg_restore --clean --if-exists --no-owner --no-privileges --file - |
	sed '/^SET transaction_timeout =/d' |
	psql "$RESTORE_DATABASE_URL" -v ON_ERROR_STOP=1
DATABASE_URL="$RESTORE_DATABASE_URL" go run ./cmd/tutor-tenant-verify \
	-mode verify -tenant "$TENANT_ID" -expected "$EXPECTED_CHECKSUM_PATH" \
	-backup-id "$(basename "$BACKUP_PATH")"
