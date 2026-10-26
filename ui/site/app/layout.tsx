import type { Metadata } from 'next';
import type { ReactNode } from 'react';
import { Inter, JetBrains_Mono } from 'next/font/google';
import './globals.css';

const inter = Inter({ subsets: ['latin'], variable: '--font-inter', display: 'swap', axes: ['opsz'] });
const mono = JetBrains_Mono({ subsets: ['latin'], weight: ['400', '500'], variable: '--font-mono', display: 'swap' });

const description = 'An open-source MCP engine for adaptive learning, self-learning and personalized AI tutoring. Bring course creation, a structured learning path and ongoing tutoring into the AI assistant you already use.';

export const metadata: Metadata = {
  metadataBase: new URL('https://tutor-mcp.dev'),
  title: { default: 'Tutor MCP — Learn throughout your life', template: '%s | Tutor MCP' },
  description,
  applicationName: 'Tutor MCP',
  authors: [{ name: 'Arnaud Guiovanna', url: 'https://aguiovanna.fr' }],
  creator: 'Arnaud Guiovanna',
  publisher: 'Tutor MCP',
  keywords: ['Tutor MCP', 'MCP tutor', 'Model Context Protocol', 'MCP server', 'AI tutor', 'intelligent tutoring system', 'self-hosted AI tutor', 'adaptive learning', 'learning community', 'open-source education', 'ACP client'],
  robots: { index: true, follow: true, googleBot: { index: true, follow: true, 'max-video-preview': -1, 'max-image-preview': 'large', 'max-snippet': -1 } },
  category: 'technology',
  alternates: { canonical: '/' },
  icons: { icon: '/icon.svg' },
  openGraph: {
    type: 'website',
    url: 'https://tutor-mcp.dev/',
    siteName: 'Tutor MCP',
    locale: 'en_US',
    title: 'Tutor MCP — Learn throughout your life',
    description,
    images: [{ url: '/opengraph-image.png', width: 1200, height: 630, alt: 'Tutor-MCP' }],
  },
  twitter: { card: 'summary_large_image', site: '@TutorMCP', creator: '@TutorMCP', title: 'Tutor MCP — Learn throughout your life', description, images: ['/opengraph-image.png'] },
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" className={`${inter.variable} ${mono.variable}`}>
      <body>
        <a className="skip" href="#main">Skip to content</a>
        {children}
      </body>
    </html>
  );
}
