"""Offline shell acceptance: platform, checksums, notices and legacy supplement."""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


class InstallerFixture:
    def __init__(self, root, system='Linux', machine='x86_64', goos='linux', arch='amd64', version='latest'):
        self.root, self.goos, self.arch = root, goos, arch
        self.assets, commands = root / 'assets', root / 'commands'
        self.assets.mkdir()
        commands.mkdir()
        self.asset = self.assets / f'tutor-mcp_{goos}_{arch}.tar.gz'
        self.supplement = self.assets / f'tutor-mcp_{goos}_{arch}_notices.tar.gz'
        self.destination = root / 'bin with spaces'
        self.data = b'#!/bin/sh\nprintf "installed fixture\\n"\n'
        self.license_name = 'components/go@go1.fixture/LICENSE'
        self.license_data = b'Original Go fixture licence\n'
        self.files = {'LICENSE': b'Project fixture licence\n',
                      'THIRD_PARTY_NOTICES/README.txt': b'Original notice instructions\n',
                      'THIRD_PARTY_NOTICES/' + self.license_name: self.license_data}
        self.inventory = {'format_version': 1, 'binary': 'tutor-mcp',
                          'binary_sha256': hashlib.sha256(self.data).hexdigest(), 'goos': goos, 'goarch': arch,
                          'components': [{'path': 'go', 'notices': [{'file': self.license_name,
                              'sha256': hashlib.sha256(self.license_data).hexdigest(), 'source': 'LICENSE'}]}]}
        self.log = root / 'downloads.txt'
        (commands / 'uname').write_text(f'#!/bin/sh\ncase "$1" in -s) echo {system};; -m) echo {machine};; esac\n')
        (commands / 'curl').write_text('#!/bin/sh\nprintf "%s\\n" "$2" >> "$INSTALL_TEST_DOWNLOAD_LOG"\ncp "$INSTALL_TEST_ASSETS/${2##*/}" "$4"\n')
        for path in commands.iterdir():
            path.chmod(0o755)
        self.env = dict(os.environ, PATH=str(commands) + os.pathsep + os.environ['PATH'],
                        INSTALL_TEST_ASSETS=str(self.assets), INSTALL_TEST_DOWNLOAD_LOG=str(self.log),
                        TUTOR_MCP_INSTALL_DIR=str(self.destination), TUTOR_MCP_VERSION=version)

    def write_tar(self, path, files):
        with tarfile.open(path, 'w:gz') as archive:
            for name, data in files.items():
                entry = tarfile.TarInfo(name)
                entry.size, entry.mode = len(data), 0o755 if name == 'tutor-mcp' else 0o644
                archive.addfile(entry, io.BytesIO(data))

    def write_main(self, legacy=False):
        files = {'tutor-mcp': self.data, 'LICENSE': self.files['LICENSE']}
        if not legacy:
            files.update(self.files)
            files['THIRD_PARTY_NOTICES/inventory.json'] = json.dumps(self.inventory, indent=2, sort_keys=True).encode()
        self.write_tar(self.asset, files)
        (self.assets / 'SHA256SUMS').write_text(hashlib.sha256(self.asset.read_bytes()).hexdigest() + '  ' + self.asset.name + '\n')

    def write_supplement(self, **changes):
        inventory = dict(self.inventory, release_version='v0.6.1',
                         source_commit='440f837e7f043f2f64db23be9459512375dfa542',
                         original_archive_sha256=hashlib.sha256(self.asset.read_bytes()).hexdigest())
        inventory.update(changes)
        files = dict(self.files)
        files['THIRD_PARTY_NOTICES/inventory.json'] = json.dumps(inventory, indent=2, sort_keys=True).encode()
        self.write_tar(self.supplement, files)
        (self.assets / 'THIRD_PARTY_NOTICES_SHA256SUMS').write_text(
            hashlib.sha256(self.supplement.read_bytes()).hexdigest() + '  ' + self.supplement.name + '\n')

    def run(self):
        return subprocess.run(['sh', str(Path(__file__).resolve().parent / 'install.sh')],
                              env=self.env, capture_output=True, text=True)

    def installed_bytes(self):
        return {path.relative_to(self.destination).as_posix(): path.read_bytes()
                for path in self.destination.rglob('*') if path.is_file()}

    def assert_installed(self, test):
        test.assertEqual(subprocess.check_output([str(self.destination / 'tutor-mcp')], text=True), 'installed fixture\n')
        notice_root = self.destination / 'tutor-mcp-notices'
        test.assertEqual((notice_root / 'LICENSE').read_bytes(), self.files['LICENSE'])
        test.assertEqual((notice_root / 'THIRD_PARTY_NOTICES' / self.license_name).read_bytes(), self.license_data)
        return self.installed_bytes()


