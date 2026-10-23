#!/bin/sh
set -eu

repo_owner="ArnaudGuiovanna"
repo_name="tutor-mcp"
binary_name="tutor-mcp"
install_dir="${TUTOR_MCP_INSTALL_DIR:-/usr/local/bin}"
version="${TUTOR_MCP_VERSION:-latest}"

die() {
	printf '%s\n' "error: $*" >&2
	exit 1
}

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

download() {
	url="$1"
	output="$2"

	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" -o "$output"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$output" "$url"
	else
		die "missing required command: curl or wget"
	fi
}

detect_os() {
	case "$(uname -s)" in
		Linux) printf '%s\n' linux ;;
		Darwin) printf '%s\n' darwin ;;
		*) die "unsupported OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
		x86_64 | amd64) printf '%s\n' amd64 ;;
		arm64 | aarch64) printf '%s\n' arm64 ;;
		*) die "unsupported architecture: $(uname -m)" ;;
	esac
}

sha256_file() {
	file="$1"

	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$file" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$file" | awk '{print $1}'
	else
		die "missing required checksum command: sha256sum or shasum"
	fi
}

verify_checksum() {
	sums_file="$1"
	archive_file="$2"
	archive_name="$3"

	expected="$(awk -v name="$archive_name" '$2 == name || $2 == "*" name {value=$1; count++} END {if(count==1) print value; else exit 1}' "$sums_file")" || die "missing or ambiguous checksum for $archive_name"
	if [ -z "$expected" ]; then
		die "$archive_name not found in SHA256SUMS"
	fi
	[ "${#expected}" -eq 64 ] || die "invalid checksum entry for $archive_name"
	case "$expected" in
		*[!0-9A-Fa-f]*) die "invalid checksum entry for $archive_name" ;;
	esac
	expected="$(printf '%s' "$expected" | tr 'A-F' 'a-f')"

	actual="$(sha256_file "$archive_file")"
	[ "$actual" = "$expected" ] || die "checksum mismatch for $archive_name"
}

inventory_string() {
	# The version-1 collector writes one JSON field per line with JSON-safe
	# component paths. Reject missing/duplicate fields rather than guessing.
	awk -F '"' -v key="$1" '$2 == key {value=$4; count++} END {if(count==1) print value; else exit 1}' "$inventory"
}

