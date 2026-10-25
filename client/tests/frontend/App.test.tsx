import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App from '../../src/App';
import type { ClientBridge } from '../../src/bridge';
import type { AgentTarget, Conversation, Intent, JsonRecord, Snapshot } from '../../src/types';

const target: AgentTarget = { id: 'wsl:Ubuntu', label: 'Codex — Ubuntu', environment: 'wsl', distribution: 'Ubuntu', adapterPath: '/home/lea/.local/bin/codex-acp', codexPath: '/home/lea/.local/bin/codex', available: true, detail: 'Installé dans Ubuntu (WSL2)' };
const intent: Intent = { id: 'intent-1', title: 'Comprendre les constellations', motivation: 'Observer le ciel', startingPoint: 'Je débute', goals: 'Reconnaître trois constellations', approach: 'Exemples visuels', constraints: '20 minutes le soir', designCriteria: 'Des étapes courtes', validated: true, revision: 1, createdAt: '2026-10-24T10:00:00Z', updatedAt: '2026-10-24T10:00:00Z' };
const conversation: Conversation = { id: 'conversation-1', title: 'Les constellations', intentId: intent.id, agentSessionId: 'agent-1', targetId: target.id, tutorBinding: 'local', createdAt: '2026-10-24T10:00:00Z', updatedAt: '2026-10-24T10:20:00Z', messages: [{ id: 'message-old', role: 'assistant', text: 'Commençons par observer la Grande Ourse.', createdAt: '2026-10-24T10:00:00Z', status: 'complete' }] };

function initialState(connected = false): Snapshot {
  return { identity: { exists: false, backupConfirmed: false, needsBackup: false }, settings: { targetId: target.id, cwd: '/home/lea/apprendre', tutor: { mode: 'local', url: '', command: '/home/lea/bin/tutor-mcp', args: ['--local'] } }, intents: [], conversations: [], agentConnected: connected, tutorStatus: { connected, label: connected ? 'Tutor local' : 'Tutor non connecté' } };
}

