import type { Metadata } from 'next';
import { Fragment } from 'react';
import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';
import { CLIENT_DOWNLOAD, Kbd, RELEASE, REPO, VERSION } from '@/components/ui';

export const metadata: Metadata = {
  title: 'Install Tutor MCP v0.7.0 and connect your AI',
  description: 'Install the Tutor MCP binary or the Tutor ACP Windows client, choose a local, hobby or institution profile, connect Claude, ChatGPT, Hermes, Gemini CLI or Le Chat, and verify your first session.',
  alternates: { canonical: '/docs/' },
};

const toc: [string, [string, string][]][] = [
  ['i. Get going', [['#quickstart', 'Quick start'], ['#requirements', 'Requirements'], ['#architecture', 'How it works']]],
  ['ii. Install', [['#install', 'Install the binary'], ['#client', 'Tutor ACP for Windows'], ['#local', 'Local profile'], ['#hobby', 'Hobby VPS'], ['#institution', 'Institution']]],
  ['iii. Connect', [['#providers', 'Connect a client'], ['#claude-code', 'Claude Code'], ['#remote', 'Remote connectors']]],
  ['iv. Verify & operate', [['#test', 'Test your setup'], ['#memory', 'Learner memory'], ['#backup', 'Backup & restore'], ['#troubleshooting', 'Troubleshooting']]],
];

function Doc(href: string) { return `${REPO}/blob/main/${href}`; }

