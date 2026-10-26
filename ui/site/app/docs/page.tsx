import type { Metadata } from 'next';
import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';
import { REPO } from '@/components/ui';

export const metadata: Metadata = {
  title: 'Documentation — choose your Tutor journey',
  description: 'Install Tutor ACP, join an institution, learn with Tutor MCP on your own computer or deploy a learning service for your community. Practical guides from first connection to sharing courses.',
  alternates: { canonical: '/docs/' },
};

const guides = [
  { href: '/docs/install-client/', title: 'Install Tutor ACP', text: 'Set up the Windows application, connect your agent and join your institution.', audience: 'Start with the desktop client' },
  { href: '/docs/first-session/', title: 'Your first learning session', text: 'Choose a course, explain your goal, practise and return with your progress intact.', audience: 'Learn and continue' },
  { href: '/docs/sharing-courses/', title: 'Create and share a course', text: 'Prepare a programme, choose an audience and review contributions before publication.', audience: 'Learn and share' },
  { href: '/docs/institutions/', title: 'Learn in an institution', text: 'Understand the shared catalogue, member roles and individual learning paths.', audience: 'Learning communities' },
  { href: '/docs/local/', title: 'Run Tutor on your computer', text: 'Install the MCP server and keep a local learning profile with no server account.', audience: 'Local installation' },
  { href: '/docs/connect-mcp-client/', title: 'Connect an MCP client', text: 'Use the right local or remote connection for the AI application you already use.', audience: 'Bring your own client' },
  { href: '/docs/hobby/', title: 'Host a small Tutor service', text: 'Set up a personal or small-group VPS with SQLite and an HTTPS endpoint.', audience: 'Hobby hosting' },
  { href: '/docs/deploy-institution/', title: 'Deploy an institution', text: 'Prepare PostgreSQL, runtime roles, public-key membership and your first owner.', audience: 'Institution operators' },
  { href: '/docs/backup-and-updates/', title: 'Backups and updates', text: 'Protect your identity, conversation files and learning record before upgrading.', audience: 'Keep your learning safe' },
  { href: '/docs/troubleshooting/', title: 'Troubleshooting', text: 'Work through connection, identity, permission and installation problems.', audience: 'Find a next step' },
];

