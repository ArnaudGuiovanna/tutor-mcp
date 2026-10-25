#!/usr/bin/env bash
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
set -euo pipefail

client_verify_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$client_verify_root"
client_verify_real=0
client_verify_install=0
for client_verify_arg in "$@"; do
    case "$client_verify_arg" in
        --real-adapter) client_verify_real=1 ;;
        --install) client_verify_install=1 ;;
        *) printf 'Usage: bash scripts/verify-client.sh [--install] [--real-adapter]\n' >&2; exit 2 ;;
    esac
done

if [[ "$client_verify_install" == 1 ]]; then
    npm ci --prefix client
    if [[ "$client_verify_real" == 1 ]]; then
        npm ci --prefix client/scripts/adapter --ignore-scripts
    fi
fi

npm --prefix client run check
npm --prefix client test
npm --prefix client run build
python3 -B -m unittest discover -s client/scripts -p test_collect_notices.py
python3 -B -m unittest discover -s client/scripts -p test_packaged_agents.py
cargo fmt --manifest-path client/src-tauri/Cargo.toml --check
cargo clippy --manifest-path client/src-tauri/Cargo.toml --locked --no-default-features --features qualification --all-targets -- -D warnings
cargo test --manifest-path client/src-tauri/Cargo.toml --locked --no-default-features --features qualification

if [[ "$client_verify_real" == 1 ]]; then
    for client_verify_package in codex-acp claude-agent-acp; do
        client_verify_entry="$client_verify_root/client/scripts/adapter/node_modules/@agentclientprotocol/$client_verify_package/dist/index.js"
        if [[ ! -f "$client_verify_entry" ]]; then
            printf 'Install the pinned adapter first: npm ci --prefix client/scripts/adapter --ignore-scripts\n' >&2
            exit 1
        fi
        client_verify_node=$(command -v node)
        # Git Bash paths must be made native for programs launched by Rust/Windows.
        if command -v cygpath >/dev/null 2>&1; then
            client_verify_node=$(cygpath -w "$client_verify_node")
            client_verify_entry=$(cygpath -w "$client_verify_entry")
        fi
        TUTOR_ACP_TEST_ADAPTER="$client_verify_node" \
        TUTOR_ACP_TEST_ADAPTER_ENTRY="$client_verify_entry" \
            cargo test --manifest-path client/src-tauri/Cargo.toml --locked --no-default-features --features qualification \
            real_codex_adapter_negotiates_acp -- --ignored
    done
fi
