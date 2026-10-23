# Notices in binary distributions

Tutor MCP's own code remains MIT licensed. Its Go dependencies, the Go runtime,
embedded third-party code and container certificates retain their original terms.

`scripts/package-release.py` builds all six Linux/macOS/Windows amd64/arm64
archives with `CGO_ENABLED=0`. Each archive contains the binary, the project's
`LICENSE` and `THIRD_PARTY_NOTICES/`. The latter contains original licence texts,
NOTICE and attribution files, additional copyright/licence comments from the
selected source files, and a versioned inventory with their SHA-256 hashes.

The collector compares the modules recorded in the actual binary with the exact
target package graph. It uses the same Go toolchain and build tags; missing
licences, unsupported CGO/replaced modules or inconsistent graphs stop packaging.
It preserves the complete MCP SDK transition text, YAML's NOTICE, SQLite/SQLite
Vec notices, libc's embedded-component notices and Go's licence/patent text. It
does not replace those texts with a guessed SPDX identifier or classify their
legal effects. Dependency changes still require review of the original terms.

The Docker builder collects notices for its actual `timetzdata` build, including
the time-zone attribution. It also records the installed Debian CA package's
version, original copyright text, referenced complete terms and certificate
bundle hash. The final scratch image retains these files at
`/usr/share/doc/tutor-mcp/`. The Compose acceptance test copies them back from the
final image and verifies both the binary and certificate bundle against them.

The LobeHub logos used in source documentation have their complete original
licence in `docs/assets/logos/LICENSE`. They are not embedded in the binary.

## Installation and older archives

Current shell and PowerShell installers verify the downloaded archive, its
inventory's binary hash and target, and every notice file before changing an
existing installation. They retain the texts in `tutor-mcp-notices/` beside the
installed executable. The client machine needs no Python or Go installation.

The original v0.6.1 archives remain unchanged. Their notices are supplied by six
additional release assets, named `tutor-mcp_OS_ARCH_notices.tar.gz` (Linux/macOS)
or `tutor-mcp_windows_ARCH_notices.zip`, plus
`THIRD_PARTY_NOTICES_SHA256SUMS` and a provenance report. The updated installers
download this supplement only when the binary archive has no bundled notices.
They verify its checksum and its inventory's exact v0.6.1 source commit, original
archive hash, binary hash and platform. A missing, altered or mismatched
supplement stops installation without replacing the installed binary or notices.

The checksum files come from the same HTTPS release as the archives. They detect
corruption and inconsistent assets; they are not an independent signature or a
second trust channel. The legacy fallback is qualified for v0.6.1 only. For
earlier archives without notices, build the corresponding source or use a newer
distribution with notices; no permissive fallback silently omits them.

## Preparing an additive v0.6.1 supplement

This operation prepares files only. It never uploads or overwrites release
assets. Export the exact source tag into a temporary directory and download the
six original archives and their existing `SHA256SUMS`. Use the original
Go 1.26.8 toolchain and the date reported by the released native binary:

```sh
TUTOR_GO_BIN=/path/to/go1.26.8/bin/go python3 -B scripts/package-legacy-notices.py \
  --version v0.6.1 \
  --source-commit 440f837e7f043f2f64db23be9459512375dfa542 \
  --build-date 2026-09-19T10:26:21Z \
  --source /path/to/exported-v0.6.1 \
  --archives /path/to/original-archives \
  --output /path/to/new-empty-supplement-directory
```

The preparer checks every original archive hash, reproduces every binary
byte-for-byte, compares its recorded module/toolchain information with the
source graph, and verifies every prepared notice payload. A discrepancy stops
the operation. Review the resulting `THIRD_PARTY_NOTICES_PROVENANCE.json` and
the original mixed/embedded-component terms before adding the eight new assets
to the existing release. Keep the existing binaries, archives and `SHA256SUMS`
unchanged. The supplement must be available before publishing installers that
depend on it; adding notices does not incorporate newer code/security fixes in
the old binaries.

## Checks

Offline tests cover licence text preservation, missing additional notices,
module-graph mismatches, all six archive target formats and installer rejection
of missing/changed/mismatched notices. Native Windows CI runs the PowerShell
installer against a real binary and generated notices. Every target build in
CI collects and verifies its actual notices, and the release packager verifies
each completed archive before writing its checksum.

```sh
python3 -B -m unittest discover -s scripts -p test_third_party_notices.py
python3 -B -m unittest discover -s scripts -p test_installers.py
python3 -B scripts/third_party_notices.py --verify-archive /path/to/tutor-mcp_linux_amd64.tar.gz
```
