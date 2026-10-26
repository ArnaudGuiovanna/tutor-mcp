#!/usr/bin/env python3
"""Build six portable archives and SHA256SUMS using the selected Go toolchain."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import zipfile
from datetime import datetime, timezone

from third_party_notices import collect, verify_archive, verify_contents


def archive_files(binary, root, notices):
    return [(binary, binary.name), (root / 'LICENSE', 'LICENSE')] + [
        (path, 'THIRD_PARTY_NOTICES/' + path.relative_to(notices).as_posix())
        for path in sorted(notices.rglob('*')) if path.is_file()]


def write_archive(asset, files, epoch, executable=None):
    """Use the source timestamp and fixed permissions for both asset families."""
    if asset.suffix == '.zip':
        with zipfile.ZipFile(asset, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
            for source, name in files:
                info = zipfile.ZipInfo(name, datetime.fromtimestamp(max(epoch, 315532800), timezone.utc).timetuple()[:6])
                info.compress_type = zipfile.ZIP_DEFLATED
                info.external_attr = (0o755 if source == executable else 0o644) << 16
                archive.writestr(info, source.read_bytes())
    else:
        import gzip
        with asset.open('wb') as raw, gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode='w') as archive:
                for source, name in files:
                    info = archive.gettarinfo(str(source), arcname=name)
                    info.uid = info.gid = 0
                    info.uname = info.gname = ''
                    info.mtime = epoch
                    info.mode = 0o755 if source == executable else 0o644
                    with source.open('rb') as data:
                        archive.addfile(info, data)


def write_notice_supplement(asset, binary, root, notices, epoch):
    files = archive_files(binary, root, notices)[1:]
    write_archive(asset, files, epoch)
    binary_data = binary.read_bytes()
    if asset.suffix == '.zip':
        with zipfile.ZipFile(asset) as archive:
            return verify_contents(lambda name: binary_data if name == binary.name else archive.read(name), binary.name)
    with tarfile.open(asset, 'r:gz') as archive:
        def read(name):
            if name == binary.name:
                return binary_data
            member = archive.getmember(name)
            if not member.isfile():
                raise ValueError('notice supplement contains a non-regular file')
            return archive.extractfile(member).read()
        return verify_contents(read, binary.name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--output', default='dist')
    args = parser.parse_args()
    if not re.fullmatch(r'v\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?', args.version):
        parser.error('version must be a v-prefixed semantic version')
    root = Path(__file__).resolve().parent.parent
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
    epoch = int(os.environ.get('SOURCE_DATE_EPOCH') or subprocess.check_output(
        ['git', 'show', '-s', '--format=%ct', 'HEAD'], cwd=root, text=True).strip())
    build_date = datetime.fromtimestamp(epoch, timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    go = os.environ.get('TUTOR_GO_BIN', 'go')
    checksums, notice_checksums, provenance = [], [], []
    for goos in ('linux', 'darwin', 'windows'):
        for goarch in ('amd64', 'arm64'):
            with tempfile.TemporaryDirectory(prefix='tutor-release-') as temp:
                binary_name = 'tutor-mcp.exe' if goos == 'windows' else 'tutor-mcp'
                binary = Path(temp) / binary_name
                env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0')
                subprocess.run([go, 'build', '-trimpath', '-buildvcs=false', '-ldflags',
                                f'-s -w -X main.version={args.version} -X main.commit={commit} -X main.buildDate={build_date}',
                                '-o', str(binary), '.'], cwd=root, env=env, check=True)
                notices = Path(temp) / 'THIRD_PARTY_NOTICES'
                inventory = collect(binary, notices, root, go)
                inventory.update(release_version=args.version, source_commit=commit)
                (notices / 'inventory.json').write_text(json.dumps(inventory, indent=2, sort_keys=True) + '\n', encoding='utf-8')
                files = archive_files(binary, root, notices)
                suffix = 'zip' if goos == 'windows' else 'tar.gz'
                asset = output / f'tutor-mcp_{goos}_{goarch}.{suffix}'
                write_archive(asset, files, epoch, binary)
                verify_archive(asset)
                archive_hash = hashlib.sha256(asset.read_bytes()).hexdigest()
                checksums.append(f'{archive_hash}  {asset.name}\n')
                supplement = output / f'tutor-mcp_{goos}_{goarch}_notices.{suffix}'
                write_notice_supplement(supplement, binary, root, notices, epoch)
                supplement_hash = hashlib.sha256(supplement.read_bytes()).hexdigest()
                line = f'{supplement_hash}  {supplement.name}\n'
                checksums.append(line)
                notice_checksums.append(line)
                provenance.append({'version': args.version, 'source_commit': commit, 'build_date': build_date,
                                   'original_archive': asset.name, 'original_archive_sha256': archive_hash,
                                   'supplement': supplement.name, 'supplement_sha256': supplement_hash,
                                   'binary_sha256': inventory['binary_sha256'],
                                   'go_version': inventory['build_info']['GoVersion'],
                                   'notice_source': 'same verified notice payload as the binary archive'})
                print(asset.name, flush=True)
                print(supplement.name, flush=True)
    for name, content in [('THIRD_PARTY_NOTICES_SHA256SUMS', ''.join(sorted(notice_checksums))),
                          ('THIRD_PARTY_NOTICES_PROVENANCE.json', json.dumps(provenance, indent=2, sort_keys=True) + '\n')]:
        path = output / name
        path.write_text(content, encoding='utf-8')
        checksums.append(f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {name}\n')
    (output / 'SHA256SUMS').write_text(''.join(sorted(checksums)), encoding='ascii')


if __name__ == '__main__':
    main()
