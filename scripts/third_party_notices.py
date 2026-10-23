#!/usr/bin/env python3
"""Preserve the actual build's third-party licence texts and source notices.

The inventory comes from the binary's Go build information and its matching
target package graph. Texts are copied verbatim from those exact module and Go
toolchain sources. This is an attribution collector, not a licence classifier.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import zipfile


LEGAL_NAME = re.compile(r'^(?:licen[cs]e|copying|copyright|notice|authors|patents)(?:$|[._-])', re.I)
LICENCE_NAME = re.compile(r'^(?:licen[cs]e|copying|copyright)(?:$|[._-])', re.I)
LEGAL_WORD = re.compile(r'copyright|licen[cs]e|redistribution|permission is hereby|public domain', re.I)
# Include complete comments, while skipping Go strings and rune literals. Some
# translated C libraries retain additional notices inside generated Go files.
GO_LEXEME = re.compile(r'//[^\n]*(?:\n[ \t]*//[^\n]*)*|/\*[\s\S]*?\*/|"(?:\\.|[^"\\])*"|`[^`]*`|\'(?:\\.|[^\'\\])*\'')
REQUIRED_FILES = {
    'modernc.org/sqlite': ('LICENSE', 'LICENSE-SQLITE', 'LICENSE-SQLITE_VEC'),
    'modernc.org/libc': ('LICENSE', 'LICENSE-3RD-PARTY.md'),
    'gopkg.in/yaml.v3': ('LICENSE', 'NOTICE'),
    'github.com/modelcontextprotocol/go-sdk': ('LICENSE',),
}


def json_stream(value):
    decoder = json.JSONDecoder()
    index = 0
    while index < len(value):
        if value[index].isspace():
            index += 1
            continue
        item, index = decoder.raw_decode(value, index)
        yield item


def run_json(go, args, root, env=None):
    # Go emits UTF-8 JSON independently of the host's console/code page.
    output = subprocess.check_output([go, *args], cwd=root, env=env, text=True, encoding='utf-8')
    return list(json_stream(output))


def source_comments(path):
    for match in GO_LEXEME.finditer(path.read_text(encoding='utf-8')):
        comment = match.group()
        if comment.startswith(('//', '/*')) and LEGAL_WORD.search(comment):
            yield comment


def legal_files(root, packages):
    """Collect root terms and every selected package's ancestor notices."""
    directories = {root}
    for package in packages:
        directory = Path(package['Dir']).resolve()
        if not directory.is_relative_to(root):
            raise ValueError(f"package source is outside its component: {package['ImportPath']}")
        directories.update((directory, *[p for p in directory.parents if p.is_relative_to(root)]))
    return sorted({path for directory in directories for path in directory.iterdir()
                   if path.is_file() and LEGAL_NAME.match(path.name)})


def copy_component(output, name, version, source_root, packages, *, extra_files=()):
    source_root = Path(source_root).resolve()
    files = legal_files(source_root, packages)
    if not any(LICENCE_NAME.match(path.name) for path in files if path.parent == source_root):
        raise ValueError(f'no root licence text for {name}@{version}')
    for required in REQUIRED_FILES.get(name, ()):
        if source_root / required not in files:
            raise ValueError(f'missing required notice for {name}: {required}')
    for relative in extra_files:
        source = source_root / relative
        if not source.is_file():
            raise ValueError(f'missing required notice for {name}: {relative}')
        files.append(source)
    component_dir = Path('components') / (name.replace('/', '__') + '@' + version)
    records = []
    for source in sorted(set(files)):
        relative = source.relative_to(source_root)
        destination = component_dir / relative
        data = source.read_bytes()
        if not data.strip():
            raise ValueError(f'empty notice: {source}')
        (output / destination).parent.mkdir(parents=True, exist_ok=True)
        (output / destination).write_bytes(data)
        records.append({'source': relative.as_posix(), 'file': destination.as_posix(),
                        'sha256': hashlib.sha256(data).hexdigest()})
    comments = {}
    for package in packages:
        for field in ('GoFiles', 'CgoFiles', 'SFiles', 'CFiles', 'HFiles'):
            # The release builds explicitly disable CGO; reject it rather than
            # silently overlooking native link dependencies and their notices.
            if field == 'CgoFiles' and package.get(field):
                raise ValueError(f"CGO build needs a native dependency inventory: {package['ImportPath']}")
            for filename in package.get(field, ()):
                source = Path(package['Dir']) / filename
                # Go/assembly comments share the // and /* syntaxes.
                for comment in source_comments(source):
                    comments.setdefault(comment, set()).add(source.relative_to(source_root).as_posix())
    if comments:
        data = '\n\n'.join('Source: ' + ', '.join(sorted(sources)) + '\n' + comment
                           for comment, sources in sorted(comments.items())).encode('utf-8') + b'\n'
        destination = component_dir / 'SOURCE-NOTICES.txt'
        (output / destination).write_bytes(data)
        records.append({'source': 'selected source comments', 'file': destination.as_posix(),
                        'sha256': hashlib.sha256(data).hexdigest()})
    return {'path': name, 'version': version,
            'packages': sorted(package['ImportPath'] for package in packages), 'notices': records}


