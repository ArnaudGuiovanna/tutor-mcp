export interface AgentTarget {
  id: string;
  label: string;
  environment: 'native' | 'wsl';
  distribution?: string;
  wslUser?: string;
  adapterPath?: string;
  codexPath?: string;
  adapterVersion?: string;
  codexVersion?: string;
  available: boolean;
  detail: string;
}

export interface Settings {
  targetId: string;
  manualTarget?: AgentTarget;
  cwd: string;
  tutor: { mode: 'local' | 'remote'; activity?: 'auto' | 'author' | 'learn' | 'train'; url: string; command: string; args: string[] };
}

export interface Intent {
  id: string;
  title: string;
  motivation: string;
  startingPoint: string;
  goals: string;
  approach: string;
  constraints: string;
  designCriteria: string;
  validated: boolean;
  revision: number;
  createdAt: string;
  updatedAt: string;
  domainId?: string;
}

export interface Message {
  id: string;
  role: 'user' | 'assistant' | 'system';
  text: string;
  createdAt: string;
  status: 'complete' | 'streaming' | 'interrupted' | 'error';
}

export interface Conversation {
  id: string;
  title: string;
  intentId?: string;
  agentSessionId?: string;
  targetId: string;
  tutorBinding: string;
  createdAt: string;
  updatedAt: string;
  messages: Message[];
}

export interface TutorStatus {
  connected: boolean;
  label: string;
  message?: string;
}

export interface Snapshot {
  profileEpoch?: number;
  profile?: { id: string; label: string; npub?: string };
  profiles?: { id: string; label: string; npub?: string }[];
  profileSwitchWarning?: string;
  settings: Settings;
  identity: PublicIdentity;
  intents: Intent[];
  conversations: Conversation[];
  activeConversationId?: string;
  tutorStatus: TutorStatus;
  agentConnected: boolean;
}

export interface AuthMethod {
  id: string;
  name: string;
  description?: string;
}

export interface AgentConnection {
  protocolVersion: number;
  agentInfo: { name?: string; title?: string; version?: string };
  agentCapabilities: Record<string, unknown>;
  authMethods: AuthMethod[];
}

export interface PermissionRequest {
  id: unknown;
  sessionId?: string;
  title: string;
  description: string;
  options: { optionId: string; name: string; kind: string }[];
}

export interface ToolActivity {
  id: string;
  title: string;
  status: string;
}

export interface PublicIdentity {
  exists: boolean;
  npub?: string;
  recoveryNpub?: string;
  backupConfirmed: boolean;
  backupExported?: boolean;
  needsBackup: boolean;
  needsRecoveryRestore?: boolean;
}

export interface Institution {
  version: number;
  institution_id: string;
  name: string;
  issuer: string;
  mcp_url: string;
}

export interface TutorOverview {
  identity?: { tenantId: string; userId: string; membershipId: string; learnerId?: string; roles?: string[] };
  capabilities?: { learning?: boolean; formationAuthoring?: boolean; training?: boolean; canRequestAuthoring?: boolean; canLearn?: boolean; canEnableLearning?: boolean };
  session?: { scope: string; allowedScopes: string[] };
  context: unknown;
  formations: unknown;
  availableFormations: unknown;
  authoredFormations?: unknown;
  trainerCohorts?: { items: { cohort_id: string; cohort_name: string; formation_id: string; formation_name: string; version_id: string; enrollment_count?: number }[]; next_after: string };
}

export type JsonRecord = Record<string, unknown>;

export interface FormationVersion {
  id: string; formation_id: string; version: number; status: 'draft' | 'published' | 'archived';
}
export interface FormationReview {
  detail: {
    formation: { id: string; name: string; description: string; status: string; enrollment_policy: string };
    version: FormationVersion;
    modules: { id: string; stable_key: string; title: string; position: number }[];
    concepts: { id: string; stable_key: string; module_stable_key: string; label: string; description?: string; level?: string; outcomes?: { id: string; statement: string; observable?: string }[]; criteria?: { id: string; description: string; evidence?: string }[]; prerequisites?: string[] }[];
  };
  publishable: boolean; issues: string[]; catalog_available?: boolean; catalog_cohort_id?: string;
}