class ShellInstallerTests(unittest.TestCase):
    def test_platform_selection_checksum_and_installed_notices(self):
        for platform in [('Linux', 'x86_64', 'linux', 'amd64'), ('Darwin', 'arm64', 'darwin', 'arm64')]:
            with self.subTest(platform=platform), tempfile.TemporaryDirectory() as directory:
                fixture = InstallerFixture(Path(directory), *platform)
                fixture.write_main()
                result = fixture.run()
                self.assertEqual(result.returncode, 0, result.stderr)
                before = fixture.assert_installed(self)
                self.assertNotIn('_notices.tar.gz', fixture.log.read_text())
                (fixture.assets / 'SHA256SUMS').write_text('0' * 64 + '  ' + fixture.asset.name + '\n')
                result = fixture.run()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('checksum mismatch', result.stderr)
                self.assertEqual(fixture.assert_installed(self), before)

    def test_missing_changed_or_foreign_bundled_notices_are_rejected(self):
        for defect in ('missing', 'changed', 'binary', 'target', 'format'):
            with self.subTest(defect=defect), tempfile.TemporaryDirectory() as directory:
                fixture = InstallerFixture(Path(directory))
                if defect == 'missing':
                    del fixture.files['THIRD_PARTY_NOTICES/' + fixture.license_name]
                elif defect == 'changed':
                    fixture.files['THIRD_PARTY_NOTICES/' + fixture.license_name] = b'Tampered notice'
                elif defect == 'binary':
                    fixture.inventory['binary_sha256'] = '0' * 64
                elif defect == 'target':
                    fixture.inventory['goarch'] = 'arm64'
                else:
                    fixture.inventory['format_version'] = 2
                fixture.write_main()
                result = fixture.run()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertFalse((fixture.destination / 'tutor-mcp').exists())

    def test_latest_and_explicit_v061_legacy_archives_install_verified_supplement(self):
        for version in ('latest', 'v0.6.1'):
            with self.subTest(version=version), tempfile.TemporaryDirectory() as directory:
                fixture = InstallerFixture(Path(directory), version=version)
                fixture.write_main(legacy=True)
                fixture.write_supplement()
                result = fixture.run()
                self.assertEqual(result.returncode, 0, result.stderr)
                fixture.assert_installed(self)
                requests = fixture.log.read_text()
                self.assertIn('_notices.tar.gz', requests)
                self.assertIn('THIRD_PARTY_NOTICES_SHA256SUMS', requests)

    def test_missing_tampered_or_mismatched_supplement_preserves_installation(self):
        for defect in ('missing', 'checksum', 'binary', 'archive', 'release', 'source', 'notice'):
            with self.subTest(defect=defect), tempfile.TemporaryDirectory() as directory:
                fixture = InstallerFixture(Path(directory))
                fixture.write_main()
                self.assertEqual(fixture.run().returncode, 0)
                before = fixture.assert_installed(self)
                fixture.write_main(legacy=True)
                changes = {}
                if defect == 'binary':
                    changes['binary_sha256'] = '0' * 64
                elif defect == 'archive':
                    changes['original_archive_sha256'] = '0' * 64
                elif defect == 'release':
                    changes['release_version'] = 'v0.6.0'
                elif defect == 'source':
                    changes['source_commit'] = '0' * 40
                elif defect == 'notice':
                    fixture.files['THIRD_PARTY_NOTICES/' + fixture.license_name] = b'Tampered notice'
                if defect != 'missing':
                    fixture.write_supplement(**changes)
                if defect == 'checksum':
                    (fixture.assets / 'THIRD_PARTY_NOTICES_SHA256SUMS').write_text('0' * 64 + '  ' + fixture.supplement.name + '\n')
                self.assertNotEqual(fixture.run().returncode, 0)
                self.assertEqual(fixture.installed_bytes(), before)

    def test_unqualified_older_release_requires_source_install(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = InstallerFixture(Path(directory), version='v0.6.0')
            fixture.write_main(legacy=True)
            result = fixture.run()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('build from source', result.stderr)
            self.assertFalse(fixture.destination.exists())


if __name__ == '__main__':
    unittest.main()
