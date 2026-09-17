#!/bin/sh
# Disposable Compose acceptance; never uses the normal deployment project name.
set -eu
cd "$(dirname "$0")/.."
export TUTOR_DOMAIN=tutor.localhost
project="tutor-acceptance-$$"
umask 077
work="$(mktemp -d)"
compose() {
    docker compose -p "$project" -f deploy/compose.hobby.yml -f deploy/compose.smoke.yml "$@"
}
cleanup() {
    compose down --volumes >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM
compose build
compose run --rm --no-deps tutor init --profile hobby --data-dir /data/hobby \
    --public-url https://tutor.localhost > "$work/invitation"
compose up -d
address="$(compose port caddy 443)"
ready() {
    attempts=0
    until curl -ksf --noproxy '*' --connect-to "tutor.localhost:443:$address" https://tutor.localhost/ready >/dev/null; do
        attempts=$((attempts + 1))
        if [ "$attempts" -ge 30 ]; then
            compose logs --tail 30
            return 1
        fi
        sleep 1
    done
}
ready
python3 scripts/smoke-hobby.py --base-url https://tutor.localhost \
    --connect-url "https://$address" --insecure-local-tls --invite-file "$work/invitation"
compose restart tutor
ready
python3 scripts/smoke-hobby.py --base-url https://tutor.localhost \
    --connect-url "https://$address" --insecure-local-tls --expect-persistence \
    --client-id-file "$work/client-id"

# Operator commands use the running server's identity and private volume.
admin() {
    compose exec -T tutor /usr/local/bin/tutor-mcp users "$@" --data-dir /data/hobby
}
smoke() {
    python3 scripts/smoke-hobby.py --base-url https://tutor.localhost \
        --connect-url "https://$address" --insecure-local-tls --client-id-file "$work/client-id" "$@"
}
admin list > "$work/users"
grep -q '^acceptance-user[[:space:]]active$' "$work/users"
admin invite > "$work/second-invitation"
smoke --username acceptance-second --invite-file "$work/second-invitation"
admin reset acceptance-user > "$work/reset"
smoke --reset-file "$work/reset" --password 'Changed-acceptance-password-456' --expect-persistence
smoke --expect-login-denied
admin disable acceptance-user
smoke --password 'Changed-acceptance-password-456' --expect-login-denied
smoke --username acceptance-second --expect-persistence
