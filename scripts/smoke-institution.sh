#!/usr/bin/env bash
# Disposable end-to-end acceptance of the institution profile with real
# production constraints: PostgreSQL over verify-full TLS, separate migrator,
# API and worker processes, least-privilege runtime logins applied with the
# repository's own grant scripts, a provisioned tenant, DCR in token mode, and
# the OAuth + MCP + admin catalog journey driven over HTTP.
#
# Requirements: Go, openssl, python3 and PostgreSQL server binaries
# (initdb/pg_ctl). Nothing outside a temporary directory is modified.
set -euo pipefail
cd "$(dirname "$0")/.."
repo="$(pwd)"

pg_bin="${PG_BIN:-}"
if [[ -z "$pg_bin" ]]; then
    if command -v pg_config >/dev/null 2>&1 && [[ -x "$(pg_config --bindir)/initdb" ]]; then
        pg_bin="$(pg_config --bindir)"
    else
        pg_bin="$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1 || true)"
    fi
fi
if [[ -z "$pg_bin" || ! -x "$pg_bin/initdb" ]]; then
    echo "PostgreSQL server binaries not found; set PG_BIN" >&2
    exit 2
fi

work="$(mktemp -d /var/tmp/tutor-institution-smoke.XXXXXX)"
chmod 755 "$work"
pg_user="$(id -un)"
as_pg() { "$@"; }
if [[ "$(id -u)" -eq 0 ]]; then
    # PostgreSQL refuses to run as root.
    pg_user=postgres
    as_pg() { runuser -u postgres -- "$@"; }
