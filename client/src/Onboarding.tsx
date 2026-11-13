// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { t, useTranslation } from './i18n';
import { Icon } from './icons';
import { WindowChrome } from './WindowChrome';
import type { ClientBridge } from './bridge';
import type { AgentTarget, Institution, Snapshot } from './types';
import { accountReady, awaitingAuthorization, normalizeTutorUrl, onboardingStep, serverReady } from './onboardingModel';
import './onboarding.css';

type Step = 1 | 2 | 3;
type Outcome<T> = { ok: true; value: T } | { ok: false; raw: string };

export interface OnboardingProps {
  api: ClientBridge;
  snapshot: Snapshot;
  /** Reopened from the settings: every step is shown again from the first one. */
  review?: boolean;
  targets?: AgentTarget[];
  onSnapshot: (next: Snapshot) => void;
  onTargets?: (targets: AgentTarget[]) => void;
  onFinished: () => void;
  /** Review only: back to the application without saving anything. */
  onExit?: () => void;
  /** Another profile was selected or created: the setup restarts for it. */
  onProfile?: (next: Snapshot) => void;
}

function rawMessage(reason: unknown): string {
  return reason instanceof Error ? reason.message : typeof reason === 'string' ? reason : 'L’opération a échoué. Réessayez après avoir vérifié les connexions.';
}

/** One asynchronous action at a time per step, with its own busy label and error. */
function useTask() {
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  async function run<T>(label: string, action: () => Promise<T>): Promise<Outcome<T>> {
    setBusy(label); setError('');
    try { return { ok: true, value: await action() }; }
    catch (reason) { const raw = rawMessage(reason); setError(raw); return { ok: false, raw }; }
    finally { setBusy(''); }
  }
  return { busy, error, setError, run };
}

const titles: Record<Step, [string, string]> = {
  1: ['Créez votre compte', 'Votre compte reste sur cet ordinateur. Une sauvegarde chiffrée permet de le retrouver.'],
  2: ['Choisissez votre assistant IA', 'Tutor s’appuie sur Claude ou Codex, avec votre propre compte fournisseur.'],
  3: ['Connectez votre serveur Tutor', 'Saisissez l’adresse fournie par votre institution.'],
};

export function Onboarding({ api, snapshot, review = false, targets = [], onSnapshot, onTargets, onFinished, onExit, onProfile }: OnboardingProps) {
  useTranslation();
  const derived = onboardingStep(snapshot, { ignoreCompletion: true });
  const [step, setStep] = useState<Step>(() => review ? 1 : derived === 'done' ? 3 : derived);
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => { heading.current?.focus(); }, [step]);
  // A step that is no longer satisfied (for example a removed identity) is shown again.
  useEffect(() => { if (derived !== 'done' && derived < step) setStep(derived); }, [derived, step]);
  async function refresh() { const next = await api.loadState(); onSnapshot(next); return next; }
  const back = step > 1 ? () => setStep((step - 1) as Step) : undefined;
  const [title, lead] = titles[step];
  return <div className="app-shell setup-shell">
    <WindowChrome />
    <main id="main-content" className="onboarding">
      <div className="onboarding-panel">
        <div className="onboarding-progress">
          <p>{t('Étape {current} sur {total}', { current: step, total: 3 })}</p>
          <ol aria-hidden="true">{([1, 2, 3] as const).map(item => <li key={item} className={item === step ? 'current' : item < step ? 'complete' : ''} />)}</ol>
        </div>
        <h1 ref={heading} tabIndex={-1}>{t(title)}</h1>
        <p className="onboarding-lead">{t(lead)}</p>
        {review && onExit && <div className="onboarding-exit"><button type="button" className="link-button" onClick={onExit}>{t('Quitter la configuration')}</button></div>}
        {step === 1 && <StepAccount api={api} snapshot={snapshot} refresh={refresh} onDone={() => setStep(2)} />}
        {step === 2 && <StepHarness api={api} snapshot={snapshot} known={targets} refresh={refresh} onTargets={onTargets} onBack={back} onDone={() => setStep(3)} />}
        {step === 3 && <StepServer api={api} snapshot={snapshot} refresh={refresh} onBack={back} onFinished={onFinished} />}
        {onProfile && <ProfileSwitch api={api} snapshot={snapshot} onProfile={onProfile} />}
      </div>
    </main>
  </div>;
}

