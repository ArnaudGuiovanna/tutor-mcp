#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT

import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("client_notices",Path(__file__).with_name("collect-notices.py"))
notices = importlib.util.module_from_spec(spec)
spec.loader.exec_module(notices)
LICENSE = Path(__file__).resolve().parents[2].joinpath("LICENSE").read_bytes()


class NoticeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)

    def tearDown(self):
        self.temporary.cleanup()

    def make_component(self):
        root = self.root / "source"
        root.mkdir()
        (root / "LICENSE").write_bytes(LICENSE)
        (root / "NOTICE").write_bytes("Attribution originale — élève\r\n".encode("utf-8"))
        (root / "src").mkdir()
        (root / "src/header.rs").write_bytes(b"/* Copyright Vendor.\r\nRedistribution requires this original notice. */\r\nfn main() {}\n")
        return root

    def inventory(self, output, records, component):
        notices.put(output,"LICENSE",LICENSE,records)
        inventory = {"format":notices.FORMAT,"target":"x86_64-pc-windows-msvc",
            "components":[component],"files":records}
        (output / "inventory.json").write_text(json.dumps(inventory),encoding="utf-8")
        return inventory

    def test_original_notices_and_embedded_legal_comments_survive_distribution(self):
        source, output, records = self.make_component(),self.root / "out",[]
        component = notices.copy_component(output,{"kind":"cargo","name":"example","version":"1.0.0","license":"MIT"},source,records)
        self.inventory(output,records,component)
        notices.verify_tree(output)
        copied = {entry["source"]:output / entry["file"] for entry in component["notices"]}
        self.assertEqual(copied["LICENSE"].read_bytes(),(source / "LICENSE").read_bytes())
        self.assertEqual(copied["NOTICE"].read_bytes(),(source / "NOTICE").read_bytes())
        self.assertIn(b"/* Copyright Vendor.\r\nRedistribution requires this original notice. */",
                      copied["original source comments (deduplicated)"].read_bytes())

    def test_an_spdx_expression_or_empty_file_cannot_replace_a_license_text(self):
        source, output = self.root / "source",self.root / "out"
        source.mkdir()
        (source / "LICENSE.spdx").write_text("SPDX-License-Identifier: MIT\n" * 8)
        (source / "LICENSE").write_bytes(b"")
        with self.assertRaisesRegex(ValueError,"no original license text"):
            notices.copy_component(output,{"kind":"cargo","name":"unknown","version":"1","license":"MIT"},source,[])

    def test_distribution_verification_rejects_missing_changed_and_extra_files(self):
        source, output, records = self.make_component(),self.root / "out",[]
        component = notices.copy_component(output,{"kind":"npm","name":"example","version":"1","license":"MIT"},source,records)
        self.inventory(output,records,component)
        original = output / component["notices"][0]["file"]
        saved = original.read_bytes()
        original.write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError,"changed notice"):
            notices.verify_tree(output)
        original.write_bytes(saved)
        extra = output / "extra.txt"
        extra.write_text("unreviewed")
        with self.assertRaisesRegex(ValueError,"absent from its inventory"):
            notices.verify_tree(output)
        extra.unlink()
        original.unlink()
        with self.assertRaisesRegex(ValueError,"missing or escaped"):
            notices.verify_tree(output)

    def test_mpl_component_requires_available_corresponding_source(self):
        source, output, records = self.make_component(),self.root / "out",[]
        component = notices.copy_component(output,{"kind":"cargo","name":"example","version":"1","license":"MPL-2.0"},source,records)
        self.inventory(output,records,component)
        with self.assertRaisesRegex(ValueError,"corresponding source"):
            notices.verify_tree(output)

    def test_inventory_cannot_escape_the_distribution_directory(self):
        source, output, records = self.make_component(),self.root / "out",[]
        component = notices.copy_component(output,{"kind":"cargo","name":"example","version":"1","license":"MIT"},source,records)
        inventory = self.inventory(output,records,component)
        inventory["files"][0]["file"] = "../private.txt"
        (output / "inventory.json").write_text(json.dumps(inventory))
        with self.assertRaisesRegex(ValueError,"unsafe inventory path"):
            notices.verify_tree(output)

    def test_upstream_supplement_is_bound_to_the_exact_crate_commit(self):
        source = self.make_component()
        supplements = {"components":{"example@1":{"vcsCommit":"reviewed-commit","files":[]}}}
        (source / ".cargo_vcs_info.json").write_text(json.dumps({"git":{"sha1":"different-commit"}}))
        with self.assertRaisesRegex(ValueError,"source commit"):
            notices.supplement_records(source,"example@1",{"license":"MIT"},supplements)

    def test_target_graph_excludes_unreachable_platform_dependencies(self):
        metadata = {"resolve":{"root":"root","nodes":[
            {"id":"root","deps":[{"pkg":"windows"}]},{"id":"windows","deps":[]},{"id":"macos","deps":[]}]},
            "packages":[{"id":name,"name":name,"version":"1"} for name in ["root","windows","macos"]]}
        self.assertEqual([package["name"] for package in notices.reachable_packages(metadata)],["windows"])

    def test_npm_graph_uses_the_actual_nearest_locked_dependency(self):
        client = self.root
        packages = {"":{"dependencies":{"a":"1"}}}
        definitions = [("node_modules/a","a","1",{"b":"1"}),
            ("node_modules/a/node_modules/b","b","1",{"shared":"2"}),
            ("node_modules/a/node_modules/shared","shared","2",{}),
            ("node_modules/shared","shared","1",{}),("node_modules/vite","vite","8",{})]
        for path,name,version,dependencies in definitions:
            packages[path] = {"version":version,"integrity":"sha512-fixture","dependencies":dependencies}
            package = client / path
            package.mkdir(parents=True)
            (package / "package.json").write_text(json.dumps({"name":name,"version":version,"license":"MIT"}))
        (client / "package-lock.json").write_text(json.dumps({"packages":packages}))
        result = {component["name"]:component["version"] for component,_ in notices.npm_packages(client)}
        self.assertEqual(result["shared"],"2")
        (client / "node_modules/a/package.json").write_text(json.dumps({"name":"a","version":"unlocked"}))
        with self.assertRaisesRegex(ValueError,"differs from its lock"):
            notices.npm_packages(client)

    def test_utf8_metadata_works_even_with_a_cp1252_host_default(self):
        original = subprocess.check_output
        def cp1252_default(*args,**kwargs):
            kwargs.setdefault("encoding","cp1252")
            return original(*args,**kwargs)
        command = [sys.executable,"-c","import sys;sys.stdout.buffer.write(b'{\\\"path\\\":\\\"\\xc3\\xa9\\xc2\\x9d\\\"}')"]
        with mock.patch.object(subprocess,"check_output",side_effect=cp1252_default):
            self.assertEqual(json.loads(notices.run(command,self.root))["path"],"é\u009d")

    def test_failed_regeneration_preserves_the_previous_distribution(self):
        source, output, records = self.make_component(),self.root / "out",[]
        component = notices.copy_component(output,{"kind":"cargo","name":"example","version":"1","license":"MIT"},source,records)
        self.inventory(output,records,component)
        before = {path.relative_to(output):path.read_bytes() for path in output.rglob("*") if path.is_file()}
        client = self.root / "client"
        (client / "src-tauri").mkdir(parents=True)
        (self.root / "LICENSE").write_bytes(LICENSE)
        (client / "src-tauri/Cargo.lock").write_text('[[package]]\nname="tutor-client"\nversion="0.1.0"\n')
        with mock.patch.object(notices,"npm_packages",side_effect=ValueError("missing new component license")):
            with self.assertRaisesRegex(ValueError,"missing new component"):
                notices.collect(client,output,"x86_64-pc-windows-msvc",metadata={},rust_root=self.root,rust_version="fixture")
        after = {path.relative_to(output):path.read_bytes() for path in output.rglob("*") if path.is_file()}
        self.assertEqual(before,after)
        notices.verify_tree(output)

    def test_installer_tool_must_match_the_reviewed_runtime_stub(self):
        source, output, records = self.make_component(),self.root / "out",[]
        component = notices.copy_component(output,{"kind":"cargo","name":"example","version":"1","license":"MIT"},source,records)
        inventory = self.inventory(output,records,component)
        inventory["installerToolFiles"] = [{"file":"Stubs/zlib-x86-unicode","sha256":notices.digest(b"reviewed installer stub")}]
        (output / "inventory.json").write_text(json.dumps(inventory))
        cache = self.root / "NSIS"
        (cache / "Stubs").mkdir(parents=True)
        stub = cache / "Stubs/zlib-x86-unicode"
        stub.write_bytes(b"reviewed installer stub")
        notices.verify_installer_tools(output,cache)
        stub.write_bytes(b"different binary")
        with self.assertRaisesRegex(ValueError,"differs from its reviewed binary"):
            notices.verify_installer_tools(output,cache)

    def test_changed_installer_compression_or_cli_requires_a_new_review(self):
        client = self.root
        (client / "node_modules/@tauri-apps/cli").mkdir(parents=True)
        package = client / "node_modules/@tauri-apps/cli/package.json"
        package.write_text(json.dumps({"version":"2.12.4"}))
        (client / "src-tauri").mkdir()
        (client / "src-tauri/tauri.conf.json").write_text(json.dumps({"bundle":{"windows":{"nsis":{"compression":"lzma"}}}}))
        reviewed = {"installer":{"tauriCliVersion":"2.12.4","compression":"zlib","components":[]}}
        with self.assertRaisesRegex(ValueError,"compression"):
            notices.installer_components(client,reviewed,self.root,self.root / "out",[],[])
        package.write_text(json.dumps({"version":"2.13.0"}))
        with self.assertRaisesRegex(ValueError,"Tauri CLI changed"):
            notices.installer_components(client,reviewed,self.root,self.root / "out",[],[])


if __name__ == "__main__":
    unittest.main()
