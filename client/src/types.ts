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
  language?: 'fr' | 'en';
  appearance?: { theme: 'dark' | 'light' | 'system'; fontSize: number; lineHeight: number; reduceMotion: boolean };
  agent?: { model: string; reasoningEffort: string; speed: string };
  permissionPolicy?: PermissionPolicy | null;
  targetId: string;
  manualTarget?: AgentTarget;
  cwd: string;
  tutor: { mode: 'local' | 'remote'; activity?: 'auto' | 'author' | 'learn' | 'train'; url: string; command: string; args: string[] };
}

export type PermissionDecision = 'ask' | 'allow' | 'deny';
export interface PermissionPolicy { origin: string; institutionId: string; agentId: string; defaultDecision: PermissionDecision; tools: Record<string, PermissionDecision> }
export interface McpTool { name: string; description?: string; annotations?: { readOnlyHint?: boolean; destructiveHint?: boolean } }
export interface AgentConfigOption { id: string; name: string; category?: string; type: string; currentValue: string; options?: ({value: string; name: string} | {group: string; options: {value: string; name: string}[]})[] }
export interface AgentConfiguration { speedOptions?: string[]; connected: boolean; configOptions: AgentConfigOption[]; current: {model: string; reasoningEffort: string; speed: string}; supported: {model: boolean; reasoningEffort: boolean; speed: boolean} }
export interface InstitutionMember { membership_id: string; user_id: string; name?: string; public_key?: string; recovery_public_key?: string; roles: string[]; status: string }
export interface InstitutionOverview { institution: {id:string;name:string}; membership:{id:string;roles:string[];status:string}; capabilities:{manage_members:boolean;transfer_ownership:boolean;request_erasure:boolean;last_owner:boolean}; personal_learning:boolean; erasure_requests:{id:string;status:string;created_at:string;completed_at?:string}[] }

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
  trainingContext?: ConversationContext;
}

export interface Conversation {
  activeTraining?: ConversationContext;
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
  pendingMessage?: { text: string; conversationId?: string } | null;
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

export interface ConversationContext {
  conversation_id: string; training_id: string; domain_id: string; revision: string; selection_version: number; name: string;
}
export interface Training {
  can_archive?: boolean; can_restore?: boolean; can_delete?: boolean; can_withdraw?: boolean;
  id: string; learning_version_id?: string; kind: 'personal' | 'institutional' | 'shared'; name: string; description: string;
  domain_id?: string; formation_id?: string; version_id?: string; cohort_id?: string; revision: string; status: string; audience: string; author_id: string;
  can_edit: boolean; can_publish: boolean; can_review: boolean; can_learn: boolean; can_share: boolean; free_publication: boolean;
  progress: { concept: string; mastery: number; last_review?: string; next_review?: string }[];
}
export interface TrainingPage { items: Training[]; next_after: string }
export interface CurriculumConcept {
  id: string; key: string; label: string; description?: string; level: string; status: string;
  outcomes: { id: string; statement: string; observable?: string }[];
  criteria: { id: string; description: string; evidence?: string }[];
}
export interface TrainingProgramme {
  training: Training;
  curriculum?: { domain_id: string; version: number; parent_version?: number; graph: { concepts: string[]; prerequisites: Record<string, string[]> }; concepts: CurriculumConcept[]; [key: string]: unknown };
  modules: { stable_key: string; title: string; position: number; metadata_json?: string }[];
  concepts: { module_stable_key: string; stable_key: string; label: string; description?: string; level?: string; position: number; prerequisites?: string[]; outcomes?: CurriculumConcept['outcomes']; criteria?: CurriculumConcept['criteria']; metadata_json?: string }[];
}
export interface TrainingDocument extends TrainingProgramme {
  proposal?: { id: string; expected_revision: string; document: TrainingProgramme };
  submission?: { id: string; version_id: string; revision: string; status: string; reason: string; author_id: string };
  calibration?: { minimum_contributors: number; minimum_observations: number; source: string; available_concepts: string[] };
}
