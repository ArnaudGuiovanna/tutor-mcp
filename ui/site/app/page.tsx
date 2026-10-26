import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';
import { ClientWindow, InstitutionPreview } from '@/components/mock';
import { CLIENT_VERSION, Meter, Pill, Providers, RELEASE, REPO, SectionTop, VERSION, Wordmark } from '@/components/ui';

const faq: [string, string][] = [
  ['What do I need to start?', 'On Windows x64, install Tutor ACP, connect Codex or Claude with your own provider account, then connect Tutor in local mode or join your institution. Tutor ACP includes the agent adapters and their runtime; you do not need to install Node.js separately. Local mode also requires the separate Tutor MCP server binary. If you already use an MCP client, you can install only the Tutor MCP server.'],
  ['Are Tutor MCP and Tutor ACP the same thing?', 'They work together. Tutor MCP is the server that stores courses, learning state and session memory and provides the pedagogical tools. Tutor ACP is the desktop application for conversations, courses and institution settings. You can also connect another compatible MCP client to Tutor MCP.'],
  ['Is everything free?', 'Both projects are open source under the MIT licence. You can use and self-host them without a Tutor subscription. Your chosen AI provider may charge for its service, and hosting an institution has infrastructure costs. Tutor does not include a language model.'],
  ['Where does my data live?', 'The learning state lives in the Tutor installation you choose: SQLite on your computer for local use, SQLite on your VPS for Hobby, or PostgreSQL for an institution. Tutor ACP stores conversations on your device. Your AI provider processes the content sent to it under its own terms; a local Tutor server does not make a remote AI model local.'],
  ['What happens when I share a course?', 'Tutor copies the course design, not your private conversations, observations or progress. Contributions follow the owner’s review workflow. An institution can offer a published version in its catalogue or invite named members. Joining remains voluntary, and enrolled learners keep their course version.'],
  ['Can I use several AI clients and keep my progress?', 'Yes, when they connect to the same Tutor installation and learner identity. Separate installations stay independent; nothing synchronizes implicitly. The connection guides explain which local and remote configurations each client supports.'],
  ['Is Tutor ready for everyone?', 'Tutor is alpha software. The repository documents its tests, supported configurations and known limits. Software tests establish implementation behaviour, not measured learning effectiveness. Mastery estimates are kept distinct from demonstrated skills.'],
  ['Can I modify it or use it commercially?', 'Yes. The MIT licence permits use, modification and redistribution, including commercially, while preserving its copyright and licence notices. It is provided as is, without warranty. The source and contribution guides are on GitHub.'],
];

const sharingSteps = [
  ['Create a private course', 'Start with a question or a learning goal. Build and refine the programme in your own space.'],
  ['Share its design', 'Create a copy for your institution. Your conversations, observations and personal progress stay out of that copy.'],
  ['Review, then publish', 'The owner examines contributions before publication. Offer the approved version in the catalogue or invite named members.'],
  ['Let people make it their own', 'Members choose to enrol and learn at their own pace. Their enrolled version stays stable as the course evolves.'],
];