def collect(binary, output, root, go, tags='', ca_certificates=False):
    binary, output, root = Path(binary).resolve(), Path(output).resolve(), Path(root).resolve()
    if output.exists() and any(output.iterdir()):
        raise ValueError(f'notice output must be empty: {output}')
    info, = run_json(go, ['version', '-m', '-json', str(binary)], root)
    settings = {setting['Key']: setting['Value'] for setting in info['Settings']}
    if settings.get('CGO_ENABLED') != '0':
        raise ValueError('notice collection requires the release CGO_ENABLED=0 build')
    env = dict(os.environ, GOOS=settings['GOOS'], GOARCH=settings['GOARCH'], CGO_ENABLED='0')
    arguments = ['list', '-deps', '-json'] + (['-tags', tags] if tags else []) + ['.']
    packages = run_json(go, arguments, root, env)
    go_env, = run_json(go, ['env', '-json', 'GOROOT', 'GOVERSION'], root)
    if go_env['GOVERSION'] != info['GoVersion']:
        raise ValueError('binary and licence-source Go toolchains differ')
    grouped = {}
    standard = []
    for package in packages:
        if package.get('Standard'):
            standard.append(package)
        elif not package.get('Module', {}).get('Main'):
            module = package.get('Module')
            if not module or module.get('Replace'):
                raise ValueError(f"unversioned or replaced dependency: {package['ImportPath']}")
            key = (module['Path'], module['Version'], module['Dir'])
            grouped.setdefault(key, []).append(package)
    binary_modules = {(dep['Path'], dep['Version']) for dep in info.get('Deps', ())}
    if binary_modules != {(name, version) for name, version, _ in grouped}:
        raise ValueError('binary modules do not match the selected target package graph')
    if settings.get('-tags', '') != tags:
        raise ValueError('binary build tags do not match the selected target package graph')
    output.mkdir(parents=True, exist_ok=True)
    components = [copy_component(output, name, version, directory, selected)
                  for (name, version, directory), selected in sorted(grouped.items())]
    go_extras = ['PATENTS']
    if any(p['ImportPath'] == 'time/tzdata' for p in standard):
        go_extras.append('lib/time/README')
    components.append(copy_component(output, 'go', info['GoVersion'], go_env['GOROOT'],
                                     standard, extra_files=go_extras))
    if ca_certificates:
        source = Path('/usr/share/doc/ca-certificates/copyright')
        if not source.is_file():
            raise ValueError('missing copyright for the container CA certificates')
        references = set(re.findall(r'/usr/share/common-licenses/([A-Za-z0-9._+-]+)', source.read_text()))
        # Debian's copyright links to the complete MPL/GPL terms in this directory.
        selected_files = [('copyright', source)] + [(name, Path('/usr/share/common-licenses') / name)
                                                     for name in sorted(references)]
        records = []
        for name, path in selected_files:
            data = path.read_bytes()
            destination = Path('components/ca-certificates') / name
            (output / destination).parent.mkdir(parents=True, exist_ok=True)
            (output / destination).write_bytes(data)
            records.append({'source': str(path), 'file': destination.as_posix(),
                            'sha256': hashlib.sha256(data).hexdigest()})
        package_version = subprocess.check_output(
            ['dpkg-query', '-W', '-f=${Version}', 'ca-certificates'], text=True, encoding='utf-8').strip()
        if not package_version:
            raise ValueError('missing container CA package version')
        components.append({'path': 'Debian ca-certificates', 'version': package_version,
                           'packages': ['/etc/ssl/certs/ca-certificates.crt'], 'notices': records,
                           'bundle_sha256': hashlib.sha256(Path('/etc/ssl/certs/ca-certificates.crt').read_bytes()).hexdigest()})
    inventory = {'format_version': 1, 'binary': binary.name,
                 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
                 'goos': settings['GOOS'], 'goarch': settings['GOARCH'], 'tags': tags,
                 'build_info': info, 'components': components}
    (output / 'inventory.json').write_text(json.dumps(inventory, indent=2, sort_keys=True) + '\n', encoding='utf-8')
    (output / 'README.txt').write_text(
        'Third-party notices for the accompanying Tutor MCP binary.\n'
        'inventory.json identifies its target, binary hash, exact Go modules and toolchain,\n'
        'selected packages, and SHA-256 hashes of the accompanying original notice texts.\n'
        'SOURCE-NOTICES.txt preserves additional copyright/licence comments from selected sources.\n'
        'These texts keep their original terms; Tutor MCP\'s MIT licence does not replace them.\n'
        'This inventory preserves supplied notices and is not a legal classification.\n', encoding='utf-8')
    return inventory


