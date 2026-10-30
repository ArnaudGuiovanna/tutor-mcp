#!/usr/bin/env bash
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
set -euo pipefail

if [[ $# != 0 ]]; then
    printf 'Usage: bash scripts/verify-local-agent.sh\n' >&2
    exit 2
fi

local_verify_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$local_verify_root"
local_verify_manifest=client/local-agent/Cargo.toml
export PYTHONDONTWRITEBYTECODE=1

# Discovery alone succeeds with zero tests when the file is missing.
for local_verify_required in "$local_verify_manifest" client/local-agent/Cargo.lock \
    client/scripts/qualify-local-agent.py client/scripts/test_qualify_local_agent.py; do
    if [[ ! -f "$local_verify_required" ]]; then
        printf 'Required local-agent verification input is missing: %s\n' "$local_verify_required" >&2
        exit 1
    fi
done

# Respect a caller-supplied CMAKE (including a path with spaces). Dependencies
# are provisioned by the caller/CI, never installed by this verification script.
python3 -B - "${CMAKE:-cmake}" <<'PY'
import re
import subprocess
import sys

try:
    output = subprocess.check_output([sys.argv[1], "--version"], text=True, stderr=subprocess.STDOUT)
except (OSError, subprocess.CalledProcessError) as error:
    raise SystemExit("CMake >=3.25 is required; install it or set CMAKE to its executable.") from error
version = re.search(r"^cmake version (\d+)\.(\d+)(?:\.(\d+))?", output, re.MULTILINE)
if version is None or tuple(map(int, version.group(1, 2))) < (3, 25):
    raise SystemExit("CMake >=3.25 is required by the native llama.cpp build.")
print(version.group(0), flush=True)
PY

cargo +1.95.0 fmt --manifest-path "$local_verify_manifest" --check
python3 -B -m unittest discover -s client/scripts -p test_qualify_local_agent.py
cargo +1.95.0 clippy --manifest-path "$local_verify_manifest" --locked --no-default-features --all-targets -- -D warnings
cargo +1.95.0 test --manifest-path "$local_verify_manifest" --locked --no-default-features
cargo +1.95.0 clippy --manifest-path "$local_verify_manifest" --locked --all-targets -- -D warnings
cargo +1.95.0 test --manifest-path "$local_verify_manifest" --locked

# Default features compile/link the CPU inference backend. The ignored test
# requiring the exact GGUF stays separate: these checks do not qualify a model.
printf 'Local-agent checks passed (native CPU build; real GGUF inference not tested).\n'
