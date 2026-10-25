#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Build the self-contained Windows x64 ACP resources from immutable inputs.

Vendor packages and binaries are unmodified; all legal files remain in place.
No provider credentials are read. Generated resources are never source files.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import urllib.request
import zipfile

NODE_VERSION = "24.13.0"
NODE_SHA256 = "ca2742695be8de44027d71b3f53a4bdb36009b95575fe1ae6f7f0b5ce091cb88"
NODE_URL = f"https://nodejs.org/dist/v{NODE_VERSION}/node-v{NODE_VERSION}-win-x64.zip"
AGENTS = {
    "codex": {"label": "Codex", "package": "@agentclientprotocol/codex-acp", "version": "2.2.2"},
    "claude": {"label": "Claude", "package": "@agentclientprotocol/claude-agent-acp", "version": "0.95.0"},
}

def sha(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()

def prepare(client, archive):
    output = client / 'bundled-agents'
    if output.exists():
        raise ValueError('bundled-agents already exists; move the previous generated bundle before rebuilding')
    with tempfile.TemporaryDirectory(prefix='.agents-', dir=client) as temporary:
        root = Path(temporary)
        for name in ['package.json', 'package-lock.json']:
            shutil.copyfile(client / 'scripts/adapter' / name, root / name)
        npm = shutil.which('npm.cmd' if os.name == 'nt' else 'npm')
        if not npm:
            raise ValueError('npm is required at build time')
        subprocess.run([npm, 'ci', '--ignore-scripts', '--omit=dev', '--include=optional', '--os=win32', '--cpu=x64'], cwd=root, check=True)
        # Shims are unnecessary: Tutor launches the packaged Node and JS entry
        # directly. npm may create host-specific symlinks here on a cross build.
        for path in sorted(root.rglob('.bin'), reverse=True):
            if path.is_dir():
                shutil.rmtree(path)
        for path in root.rglob('*'):
            if path.is_symlink():
                raise ValueError(f'unexpected package symlink: {path.relative_to(root)}')
        # This text/MCP client has no native voice feature. Project out that
        # optional host and its GStreamer/GLib/VC++ DLLs; every retained vendor
        # executable stays byte-for-byte identical to the locked npm archive.
        voice=root/'node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/codex-resources/voice'
        if voice.is_dir():
            shutil.rmtree(voice)
        shutil.copytree(client/'scripts/notices-supplements/codex-0.160.1',root/'vendor-notices/codex')
        download = root / 'node.zip'
        if archive:
            shutil.copyfile(archive, download)
        else:
            with urllib.request.urlopen(NODE_URL, timeout=120) as response, download.open('wb') as dest:
                shutil.copyfileobj(response, dest)
        if sha(download) != NODE_SHA256:
            raise ValueError('Node archive does not match the pinned official SHA256')
        (root / 'node').mkdir()
        with zipfile.ZipFile(download) as package:
            for name in ['node.exe', 'LICENSE']:
                data = package.read(f'node-v{NODE_VERSION}-win-x64/{name}')
                (root / 'node' / name).write_bytes(data)
        download.unlink()
        agents = []
        for agent_id, agent in AGENTS.items():
            base = root / 'node_modules' / agent['package']
            info = json.loads((base / 'package.json').read_text(encoding='utf-8'))
            if info['version'] != agent['version']:
                raise ValueError(f'unreviewed {agent_id} adapter version')
            entry = base / info['bin'][next(iter(info['bin']))]
            if not entry.is_file():
                raise ValueError('missing agent entry')
            agents.append({'id': agent_id, 'label': agent['label'], 'version': agent['version'], 'entry': entry.relative_to(root).as_posix()})
        required = [
            'node_modules/@anthropic-ai/claude-agent-sdk-win32-x64/claude.exe',
            'node_modules/@openai/codex-win32-x64/package.json',
        ]
        for name in required:
            if not (root / name).is_file():
                raise ValueError(f'missing Windows runtime: {name}')
        files = [{'path': p.relative_to(root).as_posix(), 'bytes': p.stat().st_size, 'sha256': sha(p)} for p in sorted(root.rglob('*')) if p.is_file()]
        manifest = {'format': 1, 'platform': 'windows-x64', 'node': 'node/node.exe', 'nodeVersion': NODE_VERSION, 'nodeSource': NODE_URL, 'nodeArchiveSha256': NODE_SHA256, 'agents': agents, 'omittedOptionalComponents':['Codex native voice host and dynamic audio libraries'], 'files': files}
        (root / 'runtime-manifest.json').write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8')
        shutil.copytree(root, output)
    print(json.dumps({'agents': list(AGENTS), 'files': len(files), 'bytes': sum(f['bytes'] for f in files)}))

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--client', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--node-archive', type=Path)
    options = parser.parse_args()
    prepare(options.client.resolve(), options.node_archive)
