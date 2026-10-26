'use client';

import Link from 'next/link';
import { useEffect, useState } from 'react';
import { CodeBlock } from './code-block';
import { fetchReleaseInfo, GITHUB_REPO, releaseSnapshot } from '@/lib/releases';

const systems = [
  { id: 'linux', label: 'Linux', extension: 'tar.gz' },
  { id: 'darwin', label: 'macOS', extension: 'tar.gz' },
  { id: 'windows', label: 'Windows', extension: 'zip' },
] as const;

export function Downloads() {
  const [release, setRelease] = useState(releaseSnapshot);
  const [checked, setChecked] = useState(false);
  const [system, setSystem] = useState<(typeof systems)[number]['id']>('linux');
  const [architecture, setArchitecture] = useState('amd64');

  useEffect(() => {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 8000);
    fetchReleaseInfo(controller.signal)
      .then(data => { setRelease(data); setChecked(true); })
      .catch(() => { /* Verified links remain usable when GitHub is unavailable. */ })
      .finally(() => clearTimeout(timeout));
    return () => { clearTimeout(timeout); controller.abort(); };
  }, []);

  const selected = systems.find(item => item.id === system)!;
  const filename = `tutor-mcp_${system}_${architecture}.${selected.extension}`;
  const asset = release.assets.find(item => item.name === filename);
  const checksums = release.assets.find(item => item.name === 'SHA256SUMS');
  const command = system === 'windows'
    ? `Invoke-WebRequest -Uri "https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/${release.version}/scripts/install.ps1" -OutFile install-tutor.ps1\n.\\install-tutor.ps1 -Version ${release.version}\n& "$env:LOCALAPPDATA\\Programs\\tutor-mcp\\tutor-mcp.exe" --version`
    : `curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/${release.version}/scripts/install.sh -o install-tutor.sh\nTUTOR_MCP_VERSION=${release.version} TUTOR_MCP_INSTALL_DIR="$HOME/.local/bin" sh install-tutor.sh\n"$HOME/.local/bin/tutor-mcp" --version`;

  return <div className="download-content">
    <p className="release-status" role="status"><span className="release-dot" aria-hidden="true" />{checked ? 'Latest stable release checked on GitHub' : 'Verified release links · live updates when GitHub is available'}<a href={release.url}>Tutor MCP {release.version} ↗</a></p>
    <section className="download-client" id="client" aria-labelledby="client-title">
      <div><p className="eyebrow">01 / Your desktop learning space</p><h2 id="client-title">Tutor ACP<span className="download-version">{release.clientVersion}</span></h2>
        <p className="download-lead">Your courses, conversations and progress. One place to learn with Codex or Claude and join your institution’s catalogue.</p>
        <div className="row"><a className="btn btn-light btn-lg" href={release.client.url}>Download for Windows x64 <span aria-hidden="true">↓</span></a><Link className="btn btn-dark btn-lg" href="/docs/install-client/">Installation guide <span aria-hidden="true">→</span></Link></div>
        <p className="cap">Windows x64 · {Math.round(release.client.size / 1_000_000)} MB · Codex and Claude adapters included</p><p className="cap">The desktop installer is currently available for Windows x64. macOS and Linux users can connect another MCP client to the server below.</p>
      </div>
      <div className="download-start"><span className="fig">Your first three steps</span><ol><li><b>Install the app.</b><span>No separate Node.js or Codex Desktop installation.</span></li><li><b>Connect your agent.</b><span>Choose Codex or Claude and sign in with your provider.</span></li><li><b>Choose where you learn.</b><span>Join your institution or <Link href="/docs/local/">install Tutor MCP locally</Link>.</span></li></ol><a className="small" href={release.clientChecksums}>Client SHA-256 checksums ↗</a></div>
    </section>
    <section className="download-server" id="server" aria-labelledby="server-title">
      <div className="sec-top"><b>02</b> The learning engine <span className="dim">→</span></div><div className="sec-head"><h2 id="server-title">Tutor MCP.<br />Run it where you choose.</h2><div className="txt"><p>Keep a local learning profile or host a shared service. One server binary, six platform builds. Tutor MCP is separate from the desktop app and the language model.</p></div></div>
      <div className="download-server-grid">
        <div className="server-picker card"><div className="picker-fields"><label>Operating system<select value={system} onChange={event => setSystem(event.target.value as typeof system)}>{systems.map(item => <option value={item.id} key={item.id}>{item.label}</option>)}</select></label><label>Processor<select value={architecture} onChange={event => setArchitecture(event.target.value)}><option value="amd64">x64 · Intel / AMD</option><option value="arm64">ARM64{system === 'darwin' ? ' · Apple Silicon' : ''}</option></select></label></div>
          <p className="small muted">On a Mac, choose ARM64 for an Apple M-series chip or x64 for Intel. Windows ARM64 and Linux ARM64 downloads here are server builds.</p><div className="selected-asset"><span className="fig">{release.version}</span><code>{filename}</code>{asset && <span className="small muted">{(asset.size / 1_000_000).toFixed(1)} MB</span>}</div>
          {asset ? <a className="btn btn-light btn-lg" href={asset.url}>Download server <span aria-hidden="true">↓</span></a> : <a className="btn btn-dark btn-lg" href={release.url}>View available release assets ↗</a>}
          <div className="download-links"><a href={checksums?.url ?? release.url}>SHA-256 checksums ↗</a><a href={release.url}>Release notes and all assets ↗</a></div>
        </div>
        <div className="server-install"><h3>Or use the {system === 'windows' ? 'PowerShell' : 'Linux / macOS'} installer.</h3><p className="muted">Download and inspect the script, then run it as your normal user. It detects the machine’s architecture, checks SHA-256 and installs the licence notices.</p><CodeBlock code={command} label={system === 'windows' ? 'PowerShell' : 'Terminal'} /><p className="small muted">Expected: <code>tutor-mcp {release.version}</code>. Then <Link href="/docs/local/">connect your local client</Link> or <Link href="/docs/deploy-institution/">deploy an institution</Link>.</p></div>
      </div>
    </section>
    <section className="download-next grid g3" aria-label="After downloading">
      <Link className="card guide-link" href="/docs/install-client/"><span className="fig">For learners</span><h3>Join your institution <span aria-hidden="true">↗</span></h3><p>Set up your identity, connect your agent and open a shared course.</p></Link>
      <Link className="card guide-link" href="/docs/local/"><span className="fig">On your computer</span><h3>Start locally <span aria-hidden="true">↗</span></h3><p>Connect the engine to Tutor ACP or an existing MCP client.</p></Link>
      <Link className="card guide-link" href="/docs/deploy-institution/"><span className="fig">For operators</span><h3>Host a community <span aria-hidden="true">↗</span></h3><p>Plan PostgreSQL, HTTPS, keys, members and your first programme.</p></Link>
    </section>
    <div className="download-notes"><p>Tutor is MIT licensed and currently in alpha. AI provider accounts and any associated costs are separate. Installing open-source software does not include a hosted Tutor service.</p><p>No GitHub account is needed to download. If the live release check is unavailable, these verified links still work. <a href={`${GITHUB_REPO}/releases/latest`}>See the latest stable release on GitHub ↗</a>.</p></div>
  </div>;
}
