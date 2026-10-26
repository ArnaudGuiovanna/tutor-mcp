import type { Metadata } from 'next';
import Link from 'next/link';
import { notFound } from 'next/navigation';
import { Footer, Header } from '@/components/chrome';
import { REPO, VERSION } from '@/components/ui';
import { installationGuides } from '@/lib/installation-guides';
import { learningGuides } from '@/lib/learning-guides';

const guides = [...installationGuides, ...learningGuides];
export const dynamicParams = false;

export function generateStaticParams() {
  return guides.map(guide => ({ slug: guide.slug }));
}

export async function generateMetadata({ params }: { params: Promise<{ slug: string }> }): Promise<Metadata> {
  const { slug } = await params;
  const guide = guides.find(item => item.slug === slug);
  return guide ? { title: guide.title, description: guide.description, alternates: { canonical: `/docs/${slug}/` } } : {};
}

export default async function DocumentationPage({ params }: { params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  const guide = guides.find(item => item.slug === slug);
  if (!guide) notFound();
  const related = guide.related.flatMap(item => guides.find(candidate => candidate.slug === item) ?? []);
  return <>
    <Header current="docs" />
    <main id="main">
      <div className="wrap">
        <header className="doc-head">
          <Link className="eyebrow" href="/docs/">Documentation / {guide.category}</Link>
          <h1>{guide.title}</h1>
          <p className="lead">{guide.description}</p>
          <div className="facts"><span className="pill">{guide.audience}</span><span className="pill">{guide.readTime}</span><span className="pill">Reference release {VERSION}</span></div>
        </header>
        <div className="doc-layout">
          <nav className="toc" aria-label="On this page">
            <div className="h">In this guide</div>
            {guide.sections.map(section => <a key={section.id} href={`#${section.id}`}>{section.title}</a>)}
            <div className="h">Keep exploring</div>
            <Link href="/docs/">All guides</Link><Link href="/download/">Downloads</Link><Link href="/guide/">Learning with Tutor</Link>
          </nav>
          <article className="prose">
            {guide.sections.map((section, index) => <section id={section.id} key={section.id}><span className="eyebrow">{String(index + 1).padStart(2, '0')}</span><h2>{section.title}</h2>{section.content}</section>)}
            {related.length > 0 && <section><span className="eyebrow">Next steps</span><h2>Keep going.</h2><div className="grid g2 related-guides">{related.map(item => <Link key={item.slug} className="card guide-link" href={`/docs/${item.slug}/`}><span className="fig">{item.category}</span><h3>{item.title}<span aria-hidden="true"> ↗</span></h3><p>{item.description}</p></Link>)}</div></section>}
            <section className="doc-sources"><h2>Source documentation</h2><p>This guide follows the project’s current documentation. For the full contracts and operator details:</p><ul>{guide.sources.map(source => <li key={source.path}><a href={`${REPO}/blob/main/${source.path}`}>{source.label} ↗</a></li>)}</ul><p className="small">Found something unclear? <a href={`${REPO}/issues`}>Report a documentation issue</a>.</p></section>
          </article>
        </div>
      </div>
    </main>
    <Footer />
  </>;
}
