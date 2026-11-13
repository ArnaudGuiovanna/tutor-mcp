"""Keep distributed platforms, CI and publication gates in the same scope."""
import json
from pathlib import Path
import re
import runpy
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parent.parent


class PlatformPolicyTests(unittest.TestCase):
    def test_packaging_rejects_stale_assets_without_deleting_them(self):
        packager = runpy.run_path(str(ROOT / 'scripts/package-release.py'))
        with tempfile.TemporaryDirectory() as directory:
            stale = Path(directory) / 'tutor-mcp_darwin_arm64.tar.gz'
            stale.write_bytes(b'historical artifact')
            with patch('sys.argv', ['package-release.py', '--version', 'v0.0.1', '--output', directory]):
                with self.assertRaisesRegex(ValueError, 'must be empty'):
                    packager['main']()
            self.assertEqual(stale.read_bytes(), b'historical artifact')

    def test_release_targets_are_exactly_the_build_and_publication_matrix(self):
        packager = runpy.run_path(str(ROOT / 'scripts/package-release.py'))
        targets = set(packager['release_targets']())
        workflow = (ROOT / '.github/workflows/ci.yml').read_text()
        matrix = workflow.split('  build:\n', 1)[1].split('    steps:\n', 1)[0]
        actual = re.findall(r'- goos: (\w+)\s+goarch: (\w+)', matrix)
        self.assertEqual(set(actual), targets)
        self.assertEqual(len(actual), len(targets))
        required = json.loads((ROOT / '.github/required-checks.json').read_text())['contexts']
        self.assertEqual({name for name in required if name.startswith('Build (')},
                         {f'Build ({goos}/{arch})' for goos, arch in targets})

    def test_native_checks_and_client_environments_match_declared_support(self):
        policy = json.loads((ROOT / 'scripts/release-platforms.json').read_text())
        workflow = (ROOT / '.github/workflows/ci.yml').read_text()
        matrix = workflow.split('  local-native:\n', 1)[1].split('    steps:\n', 1)[0]
        runners = re.search(r'os: \[([^\]]+)\]', matrix).group(1).split(', ')
        self.assertEqual(runners, policy['native_server_runners'])
        required = json.loads((ROOT / '.github/required-checks.json').read_text())['contexts']
        self.assertEqual({name for name in required if name.startswith('Local (')},
                         {f'Local ({runner})' for runner in runners})
        self.assertEqual(policy['client'], {'host': 'windows', 'architecture': 'amd64',
                                         'harness_environments': ['native', 'wsl']})
        client = (ROOT / '.github/workflows/client.yml').read_text()
        self.assertIn('runs-on: windows-2025', client)
        self.assertIn('qualify-wsl-ci.ps1', client)
        self.assertIn('qualify-runtime.ps1', client)

    def test_platform_change_preserves_security_pedagogy_and_deployment_gates(self):
        required = json.loads((ROOT / '.github/required-checks.json').read_text())
        self.assertEqual(required['app_id'], 15368)
        invariant = {'Format, modules, vet, staticcheck', 'Test (race detector)',
                     'Test (PostgreSQL 17)', 'Govulncheck', 'Secret scan (full history)',
                     'CodeQL', 'Deployment (hobby)', 'Deployment (institution)',
                     'Client (Windows)'}
        self.assertTrue(invariant.issubset(required['contexts']))
        workflow = (ROOT / '.github/workflows/ci.yml').read_text()
        self.assertIn('test_platform_policy.py', workflow)
        self.assertIn('test_platform_policy.py', (ROOT / 'scripts/verify-task.sh').read_text())


if __name__ == '__main__':
    unittest.main()