function client(initial = initialState()) {
  let state = structuredClone(initial);
  let agentListener: (event: JsonRecord) => void = () => {};
  let stateListener: (snapshot: Snapshot) => void = () => {};
  const api: ClientBridge = {
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
    sendPrompt: vi.fn(async () => ({ stopReason: 'end_turn' })),
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
  return { api, emit: (event: JsonRecord) => agentListener(event), setState: (next: Snapshot) => { state = structuredClone(next); }, broadcast: (next: Snapshot) => { state = structuredClone(next); stateListener(structuredClone(next)); } };
}

async function ready(api: ClientBridge) {
  render(<App api={api} />);
  await screen.findByRole('heading', { name: 'Une curiosité. Un nouveau départ.' });
}

describe('Le parcours de l’apprenant', () => {
  it('enregistre une intention après confirmation humaine et invalide cette confirmation après modification', async () => {
    const user = userEvent.setup();
    const { api } = client();
    await ready(api);
    await user.type(screen.getByLabelText('Ce que je souhaite apprendre'), 'Comprendre les constellations');
    await user.click(screen.getByRole('button', { name: 'Valider mon intention' }));
    expect(api.saveIntent).not.toHaveBeenCalled();
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', expect.stringContaining('Confirmez'));
    await user.click(screen.getByRole('checkbox', { name: /Cette intention correspond/ }));
    await user.click(screen.getByRole('button', { name: 'Valider mon intention' }));
    await screen.findByRole('button', { name: 'Modifier mon intention' });
    expect(api.saveIntent).toHaveBeenCalledWith(expect.objectContaining({ title: 'Comprendre les constellations', validated: true, revision: 0 }));
    expect(api.sendPrompt).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Modifier mon intention' }));
    await user.type(screen.getByLabelText('Ce que je souhaite apprendre'), ' en hiver');
    expect((screen.getByRole('checkbox', { name: /Cette intention correspond/ }) as HTMLInputElement).checked).toBe(false);
  });

  it('sélectionne une installation WSL détectée et transmet exactement son environnement', async () => {
    const user = userEvent.setup();
    const { api } = client();
    await ready(api);
    await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.click(screen.getByRole('button', { name: 'Rechercher les installations' }));
    const selection = await screen.findByRole('radio', { name: /Codex — Ubuntu/ });
    await user.click(selection);
    await user.click(screen.getByRole('button', { name: 'Connecter Codex' }));
    await waitFor(() => expect(api.connectAgent).toHaveBeenCalledWith(target, '/home/lea/apprendre'));
    expect(api.saveSettings).toHaveBeenCalledWith(expect.objectContaining({ targetId: target.id }));
    await screen.findByRole('button', { name: 'Déconnecter Codex' });
    expect(screen.getByRole('button', { name: 'Tutor non connecté' })).toBeTruthy();
  });

  it('conserve le chemin Codex explicite uniquement dans la configuration manuelle', async () => {
    const user = userEvent.setup();
    const initial = initialState(); initial.settings.cwd = 'C:\\Tutor';
    const { api } = client(initial);
    await ready(api);
    await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.click(screen.getByText('Choisir une installation manuellement'));
    await user.click(screen.getByRole('checkbox', { name: 'Utiliser les chemins ci-dessous' }));
    await user.type(screen.getByLabelText(/Chemin de l’adaptateur ACP/), 'C:\\Apps\\codex-acp.exe');
    await user.type(screen.getByLabelText(/Chemin de Codex/), 'C:\\Apps\\codex.exe');
    await user.click(screen.getByRole('button', { name: 'Enregistrer les réglages' }));
    await waitFor(() => expect(api.saveSettings).toHaveBeenCalledWith(expect.objectContaining({ targetId: 'manual', manualTarget: expect.objectContaining({ environment: 'native', adapterPath: 'C:\\Apps\\codex-acp.exe', codexPath: 'C:\\Apps\\codex.exe' }) })));
    expect(screen.getByText(/les wrappers npm .cmd ne conviennent pas/)).toBeTruthy();
  });

  it('retrouve une séance persistée sans envoyer automatiquement un nouveau message ni dupliquer le replay', async () => {
    const user = userEvent.setup();
    const initial = initialState(true);
    initial.intents = [intent]; initial.conversations = [conversation]; initial.activeConversationId = conversation.id;
    const { api, emit } = client(initial);
    await ready(api);
    await user.click(screen.getByRole('button', { name: 'Ma séance' }));
    expect((screen.getByRole('textbox', { name: 'Votre message au tuteur' }) as HTMLTextAreaElement).disabled).toBe(true);
    await act(async () => { emit({ method: 'session/update', params: { sessionId: 'agent-1', update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Commençons par observer la Grande Ourse.' } } } }); });
    expect(screen.getAllByText('Commençons par observer la Grande Ourse.')).toHaveLength(1);
    await user.click(screen.getByRole('button', { name: 'Reprendre cette séance' }));
    await waitFor(() => expect(api.startConversation).toHaveBeenCalledWith(undefined, conversation.id));
    await waitFor(() => expect((screen.getByRole('textbox', { name: 'Votre message au tuteur' }) as HTMLTextAreaElement).disabled).toBe(false));
    expect(api.sendPrompt).not.toHaveBeenCalled();
    expect(screen.getAllByText('Commençons par observer la Grande Ourse.')).toHaveLength(1);
  });

  it('affiche le streaming, respecte un refus de permission et conserve une réponse interrompue', async () => {
    const user = userEvent.setup();
    const initial = initialState(true);
    initial.intents = [intent]; initial.conversations = [conversation]; initial.activeConversationId = conversation.id;
    const { api, emit, setState, broadcast } = client(initial);
    let finish!: (value: { stopReason: string }) => void;
    vi.mocked(api.sendPrompt).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
    await ready(api);
    await user.click(screen.getByRole('button', { name: 'Reprendre ma séance' }));
    const composer = await screen.findByRole('textbox', { name: 'Votre message au tuteur' });
    await waitFor(() => expect((composer as HTMLTextAreaElement).disabled).toBe(false));
    await user.type(composer, 'Comment la repérer ?');
    await user.click(screen.getByRole('button', { name: 'Envoyer' }));
    await waitFor(() => expect(api.sendPrompt).toHaveBeenCalledWith(conversation.id, 'Comment la repérer ?'));
    const beginning = structuredClone(initial);
    beginning.conversations[0].messages.push(
      { id: 'message-new-user', role: 'user', text: 'Comment la repérer ?', createdAt: '2026-10-24T10:22:00Z', status: 'complete' },
      { id: 'message-pending', role: 'assistant', text: '', createdAt: '2026-10-24T10:22:01Z', status: 'streaming' },
    );
    await act(async () => {
      broadcast(beginning); // Native lifecycle: persist placeholder, then emit chunks.
      emit({ method: 'session/update', params: { sessionId: 'agent-1', update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'Cherchons un repère dans le ciel.' } } } });
      emit({ jsonrpc: '2.0', id: 7, method: 'session/request_permission', params: { sessionId: 'agent-1', toolCall: { title: 'Lire une ressource locale' }, options: [{ optionId: 'yes', name: 'Allow once', kind: 'allow_once' }, { optionId: 'no', name: 'Reject once', kind: 'reject_once' }] } });
    });
    expect(screen.getByText('Cherchons un repère dans le ciel.')).toBeTruthy();
    const permission = screen.getByRole('region', { name: 'Autorisation demandée' });
    await user.click(within(permission).getByRole('button', { name: 'Refuser' }));
    expect(api.answerPermission).toHaveBeenCalledWith(7, 'no');
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Autorisation demandée' })).toBeNull());
    await user.click(screen.getByRole('button', { name: 'Interrompre' }));
    expect(api.cancelPrompt).toHaveBeenCalledWith(conversation.id);
    const finalState = structuredClone(initial);
    finalState.conversations[0].messages.push({ id: 'message-new-user', role: 'user', text: 'Comment la repérer ?', createdAt: '2026-10-24T10:22:00Z', status: 'complete' }, { id: 'message-partial', role: 'assistant', text: 'Cherchons un repère dans le ciel.', createdAt: '2026-10-24T10:22:01Z', status: 'interrupted' });
    setState(finalState);
    await act(async () => { finish({ stopReason: 'cancelled' }); });
    await screen.findByText('Réponse interrompue — les éléments affichés peuvent être incomplets.');
    expect(screen.getAllByText('Cherchons un repère dans le ciel.')).toHaveLength(1);
    expect(screen.getAllByText('Comment la repérer ?')).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Interrompre' })).toBeNull();
  });

  it('ne présente pas une erreur de connexion comme une réussite', async () => {
    const user = userEvent.setup();
    const { api } = client();
    vi.mocked(api.connectAgent).mockRejectedValue(new Error('L’adaptateur ne répond pas au protocole ACP.'));
    await ready(api);
    await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.click(screen.getByRole('button', { name: 'Rechercher les installations' }));
    await screen.findByRole('radio', { name: /Codex — Ubuntu/ });
    await user.click(screen.getByRole('button', { name: 'Connecter Codex' }));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', expect.stringContaining('L’adaptateur ne répond pas'));
    expect(screen.getByRole('button', { name: 'Agent hors ligne' })).toBeTruthy();
    expect(screen.queryByText(/Connexion ACP établie/)).toBeNull();
  });

  it('présente une déconnexion normale comme une information et conserve les erreurs réelles', async () => {
    const { api, emit } = client(initialState(true));
    await ready(api);
    await act(async () => { emit({ type: 'connection', status: 'closed', message: 'Codex est déconnecté.' }); });
    expect(screen.getByText('Codex est déconnecté.')).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.getByRole('button', { name: 'Agent hors ligne' })).toBeTruthy();
    await act(async () => { emit({ type: 'connection', status: 'error', message: 'La réponse n’a pas pu être enregistrée.' }); });
    expect(screen.getByRole('alert')).toHaveProperty('textContent', expect.stringContaining('La réponse n’a pas pu être enregistrée.'));
  });

  it('bloque la connexion avant sauvegarde, puis signe avec les permissions explicitement choisies', async () => {
    const user = userEvent.setup(); const initial = initialState(); initial.settings.tutor.mode = 'remote'; initial.settings.tutor.url = 'https://institution.example';
    const { api } = client(initial); await ready(api); await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.click(screen.getByRole('button', { name: 'Vérifier l’institution' })); await screen.findByRole('region', { name: 'Institution vérifiée' });
    expect((screen.getByRole('button', { name: 'Connecter avec ma clé' }) as HTMLButtonElement).disabled).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Créer mon identité' })); await screen.findByLabelText('Identité publique (npub)');
    expect(api.connectTutor).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText(/^Phrase pour chiffrer la sauvegarde/), 'une phrase longue et privée');
    await user.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'), 'une phrase longue et privée');
    await user.click(screen.getByRole('button', { name: 'Enregistrer ma sauvegarde chiffrée' }));
    await screen.findByText('Sauvegardée');
    expect(api.confirmIdentityBackup).toHaveBeenCalledTimes(1);
    expect((screen.getByLabelText(/^Phrase pour chiffrer la sauvegarde/) as HTMLInputElement).value).toBe('');
    await waitFor(() => expect((screen.getByRole('button', { name: 'Connecter avec ma clé' }) as HTMLButtonElement).disabled).toBe(false));
    await user.click(screen.getByRole('checkbox', { name: 'Demander la création et la publication de formations' }));
    await user.click(screen.getByRole('button', { name: 'Connecter avec ma clé' }));
    await waitFor(() => expect(api.connectTutor).toHaveBeenCalledWith('https://institution.example', true));
    expect(screen.queryByLabelText(/Identifiant du client OAuth/)).toBeNull(); expect(screen.queryByText(/dans votre navigateur/)).toBeNull();
  });

  it('retrouve la confirmation d’un export en revenant sur les connexions', async () => {
    const user = userEvent.setup(); const initial = initialState();
    initial.identity = { exists: true, npub: 'npub1active', recoveryNpub: 'npub1offline', backupConfirmed: false, needsBackup: true };
    const { api } = client(initial); await ready(api);
    vi.mocked(api.confirmIdentityBackup).mockRejectedValueOnce(new Error('Le coffre ne peut pas être enregistré pour le moment.'));
    await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.type(screen.getByLabelText(/^Phrase pour chiffrer la sauvegarde/), 'une phrase pour mon fichier');
    await user.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'), 'une phrase pour mon fichier');
    await user.click(screen.getByRole('button', { name: 'Enregistrer ma sauvegarde chiffrée' }));
    await screen.findByRole('button', { name: 'Terminer ma sauvegarde' });
    await user.click(screen.getByRole('button', { name: 'Aujourd’hui' }));
    await user.click(screen.getByRole('button', { name: 'Connexions' }));
    expect(screen.getByText('Exportée · à confirmer')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Terminer ma sauvegarde' }));
    await screen.findByText('Sauvegardée');
    expect(api.backupIdentity).toHaveBeenCalledTimes(1);
    expect(api.confirmIdentityBackup).toHaveBeenCalledTimes(2);
  });

  it('explique le blocage avant confirmation de sauvegarde et peut vérifier le fichier existant', async () => {
    const user = userEvent.setup(); const initial = initialState();
    initial.settings.tutor.mode = 'remote'; initial.settings.tutor.url = 'https://institution.example';
    initial.identity = { exists: true, npub: 'npub1active', recoveryNpub: 'npub1offline', backupConfirmed: false, needsBackup: true };
    const { api, setState } = client(initial);
    vi.mocked(api.restoreIdentity).mockImplementation(async () => {
      initial.identity.backupConfirmed = true; initial.identity.needsBackup = false; setState(initial);
      return structuredClone(initial.identity);
    });
    await ready(api); await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.click(screen.getByRole('button', { name: 'Vérifier l’institution' }));
    await screen.findByRole('region', { name: 'Institution vérifiée' });
    const connect = screen.getByRole('button', { name: 'Connecter avec ma clé' }) as HTMLButtonElement;
    expect(connect.disabled).toBe(true);
    expect(screen.getByText('Confirmez votre sauvegarde dans « Votre identité locale », ou vérifiez votre fichier déjà exporté.')).toBeTruthy();
    await user.click(screen.getByText('J’ai déjà exporté ma sauvegarde'));
    await user.type(screen.getByLabelText('Phrase de la sauvegarde existante'), 'la phrase de mon fichier');
    await user.click(screen.getByRole('button', { name: 'Vérifier ma sauvegarde existante' }));
    await screen.findByText('Sauvegardée');
    await waitFor(() => expect(connect.disabled).toBe(false));
    expect(api.restoreIdentity).toHaveBeenCalledWith('la phrase de mon fichier');
    expect(api.createIdentity).not.toHaveBeenCalled(); expect(api.backupIdentity).not.toHaveBeenCalled();
    expect(api.connectTutor).not.toHaveBeenCalled();
  });

  it('restaure localement une sauvegarde sans compte e-mail', async () => {
    const user = userEvent.setup(); const { api } = client(); await ready(api); await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.click(screen.getByText('Restaurer une sauvegarde Tutor'));
    const restore = screen.getByRole('button', { name: 'Choisir et restaurer la sauvegarde' });
    await user.type(screen.getByLabelText('Phrase de la sauvegarde existante'), 'ancienne phrase privée'); await user.click(restore);
    await waitFor(() => expect(api.restoreIdentity).toHaveBeenCalledWith('ancienne phrase privée')); await screen.findByDisplayValue('npub1restored');
    expect(api.connectTutor).not.toHaveBeenCalled(); expect(api.sendPrompt).not.toHaveBeenCalled();
  });

  it('présente les droits réels du propriétaire sans inscription apprenant et demande une session d’écriture', async () => {
    const user = userEvent.setup(); const initial = initialState(true); initial.identity = { exists: true, npub: 'npub1owner', recoveryNpub: 'npub1recovery', backupConfirmed: true, needsBackup: false };
    const { api } = client(initial); vi.mocked(api.tutorOverview).mockResolvedValue({ identity: { tenantId: 'tenant', userId: 'owner', membershipId: 'staff', learnerId: '', roles: ['owner'] }, capabilities: { formationAuthoring: true }, session: { scope: 'formation:read', allowedScopes: ['formation:read', 'formation:write'] }, context: {}, formations: { items: [] }, availableFormations: { items: [] } });
    await ready(api); await user.click(screen.getByRole('button', { name: 'Mes formations' }));
    await screen.findByRole('region', { name: 'Création de formations' });
    expect((screen.getByRole('button', { name: 'Créer une formation' }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText(/Votre rôle d’équipe ne crée pas de profil apprenant/)).toBeTruthy();
    expect(api.sendPrompt).not.toHaveBeenCalled();
  });
  it('n’annonce aucune sauvegarde après un échec d’écriture et efface les phrases du formulaire', async () => {
    const user = userEvent.setup(); const initial = initialState(); initial.identity = { exists: true, npub: 'npub1active', recoveryNpub: 'npub1offline', backupConfirmed: false, needsBackup: true };
    const { api } = client(initial); vi.mocked(api.backupIdentity).mockRejectedValue(new Error('Le support de sauvegarde est inaccessible.'));
    await ready(api); await user.click(screen.getByRole('button', { name: 'Connexions' }));
    await user.type(screen.getByLabelText(/^Phrase pour chiffrer la sauvegarde/), 'une phrase pour mon fichier'); await user.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'), 'une phrase pour mon fichier');
    await user.click(screen.getByRole('button', { name: 'Enregistrer ma sauvegarde chiffrée' }));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', expect.stringContaining('support de sauvegarde est inaccessible'));
    expect(screen.queryByRole('button', { name: 'Terminer ma sauvegarde' })).toBeNull(); expect(api.confirmIdentityBackup).not.toHaveBeenCalled();
    expect((screen.getByLabelText(/^Phrase pour chiffrer la sauvegarde/) as HTMLInputElement).value).toBe(''); expect((screen.getByLabelText('Confirmer la phrase de sauvegarde') as HTMLInputElement).value).toBe('');
  });

  it('ouvre la conception autorisée même si le propriétaire possède aussi un profil apprenant', async () => {
    const user = userEvent.setup(); const { api } = client(initialState(true));
    vi.mocked(api.tutorOverview).mockResolvedValue({ identity: { tenantId: 'tenant', userId: 'owner', membershipId: 'staff', learnerId: 'actual-learner', roles: ['owner', 'learner'] }, capabilities: { learning: false, formationAuthoring: true }, session: { scope: 'formation:read formation:write', allowedScopes: ['formation:read', 'formation:write', 'learner:read', 'learner:write'] }, context: {}, formations: { items: [] }, availableFormations: { items: [] } });
    await ready(api); await user.click(screen.getByRole('button', { name: 'Mes formations' }));
    await screen.findByRole('region', { name: 'Création de formations' });
    expect((screen.getByRole('button', { name: 'Créer une formation' }) as HTMLButtonElement).disabled).toBe(false);
    expect(api.sendPrompt).not.toHaveBeenCalled();
  });

  it('change de compte sans conserver les droits affichés ni accepter les anciens événements', async()=>{
    const user=userEvent.setup();const initial=initialState(true);
    initial.profileEpoch=1;initial.profile={id:'principal',label:'Direction'};initial.profiles=[initial.profile,{id:'second',label:'Compte test'}];initial.identity={exists:true,npub:'npub1owner',backupConfirmed:true,needsBackup:false};
    const {api,setState,emit}=client(initial);
    vi.mocked(api.tutorOverview).mockResolvedValue({identity:{tenantId:'t',userId:'u',membershipId:'m',roles:['owner']},capabilities:{learning:false,canRequestAuthoring:true},context:{},formations:{items:[]},availableFormations:{items:[]}});
    vi.mocked(api.selectProfile).mockImplementation(async()=>{const next=initialState();next.profileEpoch=2;next.profile={id:'second',label:'Compte test'};next.profiles=initial.profiles;next.identity={exists:true,npub:'npub1second',backupConfirmed:false,needsBackup:true};setState(next);return next;});
    await ready(api);await screen.findAllByText('Propriétaire');
    await user.click(screen.getByLabelText('Mon compte et mes rôles'));await user.click(screen.getByRole('button',{name:'Compte test'}));
    await screen.findByDisplayValue('npub1second');expect(screen.queryByText('Propriétaire')).toBeNull();
    await act(async()=>{emit({profileEpoch:1,type:'connection',status:'error',message:'ancien compte'});});
    expect(screen.queryByText('ancien compte')).toBeNull();expect(api.selectProfile).toHaveBeenCalledWith('second');
  });

});
