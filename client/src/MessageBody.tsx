import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { ClientBridge } from './bridge';

export function MessageBody({ text, openLink }: { text: string; openLink?: ClientBridge['openExternalLink'] }) {
  return <div className="markdown-body"><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{
    h1: ({children})=><h3>{children}</h3>, h2:({children})=><h3>{children}</h3>, h3:({children})=><h4>{children}</h4>, h4:({children})=><h5>{children}</h5>,h5:({children})=><h6>{children}</h6>,
    a: ({ href, children }) => <a href={href} rel="noopener noreferrer" target="_blank" onClick={openLink ? (event) => { event.preventDefault(); if (href) void openLink(href); } : undefined}>{children}</a>,
    img: ({ src, alt }) => <span className="message-image-reference">Image : {alt || 'illustration'}{src && <a href={src} rel="noopener noreferrer" target="_blank" onClick={openLink ? (event) => { event.preventDefault(); void openLink(src); } : undefined}>Ouvrir l’image</a>}</span>,
    table: ({ children }) => <div className="message-table" role="region" aria-label="Tableau de la réponse" tabIndex={0}><table>{children}</table></div>,
    pre:({children})=><pre tabIndex={0} role="region" aria-label="Extrait de code">{children}</pre>,
  }}>{text}</ReactMarkdown></div>;
}
