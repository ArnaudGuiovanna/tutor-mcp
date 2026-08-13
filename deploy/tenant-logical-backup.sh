#!/usr/bin/env bash
set -Eeuo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
: "${TENANT_ID:?TENANT_ID is required}"
: "${ARCHIVE_PATH:?ARCHIVE_PATH is required}"
: "${ARCHIVE_RECIPIENT_CERT:?ARCHIVE_RECIPIENT_CERT is required}"
: "${ARCHIVE_REASON:?ARCHIVE_REASON is required}"
: "${ARCHIVE_REQUEST_ID:?ARCHIVE_REQUEST_ID is required}"

case "$ARCHIVE_PATH" in
	/*) ;;
	*) echo "ARCHIVE_PATH must be absolute" >&2; exit 2 ;;
esac
if [[ -e "$ARCHIVE_PATH" || -e "$ARCHIVE_PATH.sha256" ]]; then
	echo "refusing to overwrite tenant archive" >&2
	exit 2
fi
openssl x509 -in "$ARCHIVE_RECIPIENT_CERT" -noout >/dev/null

umask 077
archive_dir=$(dirname -- "$ARCHIVE_PATH")
mkdir -p "$archive_dir"
archive_tmp=$(mktemp "$archive_dir/.tutor-tenant-archive.XXXXXX")
checksum_tmp=$(mktemp "$archive_dir/.tutor-tenant-checksum.XXXXXX")
published=0
cleanup() {
	rm -f -- "$archive_tmp" "$checksum_tmp"
	if [[ "$published" != 1 ]]; then rm -f -- "$ARCHIVE_PATH" "$ARCHIVE_PATH.sha256"; fi
}
trap cleanup EXIT HUP INT TERM

go run ./cmd/tutor-tenant-archive -mode export -tenant "$TENANT_ID" -archive - \
	-reason "$ARCHIVE_REASON" -request-id "$ARCHIVE_REQUEST_ID" |
	openssl cms -encrypt -binary -stream -aes-256-gcm -outform DER \
		-out "$archive_tmp" "$ARCHIVE_RECIPIENT_CERT"
openssl cms -cmsout -inform DER -in "$archive_tmp" -noout
chmod 600 "$archive_tmp"
ln "$archive_tmp" "$ARCHIVE_PATH"
sha256sum "$ARCHIVE_PATH" >"$checksum_tmp"
chmod 600 "$checksum_tmp"
ln "$checksum_tmp" "$ARCHIVE_PATH.sha256"
published=1
printf '{"tenant_id":"%s","archive":"%s","encryption":"CMS AES-256-GCM"}\n' "$TENANT_ID" "$ARCHIVE_PATH"
