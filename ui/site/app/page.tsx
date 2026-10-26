import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';
import { ClientWindow } from '@/components/mock';
import { CLIENT_DOWNLOAD, Kbd, Meter, Pill, Providers, RELEASE, REPO, SectionTop, Wordmark } from '@/components/ui';

const faq: [string, string][] = [
  ['Do I need an account with Anthropic, OpenAI, Mistral or Google?', 'Tutor MCP does not ship a language model. It plugs into the assistant you already use: Claude Code, Claude Desktop, ChatGPT, Hermes, Pi, Gemini CLI, Le Chat and other MCP clients. Tutor ACP bundles the Codex and Claude adapters; your provider account and its costs stay separate from Tutor. The conversation lives in that client; the learning state lives in your Tutor installation.'],
  ['Where does my learning data live?', 'In the Tutor installation you choose. Local: a private SQLite directory on your machine, ~/.tutor-mcp/local. Hobby: SQLite on your own VPS behind HTTPS. Institution: PostgreSQL with tenant isolation and operator-managed keys. Narrative memory is encrypted and versioned in the database. There is no telemetry.'],
  ['Can I learn with several AI clients and keep one history?', 'Yes. Connect them to the same Tutor installation, locally over stdio or remotely over HTTPS with OAuth. Multiple local clients can share one profile and its learner identity. Separate installations stay independent; nothing synchronizes implicitly.'],
  ['How is this different from flashcards, a language app or a prompt around a chatbot?', 'A flashcard app schedules reviews. A language app plays a fixed item bank. A prompt around a chatbot forgets. Tutor MCP is the layer between them: a persistent, auditable engine that tracks estimates, retention, demonstrated skills, transfer, misconceptions and narrative memory, and decides what comes next. The AI creates and explains.'],
  ['Is this production-ready?', 'Tutor MCP is in alpha. The engine, the three profiles and the Windows client are tested in CI, and only the latest release tag and main receive fixes. Software tests establish implementation behaviour, not measured learning effectiveness; estimates are never presented as proof of mastery.'],
  ['Can I host this for a classroom, a team or a training organization?', 'Yes. The institution profile runs on PostgreSQL with separate API, worker and migrator roles. Members use public-key identities and the Tutor ACP client; owners manage membership and access; authors and authorised trainers maintain programmes. The hobby profile fits a small group on one VPS.'],
  ['Why Go?', 'One static binary per platform, pure-Go SQLite without CGO, low memory and concurrency for parallel learner loops. Binaries target Linux, macOS and Windows on amd64 and arm64; building from source needs Go 1.26.10 or a newer patched release.'],
  ['What is the license? Can I fork it or use it commercially?', 'MIT. You can use, copy, modify, merge, publish, distribute, sublicense and sell copies, including commercially, as long as the copyright and license notice stay with the software. It is provided “as is”, without warranty. The name, logo and this site remain their author’s.'],
];

