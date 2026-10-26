import Link from 'next/link';
import { CLIENT_DOWNLOAD, RELEASE, REPO, VERSION, Wordmark } from './ui';

export function Header({ current }: { current?: 'home' | 'docs' | 'guide' }) {
  return (
    <header className="top">
      <div className="wrap">
        <Link href="/" aria-label="Tutor-MCP, home" style={{ textDecoration: 'none' }}><Wordmark /></Link>
        <nav aria-label="Site">
          <Link href="/" aria-current={current === 'home' ? 'page' : undefined}>Product</Link>
          <Link href="/docs/" aria-current={current === 'docs' ? 'page' : undefined}>Documentation</Link>
          <Link href="/guide/" aria-current={current === 'guide' ? 'page' : undefined}>User guide</Link>
          <Link href="/#architecture">Algorithms</Link>
        </nav>
        <div className="r">
          <a className="ver" href={RELEASE}>{VERSION}</a>
          <a className="btn btn-ghost" href={REPO}>GitHub</a>
          <a className="btn btn-light" href={CLIENT_DOWNLOAD}>Download</a>
        </div>
      </div>
    </header>
  );
}

export function Footer() {
  return (
    <footer>
      <div className="wrap">
        <div className="cols">
          <div>
            <Wordmark size={16} still />
            <span className="l" style={{ marginTop: 10, maxWidth: '34ch' }}>An open-source MCP engine for adaptive learning, and a desktop client for learning communities.</span>
            <a href="https://x.com/TutorMCP" style={{ marginTop: 6 }}>@TutorMCP on X</a>
          </div>
          <div><b>Product</b><Link href="/#learner-state">Learner state</Link><Link href="/#use-cases">Use cases</Link><Link href="/#architecture">Architecture</Link><Link href="/#client">ACP client</Link></div>
          <div><b>Install</b><Link href="/docs/#local">Local</Link><Link href="/docs/#hobby">Hobby VPS</Link><Link href="/docs/#institution">Institution</Link><Link href="/docs/#client">Windows client</Link></div>
          <div><b>Docs</b><Link href="/docs/">Get started</Link><Link href="/guide/">User guide</Link><Link href="/docs/#providers">Connect a client</Link><a href={`${REPO}/blob/main/docs/mcp-tools.md`}>MCP tools</a><a href={`${REPO}/blob/main/OPERATIONS.md`}>Operations</a></div>
          <div><b>Project</b><a href={REPO}>GitHub</a><a href={`${REPO}/blob/main/CHANGELOG.md`}>Changelog</a><a href={`${REPO}/blob/main/LICENSE`}>License (MIT)</a><a href={`${REPO}/issues`}>Issues</a><a href={`${REPO}/discussions`}>Discussions</a><a href={`${REPO}/blob/main/SECURITY.md`}>Security</a></div>
        </div>
        <div className="legal">
          <span>© 2026 Tutor MCP · MIT licensed · <Link href="/mentions-legales/">Legal notice</Link></span>
          <span>Built by <a href="https://aguiovanna.fr">Arnaud Guiovanna</a> · {VERSION}</span>
        </div>
      </div>
    </footer>
  );
}
