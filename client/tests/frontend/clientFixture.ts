import { vi } from 'vitest';
import type { ClientBridge } from '../../src/bridge';
import type { AgentTarget, Conversation, Intent, JsonRecord, LocalModelStatus, Snapshot } from '../../src/types';
import { trainingFixture } from './trainingFixture';

export const target: AgentTarget = { id: 'wsl:Ubuntu', label: 'Codex — Ubuntu', environment: 'wsl', distribution: 'Ubuntu', adapterPath: '/home/lea/.local/bin/codex-acp', codexPath: '/home/lea/.local/bin/codex', available: true, detail: 'Installé dans Ubuntu (WSL2)' };
export const intent: Intent = { id: 'intent-1', title: 'Comprendre les constellations', motivation: 'Observer le ciel', startingPoint: 'Je débute', goals: 'Reconnaître trois constellations', approach: 'Exemples visuels', constraints: '20 minutes le soir', designCriteria: 'Des étapes courtes', validated: true, revision: 1, createdAt: '2026-10-24T10:00:00Z', updatedAt: '2026-10-24T10:00:00Z' };
export const conversation: Conversation = { id: 'conversation-1', title: 'Les constellations', intentId: intent.id, agentSessionId: 'agent-1', targetId: target.id, tutorBinding: 'local', createdAt: '2026-10-24T10:00:00Z', updatedAt: '2026-10-24T10:20:00Z', messages: [{ id: 'message-old', role: 'assistant', text: 'Commençons par observer la Grande Ourse.', createdAt: '2026-10-24T10:00:00Z', status: 'complete' }] };

export function initialState(connected = false): Snapshot {
  return { identity: { exists: false, backupConfirmed: false, needsBackup: false }, settings: { targetId: target.id, cwd: '/home/lea/apprendre', tutor: { mode: 'local', url: '', command: '/home/lea/bin/tutor-mcp', args: ['--local'] } }, intents: [], conversations: [], agentConnected: connected, tutorStatus: { connected, label: connected ? 'Tutor local' : 'Tutor non connecté' } };
}