export default function Docs() {
  return (
    <>
      <Header current="docs" />
      <main id="main">
        <div className="wrap">
          <header className="doc-head">
            <p className="eyebrow">Documentation · Install and connect</p>
            <h1>From binary to first session.</h1>
            <p className="lead">Everything you need to run your own tutor on your machine, a small VPS or an institution’s infrastructure, plug it into the AI assistant you already use, and keep progress, sessions and memory under your control.</p>
            <div className="facts">
              <a className="pill" href={RELEASE}>{VERSION}</a><span className="pill">MIT licensed</span><span className="pill">Go 1.26.10+</span><span className="pill">SQLite, no CGO</span><span className="pill">PostgreSQL for institutions</span>
              <Link className="pill" href="/guide/">User guide →</Link>
            </div>
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
              <section id="quickstart">
                <span className="eyebrow">Get going</span>
                <h2>Quick start</h2>
                <p>Two paths. For a learning community, install the Windows client and connect it to your institution. For yourself, install the binary and let your MCP client start it.</p>
                <div className="steps">
                  <div><span className="num">i.</span><b>Get the binary</b><p>Installer for Linux, macOS and Windows, or <code>go build .</code></p></div>
                  <div><span className="num">ii.</span><b>Pick a profile</b><p><code>--local</code> on your machine, <code>hobby</code> on a VPS, <code>institution</code> on PostgreSQL.</p></div>
                  <div><span className="num">iii.</span><b>Connect your AI</b><p>Add <code>tutor-mcp --local</code> to your client, or your VPS <code>/mcp</code> URL with OAuth.</p></div>
                  <div><span className="num">iv.</span><b>Start learning</b><p>“Use Tutor MCP to help me learn Go.” Resume later from the same installation.</p></div>
                </div>
                <div className="note"><b>Learning community?</b> Install <a href={CLIENT_DOWNLOAD}>Tutor ACP 0.0.1 for Windows x64</a>. Codex and Claude adapters are included; your institution provides the Tutor service. See <a href="#client">Tutor ACP</a>.</div>
              </section>

              <section id="requirements">
                <span className="eyebrow">Get going</span>
                <h2>What you need</h2>
                <table className="tt">
                  <tbody>
                    <tr><td>Binaries</td><td>Linux, macOS and Windows, amd64 and arm64, with <code>SHA256SUMS</code> on the <a href={RELEASE}>release page</a>.</td></tr>
                    <tr><td>From source</td><td>Git and Go 1.26.10 or a newer patched release. One module, pure-Go SQLite, no CGO.</td></tr>
                    <tr><td>Local</td><td>Nothing else. No account, no HTTP server, no WSL, Docker or SMTP.</td></tr>
                    <tr><td>Hobby VPS</td><td>A DNS name, inbound TCP 80 and 443 for Caddy, SSH for administration. SQLite.</td></tr>
                    <tr><td>Institution</td><td>PostgreSQL with <code>sslmode=verify-full</code>, separate API, worker and migrator processes, operator-managed keys, an HTTPS proxy.</td></tr>
                    <tr><td>Tutor ACP</td><td>Windows x64. Bundled Codex and Claude adapters with their Node.js runtime; native or an explicitly chosen WSL2 distribution.</td></tr>
                  </tbody>
                </table>
                <p>Hosted assistants such as Claude on the web, ChatGPT and Le Chat need a public HTTPS endpoint with a trusted certificate. Local clients such as Claude Code, Claude Desktop, Hermes and Gemini CLI run the binary over stdio.</p>
              </section>

              <section id="architecture">
                <span className="eyebrow">Get going</span>
                <h2>How it works</h2>
                <p>Tutor MCP sits between the assistant and the learner state. The assistant stays the conversational interface; Tutor MCP is the memory and decision runtime.</p>
                <div className="steps" style={{ gridTemplateColumns: 'repeat(3, minmax(0,1fr))' }}>
                  <div><span className="num">1</span><b>You talk to your AI</b><p>Ask for a goal, a review, a harder challenge or a session close, in natural language.</p></div>
                  <div><span className="num">2</span><b>The AI calls MCP tools</b><p>It reads learner context, asks for the next activity with <code>get_next_activity</code>, records outcomes with <code>record_interaction</code>, closes the session.</p></div>
                  <div><span className="num">3</span><b>Tutor MCP updates state</b><p>BKT estimates, FSRS-5 review timing, prerequisite access, assessment evidence, narrative memory and decision snapshots.</p></div>
                </div>
                <table className="tt">
                  <tbody>
                    <tr><td>Learning engine</td><td>Versioned concept graph, goals, interactions, mastery estimates, review dates, assessment and transfer evidence. Chooses a phase, concept and activity with explicit rules.</td></tr>
                    <tr><td>Narrative memory</td><td>Stable learner notes, pending observations, concept notes, session summaries and consolidated archives. Encrypted and versioned in the database.</td></tr>
                  </tbody>
                </table>
                <p>The important split: the AI writes explanations and summaries, but Tutor MCP owns the state transitions and the next-activity decision. Narrative text cannot promote itself into assessment evidence. Full details in the <a href={Doc('docs/architecture.md')}>architecture</a> and <a href={Doc('docs/algorithms.md')}>algorithm</a> guides.</p>
              </section>

              <section id="install">
                <span className="eyebrow">Install</span>
                <h2>Install the binary</h2>
                <h3>Linux and macOS</h3>
                <p>Download the installer from the repository, inspect it, then run it. It selects your platform, verifies the archive against <code>SHA256SUMS</code> and keeps licence notices beside the executable.</p>
                <pre><code><span className="p">$</span> curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/main/scripts/install.sh -o install-tutor.sh{'\n'}<span className="p">$</span> TUTOR_MCP_VERSION={VERSION} TUTOR_MCP_INSTALL_DIR=&quot;$HOME/.local/bin&quot; sh install-tutor.sh</code></pre>
                <p>Add <code>$HOME/.local/bin</code> to your PATH and restart your MCP client. Without <code>TUTOR_MCP_INSTALL_DIR</code>, the installer uses <code>/usr/local/bin</code> and may ask for sudo.</p>
                <h3>Windows</h3>
                <p>Download <a href={Doc('scripts/install.ps1')}>install.ps1</a>, inspect it, then run it from PowerShell as your normal user. It verifies SHA-256, installs under <code>%LOCALAPPDATA%\Programs\tutor-mcp</code> and adds that directory to your PATH.</p>
                <pre><code><span className="p">PS&gt;</span> .\install.ps1 -Version {VERSION}</code></pre>
                <h3>From source</h3>
                <pre><code><span className="p">$</span> git clone --branch main https://github.com/ArnaudGuiovanna/tutor-mcp.git{'\n'}<span className="p">$</span> cd tutor-mcp{'\n'}<span className="p">$</span> go build .</code></pre>
                <p>This creates <code>tutor-mcp</code> (<code>tutor-mcp.exe</code> on Windows). Keep its full path for your client configuration. Pin a tagged release in production; the <a href={Doc('docs/installation.md')}>installation guide</a> covers notices, checksums and upgrades.</p>
              </section>

              <section id="client">
                <span className="eyebrow">Install</span>
                <h2>Tutor ACP for Windows</h2>
                <p><a href={CLIENT_DOWNLOAD}>Tutor ACP 0.0.1 for Windows x64</a> is the desktop client for learning communities. It connects Codex or Claude to your institution’s Tutor service over the Agent Client Protocol.</p>
                <ol>
                  <li>Install the client. Codex Desktop is not required: the bundled adapters and their Node.js runtime are included.</li>
                  <li>In Settings → Agents, select Codex or Claude and connect your provider account. Provider access and costs are separate from Tutor.</li>
                  <li>In Settings → Institution, enter your institution’s address and connect with an authorised Tutor identity. The client generates the identity and a distinct recovery key locally; export the encrypted backup and keep it offline.</li>
                  <li>Open <b>Formations</b> to choose a programme from the catalogue, or write what you want to learn in <b>Conversation</b> when your institution allows personal programmes.</li>
                </ol>
                <p>The client offers French and English, light, dark and system themes and reading settings saved per account. MCP tool permissions can be remembered across conversations for the selected account, server and agent. The server controls access and progress. See the <a href={Doc('client/README.md')}>desktop client guide</a> for identities, backups, native Windows and WSL2.</p>
              </section>

              <section id="local">
                <span className="eyebrow">Install</span>
                <h2>Local profile</h2>
                <p>Your MCP client starts <code>tutor-mcp --local</code> and talks to it over stdin and stdout. No HTTP server, JWT or SMTP. Your learning profile lives in <code>~/.tutor-mcp/local</code>; add <code>--data-dir</code> for another private directory. Several local clients can share the same profile and learner identity.</p>
                <h3>Claude Desktop and other <code>mcpServers</code> clients</h3>
                <p>Open Settings → Developer → Edit Config, merge this into the configuration and restart the client. Replace <code>command</code> with the full path if the binary is not on PATH; on Windows, use a path such as <code>C:/tools/tutor-mcp.exe</code>.</p>
                <pre><code>{`{
  "mcpServers": {
    "tutor": { "command": "tutor-mcp", "args": ["--local"] }
  }
}`}</code></pre>
                <h3>Hermes</h3>
                <pre><code>{`mcp_servers:
  tutor:
    command: tutor-mcp
    args: ["--local"]`}</code></pre>
                <h3>Gemini CLI, Pi and others</h3>
                <p>Gemini CLI uses the same <code>command</code>/<code>args</code> shape. Pi delegates MCP to extensions: configure yours to launch <code>tutor-mcp --local</code>. Any client that speaks stdio MCP works. See the <a href={Doc('docs/clients.md')}>client guide</a>.</p>
              </section>

              <section id="hobby">
                <span className="eyebrow">Install</span>
                <h2>Hobby VPS</h2>
                <span id="configure" /><span id="proxy" /><span id="systemd" />
                <p>One HTTP server for a personal VPS or a small group. SQLite, username and password accounts, invitations over SSH. Choose a DNS name, point it at the VPS and allow TCP 80 and 443 for Caddy. Only Caddy is public; Tutor listens on <code>127.0.0.1:3000</code>.</p>
                <h3>Native: binary, systemd and Caddy</h3>
                <pre><code>{`$ sudo useradd --system --home-dir /var/lib/tutor-mcp --shell /usr/sbin/nologin tutor
$ sudo install -d -m 0700 -o tutor -g tutor /var/lib/tutor-mcp
$ sudo -u tutor /usr/local/bin/tutor-mcp init --profile hobby \\
    --data-dir /var/lib/tutor-mcp/hobby --public-url https://tutor.example.org
$ sudo install -m 0644 deploy/tutor-hobby.service /etc/systemd/system/tutor-hobby.service
$ sudo systemctl daemon-reload && sudo systemctl enable --now tutor-hobby`}</code></pre>
                <p><code>init</code> creates the configuration, the encryption key and the signing key, then prints an account invitation valid for 24 hours. Keep it private and open it once HTTPS is ready. Install <a href={Doc('deploy/Caddyfile.native')}>Caddyfile.native</a> with your domain, then validate and reload:</p>
                <pre><code>{`$ sudo caddy validate --config /etc/caddy/Caddyfile
$ sudo systemctl reload caddy
$ curl --fail https://tutor.example.org/ready`}</code></pre>
                <p>Your MCP URL is <code>https://tutor.example.org/mcp</code>. Administration runs over SSH as the service user:</p>
                <pre><code>{`$ sudo -u tutor tutor-mcp users invite --data-dir /var/lib/tutor-mcp/hobby
$ sudo -u tutor tutor-mcp users list --data-dir /var/lib/tutor-mcp/hobby
$ sudo -u tutor tutor-mcp users reset alice --data-dir /var/lib/tutor-mcp/hobby
$ sudo -u tutor tutor-mcp users disable alice --data-dir /var/lib/tutor-mcp/hobby`}</code></pre>
                <h3>Docker Compose</h3>
                <pre><code>{`$ export TUTOR_DOMAIN=tutor.example.org TUTOR_VERSION=${VERSION}
$ docker compose -f deploy/compose.hobby.yml build
$ docker compose -f deploy/compose.hobby.yml run --rm --no-deps tutor \\
    init --profile hobby --data-dir /data/hobby --public-url "https://$TUTOR_DOMAIN"
$ docker compose -f deploy/compose.hobby.yml up -d
$ curl --fail "https://$TUTOR_DOMAIN/ready"`}</code></pre>
                <p>Keep the three named volumes across upgrades and never upgrade with <code>down --volumes</code>. Details, including trusted proxy ranges, in the <a href={Doc('docs/installation.md#hobby-vps-with-docker-compose')}>installation guide</a>.</p>
              </section>

              <section id="institution">
                <span className="eyebrow">Install</span>
                <h2>Institution</h2>
                <span id="postgres" />
                <p><code>--profile institution</code> serves a learning community: a shared catalogue, member access, contribution review and individual learning paths. Members need no email address or server password. The Windows client generates a public-key identity and a distinct recovery key; the server stores public keys only.</p>
                <ul>
                  <li><b>PostgreSQL</b> with <code>sslmode=verify-full</code>, forced row-level security and separate <code>tutor_api</code>, <code>tutor_worker</code> and <code>tutor_restore</code> roles. Reapply <code>deploy/postgres-roles.sql</code> after every schema upgrade.</li>
                  <li><b>Three processes</b>: run the migrator once, then enable the stateless API and the worker. systemd units and a Compose file are provided in <code>deploy/</code>.</li>
                  <li><b>Operator bootstrap</b>: <code>tutor-control-plane</code> creates the plan and the tenant, then binds the owner’s two public keys. Set <code>INSTITUTION_ID</code> on the API so the client discovers the institution at <code>/.well-known/tutor-institution</code>.</li>
                  <li><b>Members</b> are added from their own public keys with roles such as <code>learner</code>, <code>admin</code>, <code>pedagogy_manager</code>, <code>trainer</code> or <code>auditor</code>. An unknown key cannot self-register.</li>
                </ul>
                <p>Follow the <a href={Doc('docs/installation.md#institutional-deployment')}>institutional setup</a> and <a href={Doc('OPERATIONS.md')}>operations guide</a> for keys, backups, tenant restoration, SLOs and erasure.</p>
              </section>

              <section id="providers">
                <span className="eyebrow">Connect</span>
                <h2>Connect a client</h2>
                <p>Tutor MCP exposes the same learning tools over local stdio and remote Streamable HTTP with OAuth. Pick the connection your client supports.</p>
                <table className="tt">
                  <thead><tr><th>Client</th><th>Connection</th><th>Setup</th></tr></thead>
                  <tbody>
                    <tr><td>Tutor ACP</td><td>Institution service, Codex or Claude via ACP</td><td><a href="#client">Windows client</a></td></tr>
                    <tr><td>Claude Code</td><td>Local stdio or remote HTTPS</td><td><a href="#claude-code">Command below</a></td></tr>
                    <tr><td>Claude Desktop</td><td>Local stdio</td><td><a href="#local">JSON configuration</a></td></tr>
                    <tr><td>Claude on the web</td><td>Remote HTTPS with OAuth</td><td>Add your <code>/mcp</code> URL as a custom connector</td></tr>
                    <tr><td>ChatGPT</td><td>Remote HTTPS with OAuth</td><td>Developer mode → Connectors → Create, with your <code>/mcp</code> URL</td></tr>
                    <tr><td>Hermes</td><td>Local stdio or remote HTTPS with OAuth</td><td><a href="#local">YAML configuration</a></td></tr>
                    <tr><td>Pi</td><td>Local stdio through an MCP extension</td><td>Launch <code>tutor-mcp --local</code> from the extension</td></tr>
                    <tr><td>Gemini CLI</td><td>Local stdio or Streamable HTTP</td><td><code>command</code>/<code>args</code> or <code>httpUrl</code></td></tr>
                    <tr><td>Le Chat</td><td>Remote HTTPS connector</td><td>Register your <code>/mcp</code> URL as a custom MCP connector</td></tr>
                    <tr><td>Gemini Enterprise</td><td>Remote custom MCP data store</td><td>Administrator OAuth and client registration</td></tr>
                  </tbody>
                </table>
                <p>Account plans and administrator policies affect connector availability; the client’s own documentation is authoritative. The <a href={Doc('docs/clients.md')}>client guide</a> links each one.</p>
              </section>

              <section id="claude-code">
                <span className="eyebrow">Connect</span>
                <h2>Claude Code</h2>
                <p>Add a user-scoped stdio server:</p>
                <pre><code><span className="p">$</span> claude mcp add --transport stdio --scope user tutor -- tutor-mcp --local</code></pre>
                <p>Or put the <code>mcpServers</code> JSON above in the project’s <code>.mcp.json</code> and approve that server in Claude Code. For a VPS, use your <code>https://your.domain/mcp</code> URL instead.</p>
              </section>

              <section id="remote">
                <span className="eyebrow">Connect</span>
                <h2>Remote connectors</h2>
                <p>Install a hobby or institution profile and expose <code>https://your.domain/mcp</code> with a publicly trusted certificate. The client discovers the authorization server, registers or resolves its client metadata and opens the sign-in and consent flow. PKCE, resource binding and refresh-token rotation protect the session.</p>
                <div className="prov">
                  <div><div className="hd"><img src="/logos/claude.svg" alt="" />Claude on the web</div><ol><li>Settings → Connectors → add.</li><li>Name: Tutor MCP. Server URL: <code>https://your.domain/mcp</code>.</li><li>Add, then complete the OAuth sign-in.</li></ol></div>
                  <div><div className="hd"><img src="/logos/openai.svg" alt="" />ChatGPT</div><ol><li>Settings → Connectors → Advanced → Developer mode.</li><li>Create: name Tutor MCP, your <code>/mcp</code> URL.</li><li>Create and authenticate via OAuth.</li></ol></div>
                  <div><div className="hd"><img src="/logos/mistral.svg" alt="" />Le Chat</div><ol><li>Connectors → Add Connector → Custom MCP.</li><li>Name <code>tutor_mcp</code>, your <code>/mcp</code> URL.</li><li>Connect, complete OAuth.</li></ol></div>
                  <div><div className="hd"><img src="/logos/gemini.svg" alt="" />Gemini</div><p>Gemini CLI connects over stdio or <code>httpUrl</code>. Gemini Enterprise registers the service as a custom MCP data store through its administrator setup.</p></div>
                </div>
                <p>Hobby accounts use an operator invitation and a username and password; institutional accounts use the client’s public-key identity. Operators can restrict dynamic client registration; see <a href={Doc('docs/oauth-dcr-production.md')}>OAuth registration</a>.</p>
              </section>

              <section id="test">
                <span className="eyebrow">Verify</span>
                <h2>Test your setup</h2>
                <p>Send this in a fresh conversation. Tutor also exposes a <code>tutor_mcp</code> prompt for clients that support MCP prompts.</p>
                <pre><code>Use Tutor MCP to help me learn Go for backend development. Find out what I already know,{'\n'}create a learning plan, and guide me through a first 20-minute session. Save my progress when we finish.</code></pre>
                <ul className="check">
                  <li>The assistant calls <code>get_learner_context</code>, then <code>init_domain</code> or <code>get_next_activity</code>.</li>
                  <li>You see a reply that names a concept, a phase and a first activity.</li>
                  <li>After an answer, <code>record_interaction</code> is called. Without it, nothing updates.</li>
                </ul>
                <p>In Tutor ACP, the <b>Conversation</b> page shows each tool call as it happens, and the programme is saved from the first exchange. Next time: “Resume my Go learning with Tutor MCP.”</p>
              </section>

              <section id="memory">
                <span className="eyebrow">Operate</span>
                <h2>Learner memory</h2>
                <p>Tutor keeps two layers. The learning engine stores structured state: domains, interactions, BKT and FSRS values, calibration, affect, assessment evidence and scheduler data. Narrative memory keeps what numbers cannot express; the AI writes it through MCP and reads it back on the next activity.</p>
                <table className="tt">
                  <tbody>
                    <tr><td>Sessions</td><td>Summaries with affect, concepts touched, salient exchanges and next-step commitments.</td></tr>
                    <tr><td>Concept notes</td><td>Current narrative state per concept, updated when a durable observation appears.</td></tr>
                    <tr><td>Stable memory</td><td>Learner preferences and durable facts, promoted only when confirmed or explicitly stated.</td></tr>
                    <tr><td>Pending</td><td>Unconfirmed hypotheses, exposed as such so the AI does not treat them as established.</td></tr>
                    <tr><td>Archives</td><td>Monthly, quarterly and annual consolidation written by the connected AI.</td></tr>
                  </tbody>
                </table>
                <p>The main tools are <code>record_session_close</code>, <code>update_learner_memory</code>, <code>read_raw_session</code> and <code>get_memory_state</code>. In the three profiles, narrative objects are encrypted, versioned database records; the key material in <code>keys.json</code> is generated before the database and must be restored with it. Your AI provider still hosts its own chat transcript. See <a href={Doc('docs/narrative-memory-operations.md')}>memory operations</a>.</p>
              </section>

              <section id="backup">
                <span className="eyebrow">Operate</span>
                <h2>Backup and restore</h2>
                <p>For local and hobby profiles, stop every process using the profile, then copy the entire directory, including <code>runtime.db</code>, any WAL and SHM files and <code>keys.json</code>, to a private destination. Restore the database and the keys together, under the service’s operating-system owner, then restart with the same profile.</p>
                <pre><code>{`$ systemctl stop tutor-hobby
$ cp -a /var/lib/tutor-mcp/hobby /private/backups/tutor-hobby-$(date -u +%FT%TZ)
$ systemctl start tutor-hobby`}</code></pre>
                <p>Never copy only the main file of an open WAL database. Backups contain secrets and must stay private. Institutions keep their PostgreSQL procedures, separate server-key backups and the <a href={Doc('docs/tenant-restore-runbook.md')}>tenant restore runbook</a>; members keep their own encrypted identity backups. A backup you have never restored is a backup you do not have.</p>
              </section>

              <section id="troubleshooting">
                <span className="eyebrow">When things go sideways</span>
                <h2>Troubleshooting</h2>
                <h3>The assistant never calls the tutor</h3>
                <p>Name the runtime explicitly: “Use Tutor MCP.” Plain conversation does not update the learning record. If the client lists the server but no tools appear, check the executable path and restart the client.</p>
                <h3>Tutor ACP keeps the message as “À envoyer”</h3>
                <p>Setup is incomplete. Finish the agent connection in Settings → Agents and the institution connection in Settings → Institution; the message is sent once both are ready.</p>
                <h3>Remote OAuth fails</h3>
                <p>The certificate must be publicly trusted: <code>curl --fail https://your.domain/ready</code> should succeed without warnings. Check <code>TRUSTED_PROXY_CIDRS</code> and that the public Host header reaches Tutor. Account creation alone does not grant client access; the account must authorize the client.</p>
                <h3>A local installation refuses to start</h3>
                <p>Its <code>keys.json</code> is missing or invalid. Restore the original key with the database instead of generating a replacement.</p>
                <p>Need more? Open an <a href={`${REPO}/issues`}>issue</a>, start a <a href={`${REPO}/discussions`}>discussion</a>, or read <a href={Doc('OPERATIONS.md')}>OPERATIONS.md</a>. For vulnerabilities, use <a href={Doc('SECURITY.md')}>private reporting</a>, never a public issue.</p>
                <p className="cap"><Kbd>Ctrl</Kbd> <Kbd>K</Kbd> in Tutor ACP opens the command palette.</p>
              </section>
            </div>
          </div>
        </div>
      </main>
      <Footer />
    </>
  );
}
