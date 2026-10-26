import snapshot from './release-data.json' with { type: 'json' };

export const GITHUB_REPO = 'https://github.com/ArnaudGuiovanna/tutor-mcp';
export const RELEASE_API = 'https://api.github.com/repos/ArnaudGuiovanna/tutor-mcp/releases/latest';
export type DownloadAsset = { name: string; url: string; size: number };
export type ReleaseInfo = {
  version: string;
  url: string;
  publishedAt: string;
  verifiedAt: string;
  assets: DownloadAsset[];
  clientVersion: string;
  client: DownloadAsset;
  clientRelease: string;
  clientChecksums: string;
};
export const releaseSnapshot: ReleaseInfo = snapshot;

export function parseLatestRelease(value: unknown, fallback = releaseSnapshot): ReleaseInfo {
  if (!value || typeof value !== 'object') throw new Error('Release unavailable');
  const data = value as Record<string, unknown>;
  if (typeof data.tag_name !== 'string' || !/^v\d+\.\d+\.\d+$/.test(data.tag_name)
      || data.draft || data.prerelease || !Array.isArray(data.assets)) throw new Error('Stable release unavailable');
  const prefix = `${GITHUB_REPO}/releases/download/${data.tag_name}/`;
  const assets: DownloadAsset[] = data.assets.flatMap((item: Record<string, unknown>) =>
    typeof item.name === 'string' && typeof item.browser_download_url === 'string'
      && item.browser_download_url.startsWith(prefix) && typeof item.size === 'number'
      ? [{ name: item.name, url: item.browser_download_url, size: item.size }] : []);
  if (!assets.some(asset => asset.name === 'tutor-mcp_linux_amd64.tar.gz')
      || !assets.some(asset => asset.name === 'SHA256SUMS')) throw new Error('Incomplete server release');
  const client = assets.find(asset => /^Tutor_ACP_\d+\.\d+\.\d+_windows_x64-setup\.exe$/.test(asset.name));
  return {
    version: data.tag_name,
    url: `${GITHUB_REPO}/releases/tag/${data.tag_name}`,
    publishedAt: typeof data.published_at === 'string' ? data.published_at : '',
    verifiedAt: new Date().toISOString(),
    assets,
    clientVersion: client?.name.match(/^Tutor_ACP_(\d+\.\d+\.\d+)_/)?.[1] ?? fallback.clientVersion,
    client: client ?? fallback.client,
    clientRelease: client ? data.tag_name : fallback.clientRelease,
    clientChecksums: client ? `${prefix}SHA256SUMS` : fallback.clientChecksums,
  };
}

export function withLatestClient(server: ReleaseInfo, candidates: unknown): ReleaseInfo {
  if (!Array.isArray(candidates)) return server;
  for (const candidate of candidates) {
    if (!candidate || typeof candidate !== 'object' || candidate.draft || candidate.prerelease
        || typeof candidate.tag_name !== 'string' || !Array.isArray(candidate.assets)) continue;
    const prefix = `${GITHUB_REPO}/releases/download/${candidate.tag_name}/`;
    const client = candidate.assets.find((asset: Record<string, unknown>) =>
      typeof asset.name === 'string' && /^Tutor_ACP_\d+\.\d+\.\d+_windows_x64-setup\.exe$/.test(asset.name)
        && typeof asset.browser_download_url === 'string' && asset.browser_download_url.startsWith(prefix)
        && typeof asset.size === 'number');
    const checksums = candidate.assets.find((asset: Record<string, unknown>) =>
      asset.name === 'SHA256SUMS' && typeof asset.browser_download_url === 'string'
        && asset.browser_download_url === `${prefix}SHA256SUMS`);
    if (client && checksums) return {
      ...server,
      client: { name: client.name, url: client.browser_download_url, size: client.size },
      clientVersion: client.name.match(/^Tutor_ACP_(\d+\.\d+\.\d+)_/)[1],
      clientRelease: candidate.tag_name,
      clientChecksums: checksums.browser_download_url,
    };
  }
  return server;
}

export async function fetchReleaseInfo(signal?: AbortSignal): Promise<ReleaseInfo> {
  const options = { signal, headers: { Accept: 'application/vnd.github+json' }, credentials: 'omit' as const };
  const response = await fetch(RELEASE_API, options);
  if (!response.ok) throw new Error(`GitHub unavailable (${response.status})`);
  const server = parseLatestRelease(await response.json());
  if (server.clientRelease === server.version) return server;
  // A newer server may ship without a new desktop client. Keep their versions independent.
  try {
    const releases = await fetch('https://api.github.com/repos/ArnaudGuiovanna/tutor-mcp/releases?per_page=20', options);
    return releases.ok ? withLatestClient(server, await releases.json()) : server;
  } catch {
    return server;
  }
}
