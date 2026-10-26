import { CodeBlock } from '@/components/code-block';
import { CLIENT_DOWNLOAD, CLIENT_VERSION, REPO, VERSION } from '@/components/ui';
import type { Guide } from '@/lib/docs-types';

const repoFile = (path: string) => `${REPO}/blob/${VERSION}/${path}`;
const localConfiguration = JSON.stringify({
  mcpServers: {
    tutor: { command: '/absolute/path/to/tutor-mcp', args: ['--local'] },
  },
}, null, 2);

const unixInstall = `curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/${VERSION}/scripts/install.sh -o install-tutor.sh
TUTOR_MCP_VERSION=${VERSION} TUTOR_MCP_INSTALL_DIR="$HOME/.local/bin" sh install-tutor.sh
"$HOME/.local/bin/tutor-mcp" --version`;

const windowsInstall = String.raw`Invoke-WebRequest -Uri "https://raw.githubusercontent.com/ArnaudGuiovanna/tutor-mcp/${VERSION}/scripts/install.ps1" -OutFile install-tutor.ps1
.\install-tutor.ps1 -Version ${VERSION}
& "$env:LOCALAPPDATA\Programs\tutor-mcp\tutor-mcp.exe" --version`;

export const installationGuides: Guide[] = [
  {
    slug: 'install-client',
    title: 'Install Tutor ACP on Windows',
    description: 'Install the app, connect your AI agent and join your learning community.',
    category: 'Get started',
    readTime: '5 min',
    audience: 'Learners and course authors using Windows x64',
    sections: [
      {
        id: 'before-you-start',
        title: 'What you need',
        content: <>
          <p>Tutor ACP is the desktop app. Tutor MCP is the learning service that stores programmes, progress and session memory. Your institution can provide that service, or you can <a href="/docs/local/">install it on your own computer</a>.</p>
          <ul>
            <li>A Windows x64 computer. This release distributes the desktop app for Windows x64.</li>
            <li>Access to Codex or Claude through the provider account or authentication method offered by your agent. Provider access and any associated costs are separate from Tutor.</li>
            <li>For an institution, its HTTPS address and an administrator who can authorise your public identity.</li>
          </ul>
          <p>The app includes the Codex and Claude adapters and their Node.js runtime. You can use these bundled agents without installing Codex Desktop, npm or Node.js separately. Advanced Windows and WSL2 setups remain available in Settings.</p>
        </>,
      },
      {
        id: 'install-the-app',
        title: '1. Install the Windows app',
        content: <>
          <p><a href={CLIENT_DOWNLOAD}>Download Tutor ACP {CLIENT_VERSION} for Windows x64</a>, run the installer and open Tutor. The <a href="/download/">downloads page</a> lists the current packages and checksums.</p>
          <p>The desktop app and the server have separate version numbers. Tutor ACP {CLIENT_VERSION} is distributed in the Tutor MCP {VERSION} release. Older technical client previews remain available in the release history.</p>
          <p>If your organisation controls software installation, use its normal approval process. The release includes checksums for verifying the downloaded files.</p>
        </>,
      },
      {
        id: 'connect-your-agent',
        title: '2. Connect Codex or Claude',
        content: <>
          <ol>
            <li>Open <strong>Settings → Agents</strong>.</li>
            <li>Select the bundled Codex or Claude agent and connect it.</li>
            <li>When sign-in is requested, choose the method offered by that agent and complete the provider’s authentication flow.</li>
          </ol>
          <p>The provider connection and the Tutor connection are separate. A successful AI sign-in does not automatically grant access to an institution.</p>
          <p>The bundled agents already have a private working folder. Keep the advanced adapter and executable fields at their defaults for the standard installation.</p>
        </>,
      },
      {
        id: 'prepare-your-identity',
        title: '3. Create and back up your Tutor identity',
        content: <>
          <ol>
            <li>Open your account and find <strong>Your local identity</strong>.</li>
            <li>Create an identity, choose a backup passphrase and save the encrypted backup separately from your computer.</li>
            <li>Wait until the identity is marked <strong>Backed up</strong>.</li>
            <li>Give your institution’s administrator the two public <code>npub</code> values shown by the app: your identity and its recovery identity.</li>
          </ol>
          <p>The administrator authorises these public identities. Your private keys and backup passphrase stay with you. The passphrase protects the backup file; it is not an institution login password.</p>
          <p>Keep the backup file and its passphrase in separate safe places. See <a href="/docs/backup-and-updates/">backups and updates</a> before replacing a device or recovering an account.</p>
        </>,
      },
      {
        id: 'join-your-institution',
        title: '4. Connect to your institution',
        content: <>
          <ol>
            <li>Open <strong>Settings → Institution</strong> and select <strong>Institution or remote server</strong>.</li>
            <li>Enter the institution’s HTTPS origin, such as <code>https://tutor.example.org</code>. Use the address without <code>/mcp</code>.</li>
            <li>Select <strong>Verify institution</strong> and check the name and address returned by the service.</li>
            <li>Once your public identities have been authorised, select <strong>Connect with my account</strong>.</li>
          </ol>
          <p>The institution connection uses your local identity. Account creation by email and an institution password are not part of this native connection.</p>
        </>,
      },
      {
        id: 'check-your-first-connection',
        title: '5. Check that you are ready to learn',
        content: <>
          <p>Both the agent and Tutor should show a successful connection. Open <strong>Courses</strong> to browse the programmes available to your account. If your institution allows personal programmes, you can also start directly in <strong>Conversation</strong>.</p>
          <CodeBlock label="Your first message" code="I want to learn Go. Help me choose a realistic goal and guide me through a first 20-minute session." />
          <p>Follow the <a href="/docs/first-session/">first-session guide</a> to choose a course, record your work and resume it later. A message marked <strong>Ready to send</strong> is kept while setup is incomplete and resumes once the required connections are ready.</p>
          <p>For a personal local setup, follow <a href="/docs/local/">Install Tutor on your computer</a> instead of the institution identity steps.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Desktop client installation and connections', path: 'client/README.md' },
      { label: 'Current server and client release', path: 'docs/release-v0.7.0.md' },
      { label: 'English interface labels', path: 'client/src/messages.en.ts' },
    ],
    related: ['first-session', 'local', 'institutions', 'backup-and-updates', 'troubleshooting'],
  },
  {
    slug: 'local',
    title: 'Install Tutor on your computer',
    description: 'Keep a personal learning profile locally and let your AI client start Tutor for you.',
    category: 'Get started',
    readTime: '7 min',
    audience: 'Individual learners on Windows, macOS or Linux',
    sections: [
      {
        id: 'choose-local',
        title: 'Choose a personal local profile',
        content: <>
          <p>Local mode runs Tutor MCP on your own computer. A compatible AI client starts it with <code>--local</code> and connects through standard input and output. Your learning history is stored in a private SQLite profile.</p>
          <p>You need the Tutor MCP server binary and an AI client. The Windows Tutor ACP app is one option; Claude Code, Claude Desktop, Hermes and other local MCP clients have their own connection settings. No separate HTTP service, PostgreSQL database, Docker installation or Tutor account is required for this profile.</p>
          <p>Your AI provider connection remains separate. A local Tutor database does not make an online model work offline.</p>
        </>,
      },
      {
        id: 'install-linux-macos',
        title: '1. Install the server on Linux or macOS',
        content: <>
          <p>Download the installer, inspect the saved script, then run the last two commands. It selects Linux or macOS and x64 or ARM64, verifies the archive against <code>SHA256SUMS</code> and keeps the required licence notices beside the binary.</p>
          <CodeBlock label="Terminal · Linux or macOS" code={unixInstall} />
          <p>The version check should report <code>tutor-mcp {VERSION}</code>. Keep the complete executable path for the next step. You can add <code>$HOME/.local/bin</code> to your shell’s PATH; a desktop client may still need the absolute path.</p>
          <p>The prebuilt server does not require Go. Go is needed only if you choose to <a href={repoFile('README.md#quickstart')}>build from source</a>.</p>
        </>,
      },
      {
        id: 'install-windows',
        title: '1. Install the server on Windows',
        content: <>
          <p>Open PowerShell as your normal Windows user. Download and inspect the installer, then execute it according to your organisation’s PowerShell policy.</p>
          <CodeBlock label="PowerShell · Windows" code={windowsInstall} />
          <p>The script selects Windows x64 or ARM64, verifies the download and installs it under <code>%LOCALAPPDATA%\Programs\tutor-mcp</code>. It updates your user PATH. Restart an already-open AI client after installation.</p>
          <p>If script execution is restricted, download the matching Windows server ZIP from <a href="/download/">Downloads</a>, verify it with the release’s <code>SHA256SUMS</code>, extract the complete archive and use the full path to <code>tutor-mcp.exe</code>. Keep its licence and notice files. The Windows desktop app is a separate download and currently targets x64.</p>
        </>,
      },
      {
        id: 'connect-local-client',
        title: '2. Tell your AI client how to start Tutor',
        content: <>
          <p><strong>In Tutor ACP for Windows:</strong> open <strong>Settings → Institution</strong>, choose <strong>Local Tutor on this computer</strong>, and put the full executable path in <strong>Tutor program</strong>. Set <strong>Program arguments</strong> to the following JSON array, then select <strong>Verify local Tutor</strong>.</p>
          <CodeBlock label="Program arguments" code={'["--local"]'} />
          <p>Use a Windows executable for a native Windows agent, or a Linux executable inside the selected distribution for a WSL2 agent. The program must be available in the same environment as the agent.</p>
          <p><strong>For a client using an <code>mcpServers</code> configuration:</strong> merge this entry into its existing configuration and replace the example command with your complete installed path.</p>
          <CodeBlock label="MCP configuration · JSON" code={localConfiguration} />
          <p>On Windows, a JSON path such as <code>C:/Users/you/AppData/Local/Programs/tutor-mcp/tutor-mcp.exe</code> avoids backslash escaping. For Claude Code, Hermes and other clients, see <a href="/docs/connect-mcp-client/">Connect another MCP client</a>.</p>
          <p>Your client starts and stops the local Tutor process. Set the command and arguments in the client rather than starting a separate HTTP server.</p>
        </>,
      },
      {
        id: 'verify-local',
        title: '3. Verify your first session',
        content: <>
          <p>Restart the client if needed and check that Tutor appears as a connected MCP server. In Tutor ACP, connect your selected AI agent as well. Then ask the assistant to use Tutor explicitly.</p>
          <CodeBlock label="Try this message" code="Use Tutor MCP to help me learn Go. Find out what I already know, create a learning plan and guide me through a first 20-minute session. Save my progress when we finish." />
          <p>The assistant should use Tutor’s tools to obtain your learning context and record your work. In a later session, ask <em>“Resume my Go learning with Tutor MCP.”</em> with the same profile and client configuration.</p>
        </>,
      },
      {
        id: 'local-data',
        title: 'Know where your learning is stored',
        content: <>
          <p>The default profile directory is <code>~/.tutor-mcp/local</code>. Several local clients can share that directory and the same learner identity. To choose a different private directory, add <code>--data-dir</code> and its absolute path as separate arguments.</p>
          <CodeBlock label="Optional custom profile · program arguments" code={'["--local", "--data-dir", "/absolute/private/path/tutor"]'} />
          <p>Separate directories and remote services have separate histories. Profiles are not synchronised or converted automatically. For a backup, close every client using the profile and copy the entire profile directory, including the database and <code>keys.json</code>, together. Follow <a href="/docs/backup-and-updates/">backups and updates</a> before moving it.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Server installation', path: 'docs/installation.md' },
      { label: 'Local profiles and data', path: 'docs/profiles.md' },
      { label: 'Verified shell installer', path: 'scripts/install.sh' },
      { label: 'Windows installer', path: 'scripts/install.ps1' },
      { label: 'Desktop client connection settings', path: 'client/README.md' },
    ],
    related: ['install-client', 'connect-mcp-client', 'first-session', 'backup-and-updates', 'troubleshooting'],
  },
  {
    slug: 'hobby',
    title: 'Run Tutor for a small group',
    description: 'Host a shared Tutor MCP service on a VPS with SQLite, HTTPS and invited accounts.',
    category: 'Run Tutor',
    readTime: '10 min',
    audience: 'Operators with a VPS and basic Docker experience',
    sections: [
      {
        id: 'choose-hobby',
        title: 'Choose the hobby profile',
        content: <>
          <p>The hobby profile runs one HTTP service with a persistent SQLite database. An operator invites users through server administration; each person signs in and authorises their remote MCP client.</p>
          <p>This is the small-group route for MCP clients that support Tutor’s HTTPS and OAuth flow. For the native Tutor ACP experience with a shared institutional catalogue, public-key identities and managed roles, use <a href="/docs/deploy-institution/">the institution profile</a>.</p>
        </>,
      },
      {
        id: 'prepare-vps',
        title: '1. Prepare your VPS and domain',
        content: <>
          <ul>
            <li>Install Git and Docker with the Compose plugin on a Linux VPS.</li>
            <li>Point a domain’s DNS A record at the VPS. Add an AAAA record only if IPv6 actually reaches the server.</li>
            <li>Allow inbound TCP 80 and 443 for Caddy. Keep SSH available for administration.</li>
            <li>Replace <code>tutor.example.org</code> below with your own domain.</li>
          </ul>
          <p>Caddy obtains the HTTPS certificate when DNS and network access are ready. The Tutor application port stays inside the container network; Caddy is the public entry point.</p>
        </>,
      },
      {
        id: 'install-compose',
        title: '2. Download and initialise the service',
        content: <>
          <CodeBlock label="VPS terminal · first installation" code={`git clone --branch ${VERSION} --depth 1 ${REPO}.git
cd tutor-mcp
export TUTOR_DOMAIN=tutor.example.org
export TUTOR_VERSION=${VERSION}
docker compose -f deploy/compose.hobby.yml build
docker compose -f deploy/compose.hobby.yml run --rm --no-deps tutor \\
  init --profile hobby --data-dir /data/hobby --public-url "https://$TUTOR_DOMAIN"`} />
          <p><strong>Keep the explicit <code>TUTOR_VERSION={VERSION}</code> setting.</strong> The Compose file has an older default version. The release checkout and image build must match.</p>
          <p>Initialisation prints a private account invitation. Keep it for the next step and share it only with its intended recipient. You can open it once HTTPS is running. An initial invitation is valid for 24 hours.</p>
        </>,
      },
      {
        id: 'start-and-check',
        title: '3. Start Tutor and check HTTPS',
        content: <>
          <CodeBlock label="VPS terminal" code={'docker compose -f deploy/compose.hobby.yml up -d\ncurl --fail "https://$TUTOR_DOMAIN/ready"'} />
          <p>A successful readiness response confirms that the service and its database are available through HTTPS. If it fails, check the DNS records, firewall and Caddy logs before configuring a client.</p>
          <p>Open the private invitation over HTTPS to create the first account. Creating an account and authorising a client are separate steps.</p>
        </>,
      },
      {
        id: 'connect-a-remote-client',
        title: '4. Connect an MCP client',
        content: <>
          <p>Give each user the MCP endpoint:</p>
          <CodeBlock label="Remote MCP URL" code="https://tutor.example.org/mcp" />
          <p>In a compatible remote MCP client, add that URL, sign in with the invited account and approve the client’s access. Connector availability and allowed authentication methods depend on the client and its account policy.</p>
          <p>Follow <a href="/docs/connect-mcp-client/">Connect another MCP client</a>, then ask the assistant to use Tutor to start a learning session.</p>
        </>,
      },
      {
        id: 'manage-hobby',
        title: '5. Invite people and preserve your data',
        content: <>
          <p>Run administrative commands on the server, through the existing container:</p>
          <CodeBlock label="Invite a user" code="docker compose -f deploy/compose.hobby.yml exec tutor /usr/local/bin/tutor-mcp users invite --data-dir /data/hobby" />
          <p>The same CLI provides <code>users list</code>, <code>users reset alice</code> and <code>users disable alice</code>. Use the same profile directory as the running service.</p>
          <p>Preserve the <code>tutor_data</code>, <code>caddy_data</code> and <code>caddy_config</code> volumes through upgrades. Keep the Compose project name stable and follow <a href="/docs/backup-and-updates/">the backup procedure</a> before replacing a version. Removing the volumes also removes their data.</p>
          <p>If the default Docker subnet overlaps your network, change the subnet, static addresses and trusted proxy settings together as explained in the <a href={repoFile('docs/installation.md#hobby-vps-with-docker-compose')}>operator installation guide</a>.</p>
          <p>Prefer systemd to Docker? The repository includes a separate <a href={repoFile('docs/installation.md#native-hobby-vps-binary-systemd-and-caddy')}>native VPS procedure with Caddy</a>.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Hobby Compose installation', path: 'docs/installation.md#hobby-vps-with-docker-compose' },
      { label: 'Hobby profile and accounts', path: 'docs/profiles.md#hobby-accounts' },
      { label: 'Compose service definition', path: 'deploy/compose.hobby.yml' },
    ],
    related: ['connect-mcp-client', 'deploy-institution', 'backup-and-updates', 'troubleshooting'],
  },
  {
    slug: 'deploy-institution',
    title: 'Deploy an institutional learning service',
    description: 'An operator checklist for PostgreSQL, separate service roles, member identities and the first course.',
    category: 'Run Tutor',
    readTime: '15 min',
    audience: 'Infrastructure operators and institutional administrators',
    sections: [
      {
        id: 'plan-the-service',
        title: '1. Prepare the service and its operator',
        content: <>
          <p>An institution gives a learning community a shared catalogue, controlled membership and individually tracked learning. Learners and authors use Tutor ACP; an operator runs the service and provisions its first owner.</p>
          <p>Prepare PostgreSQL with verified TLS, a public HTTPS origin, a trusted reverse proxy, private server signing and integration-encryption keys, and separate API, worker and migrator credentials. Keep backups of the database and server keys together.</p>
          <p>The ordered steps below link to the complete <a href={repoFile('docs/installation.md#institutional-deployment')}>institutional installation procedure</a> and <a href={repoFile('OPERATIONS.md')}>operations requirements</a>. Complete the private configuration and permissions described there before starting production processes. The operator bootstrap utility is built from the release source; use the Go version declared in that release.</p>
        </>,
      },
      {
        id: 'prepare-release-and-database',
        title: '2. Use a matching release and migrate the database',
        content: <>
          <p>Use a checkout of {VERSION}. If using Compose, set its version explicitly, then build and run the one-off migrator after preparing the separate private process configurations and PostgreSQL CA file:</p>
          <CodeBlock label="Operator terminal · after private configuration" code={`export TUTOR_DOMAIN=tutor.example.org
export TUTOR_VERSION=${VERSION}
docker compose -f deploy/compose.institution.yml build
docker compose -f deploy/compose.institution.yml run --rm migrator`} />
          <p>The Compose definition has an older default version, so keep <code>TUTOR_VERSION</code> explicit. A successful migration is one checkpoint; runtime permissions and institution provisioning come next.</p>
          <p>PostgreSQL connections require <code>sslmode=verify-full</code> and an explicit CA file. The migrator uses its dedicated owner connection. Refer to the <a href={repoFile('docs/installation.md#institutional-deployment')}>configuration and migration instructions</a> for the full prerequisites.</p>
        </>,
      },
      {
        id: 'apply-runtime-roles',
        title: '3. Apply the separate database roles',
        content: <>
          <ol>
            <li>On first installation, a PostgreSQL administrator grants the owner the ability to create the required group roles, or creates <code>tutor_api</code>, <code>tutor_worker</code> and <code>tutor_restore</code> directly.</li>
            <li>As the database owner, apply <a href={repoFile('deploy/postgres-roles.sql')}><code>deploy/postgres-roles.sql</code></a>.</li>
            <li>As a PostgreSQL superuser, apply <a href={repoFile('deploy/postgres-roles-superuser.sql')}><code>deploy/postgres-roles-superuser.sql</code></a> for logical tenant restoration.</li>
            <li>Create a separate runtime login per process with its corresponding group role. Keep the owner credentials for migrations and operator bootstrap.</li>
          </ol>
          <p>Runtime API and worker logins must not own the tables or have <code>SUPERUSER</code> or <code>BYPASSRLS</code>. The server checks these restrictions at startup. Reapply the documented grants after schema upgrades.</p>
        </>,
      },
      {
        id: 'bootstrap-the-owner',
        title: '4. Provision the institution and its first owner',
        content: <>
          <ol>
            <li>The future owner installs Tutor ACP, creates a local identity and confirms its encrypted backup.</li>
            <li>The owner sends the operator the active and recovery <code>npub</code> values. The operator uses these public values for bootstrap.</li>
            <li>Build the repository’s <code>tutor-control-plane</code> utility and use the dedicated owner connection to create an active plan, provision the tenant and bootstrap its owner.</li>
            <li>Use the returned tenant ID as <code>INSTITUTION_ID</code> on the API. Set <code>BASE_URL</code> to the exact public HTTPS origin, without <code>/mcp</code>; optionally set <code>INSTITUTION_NAME</code>.</li>
          </ol>
          <p>The <a href={repoFile('docs/installation.md#create-the-institution-and-its-first-owner')}>owner bootstrap procedure</a> provides the exact <code>plan-upsert</code>, <code>provision</code>, <code>bootstrap-owner</code> and <code>add-key-member</code> commands, their public-key arguments and request identifiers. Apply them with your institution’s plan and returned tenant ID.</p>
          <p>New public identities cannot register themselves as owners. Institution registration is operator controlled in this release.</p>
        </>,
      },
      {
        id: 'start-api-worker-proxy',
        title: '5. Start the API, worker and HTTPS proxy',
        content: <>
          <p>Once migrations, grants and provisioning have succeeded, start the persistent processes:</p>
          <CodeBlock label="Operator terminal" code={'docker compose -f deploy/compose.institution.yml up -d tutor worker caddy\ncurl --fail "https://$TUTOR_DOMAIN/ready"'} />
          <p>The API and worker use their own restricted credentials; the migrator is a one-off process. Keep the application port private, preserve the public Host header and trust only the configured proxy. For systemd, follow the <a href={repoFile('docs/installation.md#run-the-separate-processes')}>separate process procedure</a> and its delivered service units.</p>
          <p>Confirm that readiness succeeds over HTTPS and that the origin exposes <code>/.well-known/tutor-institution</code> with the expected institution identity. A valid TLS certificate and successful discovery are prerequisites for the desktop connection.</p>
        </>,
      },
      {
        id: 'accept-the-first-members',
        title: '6. Verify owner, author and learner journeys',
        content: <>
          <ol>
            <li>Give the owner the service’s HTTPS origin. In Tutor ACP, verify the institution and connect using the backed-up, authorised identity.</li>
            <li>Grant the authoring permissions needed for course creation. Institution roles and the agent’s delegated tool permissions are checked separately.</li>
            <li>Create a draft, review it and publish it to the intended institution audience.</li>
            <li>Authorise a learner’s public identities, connect as that learner and verify that the permitted course can be found and joined.</li>
            <li>Complete a short learning session and check that it resumes with the expected account and course.</li>
          </ol>
          <p>Read <a href="/docs/institutions/">institution roles and membership</a> and <a href="/docs/sharing-courses/">sharing a course</a> for everyday use. Learner progress access requires the appropriate role, assignments and explicit delegation. A tool permission does not confer an administrative role.</p>
          <p>Plan the <a href={repoFile('docs/tenant-restore-runbook.md')}>tenant restore procedure</a> and <a href="/docs/backup-and-updates/">upgrade checks</a> before opening the service to a wider group.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Institutional deployment and owner bootstrap', path: 'docs/installation.md#institutional-deployment' },
      { label: 'Operations requirements', path: 'OPERATIONS.md' },
      { label: 'Institution Compose services', path: 'deploy/compose.institution.yml' },
      { label: 'Runtime database roles', path: 'deploy/postgres-roles.sql' },
      { label: 'PostgreSQL restoration roles', path: 'deploy/postgres-roles-superuser.sql' },
      { label: 'Institution profile behaviour', path: 'docs/profiles.md' },
    ],
    related: ['institutions', 'install-client', 'sharing-courses', 'backup-and-updates', 'troubleshooting'],
  },
  {
    slug: 'connect-mcp-client',
    title: 'Connect another MCP client',
    description: 'Use Tutor with a local AI client or an HTTPS connector while keeping the same learning profile.',
    category: 'Get started',
    readTime: '6 min',
    audience: 'People using Claude Code, Claude Desktop, Hermes or another MCP client',
    sections: [
      {
        id: 'choose-the-connection',
        title: 'Choose the connection your client supports',
        content: <>
          <ul>
            <li><strong>Local MCP:</strong> your client launches the installed Tutor MCP binary with <code>--local</code> over standard input and output.</li>
            <li><strong>Remote hobby service:</strong> add the public HTTPS endpoint ending in <code>/mcp</code>, sign in and authorise your client through OAuth.</li>
            <li><strong>Native institution:</strong> use Tutor ACP’s authorised public-key identity and institution-origin connection in <a href="/docs/install-client/">the Windows app guide</a>.</li>
          </ul>
          <p>The OAuth instructions on this page apply to the hobby profile. The current native institution profile uses its own signed-identity connection.</p>
          <p>Client plans, extensions and administrator policies can affect availability. The examples describe connection routes; they are not a claim that every product and account has been tested end to end.</p>
        </>,
      },
      {
        id: 'claude-code',
        title: 'Claude Code: add the local server',
        content: <>
          <p>First <a href="/docs/local/">install the server</a>. If <code>tutor-mcp</code> is available on PATH, add a user-scoped connection:</p>
          <CodeBlock label="Terminal" code="claude mcp add --transport stdio --scope user tutor -- tutor-mcp --local" />
          <p>If the command is unavailable, use the full installed executable path in its place. Follow <a href="https://code.claude.com/docs/en/mcp">Claude Code’s MCP configuration guide</a> for its current connection checks and project-scoped settings.</p>
        </>,
      },
      {
        id: 'json-clients',
        title: 'Claude Desktop and clients using MCP JSON',
        content: <>
          <p>Merge this entry into the client’s existing MCP configuration, then restart the client. Replace the example path with the installed Tutor binary. Claude Desktop exposes its configuration through <strong>Settings → Developer → Edit Config</strong>.</p>
          <CodeBlock label="MCP configuration · JSON" code={localConfiguration} />
          <p>For Windows, use a path such as <code>C:/Users/you/AppData/Local/Programs/tutor-mcp/tutor-mcp.exe</code>. Keep the <code>--local</code> argument as a separate array item.</p>
          <p>Gemini CLI also supports an <code>mcpServers</code> configuration. Other clients can use a different schema; consult the <a href={repoFile('docs/clients.md')}>client configuration map</a> and the selected product’s documentation before copying settings.</p>
        </>,
      },
      {
        id: 'hermes',
        title: 'Hermes: use its YAML configuration',
        content: <>
          <p>Add the server to <code>~/.hermes/config.yaml</code>, using an absolute command path if required, then start Hermes:</p>
          <CodeBlock label="Hermes configuration · YAML" code={'mcp_servers:\n  tutor:\n    command: tutor-mcp\n    args: ["--local"]'} />
          <p>See the <a href="https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp">Hermes MCP guide</a> for client-specific behaviour. Pi uses an MCP extension or bridge; its extension determines the configuration format.</p>
        </>,
      },
      {
        id: 'remote-oauth',
        title: 'Remote connectors: use HTTPS and authorise access',
        content: <>
          <p>Cloud-hosted assistants need a reachable remote service. For a <a href="/docs/hobby/">hobby VPS</a>, give the client this endpoint:</p>
          <CodeBlock label="Hobby MCP endpoint" code="https://tutor.example.org/mcp" />
          <ol>
            <li>Add the endpoint using the client’s MCP or custom-connector settings.</li>
            <li>Complete Tutor’s sign-in flow with your invited hobby account.</li>
            <li>Review and approve the client’s requested access.</li>
            <li>Return to the assistant and check that the Tutor tools are available.</li>
          </ol>
          <p>A publicly trusted HTTPS certificate is required. Clients such as ChatGPT, Claude on the web and Le Chat have their own connector availability and account requirements; use the current product links in the <a href={repoFile('docs/clients.md')}>client guide</a>.</p>
          <p>In Tutor ACP’s institutional settings, the address is instead the origin <code>https://tutor.example.org</code>, without <code>/mcp</code>, so the application can discover and verify the institution.</p>
        </>,
      },
      {
        id: 'test-and-resume',
        title: 'Check the learning loop and resume later',
        content: <>
          <CodeBlock label="First message" code="Use Tutor MCP to help me learn Go. Find out what I already know, create a learning plan and guide me through a first 20-minute session. Save my progress when we finish." />
          <p>The assistant must use Tutor’s tools to obtain context and record work. A conversation that does not call the learning service does not update its learning record.</p>
          <p>Later, ask <em>“Resume my Go learning with Tutor MCP.”</em> Connect to the same server or local profile to retain that history. Multiple local clients can share the same profile; independent profiles are not automatically synchronised.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Local client configuration examples', path: 'docs/installation.md#local-client-configuration' },
      { label: 'MCP client connection map', path: 'docs/clients.md' },
      { label: 'Profile and authentication differences', path: 'docs/profiles.md' },
      { label: 'Tutor ACP native institution connection', path: 'client/README.md#connexion-à-tutor' },
    ],
    related: ['local', 'hobby', 'install-client', 'first-session', 'troubleshooting'],
  },
  {
    slug: 'troubleshooting',
    title: 'Troubleshoot your setup',
    description: 'Find the next useful check for installation, agent access, institution connection and missing courses.',
    category: 'Get started',
    readTime: '6 min',
    audience: 'Learners, course authors and the people helping them connect',
    sections: [
      {
        id: 'app-or-server',
        title: 'I installed the app, but Tutor local cannot be found',
        content: <>
          <p>Tutor ACP is the desktop app; Tutor MCP is the learning server. For a personal setup, install the server as well, then set its full executable path in <strong>Settings → Institution → Tutor program</strong>.</p>
          <p>Check the binary in a terminal with <code>tutor-mcp --version</code>, or use its complete path. If it works in a terminal but not in the app, restart the app after installation or keep the full path in its settings.</p>
          <p>A native Windows agent needs a Windows executable. A WSL2 agent needs the Linux executable in the selected distribution. Set <strong>Program arguments</strong> to the JSON array <code>["--local"]</code>, then select <strong>Verify local Tutor</strong>.</p>
          <p>For an institution, the organisation runs the server. Follow <a href="/docs/install-client/">the app connection guide</a> with its HTTPS address.</p>
        </>,
      },
      {
        id: 'provider-connection',
        title: 'The agent is installed but asks me to sign in',
        content: <>
          <p>Bundled agents provide the compatible software runtime. Your provider account or API access is still required. In <strong>Settings → Agents</strong>, select Codex or Claude, connect it and complete the authentication method it offers.</p>
          <p>The provider account is separate from your Tutor identity and institution membership. Provider limits or costs are governed by that provider. Some advanced provider features also have their own system requirements; the <a href={repoFile('client/README.md#installation-windows-et-codex')}>client runtime guide</a> documents the delivered setup.</p>
          <p>For a manual WSL2 setup, check that the adapter and provider sign-in exist for the same Linux user in that distribution. Windows and WSL do not automatically share provider credentials.</p>
        </>,
      },
      {
        id: 'institution-connection',
        title: 'I cannot connect to my institution',
        content: <>
          <ol>
            <li>Check the exact HTTPS origin provided by your institution. In Tutor ACP, use the origin without <code>/mcp</code>.</li>
            <li>Select <strong>Verify institution</strong> and check the displayed name and origin.</li>
            <li>Open your account and confirm that the identity is marked <strong>Backed up</strong>.</li>
            <li>Ask the institution administrator to confirm that your current active and recovery public <code>npub</code> values are authorised and your membership is active.</li>
            <li>Try <strong>Connect with my account</strong> again.</li>
          </ol>
          <p>A remote hobby MCP client uses an endpoint ending in <code>/mcp</code> and its account’s OAuth sign-in. A native institution uses the app’s signed-identity connection. Choose the journey that matches the service you were given.</p>
          <p>If you changed computer or restored an account, use the encrypted identity backup described in <a href="/docs/backup-and-updates/">backups and updates</a>. Generating a fresh identity creates different public keys and does not restore the previous account.</p>
        </>,
      },
      {
        id: 'https-and-readiness',
        title: 'The remote service is unreachable',
        content: <>
          <p>Operators can check the public readiness endpoint:</p>
          <CodeBlock label="Terminal · replace with your service domain" code="curl --fail https://tutor.example.org/ready" />
          <p>Check DNS, inbound ports 80/443, the HTTPS certificate and the reverse proxy. An AAAA record should exist only when the service is reachable over IPv6. Keep the application port private and preserve the public Host header through the proxy.</p>
          <p>For an institution, also check that the migrator finished successfully, runtime database grants are current and the API can access PostgreSQL with its configured CA. Follow the <a href="/docs/deploy-institution/">institution deployment checklist</a> and the server’s operational logs.</p>
          <p>Fix the certificate or trust configuration at its source. The normal setup verifies HTTPS certificates.</p>
        </>,
      },
      {
        id: 'missing-course-or-action',
        title: 'A course or publishing action is missing',
        content: <>
          <p>Check the selected account, institution and active course. A different local profile or remote service has a separate learning history. The app does not automatically move courses between profiles.</p>
          <p>Courses appear according to their audience, invitations, membership and enrolment policy. Ask the owner to confirm your access. Institution roles and the agent’s delegated permissions are separate: allowing a tool does not make a learner an administrator.</p>
          <p>If a course is shared through owner review, submit the changes for review and wait for publication. A newly published version does not silently replace the version already used by enrolled learners. See <a href="/docs/sharing-courses/">sharing and contribution review</a>.</p>
          <p>Some institutions restrict personal programmes. Their catalogue and policy determine which learning routes are available.</p>
        </>,
      },
      {
        id: 'pending-or-interrupted-message',
        title: 'My message is marked Ready to send or a reply was interrupted',
        content: <>
          <p><strong>Ready to send</strong> means the app has kept your message while a required connection is incomplete. Finish the agent and Tutor setup in the same window; the app resumes the message once the connections are ready.</p>
          <p>An interrupted response remains identified as interrupted. Review what was actually saved before continuing. You can return to the same conversation and ask to resume its current course.</p>
          <p>If a tool permission is requested, review the requested operation. Saved tool choices can be changed under <strong>Settings → Agents</strong> and remain tied to the selected account, institution and agent.</p>
        </>,
      },
      {
        id: 'installer-or-upgrade',
        title: 'Installation is blocked or data is missing after an update',
        content: <>
          <p>On Windows, follow your organisation’s software and PowerShell policies. If the server script cannot run, use the matching release ZIP, verify its checksum and set the full executable path. Download the desktop installer from the project’s official release.</p>
          <p>Before changing versions, save your identity backup and learning data. If existing local data cannot be opened because its installation key is missing, restore the original database and key together. Creating a replacement key does not recover encrypted data.</p>
          <p>For an institution upgrade, the migrator and database grants must match the new server before API and worker startup. See <a href="/docs/backup-and-updates/">backups and updates</a>.</p>
          <p>When asking for help in <a href={`${REPO}/issues`}>GitHub issues</a>, include your operating system, Tutor app and server versions, the failing step and a redacted error. Keep private keys, backups, passphrases, tokens and personal learning content out of the report.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Client prerequisites and connection errors', path: 'client/README.md' },
      { label: 'Profile storage and authentication', path: 'docs/profiles.md' },
      { label: 'Installation and readiness checks', path: 'docs/installation.md' },
      { label: 'Current release and upgrade requirements', path: 'docs/release-v0.7.0.md' },
    ],
    related: ['install-client', 'local', 'hobby', 'deploy-institution', 'backup-and-updates'],
  },
];