export default function Home() {
  return (
    <>
      <Header current="home" />
      <main id="main">
        <section className="hero">
          <div className="wrap">
            <a className="badge-new" href={RELEASE}><b>New</b> Tutor ACP 0.0.1 for Windows · Tutor MCP 0.7.0 <span className="dim">→</span></a>
            <h1>Learn throughout your life. Grow through learning communities.</h1>
            <div className="sub">
              <p>Tutor MCP is an open-source engine for adaptive learning and personalized AI tutoring. It brings course creation, a structured learning path and ongoing tutoring into the AI assistant you already use, and gives institutions a desktop client to learn together.</p>
              <div className="row">
                <a className="btn btn-light btn-lg" href={CLIENT_DOWNLOAD}>Download Tutor ACP</a>
                <Link className="btn btn-dark btn-lg" href="/docs/">Read the docs</Link>
              </div>
            </div>
            <div className="works"><span className="dim">Works with</span><Providers /><span className="dim">and any MCP client</span></div>
            <div className="shot"><ClientWindow /></div>
          </div>
        </section>

        <section className="sec" style={{ paddingTop: 64 }}>
          <div className="wrap">
            <div className="sec-head" style={{ marginTop: 0 }}>
              <h2>Generative teaching. Deterministic pedagogy.</h2>
              <div className="txt"><p>The AI creates lessons, exercises and explanations. A persistent, auditable engine governs progression: what to learn, practice or revisit next. <b>Three rules guide every decision.</b></p></div>
            </div>
            <div className="figs">
              <div>
                <span className="fig">Fig 0.1</span>
                <div className="art"><span className="wm" style={{ fontSize: 34 }}>T<span className="cursor" /></span></div>
                <h4>Local first</h4><p>One binary and SQLite. <code>tutor-mcp --local</code> needs no account, no server and no data leaving your machine.</p>
              </div>
              <div>
                <span className="fig">Fig 0.2</span>
                <div className="art" style={{ padding: '0 28px' }}><div style={{ width: '100%' }}><Meter label="P(mastery)" value={0.33} max={9999} /></div></div>
                <h4>Evidence-driven</h4><p>An estimate is not mastery. Every number ships with its threshold and its source; demonstrated skills stay distinct from estimates.</p>
              </div>
              <div>
                <span className="fig">Fig 0.3</span>
                <div className="art"><div className="row" style={{ gap: 6 }}><Kbd>Ctrl</Kbd><Kbd>K</Kbd></div></div>
                <h4>Built for communities</h4><p>Institutions publish programmes to their members. Each learner keeps their own pace, conversations and progress.</p>
              </div>
            </div>
          </div>
        </section>

        <section className="sec" id="learner-state">
          <div className="wrap">
            <SectionTop n="1.0" title="Learner state" />
            <div className="sec-head">
              <h2>The missing layer is not content. It is continuity.</h2>
              <div className="txt">
                <p>Your AI handles the conversation. Tutor MCP gives it two persistent layers, so the next session starts from what you have mastered, forgotten, misunderstood and committed to do next.</p>
                <p><b>The loop is simple:</b> choose an activity, teach and practice, record the response, update the learning state. The client calls <code>get_next_activity</code> for guidance and <code>record_interaction</code> to save observations.</p>
              </div>
            </div>
            <div className="visual grid g4">
              <div className="card"><span className="fig">structured state</span><h4 style={{ marginTop: 10 }}>Learning engine</h4><p>Concepts, prerequisites, mastery estimates, review timing and assessment evidence. Chooses what to practice, revisit or assess next.</p></div>
              <div className="card"><span className="fig">narrative memory</span><h4 style={{ marginTop: 10 }}>Session memory</h4><p>Session summaries, goals, recurring misconceptions and useful learner context, encrypted and versioned. The AI picks up the thread.</p></div>
              <div className="card"><span className="fig">assessment</span><h4 style={{ marginTop: 10 }}>Frozen evidence</h4><p>Tasks and rubrics are frozen before the response. Estimated, retained, demonstrated and transferred skills are kept apart.</p></div>
              <div className="card"><span className="fig">audit</span><h4 style={{ marginTop: 10 }}>Decision trail</h4><p>Decision snapshots and replay explain why an activity was recommended or a concept held back.</p></div>
            </div>
            <h3 className="sub">Concept states</h3>
            <div className="grid g4">
              <div className="card"><Pill kind="done">Mastered</Pill><p style={{ marginTop: 12 }}>Above the routing threshold, with assessment evidence.</p></div>
              <div className="card"><Pill kind="prog">In progress</Pill><p style={{ marginTop: 12 }}>Unlocked, BKT estimate moving.</p></div>
              <div className="card"><Pill kind="review">Review due</Pill><p style={{ marginTop: 12 }}>FSRS predicts retention below target.</p></div>
              <div className="card"><Pill kind="lock">Locked</Pill><p style={{ marginTop: 12 }}>Prerequisites not met yet. Never red.</p></div>
            </div>
          </div>
        </section>

        <section className="sec" id="use-cases">
          <div className="wrap">
            <SectionTop n="2.0" title="Use cases" />
            <div className="sec-head">
              <h2>Six ways to use your tutor.</h2>
              <div className="txt"><p>No catalogue is required. Tell your AI what you want to learn; the engine handles sequencing, pacing and reviews, and says when you have actually got it. Institutions can add a shared catalogue on top.</p></div>
            </div>
            <div className="visual grid g3">
              <div className="card"><p className="q">“I want to learn Spanish before my trip in six weeks.”</p><h4>Pick up something new</h4><p>State a goal and a deadline. The engine maps the path and only opens new ground when the basics are solid.</p><div className="tags">languages · coding · a new craft</div></div>
              <div className="card"><p className="q">“I studied statistics eight years ago.”</p><h4>Bring rusty knowledge back</h4><p>A short diagnostic shows what is still solid, what faded and where the small wrong ideas hide. You revisit only what needs it.</p><div className="tags">diagnostic · targeted recall</div></div>
              <div className="card"><p className="q">“AWS Solutions Architect in three weeks.”</p><h4>Prepare an exam or interview</h4><p>Drills focus on weak spots; reviews are scheduled so retention peaks close to the date.</p><div className="tags">certification · coding interview</div></div>
              <div className="card"><p className="q">“Just one thing today.”</p><h4>Five focused minutes a day</h4><p>Open the chat. The engine knows which concept is due, serves one calibrated exercise and records the trace for next time.</p><div className="tags">microlearning · daily habit</div></div>
              <div className="card"><p className="q">“Twelve weeks of data engineering, starting Monday.”</p><h4>Run your own bootcamp</h4><p>Set the curriculum, depth and pace. Modules are sequenced on prerequisites, with mastery checks between phases.</p><div className="tags">self-paced · e-learning</div></div>
              <div className="card"><p className="q">“Publish the onboarding programme to all members.”</p><h4>Teach a community</h4><p>Owners publish programmes to their institution or invite named members. Contributions follow owner review; learners keep their version.</p><div className="tags">institution · catalogue · review</div></div>
            </div>
          </div>
        </section>

        <section className="sec" id="client">
          <div className="wrap">
            <SectionTop n="3.0" title="Tutor ACP" />
            <div className="sec-head">
              <h2>One desktop client to learn and manage.</h2>
              <div className="txt">
                <p>Tutor ACP connects Codex or Claude to your institution’s learning service over the Agent Client Protocol. Members find shared programmes in one catalogue, learn at their own pace and come back to their conversations and progress.</p>
                <p><b>Conversation</b> and <b>Formations</b> bring together tutoring, the programme library, manual or AI-assisted editing and progress. Account and Settings cover agents, institution, members and reading preferences. French and English, light, dark and system themes.</p>
                <div className="row"><a className="btn btn-light" href={CLIENT_DOWNLOAD}>Download for Windows x64</a><a className="btn btn-dark" href={`${REPO}/blob/main/client/README.md`}>Client guide</a></div>
              </div>
            </div>
            <div className="visual grid g3">
              <div className="card"><h4>Bundled agents</h4><p>Codex and Claude adapters and their Node.js runtime are included. Connect your provider account; it stays personal.</p></div>
              <div className="card"><h4>Public-key identities</h4><p>Your identity and recovery key are generated locally and backed up encrypted. The server stores public keys only; the agent never sees a private key.</p></div>
              <div className="card"><h4>Explicit permissions</h4><p>MCP tool permissions can be remembered per account, server and agent, and changed in Settings. Interrupted answers stay marked as such.</p></div>
            </div>
          </div>
        </section>

        <section className="sec" id="architecture">
          <div className="wrap">
            <SectionTop n="4.0" title="Architecture and algorithms" />
            <div className="sec-head">
              <h2>The AI explains. Tutor MCP remembers and decides.</h2>
              <div className="txt"><p>The runtime owns the durable learner state and the pedagogical decisions. The model stays free to explain, reframe, question and summarize. Decisions are inspectable; their quality depends on the evidence the AI records.</p></div>
            </div>
            <div className="visual grid g2">
              <div className="card">
                <span className="fig">deterministic engine</span><h4 style={{ marginTop: 10 }}>Tutor MCP</h4>
                <p>Phase control, gates, concept and action selection, evidence rules, session history and encrypted narrative memory. Predictable and auditable.</p>
                <div className="row" style={{ marginTop: 14, gap: 6 }}>{['BKT', 'FSRS-5', 'KST', 'Evidence gates', 'Transfer probes', 'Calibration', 'Decision replay'].map((t) => <span className="pill" key={t}>{t}</span>)}</div>
              </div>
              <div className="card">
                <span className="fig">generative coach</span><h4 style={{ marginTop: 10 }}>Your AI</h4>
                <p>Generates lessons and exercises on demand for the selected concept and difficulty target, writes feedback and session summaries, and turns raw state into learner-facing explanations without owning the schedule.</p>
                <div className="row" style={{ marginTop: 14, gap: 6 }}>{['Content generation', 'Natural language', 'Coaching', 'Memory consolidation'].map((t) => <span className="pill" key={t}>{t}</span>)}</div>
              </div>
            </div>
            <div className="scroll-x" style={{ marginTop: 32 }}>
              <table className="tt">
                <thead><tr><th>Model</th><th>Question it answers</th><th>Current behaviour</th></tr></thead>
                <tbody>
                  <tr><td>BKT</td><td className="m">Bayesian Knowledge Tracing</td><td>How does this response change the estimate of knowledge? A posterior from success, failure, slip and guess. Diagnostic, mastery and transfer probes update the observation only.</td></tr>
                  <tr><td>Individualized BKT</td><td className="m">bounded adjustments</td><td>What can recent observations add? Bounded parameter adjustments from a fixed reference, for collective calibration within an institution. Heuristics pending empirical calibration.</td></tr>
                  <tr><td>Information gain</td><td className="m">binary entropy</td><td>Which diagnostic concept would reduce uncertainty most?</td></tr>
                  <tr><td>FSRS-5</td><td className="m">spaced repetition</td><td>When should a concept be reviewed? Stability, difficulty and retrievability, scheduled in whole days.</td></tr>
                  <tr><td>KST</td><td className="m">prerequisite graph</td><td>What is accessible now? Validates the graph and unlocks concepts whose prerequisites are satisfied. No castle on sand.</td></tr>
                  <tr><td>Evidence gates</td><td className="m">frozen rubrics</td><td>Is this mastery or an estimate? Estimated, retained, demonstrated and transferred skills stay distinct. A high estimate alone certifies nothing.</td></tr>
                  <tr><td>Transfer</td><td className="m">near · far · debugging · teaching · creative</td><td>Does the knowledge move beyond the training pattern? Transfer claims depend on eligible demonstration evidence.</td></tr>
                  <tr><td>Calibration</td><td className="m">self-prediction</td><td>How well do you predict your own results? Absolute errors with visible sample coverage; missing observations are not good calibration.</td></tr>
                </tbody>
              </table>
            </div>
            <p className="cap"><b>Limits.</b> PFA has been removed from the runtime and the legacy IRT helpers are not used to set difficulty. Software tests establish implementation behaviour, not measured learning effectiveness. See the <a href={`${REPO}/blob/main/docs/algorithms.md`}>algorithm guide</a>.</p>
          </div>
        </section>

        <section className="sec" id="profiles">
          <div className="wrap">
            <SectionTop n="5.0" title="Profiles" />
            <div className="sec-head">
              <h2>Your learning engine, where you want it.</h2>
              <div className="txt"><p>Same binaries, three profiles. Every profile uses the same learning engine and the same MCP tools. Connect the clients you use to the same installation to keep one learning history.</p></div>
            </div>
            <div className="visual grid g3">
              <div className="card"><span className="fig">tutor-mcp --local</span><h4 style={{ marginTop: 10 }}>Local</h4><p>Learning on your own computer. Your MCP client starts the binary over stdio; SQLite stores your history in <code>~/.tutor-mcp/local</code>. No account or server setup.</p></div>
              <div className="card"><span className="fig">tutor-mcp --profile hobby</span><h4 style={{ marginTop: 10 }}>Hobby</h4><p>A personal VPS or a small group using MCP clients. SQLite and an HTTPS proxy. Invite users over SSH; they sign in with a username and password.</p></div>
              <div className="card"><span className="fig">tutor-mcp --profile institution</span><h4 style={{ marginTop: 10 }}>Institution</h4><p>Learning communities, teams and training organizations. A shared catalogue, member access, contribution review. Public-key identities, PostgreSQL and separate API, worker and migrator roles.</p></div>
            </div>
            <div className="term" style={{ marginTop: 16 }}>
              <div className="tb"><span className="on">Linux · macOS</span><span>Windows</span><span>From source</span></div>
              <pre><span className="p">$</span> curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/main/scripts/install.sh -o install-tutor.sh{'\n'}<span className="p">$</span> TUTOR_MCP_VERSION=v0.7.0 TUTOR_MCP_INSTALL_DIR=&quot;$HOME/.local/bin&quot; sh install-tutor.sh{'\n'}<span className="p">$</span> tutor-mcp --local{'\n'}<span className="o">✓</span> learning engine ready · ~/.tutor-mcp/local<span className="cursor" /></pre>
            </div>
            <p className="cap">Inspect the installer before running it. It verifies the archive against <code>SHA256SUMS</code> and keeps licence notices beside the executable. Windows uses <code>install.ps1</code>. <Link href="/docs/#install">Installation details →</Link></p>
          </div>
        </section>

        <section className="sec" id="open-source">
          <div className="wrap">
            <SectionTop n="6.0" title="Open source" />
            <div className="sec-head">
              <h2>Your tutor. Your data. Your server.</h2>
              <div className="txt"><p>No telemetry, no lock-in, no usage caps. Self-host and own the learner model, the memory, the sessions and the backups. The source, the docs and the deployment templates are in one repository.</p><div className="row"><a className="btn btn-light" href={REPO}>Clone the repository</a><Link className="btn btn-dark" href="/docs/">Read the docs</Link></div></div>
            </div>
            <div className="visual log">
              <div><small>license</small><b>MIT</b><p>Use, modify, ship and sell, with the notice preserved.</p></div>
              <div><small>platforms</small><b>Linux · macOS · Windows</b><p>amd64 and arm64 binaries with checksums on every release.</p></div>
              <div><small>storage</small><b>SQLite or PostgreSQL</b><p>Pure-Go SQLite for local and hobby; PostgreSQL with row-level security for institutions.</p></div>
              <div className="cur"><small>{'v0.7.0 · current'}</small><b>Tutor ACP 0.0.1</b><p>Institutional programmes, invitations, contribution review and the Windows client.</p></div>
            </div>
          </div>
        </section>

        <section className="sec" id="contribute">
          <div className="wrap">
            <SectionTop n="7.0" title="Contribute" />
            <div className="sec-head">
              <h2>Help shape the next learning primitive.</h2>
              <div className="txt">
                <p>Tutor MCP is alpha. The core works; the best version will come from learners, teachers, builders and MCP users pushing it against real workflows. Bug reports, docs pull requests, architecture critiques and product feedback all count.</p>
                <div className="row"><a className="btn btn-light" href={REPO}>Contribute on GitHub</a><a className="btn btn-dark" href={`${REPO}/issues`}>Open issues</a><a className="btn btn-ghost" href="https://x.com/TutorMCP">Follow on X</a></div>
              </div>
            </div>
            <div className="visual grid g4">
              <div className="card"><span className="num">01</span><h4 style={{ marginTop: 8 }}>Test it in the wild</h4><p>Run it with real learning goals and report what is confusing, brittle or surprisingly useful.</p></div>
              <div className="card"><span className="num">02</span><h4 style={{ marginTop: 8 }}>Improve the engine</h4><p>Scheduling, mastery checks, misconceptions, transfer probes and the signals that drive decisions.</p></div>
              <div className="card"><span className="num">03</span><h4 style={{ marginTop: 8 }}>Build integrations</h4><p>More MCP clients, provider setup, deployment paths and boringly reliable self-hosting.</p></div>
              <div className="card"><span className="num">04</span><h4 style={{ marginTop: 8 }}>Write better docs</h4><p>Turn sharp edges into guides, examples and recipes that get people to a first session.</p></div>
            </div>
          </div>
        </section>

        <section className="sec" id="faq">
          <div className="wrap">
            <SectionTop n="8.0" title="FAQ" />
            <div className="sec-head">
              <h2>Questions you might have.</h2>
              <div className="txt"><p>Short answers. The documentation and the repository have the details.</p></div>
            </div>
            <div className="visual faq">
              {faq.map(([q, a]) => <details key={q}><summary>{q}</summary><p>{a}</p></details>)}
            </div>
          </div>
        </section>

        <section className="final">
          <div className="wrap">
            <h2>Self-learning is a superpower.</h2>
            <p>Language models put knowledge within reach, but knowing is not learning. Tutor MCP brings decades of intelligent-tutoring research into any AI conversation. Self-hosted. Open source. Free.</p>
            <div className="row"><a className="btn btn-light btn-lg" href={CLIENT_DOWNLOAD}>Download Tutor ACP</a><Link className="btn btn-dark btn-lg" href="/docs/">Get started</Link><a className="btn btn-ghost btn-lg" href={REPO}>GitHub</a></div>
            <p className="cap" style={{ marginTop: 28 }}><Wordmark size={14} still /> · built by <a href="https://aguiovanna.fr">Arnaud Guiovanna</a> · MIT licensed</p>
          </div>
        </section>
      </main>
      <Footer />
    </>
  );
}