def verify_contents(read_file, binary_name):
    """Check a built distribution, including the complete copied notice payload."""
    if not read_file('LICENSE').strip():
        raise ValueError('distribution has no project licence')
    prefix = 'THIRD_PARTY_NOTICES/'
    if not read_file(prefix + 'README.txt').strip():
        raise ValueError('distribution has no notice instructions')
    inventory = json.loads(read_file(prefix + 'inventory.json'))
    if inventory.get('format_version') != 1 or inventory.get('binary') != binary_name:
        raise ValueError('invalid distribution notice inventory')
    if hashlib.sha256(read_file(binary_name)).hexdigest() != inventory['binary_sha256']:
        raise ValueError('notice inventory does not describe the distributed binary')
    components = inventory['components']
    if not components or not any(c['path'] == 'go' for c in components):
        raise ValueError('distribution has no Go runtime notices')
    for component in components:
        if not component['notices']:
            raise ValueError('component has no notice texts')
        for notice in component['notices']:
            name = notice['file']
            if not name.startswith('components/') or '..' in Path(name).parts:
                raise ValueError('invalid notice path')
            data = read_file(prefix + name)
            if not data.strip() or hashlib.sha256(data).hexdigest() != notice['sha256']:
                raise ValueError(f'missing or changed notice: {name}')
    return inventory


def verify_archive(path):
    path = Path(path)
    if path.suffix == '.zip':
        with zipfile.ZipFile(path) as archive:
            inventory = verify_contents(archive.read, 'tutor-mcp.exe')
    else:
        with tarfile.open(path, 'r:gz') as archive:
            def read_file(name):
                member = archive.getmember(name)
                if not member.isfile():
                    raise ValueError(f'non-regular distribution member: {name}')
                return archive.extractfile(member).read()
            inventory = verify_contents(read_file, 'tutor-mcp')
    expected = f"tutor-mcp_{inventory['goos']}_{inventory['goarch']}." + ('zip' if path.suffix == '.zip' else 'tar.gz')
    if path.name != expected:
        raise ValueError('archive target differs from its binary inventory')
    return inventory


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary')
    parser.add_argument('--output')
    parser.add_argument('--root', default=str(Path(__file__).resolve().parent.parent))
    parser.add_argument('--tags', default='')
    parser.add_argument('--ca-certificates', action='store_true')
    parser.add_argument('--verify-archive')
    parser.add_argument('--verify-tree', help='directory with binary, LICENSE and THIRD_PARTY_NOTICES')
    parser.add_argument('--ca-bundle', help='also verify the container certificate bundle against its inventory')
    args = parser.parse_args()
    if args.verify_archive:
        verify_archive(args.verify_archive)
    elif args.verify_tree:
        tree = Path(args.verify_tree)
        binary_name = 'tutor-mcp.exe' if (tree / 'tutor-mcp.exe').is_file() else 'tutor-mcp'
        inventory = verify_contents(lambda name: (tree / name).read_bytes(), binary_name)
        if args.ca_bundle:
            ca_components = [c for c in inventory['components'] if c['path'] == 'Debian ca-certificates']
            if (len(ca_components) != 1 or hashlib.sha256(Path(args.ca_bundle).read_bytes()).hexdigest()
                    != ca_components[0]['bundle_sha256']):
                raise ValueError('container certificate bundle differs from its notice inventory')
    else:
        if not args.binary or not args.output:
            parser.error('--binary and --output are required when collecting notices')
        collect(args.binary, args.output, args.root, os.environ.get('TUTOR_GO_BIN', 'go'),
                args.tags, args.ca_certificates)


if __name__ == '__main__':
    main()
