#!/usr/bin/env bash
# Disposable real Rust client -> HTTPS -> Go institution acceptance.
# PostgreSQL verify-full TLS, separate least-privilege API/worker processes,
# operator bootstrap by public key, encrypted backup, recovery, authoring,
# tutoring and queued worker erasure. No mailbox or password service.
# Requirements: Go, Rust 1.93, openssl, python3 and PostgreSQL server binaries.
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
api_pid="" worker_pid="" proxy_pid=""
cleanup() {
    [[ -n "$proxy_pid" ]] && kill "$proxy_pid" 2>/dev/null || true
    [[ -n "$api_pid" ]] && kill "$api_pid" 2>/dev/null || true
    [[ -n "$worker_pid" ]] && kill "$worker_pid" 2>/dev/null || true
    as_pg "$pg_bin/pg_ctl" -D "$work/pg" -m fast stop >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

port="${SMOKE_PG_PORT:-55499}"
api_port="${SMOKE_API_PORT:-3099}"
public_port="${SMOKE_HTTPS_PORT:-3100}"
go_bin="${TUTOR_GO_BIN:-go}"

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
"$go_bin" build -o "$work/tutor-mcp" .
"$go_bin" build -o "$work/tutor-control-plane" ./cmd/tutor-control-plane
owner_dsn="postgres://tutor_owner:owner-pw@localhost:$port/tutor?$tls"
PROCESS_ROLE=migrator DATABASE_URL="$owner_dsn" "$work/tutor-mcp" --profile institution >"$work/migrator.log" 2>&1
owner -f deploy/postgres-roles.sql
super tutor -f deploy/postgres-roles-superuser.sql
super tutor -c "CREATE ROLE api_login LOGIN PASSWORD 'api-pw' IN ROLE tutor_api;"
super tutor -c "CREATE ROLE worker_login LOGIN PASSWORD 'worker-pw' IN ROLE tutor_worker;"

# Test-only request bridge: there is deliberately no public erasure endpoint.
# This helper uses the API login and the real, live owner principal to enqueue
# an authorized request. Only the separately running production worker may
# process its batches. No production CLI/API surface is added for the smoke.
cat > "$work/dsar-helper.go" <<'EOF_DSAR'
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "institution erasure smoke:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 4 {
		return errors.New("expected institution, learner and owner public key identifiers")
	}
	tenant, learner := os.Args[1], os.Args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	pool, err := db.OpenPostgres(os.Getenv("SMOKE_DSAR_DATABASE_URL"), 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := db.NewStoreWithDialect(pool, db.DialectPostgres)
	identity, err := store.GetNativeIdentity(ctx, tenant, os.Args[3], "session")
	if err != nil {
		return err
	}
	actor, err := store.GetPrincipal(ctx, models.TenantScope{TenantID: tenant, UserID: identity.UserID,
		MembershipID: identity.MembershipID}, []string{models.OAuthScopeFormationRead})
	if err != nil {
		return err
	}
	if err := store.ValidatePrincipal(ctx, actor); err != nil {
		return err
	}
	if !actor.Authorize(models.PermissionTenantManage, models.AuthorizationResource{TenantID: tenant}) {
		return errors.New("owner lacks live tenant-management permission")
	}
	// Inspection uses the same API login and transaction-local tenant binding;
	// it never changes data or borrows the migrator's privileges.
	read := func(fn func(*sql.Tx) error) error {
		tx, err := pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for setting, value := range map[string]string{"app.current_tenant": tenant,
			"app.current_user": actor.UserID, "app.current_membership": actor.MembershipID} {
			if _, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, setting, value); err != nil {
				return err
			}
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	seats := func(tx *sql.Tx) (map[string]int, error) {
		rows, err := tx.QueryContext(ctx, `SELECT c.id, c.reserved_seats - SUM(e.seat_reserved)
		 FROM cohorts c JOIN enrollments e ON e.tenant_id = c.tenant_id AND e.cohort_id = c.id
		 WHERE e.tenant_id = $1 AND e.learner_id = $2 GROUP BY c.id, c.reserved_seats`, tenant, learner)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var id string
			var count int
			if err := rows.Scan(&id, &count); err != nil {
				return nil, err
			}
			out[id] = count
		}
		return out, rows.Err()
	}
	var expectedSeats map[string]int
	if err := read(func(tx *sql.Tx) error {
		var err error
		expectedSeats, err = seats(tx)
		if err != nil {
			return err
		}
		for _, table := range []string{"interactions", "learner_concept_states", "native_sessions"} {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = $1 AND learner_id = $2`, tenant, learner).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("erasure fixture has no %s", table)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	request, err := store.RequestTenantDSAR(ctx, actor, learner, "erase", "disposable institution smoke")
	if err != nil {
		return err
	}
	emit := func(event string) error {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"event": event, "request_id": request.ID})
	}
	if err := emit("queued"); err != nil {
		return err
	}
	revoked := false
	for {
		var requestStatus, jobStatus, revokeStatus string
		if err := read(func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT r.status, j.status, COALESCE(p.status, '')
			 FROM tenant_dsar_requests r JOIN async_jobs j ON j.tenant_id = r.tenant_id
			  AND j.kind = 'tenant_dsar' AND j.idempotency_key = r.id
			 LEFT JOIN tenant_dsar_phases p ON p.tenant_id = r.tenant_id AND p.request_id = r.id AND p.phase = 'revoke_access'
			 WHERE r.tenant_id = $1 AND r.id = $2`, tenant, request.ID).Scan(&requestStatus, &jobStatus, &revokeStatus)
		}); err != nil {
			return err
		}
		if requestStatus == "failed" || requestStatus == "blocked" || jobStatus == "dead_letter" {
			return fmt.Errorf("worker request=%s job=%s", requestStatus, jobStatus)
		}
		if revokeStatus == "completed" && !revoked {
			if err := emit("revoked"); err != nil {
				return err
			}
			revoked = true
		}
		if requestStatus == "completed" && jobStatus == "completed" {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("production worker did not complete erasure before deadline")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if !revoked {
		return errors.New("worker completed without access-revocation checkpoint")
	}
	if err := read(func(tx *sql.Tx) error {
		for _, table := range []string{"native_challenges", "native_sessions", "refresh_tokens", "oauth_codes", "login_challenges", "account_tokens",
			"learner_approved_clients", "tool_call_idempotency", "concept_bkt_estimates", "collective_weight_applications",
			"learning_badges", "learning_badge_evidence", "webhook_delivery_transitions", "webhook_push_log",
			"webhook_message_queue", "narrative_mutations", "narrative_objects", "pedagogical_snapshots", "transfer_records",
			"interactions", "learning_events", "assessment_adjudications", "assessment_reviews", "curriculum_review_opinions",
			"assessment_attempts", "pedagogical_decisions", "affect_states", "implementation_intentions", "learning_sessions",
			"concept_states", "learner_concept_states", "calibration_records", "pending_consolidations", "scheduled_alerts", "availability"} {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = $1 AND learner_id = $2`, tenant, learner).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("worker left %d rows in %s", count, table)
			}
		}
		actual, err := seats(tx)
		if err != nil {
			return err
		}
		for cohort, expected := range expectedSeats {
			if actual[cohort] != expected {
				return fmt.Errorf("cohort reservation mismatch: expected %d, got %d", expected, actual[cohort])
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return emit("completed")
}
EOF_DSAR
"$go_bin" build -o "$work/dsar-helper" "$work/dsar-helper.go"

# ── Real client identities, generated only by the Rust client ────────────────
if [[ -n "${SMOKE_NATIVE_CLIENT_BIN:-}" ]]; then
    native_client="$SMOKE_NATIVE_CLIENT_BIN"
else
    cargo build --manifest-path client/src-tauri/Cargo.toml --locked \
        --no-default-features --features qualification --bin tutor-client-native-qualify
    native_client="$repo/client/src-tauri/target/debug/tutor-client-native-qualify"
fi
"$native_client" prepare "$work/client"
public_key() {
    python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]][sys.argv[3]])' \
        "$work/client/public-keys.json" "$1" "$2"
}

# Server signing/encryption keys are independent of the client identities.
mkdir -p "$work/keygen"
cat > "$work/keygen/main.go" <<'EOF_KEYS'
package main
import (
    "crypto/ed25519"
    "crypto/rand"
    "encoding/base64"
    "encoding/json"
    "fmt"
)
func main() {
    pub, priv, err := ed25519.GenerateKey(rand.Reader)
    if err != nil { panic(err) }
    keys, err := json.Marshal([]map[string]any{{"kid":"smoke-1","public_key":base64.StdEncoding.EncodeToString(pub),"private_key":base64.StdEncoding.EncodeToString(priv.Seed()),"active":true}})
    if err != nil { panic(err) }
    secret := make([]byte,32)
    if _, err := rand.Read(secret); err != nil { panic(err) }
    fmt.Printf("export JWT_ED25519_KEYS='%s'\n",keys)
    fmt.Printf("export INTEGRATION_SECRET_KEYS=primary:%s\n",base64.StdEncoding.EncodeToString(secret))
    fmt.Println("export INTEGRATION_SECRET_CURRENT_KEY_ID=primary")
}
EOF_KEYS
"$go_bin" run "$work/keygen/main.go" > "$work/keys.env"
chmod 600 "$work/keys.env"
# shellcheck disable=SC1091
source "$work/keys.env"
export RATELIMIT_BACKEND=postgres SCHEDULER_MODE=distributed \
    TENANT_INTEGRATION_ALLOWED_HOSTS=discord.com TUTOR_MCP_MEMORY_ROOT="$work/memory"

DATABASE_URL="$owner_dsn" "$work/tutor-control-plane" -action=plan-upsert -plan=smoke -name=Smoke -status=active \
    -entitlements='{"active_learners":50,"mcp_calls_month":100000}' -reason=smoke -request-id=S1 >/dev/null
export DATABASE_URL="$owner_dsn"
tenant="$("$work/tutor-control-plane" -action=provision -slug=acme -name='Acme Academy' -region=local \
    -plan=smoke -reason=smoke -request-id=S2 | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["ID"])')"
"$work/tutor-control-plane" -action=bootstrap-owner -tenant="$tenant" -name='Test owner' \
    -owner-npub="$(public_key owner npub)" -recovery-npub="$(public_key owner recoveryNpub)" \
    -reason=smoke -request-id=S3 >/dev/null
"$work/tutor-control-plane" -action=add-key-member -tenant="$tenant" -name='Test learner' \
    -npub="$(public_key learner npub)" -recovery-npub="$(public_key learner recoveryNpub)" \
    -roles=learner -reason=smoke -request-id=S4 >/dev/null
unset DATABASE_URL

PROCESS_ROLE=worker DATABASE_URL="postgres://worker_login:worker-pw@localhost:$port/tutor?$tls" \
    "$work/tutor-mcp" --profile institution >"$work/worker.log" 2>&1 &
worker_pid=$!
api_dsn="postgres://api_login:api-pw@localhost:$port/tutor?$tls"
PROCESS_ROLE=api DATABASE_URL="$api_dsn" \
    BASE_URL="https://localhost:$public_port" PORT="$api_port" INSTITUTION_ID="$tenant" INSTITUTION_NAME='Acme Academy' \
    TRUSTED_PROXY_CIDRS=127.0.0.1/32 \
    "$work/tutor-mcp" --profile institution >"$work/api.log" 2>&1 &
api_pid=$!
python3 scripts/native-test-proxy.py --port "$public_port" --upstream "$api_port" \
    --certificate "$work/tls/server.crt" --key "$work/tls/server.key" >"$work/proxy.log" 2>&1 &
proxy_pid=$!
fail() {
    echo "FAIL: $1" >&2
    tail -n 20 -- "$work/api.log" "$work/worker.log" "$work/proxy.log" >&2
    exit 1
}
for _ in $(seq 1 60); do
    curl -sf --noproxy '*' --cacert "$work/tls/ca.pem" "https://localhost:$public_port/ready" >/dev/null && break
    kill -0 "$api_pid" 2>/dev/null || fail 'API exited during startup'
    sleep 1
done
curl -sf --noproxy '*' --cacert "$work/tls/ca.pem" "https://localhost:$public_port/ready" >/dev/null || fail 'API never became ready'
kill -0 "$worker_pid" 2>/dev/null || fail 'worker exited with the documented grants'
echo 'PASS: TLS PostgreSQL, migrator, documented grants, separate API and worker; no SMTP'
SSL_CERT_FILE="$work/tls/ca.pem" SMOKE_DSAR_HELPER="$work/dsar-helper" SMOKE_DSAR_DATABASE_URL="$api_dsn" \
    "$native_client" verify "$work/client" "https://localhost:$public_port" || fail 'native institution journey'
kill -0 "$worker_pid" 2>/dev/null || fail 'worker exited during qualification'
