#!/usr/bin/env bash
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT

# This script runs as root only inside the disposable CI distribution imported
# by qualify-wsl-ci.ps1. It never installs into an existing developer distro.
set -euo pipefail

if [[ $# != 2 || $(id -u) != 0 ]]; then
    printf '%s\n' 'Expected the fixture manifest directory and official Node archive, as fixture root.' >&2
    exit 2
fi
fixture_manifest=$1
fixture_node_archive=$2
if [[ ! -f "$fixture_manifest/package.json" || ! -f "$fixture_manifest/package-lock.json" ]]; then
    printf '%s\n' 'The locked adapter manifests are missing.' >&2
    exit 2
fi

export DEBIAN_FRONTEND=noninteractive
apt-get -o Acquire::Retries=3 update -qq
apt-get -o Acquire::Retries=3 install -y --no-install-recommends ca-certificates xz-utils util-linux passwd procps

printf '%s  %s\n' 'e798599612f4bb71333a3397ab0d095fd62214e115aea45aa858a145fc72d67e' "$fixture_node_archive" | sha256sum --check --status
mkdir -p /opt/tutor-ci-node /opt/tutor-ci-adapter
tar -xJf "$fixture_node_archive" -C /opt/tutor-ci-node --strip-components=1
export PATH="/opt/tutor-ci-node/bin:/usr/sbin:/usr/bin:/sbin:/bin"
cp -- "$fixture_manifest/package.json" "$fixture_manifest/package-lock.json" /opt/tutor-ci-adapter/
npm ci --prefix /opt/tutor-ci-adapter --ignore-scripts --no-audit --no-fund

useradd --create-home --shell /bin/bash tutorci
install -d -m 0700 -o tutorci -g tutorci /home/tutorci/.codex
install -d -m 0755 -o tutorci -g tutorci /home/tutorci/.local /home/tutorci/.local/bin
install -d -m 0700 -o tutorci -g tutorci '/home/tutorci/mes cours'
ln -s /opt/tutor-ci-node/bin/node /home/tutorci/.local/bin/node
ln -s /opt/tutor-ci-adapter/node_modules/.bin/codex-acp /home/tutorci/.local/bin/codex-acp
cat > /home/tutorci/.local/bin/codex <<'EOF'
#!/bin/sh
# Instrument the actual installed CLI only to prove explicit runtime selection.
# No protocol response is mocked and every argument reaches the real Codex CLI.
if [ "${1-}" = app-server ]; then
    : > "$HOME/.codex/tutor-ci-selected-cli"
fi
exec /opt/tutor-ci-node/bin/node /opt/tutor-ci-adapter/node_modules/@openai/codex/bin/codex.js "$@"
EOF
chmod 0755 /home/tutorci/.local/bin/codex
chown -R tutorci:tutorci /home/tutorci

# Import defaults to root. Discovery must find and preserve the actual regular
# Linux user's environment, including an empty, private ~/.codex directory.
cat > /etc/wsl.conf <<'EOF'
[user]
default=tutorci
[boot]
systemd=false
[interop]
appendWindowsPath=false
EOF

runuser -u tutorci -- /usr/bin/env PATH=/home/tutorci/.local/bin:/usr/bin:/bin node --version
runuser -u tutorci -- /usr/bin/env PATH=/home/tutorci/.local/bin:/usr/bin:/bin codex --version
printf '%s\n' 'Disposable WSL2 fixture prepared; no account or model authentication was used.'