function Actions({ onBack, busy, children }: { onBack?: () => void; busy: string; children: ReactNode }) {
  return <div className="onboarding-actions">
    {onBack && <button type="button" className="button secondary" disabled={!!busy} onClick={onBack}>{t('Retour')}</button>}
    {children}
  </div>;
}

function TaskStatus({ busy, error }: { busy: string; error: string }) {
  return <>
    <p className="onboarding-status" role="status">{busy && <><span className="spinner" />{t(busy)}</>}</p>
    <div aria-live="assertive">{error && <p className="notice error" role="alert">{t(error)}</p>}</div>
  </>;
}

function StepAccount({ api, snapshot, refresh, onDone }: { api: ClientBridge; snapshot: Snapshot; refresh: () => Promise<Snapshot>; onDone: () => void }) {
  const { identity } = snapshot;
  const task = useTask();
  const [name, setName] = useState(snapshot.profile?.label || '');
  const [phrase, setPhrase] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [restoring, setRestoring] = useState(!!identity.needsRecoveryRestore);
  const [restorePhrase, setRestorePhrase] = useState('');
  const ids = { name: useId(), phrase: useId(), confirmation: useId(), help: useId(), restore: useId() };
  const ready = accountReady(snapshot);
  const exported = identity.exists && identity.backupExported === true && !identity.backupConfirmed;

  async function create(event: FormEvent) {
    event.preventDefault();
    if (task.busy) return;
    const label = name.trim();
    if (!label) { task.setError('Indiquez le nom de votre compte.'); return; }
    if (phrase.length < 12) { task.setError('Choisissez une phrase de sauvegarde d’au moins 12 caractères.'); return; }
    if (phrase !== confirmation) { task.setError('Les deux phrases de sauvegarde doivent être identiques.'); return; }
    const secret = phrase;
    const result = await task.run('Création de votre compte…', async () => {
      if (label !== (snapshot.profile?.label || '')) await api.renameProfile(label);
      if (!snapshot.identity.exists) await api.createIdentity();
      // The native layer asks where to write the encrypted file, then checks it.
      await api.backupIdentity(secret);
      await api.confirmIdentityBackup();
      return refresh();
    });
    if (result.ok) { setPhrase(''); setConfirmation(''); if (accountReady(result.value)) onDone(); }
  }
  async function confirmExported() {
    const result = await task.run('Confirmation de la sauvegarde…', async () => { await api.confirmIdentityBackup(); return refresh(); });
    if (result.ok && accountReady(result.value)) onDone();
  }
  async function restore(event: FormEvent) {
    event.preventDefault();
    if (task.busy || !restorePhrase) return;
    const secret = restorePhrase; setRestorePhrase('');
    const result = await task.run(identity.exists ? 'Vérification de la sauvegarde existante…' : 'Restauration de l’identité locale…', async () => { await api.restoreIdentity(secret); return refresh(); });
    if (result.ok && accountReady(result.value)) onDone();
  }

  if (ready) return <section className="onboarding-step" aria-label={t('Compte')}>
    <div className="onboarding-summary"><Icon name="check" size={18} /><div><p>{t('Votre compte est prêt et sauvegardé.')}</p>{identity.npub && <p className="mono">{identity.npub}</p>}</div></div>
    <Actions busy=""><button type="button" className="button primary" onClick={onDone}>{t('Continuer')}<Icon name="arrow" size={14} /></button></Actions>
  </section>;

  if (exported && !restoring) return <section className="onboarding-step" aria-label={t('Compte')}>
    <p>{t('Le fichier est exporté. Son enregistrement peut être terminé sans refaire l’export.')}</p>
    <TaskStatus busy={task.busy} error={task.error} />
    <Actions busy={task.busy}><button type="button" className="button primary" disabled={!!task.busy} onClick={() => void confirmExported()}>{t('Terminer ma sauvegarde')}</button></Actions>
  </section>;

  if (restoring) return <form className="onboarding-step" aria-label={t('Restaurer un compte')} onSubmit={event => void restore(event)}>
    <p>{t(identity.exists ? 'Choisissez votre fichier et sa phrase pour vérifier la sauvegarde existante et terminer la confirmation. L’identité du fichier doit correspondre à celle de cet appareil.' : 'Sélectionnez votre fichier de sauvegarde chiffrée. Seule la clé active sera conservée sur cet appareil.')}</p>
    <div className="field"><label htmlFor={ids.restore}>{t('Phrase de la sauvegarde existante')}</label><input id={ids.restore} type="password" autoComplete="off" value={restorePhrase} disabled={!!task.busy} onChange={event => setRestorePhrase(event.target.value)} /></div>
    <TaskStatus busy={task.busy} error={task.error} />
    <Actions busy={task.busy}>
      {!identity.needsRecoveryRestore && <button type="button" className="link-button" disabled={!!task.busy} onClick={() => { setRestoring(false); task.setError(''); }}>{t('Créer un nouveau compte')}</button>}
      <button type="submit" className="button primary" disabled={!!task.busy || !restorePhrase}>{t(identity.exists ? 'Vérifier ma sauvegarde existante' : 'Choisir et restaurer la sauvegarde')}</button>
    </Actions>
  </form>;

  return <form className="onboarding-step" aria-label={t('Compte')} onSubmit={event => void create(event)}>
    <div className="field"><label htmlFor={ids.name}>{t('Nom du compte')}</label><input id={ids.name} value={name} maxLength={80} autoComplete="name" disabled={!!task.busy} onChange={event => setName(event.target.value)} /></div>
    <div className="field"><label htmlFor={ids.phrase}>{t('Phrase de sauvegarde')}</label><input id={ids.phrase} type="password" autoComplete="new-password" aria-describedby={ids.help} value={phrase} disabled={!!task.busy} onChange={event => setPhrase(event.target.value)} /><small id={ids.help}>{t('Au moins 12 caractères. Elle chiffre le fichier de sauvegarde que vous allez enregistrer ; gardez-la à part.')}</small></div>
    <div className="field"><label htmlFor={ids.confirmation}>{t('Confirmer la phrase de sauvegarde')}</label><input id={ids.confirmation} type="password" autoComplete="new-password" value={confirmation} disabled={!!task.busy} onChange={event => setConfirmation(event.target.value)} /></div>
    <TaskStatus busy={task.busy} error={task.error} />
    <Actions busy={task.busy}>
      <button type="button" className="link-button" disabled={!!task.busy} onClick={() => { setRestoring(true); task.setError(''); }}>{t('J’ai déjà un compte')}</button>
      <button type="submit" className="button primary" disabled={!!task.busy}>{t('Créer mon compte')}<Icon name="arrow" size={14} /></button>
    </Actions>
  </form>;
}

