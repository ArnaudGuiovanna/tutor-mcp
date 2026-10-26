import type { Metadata } from 'next';
import { Footer, Header } from '@/components/chrome';
import { Downloads } from '@/components/downloads';

export const metadata: Metadata = {
  title: 'Download Tutor ACP and Tutor MCP',
  description: 'Get the Windows desktop client or the Tutor MCP server for Linux, macOS and Windows. Choose your architecture, verify the download and follow a guided installation.',
  alternates: { canonical: '/download/' },
};

export default function DownloadPage() {
  return <><Header current="download" /><main id="main"><div className="wrap"><header className="doc-head"><p className="eyebrow">Open source · MIT licensed</p><h1>Your next chapter starts here.</h1><p className="lead">Get Tutor ACP to learn with your community. Get Tutor MCP to run your own learning engine. Both are open source; your AI provider account stays separate.</p></header><Downloads /></div></main><Footer /></>;
}