export default function Home() {
  return (
    <>
      <Header current="home" />
      <main id="main">
        <section className="hero">
          <div className="wrap">
            <a className="badge-new" href={RELEASE}><b>Latest release</b> Tutor ACP {CLIENT_VERSION} · Tutor MCP {VERSION.replace(/^v/, '')} <span className="dim">→</span></a>
            <h1>Keep learning.<br />Grow together.</h1>
            <div className="sub">
              <p>Turn a question into a course. Pick up where you left off. Tutor MCP and its ACP client help you learn throughout your life, on your own and in learning communities.</p>
              <div className="row">
                <Link className="btn btn-light btn-lg" href="/download/">Download Tutor</Link>
                <Link className="btn btn-dark btn-lg" href="/docs/">Get started</Link>
              </div>
            </div>
            <div className="works"><span className="dim">Open source · Self-hosted</span><span>Desktop client for Windows x64</span><a href="#products">Already use an AI assistant? Connect Tutor MCP →</a></div>
            <div className="shot"><ClientWindow /></div>
            <p className="cap mock-example-note">Illustrative preview of Tutor ACP, using example course and learning data.</p>
          </div>
        </section>

        <section className="sec" id="products">
          <div className="wrap">
            <SectionTop n="1.0" title="Two open-source building blocks" />
            <div className="sec-head">
              <h2>A learning engine.<br />A place to learn.</h2>
              <div className="txt"><p>Your AI explains and practises with you. Tutor MCP gives that conversation continuity. Tutor ACP brings your conversations, courses and institution into one desktop application.</p></div>
            </div>
            <div className="visual grid g2 product-pair">
              <article className="card product-card">
                <span className="fig">The server · MCP</span>
                <h3>Tutor MCP</h3>
                <p>Stores your courses, progress, review schedule and session memory. It gives compatible AI clients the tools to choose what to practise, revisit or assess next.</p>
                <div className="works"><span className="dim">Connect with</span><Providers /><span>and other MCP clients</span></div>
                <div className="row"><Link className="btn btn-dark" href="/docs/local/">Use it with your MCP client</Link><Link className="more" href="/download/#server">Server downloads</Link></div>
              </article>
              <article className="card product-card">
                <span className="fig">The desktop application · ACP</span>
                <h3>Tutor ACP</h3>
                <p>Learn with Codex or Claude, resume conversations and manage your courses. Connect a local Tutor server or join your institution. Agent adapters and their runtime are included.</p>
                <p className="small">Windows x64 installer. Your AI provider account and its costs are separate.</p>
                <div className="row"><Link className="btn btn-light" href="/download/#client">Get the Windows client</Link><Link className="more" href="/docs/install-client/">Installation guide</Link></div>
              </article>
            </div>
          </div>
        </section>

        <section className="sec" id="institutions">
          <div className="wrap">
            <SectionTop n="2.0" title="Learning communities" />
            <div className="sec-head">
              <h2>Build a shared programme.<br />Leave room for every learner.</h2>
              <div className="txt">
                <p>A classroom, a training organisation, a team or a community of practice: institution mode gives your members a common place to find and develop courses.</p>
                <p>Share the programme and its learning goals. Each member keeps their own conversations, learning history and pace. The server enforces access and roles.</p>
                <div className="row"><Link className="btn btn-light" href="/docs/institutions/">Explore institution mode</Link><Link className="more" href="/docs/deploy-institution/">Host an institution</Link></div>
              </div>
            </div>
            <div className="visual institution-layout">
              <InstitutionPreview />
              <div className="institution-benefits">
                <article><span className="num">01</span><h3>A catalogue for your members</h3><p>Publish a reviewed version for the institution or offer it to named members. Members choose to enrol; an invitation does not enrol them automatically.</p></article>
                <article><span className="num">02</span><h3>Contribute with a clear review</h3><p>Authors and authorised contributors improve a programme. Proposals can be examined and refined before an owner approves publication.</p></article>
                <article><span className="num">03</span><h3>Keep learning on a stable version</h3><p>A new publication does not silently replace an enrolled learner’s programme. Changes to existing enrolments stay explicit.</p></article>
              </div>
            </div>
          </div>
        </section>

        <section className="sec" id="sharing">
          <div className="wrap">
            <SectionTop n="3.0" title="Share what you build" />
            <div className="sec-head">
              <h2>Your next course could help someone else start.</h2>
              <div className="txt"><p>Build something for your own learning, then contribute its design to your institution. A shared course can evolve without turning your private learning history into public content.</p><Link className="more" href="/docs/sharing-courses/">Follow the course-sharing guide</Link></div>
            </div>
            <ol className="visual sharing-flow">
              {sharingSteps.map(([title, text], index) => <li className="sharing-step" key={title}><span className="num">0{index + 1}</span><h3>{title}</h3><p>{text}</p></li>)}
            </ol>
            <p className="privacy-note"><b>The course design is shared. Your learning history is not.</b> Conversations, private observations and personal progress are not copied when you share a course.</p>
          </div>
        </section>

        <section className="sec" id="learner-state">
          <div className="wrap">
            <SectionTop n="4.0" title="Continuity, one session at a time" />
            <div className="sec-head">
              <h2>Come back to your learning, not a blank conversation.</h2>
              <div className="txt"><p>A new interest, a change of career or a course you left months ago: Tutor keeps the programme, the observations and the context needed to resume. Tell your tutor what you want to learn, then come back when you are ready for the next step.</p><Link className="more" href="/docs/first-session/">Start your first learning session</Link></div>
            </div>
            <div className="figs">
              <div><span className="fig">Fig 4.1 · continuity</span><div className="art"><span className="wm" style={{ fontSize: 34 }}>T<span className="cursor" /></span></div><h3>A course that stays with you</h3><p>Your learning goals, programme and session memory live in the Tutor installation you choose. Separate installations do not synchronise automatically.</p></div>
              <div><span className="fig">Fig 4.2 · evidence</span><div className="art" style={{ padding: '0 28px' }}><div style={{ width: '100%' }}><Meter label="Example mastery estimate" value={0.33} max={9999} /></div></div><h3>Progress with a reason</h3><p>Practice, reviews and assessments feed the learning state. An estimate is shown with its threshold; it is kept distinct from demonstrated skills.</p></div>
              <div><span className="fig">Fig 4.3 · your next step</span><div className="art"><div className="row"><Pill kind="review">Review due</Pill><Pill kind="prog">In progress</Pill></div></div><h3>Pick up at your own pace</h3><p>Resume a course, revisit a concept or work towards a new goal. The engine uses the recorded evidence to suggest the next activity.</p></div>
            </div>
          </div>
        </section>

        <section className="sec" id="client">
          <div className="wrap">
            <SectionTop n="5.0" title="Tutor ACP" />
            <div className="sec-head">
              <h2>Your conversations and courses, in one place.</h2>
              <div className="txt">
                <p>Conversation is where learning starts. Courses brings together your private programmes and the courses available to you in your institution. Refine a programme manually or examine an AI proposal in the same editor.</p>
                <p>Account and Settings cover agents, institution, members and reading preferences. The client supports French and English, light, dark and system themes, text size and reduced motion.</p>
                <div className="row"><Link className="btn btn-light" href="/download/">Download Tutor ACP</Link><Link className="more" href="/docs/install-client/">Set up the Windows client</Link></div>
              </div>
            </div>
            <div className="visual grid g3">
              <article className="card"><h3>Codex and Claude included</h3><p>The adapters and their runtime are bundled. Choose an agent and connect your provider account, without a separate Node.js installation.</p></article>
              <article className="card"><h3>Your identity, created locally</h3><p>For an institution, create or import your public-key identity and save its encrypted backup. Keep that backup so you can recover your identity.</p></article>
              <article className="card"><h3>Permissions you can revisit</h3><p>Allow, ask or refuse MCP tools for the selected account, server and agent. Change those choices in Settings; they never grant extra institution roles.</p></article>
            </div>
          </div>
        </section>

        <section className="sec" id="profiles">
          <div className="wrap">
            <SectionTop n="6.0" title="Choose where to run Tutor" />
            <div className="sec-head"><h2>Start on your computer.<br />Grow into a community.</h2><div className="txt"><p>The same server supports three deployment profiles. Start with the route that fits you; the guides separate everyday learning from server administration.</p><Link className="more" href="/docs/">Find your installation path</Link></div></div>
            <div className="visual grid g3">
              <article className="card"><span className="fig">tutor-mcp --local</span><h3>Local</h3><p>Your MCP client starts the server on your computer. SQLite stores your learning state; no Tutor account or hosted server is required.</p><Link className="more" href="/docs/local/">Start locally</Link></article>
              <article className="card"><span className="fig">tutor-mcp --profile hobby</span><h3>Hobby</h3><p>Run a personal server or a small group on a VPS, with SQLite and HTTPS. Members of this profile sign in with a username and password.</p><Link className="more" href="/docs/#hobby">See the Hobby profile</Link></article>
              <article className="card"><span className="fig">tutor-mcp --profile institution</span><h3>Institution</h3><p>A shared catalogue, public-key identities, members and reviewed contributions. PostgreSQL and separate service roles support the institution deployment.</p><Link className="more" href="/docs/deploy-institution/">Deploy for a community</Link></article>
            </div>
            <p className="cap">Local learning state does not mean a local AI model. Your selected provider processes the content sent to it. Use the client and provider configuration that fits your needs.</p>
          </div>
        </section>

        <section className="sec" id="architecture">
          <div className="wrap">
            <SectionTop n="7.0" title="Inside the learning engine" />
            <div className="sec-head"><h2>The AI explains.<br />Tutor MCP remembers and plans.</h2><div className="txt"><p>The model creates explanations, exercises and feedback. The engine maintains the learning state and selects activities from the recorded evidence. Its decisions are inspectable; their quality depends on the observations provided.</p><a className="more" href={REPO + '/blob/main/docs/algorithms.md'}>Read the algorithm guide</a></div></div>
            <div className="visual grid g3">
              <article className="card"><span className="fig">BKT · evidence gates</span><h3>Estimate and demonstrate</h3><p>Knowledge tracing updates estimates from responses. Assessment evidence is tracked separately, so a high probability alone does not certify mastery.</p></article>
              <article className="card"><span className="fig">FSRS-5 · prerequisites</span><h3>Review and progress</h3><p>Spaced review estimates retention. A prerequisite graph helps determine which concepts are accessible and what should be revisited.</p></article>
              <article className="card"><span className="fig">Session memory · decision replay</span><h3>Remember and inspect</h3><p>Versioned, encrypted narrative memory carries useful session context. Decision snapshots and replay help explain the recommended next step.</p></article>
            </div>
            <p className="cap">These are software capabilities, not a claim of measured learning effectiveness. Tutor is alpha, and its documentation describes the implementation and its limits.</p>
          </div>
        </section>

        <section className="sec" id="open-source">
          <div className="wrap">
            <SectionTop n="8.0" title="Open source, from the start" />
            <div className="sec-head"><h2>Use it. Understand it.<br />Help it improve.</h2><div className="txt"><p>The server, desktop client, documentation and deployment templates are in one repository. Self-host your installation, inspect how it works and contribute what you learn.</p><div className="row"><a className="btn btn-light" href={REPO}>Explore the source</a><a className="btn btn-dark" href={REPO + '/issues'}>Report an issue</a><a className="more" href={REPO + '/discussions'}>Join the discussion</a></div></div></div>
            <div className="visual log">
              <div><small>licence</small><b>MIT</b><p>Use, modify and redistribute, with the licence notice preserved.</p></div>
              <div><small>server</small><b>Linux · macOS · Windows</b><p>amd64 and arm64 releases, with checksums and dependency notices.</p></div>
              <div><small>desktop client</small><b>Windows x64</b><p>Codex and Claude adapters included. Your provider account stays separate.</p></div>
              <div className="cur"><small>{VERSION + ' · latest release'}</small><b>Tutor ACP {CLIENT_VERSION}</b><p>Download the current packages or read the release notes.</p><Link className="more" href="/download/">All downloads</Link></div>
            </div>
          </div>
        </section>

        <section className="sec" id="faq">
          <div className="wrap"><SectionTop n="9.0" title="FAQ" /><div className="sec-head"><h2>A few things before you start.</h2><div className="txt"><p>Choose your installation, connect your AI provider and start with one learning goal. The guides walk through each step.</p></div></div><div className="visual faq">{faq.map(([question, answer]) => <details key={question}><summary>{question}</summary><p>{answer}</p></details>)}</div></div>
        </section>

        <section className="final">
          <div className="wrap">
            <h2>Start with something<br />you want to learn.</h2>
            <p>A question today. A course you can come back to. A programme your community can build on.</p>
            <div className="row"><Link className="btn btn-light btn-lg" href="/download/">Download Tutor</Link><Link className="btn btn-dark btn-lg" href="/docs/first-session/">Your first session</Link><a className="btn btn-ghost btn-lg" href={REPO}>GitHub</a></div>
            <p className="cap" style={{ marginTop: 28 }}><Wordmark size={14} still /> · built by <a href="https://aguiovanna.fr">Arnaud Guiovanna</a> · MIT licensed</p>
          </div>
        </section>
      </main>
      <Footer />
    </>
  );
}