const harnessName = (target: AgentTarget) => target.id === 'bundled:claude' ? 'Claude' : target.id === 'bundled:codex' ? 'Codex' : target.label;

function StepHarness({ api, snapshot, known, refresh, onTargets, onBack, onDone }: { api: ClientBridge; snapshot: Snapshot; known: AgentTarget[]; refresh: () => Promise<Snapshot>; onTargets?: (targets: AgentTarget[]) => void; onBack?: () => void; onDone: () => void }) {
  const task = useTask();
  const configuredId = snapshot.settings.targetId;
  const manual = snapshot.settings.manualTarget?.id === configuredId ? snapshot.settings.manualTarget : undefined;
  // The configured harness (manual or discovered) stays a choice next to the bundled ones.
  const withConfigured = (list: AgentTarget[], configured = manual || known.find(target => target.id === configuredId)) =>
    configured && !list.some(target => target.id === configured.id) ? [...list, configured] : list;
  const [choices, setChoices] = useState<AgentTarget[]>(() => withConfigured(known.filter(target => target.id.startsWith('bundled:'))));
  const [detecting, setDetecting] = useState(true);
  const [selected, setSelected] = useState(snapshot.settings.targetId);
  const [needsLogin, setNeedsLogin] = useState(false);
  const group = useId();
  useEffect(() => {
    let live = true;
    void (async () => {
      let found: AgentTarget[] = [];
      try { found = (await api.bundledAgents()).filter(target => target.id.startsWith('bundled:')); } catch { /* falls back to detected installations */ }
      // A build without bundled harnesses still offers the installations found on this computer.
      let configured = manual || known.find(target => target.id === configuredId);
      if (!found.length || (configuredId && !configured && !found.some(target => target.id === configuredId))) {
        try {
          const discovered = await api.discoverAgents();
          if (!found.length) found = discovered;
          configured ||= discovered.find(target => target.id === configuredId);
        } catch { /* reported below */ }
      }
      if (!live) return;
      const all = withConfigured(found, configured);
      setChoices(all); setDetecting(false); onTargets?.(all);
      setSelected(previous => previous && all.some(target => target.id === previous) ? previous : all.find(target => target.available)?.id || '');
    })();
    return () => { live = false; };
  }, [api]);
  const target = choices.find(item => item.id === selected);
  const authentication = snapshot.agentStatus?.authentication;

  async function connect(event: FormEvent) {
    event.preventDefault();
    if (task.busy || !target?.available) return;
    const connected = snapshot.agentConnected && snapshot.settings.targetId === target.id;
    if (connected && authentication !== 'none') { onDone(); return; }
    const result = await task.run('Connexion à votre agent…', async () => {
      if (!connected) {
        // A kept manual harness keeps its configuration; a new choice replaces it.
        await api.saveSettings({ ...snapshot.settings, targetId: target.id, manualTarget: manual && manual.id === target.id ? manual : undefined });
        await api.connectAgent(target, snapshot.settings.cwd);
      }
      return refresh();
    });
    if (!result.ok) return;
    if (result.value.agentStatus?.authentication === 'none') { setNeedsLogin(true); return; }
    onDone();
  }
  async function login() {
    const result = await task.run('Identification auprès du fournisseur…', async () => { await api.authenticateAgent(''); return refresh(); });
    if (result.ok && result.value.agentStatus?.authentication !== 'none') { setNeedsLogin(false); onDone(); }
  }

  return <form className="onboarding-step" aria-label={t('Assistant IA')} onSubmit={event => void connect(event)}>
    <fieldset className="harness-choices" disabled={!!task.busy}>
      <legend className="visually-hidden">{t('Choisissez votre assistant IA')}</legend>
      {choices.map(item => <label key={item.id} className={`harness-card ${selected === item.id ? 'selected' : ''} ${item.available ? '' : 'unavailable'}`}>
        <input type="radio" name={group} value={item.id} checked={selected === item.id} disabled={!item.available} onChange={() => { setSelected(item.id); setNeedsLogin(false); task.setError(''); }} />
        <span className="harness-name">{harnessName(item)}</span>
        <span className="harness-detail">{t(!item.available ? 'Non détecté sur cet ordinateur' : item.id === configuredId && !item.id.startsWith('bundled:') ? 'Configuration actuelle' : 'Détecté · prêt à être connecté')}</span>
        {selected === item.id && <span className="harness-check" aria-hidden="true"><Icon name="check" size={16} /></span>}
      </label>)}
    </fieldset>
    {detecting && !choices.length && <p className="onboarding-status" role="status"><span className="spinner" />{t('Recherche des installations…')}</p>}
    {!detecting && !choices.length && <p className="notice error" role="alert">{t('Aucun assistant IA n’a été trouvé. Réinstallez Tutor ou installez Claude ou Codex, puis relancez l’application.')}</p>}
    {needsLogin && <div className="onboarding-callout" role="status">
      <p>{t('Votre harness demande une connexion. Tutor utilise ensuite les accès qu’il gère.')}</p>
      <button type="button" className="button secondary" disabled={!!task.busy} onClick={() => void login()}><Icon name="external" size={14} />{t('Ouvrir la connexion officielle')}</button>
    </div>}
    <TaskStatus busy={task.busy} error={task.error} />
    <Actions onBack={onBack} busy={task.busy}>
      <button type="submit" className="button primary" disabled={!!task.busy || !target?.available}>{t('Continuer')}<Icon name="arrow" size={14} /></button>
    </Actions>
  </form>;
}