export function client(initial = initialState()) {
  let state = structuredClone(initial);
  let agentListener: (event: JsonRecord) => void = () => {};
  let stateListener: (snapshot: Snapshot) => void = () => {};
  const api: ClientBridge = {
    localModelStatus: vi.fn(async (): Promise<LocalModelStatus> => ({phase:'missing', downloaded:0, total:2497281120, error:null, agentAvailable:true})),
    downloadLocalModel: vi.fn(async (): Promise<LocalModelStatus> => ({phase:'downloading', downloaded:0, total:2497281120, error:null, agentAvailable:true})),
    importLocalModel: vi.fn(async (): Promise<LocalModelStatus> => ({phase:'importing', downloaded:0, total:2497281120, error:null, agentAvailable:true})),
    cancelLocalModel: vi.fn(async (): Promise<LocalModelStatus> => ({phase:'cancelled', downloaded:0, total:2497281120, error:null, agentAvailable:true})),
    deleteProfile: vi.fn(async () => structuredClone(state)),
    institutionAction: vi.fn(async (action) => action === 'list_members' ? {items:[],next_after:''} : {institution:{id:'tenant-1',name:'Institution de test'},membership:{id:'member-1',roles:['learner'],status:'active'},capabilities:{manage_members:false,transfer_ownership:false,request_erasure:true,last_owner:false},personal_learning:true,erasure_requests:[]}),
    listMcpTools: vi.fn(async () => ({tools:[]})),
    agentConfiguration: vi.fn(async () => ({connected:state.agentConnected,configOptions:[],current:{model:'',reasoningEffort:'',speed:'balanced'},supported:{model:false,reasoningEffort:false,speed:false}})),
    setAgentConfiguration: vi.fn(async (configuration) => ({connected:state.agentConnected,configOptions:[],current:configuration,supported:{model:false,reasoningEffort:false,speed:false}})),
    trainingAction: vi.fn(async (action) => action === 'list_training_library' ? {items:[],next_after:''} : action === 'get_training_policy' ? {personal_learning:true,can_manage:false} : trainingFixture()),
    queuePrompt: vi.fn(async (text, conversationId) => { state.pendingMessage = text ? {text,conversationId} : null; return structuredClone(state); }),
    selectConversation: vi.fn(async (id) => {state.activeConversationId=id;return structuredClone(state);}),
    bundledAgents: vi.fn(async () => []),
    loadState: vi.fn(async () => structuredClone(state)),
    createProfile: vi.fn(async () => structuredClone(state)),
    selectProfile: vi.fn(async () => structuredClone(state)),
    renameProfile: vi.fn(async () => structuredClone(state)),
    openExternalLink: vi.fn(async () => {}),
    formationVersions: vi.fn(async () => ({items:[],next_after:''})),
    formationReview: vi.fn(async () => {throw new Error('Version absente');}),
    createFormation: vi.fn(async () => {throw new Error('Non configuré');}),
    publishFormation: vi.fn(async () => ({})),
    cloneFormation: vi.fn(async () => {throw new Error('Non configuré');}),
    enableLearning: vi.fn(async () => structuredClone(state)),
    discoverAgents: vi.fn(async () => [target]),
    saveSettings: vi.fn(async (settings) => { state.settings = settings; return structuredClone(state); }),
    saveIntent: vi.fn(async (draft) => { const saved = { ...draft, id: draft.id || 'new-intent', revision: draft.revision + 1, createdAt: draft.createdAt || '2026-10-24T10:00:00Z', updatedAt: '2026-10-24T10:21:00Z' }; state.intents = [...state.intents.filter((item) => item.id !== saved.id), saved]; return structuredClone(state); }),
    connectAgent: vi.fn(async () => { state.agentConnected = true; return { protocolVersion: 1, agentInfo: { name: 'codex', version: 'test' }, agentCapabilities: { loadSession: true }, authMethods: [] }; }),
    authenticateAgent: vi.fn(async () => ({})),
    startConversation: vi.fn(async (_intentId, resumeId) => { const selected = state.conversations.find((item) => item.id === resumeId) || structuredClone(conversation); state.activeConversationId = selected.id; if (!state.conversations.some((item) => item.id === selected.id)) state.conversations.push(selected); return structuredClone(selected); }),
    sendPrompt: vi.fn(async (id, text) => {state.pendingMessage=null;const current=state.conversations.find(c=>c.id===id);if(current)current.messages.push({id:crypto.randomUUID(),role:'user',text,createdAt:'2026-10-24T11:00:00Z',status:'complete'});return { stopReason: 'end_turn' };}),
    cancelPrompt: vi.fn(async () => {}),
    answerPermission: vi.fn(async () => {}),
    disconnectAgent: vi.fn(async () => { state.agentConnected = false; }),
    createIdentity: vi.fn(async () => { state.identity = { exists: true, npub: 'npub1test', recoveryNpub: 'npub1recovery', backupConfirmed: false, needsBackup: true }; return structuredClone(state.identity); }),
    backupIdentity: vi.fn(async () => { state.identity.backupExported = true; stateListener(structuredClone(state)); }),
    confirmIdentityBackup: vi.fn(async () => { state.identity.backupConfirmed = true; state.identity.backupExported = false; state.identity.needsBackup = false; return structuredClone(state.identity); }),
    restoreIdentity: vi.fn(async () => { state.identity = { exists: true, npub: 'npub1restored', recoveryNpub: 'npub1offline', backupConfirmed: true, needsBackup: false }; return structuredClone(state.identity); }),
    discoverInstitution: vi.fn(async () => ({ version: 1, institution_id: 'tenant-1', name: 'Institution de test', issuer: 'https://institution.example', mcp_url: 'https://institution.example/mcp' })),
    connectTutor: vi.fn(async () => { state.tutorStatus = { connected: true, label: 'Institution de test' }; return structuredClone(state); }),
    rotateIdentity: vi.fn(async () => structuredClone(state.identity)),
    recoverIdentity: vi.fn(async () => structuredClone(state.identity)),
    disconnectTutor: vi.fn(async () => { state.tutorStatus.connected = false; return structuredClone(state); }),
    tutorOverview: vi.fn(async () => ({ context: { domains: [] }, formations: { items: [] }, availableFormations: { items: [] } })),
    listenAgent: vi.fn(async (callback) => { agentListener = callback; return () => { agentListener = () => {}; }; }),
    listenState: vi.fn(async (callback) => { stateListener = callback; return () => { stateListener = () => {}; }; }),
    listenTutor: vi.fn(async () => () => {}),
  };
  return { api, getState: () => structuredClone(state), emit: (event: JsonRecord) => agentListener(event), setState: (next: Snapshot) => { state = structuredClone(next); }, broadcast: (next: Snapshot) => { state = structuredClone(next); stateListener(structuredClone(next)); } };
}
