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

	expected="$(awk -v name="$archive_name" '$2 == name || $2 == "*" name {print $1; exit}' "$sums_file")"
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

need_cmd uname
need_cmd tar
need_cmd awk
need_cmd find
need_cmd head
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
binary_path="$(find "$tmpdir" -type f -name "$binary_name" | head -n 1)"
[ -n "$binary_path" ] || die "archive did not contain $binary_name"

if mkdir -p "$install_dir" 2>/dev/null && [ -w "$install_dir" ]; then
	install -m 0755 "$binary_path" "${install_dir}/${binary_name}"
elif command -v sudo >/dev/null 2>&1; then
	sudo mkdir -p "$install_dir"
	sudo install -m 0755 "$binary_path" "${install_dir}/${binary_name}"
else
	die "cannot write to $install_dir; set TUTOR_MCP_INSTALL_DIR to a writable directory"
fi

printf '%s\n' "Installed ${binary_name} to ${install_dir}/${binary_name}"