function StepServer({ api, snapshot, refresh, onBack, onFinished }: { api: ClientBridge; snapshot: Snapshot; refresh: () => Promise<Snapshot>; onBack?: () => void; onFinished: () => void }) {
  const task = useTask();
  const tutor = snapshot.settings.tutor;
  const [address, setAddress] = useState(tutor.mode === 'remote' ? tutor.url : '');
  const [institution, setInstitution] = useState<Institution | null>(null);
  const [waiting, setWaiting] = useState(false);
  const [copied, setCopied] = useState('');
  const ids = { url: useId(), help: useId(), npub: useId() };
  const connected = serverReady(snapshot) && tutor.mode === 'remote';
  // A ready local server can finish the setup as is: switching to remote is a separate choice.
  const localReady = serverReady(snapshot) && tutor.mode === 'local';
  const [remoteForm, setRemoteForm] = useState(false);

  async function connect(event?: FormEvent) {
    event?.preventDefault();
    if (task.busy) return;
    const normalized = normalizeTutorUrl(address);
    if (!normalized.ok) { task.setError(normalized.error); return; }
    const url = normalized.url;
    setAddress(url); setWaiting(false);
    const found = await task.run('Vérification de l’institution…', () => api.discoverInstitution(url));
    if (!found.ok) { setInstitution(null); return; }
    setInstitution(found.value);
    const result = await task.run('Connexion à Tutor…', async () => {
      const current = await api.loadState();
      await api.saveSettings({ ...current.settings, tutor: { ...current.settings.tutor, mode: 'remote', url, activity: 'auto' } });
      await api.connectTutor(url, false);
      return refresh();
    });
    if (!result.ok) {
      if (awaitingAuthorization(result.raw)) { setWaiting(true); task.setError(''); }
      try { await refresh(); } catch { /* the original error stays visible */ }
    }
  }
  async function finish() {
    const result = await task.run('Enregistrement…', async () => {
      const current = await api.loadState();
      await api.saveSettings({ ...current.settings, setupCompleted: true });
      return refresh();
    });
    if (result.ok) onFinished();
  }
  async function copy() {
    try { await navigator.clipboard.writeText(snapshot.identity.npub || ''); setCopied('Identifiant public copié.'); }
    catch { setCopied('Sélectionnez votre identifiant puis utilisez Ctrl + C.'); }
  }

  if (connected && !waiting) return <section className="onboarding-step" aria-label={t('Serveur Tutor')}>
    <div className="onboarding-summary"><Icon name="check" size={18} /><div><p>{t('Connecté à {name}.', { name: institution?.name || snapshot.tutorStatus.label })}</p><p className="mono">{tutor.url}</p></div></div>
    <TaskStatus busy={task.busy} error={task.error} />
    <Actions onBack={onBack} busy={task.busy}><button type="button" className="button primary" disabled={!!task.busy} onClick={() => void finish()}>{t('Accéder à Tutor')}<Icon name="arrow" size={14} /></button></Actions>
  </section>;

  if (localReady && !remoteForm) return <section className="onboarding-step" aria-label={t('Serveur Tutor')}>
    <div className="onboarding-summary"><Icon name="check" size={18} /><div><p>{t('Le serveur Tutor local est prêt.')}</p>{snapshot.tutorStatus.label && <p>{snapshot.tutorStatus.label}</p>}</div></div>
    <TaskStatus busy={task.busy} error={task.error} />
    <Actions onBack={onBack} busy={task.busy}>
      <button type="button" className="link-button" disabled={!!task.busy} onClick={() => { setRemoteForm(true); task.setError(''); }}>{t('Utiliser plutôt un serveur distant')}</button>
      <button type="button" className="button primary" disabled={!!task.busy} onClick={() => void finish()}>{t('Accéder à Tutor')}<Icon name="arrow" size={14} /></button>
    </Actions>
  </section>;

  return <form className="onboarding-step" aria-label={t('Serveur Tutor')} onSubmit={event => void connect(event)}>
    <div className="field"><label htmlFor={ids.url}>{t('Adresse de votre serveur Tutor')}</label>
      <input id={ids.url} type="text" inputMode="url" autoComplete="url" spellCheck={false} aria-describedby={ids.help} placeholder="https://tutor.votre-institution.fr" value={address} disabled={!!task.busy}
        onChange={event => { setAddress(event.target.value); setInstitution(null); setWaiting(false); task.setError(''); }} />
      <small id={ids.help}>{t('Par exemple https://tutor.exemple.fr. Une adresse se terminant par /mcp est aussi acceptée.')}</small>
    </div>
    {institution && <p className="onboarding-institution"><Icon name="check" size={16} />{t('Institution trouvée : {name}', { name: institution.name })}</p>}
    {waiting && <section className="onboarding-callout" aria-label={t('En attente d’autorisation')} aria-live="polite">
      <h2>{t('En attente d’autorisation')}</h2>
      <p>{t('Votre institution doit autoriser votre identifiant public. Transmettez-le à son administrateur, puis réessayez.')}</p>
      <div className="field"><label htmlFor={ids.npub}>{t('Identifiant public du membre')}</label><input id={ids.npub} readOnly className="mono" value={snapshot.identity.npub || ''} onFocus={event => event.currentTarget.select()} /></div>
      <div className="button-row">
        <button type="button" className="button secondary" onClick={() => void copy()}><Icon name="copy" size={14} />{t('Copier l’identifiant public')}</button>
        <button type="button" className="button secondary" disabled={!!task.busy} onClick={() => void connect()}><Icon name="refresh" size={14} />{t('Réessayer')}</button>
      </div>
      <p className="inline-status" role="status">{t(copied)}</p>
    </section>}
    <TaskStatus busy={task.busy} error={task.error} />
    <Actions onBack={onBack} busy={task.busy}>
      <button type="submit" className="button primary" disabled={!!task.busy || !address.trim()}>{t('Tester et connecter')}<Icon name="arrow" size={14} /></button>
    </Actions>
  </form>;
}

