import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { useState } from 'react';
import App from '../../src/App';
import { Onboarding } from '../../src/Onboarding';
import type { AgentTarget, Snapshot } from '../../src/types';
import { client, conversation, initialState, target } from './clientFixture';

const claude: AgentTarget = { id: 'bundled:claude', label: 'Claude', environment: 'native', available: true, detail: 'Inclus dans Tutor' };
const codex: AgentTarget = { id: 'bundled:codex', label: 'Codex', environment: 'native', available: true, detail: 'Inclus dans Tutor' };
const unauthorized = 'Clé non autorisée ou permissions insuffisantes : demandez l’accès à votre institution. (HTTP 403).';

/** A first launch: no account, no harness, no server. */
function freshState(): Snapshot {
  const state = initialState();
  state.profile = { id: 'profile-1', label: 'Compte principal' };
  state.settings.targetId = '';
  state.settings.tutor = { mode: 'remote', activity: 'auto', url: '', command: '', args: [] };
  return state;
}
function accountState(): Snapshot {
  const state = freshState();
  state.identity = { exists: true, npub: 'npub1lea', recoveryNpub: 'npub1recovery', backupConfirmed: true, needsBackup: false };
  return state;
}
function serverStepState(): Snapshot {
  const state = accountState(); state.settings.targetId = claude.id; return state;
}
function setup(state: Snapshot) {
  const fixture = client(state);
  vi.mocked(fixture.api.bundledAgents).mockResolvedValue([claude, codex]);
  render(<App api={fixture.api}/>);
  return fixture;
}
const heading = (name: string) => screen.findByRole('heading', { level: 1, name });

