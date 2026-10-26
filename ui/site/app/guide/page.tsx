import type { Metadata } from 'next';
import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';
import { CodeBlock } from '@/components/code-block';

export const metadata: Metadata = {
  title: 'Beginner’s guide — your first useful Tutor session',
  description: 'Choose a learning goal, start a course, practise at your own pace, understand your progress and return for your next session. A practical beginner’s guide to Tutor ACP and Tutor MCP.',
  alternates: { canonical: '/guide/' },
};

export default function BeginnerGuide() {
  return <>
    <Header current="guide" />
    <main id="main">
      <div className="wrap">
        <header className="doc-head">
          <p className="eyebrow">User guide · Your first useful session</p>
          <h1>Bring a goal. Take a first step.</h1>
          <p className="lead">You do not need to know a list of tools or write a perfect prompt. Choose something you want to learn, try an activity and leave with a next step you can return to.</p>
          <div className="facts"><Link className="pill" href="/docs/install-client/">Install Tutor ACP →</Link><Link className="pill" href="/docs/local/">Use a local MCP client</Link><span className="pill">About 10 minutes to try a session</span></div>
        </header>

        <div className="doc-layout">
          <nav className="toc" aria-label="Beginner’s guide sections">
            <div className="h">Start</div>
            <a href="#client">Get connected</a>
            <a href="#first-session">Choose your starting point</a>
            <div className="h">Learn</div>
            <a href="#steer">Practise and adjust</a>
            <a href="#progression">Read your progress</a>
            <a href="#close">Finish and return</a>
            <div className="h">Continue</div>
            <a href="#share">Share a programme</a>
            <a href="#privacy">Look after your account</a>
          </nav>

          <div className="prose">
            <section id="client">
              <span className="eyebrow">01 · Connect</span>
              <h2>Make sure you are ready.</h2>
              <p>In Tutor ACP, connect your <b>agent</b> and your <b>Tutor service</b>. Choose Codex or Claude in the agent settings. For an institution, your organisation supplies the Tutor address and authorises your public identity.</p>
              <p>Starting on your own computer? You can run Tutor MCP locally and connect it to a compatible AI application. Follow <Link href="/docs/local/">Local installation</Link> before continuing.</p>
              <div className="note" id="mental-model"><b>Two parts with different jobs.</b> Your AI explains and responds. Tutor keeps the programme, learning evidence and review schedule. You can ask questions naturally; Tutor gives the conversation a learning history to build on.</div>
              <p>If setup is incomplete, your message is saved as <b>Ready to send</b>. Finish connecting to continue. The <Link href="/docs/install-client/">client guide</Link> explains identity backups and first connection.</p>
            </section>

            <section id="first-session">
              <span className="eyebrow">02 · Choose</span>
              <h2>Start with something that matters to you.</h2>
              <p>Open <b>Courses</b> and choose a programme from your institution’s catalogue, or start a personal programme in <b>Conversation</b> if your institution allows it. A shared course may require an invitation or approval before you can join.</p>
              <p>For a personal programme, tell the tutor what you want to do, what you already know and how much time you have.</p>
              <CodeBlock label="Try a first message" code={'I want to learn enough Spanish to talk with people when I travel.\nI am a beginner and can practise for 15 minutes a day.\nHelp me choose a starting point, then guide me through one short activity.'} />
              <p>In another MCP application, begin with “Use Tutor MCP” so the assistant knows to use your connected learning service. You can replace Spanish with a skill, project or subject of your own.</p>
              <p>Tutor searches for an existing personal programme before creating a new one. A new personal programme is private and saved from the first exchange.</p>
            </section>

            <section id="steer">
              <span className="eyebrow">03 · Practise</span>
              <h2>Try it yourself, then ask for help.</h2>
              <p>Answer the questions, explain your reasoning and tell the tutor where you get stuck. Try an exercise before asking to see the answer. If you used a hint, make that clear.</p>
              <p id="prompts">You can ask for a different pace, explanation or example:</p>
              <CodeBlock label="Make the session work for you" code={'Give me a smaller example.\nExplain it in another way.\nLet me try one exercise without hints.\nConnect this idea to a situation from my work.\nI have five minutes left. What is a useful stopping point?'} />
              <p id="learning-loop">Each activity can add evidence to the course, helping Tutor choose what to practise or review next. The AI can make mistakes: question an explanation that seems wrong and ask it to check the reasoning.</p>
              <p>When a tool asks for permission, review the request. Your choice can be saved for the relevant account, service and agent, and changed in <b>Settings → Agents</b>.</p>
            </section>

            <section id="progression">
              <span className="eyebrow">04 · Notice progress</span>
              <h2>Look for evidence, not just a high number.</h2>
              <p>Open the course’s <b>Progress</b> view to see what has been recorded and what is due for review. A mastery estimate helps route your learning; it does not by itself prove that you have retained or demonstrated a skill.</p>
              <p>Delayed recall can provide evidence of retention. Eligible assessments can support demonstration and transfer to other situations. Badges reflect their recorded evidence and are not an external certification.</p>
              <p>A useful question is: “What can I now do, and what should I try next?” Read <Link href="/docs/first-session/#read-your-progress">the progress guide</Link> for the differences between estimated, retained, demonstrated and transferred learning.</p>
            </section>

            <section id="close">
              <span className="eyebrow">05 · Return</span>
              <h2>Leave a clear next step.</h2>
              <CodeBlock label="Close the session" code={'Summarise what I practised and what is still difficult.\nSave the session in Tutor.\nSuggest one next step and tell me what I should review.'} />
              <p id="memory">Next time, reopen the same account and Tutor service, then return to the conversation or select your course. Ask: <b>“Let’s continue my Spanish course.”</b></p>
              <p>Each conversation keeps its selected course. Switch courses explicitly when you want a different subject, rather than assuming that another conversation changed the current one.</p>
              <Link className="more" href="/docs/first-session/">Read the complete first-session guide</Link>
            </section>

            <section id="share">
              <span className="eyebrow">06 · Learn together</span>
              <h2>Turn a useful programme into a shared starting point.</h2>
              <p>If you want others in your institution to follow your programme, open its <b>Sharing and access</b> tab. Sharing a personal course copies its design while keeping private conversations and progress separate.</p>
              <p>Review the objectives, choose who can join and submit the programme for the owner’s approval when required. Each member then follows the shared programme with their own learning record.</p>
              <div className="row"><Link className="btn btn-dark" href="/docs/sharing-courses/">Create and share a course</Link><Link className="btn btn-ghost" href="/docs/institutions/">Understand institution roles →</Link></div>
            </section>

            <section id="privacy">
              <span className="eyebrow">07 · Keep it yours</span>
              <h2>Look after your account and your learning.</h2>
              <p>Keep your encrypted identity backup and its phrase safe and separate. Your identity export, conversation files on the device and learning data on the Tutor service have different backup procedures.</p>
              <p>You can adjust the application’s language, theme, text size, line spacing and reduced motion in <b>Settings → Reading and appearance</b>. Changing the interface language does not translate your existing conversations or course content.</p>
              <div className="row"><Link className="btn btn-dark" href="/docs/backup-and-updates/">Backups and updates</Link><Link className="btn btn-ghost" href="/docs/troubleshooting/">Get unstuck →</Link></div>
            </section>
          </div>
        </div>
      </div>
    </main>
    <Footer />
  </>;
}
