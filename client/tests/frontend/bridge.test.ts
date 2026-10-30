import { clearMocks, mockIPC } from '@tauri-apps/api/mocks';
import { emit } from '@tauri-apps/api/event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { bridge } from '../../src/bridge';
import { blankIntent } from '../../src/model';
import type { AgentTarget, Snapshot } from '../../src/types';

afterEach(() => { clearMocks(); vi.unstubAllGlobals(); });

describe('Le contrat IPC du bridge Tauri', () => {
  it('prépare le modèle local par IPC sans chemin, URL ou commande fournis par la page', async () => {
    vi.stubGlobal('isTauri', true);
    const calls: [string, unknown][] = [];
    const status = {phase:'missing', downloaded:0, total:2497281120, error:null, agentAvailable:true};
    mockIPC((command, args) => { calls.push([command, args]); return status; });
    expect(await bridge.localModelStatus()).toEqual(status);
    expect(await bridge.downloadLocalModel()).toEqual(status);
    expect(await bridge.importLocalModel()).toEqual(status);
    expect(await bridge.cancelLocalModel()).toEqual(status);
    expect(calls.map(([command]) => command)).toEqual([
      'local_model_status', 'download_local_model', 'import_local_model', 'cancel_local_model',
    ]);
    for (const [, args] of calls) expect(args ?? {}).toEqual({});
  });
  it('utilise les noms de commandes et les arguments camelCase du backend natif', async () => {
    vi.stubGlobal('isTauri', true);
    const calls: { command: string; args: unknown }[] = [];
    mockIPC((command, args) => { calls.push({ command, args: args === undefined ? {} : JSON.parse(JSON.stringify(args)) }); return {}; });
    const target: AgentTarget = { id: 'manual', label: 'Codex', environment: 'native', adapterPath: 'C:\\Codex\\codex-acp.exe', available: true, detail: '' };
    const settings = { targetId: target.id, cwd: 'C:\\Tutor', tutor: { mode: 'remote' as const, url: 'https://institution.example', command: '', args: [] } };
    const intent = { ...blankIntent(), title: 'Les constellations', validated: true };
    await bridge.loadState();
    await bridge.discoverAgents();
    await bridge.saveSettings(settings);
    await bridge.saveIntent(intent);
    await bridge.connectAgent(target, settings.cwd);
    await bridge.authenticateAgent('chatgpt');
    await bridge.startConversation('intent-1', 'conversation-1');
    await bridge.sendPrompt('conversation-1', 'Bonjour');
    await bridge.cancelPrompt('conversation-1');
    await bridge.answerPermission(7, 'allow-once');
    await bridge.answerPermission('permission-8', null);
    await bridge.disconnectAgent();
    await bridge.createIdentity();
    await bridge.backupIdentity('phrase privée de sauvegarde');
    await bridge.confirmIdentityBackup();
    await bridge.restoreIdentity('phrase privée de sauvegarde');
    await bridge.discoverInstitution(settings.tutor.url);
    await bridge.connectTutor(settings.tutor.url, false);
    await bridge.rotateIdentity('ancienne phrase privée', 'nouvelle phrase privée');
    await bridge.recoverIdentity(settings.tutor.url, 'ancienne phrase privée', 'nouvelle phrase privée');
    await bridge.disconnectTutor();
    await bridge.tutorOverview();
    await bridge.bundledAgents();
    await bridge.createProfile('Apprenant');
    await bridge.selectProfile('profile-2');
    await bridge.renameProfile('Mon nom');
    await bridge.formationVersions('formation-1');
    await bridge.formationReview('version-1');
    await bridge.createFormation('Titre','Objectif','create-key');
    await bridge.publishFormation('version-1','publish-key');
    await bridge.cloneFormation('version-1','clone-key');
    await bridge.enableLearning();

    expect(calls).toEqual([
      { command: 'load_state', args: {} },
      { command: 'discover_agents', args: {} },
      { command: 'save_settings', args: { settings } },
      { command: 'save_intent', args: { intent } },
      { command: 'connect_agent', args: { target, cwd: settings.cwd } },
      { command: 'authenticate_agent', args: { methodId: 'chatgpt' } },
      { command: 'start_conversation', args: { intentId: 'intent-1', resumeId: 'conversation-1' } },
      { command: 'send_prompt', args: { conversationId: 'conversation-1', text: 'Bonjour' } },
      { command: 'cancel_prompt', args: { conversationId: 'conversation-1' } },
      { command: 'answer_permission', args: { requestId: 7, optionId: 'allow-once' } },
      { command: 'answer_permission', args: { requestId: 'permission-8', optionId: null } },
      { command: 'disconnect_agent', args: {} },
      { command: 'create_identity', args: {} },
      { command: 'backup_identity', args: { passphrase: 'phrase privée de sauvegarde' } },
      { command: 'confirm_identity_backup', args: {} },
      { command: 'restore_identity', args: { passphrase: 'phrase privée de sauvegarde' } },
      { command: 'discover_institution', args: { url: settings.tutor.url } },
      { command: 'connect_tutor', args: { url: settings.tutor.url, authoring: false } },
      { command: 'rotate_identity', args: { oldPassphrase: 'ancienne phrase privée', newPassphrase: 'nouvelle phrase privée' } },
      { command: 'recover_identity', args: { url: settings.tutor.url, oldPassphrase: 'ancienne phrase privée', newPassphrase: 'nouvelle phrase privée' } },
      { command: 'disconnect_tutor', args: {} },
      { command: 'tutor_overview', args: {} },
      { command: 'bundled_agents', args: {} },
      { command: 'create_profile', args: {label:'Apprenant'} },
      { command: 'select_profile', args: {id:'profile-2'} },
      { command: 'rename_profile', args: {label:'Mon nom'} },
      { command: 'formation_versions', args: {formationId:'formation-1'} },
      { command: 'formation_review', args: {versionId:'version-1'} },
      { command: 'create_formation', args: {name:'Titre',description:'Objectif',key:'create-key'} },
      { command: 'publish_formation_to_catalog', args: {versionId:'version-1',key:'publish-key'} },
      { command: 'clone_formation_version', args: {versionId:'version-1',key:'clone-key'} },
      { command: 'enable_learning', args: {} },

    ]);
  });

  it('transmet les événements du runtime sans remplacer les données par une simulation', async () => {
    mockIPC(() => undefined, { shouldMockEvents: true });
    const agent = vi.fn(); const state = vi.fn(); const tutor = vi.fn();
    const stopAgent = await bridge.listenAgent(agent);
    const stopState = await bridge.listenState(state);
    const stopTutor = await bridge.listenTutor(tutor);
    const packet = { method: 'session/update', params: { sessionId: 'session-1', update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Bonjour' } } } };
    const snapshot = { agentConnected: true } as Snapshot;
    const status = { connected: true, label: 'Institution' };
    await emit('acp:event', packet);
    await emit('client:state', snapshot);
    await emit('tutor:status', status);
    expect(agent).toHaveBeenCalledWith(packet);
    expect(state).toHaveBeenCalledWith(snapshot);
    expect(tutor).toHaveBeenCalledWith(status);
    stopAgent(); stopState(); stopTutor();
  });

  it('explique l’absence de runtime dans une prévisualisation web', async () => {
    vi.stubGlobal('isTauri', false);
    const invoke = vi.fn(); mockIPC(invoke);
    await expect(bridge.discoverAgents()).rejects.toThrow('Ouvrez Tutor depuis son application installée.');
    expect(invoke).not.toHaveBeenCalled();
  });
});