verify_notices() {
	inventory="${notices_path}/inventory.json"
	[ -s "$inventory" ] && [ -s "${notices_path}/README.txt" ] || die "archive did not contain third-party notices"
	awk '/^[ \t]*"format_version"[ \t]*:[ \t]*1[, \t]*$/ {count++} END {if(count!=1) exit 1}' "$inventory" || die "unsupported notice inventory format"
	awk -F '"' '$2 == "path" && $4 == "go" {count++} END {if(count!=1) exit 1}' "$inventory" || die "notice inventory lacks Go runtime"
	[ "$(inventory_string binary)" = "$binary_name" ] || die "notice inventory binary mismatch"
	[ "$(inventory_string goos)" = "$os" ] && [ "$(inventory_string goarch)" = "$arch" ] || die "notice inventory target mismatch"
	[ "$(inventory_string binary_sha256)" = "$(sha256_file "$binary_path")" ] || die "notice inventory binary checksum mismatch"
	awk -F '"' '
		$2 == "file" {if(file != "") exit 1; file=$4}
		$2 == "sha256" {if(file == "") exit 1; print $4 "  " file; file=""; count++}
		END {if(file != "" || count == 0) exit 1}
	' "$inventory" > "${tmpdir}/notice-files.sha256" || die "invalid notice file inventory"
	while read -r notice_hash notice_file; do
		case "$notice_file" in components/*) ;; *) die "invalid notice file path" ;; esac
		case "$notice_file" in *'/../'* | *'/..' | *'\'* | *'"'*) die "invalid notice file path" ;; esac
		[ "${#notice_hash}" -eq 64 ] || die "invalid notice file checksum"
		case "$notice_hash" in *[!0-9a-f]*) die "invalid notice file checksum" ;; esac
		[ -s "${notices_path}/${notice_file}" ] || die "missing notice file: $notice_file"
		[ "$(sha256_file "${notices_path}/${notice_file}")" = "$notice_hash" ] || die "notice file checksum mismatch: $notice_file"
	done < "${tmpdir}/notice-files.sha256"
}

need_cmd uname
need_cmd tar
need_cmd awk
need_cmd install
need_cmd mktemp
need_cmd tr

os="$(detect_os)"
arch="$(detect_arch)"
asset="${binary_name}_${os}_${arch}.tar.gz"

if [ "$version" = "latest" ]; then
	base_url="https://github.com/${repo_owner}/${repo_name}/releases/latest/download"
else
	base_url="https://github.com/${repo_owner}/${repo_name}/releases/download/${version}"
fi

tmpdir="$(mktemp -d 2>/dev/null || mktemp -d -t tutor-mcp)"
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

archive_path="${tmpdir}/${asset}"
sums_path="${tmpdir}/SHA256SUMS"

printf '%s\n' "Downloading ${asset}..."
download "${base_url}/${asset}" "$archive_path"

download "${base_url}/SHA256SUMS" "$sums_path" || die "failed to download required SHA256SUMS"
verify_checksum "$sums_path" "$archive_path" "$asset"

tar -xzf "$archive_path" -C "$tmpdir"
binary_path="${tmpdir}/${binary_name}"
[ -f "$binary_path" ] || die "archive did not contain $binary_name"
notice_source="$tmpdir"
notices_path="${notice_source}/THIRD_PARTY_NOTICES"
if [ ! -e "$notices_path" ]; then
	# v0.6.1 binaries remain unchanged. Their separately published supplement
	# is bound to this exact archive, binary, target and source release.
	[ "$version" = latest ] || [ "$version" = v0.6.1 ] || die "this release predates bundled notices; build from source or use a release with notices"
	supplement="${binary_name}_${os}_${arch}_notices.tar.gz"
	download "${base_url}/${supplement}" "${tmpdir}/${supplement}" || die "required legacy notice supplement unavailable; build from source"
	download "${base_url}/THIRD_PARTY_NOTICES_SHA256SUMS" "${tmpdir}/THIRD_PARTY_NOTICES_SHA256SUMS" || die "required legacy notice checksums unavailable"
	verify_checksum "${tmpdir}/THIRD_PARTY_NOTICES_SHA256SUMS" "${tmpdir}/${supplement}" "$supplement"
	notice_source="${tmpdir}/supplement"
	mkdir "$notice_source"
	tar -xzf "${tmpdir}/${supplement}" -C "$notice_source"
	notices_path="${notice_source}/THIRD_PARTY_NOTICES"
	inventory="${notices_path}/inventory.json"
	[ -s "$inventory" ] || die "legacy notice supplement has no inventory"
	[ "$(inventory_string release_version)" = v0.6.1 ] || die "legacy notice supplement release mismatch"
	[ "$(inventory_string source_commit)" = 440f837e7f043f2f64db23be9459512375dfa542 ] || die "legacy notice supplement source mismatch"
	[ "$(inventory_string original_archive_sha256)" = "$(sha256_file "$archive_path")" ] || die "legacy notice supplement archive checksum mismatch"
fi
[ -s "${notice_source}/LICENSE" ] || die "archive did not contain LICENSE"
verify_notices
notices_dir="${install_dir}/tutor-mcp-notices"

if mkdir -p "$install_dir" 2>/dev/null && [ -w "$install_dir" ]; then
	mkdir -p "$notices_dir"
	cp -R "$notices_path" "${notices_dir}/"
	install -m 0644 "${notice_source}/LICENSE" "${notices_dir}/LICENSE"
	install -m 0755 "$binary_path" "${install_dir}/${binary_name}"
elif command -v sudo >/dev/null 2>&1; then
	sudo mkdir -p "$notices_dir"
	sudo cp -R "$notices_path" "${notices_dir}/"
	sudo install -m 0644 "${notice_source}/LICENSE" "${notices_dir}/LICENSE"
	sudo install -m 0755 "$binary_path" "${install_dir}/${binary_name}"
else
	die "cannot write to $install_dir; set TUTOR_MCP_INSTALL_DIR to a writable directory"
fi

printf '%s\n' "Installed ${binary_name} to ${install_dir}/${binary_name}"
printf '%s\n' "Licence texts and third-party notices: ${notices_dir}"
printf '%s\n' 'For local MCP clients, use command: tutor-mcp and args: ["--local"].'
