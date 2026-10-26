import type { Metadata } from 'next';
import { Fragment } from 'react';
import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';
import { REPO } from '@/components/ui';

export const metadata: Metadata = {
  title: 'User guide — learn with Tutor MCP',
  description: 'How to start a goal, steer the tutor, ask for reviews, change the teaching style, close a session and understand the progression signals behind each decision.',
  alternates: { canonical: '/guide/' },
};

const toc: [string, [string, string][]][] = [
  ['i. Start', [['#mental-model', 'Engine vs AI'], ['#first-session', 'First session'], ['#client', 'In Tutor ACP']]],
  ['ii. Your moves', [['#steer', 'Ask for what you need'], ['#prompts', 'Prompt examples'], ['#close', 'Close a session']]],
  ['iii. Under the hood', [['#learning-loop', 'Conversation loop'], ['#progression', 'Progression signals'], ['#memory', 'Learner memory'], ['#privacy', 'Privacy & backups']]],
];

function Prompt({ children }: { children: string }) { return <pre><code>{children}</code></pre>; }

export default function Guide() {
  return (
    <>
      <Header current="guide" />
      <main id="main">
        <div className="wrap">
          <header className="doc-head">
            <p className="eyebrow">Guide · Learning with Tutor MCP</p>
            <h1>Use the tutor as a learning engine.</h1>
            <p className="lead">Tutor MCP is not another chat prompt. It is the durable layer between you and an AI: explicit signals for what should happen next, plus narrative memory for what you actually went through.</p>
            <div className="facts"><Link className="pill" href="/docs/">Install first →</Link><a className="pill" href={`${REPO}/blob/main/docs/mcp-tools.md`}>Tool catalogue</a></div>
          </header>

          <div className="doc-layout">
            <nav className="toc" aria-label="On this page">
              {toc.map(([h, items]) => (
                <Fragment key={h}>
                  <div className="h">{h}</div>
                  {items.map(([href, label]) => <a key={href} href={href}>{label}</a>)}
                </Fragment>
              ))}
            </nav>

            <div className="prose">
              <section id="mental-model">
                <span className="eyebrow">Model</span>
                <h2>Engine vs AI</h2>
                <p>The AI is the interface: it explains, asks questions, adapts tone, writes examples and summarizes the session. Tutor MCP is the engine: it stores state, chooses the next activity, validates evidence, detects gaps and decides when to review or move on.</p>
                <div className="note">The useful split: <b>Tutor MCP decides what and why. The AI decides how to say it.</b></div>
              </section>

              <section id="first-session">
                <span className="eyebrow">Start</span>
                <h2>First session</h2>
                <p>Start with a goal concrete enough to decompose into concepts. The assistant creates a domain if none exists, diagnoses your starting point and proposes the first activity.</p>
                <Prompt>{`Use Tutor MCP.
I want to learn Go concurrency in three weeks.
Create a learning domain, start with a short diagnostic, and keep session memory enabled.`}</Prompt>
                <p>A good first run calls <code>init_domain</code>, then <code>get_next_activity</code>. If the assistant skips the engine and starts tutoring directly, ask it to use Tutor MCP explicitly.</p>
              </section>

              <section id="client">
                <span className="eyebrow">Start</span>
                <h2>In Tutor ACP</h2>
                <p>Open <b>Formations</b> to choose a programme from your institution’s catalogue, or write what you want to learn in <b>Conversation</b> when your institution allows personal programmes. The programme is saved from the first exchange and can be resumed later.</p>
                <ul>
                  <li><b>Conversation</b> shows each tool call as it happens and asks for your decision when a tool needs permission. Interrupted answers stay marked as such.</li>
                  <li><b>Formations</b> lists your programmes and the catalogue, with concepts, prerequisites and progress. Owners and authorised trainers edit programmes here, manually or with the agent.</li>
                  <li><b>Settings</b> keep your agent, institution, members, language, theme and reading preferences per account.</li>
                </ul>
                <p>If setup is incomplete, your message is kept as “À envoyer” (to send) until both connections are ready.</p>
              </section>

              <section id="steer">
                <span className="eyebrow">Your moves</span>
                <h2>Ask for what you need</h2>
                <p>You do not have to accept the default path silently. Ask for a review, a different pace, a harder challenge, a safer exercise, another style or a visible explanation of the plan. The assistant should use <code>learning_negotiation</code>, <code>transfer_challenge</code>, <code>feynman_challenge</code> or <code>get_olm_snapshot</code> rather than inventing a plan from scratch.</p>
                <Prompt>{`Use Tutor MCP.
Show me why you picked this activity in plain language.
Then offer one easier option, one harder option, and one review-only option.`}</Prompt>
              </section>

              <section id="prompts">
                <span className="eyebrow">Prompts</span>
                <h2>Prompt examples</h2>
                <p>These name the engine explicitly so the assistant calls the tools instead of improvising a normal tutoring chat.</p>
                <h3>Start a new goal</h3>
                <Prompt>{`Use Tutor MCP.
I want to learn SQL window functions for analytics interviews in two weeks.
Create the domain, start with a diagnostic, and keep the first exercise short.`}</Prompt>
                <h3>Resume where you stopped</h3>
                <Prompt>{`Use Tutor MCP.
Resume my Go concurrency track from the last session.
Read my learner context and memory before choosing the next activity.`}</Prompt>
                <h3>Ask for review instead of new material</h3>
                <Prompt>{`Use Tutor MCP with review intent.
Pick the concept most at risk of forgetting and give me one calibrated exercise.
Record the result after I answer.`}</Prompt>
                <h3>Change the teaching style</h3>
                <Prompt>{`Use Tutor MCP.
Keep the same activity, but teach it Socratically.
Ask one question at a time and do not reveal the answer until I try.`}</Prompt>
                <h3>Make the plan transparent</h3>
                <Prompt>{`Use Tutor MCP.
Show me why this activity was selected in learner-friendly terms.
Then offer one safer option and one harder option.`}</Prompt>
                <h3>Correct memory</h3>
                <Prompt>{`Use Tutor MCP.
You remembered that I prefer visual explanations, but that is only true for math.
Update learner memory so programming sessions use concrete code examples first.`}</Prompt>
              </section>

              <section id="close">
                <span className="eyebrow">End</span>
                <h2>Close a session</h2>
                <p>A clean close preserves continuity. The engine returns a recap and asks the AI to write the session summary through <code>update_learner_memory</code>. It can also record an if-then commitment for the next session.</p>
                <Prompt>{`Close the Tutor MCP session.
Summarize what changed, ask me when I will practice next, and save the session memory.`}</Prompt>
              </section>

              <section id="learning-loop">
                <span className="eyebrow">Under the hood</span>
                <h2>Conversation loop</h2>
                <table className="tt">
                  <tbody>
                    <tr><td className="m">get_learner_context</td><td><b>Open with context.</b> Active domains, recent history, concept states and active misconceptions, before tutoring.</td></tr>
                    <tr><td className="m">get_next_activity</td><td><b>Let the engine choose.</b> Phase, concept, activity, difficulty target, motivation brief and relevant memory, with a recorded decision contract.</td></tr>
                    <tr><td className="m">record_interaction</td><td><b>Record the outcome.</b> Success, error type, hints, initiative and interpretation notes. Evidence-bearing answers go through a frozen assessment attempt.</td></tr>
                    <tr><td className="m">record_session_close</td><td><b>Close cleanly.</b> Recap signals and a memory-writing request so the AI stores a factual session summary.</td></tr>
                  </tbody>
                </table>
                <div className="note">The loop only works if outcomes are recorded. If the assistant gives exercises but never calls <code>record_interaction</code>, the engine cannot update estimates, retention or misconceptions.</div>
              </section>

              <section id="progression">
                <span className="eyebrow">Signals</span>
                <h2>Progression signals</h2>
                <p>Explicit models keep the schedule rigorous; the memory layer gives the AI enough context to avoid generic teaching. Each signal is shown with its threshold and its source. An estimate is not mastery.</p>
                <table className="tt">
                  <thead><tr><th>Signal</th><th>Source</th><th>Learner-facing effect</th></tr></thead>
                  <tbody>
                    <tr><td>Mastery</td><td className="m">BKT, evidence gates</td><td>Whether a concept is ready, shaky or needs stronger proof. Estimated, retained and demonstrated are kept apart.</td></tr>
                    <tr><td>Retention</td><td className="m">FSRS-5</td><td>When to review before forgetting becomes expensive.</td></tr>
                    <tr><td>Structure</td><td className="m">KST, prerequisites</td><td>Which concepts can unlock without skipping foundations.</td></tr>
                    <tr><td>Transfer</td><td className="m">near · far · debugging · teaching · creative</td><td>Whether knowledge moves beyond the training pattern.</td></tr>
                    <tr><td>Metacognition</td><td className="m">affect, calibration</td><td>How much support, prediction or reflection the tutor should propose. Descriptive, not a validated autonomy score.</td></tr>
                    <tr><td>Narrative</td><td className="m">sessions, concept notes, archives</td><td>What happened, what clicked, what remains uncertain and what to verify.</td></tr>
                  </tbody>
                </table>
              </section>

              <section id="memory">
                <span className="eyebrow">Memory</span>
                <h2>Learner memory</h2>
                <p>Narrative memory is designed for factual summaries, not diary prose. Sessions capture what happened; concept notes track durable observations; pending memory holds unconfirmed hypotheses; archives consolidate longer periods. In the local, hobby and institution profiles it is stored encrypted and versioned in the database.</p>
                <Prompt>{`get_memory_state
read_raw_session(timestamp: "2026-10-14T16:30:00Z")
update_learner_memory(scope: "concept", concept_slug: "goroutines",
                      operation: "replace_section", section_key: "Current state", content: "...")`}</Prompt>
                <p>Ask the assistant to mark uncertain observations as hypotheses. Tutor MCP exposes pending memory so the AI can use it without treating it as established truth.</p>
              </section>

              <section id="privacy">
                <span className="eyebrow">Operate</span>
                <h2>Privacy and backups</h2>
                <p>Your AI provider or local client keeps its own chat transcript. Tutor MCP keeps the learning state you preserve through MCP calls, in the installation you chose. For local and hobby profiles, back up the whole profile directory, including <code>runtime.db</code> and <code>keys.json</code>, with every process stopped. Institutions keep PostgreSQL procedures and separate server-key backups; members keep their own encrypted identity backups.</p>
                <p>For sensitive use, self-host behind HTTPS, restrict <code>TRUSTED_PROXY_CIDRS</code> and review what the AI writes to memory from time to time: it is meant to be readable. See <Link href="/docs/#backup">backup and restore</Link>.</p>
              </section>
            </div>
          </div>
        </div>
      </main>
      <Footer />
    </>
  );
}
