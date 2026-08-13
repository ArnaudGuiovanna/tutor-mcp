#!/usr/bin/env bash
set -Eeuo pipefail

: "${SOURCE_DATABASE_URL:?SOURCE_DATABASE_URL is required}"
: "${RESTORE_DATABASE_URL:?RESTORE_DATABASE_URL is required}"
: "${TENANT_ID:?TENANT_ID is required}"
: "${ARCHIVE_PATH:?ARCHIVE_PATH is required}"
: "${ARCHIVE_RECIPIENT_CERT:?ARCHIVE_RECIPIENT_CERT is required}"
: "${ARCHIVE_RECIPIENT_KEY:?ARCHIVE_RECIPIENT_KEY is required}"
: "${ALLOW_DESTRUCTIVE_RESTORE_EXERCISE:?set ALLOW_DESTRUCTIVE_RESTORE_EXERCISE=yes}"

if [[ "$ALLOW_DESTRUCTIVE_RESTORE_EXERCISE" != yes ]]; then
	echo "restore exercise approval flag must equal yes" >&2
	exit 2
fi
if [[ "$SOURCE_DATABASE_URL" == "$RESTORE_DATABASE_URL" ]]; then
	echo "source and isolated restore databases must differ" >&2
	exit 2
fi

if [[ ! -e "$ARCHIVE_PATH" ]]; then
	DATABASE_URL="$SOURCE_DATABASE_URL" TENANT_ID="$TENANT_ID" \
		ARCHIVE_PATH="$ARCHIVE_PATH" ARCHIVE_RECIPIENT_CERT="$ARCHIVE_RECIPIENT_CERT" \
		ARCHIVE_REASON="isolated logical restore exercise" \
		ARCHIVE_REQUEST_ID="logical-export-$(date -u +%Y%m%dT%H%M%SZ)" \
		./deploy/tenant-logical-backup.sh
fi
sha256sum -c "$ARCHIVE_PATH.sha256"

decrypt_args=(cms -decrypt -binary -inform DER -in "$ARCHIVE_PATH" \
	-recip "$ARCHIVE_RECIPIENT_CERT" -inkey "$ARCHIVE_RECIPIENT_KEY")
if [[ -n "${ARCHIVE_PRIVATE_KEY_PASSIN:-}" ]]; then
	decrypt_args+=(-passin "env:ARCHIVE_PRIVATE_KEY_PASSIN")
fi
# The JSON archive exists only in the pipe. The target must be a migrated,
# disposable database where the tenant ID is absent and the restore role is in use.
openssl "${decrypt_args[@]}" |
	ALLOW_TENANT_LOGICAL_RESTORE=yes DATABASE_URL="$RESTORE_DATABASE_URL" \
	go run ./cmd/tutor-tenant-archive -mode restore -archive - \
		-reason "isolated logical restore exercise" \
		-request-id "logical-restore-$(date -u +%Y%m%dT%H%M%SZ)"
