import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App from '../../src/App';
import { IdentityPanel } from '../../src/IdentityPanel';
import { SettingsWorkspace } from '../../src/SettingsWorkspace';
import type { SettingsTab } from '../../src/SettingsWorkspace';
import { WorkspaceTrainingPanel } from '../../src/WorkspaceTraining';
import { HarnessStatus } from '../../src/HarnessStatus';
import type { InstitutionOverview } from '../../src/types';
import { client, initialState } from './clientFixture';

const institution: InstitutionOverview = { institution: { id: 'tenant-1', name: 'Institution pilote' }, membership: { id: 'member-1', roles: ['owner'], status: 'active' }, capabilities: { manage_members: true, transfer_ownership: false, request_erasure: true, last_owner: false }, personal_learning: true, erasure_requests: [] };
const identity = { exists: true, npub: 'npub1identity', recoveryNpub: 'npub1recovery', backupConfirmed: false, needsBackup: true };
const run = async (_label: string, action: () => Promise<unknown>) => { await action(); return true; };
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: unknown) => void; const promise = new Promise<T>((ok, ko) => { resolve = ok; reject = ko; }); return { promise, resolve, reject }; }

describe('états de lecture et d’échec', () => {
  it('annonce la lecture des membres puis propose de réessayer après un échec', async () => {
    const state = initialState(true); const fixture = client(state);
    const first = deferred<unknown>();
    vi.mocked(fixture.api.institutionAction).mockImplementationOnce(() => first.promise)
      .mockImplementation(async action => action === 'overview' ? institution : { items: [{ membership_id: 'member-2', user_id: 'user-2', name: 'Alice', roles: ['learner'], status: 'active' }], next_after: '' });
    render(<SettingsWorkspace api={fixture.api} snapshot={state} overview={null} busy={false} initialTab="members" renderConnection={() => null} onSaved={() => {}}/>);
    expect(screen.getByText('Lecture des membres…').closest('[role="status"]')).not.toBeNull();
    first.reject(new Error('Serveur injoignable.'));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Les membres sont momentanément indisponibles.');
    expect(alert.textContent).toContain('Serveur injoignable.');
    await userEvent.click(within(alert).getByRole('button', { name: 'Réessayer' }));
    expect(await screen.findByRole('heading', { name: 'Alice' })).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('affiche une seule fois le nom de l’institution et rend la politique relisible', async () => {
    const state = initialState(true); const fixture = client(state);
    vi.mocked(fixture.api.institutionAction).mockRejectedValueOnce(new Error('Délai dépassé.')).mockResolvedValue(institution);
    render(<SettingsWorkspace api={fixture.api} snapshot={state} overview={null} busy={false} initialTab="institution" renderConnection={() => null} onSaved={() => {}}/>);
    expect(screen.getByText('Lecture de la politique de l’institution…').closest('[role="status"]')).not.toBeNull();
    await userEvent.click(within(await screen.findByRole('alert')).getByRole('button', { name: 'Réessayer' }));
    expect(await screen.findByLabelText('Autoriser les formations personnelles privées')).toBeTruthy();
    expect(screen.getAllByText('Institution pilote')).toHaveLength(1);
    expect(fixture.api.institutionAction).toHaveBeenCalledTimes(2);
  });

  it('suit la lecture des demandes d’effacement dans Mon compte', async () => {
    const state = initialState(true); const fixture = client(state);
    vi.mocked(fixture.api.institutionAction).mockRejectedValueOnce(new Error('Session expirée.'));
    render(<App api={fixture.api}/>);
    await userEvent.click(await screen.findByRole('button', { name: 'Mon compte' }));
    const alert = await screen.findByText('Le suivi de vos demandes est momentanément indisponible.', { exact: false });
    await userEvent.click(within(alert.closest('[role="alert"]') as HTMLElement).getByRole('button', { name: 'Réessayer' }));
    await waitFor(() => expect(screen.queryByText('Le suivi de vos demandes est momentanément indisponible.', { exact: false })).toBeNull());
    expect((screen.getByRole('button', { name: 'Demander l’effacement institutionnel' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('montre la lecture du programme dans le panneau de formation et le relit après un échec', async () => {
    const fixture = client(initialState(true));
    const first = deferred<unknown>();
    vi.mocked(fixture.api.trainingAction).mockImplementationOnce(() => first.promise);
    render(<WorkspaceTrainingPanel api={fixture.api} id="training-1" scope="scope" onClose={() => {}} onOpenLibrary={() => {}}/>);
    expect(screen.getByRole('status').textContent).toContain('Lecture du programme…');
    first.reject(new Error('indisponible'));
    await userEvent.click(within(await screen.findByRole('alert')).getByRole('button', { name: 'Réessayer' }));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
    expect(fixture.api.trainingAction).toHaveBeenCalledTimes(2);
  });

  it('distingue lecture, échec et absence de formations dans la barre latérale', async () => {
    const fixture = client(initialState(true));
    const first = deferred<unknown>();
    vi.mocked(fixture.api.trainingAction).mockImplementation(action => action === 'list_training_library' ? first.promise : Promise.resolve({}));
    render(<App api={fixture.api}/>);
    const sidebar = await screen.findByRole('complementary', { name: 'Barre latérale' });
    expect(await within(sidebar).findByText('Lecture des formations…')).toBeTruthy();
    expect(within(sidebar).queryByText('Aucune formation suivie')).toBeNull();
    await waitFor(() => expect(fixture.api.trainingAction).toHaveBeenCalledWith('list_training_library', expect.anything()));
    vi.mocked(fixture.api.trainingAction).mockImplementation(async action => action === 'list_training_library' ? { items: [], next_after: '' } : {});
    first.reject(new Error('indisponible'));
    await userEvent.click(await within(sidebar).findByRole('button', { name: 'Relire les formations' }));
    expect(await within(sidebar).findByText('Aucune formation suivie')).toBeTruthy();
  });
});

describe('formulaires d’identité accessibles', () => {
  it('place chaque phrase secrète dans un formulaire validable au clavier', async () => {
    const { api } = client();
    render(<IdentityPanel identity={identity} api={api} busy={false} connected={false} institutionUrl="https://tutor.example" run={run}/>);
    for (const field of document.querySelectorAll('input[type="password"]')) expect(field.closest('form')).not.toBeNull();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Phrase pour chiffrer/), 'Une longue phrase privée');
    await user.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'), 'Une longue phrase privée{Enter}');
    expect(api.backupIdentity).toHaveBeenCalledWith('Une longue phrase privée');
  });

  it('garde des identifiants d’aide uniques lorsque deux panneaux sont affichés', () => {
    const { api } = client();
    render(<><IdentityPanel identity={identity} api={api} busy={false} connected={false} institutionUrl="" run={run}/><IdentityPanel identity={identity} api={api} busy={false} connected={false} institutionUrl="" run={run}/></>);
    const fields = screen.getAllByLabelText(/Phrase pour chiffrer/);
    const ids = fields.map(field => field.getAttribute('aria-describedby'));
    expect(new Set(ids).size).toBe(2);
    for (const id of ids) expect(document.getElementById(id!)?.textContent).toContain('Au moins 12 caractères');
  });

  it('désactive la copie des identifiants pendant une opération', () => {
    const { api } = client();
    render(<IdentityPanel identity={{ ...identity, backupConfirmed: true, needsBackup: false }} api={api} busy connected={false} institutionUrl="" run={run}/>);
    expect((screen.getByRole('button', { name: 'Copier mon identifiant' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Copier les identifiants publics' }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe('onglets des paramètres', () => {
  function workspace(tab: SettingsTab, onTabChange: (tab: SettingsTab) => void, api = client(initialState(true)).api) {
    const state = initialState(true);
    return <SettingsWorkspace api={api} snapshot={state} overview={null} busy={false} tab={tab} onTabChange={onTabChange} renderConnection={() => null} onSaved={() => {}}/>;
  }

  it('expose une liste d’onglets reliée à ses panneaux et navigable aux flèches', async () => {
    const onTabChange = vi.fn();
    render(workspace('agents', onTabChange));
    const list = screen.getByRole('tablist', { name: 'Catégories des paramètres' });
    expect(list.tagName).not.toBe('NAV');
    for (const tab of within(list).getAllByRole('tab')) expect(document.getElementById(tab.getAttribute('aria-controls')!)?.getAttribute('role')).toBe('tabpanel');
    screen.getByRole('tab', { name: 'Connexion et outils' }).focus();
    await userEvent.keyboard('{ArrowLeft}');
    expect(onTabChange).toHaveBeenLastCalledWith('ergonomics');
    await userEvent.keyboard('{End}');
    expect(onTabChange).toHaveBeenLastCalledWith('ergonomics');
  });

  it('suit l’onglet demandé par la navigation sans remonter le panneau ni perdre la saisie', async () => {
    const fixture = client(initialState(true));
    vi.mocked(fixture.api.institutionAction).mockImplementation(async action => action === 'overview' ? institution : { items: [], next_after: '' });
    const view = render(workspace('members', () => {}, fixture.api));
    const search = await screen.findByLabelText('Rechercher un membre');
    await userEvent.type(search, 'Alice');
    const calls = vi.mocked(fixture.api.institutionAction).mock.calls.length;
    view.rerender(workspace('ergonomics', () => {}, fixture.api));
    expect(screen.getByRole('tab', { name: 'Ergonomie' }).getAttribute('aria-selected')).toBe('true');
    expect(screen.getByRole('heading', { name: 'Lecture et apparence' })).toBeTruthy();
    view.rerender(workspace('members', () => {}, fixture.api));
    expect((screen.getByLabelText('Rechercher un membre') as HTMLInputElement).value).toBe('Alice');
    expect(vi.mocked(fixture.api.institutionAction).mock.calls.length).toBe(calls);
  });

  it('garde la saisie lorsque la palette change d’onglet dans l’application', async () => {
    const fixture = client(initialState(true));
    vi.mocked(fixture.api.institutionAction).mockImplementation(async action => action === 'overview' ? institution : { items: [], next_after: '' });
    render(<App api={fixture.api}/>);
    await userEvent.click(await screen.findByRole('button', { name: 'Paramètres' }));
    await userEvent.click(screen.getByRole('tab', { name: 'Membres' }));
    await userEvent.type(await screen.findByLabelText('Rechercher un membre'), 'Alice');
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    await screen.findByRole('dialog', { name: 'Palette de commandes' });
    await userEvent.keyboard('Ergonomie{Enter}');
    expect(screen.getByRole('tab', { name: 'Ergonomie' }).getAttribute('aria-selected')).toBe('true');
    await userEvent.click(screen.getByRole('tab', { name: 'Membres' }));
    expect((screen.getByLabelText('Rechercher un membre') as HTMLInputElement).value).toBe('Alice');
  });
});

describe('bandeau d’état du harness', () => {
  it('donne un niveau à chaque ligne : information, alerte et avertissement', () => {
    const view = render(<HarnessStatus connected={false} reconnect={{ state: 'waiting', attempt: 1, nextAttemptAt: null, cause: null }} limit={null} busy={false} onReconnect={() => {}}/>);
    expect(document.querySelector('.harness-status-line')?.classList.contains('info')).toBe(true);
    view.rerender(<HarnessStatus connected={false} reconnect={{ state: 'connecting', attempt: 1, nextAttemptAt: null, cause: null }} limit={null} busy={false} onReconnect={() => {}}/>);
    expect(document.querySelector('.harness-status-line')?.classList.contains('info')).toBe(true);
    view.rerender(<HarnessStatus connected={false} reconnect={{ state: 'failed', attempt: 3, nextAttemptAt: null, cause: null }} limit={null} busy={false} onReconnect={() => {}}/>);
    expect(document.querySelector('.harness-status-line')?.classList.contains('failed')).toBe(true);
    view.rerender(<HarnessStatus connected reconnect={{ state: 'idle', attempt: 0, nextAttemptAt: null, cause: null }} limit={{ kind: 'rate_limit', until: Date.now() + 60_000 }} busy={false} onReconnect={() => {}}/>);
    expect(document.querySelector('.harness-status-line')?.classList.contains('limited')).toBe(true);
  });
});