/**
 * Always-available way out of the setup: open another profile or create one.
 * It never bypasses the current profile's backup confirmation.
 */
function ProfileSwitch({ api, snapshot, onProfile }: { api: ClientBridge; snapshot: Snapshot; onProfile: (next: Snapshot) => void }) {
  const task = useTask();
  const others = (snapshot.profiles || []).filter(profile => profile.id !== snapshot.profile?.id);
  const [selected, setSelected] = useState(others[0]?.id || '');
  const [label, setLabel] = useState('');
  const ids = { profile: useId(), label: useId() };
  async function open() {
    if (task.busy || !selected) return;
    const result = await task.run('Changement de compte…', () => api.selectProfile(selected));
    if (result.ok) onProfile(result.value);
  }
  async function create(event: FormEvent) {
    event.preventDefault();
    const name = label.trim();
    if (task.busy) return;
    if (!name) { task.setError('Indiquez le nom de votre compte.'); return; }
    const result = await task.run('Création du compte…', () => api.createProfile(name));
    if (result.ok) { setLabel(''); onProfile(result.value); }
  }
  return <details className="onboarding-profiles">
    <summary>{t('Changer de compte')}</summary>
    {others.length > 0 && <div className="field">
      <label htmlFor={ids.profile}>{t('Autre compte sur cet ordinateur')}</label>
      <div className="button-row">
        <select id={ids.profile} value={selected} disabled={!!task.busy} onChange={event => setSelected(event.target.value)}>
          {others.map(profile => <option key={profile.id} value={profile.id}>{profile.label}</option>)}
        </select>
        <button type="button" className="button secondary" disabled={!!task.busy || !selected} onClick={() => void open()}>{t('Ouvrir ce compte')}</button>
      </div>
    </div>}
    <form onSubmit={event => void create(event)}>
      <div className="field">
        <label htmlFor={ids.label}>{t('Nom du nouveau compte')}</label>
        <div className="button-row">
          <input id={ids.label} value={label} maxLength={80} disabled={!!task.busy} onChange={event => setLabel(event.target.value)} />
          <button type="submit" className="button secondary" disabled={!!task.busy}>{t('Créer ce compte')}</button>
        </div>
      </div>
    </form>
    <TaskStatus busy={task.busy} error={task.error} />
  </details>;
}
