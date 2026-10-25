#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Preserve original distribution notices for the Windows Tutor client.

Cargo's target-filtered, reachable desktop graph is a declared superset: build
and development dependencies may appear too. This is not an SPDX-to-text
generator, an exact linker map, or a license approval classifier.
"""

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tempfile
import tomllib


FORMAT = "tutor-client-notices-v1"
LEGAL_NAME = re.compile(r"licen[cs]e|copying|copyright|notice|authors|patents", re.I)
PRIMARY_NAME = re.compile(r"licen[cs]e|copying", re.I)
LEGAL_WORD = re.compile(rb"copyright|licen[cs]e|redistribution|permission is hereby|public domain", re.I)
COMMENT = re.compile(rb"/\*[\s\S]*?\*/|//[^\r\n]*(?:\r?\n[ \t]*//[^\r\n]*)*")
SOURCE_SUFFIXES = {".rs", ".c", ".h", ".cc", ".cpp", ".hpp", ".s", ".js", ".mjs", ".cjs"}
TARGETS = {"x86_64-pc-windows-msvc": "x64", "aarch64-pc-windows-msvc": "arm64", "i686-pc-windows-msvc": "x86"}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def run(args, cwd):
    # Cargo, rustc and npm metadata use UTF-8 regardless of Windows' code page.
    return subprocess.check_output(args, cwd=cwd, text=True, encoding="utf-8")


def safe_path(value):
    path = PurePosixPath(value)
    if not value or path.is_absolute() or ".." in path.parts or "\\" in value or ":" in value:
        raise ValueError(f"unsafe inventory path: {value!r}")
    return path


def put(output, relative, data, records):
    path = output.joinpath(*safe_path(relative).parts)
    if path.exists():
        raise ValueError(f"duplicate notice destination: {relative}")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    records.append({"file": relative, "sha256": digest(data), "bytes": len(data)})
    return relative


def original_files(root):
    result = []
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            # Cargo archives do not require symlink traversal. A changed cache
            # must not make us copy a file from outside the package.
            if LEGAL_NAME.search(path.name):
                raise ValueError(f"symlinked legal file: {path}")
            continue
        if path.is_file() and path.suffix.lower() not in SOURCE_SUFFIXES and LEGAL_NAME.search(path.name):
            result.append(path)
    return result


def source_notices(root):
    notices = {}
    for path in sorted(root.rglob("*")):
        if not path.is_file() or path.is_symlink() or path.suffix.lower() not in SOURCE_SUFFIXES:
            continue
        for match in COMMENT.finditer(path.read_bytes()):
            comment = match.group()
            if LEGAL_WORD.search(comment):
                entry = notices.setdefault(comment, [])
                entry.append(path.relative_to(root).as_posix())
    # Original comment bytes are copied without rewrapping or translating.
    return b"\n\n".join(b"Source: " + ", ".join(paths).encode("utf-8") + b"\n" + comment
                          for comment, paths in notices.items())


def supplement_records(root, key, package, supplements):
    record = supplements["components"].get(key)
    if record is None:
        return []
    vcs_path = root / ".cargo_vcs_info.json"
    if not vcs_path.is_file() or json.loads(vcs_path.read_text(encoding="utf-8")).get("git", {}).get("sha1") != record["vcsCommit"]:
        raise ValueError(f"supplement does not match Cargo's source commit: {key}")
    if package.get("license") == "MPL-2.0" and key.startswith("selectors@"):
        # This exact archive's own header identifies the Mozilla text. The
        # supplemental text comes from that steward, not an SPDX template.
        header = (root / "lib.rs").read_bytes()[:512]
        if b"Mozilla Public" not in header or b"https://mozilla.org/MPL/2.0/" not in header:
            raise ValueError("selectors no longer references the reviewed MPL text")
    return record["files"]


def copy_component(output, component, root, records, *, supplement_root=None,
                   supplements=(), extra_files=(), scan_original=True,
                   scan_source=True):
    name = component["name"].replace("/", "__").replace("@", "")
    directory = f"components/{component['kind']}/{name}@{component['version']}"
    files = original_files(root) if scan_original else []
    for path in extra_files:
        if not path.is_file() or not path.resolve().is_relative_to(root.resolve()):
            raise ValueError(f"missing or escaped original legal file: {path}")
        if path not in files:
            files.append(path)
    meaningful_primary = any((PRIMARY_NAME.search(path.name) or path in extra_files)
                             and path.suffix.lower() != ".spdx"
                             and len(path.read_bytes().strip()) >= 100 for path in files)
    component["notices"] = []
    for path in sorted(files):
        relative = path.relative_to(root).as_posix()
        destination = put(output, f"{directory}/{relative}", path.read_bytes(), records)
        component["notices"].append({"source": relative, "file": destination})
    for item in supplements:
        relative = safe_path(item["file"])
        source = supplement_root.joinpath(*relative.parts)
        data = source.read_bytes()
        if not data.strip() or digest(data) != item["sha256"]:
            raise ValueError(f"missing, empty or changed reviewed supplement: {source}")
        destination = put(output, f"{directory}/upstream/{relative.name}", data, records)
        component["notices"].append({"source": item["source"], "file": destination})
        meaningful_primary |= bool(PRIMARY_NAME.search(relative.name))
    if not meaningful_primary:
        raise ValueError(f"no original license text for {component['kind']} {component['name']}@{component['version']}")
    comments = source_notices(root) if scan_source else b""
    if comments:
        destination = put(output, f"{directory}/SOURCE-NOTICES.txt", comments, records)
        component["notices"].append({"source": "original source comments (deduplicated)", "file": destination})
    return component


def reachable_packages(metadata):
    resolution = metadata.get("resolve") or {}
    root = resolution.get("root")
    nodes = {node["id"]: node for node in resolution.get("nodes", [])}
    if root not in nodes:
        raise ValueError("Cargo metadata does not identify the client root")
    seen, stack = set(), [root]
    while stack:
        current = stack.pop()
        if current in seen:
            continue
        if current not in nodes:
            raise ValueError(f"incomplete Cargo graph: {current}")
        seen.add(current)
        stack.extend(dependency["pkg"] for dependency in nodes[current].get("deps", []))
    packages = [package for package in metadata["packages"] if package["id"] in seen and package["id"] != root]
    if len(packages) != len(seen) - 1:
        raise ValueError("Cargo graph contains packages without metadata")
    return sorted(packages, key=lambda package: (package["name"], package["version"], package["id"]))


def npm_packages(client):
    lock = json.loads((client / "package-lock.json").read_text(encoding="utf-8"))
    packages = lock["packages"]
    todo = [("", name) for name in packages[""].get("dependencies", {})]
    # Vite's generated modulepreload helper is included in the shipped frontend.
    todo.append(("", "vite"))
    seen = set()
    result = []
    while todo:
        parent, name = todo.pop()
        candidates = []
        ancestor = parent
        while ancestor:
            candidates.append(f"{ancestor}/node_modules/{name}")
            ancestor = ancestor.rpartition("/node_modules/")[0]
        candidates.append(f"node_modules/{name}")
        key = next((candidate for candidate in candidates if candidate in packages), None)
        if key is None:
            raise ValueError(f"missing locked npm dependency: {name}")
        if key in seen:
            continue
        seen.add(key)
        locked = packages[key]
        root = client.joinpath(*safe_path(key).parts)
        package = json.loads((root / "package.json").read_text(encoding="utf-8"))
        if package.get("version") != locked.get("version") or package.get("name") != name:
            raise ValueError(f"installed npm package differs from its lock: {name}")
        if not locked.get("integrity") or locked.get("link"):
            raise ValueError(f"npm dependency has no immutable archive integrity: {name}")
        result.append(({"kind":"npm", "name":name, "version":package["version"],
                        "license":package.get("license"), "integrity":locked["integrity"],
                        "scope":"modulepreload helper and original Vite notices" if name == "vite" else "runtime dependency"}, root))
        if name != "vite":
            todo.extend((key, child) for child in locked.get("dependencies", {}))
    return sorted(result, key=lambda item: (item[0]["name"], item[0]["version"]))


def installer_components(client, supplements, supplement_root, output, files, cargo_components):
    installer = supplements["installer"]
    cli = json.loads((client / "node_modules/@tauri-apps/cli/package.json").read_text(encoding="utf-8"))
    if cli.get("version") != installer["tauriCliVersion"]:
        raise ValueError("Tauri CLI changed: review installer notices and tool hashes")
    config = json.loads((client / "src-tauri/tauri.conf.json").read_text(encoding="utf-8"))
    bundle = config.get("bundle",{})
    windows = bundle.get("windows",{})
    if windows.get("nsis",{}).get("compression") != installer["compression"]:
        raise ValueError("use the reviewed explicit zlib NSIS compression, or review its additional source/license requirements")
    if windows.get("nsis",{}).get("template") or bundle.get("externalBin"):
        raise ValueError("custom installer templates or embedded executables require an updated notice inventory")
    if any(windows.get(key,False) for key in ["bundleVCRuntime","bundleVcRuntime","bundle-vc-runtime"]):
        raise ValueError("bundled Visual C++ runtime requires a reviewed redistributable inventory")
    if windows.get("webviewInstallMode",{}).get("type","downloadBootstrapper") not in {"downloadBootstrapper","skip"}:
        raise ValueError("embedded WebView2 installers or runtimes require an updated redistributable inventory")
    result = []
    by_name = {component["name"]:component for component in cargo_components}
    for reviewed in installer["components"]:
        root = supplement_root / f"{reviewed['name'].lower()}-{reviewed['version']}"
        for item in reviewed["files"]:
            if digest((supplement_root / item["file"]).read_bytes()) != item["sha256"]:
                raise ValueError(f"changed installer legal text: {item['file']}")
        component = {"kind":"installer","name":reviewed["name"],"version":reviewed["version"],
            "source":reviewed["source"],"prebuiltSha256":reviewed["binarySha256"],
            "license":"original upstream installer license texts",
            "tauriCliVersion":installer["tauriCliVersion"],"compression":installer["compression"]}
        if reviewed["name"] == "nsis-tauri-utils":
            # The upstream prebuilt plugin publishes its source commit but no
            # dependency lock. Do not invent exact versions for that DLL. Its
            # runtime dependency families' original terms are already retained.
            for dependency in ["windows-sys","semver"]:
                if dependency not in by_name:
                    raise ValueError(f"missing original installer dependency terms: {dependency}")
            component["additionalDependencyNotices"] = [
                {"name":dependency,"files":[entry["file"] for entry in by_name[dependency]["notices"]]}
                for dependency in ["windows-sys","semver"]]
            component["dependencyVersionScope"] = "upstream prebuilt DLL; source commit and binary hash pinned, no upstream dependency lock published"
        result.append(copy_component(output,component,root,files))
    return result


def bundled_components(client, output, files):
    root = client / 'bundled-agents'
    manifest = json.loads((root / 'runtime-manifest.json').read_text(encoding='utf-8'))
    if manifest.get('format') != 1 or manifest.get('platform') != 'windows-x64':
        raise ValueError('unsupported bundled-agent inventory')
    for asset in manifest['files']:
        path = root.joinpath(*safe_path(asset['path']).parts)
        if path.is_symlink() or not path.is_file() or not path.resolve().is_relative_to(root.resolve()) or path.stat().st_size != asset['bytes'] or digest(path.read_bytes()) != asset['sha256']:
            raise ValueError(f"modified bundled resource: {asset['path']}")
    locked = json.loads((root / 'package-lock.json').read_text(encoding='utf-8'))['packages']
    result, seen = [], set()
    for key, entry in locked.items():
        if not key:
            continue
        directory = root.joinpath(*safe_path(key).parts)
        if not directory.is_dir():  # Optional runtimes for other platforms.
            continue
        package = json.loads((directory / 'package.json').read_text(encoding='utf-8'))
        if package['version'] != entry['version'] or not entry.get('integrity'):
            raise ValueError(f'changed locked agent package: {key}')
        identity = (package['name'],package['version'])
        if identity in seen:
            continue
        seen.add(identity)
        component = {'kind':'agent-npm','name':package['name'],'version':package['version'], 'license':package.get('license'), 'integrity':entry['integrity'], 'scope':'unmodified bundled ACP agent/runtime'}
        # The SDK's own license points to the terms in its original README.
        extra = [directory / 'README.md'] if package['name'] == '@anthropic-ai/claude-agent-sdk' else []
        supplement_root=Path(__file__).resolve().parent/'notices-supplements'
        supplements=[]
        if package['name']=='@openai/codex':
            provenance=json.loads((supplement_root/'agent-provenance.json').read_text(encoding='utf-8'))
            if package['version'] not in {provenance['codexVersion'],provenance['codexVersion']+'-win32-x64'}:
                raise ValueError('Codex changed: review original vendor notices')
            component['vendorSourceCommit']=provenance['codexCommit']
            component['binaryScope']='vendor prebuilt, not a reconstructed linker map; optional voice runtime excluded'
            supplements=provenance['files']
        elif package['name']=='standardwebhooks':
            provenance=json.loads((supplement_root/'agent-provenance.json').read_text(encoding='utf-8'))['standardwebhooks']
            if package['version']!=provenance['version']:
                raise ValueError('standardwebhooks changed: review upstream original license')
            supplements=provenance['files']
        result.append(copy_component(output,component,directory,files,extra_files=extra,supplement_root=supplement_root,supplements=supplements))
    node = {'kind':'bundled-runtime','name':'Node.js','version':manifest['nodeVersion'],'license':'original Node.js LICENSE including bundled dependency notices','source':manifest['nodeSource'],'archiveSha256':manifest['nodeArchiveSha256']}
    result.append(copy_component(output,node,root / 'node',files,scan_source=False))
    return result


def collect(client, output, target, *, metadata=None, cargo="cargo", rustc="rustc",
            rust_root=None, rust_version=None):
    client, output = Path(client).resolve(), Path(output).resolve()
    if target not in TARGETS:
        raise ValueError(f"unsupported notice target: {target}")
    if output.exists() and any(output.iterdir()):
        verify_tree(output)  # Never replace an unrelated or modified directory.
    supplement_root = Path(__file__).resolve().parent / "notices-supplements"
    supplements = json.loads((supplement_root / "provenance.json").read_text(encoding="utf-8"))
    if metadata is None:
        metadata = json.loads(run([cargo,"metadata","--manifest-path",str(client / "src-tauri/Cargo.toml"),
            "--format-version","1","--locked","--offline","--filter-platform",target,
            "--no-default-features","--features","desktop"], client))
    rust_root = Path(rust_root) if rust_root is not None else Path(run([rustc,"--print","sysroot"],client).strip()) / "share/doc/rust"
    rust_version = rust_version or run([rustc,"--version"],client).strip()
    cargo_lock = tomllib.loads((client / "src-tauri/Cargo.lock").read_text(encoding="utf-8"))
    checksums = {(package["name"],package["version"]):package.get("checksum") for package in cargo_lock["package"]}
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".client-notices-",dir=output.parent) as staging:
        staging = Path(staging)
        files, components = [], []
        project_license = client.parent / "LICENSE"
        if not project_license.is_file() or not project_license.read_bytes().strip():
            raise ValueError("missing original project LICENSE")
        put(staging,"LICENSE",project_license.read_bytes(),files)
        for component, root in npm_packages(client):
            components.append(copy_component(staging,component,root,files))
        components.extend(bundled_components(client,staging,files))
        packages = reachable_packages(metadata)
        for package in packages:
            root = Path(package["manifest_path"]).parent
            component = {"kind":"cargo","name":package["name"],"version":package["version"],
                "license":package.get("license"),"source":package.get("source"),
                "archiveSha256":checksums.get((package["name"],package["version"])),
                "features":next(node["features"] for node in metadata["resolve"]["nodes"] if node["id"] == package["id"])}
            if not component["license"] and not package.get("license_file"):
                raise ValueError(f"Cargo component has no license declaration: {package['name']}")
            key = f"{package['name']}@{package['version']}"
            extra = [root / package["license_file"]] if package.get("license_file") else []
            reviewed = supplement_records(root,key,package,supplements)
            component = copy_component(staging,component,root,files,supplement_root=supplement_root,
                                       supplements=reviewed,extra_files=extra)
            if "MPL-2.0" in (package.get("license") or ""):
                archive = root.parent.parent.parent / "cache" / root.parent.name / f"{package['name']}-{package['version']}.crate"
                data = archive.read_bytes()
                if not component["archiveSha256"] or digest(data) != component["archiveSha256"]:
                    raise ValueError(f"missing or changed corresponding source archive: {key}")
                component["correspondingSource"] = put(staging,f"sources/{archive.name}",data,files)
            components.append(component)
        sdk = supplements["microsoftWebView2Sdk"]
        loader_package = next((package for package in packages if f"{package['name']}@{package['version']}" == sdk["crate"]),None)
        if loader_package is None:
            raise ValueError("native WebView2 loader requires a reviewed SDK provenance update")
        loader = Path(loader_package["manifest_path"]).parent / TARGETS[target] / "WebView2LoaderStatic.lib"
        if digest(loader.read_bytes()) != sdk["loaders"][TARGETS[target]]:
            raise ValueError("WebView2 static loader differs from its reviewed Microsoft SDK")
        native = {"kind":"native-sdk","name":"Microsoft.Web.WebView2","version":sdk["version"],
            "source":sdk["source"],"archiveSha256":sdk["archiveSha256"],
            "loaderSha256":sdk["loaders"][TARGETS[target]],"license":"original Microsoft SDK LICENSE.txt and NOTICE.txt"}
        # The supplement directory contains the exact SDK legal files, whose
        # hashes are verified by copy_component before they enter the artifact.
        native_root = supplement_root / f"microsoft-webview2-sdk-{sdk['version']}"
        components.append(copy_component(staging,native,native_root,files))
        for item in sdk["files"]:
            if digest((supplement_root / item["file"]).read_bytes()) != item["sha256"]:
                raise ValueError("changed Microsoft SDK notice supplement")
        required_rust = [rust_root / "COPYRIGHT-library.html",rust_root / "licenses/MIT.txt",rust_root / "licenses/Apache-2.0.txt"]
        rust = {"kind":"toolchain","name":"Rust standard library","version":rust_version,
                "license":"original Rust standard-library copyright inventory and accompanying license texts"}
        components.append(copy_component(staging,rust,rust_root,files,
            extra_files=required_rust + list((rust_root / "licenses").glob("*")),
            scan_original=False,scan_source=False))
        components.extend(installer_components(client,supplements,supplement_root,staging,files,components))
        inputs = []
        for path in [client / "package.json",client / "package-lock.json",client / "src-tauri/Cargo.toml",client / "src-tauri/Cargo.lock",client / "src-tauri/tauri.conf.json",client / "scripts/adapter/package-lock.json",client / "bundled-agents/runtime-manifest.json"] + sorted((client / "dist").rglob("*")):
            if path.is_file():
                inputs.append({"file":path.relative_to(client).as_posix(),"sha256":digest(path.read_bytes())})
        readme = ("Tutor client third-party notices\n\nOriginal license/notice files and legal source comments are retained.\n"
            "Cargo coverage is the reachable target-filtered desktop graph, including a build/dev superset.\n"
            "MPL-2.0 corresponding source archives are included under sources/.\n"
            "Codex and Claude ACP adapters, their compatible vendor runtimes and Node.js are embedded unmodified.\n"
            "Anthropic SDK/runtime terms are proprietary; retain the original LICENSE and SDK README. Provider authentication remains with the provider.\n"
            "WebView2 Runtime is supplied by Microsoft; the linked SDK loader's original terms are included.\n")
        put(staging,"README.txt",readme.encode("utf-8"),files)
        inventory = {"format":FORMAT,"target":target,"coverage":"reachable Cargo desktop graph including build/dev superset; npm runtime graph plus Vite helper; bundled ACP npm graphs and vendor runtimes; Node.js original terms; Rust std; native WebView2 SDK loader; NSIS installer and Tauri plugin",
            "rootCargoFeatures":next(node["features"] for node in metadata["resolve"]["nodes"] if node["id"] == metadata["resolve"]["root"]),
            "installerToolFiles":supplements["installer"]["toolFiles"],
            "inputs":inputs,"components":components,"files":sorted(files,key=lambda item:item["file"])}
        (staging / "inventory.json").write_text(json.dumps(inventory,indent=2,ensure_ascii=False)+"\n",encoding="utf-8")
        verify_tree(staging)
        if output.exists():
            shutil.rmtree(output)  # Its entire previous generated inventory was verified above.
        shutil.copytree(staging,output)
    return inventory


def verify_tree(output):
    output = Path(output)
    inventory = json.loads((output / "inventory.json").read_text(encoding="utf-8"))
    if inventory.get("format") != FORMAT or inventory.get("target") not in TARGETS:
        raise ValueError("unknown notice inventory format or target")
    expected = {"inventory.json"}
    for record in inventory["files"]:
        relative = safe_path(record["file"])
        if relative.as_posix() in expected:
            raise ValueError("duplicate inventory file")
        expected.add(relative.as_posix())
        path = output.joinpath(*relative.parts)
        if path.is_symlink() or not path.is_file() or not path.resolve().is_relative_to(output.resolve()):
            raise ValueError(f"missing or escaped notice: {relative}")
        data = path.read_bytes()
        if digest(data) != record["sha256"] or len(data) != record["bytes"]:
            raise ValueError(f"changed notice: {relative}")
    actual = {path.relative_to(output).as_posix() for path in output.rglob("*") if path.is_file() or path.is_symlink()}
    if actual != expected:
        raise ValueError("notice directory contains files absent from its inventory")
    if not inventory.get("components") or "LICENSE" not in expected:
        raise ValueError("notice inventory omits its components or project license")
    for component in inventory["components"]:
        if not component.get("notices") or any(item["file"] not in expected for item in component["notices"]):
            raise ValueError("component has no inventoried legal notices")
        if "MPL-2.0" in (component.get("license") or "") and component.get("correspondingSource") not in expected:
            raise ValueError("MPL component has no corresponding source archive")
    return inventory


def verify_installer_tools(inventory_directory, cache):
    inventory = verify_tree(inventory_directory)
    cache = Path(cache).resolve()
    for item in inventory["installerToolFiles"]:
        path = cache.joinpath(*safe_path(item["file"]).parts)
        if not path.is_file() or not path.resolve().is_relative_to(cache) or digest(path.read_bytes()) != item["sha256"]:
            raise ValueError(f"installer tool differs from its reviewed binary: {item['file']}")
    return inventory


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command",required=True)
    build = sub.add_parser("collect")
    build.add_argument("--client",type=Path,default=Path(__file__).resolve().parents[1])
    build.add_argument("--output",type=Path)
    build.add_argument("--target",default="x86_64-pc-windows-msvc",choices=TARGETS)
    build.add_argument("--cargo",default="cargo")
    build.add_argument("--rustc",default="rustc")
    verify = sub.add_parser("verify-tree")
    verify.add_argument("path",type=Path)
    tools = sub.add_parser("verify-installer-tools")
    tools.add_argument("--inventory",type=Path,required=True)
    tools.add_argument("--cache",type=Path,required=True)
    args = parser.parse_args()
    try:
        if args.command == "collect":
            inventory = collect(args.client,args.output or args.client / "THIRD_PARTY_NOTICES",args.target,cargo=args.cargo,rustc=args.rustc)
        elif args.command == "verify-installer-tools":
            inventory = verify_installer_tools(args.inventory,args.cache)
        else:
            inventory = verify_tree(args.path)
    except (ValueError,OSError,KeyError,subprocess.CalledProcessError) as error:
        parser.exit(1,f"Client notice verification failed: {error}\n")
    print(json.dumps({"target":inventory["target"],"components":len(inventory["components"]),"files":len(inventory["files"])}))


if __name__ == "__main__":
    main()
