"""Offline shell installer acceptance: OS selection, checksum and installed binary."""
import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


class ShellInstallerTests(unittest.TestCase):
    def test_platform_selection_and_checksum(self):
        installer = Path(__file__).resolve().parent / 'install.sh'
        for system, machine, goos, arch in [('Linux', 'x86_64', 'linux', 'amd64'),
                                            ('Darwin', 'arm64', 'darwin', 'arm64')]:
            with self.subTest(system=system), tempfile.TemporaryDirectory() as root:
                root = Path(root)
                commands = root / 'commands'
                commands.mkdir()
                assets = root / 'assets'
                assets.mkdir()
                asset = assets / f'tutor-mcp_{goos}_{arch}.tar.gz'
                data = b'#!/bin/sh\nprintf "installed fixture\\n"\n'
                with tarfile.open(asset, 'w:gz') as archive:
                    info = tarfile.TarInfo('tutor-mcp')
                    info.size = len(data)
                    info.mode = 0o755
                    archive.addfile(info, io.BytesIO(data))
                sums = assets / 'SHA256SUMS'
                sums.write_text(hashlib.sha256(asset.read_bytes()).hexdigest() + '  ' + asset.name + '\n')
                (commands / 'uname').write_text(f'#!/bin/sh\ncase "$1" in -s) echo {system};; -m) echo {machine};; esac\n')
                # Intercept only the download. The real tar/checksum/install tools run.
                (commands / 'curl').write_text('#!/bin/sh\ncp "$INSTALL_TEST_ASSETS/${2##*/}" "$4"\n')
                for path in commands.iterdir():
                    path.chmod(0o755)
                install_dir = root / 'bin with spaces'
                env = dict(os.environ, PATH=str(commands) + os.pathsep + os.environ['PATH'],
                           INSTALL_TEST_ASSETS=str(assets), TUTOR_MCP_INSTALL_DIR=str(install_dir),
                           TUTOR_MCP_VERSION='v0.6.0')
                subprocess.run(['sh', str(installer)], env=env, check=True, capture_output=True)
                self.assertEqual(subprocess.check_output([str(install_dir / 'tutor-mcp')], text=True), 'installed fixture\n')
                before = (install_dir / 'tutor-mcp').read_bytes()
                sums.write_text('0' * 64 + '  ' + asset.name + '\n')
                bad = subprocess.run(['sh', str(installer)], env=env, capture_output=True, text=True)
                self.assertNotEqual(bad.returncode, 0)
                self.assertIn('checksum mismatch', bad.stderr)
                self.assertEqual((install_dir / 'tutor-mcp').read_bytes(), before)


if __name__ == '__main__':
    unittest.main()