describe('configuration guidée au premier lancement', () => {
  it('verrouille l’accès à l’application tant que les trois étapes ne sont pas terminées', async () => {
    const fixture = setup(freshState()); const user = userEvent.setup();
    expect(await heading('Créez votre compte')).toBeTruthy();
    expect(screen.getByText('Étape 1 sur 3')).toBeTruthy();
    expect(screen.queryByLabelText('Message au tuteur')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Paramètres' })).toBeNull();
    expect(screen.queryByRole('navigation')).toBeNull();
    await user.keyboard('{Control>}k{/Control}');
    await user.keyboard('{Control>}n{/Control}');
    expect(screen.queryByRole('dialog', { name: 'Palette de commandes' })).toBeNull();
    expect(fixture.api.selectConversation).not.toHaveBeenCalled();
    expect(fixture.api.connectAgent).not.toHaveBeenCalled();
  });

  it('crée le compte avec sa sauvegarde confirmée, choisit le harness puis connecte un serveur dont l’adresse contient un chemin', async () => {
    const fixture = setup(freshState()); const user = userEvent.setup();
    await heading('Créez votre compte');
    expect(screen.getByLabelText('Nom du compte')).toHaveProperty('value', 'Compte principal');
    await user.clear(screen.getByLabelText('Nom du compte')); await user.type(screen.getByLabelText('Nom du compte'), 'Léa');
    await user.type(screen.getByLabelText('Phrase de sauvegarde'), 'courte');
    await user.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'), 'courte');
    await user.click(screen.getByRole('button', { name: 'Créer mon compte' }));
    expect((await screen.findByRole('alert')).textContent).toContain('au moins 12 caractères');
    expect(fixture.api.createIdentity).not.toHaveBeenCalled();
    await user.clear(screen.getByLabelText('Phrase de sauvegarde')); await user.type(screen.getByLabelText('Phrase de sauvegarde'), 'une phrase assez longue');
    await user.clear(screen.getByLabelText('Confirmer la phrase de sauvegarde')); await user.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'), 'une phrase assez longue');
    await user.click(screen.getByRole('button', { name: 'Créer mon compte' }));

    const harness = await heading('Choisissez votre assistant IA');
    expect(fixture.api.renameProfile).toHaveBeenCalledWith('Léa');
    expect(fixture.api.createIdentity).toHaveBeenCalledTimes(1);
    expect(fixture.api.backupIdentity).toHaveBeenCalledWith('une phrase assez longue');
    expect(fixture.api.confirmIdentityBackup).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(document.activeElement).toBe(harness));
    expect(screen.getByText('Étape 2 sur 3')).toBeTruthy();
    await user.click(await screen.findByRole('radio', { name: /Codex/ }));
    await user.click(screen.getByRole('button', { name: 'Continuer' }));

    const server = await heading('Connectez votre serveur Tutor');
    expect(fixture.api.saveSettings).toHaveBeenCalledWith(expect.objectContaining({ targetId: codex.id }));
    expect(fixture.api.connectAgent).toHaveBeenCalledWith(codex, '/home/lea/apprendre');
    await waitFor(() => expect(document.activeElement).toBe(server));
    // Only the URL is asked: no local-mode or other field.
    expect(within(screen.getByRole('form', { name: 'Serveur Tutor' })).getAllByRole('textbox')).toHaveLength(1);
    await user.type(screen.getByLabelText('Adresse de votre serveur Tutor'), '  tutor.example.org/mcp?x=1#top ');
    await user.click(screen.getByRole('button', { name: 'Tester et connecter' }));

    expect(await screen.findByText('Connecté à Institution de test.')).toBeTruthy();
    expect(fixture.api.discoverInstitution).toHaveBeenCalledWith('https://tutor.example.org');
    expect(fixture.api.saveSettings).toHaveBeenCalledWith(expect.objectContaining({ tutor: expect.objectContaining({ mode: 'remote', url: 'https://tutor.example.org' }) }));
    expect(fixture.api.connectTutor).toHaveBeenCalledWith('https://tutor.example.org', false);
    await user.click(screen.getByRole('button', { name: 'Accéder à Tutor' }));

    expect(await screen.findByLabelText('Message au tuteur')).toBeTruthy();
    expect(fixture.getState().settings.setupCompleted).toBe(true);
    expect(screen.getByRole('button', { name: 'Paramètres' })).toBeTruthy();
  });

  it('reprend à l’étape 2 puis à l’étape 3 d’après l’état enregistré, sans connexion automatique pendant la configuration', async () => {
    const first = setup(accountState());
    expect(await heading('Choisissez votre assistant IA')).toBeTruthy();
    expect(screen.getByText('Étape 2 sur 3')).toBeTruthy();
    expect(first.api.connectAgent).not.toHaveBeenCalled();
    cleanup();

    const second = setup(serverStepState());
    expect(await heading('Connectez votre serveur Tutor')).toBeTruthy();
    expect(screen.getByText('Étape 3 sur 3')).toBeTruthy();
    await new Promise(resolve => setTimeout(resolve, 50));
    expect(second.api.connectAgent).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('button', { name: 'Retour' }));
    expect(await heading('Choisissez votre assistant IA')).toBeTruthy();
  });

  it('ouvre la connexion officielle du harness quand son authentification manque', async () => {
    const fixture = setup(accountState()); const user = userEvent.setup();
    vi.mocked(fixture.api.connectAgent).mockImplementation(async () => {
      const next = fixture.getState(); next.agentConnected = true; next.agentStatus = { phase: 'running', authentication: 'none' }; fixture.setState(next);
      return { protocolVersion: 1, agentInfo: { name: 'claude' }, agentCapabilities: {}, authMethods: [{ id: 'claude-login', name: 'Login' }] };
    });
    vi.mocked(fixture.api.authenticateAgent).mockImplementation(async () => {
      const next = fixture.getState(); next.agentStatus = { phase: 'running', authentication: 'authenticated' }; fixture.setState(next); return {};
    });
    await heading('Choisissez votre assistant IA');
    await user.click(await screen.findByRole('radio', { name: /Claude/ }));
    await user.click(screen.getByRole('button', { name: 'Continuer' }));
    await user.click(await screen.findByRole('button', { name: 'Ouvrir la connexion officielle' }));
    expect(fixture.api.authenticateAgent).toHaveBeenCalledWith('');
    expect(await heading('Connectez votre serveur Tutor')).toBeTruthy();
  });

  it('refuse une adresse http distante avant toute requête et affiche l’échec de découverte de l’institution', async () => {
    const fixture = setup(serverStepState()); const user = userEvent.setup();
    await heading('Connectez votre serveur Tutor');
    const field = screen.getByLabelText('Adresse de votre serveur Tutor');
    await user.type(field, 'http://tutor.example.org{Enter}');
    expect((await screen.findByRole('alert')).textContent).toContain('n’est acceptée que pour un serveur sur cet ordinateur');
    expect(fixture.api.discoverInstitution).not.toHaveBeenCalled();

    vi.mocked(fixture.api.discoverInstitution).mockRejectedValue('L’institution est inaccessible.');
    await user.clear(field); await user.type(field, 'https://tutor.example.org{Enter}');
    expect((await screen.findByRole('alert')).textContent).toContain('L’institution est inaccessible.');
    expect(fixture.api.connectTutor).not.toHaveBeenCalled();
    expect(fixture.getState().settings.tutor.url).toBe('');
    expect(screen.queryByLabelText('Message au tuteur')).toBeNull();
  });

  it('affiche l’attente d’autorisation pour une clé non admise, copie l’identifiant public et réessaie', async () => {
    const fixture = setup(serverStepState()); const user = userEvent.setup();
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue();
    const connect = fixture.api.connectTutor;
    vi.mocked(connect).mockRejectedValueOnce(unauthorized);
    await heading('Connectez votre serveur Tutor');
    await user.type(screen.getByLabelText('Adresse de votre serveur Tutor'), 'https://tutor.example.org');
    await user.click(screen.getByRole('button', { name: 'Tester et connecter' }));
    const waiting = await screen.findByRole('region', { name: 'En attente d’autorisation' });
    expect(within(waiting).getByLabelText('Identifiant public du membre')).toHaveProperty('value', 'npub1lea');
    expect(screen.queryByRole('alert')).toBeNull();
    await user.click(within(waiting).getByRole('button', { name: 'Copier l’identifiant public' }));
    expect(copy).toHaveBeenCalledWith('npub1lea');
    await user.click(within(waiting).getByRole('button', { name: 'Réessayer' }));
    expect(await screen.findByText('Connecté à Institution de test.')).toBeTruthy();
    expect(connect).toHaveBeenCalledTimes(2);
    copy.mockRestore();
  });

  it('se termine définitivement une fois enregistrée et peut être relancée depuis les paramètres', async () => {
    const state = serverStepState(); state.settings.setupCompleted = true;
    setup(state); const user = userEvent.setup();
    expect(await screen.findByLabelText('Message au tuteur')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Paramètres' }));
    await user.click(await screen.findByRole('button', { name: 'Relancer la configuration guidée' }));
    const title = await heading('Créez votre compte');
    expect(screen.getByText('Votre compte est prêt et sauvegardé.')).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(title));
    await user.click(screen.getByRole('button', { name: 'Continuer' }));
    expect(await heading('Choisissez votre assistant IA')).toBeTruthy();
  });

  it('applique la règle de migration aux profils configurés avant la configuration guidée', async () => {
    // The shared fixture: no identity, but an agent and a local Tutor.
    setup(initialState());
    expect(await screen.findByLabelText('Message au tuteur')).toBeTruthy();
    cleanup();

    const remote = freshState(); remote.settings.targetId = claude.id; remote.conversations = [conversation];
    setup(remote);
    expect(await screen.findByLabelText('Message au tuteur')).toBeTruthy();
    cleanup();

    // A remote profile already connected to its saved server, without conversation nor confirmed backup.
    const configured = freshState(); configured.settings.targetId = claude.id; configured.settings.tutor.url = 'https://tutor.example.org';
    configured.settings.tutorConnectedOrigin = 'https://tutor.example.org';
    configured.identity = { exists: true, npub: 'npub1lea', backupConfirmed: false, needsBackup: true };
    setup(configured);
    expect(await screen.findByLabelText('Message au tuteur')).toBeTruthy();
    cleanup();

    // A new setup interrupted after saving the remote address (key awaiting authorisation) resumes at the server step.
    const interrupted = freshState(); interrupted.settings.targetId = claude.id; interrupted.settings.tutor.url = 'https://tutor.example.org';
    interrupted.identity = { exists: true, npub: 'npub1lea', backupConfirmed: true, needsBackup: false };
    setup(interrupted);
    expect(await heading('Connectez votre serveur Tutor')).toBeTruthy();
    expect(screen.queryByLabelText('Message au tuteur')).toBeNull();
    cleanup();

    const unfinished = freshState(); unfinished.settings.targetId = claude.id;
    setup(unfinished);
    expect(await heading('Créez votre compte')).toBeTruthy();
  });

  it('bloque l’application sur un écran d’erreur lorsque l’état local ne peut pas être lu, puis réessaie', async () => {
    const fixture = client(initialState());
    const load = fixture.api.loadState;
    vi.mocked(load).mockRejectedValueOnce('Coffre local illisible.');
    render(<App api={fixture.api}/>);
    expect((await screen.findByRole('alert')).textContent).toContain('Coffre local illisible.');
    expect(screen.queryByLabelText('Message au tuteur')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Réessayer' }));
    expect(await screen.findByLabelText('Message au tuteur')).toBeTruthy();
  });
});

