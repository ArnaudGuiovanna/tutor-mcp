import { invoke, isTauri } from '@tauri-apps/api/core';
import { listen } from '@tauri-apps/api/event';
import type { AgentConfiguration, McpTool, AgentConnection, AgentTarget, Conversation, FormationReview, FormationVersion, Intent, Institution, PublicIdentity, JsonRecord, Settings, Snapshot, TutorOverview, TutorStatus } from './types';

function call<T>(command: string, args?: Record<string, unknown>): Promise<T> {
  if (!isTauri()) {
    return Promise.reject(new Error('Ouvrez Tutor depuis son application installée. La prévisualisation web ne peut pas lancer Codex ni accéder à vos connexions.'));
  }
  return invoke<T>(command, args);
}

export const bridge = {
  deleteProfile: (id: string, confirmation: string) => call<Snapshot>('delete_profile', {id, confirmation}),
  institutionAction: (action: string, input: Record<string, unknown> = {}) => call<unknown>('institution_action', {action,input}),
  listMcpTools: () => call<{tools: McpTool[]}>('list_mcp_tools'),
  agentConfiguration: () => call<AgentConfiguration>('agent_configuration'),
  setAgentConfiguration: (configuration: AgentConfiguration['current']) => call<AgentConfiguration>('set_agent_configuration', {configuration}),
  trainingAction: (action: string, input: Record<string, unknown>) => call<unknown>('training_action', { action, input }),
  queuePrompt: (text: string | null, conversationId?: string) => call<Snapshot>('queue_prompt', { text, conversationId }),
  selectConversation: (id?: string) => call<Snapshot>('select_conversation', { id }),
  bundledAgents: () => call<AgentTarget[]>('bundled_agents'),
  loadState: () => call<Snapshot>('load_state'),
  createProfile: (label: string) => call<Snapshot>('create_profile', { label }),
  selectProfile: (id: string) => call<Snapshot>('select_profile', { id }),
  renameProfile: (label: string) => call<Snapshot>('rename_profile', { label }),
  openExternalLink: (url: string) => call<void>('open_external_link', { url }),
  formationVersions: (formationId: string) => call<{ items: FormationVersion[]; next_after: string }>('formation_versions', { formationId }),
  formationReview: (versionId: string) => call<FormationReview>('formation_review', { versionId }),
  createFormation: (name: string, description: string, key: string) => call<{ formation: { id: string }; version: FormationVersion }>('create_formation', { name, description, key }),
  publishFormation: (versionId: string, key: string) => call<unknown>('publish_formation_to_catalog', { versionId, key }),
  cloneFormation: (versionId: string, key: string) => call<FormationReview['detail']>('clone_formation_version', { versionId, key }),
  enableLearning: () => call<Snapshot>('enable_learning'),
  discoverAgents: () => call<AgentTarget[]>('discover_agents'),
  saveSettings: (settings: Settings) => call<Snapshot>('save_settings', { settings }),
  saveIntent: (intent: Intent) => call<Snapshot>('save_intent', { intent }),
  connectAgent: (target: AgentTarget, cwd: string) => call<AgentConnection>('connect_agent', { target, cwd }),
  authenticateAgent: (methodId: string) => call<unknown>('authenticate_agent', { methodId }),
  startConversation: (intentId?: string, resumeId?: string) => call<Conversation>('start_conversation', { intentId, resumeId }),
  sendPrompt: (conversationId: string, text: string) => call<{ stopReason: string }>('send_prompt', { conversationId, text }),
  cancelPrompt: (conversationId: string) => call<void>('cancel_prompt', { conversationId }),
  answerPermission: (requestId: unknown, optionId: string | null) => call<void>('answer_permission', { requestId, optionId }),
  disconnectAgent: () => call<void>('disconnect_agent'),
  createIdentity: (nsec?: string) => call<PublicIdentity>('create_identity', { nsec }),
  backupIdentity: (passphrase: string) => call<void>('backup_identity', { passphrase }),
  confirmIdentityBackup: () => call<PublicIdentity>('confirm_identity_backup'),
  restoreIdentity: (passphrase: string) => call<PublicIdentity>('restore_identity', { passphrase }),
  discoverInstitution: (url: string) => call<Institution>('discover_institution', { url }),
  connectTutor: (url: string, authoring: boolean) => call<Snapshot>('connect_tutor', { url, authoring }),
  rotateIdentity: (oldPassphrase: string, newPassphrase: string) => call<PublicIdentity>('rotate_identity', { oldPassphrase, newPassphrase }),
  recoverIdentity: (url: string, oldPassphrase: string, newPassphrase: string) => call<PublicIdentity>('recover_identity', { url, oldPassphrase, newPassphrase }),
  disconnectTutor: () => call<Snapshot>('disconnect_tutor'),
  tutorOverview: () => call<TutorOverview>('tutor_overview'),
  listenAgent: (callback: (event: JsonRecord) => void) => listen<JsonRecord>('acp:event', (event) => callback(event.payload)),
  listenState: (callback: (snapshot: Snapshot) => void) => listen<Snapshot>('client:state', (event) => callback(event.payload)),
  listenTutor: (callback: (status: TutorStatus) => void) => listen<TutorStatus>('tutor:status', (event) => callback(event.payload)),
};

export type ClientBridge = typeof bridge;
