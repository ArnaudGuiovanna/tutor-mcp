"""Offline acceptance for exact source notices and distribution contents."""
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from third_party_notices import collect, copy_component, run_json, source_comments, verify_archive


class NoticeTests(unittest.TestCase):
    def test_go_json_utf8_survives_windows_cp1252_default(self):
        documents = [{'Dir': 'C:/Users/Zoë/学習', 'Doc': '“MCP” — apprendre'},
                     {'ImportPath': 'example.org/日本語', 'Doc': 'Deuxième document'}]
        payload = '\n'.join(json.dumps(item, ensure_ascii=False) for item in documents).encode('utf-8')
        emitter = 'import os; os.write(1, bytes.fromhex(' + repr(payload.hex()) + '))'
        real_popen = subprocess.Popen

        def windows_default_popen(*args, **kwargs):
            if kwargs.get('text') and kwargs.get('encoding') is None:
                kwargs['encoding'] = 'cp1252'
            return real_popen(*args, **kwargs)

        with tempfile.TemporaryDirectory() as root, patch('third_party_notices.subprocess.Popen', side_effect=windows_default_popen):
            # Verify that the simulated Windows default really rejects the
            # emitter's UTF-8 bytes (including undefined cp1252 byte 0x9d).
            with self.assertRaises(UnicodeDecodeError):
                subprocess.check_output([sys.executable, '-c', emitter], cwd=root, text=True)
            self.assertEqual(run_json(sys.executable, ['-c', emitter], root), documents)

    def test_original_terms_nested_notices_and_generated_source_attribution(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            module, output = root / 'module', root / 'notices'
            package = module / 'internal' / 'selected'
            package.mkdir(parents=True)
            output.mkdir()
            license_bytes = b'Original licence\r\nCopyright fixture author\r\n'
            (module / 'LICENSE').write_bytes(license_bytes)
            (module / 'NOTICE').write_text('Original root attribution\n')
            (package.parent / 'LICENSE-EMBEDDED').write_text('Embedded component terms\n')
            (package / 'NOTICE').write_text('Package attribution\n')
            comment = '/* Copyright embedded author\n * Redistribution in binary form must retain this notice.\n */'
            source = package / 'generated.go'
            source.write_text('package selected\n' + comment + '\nvar ignored = "// Copyright string, not a comment"\n')
            metadata = {'ImportPath': 'example.org/selected/internal/selected', 'Dir': str(package), 'GoFiles': ['generated.go']}
            component = copy_component(output, 'example.org/selected', 'v1.2.3', module, [metadata])
            files = {record['source']: (output / record['file']).read_bytes() for record in component['notices']}
            self.assertEqual(files['LICENSE'], license_bytes)
            self.assertEqual(files['NOTICE'], b'Original root attribution\n')
            self.assertEqual(files['internal/LICENSE-EMBEDDED'], b'Embedded component terms\n')
            self.assertEqual(files['internal/selected/NOTICE'], b'Package attribution\n')
            self.assertIn(comment.encode(), files['selected source comments'])
            self.assertNotIn(b'Copyright string', files['selected source comments'])

    def test_module_without_licence_and_missing_additional_native_notice_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            module, output = root / 'module', root / 'out'
            module.mkdir()
            with self.assertRaisesRegex(ValueError, 'no root licence'):
                copy_component(output, 'example.org/missing', 'v1.0.0', module, [])
            (module / 'LICENSE').write_text('Original licence')
            (module / 'LICENSE-SQLITE').write_text('Original SQLite terms')
            with self.assertRaisesRegex(ValueError, 'LICENSE-SQLITE_VEC'):
                copy_component(output, 'modernc.org/sqlite', 'v1.58.0', module, [])

    def test_unreferenced_modules_are_excluded_and_graph_mismatch_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / 'tutor-mcp'
            binary.write_bytes(b'built fixture')
            goroot = root / 'go'
            goroot.mkdir()
            for name in ('LICENSE', 'PATENTS'):
                (goroot / name).write_text('Original Go ' + name)
            module = root / 'module'
            module.mkdir()
            (module / 'LICENSE').write_text('Original dependency licence')
            info = {'GoVersion': 'go1.fixture', 'Settings': [{'Key': key, 'Value': value} for key, value in
                    [('CGO_ENABLED', '0'), ('GOOS', 'linux'), ('GOARCH', 'amd64')]],
                    'Deps': [{'Path': 'example.org/used', 'Version': 'v1.0.0'}]}
            packages = [{'ImportPath': 'example.org/used', 'Dir': str(module),
                         'Module': {'Path': 'example.org/used', 'Version': 'v1.0.0', 'Dir': str(module)}}]
            with patch('third_party_notices.run_json', side_effect=[[info], packages, [{'GOROOT': str(goroot), 'GOVERSION': 'go1.fixture'}]]):
                inventory = collect(binary, root / 'out', root, 'go')
            self.assertEqual({c['path'] for c in inventory['components']}, {'example.org/used', 'go'})
            info['Deps'].append({'Path': 'example.org/not-in-selected-graph', 'Version': 'v2.0.0'})
            with patch('third_party_notices.run_json', side_effect=[[info], packages, [{'GOROOT': str(goroot), 'GOVERSION': 'go1.fixture'}]]):
                with self.assertRaisesRegex(ValueError, 'do not match'):
                    collect(binary, root / 'mismatch', root, 'go')

    def test_all_six_archive_targets_require_complete_unmodified_texts(self):
        for goos in ('linux', 'darwin', 'windows'):
            for goarch in ('amd64', 'arm64'):
                with self.subTest(goos=goos, goarch=goarch), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    binary_name = 'tutor-mcp.exe' if goos == 'windows' else 'tutor-mcp'
                    binary = b'compiled target fixture'
                    notice = b'Original Go licence text\n'
                    inventory = {'format_version': 1, 'binary': binary_name,
                                 'binary_sha256': hashlib.sha256(binary).hexdigest(), 'goos': goos, 'goarch': goarch,
                                 'components': [{'path': 'go', 'version': 'go1.fixture', 'notices': [
                                     {'file': 'components/go@go1.fixture/LICENSE', 'sha256': hashlib.sha256(notice).hexdigest()}]}]}
                    files = {binary_name: binary, 'LICENSE': b'Project licence',
                             'THIRD_PARTY_NOTICES/README.txt': b'Notice instructions',
                             'THIRD_PARTY_NOTICES/inventory.json': json.dumps(inventory).encode(),
                             'THIRD_PARTY_NOTICES/components/go@go1.fixture/LICENSE': notice}
                    suffix = 'zip' if goos == 'windows' else 'tar.gz'
                    archive = root / f'tutor-mcp_{goos}_{goarch}.{suffix}'
                    def write_archive():
                        if goos == 'windows':
                            with zipfile.ZipFile(archive, 'w') as target:
                                for name, data in files.items():
                                    target.writestr(name, data)
                        else:
                            with tarfile.open(archive, 'w:gz') as target:
                                for name, data in files.items():
                                    member = tarfile.TarInfo(name)
                                    member.size = len(data)
                                    target.addfile(member, io.BytesIO(data))
                    write_archive()
                    self.assertEqual(verify_archive(archive)['goarch'], goarch)
                    files['THIRD_PARTY_NOTICES/components/go@go1.fixture/LICENSE'] = b'Changed text'
                    write_archive()
                    with self.assertRaisesRegex(ValueError, 'changed notice'):
                        verify_archive(archive)
                    del files['THIRD_PARTY_NOTICES/components/go@go1.fixture/LICENSE']
                    write_archive()
                    with self.assertRaises(KeyError):
                        verify_archive(archive)


if __name__ == '__main__':
    unittest.main()
