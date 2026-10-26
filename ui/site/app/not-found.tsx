import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';

export default function NotFound() {
  return <><Header /><main id="main"><div className="wrap"><header className="doc-head" style={{ paddingBottom: 120 }}><p className="eyebrow">404 / Page not found</p><h1>Let’s find your next step.</h1><p className="lead">This page may have moved. Start with the documentation, choose a download or return to the product.</p><div className="row" style={{ marginTop: 32 }}><Link className="btn btn-light" href="/docs/">Browse the guides</Link><Link className="btn btn-dark" href="/download/">Downloads</Link><Link className="btn btn-ghost" href="/">Back to home →</Link></div></header></div></main><Footer /></>;
}
