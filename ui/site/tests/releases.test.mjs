import assert from 'node:assert/strict';
import { test } from 'node:test';
import { GITHUB_REPO, parseLatestRelease, releaseSnapshot, withLatestClient } from '../lib/releases.ts';

function candidate({ version = 'v9.0.0', client = true, ...extra } = {}) {
  const names = ['tutor-mcp_linux_amd64.tar.gz', 'SHA256SUMS'];
  if (client) names.push('Tutor_ACP_2.0.0_windows_x64-setup.exe');
  return { tag_name: version, draft: false, prerelease: false, published_at: '2026-10-26T12:00:00Z',
    assets: names.map(name => ({ name, browser_download_url: `${GITHUB_REPO}/releases/download/${version}/${name}`, size: 100 })), ...extra };
}

test('the verified release includes six server platforms and the product-version client', () => {
  assert.equal(releaseSnapshot.assets.filter(asset => /^tutor-mcp_(linux|darwin|windows)_(amd64|arm64)\.(tar\.gz|zip)$/.test(asset.name)).length, 6);
  assert.match(releaseSnapshot.clientVersion, /^\d+\.\d+\.\d+$/);
  assert.ok(releaseSnapshot.client.url.endsWith(`Tutor_ACP_${releaseSnapshot.clientVersion}_windows_x64-setup.exe`));
});
test('future stable release updates the actual client asset and its own checksums', () => {
  const result = parseLatestRelease(candidate());
  assert.equal(result.version, 'v9.0.0');
  assert.equal(result.clientVersion, '2.0.0');
  assert.match(result.clientChecksums, /v9\.0\.0\/SHA256SUMS$/);
});
test('server-only releases keep the last verified ACP installer and checksum together', () => {
  const result = parseLatestRelease(candidate({ client: false }));
  assert.equal(result.version, 'v9.0.0');
  assert.deepEqual(result.client, releaseSnapshot.client);
  assert.equal(result.clientChecksums, releaseSnapshot.clientChecksums);
});
test('technical prereleases and drafts never replace the stable downloads', () => {
  assert.throws(() => parseLatestRelease(candidate({ prerelease: true })));
  assert.throws(() => parseLatestRelease(candidate({ draft: true })));
  assert.throws(() => parseLatestRelease(candidate({ version: 'tutor-client-v0.3.0' })));
});
test('untrusted URLs and incomplete API responses cannot become download links', () => {
  const data = candidate();
  data.assets.forEach(asset => { asset.browser_download_url = 'https://example.invalid/download.exe'; });
  assert.throws(() => parseLatestRelease(data));
  assert.throws(() => parseLatestRelease(null));
  assert.throws(() => parseLatestRelease(candidate({ assets: [] })));
});
test('a server-only release can discover the most recent stable ACP client independently', () => {
  const server = parseLatestRelease(candidate({ client: false }));
  const newerTechnicalPreview = candidate({ version: 'v10.0.0', prerelease: true });
  const clientRelease = candidate({ version: 'tutor-acp-v2.0.0' });
  const result = withLatestClient(server, [newerTechnicalPreview, clientRelease]);
  assert.equal(result.version, 'v9.0.0');
  assert.equal(result.clientVersion, '2.0.0');
  assert.equal(result.clientRelease, 'tutor-acp-v2.0.0');
  assert.ok(result.clientChecksums.includes('/tutor-acp-v2.0.0/'));
});
