#!/usr/bin/env python3
"""Build six portable archives and SHA256SUMS using the selected Go toolchain."""

import argparse
import hashlib
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import zipfile
from datetime import datetime, timezone


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
    checksums = []
    for goos in ('linux', 'darwin', 'windows'):
        for goarch in ('amd64', 'arm64'):
            with tempfile.TemporaryDirectory(prefix='tutor-release-') as temp:
                binary_name = 'tutor-mcp.exe' if goos == 'windows' else 'tutor-mcp'
                binary = Path(temp) / binary_name
                env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0')
                subprocess.run([go, 'build', '-trimpath', '-buildvcs=false', '-ldflags',
                                f'-s -w -X main.version={args.version} -X main.commit={commit} -X main.buildDate={build_date}',
                                '-o', str(binary), '.'], cwd=root, env=env, check=True)
                suffix = 'zip' if goos == 'windows' else 'tar.gz'
                asset = output / f'tutor-mcp_{goos}_{goarch}.{suffix}'
                if suffix == 'zip':
                    with zipfile.ZipFile(asset, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
                        for source in (binary, root / 'LICENSE'):
                            info = zipfile.ZipInfo(source.name, datetime.fromtimestamp(max(epoch, 315532800), timezone.utc).timetuple()[:6])
                            info.compress_type = zipfile.ZIP_DEFLATED
                            info.external_attr = (0o755 if source == binary else 0o644) << 16
                            archive.writestr(info, source.read_bytes())
                else:
                    # Gzip and tar timestamps come from the source commit.
                    import gzip
                    with asset.open('wb') as raw, gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=epoch) as compressed:
                        with tarfile.open(fileobj=compressed, mode='w') as archive:
                            for source in (binary, root / 'LICENSE'):
                                info = archive.gettarinfo(str(source), arcname=source.name)
                                info.uid = info.gid = 0
                                info.uname = info.gname = ''
                                info.mtime = epoch
                                info.mode = 0o755 if source == binary else 0o644
                                with source.open('rb') as data:
                                    archive.addfile(info, data)
                checksums.append(f'{hashlib.sha256(asset.read_bytes()).hexdigest()}  {asset.name}\n')
                print(asset.name, flush=True)
    (output / 'SHA256SUMS').write_text(''.join(sorted(checksums)), encoding='ascii')


if __name__ == '__main__':
    main()