/** Renders the setup alone, as App does, with its snapshot kept up to date. */
function ReviewHarness({ api, initial, known = [], onFinished, onExit }: { api: ReturnType<typeof client>['api']; initial: Snapshot; known?: AgentTarget[]; onFinished: () => void; onExit: () => void }) {
  const [snapshot, setSnapshot] = useState(initial);
  return <Onboarding api={api} snapshot={snapshot} review targets={known} onSnapshot={setSnapshot} onFinished={onFinished} onExit={onExit} />;
}
function localProfile(): Snapshot {
  const state = initialState(true);
  state.profile = { id: 'profile-1', label: 'Compte principal' };
  state.identity = { exists: true, npub: 'npub1lea', recoveryNpub: 'npub1recovery', backupConfirmed: true, needsBackup: false };
  state.settings.setupCompleted = true;
  return state;
}

describe('configuration guidée relancée', () => {
  it('se quitte sans rien enregistrer et ramène à l’application', async () => {
    const fixture = setup(localProfile()); const user = userEvent.setup();
    await screen.findByLabelText('Message au tuteur');
    await user.click(screen.getByRole('button', { name: 'Paramètres' }));
    await user.click(await screen.findByRole('button', { name: 'Relancer la configuration guidée' }));
    await heading('Créez votre compte');
    const saves = vi.mocked(fixture.api.saveSettings).mock.calls.length;
    await user.click(screen.getByRole('button', { name: 'Quitter la configuration' }));
    expect(await screen.findByRole('button', { name: 'Relancer la configuration guidée' })).toBeTruthy();
    expect(screen.queryByRole('heading', { level: 1, name: 'Créez votre compte' })).toBeNull();
    expect(vi.mocked(fixture.api.saveSettings).mock.calls.length).toBe(saves);
    expect(fixture.getState().settings.tutor.mode).toBe('local');
  });

  it('n’est pas proposée hors relance', async () => {
    setup(accountState());
    await heading('Choisissez votre assistant IA');
    expect(screen.queryByRole('button', { name: 'Quitter la configuration' })).toBeNull();
  });

  it('conserve le harness manuel et termine sur le serveur local prêt sans passer en mode distant', async () => {
    const state = localProfile(); state.agentConnected = false; state.settings.manualTarget = target;
    const fixture = client(state); vi.mocked(fixture.api.bundledAgents).mockResolvedValue([claude, codex]);
    const finished = vi.fn(); const user = userEvent.setup();
    render(<ReviewHarness api={fixture.api} initial={state} onFinished={finished} onExit={vi.fn()} />);
    await heading('Créez votre compte');
    await user.click(screen.getByRole('button', { name: 'Continuer' }));
    await heading('Choisissez votre assistant IA');
    const manual = await screen.findByRole('radio', { name: /Codex — Ubuntu/ });
    await waitFor(() => expect((manual as HTMLInputElement).checked).toBe(true));
    expect(screen.getByText('Configuration actuelle')).toBeTruthy();
    expect(screen.getByRole('radio', { name: /Claude/ })).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Continuer' }));
    await heading('Connectez votre serveur Tutor');
    expect(fixture.api.saveSettings).toHaveBeenCalledWith(expect.objectContaining({ targetId: target.id, manualTarget: target }));
    expect(fixture.api.connectAgent).toHaveBeenCalledWith(target, '/home/lea/apprendre');

    expect(screen.getByText('Le serveur Tutor local est prêt.')).toBeTruthy();
    expect(screen.queryByLabelText('Adresse de votre serveur Tutor')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Accéder à Tutor' }));
    await waitFor(() => expect(finished).toHaveBeenCalledOnce());
    expect(fixture.getState().settings.tutor.mode).toBe('local');
    expect(fixture.getState().settings.manualTarget).toEqual(target);
    expect(fixture.getState().settings.setupCompleted).toBe(true);
    expect(fixture.api.connectTutor).not.toHaveBeenCalled();
    expect(fixture.api.discoverInstitution).not.toHaveBeenCalled();
  });

  it('propose le harness découvert actuellement configuré et permet de choisir un serveur distant à la place du local', async () => {
    const state = localProfile();
    const fixture = client(state); vi.mocked(fixture.api.bundledAgents).mockResolvedValue([claude, codex]);
    const user = userEvent.setup();
    render(<ReviewHarness api={fixture.api} initial={state} onFinished={vi.fn()} onExit={vi.fn()} />);
    await heading('Créez votre compte');
    await user.click(screen.getByRole('button', { name: 'Continuer' }));
    const discovered = await screen.findByRole('radio', { name: /Codex — Ubuntu/ });
    expect(fixture.api.discoverAgents).toHaveBeenCalled();
    await waitFor(() => expect((discovered as HTMLInputElement).checked).toBe(true));
    await user.click(screen.getByRole('button', { name: 'Continuer' }));
    await heading('Connectez votre serveur Tutor');
    // Already connected to the configured harness: nothing is saved.
    expect(fixture.api.saveSettings).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Utiliser plutôt un serveur distant' }));
    expect(screen.getByLabelText('Adresse de votre serveur Tutor')).toBeTruthy();
  });
});

describe('sortie de secours de la configuration guidée', () => {
  function blockedState(): Snapshot {
    const state = freshState();
    state.identity = { exists: true, npub: 'npub1lea', backupConfirmed: false, needsBackup: true, needsRecoveryRestore: true };
    state.profiles = [{ id: 'profile-1', label: 'Compte principal' }, { id: 'profile-2', label: 'Compte de Léa' }];
    return state;
  }

  it('ouvre un autre compte quand la restauration est impossible, sans confirmer la sauvegarde', async () => {
    const fixture = setup(blockedState()); const user = userEvent.setup();
    await screen.findByRole('form', { name: 'Restaurer un compte' });
    expect(screen.queryByRole('button', { name: 'Créer un nouveau compte' })).toBeNull();
    const other = localProfile(); other.profile = { id: 'profile-2', label: 'Compte de Léa' }; other.profiles = blockedState().profiles;
    vi.mocked(fixture.api.selectProfile).mockImplementation(async () => { fixture.setState(other); return structuredClone(other); });
    await user.click(screen.getByText('Changer de compte'));
    await user.selectOptions(screen.getByLabelText('Autre compte sur cet ordinateur'), 'profile-2');
    await user.click(screen.getByRole('button', { name: 'Ouvrir ce compte' }));
    expect(fixture.api.selectProfile).toHaveBeenCalledWith('profile-2');
    expect(await screen.findByLabelText('Message au tuteur')).toBeTruthy();
    expect(fixture.api.confirmIdentityBackup).not.toHaveBeenCalled();
    expect(fixture.api.restoreIdentity).not.toHaveBeenCalled();
  });

  it('crée un nouveau compte qui reprend la configuration à la première étape', async () => {
    const fixture = setup(blockedState()); const user = userEvent.setup();
    await screen.findByRole('form', { name: 'Restaurer un compte' });
    const created = freshState(); created.profile = { id: 'profile-3', label: 'Nouveau départ' };
    vi.mocked(fixture.api.createProfile).mockImplementation(async () => { fixture.setState(created); return structuredClone(created); });
    await user.click(screen.getByText('Changer de compte'));
    await user.click(screen.getByRole('button', { name: 'Créer ce compte' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Indiquez le nom de votre compte.');
    expect(fixture.api.createProfile).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText('Nom du nouveau compte'), 'Nouveau départ');
    await user.click(screen.getByRole('button', { name: 'Créer ce compte' }));
    expect(fixture.api.createProfile).toHaveBeenCalledWith('Nouveau départ');
    expect(await screen.findByLabelText('Nom du compte')).toHaveProperty('value', 'Nouveau départ');
    expect(screen.getByRole('heading', { level: 1, name: 'Créez votre compte' })).toBeTruthy();
    expect(fixture.api.confirmIdentityBackup).not.toHaveBeenCalled();
  });
});
