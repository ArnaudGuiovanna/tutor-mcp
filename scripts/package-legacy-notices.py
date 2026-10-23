#!/usr/bin/env python3
"""Prepare supplemental notices for unchanged, checksum-verified release binaries.

Nothing is uploaded. Use a clean export of the release's source commit and its
exact Go toolchain. Existing release assets and SHA256SUMS are left unchanged.
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import zipfile

from third_party_notices import collect, run_json, verify_contents


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--source-commit', required=True)
    parser.add_argument('--build-date', required=True, help='exact UTC build date reported by the released binary')
    parser.add_argument('--source', required=True, type=Path)
    parser.add_argument('--archives', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    if (not re.fullmatch(r'v\d+\.\d+\.\d+', args.version)
            or not re.fullmatch(r'[0-9a-f]{40}', args.source_commit)
            or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', args.build_date)):
        parser.error('an exact release version and source commit are required')
    go = os.environ.get('TUTOR_GO_BIN', 'go')
    sums = {}
    for line in (args.archives / 'SHA256SUMS').read_text().splitlines():
        match = re.fullmatch(r'([0-9a-fA-F]{64})\s+\*?(\S+)', line)
        if not match or match[2] in sums:
            raise ValueError('invalid or duplicate original release checksum')
        sums[match[2]] = match[1].lower()
    args.output.mkdir(parents=True, exist_ok=True)
    supplement_sums, report = [], []
    for goos in ('linux', 'darwin', 'windows'):
        for goarch in ('amd64', 'arm64'):
            suffix = 'zip' if goos == 'windows' else 'tar.gz'
            name = f'tutor-mcp_{goos}_{goarch}.{suffix}'
            archive_path = args.archives / name
            original_hash = hashlib.sha256(archive_path.read_bytes()).hexdigest()
            if sums.get(name) != original_hash:
                raise ValueError(f'original release checksum mismatch: {name}')
            binary_name = 'tutor-mcp.exe' if goos == 'windows' else 'tutor-mcp'
            if goos == 'windows':
                with zipfile.ZipFile(archive_path) as original:
                    binary_data = original.read(binary_name)
            else:
                with tarfile.open(archive_path, 'r:gz') as original:
                    member = original.getmember(binary_name)
                    if not member.isfile():
                        raise ValueError(f'original binary is not a regular file: {name}')
                    binary_data = original.extractfile(member).read()
            with tempfile.TemporaryDirectory(prefix='tutor-legacy-notices-') as temporary:
                temporary = Path(temporary)
                binary = temporary / binary_name
                binary.write_bytes(binary_data)
                info, = run_json(go, ['version', '-m', '-json', str(binary)], args.source)
                settings = {item['Key']: item['Value'] for item in info['Settings']}
                if settings.get('GOOS') != goos or settings.get('GOARCH') != goarch:
                    raise ValueError(f'binary does not match the source release and target: {name}')
                # Trimmed Go binaries omit their -ldflags build setting. Rebuild
                # the exported source with the original metadata and require an
                # exact byte match, rather than guessing provenance from strings.
                rebuilt = temporary / ('rebuilt-' + binary_name)
                subprocess.run([go, 'build', '-trimpath', '-buildvcs=false', '-ldflags',
                                f'-s -w -X main.version={args.version} -X main.commit={args.source_commit} -X main.buildDate={args.build_date}',
                                '-o', str(rebuilt), '.'], cwd=args.source,
                               env=dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0'), check=True)
                if hashlib.sha256(rebuilt.read_bytes()).digest() != hashlib.sha256(binary_data).digest():
                    raise ValueError(f'rebuilt binary differs from the original release: {name}')
                notices = temporary / 'THIRD_PARTY_NOTICES'
                inventory = collect(binary, notices, args.source, go, settings.get('-tags', ''))
                inventory['release_version'] = args.version
                inventory['source_commit'] = args.source_commit
                inventory['original_archive_sha256'] = original_hash
                (notices / 'inventory.json').write_text(json.dumps(inventory, indent=2, sort_keys=True) + '\n')
                files = {'LICENSE': (args.source / 'LICENSE').read_bytes()}
                files.update({'THIRD_PARTY_NOTICES/' + path.relative_to(notices).as_posix(): path.read_bytes()
                              for path in notices.rglob('*') if path.is_file()})
                supplement = args.output / f'tutor-mcp_{goos}_{goarch}_notices.{suffix}'
                if supplement.exists():
                    raise ValueError(f'refusing to overwrite a prepared supplement: {supplement}')
                if goos == 'windows':
                    with zipfile.ZipFile(supplement, 'w', compression=zipfile.ZIP_DEFLATED) as target:
                        for filename, data in sorted(files.items()):
                            entry = zipfile.ZipInfo(filename, (1980, 1, 1, 0, 0, 0))
                            entry.compress_type = zipfile.ZIP_DEFLATED
                            entry.external_attr = 0o644 << 16
                            target.writestr(entry, data)
                    with zipfile.ZipFile(supplement) as target:
                        verify_contents(lambda key: binary_data if key == binary_name else target.read(key), binary_name)
                else:
                    import gzip
                    with supplement.open('wb') as raw, gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as compressed:
                        with tarfile.open(fileobj=compressed, mode='w') as target:
                            for filename, data in sorted(files.items()):
                                entry = tarfile.TarInfo(filename)
                                entry.size, entry.mode = len(data), 0o644
                                target.addfile(entry, io.BytesIO(data))
                    with tarfile.open(supplement, 'r:gz') as target:
                        verify_contents(lambda key: binary_data if key == binary_name else target.extractfile(key).read(), binary_name)
                supplement_sums.append(f'{hashlib.sha256(supplement.read_bytes()).hexdigest()}  {supplement.name}\n')
                report.append({'original_archive': name, 'original_archive_sha256': original_hash,
                               'supplement': supplement.name, 'binary_sha256': inventory['binary_sha256'],
                               'go_version': info['GoVersion'], 'module_count': len(info.get('Deps', ())),
                               'source_commit': args.source_commit, 'version': args.version,
                               'build_date': args.build_date, 'exact_rebuild_verified': True})
                print(supplement.name, flush=True)
    for filename, text in [('THIRD_PARTY_NOTICES_SHA256SUMS', ''.join(sorted(supplement_sums))),
                           ('THIRD_PARTY_NOTICES_PROVENANCE.json', json.dumps(report, indent=2, sort_keys=True) + '\n')]:
        path = args.output / filename
        if path.exists():
            raise ValueError(f'refusing to overwrite a prepared supplement: {path}')
        path.write_text(text, encoding='utf-8')


if __name__ == '__main__':
    main()
