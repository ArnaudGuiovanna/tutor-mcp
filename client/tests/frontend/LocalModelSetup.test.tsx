import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LocalModelSetup } from '../../src/LocalModelSetup';
import { SettingsWorkspace } from '../../src/SettingsWorkspace';
import App from '../../src/App';
import { setInterfaceLanguage } from '../../src/i18n';
import type { AgentTarget, LocalModelStatus } from '../../src/types';
import { client, initialState, target } from './clientFixture';

const missing: LocalModelStatus = {phase:'missing', downloaded:0, total:2497281120, error:null, agentAvailable:true};
const ready: LocalModelStatus = {...missing, phase:'ready', downloaded:missing.total};
const localTarget: AgentTarget = {id:'local:qwen', label:'Qwen — local (expérimental)', environment:'native', available:true, detail:'Modèle local vérifié'};

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return {promise, resolve, reject};
}
async function flush() { await act(async () => { await Promise.resolve(); }); }
async function tick() { await act(async () => { await vi.advanceTimersByTimeAsync(1000); }); }
afterEach(() => vi.useRealTimers());

describe('Préparation du modèle local', () => {
  it('lit le statut sans télécharger et présente les limites CPU sans réglage technique', async () => {
    const {api} = client();
    render(<LocalModelSetup api={api} busy={false}/>);
    await screen.findByText('Modèle à installer.');
    expect(api.localModelStatus).toHaveBeenCalledTimes(1);
    expect(api.downloadLocalModel).not.toHaveBeenCalled();
    expect(screen.getByText(/Hugging Face/)).toBeTruthy();
    expect(screen.getByText(/serveur Tutor peut être distant/)).toBeTruthy();
    expect(screen.getByText(/Quadro P2000 n’est pas encore qualifiée/)).toBeTruthy();
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.queryByRole('combobox')).toBeNull();
    expect(api.saveSettings).not.toHaveBeenCalled();
    expect(api.connectAgent).not.toHaveBeenCalled();
  });

  it('suit téléchargement puis vérification et ne notifie ready qu’une fois', async () => {
    vi.useFakeTimers();
    const {api} = client();
    const onReady = vi.fn(async () => {});
    vi.mocked(api.localModelStatus).mockResolvedValueOnce(missing)
      .mockResolvedValueOnce({...missing, phase:'downloading', downloaded:1_000_000_000})
      .mockResolvedValueOnce({...ready, phase:'checking'}).mockResolvedValue(ready);
    render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    await flush();
    fireEvent.click(screen.getByRole('button', {name:'Télécharger le modèle'}));
    await flush();
    expect(api.downloadLocalModel).toHaveBeenCalledTimes(1);
    await tick();
    expect(screen.getByRole('progressbar').getAttribute('value')).toBe('1000000000');
    expect(screen.getByText('1 Go sur 2,5 Go')).toBeTruthy();
    await tick();
    expect(screen.getByText('Vérification du modèle…')).toBeTruthy();
    expect(screen.getByRole('progressbar').hasAttribute('value')).toBe(false);
    expect(onReady).not.toHaveBeenCalled();
    await tick();
    expect(screen.getByText('Modèle vérifié.')).toBeTruthy();
    expect(onReady).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('progressbar')).toBeNull();
    const reads = vi.mocked(api.localModelStatus).mock.calls.length;
    await tick(); await tick();
    expect(api.localModelStatus).toHaveBeenCalledTimes(reads);
    fireEvent.click(screen.getByRole('button', {name:'Actualiser le statut du modèle'}));
    await flush();
    expect(onReady).toHaveBeenCalledTimes(1);
  });

  it('importe via le dialogue natif et accepte son annulation sans erreur ni chemin saisi', async () => {
    const {api} = client();
    const picker = deferred<LocalModelStatus>();
    vi.mocked(api.importLocalModel).mockReturnValueOnce(picker.promise).mockResolvedValueOnce({...missing, phase:'importing'});
    render(<LocalModelSetup api={api} busy={false}/>);
    await screen.findByText('Modèle à installer.');
    fireEvent.click(screen.getByRole('button', {name:'Choisir un GGUF existant'}));
    expect(screen.getByText('Choisissez un fichier dans la fenêtre de sélection.')).toBeTruthy();
    expect(api.importLocalModel).toHaveBeenCalledWith();
    expect((screen.getByRole('button', {name:'Télécharger le modèle'}) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => picker.resolve(missing));
    expect(screen.queryByRole('alert')).toBeNull();
    fireEvent.click(screen.getByRole('button', {name:'Choisir un GGUF existant'}));
    await screen.findByText('Copie et vérification du modèle…');
    expect(screen.getByRole('button', {name:'Annuler la préparation'})).toBeTruthy();
    expect(api.downloadLocalModel).not.toHaveBeenCalled();
  });

  it.each(['checking', 'downloading', 'importing'] as const)('annule pendant %s même si la page est occupée', async phase => {
    vi.useFakeTimers();
    const {api} = client();
    vi.mocked(api.localModelStatus).mockResolvedValue({...missing, phase});
    render(<LocalModelSetup api={api} busy/>);
    await flush();
    const cancel = screen.getByRole('button', {name:'Annuler la préparation'}) as HTMLButtonElement;
    expect(cancel.disabled).toBe(false);
    fireEvent.click(cancel);
    await flush();
    expect(api.cancelLocalModel).toHaveBeenCalledTimes(1);
    expect(screen.getByText('Préparation annulée. Vous pouvez réessayer.')).toBeTruthy();
    await tick();
    expect(api.localModelStatus).toHaveBeenCalledTimes(1);
  });

  it('ignore une ancienne lecture en vol après annulation', async () => {
    vi.useFakeTimers();
    const {api} = client();
    const stale = deferred<LocalModelStatus>();
    const onReady = vi.fn(async () => {});
    vi.mocked(api.localModelStatus).mockResolvedValueOnce({...missing, phase:'downloading'}).mockReturnValueOnce(stale.promise);
    render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    await flush(); await tick();
    fireEvent.click(screen.getByRole('button', {name:'Annuler la préparation'}));
    await flush();
    await act(async () => stale.resolve(ready));
    expect(screen.getByText('Préparation annulée. Vous pouvez réessayer.')).toBeTruthy();
    expect(onReady).not.toHaveBeenCalled();
    await tick();
    expect(api.localModelStatus).toHaveBeenCalledTimes(2);
  });

  it('arrête les lectures en quittant l’écran et retrouve un travail natif en cours au retour', async () => {
    vi.useFakeTimers();
    const {api} = client();
    const late = deferred<LocalModelStatus>();
    const onReady = vi.fn(async () => {});
    vi.mocked(api.localModelStatus).mockResolvedValueOnce({...missing, phase:'downloading'})
      .mockReturnValueOnce(late.promise).mockResolvedValue(ready);
    const view = render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    await flush(); await tick();
    view.unmount();
    await act(async () => late.resolve(ready));
    await tick();
    expect(onReady).not.toHaveBeenCalled();
    expect(api.cancelLocalModel).not.toHaveBeenCalled();
    expect(api.localModelStatus).toHaveBeenCalledTimes(2);
    render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    await flush();
    expect(onReady).toHaveBeenCalledTimes(1);
    expect(screen.getByText('Modèle vérifié.')).toBeTruthy();
  });

  it('affiche l’échec natif, permet de réessayer et ne découvre pas un modèle invalide', async () => {
    const {api} = client();
    const onReady = vi.fn(async () => {});
    vi.mocked(api.localModelStatus).mockResolvedValue({...missing, phase:'error', error:'Le fichier ne correspond pas au modèle attendu.'});
    vi.mocked(api.downloadLocalModel).mockRejectedValueOnce(new Error('Connexion indisponible.'));
    render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Le fichier ne correspond pas au modèle attendu.');
    fireEvent.click(screen.getByRole('button', {name:'Télécharger le modèle'}));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveProperty('textContent', 'Connexion indisponible.'));
    expect((screen.getByRole('button', {name:'Télécharger le modèle'}) as HTMLButtonElement).disabled).toBe(false);
    expect(onReady).not.toHaveBeenCalled();
  });

  it('récupère une erreur de lecture périodique sans lancer une seconde installation', async () => {
    vi.useFakeTimers();
    const {api} = client();
    vi.mocked(api.localModelStatus).mockResolvedValueOnce({...missing, phase:'downloading'})
      .mockRejectedValueOnce(new Error('Statut momentanément indisponible.')).mockResolvedValue(ready);
    const onReady = vi.fn(async () => {});
    render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    await flush(); await tick();
    expect(screen.getByRole('alert').textContent).toBe('Statut momentanément indisponible.');
    await tick();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(onReady).toHaveBeenCalledTimes(1);
    expect(api.downloadLocalModel).not.toHaveBeenCalled();
  });

  it('signale un agent absent et ne présente pas le modèle seul comme un agent disponible', async () => {
    const {api} = client();
    vi.mocked(api.localModelStatus).mockResolvedValue({...ready, agentAvailable:false});
    const onReady = vi.fn(async () => {});
    render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    await screen.findByText(/L’agent local est absent de cette installation/);
    expect(screen.queryByText(/Choisissez Qwen — local/)).toBeNull();
    expect(onReady).not.toHaveBeenCalled();
  });

  it('permet de relancer une actualisation des agents échouée sans réinstaller le modèle', async () => {
    const {api} = client();
    vi.mocked(api.localModelStatus).mockResolvedValue(ready);
    const onReady = vi.fn<() => Promise<void>>().mockRejectedValueOnce(new Error('Liste indisponible.')).mockResolvedValue(undefined);
    render(<LocalModelSetup api={api} busy={false} onReady={onReady}/>);
    await screen.findByRole('alert');
    fireEvent.click(screen.getByRole('button', {name:'Actualiser le statut du modèle'}));
    await waitFor(() => expect(onReady).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole('alert')).toBeNull();
    expect(api.downloadLocalModel).not.toHaveBeenCalled();
  });

  it('présente le parcours en anglais', async () => {
    setInterfaceLanguage('en');
    const {api} = client();
    render(<LocalModelSetup api={api} busy={false}/>);
    await screen.findByText('Model not yet installed.');
    expect(screen.getByRole('heading', {name:'Local Qwen (experimental)'})).toBeTruthy();
    expect(screen.getByRole('button', {name:'Download the model'})).toBeTruthy();
    expect(screen.getByRole('button', {name:'Choose an existing GGUF'})).toBeTruthy();
    expect(screen.getByText(/the Tutor server may be remote/)).toBeTruthy();
    expect(screen.getByText(/Quadro P2000 has not yet been qualified/)).toBeTruthy();
  });

  it('n’installe rien depuis les autres onglets des paramètres', async () => {
    const {api} = client();
    render(<SettingsWorkspace api={api} snapshot={initialState()} overview={null} busy={false}
      initialTab="ergonomics" renderConnection={() => null} onSaved={() => {}}/>);
    expect(api.localModelStatus).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('tab', {name:'Connexion et outils'}));
    await screen.findByText('Modèle à installer.');
    expect(api.localModelStatus).toHaveBeenCalledTimes(1);
    expect(api.downloadLocalModel).not.toHaveBeenCalled();
  });

  it('actualise les agents intégrés dans App sans découverte WSL, sélection ou connexion automatique', async () => {
    const fixture = client();
    vi.mocked(fixture.api.bundledAgents).mockResolvedValueOnce([target, {...localTarget, available:false}])
      .mockResolvedValue([localTarget]);
    vi.mocked(fixture.api.downloadLocalModel).mockResolvedValue(ready);
    render(<App api={fixture.api}/>);
    await screen.findByRole('button', {name:'Paramètres'});
    fireEvent.click(screen.getByRole('button', {name:'Paramètres'}));
    await screen.findByText('Modèle à installer.');
    const radio = screen.getByRole('radio', {name:/Qwen — local/}) as HTMLInputElement;
    expect(radio.disabled).toBe(true);
    fireEvent.click(screen.getByRole('button', {name:'Télécharger le modèle'}));
    await waitFor(() => expect(radio.disabled).toBe(false));
    expect(fixture.api.bundledAgents).toHaveBeenCalledTimes(2);
    expect(fixture.api.discoverAgents).not.toHaveBeenCalled();
    expect(radio.checked).toBe(false);
    expect((screen.getByRole('radio', {name:/Codex — Ubuntu/}) as HTMLInputElement).checked).toBe(true);
    expect(fixture.getState().settings.targetId).toBe(target.id);
    expect(fixture.api.saveSettings).not.toHaveBeenCalled();
    expect(fixture.api.connectAgent).not.toHaveBeenCalled();
    expect(fixture.api.selectProfile).not.toHaveBeenCalled();
  });
});
