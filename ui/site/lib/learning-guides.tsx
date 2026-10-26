import Link from 'next/link';
import { CodeBlock } from '@/components/code-block';
import type { Guide } from '@/lib/docs-types';

export const learningGuides: Guide[] = [
  {
    slug: 'first-session',
    title: 'Your first learning session',
    description: 'Choose a course, explain your goal and leave with a next step you can return to.',
    category: 'Learn and share',
    readTime: '5 min',
    audience: 'Learners',
    sections: [
      {
        id: 'before-you-start',
        title: 'Start with two working connections',
        content: <>
          <p>In Tutor ACP, connect your chosen agent and your Tutor service. The agent provides the conversation; Tutor keeps the course and learning state. Your institution supplies its Tutor address and authorises your membership.</p>
          <p>If you have not connected yet, follow <Link href="/docs/install-client/">Install Tutor ACP</Link>. If you use another AI application, follow <Link href="/docs/connect-mcp-client/">Connect an MCP client</Link> instead.</p>
          <p>A message written before setup is complete stays saved as <b>Ready to send</b>. Finish connecting in the same window to continue.</p>
        </>,
      },
      {
        id: 'choose-a-course',
        title: 'Choose a course or bring your own goal',
        content: <>
          <p>Open <b>Courses</b> to find your personal programmes and the shared courses available in your institution. Read the programme, check its access conditions and choose the course you want to follow. An invitation or approval may be required.</p>
          <p>If your institution allows personal courses, start in <b>Conversation</b> with a subject you care about. Tutor looks for an existing programme before creating a new private one, and saves a new programme from the first exchange.</p>
          <CodeBlock label="A useful first message" code={'I want to learn Go so I can build a small web API.\nI already know Python and can practise for 20 minutes a day.\nHelp me find my starting point and guide me through a first session.'} />
          <p>In a general MCP client, add “Use Tutor MCP” to make clear which learning service the assistant should use.</p>
        </>,
      },
      {
        id: 'learn-by-doing',
        title: 'Try an activity, then explain your thinking',
        content: <>
          <p>The tutor can check what you already know, explain a concept and propose an exercise. Answer in your own words and show your reasoning. If you needed a hint, say so: unaided recall and assisted practice provide different evidence.</p>
          <p>You can change the pace without abandoning the course. Ask for a smaller example, a different explanation, a review or a challenge closer to your own project.</p>
          <CodeBlock label="Adjust the session" code={'Explain this with a smaller example.\nThen give me one exercise without showing the answer.\nAfter I try it, help me understand any mistakes.'} />
          <p>Check any requested tool permission before allowing it. In <b>Settings → Agents</b>, permissions can be remembered for your selected account, Tutor service and agent, then changed later.</p>
        </>,
      },
      {
        id: 'read-your-progress',
        title: 'Understand what your progress means',
        content: <>
          <p>Open the course and choose <b>Progress</b>. Tutor keeps review dates and learning evidence so the next session can build on previous work.</p>
          <ul>
            <li><b>Estimated:</b> a model uses your recorded responses to estimate what you know.</li>
            <li><b>Retained:</b> eligible delayed recall provides evidence that learning lasted.</li>
            <li><b>Demonstrated:</b> an eligible assessment supports a skill claim.</li>
            <li><b>Transferred:</b> eligible assessments show use across different situations.</li>
          </ul>
          <p>A high estimate alone does not prove mastery, and badges are not an external qualification. Treat missing evidence as something to practise, rather than a score to maximise.</p>
        </>,
      },
      {
        id: 'finish-and-return',
        title: 'Finish with a next step',
        content: <>
          <CodeBlock label="Before you leave" code={'Summarise what we worked on, including what I still find difficult.\nSave the session in Tutor and suggest a useful next step.\nTell me what is due for review.'} />
          <p>Next time, select the same account and Tutor service, reopen the conversation or choose the course in <b>Courses</b>, and ask to continue. Each conversation has its own active course; select another course explicitly when you want to switch.</p>
          <p>Your server keeps the learning record, while Tutor ACP keeps conversation files on your device. See <Link href="/docs/backup-and-updates/">Backups and updates</Link> to protect both.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Tutor ACP user guide', path: 'client/README.md' },
      { label: 'Conversation and courses', path: 'docs/tutor-conversation-formations.md' },
      { label: 'Learning evidence contract', path: 'docs/learning-integrity.md' },
    ],
    related: ['install-client', 'sharing-courses', 'backup-and-updates'],
  },
  {
    slug: 'sharing-courses',
    title: 'Create and share a course',
    description: 'Turn a personal programme into a shared course while keeping individual conversations and progress separate.',
    category: 'Learn and share',
    readTime: '6 min',
    audience: 'Learners, authors and trainers',
    sections: [
      {
        id: 'what-is-shared',
        title: 'Share a programme, keep personal learning separate',
        content: <>
          <p>A shared course gives members a common programme: concepts, prerequisites, objectives and assessment criteria. Each learner follows it with their own conversations, observations and progress.</p>
          <p>Sharing a personal programme creates a copy of its design. It does not publish your private conversations, narrative memory or individual learning state.</p>
          <div className="note"><b>Within your institution.</b> A course offered to “all members” is available to that institution’s members. It is not a public listing on this website or a cross-institution marketplace.</div>
        </>,
      },
      {
        id: 'prepare-your-course',
        title: 'Prepare a programme worth sharing',
        content: <>
          <ol>
            <li>Open <b>Courses</b> and select your personal programme or an institutional course you are authorised to edit.</li>
            <li>Choose <b>Curriculum</b> and review the concepts, prerequisites and learning objectives.</li>
            <li>Use manual editing or <b>Edit with the agent</b> to improve the same programme document.</li>
            <li>Review the preview before choosing <b>Apply changes</b>. It explains which changed concepts may have their estimates reset.</li>
          </ol>
          <CodeBlock label="Ask the agent to help" code={'Help me prepare this programme for other learners.\nMake each objective observable, check the prerequisites,\nand propose the changes for me to review before applying them.'} />
          <p>An AI proposal does not apply itself. If someone else edits the programme first, the editor keeps your text and asks you to review the newer version.</p>
        </>,
      },
      {
        id: 'choose-an-audience',
        title: 'Choose who can join',
        content: <>
          <p>Open <b>Sharing and access</b>. For a personal programme, choose <b>Share a copy of the curriculum</b> to prepare a shared version. The actions offered depend on your role and permissions.</p>
          <ul>
            <li><b>All institution members:</b> members can discover the published offer and join when admission and capacity allow.</li>
            <li><b>Invited members only:</b> access is limited to named members of the institution.</li>
            <li><b>On request:</b> a member asks for admission, an authorised manager decides, and the learner then confirms enrollment.</li>
          </ul>
          <p>An invitation or an approval does not enroll someone automatically. An approved request reserves no seat before the learner confirms enrollment.</p>
        </>,
      },
      {
        id: 'review-and-publish',
        title: 'Review contributions before publication',
        content: <>
          <p>Authors can choose <b>Submit to owner</b>. The owner reviews the submitted revision and can request corrections or choose <b>Approve and publish</b>.</p>
          <p>A later revision needs its own review. A previous approval is not permission to publish every future change. Publication without prior review is available only when explicitly authorised for that course, and the owner can withdraw that authorisation.</p>
          <p>Course contribution rights do not grant membership administration or access to another learner’s private data. Being assigned to train a group does not automatically grant permission to edit its programme.</p>
        </>,
      },
      {
        id: 'published-versions',
        title: 'Let learners keep a stable programme',
        content: <>
          <p>A published version cannot be edited in place. Publishing a new version leaves enrolled learners on the version they joined until an authorised, explicit migration.</p>
          <p>When a publication is withdrawn or archived, new enrollments stop while existing learning history is preserved. Revoking an invitation does not erase someone’s completed learning record.</p>
          <p>This lets authors improve a programme without silently changing the course underneath its learners.</p>
        </>,
      },
      {
        id: 'learn-together',
        title: 'Build a learning community',
        content: <>
          <p>Invite colleagues to improve the programme, use reviews to make changes clear, and let each member practise at their own pace. Authorised staff can follow the progress of the courses and groups assigned to them.</p>
          <p>Collective calibration uses eligible observations within the same institution, published version and concept. A shared programme does not import a learner’s private history into that process, and it does not provide a public leaderboard.</p>
          <p>See <Link href="/docs/institutions/">Learning in an institution</Link> for the member and administrator perspectives.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Course editing and sharing', path: 'docs/tutor-conversation-formations.md' },
      { label: 'Tutor ACP course screens', path: 'client/README.md' },
      { label: 'Institution admission and versioning', path: 'docs/installation.md' },
      { label: 'Collective calibration policy', path: 'docs/bkt-parameter-policy.md' },
    ],
    related: ['first-session', 'institutions', 'backup-and-updates'],
  },
  {
    slug: 'institutions',
    title: 'Learn in an institution',
    description: 'A shared catalogue, clear responsibilities and an individual learning path for every member.',
    category: 'Learn and share',
    readTime: '6 min',
    audience: 'Members, trainers and institution owners',
    sections: [
      {
        id: 'a-shared-space',
        title: 'Give your community a shared place to learn',
        content: <>
          <p>An institution can be a training organisation, school, team or learning community operating a Tutor service. Members find shared courses in one catalogue and continue learning through their own account.</p>
          <p>Tutor ACP is the desktop application. Tutor MCP is the service that stores programmes, applies access rules and keeps learning state. This website introduces the software and its documentation; it does not create or host an institution for you.</p>
          <p>If you are joining an existing organisation, start with <Link href="/docs/install-client/">the client installation guide</Link>. If you operate the service, follow <Link href="/docs/deploy-institution/">Deploy an institution</Link>.</p>
        </>,
      },
      {
        id: 'join-your-institution',
        title: 'Join with your own identity',
        content: <>
          <ol>
            <li>Create or restore your identity in Tutor ACP, then save and confirm its encrypted backup.</li>
            <li>Give your administrator the two <b>public</b> keys shown by the application: your identity key and recovery key.</li>
            <li>In <b>Settings → Institution</b>, enter the HTTPS address supplied by your organisation, without adding <code>/mcp</code>.</li>
            <li>Verify the institution’s name and address, then connect once your membership is authorised.</li>
          </ol>
          <p>The current institution profile does not register accounts by email or institution password. The administrator authorises the public identity; an unknown key cannot register itself or claim an owner role.</p>
          <p>Your private keys and backup phrase stay under your control. The institution needs neither your private key nor your backup passphrase.</p>
        </>,
      },
      {
        id: 'roles-and-responsibilities',
        title: 'Keep responsibilities clear',
        content: <>
          <div className="scroll-x"><table className="tt">
            <thead><tr><th scope="col">Your role</th><th scope="col">What it enables</th></tr></thead>
            <tbody>
              <tr><td>Learner</td><td>Find eligible courses, enroll, learn and read your own progress.</td></tr>
              <tr><td>Author or authorised editor</td><td>Prepare and contribute to the programmes you are allowed to edit.</td></tr>
              <tr><td>Trainer</td><td>Support assigned groups and read the progress permitted by those assignments.</td></tr>
              <tr><td>Pedagogy manager</td><td>Manage owned or assigned courses and their authorised learning workflows.</td></tr>
              <tr><td>Owner or administrator</td><td>Manage membership, access and institution-wide responsibilities.</td></tr>
            </tbody>
          </table></div>
          <p>A programme contribution permission and an administrative role are different grants. Allowing an AI tool does not give it additional institutional rights. The server checks the account’s actual role and assignments.</p>
          <p>Owners manage supported membership actions in <b>Settings → Members</b>. A staff member who wants to learn can start their own learning journey without changing the team responsibilities they already hold.</p>
        </>,
      },
      {
        id: 'choose-and-follow-courses',
        title: 'Choose a course and keep your own pace',
        content: <>
          <p>In <b>Courses</b>, inspect the programme before joining. Some offers are open to institution members, some require an invitation, and others require approval. Published versions provide a common structure while conversations and individual progression remain separate.</p>
          <p>Leaving a course releases its place and preserves its learning history. Rejoining the same eligible group can restore that history, subject to admission and capacity. Choosing another group creates its own enrollment history.</p>
          <p>Personal programmes depend on the institution’s learning policy. They are disabled by default in a new institution; ask the owner if the option is unavailable.</p>
        </>,
      },
      {
        id: 'support-your-learners',
        title: 'Support progress with the right context',
        content: <>
          <p>Authorised staff can follow the institutional courses and groups within their scope. An individual progress report shows the learning evidence they are permitted to read. It does not open every private conversation.</p>
          <p>Course estimates and review evidence help identify useful follow-up. Anonymous statistics suppress small groups; collective calibration requires enough independent, eligible observations within the same institution and programme version.</p>
          <p>These signals are not grades, a diagnosis or an external certificate. An absent or suppressed value is not a zero.</p>
        </>,
      },
      {
        id: 'account-and-control',
        title: 'Know what happens when an account changes',
        content: <>
          <p>The account page shows your public identity, institution and roles. Separate local accounts keep their own conversations, settings and working folders. Switching accounts ends the previous agent session and its Tutor access.</p>
          <p>Removing an account from this device and requesting erasure in an institution are separate actions. An institutional erasure concerns that organisation’s data and follows its documented processing and retention rules; it does not remove your local backups or other institutional memberships.</p>
          <p>An institution’s last active owner must transfer ownership before requesting their own erasure. See <Link href="/docs/backup-and-updates/">Backups and updates</Link> before changing devices or restoring an identity.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Project and institution overview', path: 'README.md' },
      { label: 'Native institution setup and identities', path: 'docs/installation.md' },
      { label: 'Accounts, settings and course use', path: 'client/README.md' },
      { label: 'Progress evidence and access rules', path: 'docs/learning-integrity.md' },
    ],
    related: ['install-client', 'sharing-courses', 'deploy-institution', 'backup-and-updates'],
  },
  {
    slug: 'backup-and-updates',
    title: 'Backups and updates',
    description: 'Protect your identity, conversation files and learning record before changing devices or upgrading.',
    category: 'Run Tutor',
    readTime: '6 min',
    audience: 'Everyone using or operating Tutor',
    sections: [
      {
        id: 'three-things-to-protect',
        title: 'Back up the right things',
        content: <>
          <p>Your identity, desktop files and Tutor service have different responsibilities. A backup of one does not automatically restore the others.</p>
          <div className="scroll-x"><table className="tt">
            <thead><tr><th scope="col">What</th><th scope="col">Who keeps the backup</th><th scope="col">What it protects</th></tr></thead>
            <tbody>
              <tr><td>Encrypted identity export</td><td>The account holder</td><td>Your active and recovery keys for access and account recovery.</td></tr>
              <tr><td>Tutor ACP application data</td><td>The device owner</td><td>Local conversations, settings and working files.</td></tr>
              <tr><td>Tutor MCP database and server keys</td><td>You in local mode, or the service operator</td><td>Programmes, learner state, recorded evidence and server-side memory.</td></tr>
            </tbody>
          </table></div>
        </>,
      },
      {
        id: 'identity-backup',
        title: 'Save your identity before you need it',
        content: <>
          <p>When creating an institutional identity in Tutor ACP, choose a backup phrase and save the encrypted file on a safe storage device separate from the computer. Keep the phrase separately. The application reads the written file to confirm it, then marks the identity <b>Backed up</b>.</p>
          <p>This file contains the encrypted active and recovery keys. After confirmation, the private recovery key is removed from the active vault. Share only the two public keys with an administrator.</p>
          <p>On another device, use Restore backup to keep the same identity. Use rotation or recovery only when deliberately replacing keys: this revokes the old keys and sessions. Save the replacement identity backup before completing the operation, and keep both backups until reconnection succeeds.</p>
          <div className="note"><b>Keep the backup usable.</b> Losing both the active key and the recovery backup cannot be repaired with an email reset or an institution password. The backup phrase encrypts the file; it is not a server login password.</div>
        </>,
      },
      {
        id: 'desktop-update',
        title: 'Update Tutor ACP without starting over',
        content: <>
          <ol>
            <li>Confirm you have your encrypted identity backup and its phrase.</li>
            <li>Close Tutor and preserve a private copy of its application data directory if you need to retain local conversations.</li>
            <li>Download the current stable Windows installer from <Link href="/download/">Downloads</Link> and install it over the existing application.</li>
            <li>Reopen Tutor, check the selected account and institution, and resume a known course.</li>
          </ol>
          <p>The current ACP release preserves the application identity and data locations. Do not generate a new identity or reimport your keys just because you are upgrading.</p>
          <p>The application-data folder is named <code>fr.aguiovanna.tutor</code>. Its <code>workspace.json</code> files keep conversations and settings; the identity vault is separate. Close Tutor before copying its data.</p>
          <p>The Windows vault is protected for the Windows user who created it. Copying that vault to another Windows account does not make it a portable identity backup: use the encrypted export.</p>
        </>,
      },
      {
        id: 'local-server-backup',
        title: 'Back up a local or hobby Tutor service',
        content: <>
          <p>Stop every client or service using the profile, then copy the entire profile directory to a private backup. The default directories are <code>~/.tutor-mcp/local</code> and <code>~/.tutor-mcp/hobby</code>; use your configured data directory if it is different.</p>
          <p>Keep <code>runtime.db</code>, <code>keys.json</code> and any WAL/SHM files together. Do not copy only the main database file while it is open. Restore the original database and original keys into an empty private directory, with the correct operating-system owner.</p>
          <p>Keep the previous binary and a matching backup before upgrading. Schema rollback is not automatic. If an existing encrypted database refuses to start because its key is missing, restore that original key rather than generating a replacement.</p>
          <p>See <Link href="/docs/local/">Local installation</Link> and <Link href="/docs/hobby/">Hobby hosting</Link> for their configuration paths.</p>
        </>,
      },
      {
        id: 'institution-operations',
        title: 'Let the operator protect the institution',
        content: <>
          <p>Institution operators back up PostgreSQL together with the recoverable signing and encryption keys, and test restoration in an isolated environment. Members retain their own identity exports; those do not replace a server backup.</p>
          <p>Follow the release’s migration instructions before restarting the API and worker. In the institution profile, run the migrator and reapply the delivered PostgreSQL runtime roles after a schema upgrade.</p>
          <p>A new course version and a software upgrade do not automatically publish existing personal programmes or silently move enrolled learners onto a different course version.</p>
          <p>Continue with <Link href="/docs/deploy-institution/">Institution deployment</Link> for the operator workflow, or <Link href="/docs/troubleshooting/">Troubleshooting</Link> if a restore or reconnect fails.</p>
        </>,
      },
    ],
    sources: [
      { label: 'Desktop identities, backups and upgrades', path: 'client/README.md' },
      { label: 'Service backup and restore', path: 'docs/installation.md' },
      { label: 'Profile locations and key handling', path: 'docs/profiles.md' },
      { label: 'Institution restore runbook', path: 'docs/tenant-restore-runbook.md' },
    ],
    related: ['install-client', 'local', 'deploy-institution', 'troubleshooting'],
  },
];