export default function Documentation() {
  return <>
    <Header current="docs" />
    <main id="main">
      <div className="wrap">
        <header className="doc-head">
          <p className="eyebrow">Documentation · A useful next step</p>
          <h1>Start where you are.</h1>
          <p className="lead">Join a learning community, keep a tutor on your own computer, or run a service for your team. Choose your path and follow the steps that matter to you.</p>
          <div className="facts"><Link className="pill" href="/guide/">Beginner’s guide →</Link><Link className="pill" href="/download/">Downloads</Link><span className="pill">Open source · MIT</span></div>
        </header>

        <div className="doc-layout">
          <nav className="toc" aria-label="Documentation sections">
            <div className="h">Find your path</div>
            <a href="#quickstart">Choose a starting point</a>
            <a href="#architecture">How the parts fit</a>
            <div className="h">Make it work</div>
            <a href="#install">All practical guides</a>
            <a href="#providers">Connect your AI</a>
            <a href="#test">Check your first session</a>
            <div className="h">Keep going</div>
            <a href="#backup">Backups and updates</a>
            <a href="#reference">Technical reference</a>
          </nav>

          <div className="prose">
            <section id="quickstart">
              <span className="eyebrow">01 · Choose your path</span>
              <h2>What would you like to do?</h2>
              <div className="grid g3" id="requirements">
                <article className="card" id="client">
                  <span className="num">For members</span>
                  <h3>Join my institution</h3>
                  <p>Your organisation already runs Tutor. Install the Windows client, connect your agent and join with your authorised identity.</p>
                  <Link className="more" href="/docs/install-client/">Install Tutor ACP</Link>
                </article>
                <article className="card" id="local">
                  <span className="num">For independent learners</span>
                  <h3>Learn on my computer</h3>
                  <p>Install Tutor MCP and connect a compatible AI client. Your local profile needs no Tutor account or HTTP server.</p>
                  <Link className="more" href="/docs/local/">Set up a local tutor</Link>
                </article>
                <article className="card" id="institution">
                  <span className="num">For operators</span>
                  <h3>Run Tutor for my team</h3>
                  <p>Provide a shared catalogue, member access and reviewed programmes on infrastructure you operate.</p>
                  <Link className="more" href="/docs/deploy-institution/">Deploy an institution</Link>
                </article>
              </div>
              <p id="hobby" style={{ marginTop: 20 }}>Running a personal VPS or a small service instead? The <Link href="/docs/hobby/">hobby profile</Link> uses SQLite and a simpler account workflow.</p>
            </section>

            <section id="architecture">
              <span className="eyebrow">02 · Two parts, one learning journey</span>
              <h2>The application and the engine.</h2>
              <p><b>Tutor ACP</b> is the desktop application for conversations, courses and institutional settings. It includes the Codex and Claude adapters; their provider connection remains separate from your Tutor membership.</p>
              <p><b>Tutor MCP</b> is the learning service. It keeps the programme, learning evidence and review schedule so you can return to your course. You can run it locally, host a hobby service or operate an institution.</p>
              <p id="memory">Continue with the same Tutor installation to retain the same server-side learning history. A different installation does not automatically receive your profile.</p>
            </section>

            <section id="install">
              <span className="eyebrow">03 · Practical guides</span>
              <h2>One guide for each next step.</h2>
              <div className="grid g2">
                {guides.map(guide => <article className="card" key={guide.href}>
                  <span className="num">{guide.audience}</span>
                  <h3><Link href={guide.href}>{guide.title}</Link></h3>
                  <p>{guide.text}</p>
                </article>)}
              </div>
            </section>

            <section id="providers">
              <span className="eyebrow">04 · Connect</span>
              <h2>Keep the connections clear.</h2>
              <p>In Tutor ACP, choose <b>Codex or Claude</b> and connect through the provider’s supported account workflow. Then connect Tutor with your local profile or institution identity. Provider access and any related costs remain separate.</p>
              <p id="claude-code">If you already use <b>Claude Code, Claude Desktop, Hermes or another compatible MCP client</b>, connect the Tutor MCP server directly. Local desktop and command-line clients can start it over stdio.</p>
              <p id="remote">Cloud-based clients need a compatible public HTTPS service. The hobby OAuth connection and the native institution identity workflow are different: use the setup for your actual profile.</p>
              <Link className="more" href="/docs/connect-mcp-client/">Choose the right MCP connection</Link>
            </section>

            <section id="test">
              <span className="eyebrow">05 · Check your setup</span>
              <h2>Make the first session count.</h2>
              <ol>
                <li>Confirm that your agent and Tutor connections are ready.</li>
                <li>Choose a course you can access, or create a personal programme if your institution allows it.</li>
                <li>Complete an activity and ask the tutor to save a session summary and next step.</li>
                <li>Reopen the same account and course, then ask to continue.</li>
              </ol>
              <p id="troubleshooting">If a connection or permission blocks the journey, follow <Link href="/docs/troubleshooting/">Troubleshooting</Link>. For a guided example, read <Link href="/guide/">Your first practical session</Link>.</p>
            </section>

            <section id="backup">
              <span className="eyebrow">06 · Look after your learning</span>
              <h2>Keep a way back.</h2>
              <p>Your identity export, desktop conversations and service database are different backups. Save your encrypted identity before you need recovery, and protect the data you operate before a software upgrade.</p>
              <Link className="more" href="/docs/backup-and-updates/">Back up and update Tutor</Link>
            </section>

            <section id="reference">
              <span className="eyebrow">07 · Go deeper</span>
              <h2>Source and technical reference.</h2>
              <p>The repository contains the complete deployment and integration reference. These documents are useful when you operate, extend or audit Tutor.</p>
              <ul>
                <li><a href={`${REPO}/blob/main/README.md`}>Project README</a> — capabilities, profiles and supported clients.</li>
                <li><a href={`${REPO}/blob/main/docs/installation.md`}>Installation reference</a> — service deployment, roles, migrations and restore.</li>
                <li><a href={`${REPO}/blob/main/docs/architecture.md`}>Architecture</a> and <a href={`${REPO}/blob/main/docs/algorithms.md`}>algorithms</a> — how learning state and activity decisions work.</li>
                <li><a href={`${REPO}/blob/main/docs/mcp-tools.md`}>MCP tool reference</a> — integration contracts for developers.</li>
                <li><a href={`${REPO}/blob/main/docs/learning-integrity.md`}>Learning evidence</a> — what progress claims mean.</li>
                <li><a href={`${REPO}/blob/main/OPERATIONS.md`}>Operations</a> — institutional runtime and recovery requirements.</li>
              </ul>
            </section>
          </div>
        </div>
      </div>
    </main>
    <Footer />
  </>;
}
