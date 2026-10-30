#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Offline packaging fixtures; these do not qualify a native executable/model."""

import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("local_agent_notices", Path(__file__).with_name("collect-local-agent-notices.py"))
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)
LICENSE = Path(__file__).resolve().parents[2].joinpath("LICENSE").read_bytes()
BLOB = hashlib.sha1(b"blob " + str(len(LICENSE)).encode("ascii") + b"\0" + LICENSE).hexdigest()


class LocalNoticeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="notices-é-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.client = self.root / "client"
        self.agent = self.client / "local-agent"
        self.agent.mkdir(parents=True)
        (self.root / "LICENSE").write_bytes(LICENSE)
        (self.agent / "Cargo.toml").write_text('[package]\nname="tutor-local-agent"\nversion="0.1.0"\n')
        self.output = self.root / "out"
        self.rust = self.root / "rust-docs"
        (self.rust / "licenses").mkdir(parents=True)
        for name in ("COPYRIGHT-library.html", "licenses/MIT.txt", "licenses/Apache-2.0.txt"):
            (self.rust / name).write_bytes(LICENSE)
        self.metadata = {"packages": [{"id": "root", "name": "tutor-local-agent", "version": "0.1.0",
            "manifest_path": str(self.agent / "Cargo.toml")}],
            "resolve": {"root": "root", "nodes": [{"id": "root", "features": ["inference"], "deps": []}]}}
        self.lock_entries = []
        self.packages = {}
        for name in ("llama-cpp-2", "llama-cpp-sys-2"):
            files = {"Cargo.toml": b'[package]\nname="fixture"\n', "LICENSE": LICENSE,
                     ".cargo_vcs_info.json": json.dumps({"git": {"sha1": collector.LLAMA_RS}}).encode()}
            if name == "llama-cpp-sys-2":
                files["llama.cpp/ggml/src/ggml.c"] = b"/* Copyright Fixture; permission is hereby granted. */\n"
                files["llama.cpp/vendor/LICENSE"] = LICENSE
            self.add_package(name, "0.1.146", files)
        self.write_lock()
        # Network never used. Original repository license bytes stand in for
        # upstream licenses in fixtures; no real artifact qualification claimed.
        self.addCleanup(patch.stopall)
        patch.object(collector, "SUPPLEMENTS", {}).start()
        patch.object(collector, "LLAMA_LICENSE", ("LICENSE", BLOB)).start()
        self.fetch = lambda url: LICENSE

    def add_package(self, name, version, files, license_name="MIT"):
        key = f"{name}-{version}"
        source = self.root / "registry/src/test-index" / key
        source.mkdir(parents=True)
        (source / "private-account.json").write_text('PRIVATE-ACCOUNT-TOKEN')
        archive = self.root / "registry/cache/test-index" / f"{key}.crate"
        archive.parent.mkdir(parents=True, exist_ok=True)
        with tarfile.open(archive, "w:gz") as tar:
            for filename, data in files.items():
                entry = tarfile.TarInfo(f"{key}/{filename}")
                entry.size = len(data)
                tar.addfile(entry, io.BytesIO(data))
        package = {"id": key, "name": name, "version": version, "license": license_name,
                   "source": collector.REGISTRY, "manifest_path": str(source / "Cargo.toml")}
        self.metadata["packages"].append(package)
        self.metadata["resolve"]["nodes"].append({"id": key, "features": [], "deps": []})
        self.metadata["resolve"]["nodes"][0]["deps"].append({"pkg": key})
        self.lock_entries.append({"name": name, "version": version, "source": collector.REGISTRY,
                                  "checksum": collector.file_hash(archive)})
        self.packages[name] = (package, archive, files)

    def write_lock(self):
        (self.agent / "Cargo.lock").write_text("\n".join("[[package]]\n" + "\n".join(
            f"{key} = {json.dumps(value)}" for key, value in entry.items()) for entry in self.lock_entries))

    def collect(self):
        return collector.collect(self.client, self.output, metadata=self.metadata,
            rust_root=self.rust, rust_version="rustc 1.95.0 (fixture 2026-04-14)", fetch=self.fetch)

    def test_original_bytes_cpu_scope_and_existing_verify_tree(self):
        result = self.collect()
        self.assertEqual(collector.notices.verify_tree(self.output), result)
        self.assertEqual(result["backend"], "llama.cpp CPU")
        self.assertFalse(result["gpuQualified"])
        self.assertFalse(result["mutation"])
        self.assertFalse(result["modelIncluded"])
        self.assertIn("superset", result["coverage"])
        self.assertEqual(len(result["components"]), 4)
        encoded = json.dumps(result)
        self.assertNotIn(str(self.root), encoded)
        for path in self.output.rglob("*"):
            if path.is_file():
                self.assertNotIn(b"PRIVATE-ACCOUNT-TOKEN", path.read_bytes())
                self.assertNotEqual(path.suffix.lower(), ".gguf")
        native = next(c for c in result["components"] if c["kind"] == "embedded-native")
        legal = next(item for item in native["notices"] if item["source"].startswith("https://"))
        self.assertEqual((self.output / legal["file"]).read_bytes(), LICENSE)
        self.assertEqual((self.output / "LICENSE").read_bytes(), LICENSE)

    def test_cli_verifier_accepts_inventory(self):
        self.collect()
        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("collect-notices.py")),
                                 "verify-tree", str(self.output)], capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr.decode())

    def test_missing_dependency_license_refuses_output(self):
        self.add_package("unlicensed", "1.0.0", {"Cargo.toml": b"fixture"})
        self.write_lock()
        with self.assertRaisesRegex(ValueError, "no original license text"):
            self.collect()
        self.assertFalse(self.output.exists())

    def test_spdx_only_is_not_an_original_license(self):
        self.add_package("spdx-only", "1.0.0", {"LICENSE.spdx": b"SPDX-License-Identifier: MIT\n" * 20})
        self.write_lock()
        with self.assertRaisesRegex(ValueError, "no original license text"):
            self.collect()

    def test_missing_rust_library_notices(self):
        (self.rust / "COPYRIGHT-library.html").unlink()
        with self.assertRaisesRegex(ValueError, "Rust standard-library notices"):
            self.collect()
        self.assertFalse(self.output.exists())

    def test_wrong_rust_version(self):
        with self.assertRaisesRegex(ValueError, "toolchain 1.95.0"):
            collector.collect(self.client, self.output, metadata=self.metadata, rust_root=self.rust,
                              rust_version="rustc 1.94.0 (fixture 2026-03-01)")

    def test_tampered_archive_fails_before_copy(self):
        self.packages["llama-cpp-2"][1].write_bytes(b"changed archive")
        with self.assertRaisesRegex(ValueError, "changed locked Cargo archive"):
            self.collect()
        self.assertFalse(self.output.exists())

    def test_missing_lock_checksum(self):
        self.lock_entries[0].pop("checksum")
        self.write_lock()
        with self.assertRaisesRegex(ValueError, "archive checksum"):
            self.collect()

    def test_cuda_or_vulkan_root_features_rejected(self):
        for feature in ("cuda", "vulkan"):
            with self.subTest(feature=feature):
                self.metadata["resolve"]["nodes"][0]["features"] = ["inference", feature]
                with self.assertRaisesRegex(ValueError, "CPU inference"):
                    self.collect()

    def test_native_gpu_feature_rejected(self):
        self.metadata["resolve"]["nodes"][1]["features"] = ["cuda"]
        with self.assertRaisesRegex(ValueError, "binding version/features"):
            self.collect()

    def test_metadata_macos_metal_edge_is_an_explicit_superset(self):
        self.metadata["resolve"]["nodes"][2]["features"] = ["metal"]
        result = self.collect()
        component = next(c for c in result["components"] if c["name"] == "llama-cpp-sys-2")
        self.assertEqual(component["features"], ["metal"])
        self.assertFalse(result["gpuQualified"])
        self.assertIn("macOS-only", (self.output / "README.txt").read_text())

    def test_wrong_metadata_root_and_nonregistry_dependency(self):
        saved = copy.deepcopy(self.metadata)
        self.metadata["packages"][0]["name"] = "other-program"
        with self.assertRaisesRegex(ValueError, "Cargo root"):
            self.collect()
        self.metadata = saved
        self.metadata["packages"][1]["source"] = "git+https://private.example/token"
        with self.assertRaisesRegex(ValueError, "non-registry"):
            self.collect()

    def test_pinned_upstream_bytes_refuse_changed_licenses(self):
        self.fetch = lambda url: LICENSE + b"changed"
        with self.assertRaisesRegex(ValueError, "pinned Git blob"):
            self.collect()
        self.assertFalse(self.output.exists())

    def test_missing_upstream_native_license_fails(self):
        self.fetch = lambda url: b""
        with self.assertRaisesRegex(ValueError, "missing or oversized"):
            self.collect()

    def test_binding_supplement_wrong_source_commit_fails(self):
        with patch.object(collector, "SUPPLEMENTS", {"llama-cpp-2@0.1.146":
                ("example/original", "0" * 40, (("LICENSE", BLOB),))}):
            with self.assertRaisesRegex(ValueError, "upstream source commit mismatch"):
                self.collect()

    def test_original_upstream_supplement_is_inventoried(self):
        with patch.object(collector, "SUPPLEMENTS", {"llama-cpp-2@0.1.146":
                ("example/original", collector.LLAMA_RS, (("LICENSE", BLOB),))}):
            result = self.collect()
        component = next(c for c in result["components"] if c["name"] == "llama-cpp-2")
        self.assertEqual(component["upstreamCommit"], collector.LLAMA_RS)
        self.assertTrue(any(n["source"].startswith("https://raw.githubusercontent.com/example/")
                            for n in component["notices"]))

    def test_archive_path_traversal_and_disguised_model_forbidden(self):
        for filename, content in (("../../private.txt", b"secret"), ("/absolute", b"secret"),
                                  ("LICENSE", b"GGUFhidden model"),
                                  ("C:\\private", b"secret")):
            with self.subTest(filename=filename):
                archive = self.root / "unsafe.crate"
                with tarfile.open(archive, "w:gz") as tar:
                    entry = tarfile.TarInfo("fixture-1/" + filename)
                    entry.size = len(content)
                    tar.addfile(entry, io.BytesIO(content))
                with self.assertRaises(ValueError):
                    collector.extract_archive(archive, self.root / "extracted", {"name": "fixture", "version": "1"},
                                              collector.file_hash(archive))

    def test_source_vocabulary_gguf_is_skipped_never_distributed(self):
        self.add_package("vocabulary-fixture", "1.0.0", {
            "LICENSE": LICENSE,
            "src/gguf/ggml-vocab-bert-bge.gguf": b"GGUFPRIVATE-MODEL-SENTINEL",
            "tests/model.GGUF": b"GGUFSECOND-MODEL-SENTINEL",
            "src/lib.rs": b"/* Copyright Fixture; original license notice preserved. */\n"})
        self.write_lock()
        package, archive, _ = self.packages["vocabulary-fixture"]
        extracted = self.root / "extracted"
        skipped = collector.extract_archive(archive, extracted, package, collector.file_hash(archive))
        self.assertTrue(skipped)
        self.assertFalse((extracted / "src/gguf/ggml-vocab-bert-bge.gguf").exists())
        self.assertFalse((extracted / "tests/model.GGUF").exists())
        self.assertEqual((extracted / "LICENSE").read_bytes(), LICENSE)
        self.collect()
        for path in self.output.rglob("*"):
            if path.is_file():
                self.assertNotEqual(path.suffix.lower(), ".gguf")
                self.assertNotIn(b"MODEL-SENTINEL", path.read_bytes())
        with self.assertRaisesRegex(ValueError, "forbidden model"):
            collector.safe_name("notices/model.gguf")

    def test_model_bearing_mpl_archive_not_redistributed(self):
        self.add_package("mpl-with-model", "1.0.0", {"LICENSE": LICENSE, "test.gguf": b"GGUFfixture"}, "MPL-2.0")
        self.write_lock()
        with self.assertRaisesRegex(ValueError, "model-bearing corresponding source"):
            self.collect()
        self.assertFalse(self.output.exists())

    def test_archive_symlinks_rejected(self):
        archive = self.root / "symlink.crate"
        with tarfile.open(archive, "w:gz") as tar:
            entry = tarfile.TarInfo("fixture-1/LICENSE")
            entry.type = tarfile.SYMTYPE
            entry.linkname = "/private/secret"
            tar.addfile(entry)
        with self.assertRaisesRegex(ValueError, "nonregular"):
            collector.extract_archive(archive, self.root / "extracted", {"name": "fixture", "version": "1"},
                                      collector.file_hash(archive))

    def test_mpl_original_source_archive_is_included(self):
        self.add_package("mpl-fixture", "1.0.0", {"LICENSE": LICENSE}, "MPL-2.0")
        self.write_lock()
        result = self.collect()
        component = next(c for c in result["components"] if c["name"] == "mpl-fixture")
        self.assertEqual((self.output / component["correspondingSource"]).read_bytes(),
                         self.packages["mpl-fixture"][1].read_bytes())

    def test_existing_output_is_preserved(self):
        self.output.mkdir()
        (self.output / "keep").write_bytes(b"untouched")
        with self.assertRaisesRegex(ValueError, "new directory"):
            self.collect()
        self.assertEqual((self.output / "keep").read_bytes(), b"untouched")

    def test_subprocess_output_is_not_exposed(self):
        with patch.object(collector.subprocess, "run", side_effect=subprocess.CalledProcessError(
                1, ["cargo"], output="PRIVATE", stderr="PRIVATE")):
            with self.assertRaises(collector.NoticeError) as caught:
                collector.run(["cargo"], self.client)
        self.assertNotIn("PRIVATE", str(caught.exception))

    def test_help_needs_no_cargo_or_network(self):
        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("collect-local-agent-notices.py")),
                                 "collect", "--help"], capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0)
        self.assertIn(b"--output", result.stdout)
        self.assertIn(b"--target", result.stdout)


if __name__ == "__main__":
    unittest.main()
