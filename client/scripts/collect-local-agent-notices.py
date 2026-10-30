#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Collect original notices for the Windows CPU local agent (no model).

Output is compatible with collect-notices.py verify-tree. Requires the locked
Cargo registry archives and Rust 1.95.0 rust-docs. The reachable Windows CPU graph
is a declared build/dev superset, not a linker map. Missing upstream license
texts are fetched only from the commit/blob pins below; no model is downloaded.
"""

import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import tomllib
import urllib.request


_spec = importlib.util.spec_from_file_location("client_notices", Path(__file__).with_name("collect-notices.py"))
notices = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(notices)

TARGET = "x86_64-pc-windows-msvc"
RUST_VERSION = "1.95.0"
REGISTRY = "registry+https://github.com/rust-lang/crates.io-index"
COVERAGE = ("reachable target-filtered Cargo Windows CPU graph, including build/dev superset; "
            "embedded llama.cpp/ggml original licenses and legal source comments; Rust standard library; "
            "no model, GPU runtime, Tauri installer or exact linker-map claim")
LLAMA_RS = "4afdaf0782ef7f3254a186a7ff67a1c7491c6dce"
LLAMA_CPP = "e21cdc11a0461d8b0cbd28cc356d993bf6be7282"
# Original Git blobs, not generated SPDX templates. The llama.cpp commit is the
# llama-cpp-sys-2 submodule gitlink at LLAMA_RS. ggml is in that same source tree;
# it has no separate LICENSE at this commit. Preserve that tree's root license.
SUPPLEMENTS = {
    "llama-cpp-2@0.1.146": ("utilityai/llama-cpp-rs", LLAMA_RS, (
        ("LICENSE-APACHE", "1b5ec8b78e237b5c3b3d812a7c0a6589d0f7161d"),
        ("LICENSE-MIT", "7eadd8818796d258f5c90ef7980e0d47903a50b6"))),
    "llama-cpp-sys-2@0.1.146": ("utilityai/llama-cpp-rs", LLAMA_RS, (
        ("LICENSE-APACHE", "1b5ec8b78e237b5c3b3d812a7c0a6589d0f7161d"),
        ("LICENSE-MIT", "7eadd8818796d258f5c90ef7980e0d47903a50b6"))),
    "jsonschema-regex@0.60.2": ("Stranger6667/jsonschema", "0b657fa991c9333908e8c51e8a582d602aac5391", (
        ("LICENSE", "20c070f6401feda6f47be1b158f4f5a4a9d14062"),)),
    "jsonschema-value@0.60.2": ("Stranger6667/jsonschema", "0b657fa991c9333908e8c51e8a582d602aac5391", (
        ("LICENSE", "20c070f6401feda6f47be1b158f4f5a4a9d14062"),)),
    "uuid-simd@0.8.0": ("Nugine/simd", "d74c030d9dc4f3cae02146d1f497ff62726ef09a", (
        ("LICENSE", "2c7eabb59093ea117d73b20effdd0414f87b2af2"),)),
    "vsimd@0.8.0": ("Nugine/simd", "d74c030d9dc4f3cae02146d1f497ff62726ef09a", (
        ("LICENSE", "2c7eabb59093ea117d73b20effdd0414f87b2af2"),)),
    # This published crate omits .cargo_vcs_info.json: bind to the exact archive
    # below and retain original project texts from the upstream 0.2 tag commit.
    "find_cuda_helper@0.2.0": ("Rust-GPU/Rust-CUDA", "aaa3f92fbad80afc6c6021fb2b58b6d13c00fc43", (
        ("LICENSE-APACHE", "941b8bef62fcdd7d80e86fadb88d346b16f90d8d"),
        ("LICENSE-MIT", "afb9e80205037b9122b4c85d21ee4615e9ab583d"))),
}
CUDA_HELPER_ARCHIVE = "f9f9e65c593dd01ac77daad909ea4ad17f0d6d1776193fc8ea766356177abdad"
LLAMA_LICENSE = ("LICENSE", "e7dca554bcb802f98408383a864404e3aa4eacca")


class NoticeError(ValueError):
    """Public component/error identifiers, without private paths or subprocess output."""


def require(condition, message):
    if not condition:
        raise NoticeError(message)


def run(args, cwd):
    try:
        return subprocess.run(args, cwd=cwd, check=True, capture_output=True,
                              encoding="utf-8", timeout=180).stdout
    except (OSError, subprocess.SubprocessError) as error:
        raise NoticeError("metadata command failed; install cached dependencies and Rust 1.95.0 rust-docs") from error


def file_hash(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def safe_name(value, *, source_archive=False):
    path = notices.safe_path(value)
    require(path.as_posix() == value and (source_archive or path.suffix.lower() != ".gguf")
            and not any(ord(char) < 32 or char in '<>"|?*' for char in value),
            "unsafe source/notice path or forbidden model")
    return path


def fetch_original(url):
    try:
        with urllib.request.urlopen(url, timeout=20) as response:
            require(response.geturl() == url, "unexpected upstream license redirect")
            data = response.read(1024 * 1024 + 1)
            require(100 <= len(data) <= 1024 * 1024, "missing or oversized upstream license")
            return data
    except (OSError, ValueError) as error:
        if isinstance(error, NoticeError):
            raise
        raise NoticeError("pinned upstream license unavailable; collection refused") from error


def supplement_files(repository, commit, files, destination, fetch):
    records = []
    for name, blob in files:
        safe_name(name)
        url = f"https://raw.githubusercontent.com/{repository}/{commit}/{name}"
        data = fetch(url)
        require(isinstance(data, bytes) and 100 <= len(data) <= 1024 * 1024,
                "missing or oversized upstream license")
        identity = hashlib.sha1(b"blob " + str(len(data)).encode("ascii") + b"\0" + data).hexdigest()
        require(identity == blob, "upstream license differs from pinned Git blob")
        path = destination / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        records.append({"file": name, "source": url, "sha256": notices.digest(data), "gitBlob": blob})
    return records


def extract_archive(archive, root, package, checksum):
    key = f"{package['name']}-{package['version']}"
    require(not archive.is_symlink() and archive.is_file() and file_hash(archive) == checksum,
            f"missing or changed locked Cargo archive: {key}")
    total, seen, skipped_models = 0, set(), False
    with tarfile.open(archive, "r:gz") as source:
        for entry in source:
            path = safe_name(entry.name, source_archive=True)
            require(path.parts[0] == key and len(path.parts) > 1,
                    f"escaped Cargo archive: {key}")
            if entry.isdir():
                continue
            require(entry.isfile() and entry.name.casefold() not in seen,
                    f"nonregular or duplicate Cargo archive entry: {key}")
            seen.add(entry.name.casefold())
            total += entry.size
            require(0 <= entry.size <= 64 * 1024 * 1024 and total <= 512 * 1024 * 1024
                    and len(seen) <= 30000, f"Cargo archive exceeds bounds: {key}")
            # Cargo ships GGUF vocabulary/test fixtures alongside Rust source.
            # They are not legal notices: never extract or distribute them.
            # Path/type/size validation still applies before skipping a member.
            if path.suffix.lower() == ".gguf":
                skipped_models = True
                continue
            destination = root.joinpath(*path.parts[1:])
            destination.parent.mkdir(parents=True, exist_ok=True)
            with source.extractfile(entry) as content, destination.open("xb") as target:
                first = content.read(4)
                require(first != b"GGUF", "model content forbidden")
                target.write(first)
                shutil.copyfileobj(content, target, 1024 * 1024)
    return skipped_models


def collect(client, output, target=TARGET, *, metadata=None, rust_root=None,
            rust_version=None, fetch=fetch_original):
    client, output = Path(client).resolve(), Path(output).absolute()
    require(target == TARGET, "only Windows x64 CPU is supported")
    require(not output.exists() and not output.is_symlink(), "output must be a new directory")
    manifest = client / "local-agent/Cargo.toml"
    lock_path = client / "local-agent/Cargo.lock"
    if metadata is None:
        metadata = json.loads(run(["cargo", "+1.95.0", "metadata", "--manifest-path", str(manifest),
            "--format-version", "1", "--locked", "--offline", "--filter-platform", target,
            "--no-default-features", "--features", "inference"], client))
    nodes = {node["id"]: node for node in metadata["resolve"]["nodes"]}
    root_id = metadata["resolve"]["root"]
    root_package = next((item for item in metadata["packages"] if item["id"] == root_id), {})
    require(root_package.get("name") == "tutor-local-agent"
            and Path(root_package.get("manifest_path", "")).resolve() == manifest.resolve(),
            "Cargo root differs from the local agent")
    root_features = nodes[root_id]["features"]
    require("inference" in root_features and set(root_features) <= {"default", "inference"},
            "Cargo graph is not the CPU inference build")
    packages = notices.reachable_packages(metadata)
    for package in packages:
        require(re.fullmatch(r"[A-Za-z0-9_-]+", package["name"]) is not None
                and re.fullmatch(r"[A-Za-z0-9.+_-]+", package["version"]) is not None,
                "invalid Cargo package identity")
        require(package.get("source") == REGISTRY, "unreviewed non-registry Cargo dependency")
        if package["name"] in {"llama-cpp-2", "llama-cpp-sys-2"}:
            # Cargo metadata unifies features across target-specific edges even
            # with --filter-platform. Binding 0.1.146 has a macOS-only sys/metal
            # edge; this is retained in the inventory, not a Windows backend.
            allowed = {"sampler"} if package["name"] == "llama-cpp-2" else {"metal"}
            require(package["version"] == "0.1.146"
                    and set(nodes[package["id"]]["features"]) <= allowed,
                    "llama.cpp binding version/features require notice review")
    require({"llama-cpp-2", "llama-cpp-sys-2"} <= {p["name"] for p in packages},
            "CPU graph omits native inference dependencies")
    lock = tomllib.loads(lock_path.read_text(encoding="utf-8"))
    checksums = {(p["name"], p["version"], p.get("source")): p.get("checksum") for p in lock["package"]}
    rust_version = rust_version or run(["rustc", "+1.95.0", "--version"], client).strip()
    require(re.fullmatch(r"rustc 1\.95\.0 \([A-Za-z0-9 -]+\)", rust_version) is not None,
            "Rust standard-library notices require toolchain 1.95.0")
    rust_root = Path(rust_root) if rust_root is not None else Path(
        run(["rustc", "+1.95.0", "--print", "sysroot"], client).strip()) / "share/doc/rust"
    output.parent.mkdir(parents=True, exist_ok=True)
    cache = {}

    def cached_fetch(url):
        if url not in cache:
            cache[url] = fetch(url)
        return cache[url]

    with tempfile.TemporaryDirectory(prefix=".local-agent-notices-", dir=output.parent) as temporary:
        temporary = Path(temporary)
        staging = temporary / "notices"
        records, components = [], []
        license_bytes = (client.parent / "LICENSE").read_bytes()
        require(len(license_bytes.strip()) >= 100, "missing original project license")
        notices.put(staging, "LICENSE", license_bytes, records)
        for package in packages:
            key = f"{package['name']}@{package['version']}"
            checksum = checksums.get((package["name"], package["version"], package["source"]))
            require(isinstance(checksum, str) and re.fullmatch(r"[0-9a-f]{64}", checksum),
                    f"missing locked archive checksum: {key}")
            cached_root = Path(package["manifest_path"]).parent
            archive = (cached_root.parent.parent.parent / "cache" / cached_root.parent.name
                       / f"{package['name']}-{package['version']}.crate")
            with tempfile.TemporaryDirectory(prefix="crate-", dir=temporary) as extracted:
                root = Path(extracted)
                skipped_models = extract_archive(archive, root, package, checksum)
                component = {"kind": "cargo", "name": package["name"], "version": package["version"],
                    "license": package.get("license"), "source": package["source"], "archiveSha256": checksum,
                    "features": nodes[package["id"]]["features"]}
                require(component["license"] or package.get("license_file"), f"missing license declaration: {key}")
                extra = []
                if package.get("license_file"):
                    relative = safe_name(package["license_file"])
                    extra = [root.joinpath(*relative.parts)]
                supplements = []
                supplement_root = temporary / "upstream" / key
                if key in SUPPLEMENTS:
                    repository, commit, files = SUPPLEMENTS[key]
                    if key == "find_cuda_helper@0.2.0":
                        require(checksum == CUDA_HELPER_ARCHIVE, "CUDA helper archive identity mismatch")
                    else:
                        vcs = json.loads((root / ".cargo_vcs_info.json").read_text(encoding="utf-8"))
                        require(vcs.get("git", {}).get("sha1") == commit, f"upstream source commit mismatch: {key}")
                    supplements = supplement_files(repository, commit, files, supplement_root, cached_fetch)
                    component["upstreamCommit"] = commit
                components.append(notices.copy_component(staging, component, root, records,
                    extra_files=extra, supplement_root=supplement_root, supplements=supplements))
                if "MPL-2.0" in (package.get("license") or ""):
                    require(not skipped_models, "model-bearing corresponding source archive requires separate review")
                    component["correspondingSource"] = notices.put(staging, f"sources/{archive.name}", archive.read_bytes(), records)
                if package["name"] == "llama-cpp-sys-2":
                    native = root / "llama.cpp"
                    require((native / "ggml/src/ggml.c").is_file(), "embedded ggml source missing")
                    upstream = temporary / "llama-upstream"
                    legal = supplement_files("ggml-org/llama.cpp", LLAMA_CPP, (LLAMA_LICENSE,), upstream, cached_fetch)
                    components.append(notices.copy_component(staging,
                        {"kind": "embedded-native", "name": "llama.cpp-ggml", "version": LLAMA_CPP,
                         "license": "MIT", "upstreamCommit": LLAMA_CPP, "archiveSha256": checksum,
                         "scope": "llama.cpp and ggml bundled by llama-cpp-sys-2; original tree license and source comments"},
                        native, records, supplement_root=upstream, supplements=legal))
        required_rust = [rust_root / "COPYRIGHT-library.html", rust_root / "licenses/MIT.txt",
                         rust_root / "licenses/Apache-2.0.txt"]
        require(all(path.is_file() and not path.is_symlink() and path.stat().st_size >= 100
                    for path in required_rust), "missing original Rust standard-library notices; install rust-docs")
        components.append(notices.copy_component(staging,
            {"kind": "toolchain", "name": "Rust-standard-library", "version": RUST_VERSION,
             "license": "original standard-library copyright inventory and accompanying license texts",
             "rustcVersion": rust_version}, rust_root, records,
            extra_files=required_rust + sorted((rust_root / "licenses").glob("*")),
            scan_original=False, scan_source=False))
        notices.put(staging, "README.txt", ("Tutor local agent CPU third-party notices\n\n" + COVERAGE
            + ".\nOriginal bytes retained; no SPDX-generated substitute texts.\n"
            "A build dependency named find_cuda_helper does not imply GPU support.\n"
            "Cargo metadata may list sys/metal from the macOS-only dependency edge; Windows uses CPU.\n"
            "Pinned original upstream texts supplement omissions from published Cargo archives.\n"
            "MPL-2.0 corresponding source archives are included under sources/.\n").encode("utf-8"), records)
        for record in records:
            safe_name(record["file"])
            with (staging / record["file"]).open("rb") as content:
                require(content.read(4) != b"GGUF", "model content forbidden in notice output")
        inventory = {"format": notices.FORMAT, "target": target, "distribution": "tutor-local-agent-cpu",
            "backend": "llama.cpp CPU", "gpuQualified": False, "mutation": False, "modelIncluded": False,
            "coverage": COVERAGE, "rootCargoFeatures": root_features, "components": components,
            "inputs": [{"file": path.relative_to(client).as_posix(), "sha256": file_hash(path)}
                       for path in (manifest, lock_path)], "files": sorted(records, key=lambda item: item["file"])}
        (staging / "inventory.json").write_text(json.dumps(inventory, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        notices.verify_tree(staging)
        require(not output.exists(), "output appeared during collection")
        staging.rename(output)
    return inventory


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    build = sub.add_parser("collect", help="collect Windows CPU originals; fail on absent/changed licenses")
    build.add_argument("--client", type=Path, default=Path(__file__).resolve().parents[1])
    build.add_argument("--target", choices=[TARGET], default=TARGET)
    build.add_argument("--output", type=Path, required=True, help="new output directory (never replaces an existing directory)")
    args = parser.parse_args()
    try:
        inventory = collect(args.client, args.output, args.target)
        print(json.dumps({"format": inventory["format"], "target": inventory["target"],
                          "components": len(inventory["components"]), "files": len(inventory["files"])}))
    except (ValueError, OSError, KeyError, TypeError, tarfile.TarError) as error:
        parser.exit(1, "Notice collection failed: " + (str(error) if isinstance(error, NoticeError)
            else "required original license/source missing or invalid; private paths withheld") + "\n")


if __name__ == "__main__":
    main()
