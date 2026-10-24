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
  tutor: { mode: 'local' | 'remote'; url: string; command: string; args: string[] };
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
  settings: Settings;
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

export interface TutorOverview {
  context: unknown;
  formations: unknown;
  availableFormations: unknown;
}

export type JsonRecord = Record<string, unknown>;
