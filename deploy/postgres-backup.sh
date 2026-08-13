#!/usr/bin/env bash
set -Eeuo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
: "${BACKUP_PATH:?BACKUP_PATH is required}"
: "${BACKUP_RECIPIENT_CERT:?BACKUP_RECIPIENT_CERT is required}"
: "${KEYRING_MANIFEST_PATH:?KEYRING_MANIFEST_PATH is required}"

for required_path in "$BACKUP_PATH" "$BACKUP_RECIPIENT_CERT" "$KEYRING_MANIFEST_PATH"; do
	case "$required_path" in
	/*) ;;
	*) echo "backup paths must be absolute" >&2; exit 2 ;;
	esac
done
for output_path in "$BACKUP_PATH" "$BACKUP_PATH.sha256" "$BACKUP_PATH.keys.json"; do
	if [[ -e "$output_path" ]]; then
		echo "refusing to overwrite $output_path" >&2
		exit 2
	fi
done

openssl x509 -in "$BACKUP_RECIPIENT_CERT" -noout >/dev/null
if [[ ! -r "$KEYRING_MANIFEST_PATH" ]]; then
	echo "KEYRING_MANIFEST_PATH must be readable" >&2
	exit 2
fi

umask 077
backup_dir=$(dirname -- "$BACKUP_PATH")
mkdir -p "$backup_dir"
encrypted_tmp=$(mktemp "$backup_dir/.tutor-postgres-backup.XXXXXX")
checksum_tmp=$(mktemp "$backup_dir/.tutor-postgres-checksum.XXXXXX")
manifest_tmp=$(mktemp "$backup_dir/.tutor-postgres-keys.XXXXXX")
published=0
cleanup() {
	rm -f -- "$encrypted_tmp" "$checksum_tmp" "$manifest_tmp"
	if [[ "$published" != 1 ]]; then
		rm -f -- "$BACKUP_PATH" "$BACKUP_PATH.sha256" "$BACKUP_PATH.keys.json"
	fi
}
trap cleanup EXIT HUP INT TERM

# The custom-format dump is streamed directly into a CMS envelope: no
# plaintext database dump reaches disk. The X.509 private key remains outside
# the backup location and may itself be held by KMS/HSM-backed operations.
pg_dump --format=custom --compress=9 --no-owner --no-privileges "$DATABASE_URL" |
	openssl cms -encrypt -binary -stream -aes-256-gcm -outform DER \
		-out "$encrypted_tmp" "$BACKUP_RECIPIENT_CERT"
openssl cms -cmsout -inform DER -in "$encrypted_tmp" -noout
chmod 600 "$encrypted_tmp"

# Hard links publish all outputs without silently replacing an existing file.
ln "$encrypted_tmp" "$BACKUP_PATH"
sha256sum "$BACKUP_PATH" >"$checksum_tmp"
cp "$KEYRING_MANIFEST_PATH" "$manifest_tmp"
chmod 600 "$checksum_tmp" "$manifest_tmp"
ln "$checksum_tmp" "$BACKUP_PATH.sha256"
ln "$manifest_tmp" "$BACKUP_PATH.keys.json"
published=1

printf '{"backup":"%s","encryption":"CMS AES-256-GCM","sha256_file":"%s"}\n' \
	"$BACKUP_PATH" "$BACKUP_PATH.sha256"
