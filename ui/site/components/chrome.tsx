import Link from 'next/link';
import { RELEASE, REPO, VERSION, Wordmark } from './ui';

type Page = 'home' | 'docs' | 'guide' | 'download';
const navigation: { href: string; label: string; page?: Page }[] = [
  { href: '/', label: 'Product', page: 'home' },
  { href: '/#institutions', label: 'Institutions' },
  { href: '/docs/', label: 'Documentation', page: 'docs' },
  { href: '/guide/', label: 'User guide', page: 'guide' },
];

export function Header({ current }: { current?: Page }) {
  return <header className="top"><div className="wrap">
    <Link href="/" aria-label="Tutor-MCP, home" className="brand-link"><Wordmark /></Link>
    <nav className="desktop-navigation" aria-label="Main navigation">{navigation.map(item => <Link key={item.href} href={item.href} aria-current={item.page !== undefined && current === item.page ? 'page' : undefined}>{item.label}</Link>)}</nav>
    <div className="r"><a className="ver" href={RELEASE}>{VERSION}</a><a className="btn btn-ghost" href={REPO} aria-label="Tutor MCP on GitHub">GitHub ↗</a><Link className="btn btn-light" href="/download/" aria-current={current === 'download' ? 'page' : undefined}>Download <span aria-hidden="true">↓</span></Link></div>
    <details className="mobile-navigation"><summary aria-label="Open navigation menu"><span className="menu-lines" aria-hidden="true" /><span className="visually-hidden">Menu</span></summary><nav aria-label="Mobile navigation">{navigation.map(item => <Link key={item.href} href={item.href} aria-current={item.page !== undefined && current === item.page ? 'page' : undefined}>{item.label}</Link>)}<Link href="/download/">Downloads</Link><a href={REPO}>GitHub ↗</a></nav></details>
  </div></header>;
}

export function Footer() {
  return <footer><div className="wrap"><div className="cols">
    <div><Wordmark size={16} still /><span className="l" style={{ marginTop: 10, maxWidth: '34ch' }}>A learning engine that remembers. A community to grow with. Open source, under your control.</span><a href="https://x.com/TutorMCP" style={{ marginTop: 6 }}>@TutorMCP on X ↗</a></div>
    <div><b>Discover</b><Link href="/#products">The two building blocks</Link><Link href="/#institutions">Institutions</Link><Link href="/#sharing">Share a course</Link><Link href="/#architecture">How the engine works</Link></div>
    <div><b>Get started</b><Link href="/download/">Downloads</Link><Link href="/docs/install-client/">Install Tutor ACP</Link><Link href="/docs/local/">Learn locally</Link><Link href="/docs/deploy-institution/">Host an institution</Link></div>
    <div><b>Learn and share</b><Link href="/docs/">Documentation</Link><Link href="/guide/">User guide</Link><Link href="/docs/first-session/">Your first session</Link><Link href="/docs/sharing-courses/">Sharing courses</Link><Link href="/docs/backup-and-updates/">Backup and updates</Link></div>
    <div><b>Open source</b><a href={REPO}>GitHub ↗</a><a href={RELEASE}>Latest release ↗</a><a href={`${REPO}/blob/main/LICENSE`}>MIT licence ↗</a><a href={`${REPO}/issues`}>Issues ↗</a><a href={`${REPO}/discussions`}>Discussions ↗</a><a href={`${REPO}/blob/main/SECURITY.md`}>Security ↗</a></div>
  </div><div className="legal"><span>© 2026 Tutor MCP · MIT licensed · <Link href="/mentions-legales/">Legal notice</Link></span><span>Built by <a href="https://aguiovanna.fr">Arnaud Guiovanna</a> · {VERSION}</span></div></div></footer>;
}
