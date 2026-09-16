#!/usr/bin/env bash
set -euo pipefail

vuln_go_bin=${TUTOR_GO_BIN:-go}
if [[ -z ${TUTOR_GO_BIN:-} && -x /snap/go/current/bin/go ]]; then
    vuln_go_bin=/snap/go/current/bin/go
fi
vuln_go_bin=$(command -v "$vuln_go_bin")
# govulncheck invokes go to load the application's packages. Use the same
# toolchain for that subprocess as for building the scanner itself.
export PATH="$(dirname "$vuln_go_bin"):$PATH"
exec "$vuln_go_bin" run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...
