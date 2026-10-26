import { t } from './i18n';
import { Children, isValidElement, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { ClientBridge } from './bridge';

function childText(value:ReactNode):string { return Children.toArray(value).map(child=>typeof child==='string'||typeof child==='number'?String(child):isValidElement<{children?:ReactNode}>(child)?childText(child.props.children):'').join(''); }
function CodeBlock({children}:{children:ReactNode}) {
  const [copied,setCopied]=useState(false);const [error,setError]=useState(false);
  return <div className="code-block"><div className="code-block-header"><button className="link-button" onClick={()=>void navigator.clipboard.writeText(childText(children)).then(()=>{setCopied(true);setError(false);}).catch(()=>setError(true))}>{copied?t("Copié"):t("Copier le code")}</button>{error&&<span role="status">{t("La copie a échoué. Sélectionnez le code.")}</span>}</div><pre tabIndex={0} role="region" aria-label={t("Extrait de code")}>{children}</pre></div>;
}
function ResponsiveTable({children}:{children:ReactNode}) {
  const root=useRef<HTMLDivElement>(null);const [width,setWidth]=useState(800);
  useEffect(()=>{if(typeof ResizeObserver==='undefined'||!root.current)return;const observer=new ResizeObserver(entries=>setWidth(entries[0].contentRect.width));observer.observe(root.current);return()=>observer.disconnect();},[]);
  const sections=Children.toArray(children).filter(isValidElement<{children?:ReactNode}>);
  const rows=sections.flatMap(section=>Children.toArray(section.props.children).filter(isValidElement<{children?:ReactNode}>));
  const cells=rows.map(row=>Children.toArray(row.props.children).filter(isValidElement<{children?:ReactNode}>).map(cell=>cell.props.children));
  const headers=cells[0]||[];const stacked=headers.length>4||headers.length*170>width;
  return <div ref={root} className="message-table" role="region" aria-label={t("Tableau de la réponse")} tabIndex={0}>{stacked?<div>{cells.slice(1).map((row,index)=><div className="stacked-table-row" key={index}><dl>{row.map((value,i)=><div key={i}><dt>{headers[i]||t('Colonne {number}',{number:i+1})}</dt><dd>{value}</dd></div>)}</dl></div>)}</div>:<table>{children}</table>}</div>;
}
export function MessageBody({text,openLink}:{text:string;openLink?:ClientBridge['openExternalLink']}) {
  return <div className="markdown-body"><ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml components={{
    h1:({children})=><h3>{children}</h3>,h2:({children})=><h3>{children}</h3>,h3:({children})=><h4>{children}</h4>,h4:({children})=><h5>{children}</h5>,h5:({children})=><h6>{children}</h6>,
    a:({href,children})=><a href={href} rel="noopener noreferrer" target="_blank" onClick={openLink?event=>{event.preventDefault();if(href)void openLink(href);}:undefined}>{children}</a>,
    img:({src,alt})=><span className="message-image-reference">{t("Image :")} {alt||'illustration'}{src&&<a href={src} rel="noopener noreferrer" target="_blank" onClick={openLink?event=>{event.preventDefault();void openLink(src);}:undefined}>{t("Ouvrir l’image")}</a>}</span>,
    table:({children})=><ResponsiveTable>{children}</ResponsiveTable>,pre:({children})=><CodeBlock>{children}</CodeBlock>,
  }}>{text}</ReactMarkdown></div>;
}