fi
api_pid="" worker_pid=""
cleanup() {
    [[ -n "$api_pid" ]] && kill "$api_pid" 2>/dev/null || true
    [[ -n "$worker_pid" ]] && kill "$worker_pid" 2>/dev/null || true
    as_pg "$pg_bin/pg_ctl" -D "$work/pg" -m fast stop >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

port="${SMOKE_PG_PORT:-55499}"
api_port="${SMOKE_API_PORT:-3099}"

# ── TLS for verify-full ─────────────────────────────────────────────────────
mkdir -p "$work/tls"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=smoke-ca" \
    -keyout "$work/tls/ca.key" -out "$work/tls/ca.pem" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=localhost" \
    -keyout "$work/tls/server.key" -out "$work/tls/server.csr" 2>/dev/null
printf "subjectAltName=DNS:localhost,IP:127.0.0.1\n" > "$work/tls/san.ext"
openssl x509 -req -in "$work/tls/server.csr" -CA "$work/tls/ca.pem" -CAkey "$work/tls/ca.key" \
    -CAcreateserial -days 1 -extfile "$work/tls/san.ext" -out "$work/tls/server.crt" 2>/dev/null
chmod 644 "$work/tls/ca.pem" "$work/tls/server.crt"
chmod 600 "$work/tls/server.key"
[[ "$pg_user" != "$(id -un)" ]] && chown -R "$pg_user" "$work/tls"

# ── PostgreSQL cluster ──────────────────────────────────────────────────────
mkdir -p "$work/pg" "$work/run" "$work/pglog"
[[ "$pg_user" != "$(id -un)" ]] && chown "$pg_user" "$work/pg" "$work/run" "$work/pglog"
echo "smoke-superuser" > "$work/pgpw"
chmod 644 "$work/pgpw"
as_pg "$pg_bin/initdb" -D "$work/pg" -A scram-sha-256 --pwfile="$work/pgpw" -U postgres >/dev/null
cat >> "$work/pg/postgresql.conf" <<EOF
port = $port
listen_addresses = '127.0.0.1'
unix_socket_directories = '$work/run'
ssl = on
ssl_cert_file = '$work/tls/server.crt'
ssl_key_file = '$work/tls/server.key'
EOF
as_pg "$pg_bin/pg_ctl" -D "$work/pg" -l "$work/pglog/pg.log" -w start >/dev/null

tls="sslmode=verify-full&sslrootcert=$work/tls/ca.pem"
super() { PGPASSWORD=smoke-superuser psql "host=localhost port=$port user=postgres dbname=${1} sslmode=verify-full sslrootcert=$work/tls/ca.pem" -q -v ON_ERROR_STOP=1 "${@:2}"; }
owner() { PGPASSWORD=owner-pw psql "host=localhost port=$port user=tutor_owner dbname=tutor sslmode=verify-full sslrootcert=$work/tls/ca.pem" -q -v ON_ERROR_STOP=1 "$@"; }

super postgres -c "CREATE ROLE tutor_owner LOGIN PASSWORD 'owner-pw';"
super postgres -c "CREATE DATABASE tutor OWNER tutor_owner;"
super tutor -c "ALTER SCHEMA public OWNER TO tutor_owner;"
# Documented step 1: the owner creates the group roles the first time.
super postgres -c "ALTER ROLE tutor_owner CREATEROLE;"

# ── Build, migrate, grant ───────────────────────────────────────────────────
go build -o "$work/tutor-mcp" .
go build -o "$work/tutor-control-plane" ./cmd/tutor-control-plane
owner_dsn="postgres://tutor_owner:owner-pw@localhost:$port/tutor?$tls"
PROCESS_ROLE=migrator DATABASE_URL="$owner_dsn" "$work/tutor-mcp" --profile institution >"$work/migrator.log" 2>&1
owner -f deploy/postgres-roles.sql
super tutor -f deploy/postgres-roles-superuser.sql
super tutor -c "CREATE ROLE api_login LOGIN PASSWORD 'api-pw' IN ROLE tutor_api;"
super tutor -c "CREATE ROLE worker_login LOGIN PASSWORD 'worker-pw' IN ROLE tutor_worker;"

# ── Secrets ─────────────────────────────────────────────────────────────────
mkdir -p "$work/keygen"
cat > "$work/keygen/main.go" <<'EOF'
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

func main() {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys, _ := json.Marshal([]map[string]any{{"kid": "smoke-1", "public_key": base64.StdEncoding.EncodeToString(pub), "private_key": base64.StdEncoding.EncodeToString(priv.Seed()), "active": true}})
	secret, iat := make([]byte, 32), make([]byte, 32)
	rand.Read(secret)
	rand.Read(iat)
	fmt.Printf("export JWT_ED25519_KEYS='%s'\n", keys)
	fmt.Printf("export INTEGRATION_SECRET_KEYS=primary:%s\n", base64.StdEncoding.EncodeToString(secret))
	fmt.Println("export INTEGRATION_SECRET_CURRENT_KEY_ID=primary")
	fmt.Printf("export OAUTH_DCR_INITIAL_ACCESS_TOKEN=%s\n", base64.RawURLEncoding.EncodeToString(iat))
}
EOF
(cd "$work/keygen" && printf 'module keygen\n\ngo 1.26\n' > go.mod && go run . > "$work/keys.env")
# shellcheck disable=SC1091
source "$work/keys.env"
export RATELIMIT_BACKEND=postgres SCHEDULER_MODE=distributed \
    TENANT_INTEGRATION_ALLOWED_HOSTS=discord.com TUTOR_MCP_MEMORY_ROOT="$work/memory"

# ── Worker then API with least-privilege logins ─────────────────────────────
PROCESS_ROLE=worker DATABASE_URL="postgres://worker_login:worker-pw@localhost:$port/tutor?$tls" \
    "$work/tutor-mcp" --profile institution >"$work/worker.log" 2>&1 &
worker_pid=$!
PROCESS_ROLE=api DATABASE_URL="postgres://api_login:api-pw@localhost:$port/tutor?$tls" \
    BASE_URL=https://tutor.localhost PORT="$api_port" OAUTH_DCR_MODE=token \
    SMTP_ADDR=127.0.0.1:2525 SMTP_FROM=tutor@tutor.localhost TRUSTED_PROXY_CIDRS=127.0.0.1/32 \
    "$work/tutor-mcp" --profile institution >"$work/api.log" 2>&1 &
api_pid=$!
for _ in $(seq 1 60); do
    curl -sf --noproxy '*' "http://127.0.0.1:$api_port/ready" >/dev/null && break
    sleep 1
done
fail() {
    echo "FAIL: $1" >&2
    echo "--- api.log"; tail -20 "$work/api.log" >&2
    echo "--- worker.log"; tail -20 "$work/worker.log" >&2
    exit 1
}
curl -sf --noproxy '*' "http://127.0.0.1:$api_port/ready" >/dev/null || fail "API never became ready"
sleep 3
kill -0 "$worker_pid" 2>/dev/null || fail "worker exited during startup with the documented grants"
echo "PASS: migrator, documented grants, least-privilege API and worker"

# ── Tenant provisioning and accounts ────────────────────────────────────────
export DATABASE_URL="$owner_dsn"
"$work/tutor-control-plane" -action=plan-upsert -plan=smoke -name=Smoke -status=active \
    -entitlements='{"active_learners":50,"mcp_calls_month":100000}' -reason=smoke -request-id=S1 >/dev/null
tenant="$("$work/tutor-control-plane" -action=provision -slug=acme -name='Acme Academy' -region=local \
    -plan=smoke -reason=smoke -request-id=S2 | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["ID"])')"
unset DATABASE_URL
super postgres -c "CREATE EXTENSION IF NOT EXISTS pgcrypto;"
member_secret="$(openssl rand -hex 16)"
hash="$(PGPASSWORD=smoke-superuser psql "host=localhost port=$port user=postgres dbname=postgres sslmode=verify-full sslrootcert=$work/tls/ca.pem" \
    -Atc "select crypt('$member_secret', gen_salt('bf', 10))")"
# Until invitations exist, accounts are seeded through the identity triggers.
super tutor <<SQL
INSERT INTO learners (id,email,password_hash,objective,profile_json,created_at,email_verified_at,tenant_id,user_id,membership_id) VALUES
 ('lrn_manager','manager@acme.test','$hash','','{}',now(),now(),'$tenant','usr_manager','mem_manager'),
 ('lrn_alice','alice@acme.test','$hash','','{}',now(),now(),'$tenant','usr_alice','mem_alice');
UPDATE tenant_memberships SET roles_json='["pedagogy_manager","learner"]'::jsonb, version=version+1 WHERE id='mem_manager';
SQL

SMOKE_BASE_URL=https://tutor.localhost SMOKE_CONNECT="127.0.0.1:$api_port" SMOKE_TENANT="$tenant" \
    SMOKE_PASSWORD="$member_secret" python3 scripts/smoke-institution.py || fail "institution journey"
