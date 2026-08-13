#!/usr/bin/env bash
set -Eeuo pipefail

: "${ALLOW_DESTRUCTIVE_PITR_EXERCISE:?set ALLOW_DESTRUCTIVE_PITR_EXERCISE=yes}"
: "${PITR_REPORT_PATH:?PITR_REPORT_PATH is required}"

if [[ "$ALLOW_DESTRUCTIVE_PITR_EXERCISE" != yes ]]; then
	echo "PITR exercise approval flag must equal yes" >&2
	exit 2
fi
case "$PITR_REPORT_PATH" in
	/*) ;;
	*) echo "PITR_REPORT_PATH must be absolute" >&2; exit 2 ;;
esac
if [[ -e "$PITR_REPORT_PATH" ]]; then
	echo "refusing to overwrite PITR report" >&2
	exit 2
fi

image=${PITR_DOCKER_IMAGE:-postgres:17}
suffix="$(date -u +%Y%m%d%H%M%S)-$$"
source_container="tutor-pitr-source-$suffix"
restore_container="tutor-pitr-restore-$suffix"
data_volume="tutor-pitr-data-$suffix"
archive_volume="tutor-pitr-archive-$suffix"
base_volume="tutor-pitr-base-$suffix"
restore_volume="tutor-pitr-restored-$suffix"
password=$(openssl rand -hex 24)

cleanup() {
	docker rm -f "$source_container" "$restore_container" >/dev/null 2>&1 || true
	docker volume rm "$data_volume" "$archive_volume" "$base_volume" "$restore_volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

for volume in "$data_volume" "$archive_volume" "$base_volume" "$restore_volume"; do
	docker volume create "$volume" >/dev/null
done

docker run --pull=never -d --name "$source_container" \
	-e POSTGRES_PASSWORD="$password" \
	-v "$data_volume:/var/lib/postgresql/data" \
	-v "$archive_volume:/archive" \
	-v "$base_volume:/base" \
	"$image" \
	-c wal_level=replica \
	-c archive_mode=on \
	-c "archive_command=test ! -f /archive/%f && cp %p /archive/%f" \
	-c archive_timeout=5s \
	-c max_wal_senders=4 >/dev/null

for _ in $(seq 1 90); do
	if docker exec "$source_container" pg_isready -U postgres >/dev/null 2>&1; then break; fi
	sleep 1
done
docker exec "$source_container" pg_isready -U postgres >/dev/null
docker exec -u root "$source_container" chown postgres:postgres /archive /base
docker exec "$source_container" psql -v ON_ERROR_STOP=1 -U postgres -c 'CREATE DATABASE pitr_exercise' >/dev/null
docker exec "$source_container" psql -v ON_ERROR_STOP=1 -U postgres -d pitr_exercise -c \
	"CREATE TABLE pitr_markers (id bigserial PRIMARY KEY, marker text NOT NULL UNIQUE); INSERT INTO pitr_markers(marker) VALUES ('base-backup');" >/dev/null

docker exec "$source_container" pg_basebackup -U postgres -D /base -Fp -Xs --checkpoint=fast >/dev/null

docker exec "$source_container" psql -v ON_ERROR_STOP=1 -U postgres -d pitr_exercise -c \
	"INSERT INTO pitr_markers(marker) VALUES ('before-target');" >/dev/null
target_lsn=$(docker exec "$source_container" psql -At -v ON_ERROR_STOP=1 -U postgres -d pitr_exercise -c 'SELECT pg_current_wal_lsn()')
if [[ ! "$target_lsn" =~ ^[0-9A-F]+/[0-9A-F]+$ ]]; then
	echo "invalid recovery target LSN" >&2
	exit 1
fi
target_wal=$(docker exec "$source_container" psql -At -v ON_ERROR_STOP=1 -U postgres -d pitr_exercise -c "SELECT pg_walfile_name('$target_lsn')")
archive_started_ns=$(date +%s%N)
docker exec "$source_container" psql -v ON_ERROR_STOP=1 -U postgres -d pitr_exercise -c \
	"INSERT INTO pitr_markers(marker) VALUES ('after-target'); SELECT pg_switch_wal();" >/dev/null

for _ in $(seq 1 90); do
	if docker exec "$source_container" test -f "/archive/$target_wal"; then break; fi
	sleep 1
done
docker exec "$source_container" test -f "/archive/$target_wal"
archive_finished_ns=$(date +%s%N)
archive_latency_ms=$(((archive_finished_ns - archive_started_ns) / 1000000))
docker stop -t 30 "$source_container" >/dev/null

docker run --pull=never --rm -u root \
	-v "$base_volume:/from:ro" -v "$restore_volume:/to" \
	-e TARGET_LSN="$target_lsn" "$image" sh -euc '
		cp -a /from/. /to/
		touch /to/recovery.signal
		printf "restore_command = '\''cp /archive/%%f %%p'\''\n" >> /to/postgresql.auto.conf
		printf "recovery_target_lsn = '\''%s'\''\n" "$TARGET_LSN" >> /to/postgresql.auto.conf
		printf "recovery_target_action = '\''promote'\''\n" >> /to/postgresql.auto.conf
		chown -R postgres:postgres /to
	' >/dev/null

restore_started_ns=$(date +%s%N)
docker run --pull=never -d --name "$restore_container" \
	-v "$restore_volume:/var/lib/postgresql/data" \
	-v "$archive_volume:/archive:ro" "$image" >/dev/null
for _ in $(seq 1 120); do
	if docker exec "$restore_container" pg_isready -U postgres >/dev/null 2>&1; then break; fi
	sleep 1
done
docker exec "$restore_container" pg_isready -U postgres >/dev/null
restore_finished_ns=$(date +%s%N)
rto_ms=$(((restore_finished_ns - restore_started_ns) / 1000000))

markers=$(docker exec "$restore_container" psql -At -v ON_ERROR_STOP=1 -U postgres -d pitr_exercise -c \
	"SELECT string_agg(marker, ',' ORDER BY id) FROM pitr_markers")
in_recovery=$(docker exec "$restore_container" psql -At -v ON_ERROR_STOP=1 -U postgres -d pitr_exercise -c \
	'SELECT pg_is_in_recovery()')
if [[ "$markers" != "base-backup,before-target" || "$in_recovery" != f ]]; then
	echo "PITR verification failed: markers=$markers in_recovery=$in_recovery" >&2
	exit 1
fi

umask 077
mkdir -p "$(dirname -- "$PITR_REPORT_PATH")"
report_tmp=$(mktemp "$(dirname -- "$PITR_REPORT_PATH")/.tutor-pitr-report.XXXXXX")
printf '{"postgres_image":"%s","target_lsn":"%s","archived_wal":"%s","archive_latency_ms":%d,"recovery_time_ms":%d,"restored_markers":["base-backup","before-target"],"excluded_marker":"after-target","promoted":true}\n' \
	"$image" "$target_lsn" "$target_wal" "$archive_latency_ms" "$rto_ms" >"$report_tmp"
chmod 600 "$report_tmp"
ln "$report_tmp" "$PITR_REPORT_PATH"
rm -f "$report_tmp"
printf 'PITR verified: report=%s archive_latency_ms=%d recovery_time_ms=%d\n' \
	"$PITR_REPORT_PATH" "$archive_latency_ms" "$rto_ms"
