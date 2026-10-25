import { useEffect, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { bridge } from './bridge';
import type { ClientBridge } from './bridge';
import { Icon } from './icons';
import { IdentityPanel } from './IdentityPanel';
import { MessageBody } from './MessageBody';
import { AccountMenu } from './AccountMenu';
import { FormationReviewPanel, NewFormation } from './FormationReviewPanel';
import { authoredFormationViews, availableFormationViews, blankIntent, chunkFromEvent, displayDate, errorMessage, formationViews, permissionFromEvent, permissionLabel, record, statusLabel, text, toolFromEvent, validateIntent } from './model';
import type { FormationView } from './model';
import type { AgentConnection, AgentTarget, Conversation, FormationReview, FormationVersion, Intent, Institution, PermissionRequest, Settings, Snapshot, ToolActivity, TutorOverview } from './types';

type Page = 'today' | 'formations' | 'session' | 'connections';
const pageLabels: Record<Page, string> = { today: 'Aujourd’hui', formations: 'Mes formations', session: 'Ma séance', connections: 'Connexions' };
const emptyState: Snapshot = {
  settings: { targetId: '', cwd: '', tutor: { mode: 'remote', url: '', command: 'tutor-mcp', args: ['--local'] } },
  identity: { exists: false, backupConfirmed: false, needsBackup: false },
  intents: [], conversations: [], tutorStatus: { connected: false, label: 'Tutor non connecté' }, agentConnected: false,
};

function newestIntent(intents: Intent[]): Intent | undefined {
  return [...intents].sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))[0];
}

export default function App({ api = bridge }: { api?: ClientBridge }) {
  const [snapshot, setSnapshot] = useState<Snapshot>(emptyState);
  const snapshotRef = useRef(snapshot);
  snapshotRef.current = snapshot;
  const [page, setPage] = useState<Page>('today');
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [targets, setTargets] = useState<AgentTarget[]>([]);
  const [agentConnection, setAgentConnection] = useState<AgentConnection | null>(null);
  const [draft, setDraft] = useState<Intent>(blankIntent);
  const [intentEditing, setIntentEditing] = useState(true);
  const [selectedIntentId, setSelectedIntentId] = useState<string | undefined>();
  const [overview, setOverview] = useState<TutorOverview | null>(null);
  const [overviewError, setOverviewError] = useState('');
  const [permissions, setPermissions] = useState<PermissionRequest[]>([]);
  const [tools, setTools] = useState<ToolActivity[]>([]);
  const [pendingConversationId, setPendingConversationId] = useState<string | null>(null);
  const pendingRef = useRef<string | null>(null);
  const [streamText, setStreamText] = useState('');
  const [promptDraft, setPromptDraft] = useState('');
  const [activeConversationId, setActiveConversationId] = useState<string | undefined>();
  const [readyConversationId, setReadyConversationId] = useState<string | undefined>();
  const [permissionBusy, setPermissionBusy] = useState(false);
  const [contextOpen, setContextOpen] = useState(false);
  const contextButton = useRef<HTMLButtonElement>(null);
  const contextPanel=useRef<HTMLElement>(null);
  useEffect(()=>{if(contextOpen)contextPanel.current?.querySelector<HTMLButtonElement>('button')?.focus();},[contextOpen]);
  const [newFormationOpen, setNewFormationOpen] = useState(false);
  const [reviewFormationId, setReviewFormationId] = useState<string>();

  const conversation = snapshot.conversations.find((item) => item.id === (activeConversationId || snapshot.activeConversationId));
  const currentIntent = page === 'session'
    ? snapshot.intents.find((item) => item.id === conversation?.intentId)
    : snapshot.intents.find((item) => item.id === selectedIntentId) || newestIntent(snapshot.intents);
  const lastConversation = [...snapshot.conversations].sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))[0];
  const formations = formationViews(overview);
  const availableFormations = availableFormationViews(overview);
  const authoredFormations = authoredFormationViews(overview);
  const staff = !!overview?.identity && (overview.capabilities?.learning === false || !overview.identity.learnerId);
  const canAuthor = overview?.capabilities?.formationAuthoring === true;
  const canRequestAuthoring = canAuthor || overview?.capabilities?.canRequestAuthoring === true || overview?.session?.allowedScopes.includes('formation:write') === true;
  const authoringGrant = overview?.session?.scope.split(' ').includes('formation:write') === true;
  const learningReady = snapshot.agentConnected && snapshot.tutorStatus.connected;
  const conversationReady = learningReady && !!conversation && readyConversationId === conversation.id;

  useEffect(() => {
    let live = true;
    api.bundledAgents().then((found) => {if(live) setTargets(found);}).catch(() => {});
    const unlisten: (() => void)[] = [];
    const register = (promise: Promise<() => void>) => promise.then((dispose) => { if (live) unlisten.push(dispose); else dispose(); }).catch(() => {});
    api.loadState().then((state) => {
      if (!live) return;
      setSnapshot(state);
      setActiveConversationId(state.activeConversationId);
      const intent = newestIntent(state.intents);
      if (intent) { setDraft(intent); setSelectedIntentId(intent.id); setIntentEditing(!intent.validated); }
    }).catch((reason) => { if (live) setError(errorMessage(reason)); }).finally(() => { if (live) setLoading(false); });
    register(api.listenState((state) => {
      if (!live || (state.profileEpoch || 0) < (snapshotRef.current.profileEpoch || 0)) return;
      if (state.profile?.id && snapshotRef.current.profile?.id && state.profile.id !== snapshotRef.current.profile.id) displayProfile(state);
      else setSnapshot(state);
    }));
    register(api.listenTutor((status) => { if (live) setSnapshot((state) => ({ ...state, tutorStatus: status })); }));
    register(api.listenAgent((event) => {
      if (typeof event.profileEpoch === 'number' && event.profileEpoch < (snapshotRef.current.profileEpoch || 0)) return;
      if (!live) return;
      if (event.type === 'connection' && (event.status === 'closed' || event.status === 'error')) {
        setSnapshot((state) => ({ ...state, agentConnected: false }));
        setAgentConnection(null);
        setReadyConversationId(undefined);
        setPermissions([]);
        if (text(event.message)) {
          if (event.status === 'error') setError(text(event.message));
          else setNotice(text(event.message));
        }
        return;
      }
      const permission = permissionFromEvent(event);
      if (permission) {
        setPage('session');
        setPermissions((existing) => existing.some((item) => JSON.stringify(item.id) === JSON.stringify(permission.id)) ? existing : [...existing, permission]);
        return;
      }
      if (!pendingRef.current) return; // Session replay is reconstructed from the authoritative snapshot.
      const pendingConversation = snapshotRef.current.conversations.find((item) => item.id === pendingRef.current);
      const eventSessionId = text(record(event.params).sessionId);
      if (pendingConversation?.agentSessionId && eventSessionId && eventSessionId !== pendingConversation.agentSessionId) return;
      const delta = chunkFromEvent(event);
      if (delta) setStreamText((previous) => previous + delta);
      const tool = toolFromEvent(event);
      if (tool) setTools((previous) => {
        const existing = previous.find((item) => item.id === tool.id);
        return existing ? previous.map((item) => item.id === tool.id ? { ...item, title: tool.title || item.title, status: tool.status || item.status } : item) : [...previous, { ...tool, title: tool.title || 'Opération du tuteur' }];
      });
    }));
    return () => { live = false; unlisten.forEach((dispose) => dispose()); };
  }, [api]);

  useEffect(() => {
    if (!snapshot.tutorStatus.connected) { setOverview(null); setOverviewError(''); return; }
    let live = true;
    api.tutorOverview().then((value) => { if (live) { setOverview(value); setOverviewError(''); } }).catch((reason) => { if (live) setOverviewError(errorMessage(reason)); });
    return () => { live = false; };
  }, [api, snapshot.tutorStatus.connected, snapshot.profile?.id, snapshot.settings.tutor.activity]);

  async function perform<T>(label: string, operation: () => Promise<T>): Promise<T | undefined> {
    setBusy(label); setError(''); setNotice('');
    try { return await operation(); }
    catch (reason) { setError(errorMessage(reason)); return undefined; }
    finally { setBusy(null); }
  }

  async function refreshOverview() {
    const profileId = snapshotRef.current.profile?.id;
    setOverviewError('');
    try { const next = await api.tutorOverview(); if (snapshotRef.current.profile?.id === profileId) setOverview(next); }
    catch (reason) { if (snapshotRef.current.profile?.id === profileId) setOverviewError(errorMessage(reason)); }
  }

  async function discover() {
    const found = await perform('Recherche des installations…', () => api.discoverAgents());
    if (found) { setTargets(found); if (!found.length) setNotice('Aucune installation détectée. Vous pouvez préciser le chemin de l’adaptateur dans les réglages avancés.'); }
  }

  async function saveSettings(settings: Settings) {
    return perform('Enregistrement des connexions…', async () => {
      const state = await api.saveSettings(settings);
      setSnapshot(state);
      setNotice('Configuration enregistrée.');
      return state;
    });
  }

  async function connect(target: AgentTarget, settings: Settings) {
    await perform('Connexion à votre agent…', async () => {
      setSnapshot(await api.saveSettings(settings));
      const connection = await api.connectAgent(target, settings.cwd);
      setAgentConnection(connection);
      setReadyConversationId(undefined);
      setSnapshot(await api.loadState());
      setNotice('Connexion ACP établie. Si votre agent demande une identification, choisissez sa méthode de connexion ci-dessous.');
    });
  }

  async function authenticate(methodId: string) {
    await perform('Identification auprès du fournisseur…', async () => {
      await api.authenticateAgent(methodId);
      setSnapshot(await api.loadState());
      setNotice('Identification auprès du fournisseur terminée.');
    });
  }

  async function disconnectAgent() {
    await perform('Déconnexion de votre agent…', async () => {
      await api.disconnectAgent(); setAgentConnection(null); setPermissions([]); setReadyConversationId(undefined);
      setSnapshot(await api.loadState());
    });
  }

  async function connectTutor(settings: Settings, authoring: boolean) {
    await perform('Connexion signée à Tutor…', async () => {
      setSnapshot(await api.saveSettings(settings));
      if (settings.tutor.mode === 'remote') {
        setSnapshot(await api.connectTutor(settings.tutor.url, authoring));
        setNotice('Identité vérifiée par l’institution. Connectez votre agent pour commencer une séance.');
      } else {
        setOverview(await api.tutorOverview());
        setSnapshot(await api.loadState());
        setNotice('Tutor local répond.');
      }
      await refreshOverview();
    });
  }
  async function identityAction(label: string, operation: () => Promise<unknown>): Promise<boolean> {
    return (await perform(label, async () => { await operation(); setSnapshot(await api.loadState()); return true; })) === true;
  }

  async function disconnectTutor() {
    await perform('Déconnexion de Tutor…', async () => { setSnapshot(await api.disconnectTutor()); setOverview(null); });
  }

  function displayProfile(state: Snapshot) {
    const intent = newestIntent(state.intents);
    setSnapshot(state); setOverview(null); setOverviewError(''); setAgentConnection(null); setTargets((items)=>items.filter((item)=>item.id.startsWith('bundled:')));
    setActiveConversationId(state.activeConversationId); setReadyConversationId(undefined); setSelectedIntentId(intent?.id);
    setDraft(intent || blankIntent()); setIntentEditing(!intent?.validated);
    setPendingConversationId(null); pendingRef.current = null; setPermissions([]); setTools([]); setStreamText(''); setPromptDraft('');
    setContextOpen(false); setPage('connections');
    setNotice(state.profileSwitchWarning || '');
    setNewFormationOpen(false); setReviewFormationId(undefined);
    setNotice(state.profileSwitchWarning || 'Compte actif changé. Les conversations et les clés de chaque compte restent séparées.');
  }

  async function selectProfile(id: string) {
    await perform('Changement de compte…', async () => displayProfile(await api.selectProfile(id)));
  }

  async function createProfile(label: string) {
    await perform('Création du compte et de ses clés…', async () => {
      displayProfile(await api.createProfile(label));
      await api.createIdentity(); setSnapshot(await api.loadState());
      setNotice('Votre nouveau compte et ses clés sont créés. Copiez votre identifiant public ou protégez votre compte avec une sauvegarde.');
    });
  }

  async function renameProfile(label: string) {
    await perform('Enregistrement du nom du compte…', async () => { setSnapshot(await api.renameProfile(label)); setNotice('Nom du compte enregistré.'); });
  }

  async function activateActivity(activity: 'author' | 'learn' | 'train') {
    const previous = snapshotRef.current;
    if (previous.settings.tutor.mode !== 'remote') throw new Error('Cette activité appartient à une institution distante.');
    const settings: Settings = { ...previous.settings, tutor: { ...previous.settings.tutor, activity } };
    const selected = settings.manualTarget || targets.find((target) => target.id === settings.targetId);
    setSnapshot(await api.saveSettings(settings));
    setSnapshot(await api.connectTutor(settings.tutor.url, activity === 'author'));
    setAgentConnection(null); setReadyConversationId(undefined);
    if (previous.agentConnected && selected) { setAgentConnection(await api.connectAgent(selected, settings.cwd)); setSnapshot(await api.loadState()); }
    await refreshOverview();
  }

  async function ensureAuthoring() {
    if (!snapshotRef.current.tutorStatus.connected) throw new Error('Connectez votre compte à l’institution avant de créer une formation.');
    if (!authoringGrant) await activateActivity('author');
  }

  async function changeActivity(activity: 'author' | 'learn' | 'train') {
    await perform('Changement d’activité…', async () => { await activateActivity(activity); setPage('formations'); setReviewFormationId(undefined); setNotice('Votre activité est active. Les droits sont ceux accordés par l’institution.'); });
  }

  async function enableLearning() {
    await perform('Activation de votre espace apprenant…', async () => { setSnapshot(await api.enableLearning()); setAgentConnection(null); setReadyConversationId(undefined); await refreshOverview(); setPage('formations'); setNotice('Votre espace apprenant est activé. Choisissez une formation dans le catalogue ; aucune inscription n’a été créée.'); });
  }

  async function createInstitutionFormation(name: string, description: string, key: string) {
    await perform('Création du brouillon institutionnel…', async () => {
      await ensureAuthoring();
      const created = await api.createFormation(name, description, key);
      setSnapshot(await api.loadState()); await refreshOverview();
      setNewFormationOpen(false); setReviewFormationId(created.formation.id);
      setNotice('Le brouillon est créé. Examinez son programme ou construisez-le avec votre agent.');
    });
  }

  async function publishInstitutionFormation(versionId: string, key: string): Promise<boolean> {
    return (await perform('Publication dans le catalogue de l’institution…', async () => {
      await ensureAuthoring(); await api.publishFormation(versionId, key); await refreshOverview();
      setNotice('La version est publiée et proposée aux membres de l’institution. Aucune inscription n’a été créée.');
      return true;
    })) === true;
  }

  async function cloneInstitutionFormation(versionId: string, key: string): Promise<FormationVersion | undefined> {
    return await perform('Préparation d’un nouveau brouillon…', async () => {
      await ensureAuthoring(); const result = await api.cloneFormation(versionId, key); await refreshOverview(); return result.version;
    });
  }

  async function discussInstitutionFormation(review: FormationReview) {
    const authorized = await perform('Préparation de la conception…', async () => { await ensureAuthoring(); return true; });
    if (!authorized) return;
    const { formation, version } = review.detail;
    await startLearning(undefined, undefined, `Examine la formation institutionnelle « ${formation.name} » et sa version ${version.version} avec les outils MCP autorisés. formation_id=${formation.id}, version_id=${version.id}. Ces deux identifiants viennent du serveur ; un identifiant d’intention locale ne désigne pas une version. Lis cette version avec review_formation_version ou get_formation_version et présente son programme réel. Propose les prochaines modifications. Ne publie aucune version et ne crée aucune inscription sans ma confirmation explicite. Si la version est publiée, propose une nouvelle version avant de modifier son contenu. Mon rôle d’équipe ne requiert pas de profil apprenant.`);
  }

  async function saveIntention(event: FormEvent) {
    event.preventDefault();
    const validation = validateIntent(draft);
    if (validation) { setError(validation); return; }
    await perform('Enregistrement de votre intention…', async () => {
      const state = await api.saveIntent(draft);
      const saved = state.intents.find((item) => draft.id && item.id === draft.id) || newestIntent(state.intents);
      setSnapshot(state);
      if (saved) { setDraft(saved); setSelectedIntentId(saved.id); setIntentEditing(false); }
      setNotice('Votre intention est enregistrée sur cet appareil. Vous pourrez la modifier avant de poursuivre la conception.');
    });
  }

  async function sendText(id: string, value: string) {
    const body = value.trim();
    if (!body || pendingRef.current) return;
    pendingRef.current = id;
    setPendingConversationId(id); setStreamText(''); setTools([]); setError(''); setNotice('');
    const optimisticId = crypto.randomUUID();
    setSnapshot((state) => ({ ...state, conversations: state.conversations.map((item) => item.id === id ? { ...item, messages: [...item.messages, { id: optimisticId, role: 'user', text: body, status: 'complete', createdAt: new Date().toISOString() }] } : item) }));
    try {
      const result = await api.sendPrompt(id, body);
      if (result.stopReason === 'cancelled') setNotice('Séance interrompue. Les éléments déjà enregistrés restent disponibles.');
      else if (result.stopReason !== 'end_turn') setNotice(`Le tuteur a terminé ce tour (${result.stopReason}). Vous pouvez poursuivre la séance.`);
    } catch (reason) { setError(errorMessage(reason)); }
    finally {
      try { setSnapshot(await api.loadState()); setStreamText(''); } catch (reason) { setError(errorMessage(reason)); }
      setPermissions([]); pendingRef.current = null; setPendingConversationId(null);
      if (snapshotRef.current.tutorStatus.connected) void refreshOverview();
    }
  }

  async function startLearning(intent?: Intent, resume?: Conversation, initialPrompt?: string) {
    if (pendingRef.current) return;
    if (!learningReady) { setPage('connections'); setNotice('Connectez votre agent et Tutor pour commencer ou reprendre votre séance.'); return; }
    const started = await perform(resume ? 'Reprise de votre séance…' : 'Préparation de votre séance…', async () => {
      const result = await api.startConversation(intent?.id, resume?.id);
      const state = await api.loadState();
      setSnapshot(state);
      setActiveConversationId(result.id);
      setReadyConversationId(result.id);
      if (result.intentId) setSelectedIntentId(result.intentId);
      setPage('session'); setTools([]); setStreamText('');
      return result;
    });
    if (started && initialPrompt) await sendText(started.id, initialPrompt);
  }

  async function cancel() {
    if (!pendingRef.current) return;
    const id = pendingRef.current;
    try { await api.cancelPrompt(id); setNotice('Annulation demandée au tuteur…'); }
    catch (reason) { setError(errorMessage(reason)); }
  }

  async function answerPermission(permission: PermissionRequest, optionId: string | null) {
    setPermissionBusy(true); setError('');
    try {
      await api.answerPermission(permission.id, optionId);
      setPermissions((items) => items.filter((item) => JSON.stringify(item.id) !== JSON.stringify(permission.id)));
    } catch (reason) { setError(errorMessage(reason)); }
    finally { setPermissionBusy(false); }
  }

  function editIntent(intent?: Intent) {
    setDraft(intent ? { ...intent } : blankIntent()); setSelectedIntentId(intent?.id); setIntentEditing(true); setPage('today'); setNotice('');
  }

  function openFormation(formation: FormationView) {
    if (formation.kind === 'authored' && formation.formationId) { setReviewFormationId(formation.formationId); return; }
    const prompt = formation.kind === 'authored'
      ? `Examine la formation institutionnelle « ${formation.name} » (formation_id ${formation.formationId}) et ses versions avec les outils MCP autorisés. Présente son état réel et propose les prochaines modifications. Ne publie aucune version et ne crée aucune inscription sans ma confirmation explicite. Mon rôle d’équipe ne requiert pas de profil apprenant.`
      : formation.domainId
      ? `Je souhaite reprendre ma formation « ${formation.name} » (domaine ${formation.domainId}). Consulte mon état pédagogique enregistré et propose la prochaine activité adaptée, sans modifier mes critères validés.`
      : `Présente-moi la formation « ${formation.name} » (cohorte ${formation.cohortId || ''}) et ses conditions d’accès. Attends ma confirmation explicite avant toute inscription.`;
    void startLearning(undefined, undefined, prompt);
  }

  if (loading) return <div className="loading-screen"><div className="button-row"><span className="spinner" />Ouverture de votre espace…</div></div>;

  return <div className="app-shell">
    <a className="skip-link" href="#main-content">Aller au contenu principal</a>
    <aside className="sidebar">
      <div className="brand"><span className="brand-symbol">t</span><span>tutor</span></div>
      <p className="sidebar-caption">Votre espace</p>
      <nav className="navigation" aria-label="Navigation principale">
        {(['today', 'formations', 'session', 'connections'] as Page[]).map((item) => <button key={item} aria-label={pageLabels[item]} className={`nav-item ${page === item ? 'active' : ''}`} aria-current={page === item ? 'page' : undefined} onClick={() => setPage(item)}>
          <Icon name={item === 'today' ? 'sun' : item === 'formations' ? 'book' : item === 'session' ? 'chat' : 'plug'} /><span>{pageLabels[item]}</span>
          {item === 'formations' && formations.length > 0 && <span className="nav-count">{formations.length}</span>}
          {item === 'session' && pendingConversationId && <span className="dot" aria-label="Réponse en cours" />}
        </button>)}
      </nav>
      <div className="sidebar-bottom">
        <p className="sidebar-note">Votre curiosité donne le cap.<br />Votre tuteur vous accompagne.</p>
        <div className="sidebar-footer"><span className="dot" />Un apprentissage à votre rythme</div>
      </div>
    </aside>
    <div className="main-shell">
      <header className="topbar">
        <div className="breadcrumb"><span>Mon espace</span><Icon name="chevron" size={12} /><strong>{pageLabels[page]}</strong></div>
        <AccountMenu key={snapshot.profile?.id || 'principal'} snapshot={snapshot} overview={overview} busy={!!busy || !!pendingConversationId} onSelect={(id) => void selectProfile(id)} onCreate={(label) => void createProfile(label)} onRename={(label) => void renameProfile(label)} onActivity={(activity) => void changeActivity(activity)} onEnableLearning={() => void enableLearning()} />
        <div className="topbar-status" aria-label="État des connexions">
          <button className={`status-pill ${snapshot.tutorStatus.connected ? 'connected' : ''}`} onClick={() => setPage('connections')}><span className="dot" />{snapshot.tutorStatus.connected ? snapshot.tutorStatus.label || 'Tutor connecté' : 'Tutor non connecté'}</button>
          <button className={`status-pill ${snapshot.agentConnected ? 'connected' : ''}`} onClick={() => setPage('connections')}><span className="dot" />{snapshot.agentConnected ? `${snapshot.settings.targetId==='bundled:claude'?'Claude':'Codex'} connecté` : 'Agent hors ligne'}</button>
        </div>
      </header>
      <main className={`content ${page === 'session' ? 'session-content' : ''}`} id="main-content" tabIndex={-1}>
        {error && <Notice kind="error" onDismiss={() => setError('')}>{error}</Notice>}
        {notice && <Notice kind="info" onDismiss={() => setNotice('')}>{notice}</Notice>}
        {busy && <div className="notice" role="status"><span className="spinner" /><p>{busy}</p></div>}

        {page === 'today' && <>
          <PageHeading eyebrow="À votre rythme" title="Une curiosité. Un nouveau départ." description="Exprimez ce que vous aimeriez comprendre ou savoir faire. Construisez ensuite un parcours avec votre tuteur." />
          {!intentEditing && currentIntent ? <section className="card intent-card" aria-labelledby="validated-intent-title">
            <div className="formation-meta"><h2 className="card-title" id="validated-intent-title"><Icon name="spark" />Votre intention</h2><span className="tag green"><Icon name="check" size={11} />Validée · révision {currentIntent.revision}</span></div>
            <p className="validated-intent-text">{currentIntent.title}</p>
            {currentIntent.goals && <p><strong>Ce que vous visez :</strong> {currentIntent.goals}</p>}
            {currentIntent.constraints && <p><strong>Votre cadre :</strong> {currentIntent.constraints}</p>}
            <div className="intent-footer"><button className="button secondary" onClick={() => editIntent(currentIntent)}>Modifier mon intention</button><button className="button primary" disabled={!!busy || !!pendingConversationId} onClick={() => void startLearning(currentIntent, undefined, 'Aide-moi à concevoir une formation à partir de mon intention validée. Commence par reformuler mon objectif et proposer des étapes de conception que je pourrai examiner et modifier avant de les valider.')}><Icon name="spark" size={15} />Concevoir ma formation<Icon name="arrow" size={15} /></button></div>
            {!learningReady && <p className="subtle-note">La conception commencera après la connexion de votre agent et Tutor.</p>}
          </section> : <IntentEditor draft={draft} setDraft={setDraft} onSubmit={(event) => void saveIntention(event)} busy={!!busy} />}

          <div className="guide-grid" aria-label="Les étapes de votre apprentissage">
            <GuideStep number="01" title="Vous choisissez le cap" description="Vos objectifs, vos contraintes et votre manière d’apprendre." />
            <GuideStep number="02" title="Vous concevez ensemble" description="Un parcours proposé par l’IA, que vous pouvez ajuster et valider." />
            <GuideStep number="03" title="Vous avancez et reprenez" description="Des séances guidées et un suivi qui retrouve votre progression." />
          </div>

          <div className="section-heading"><h2>Retrouver le fil</h2><span className="section-caption">Votre espace reste à portée de main</span></div>
          <div className="two-columns">
            {lastConversation ? <section className="card">
              <h3 className="card-title"><Icon name="chat" size={18} />Votre dernière séance</h3>
              <p>{lastConversation.title}</p>
              <p className="subtle-note">{displayDate(lastConversation.updatedAt)} · {lastConversation.messages.length} messages conservés sur cet appareil</p>
              <button className="link-button" disabled={!!busy || !!pendingConversationId} onClick={() => void startLearning(undefined, lastConversation)}>Reprendre ma séance<Icon name="arrow" size={14} /></button>
            </section> : <EmptyState icon="chat" title="Votre première séance vous attend" description="Une fois votre intention définie, votre tuteur vous aide à transformer votre curiosité en parcours." />}
            {formations.length ? <section className="card">
              <h3 className="card-title"><Icon name="book" size={18} />Vos parcours</h3><p>{formations.length} formation{formations.length > 1 ? 's' : ''} retrouvée{formations.length > 1 ? 's' : ''} dans Tutor.</p>
              <button className="link-button" onClick={() => setPage('formations')}>Voir mes formations<Icon name="arrow" size={14} /></button>
            </section> : <EmptyState icon="book" title="Un espace pour vos apprentissages" description={snapshot.tutorStatus.connected ? 'Les parcours enregistrés dans Tutor apparaîtront ici.' : 'Connectez Tutor pour retrouver vos formations et votre progression.'} action={<button className="link-button" onClick={() => setPage(snapshot.tutorStatus.connected ? 'formations' : 'connections')}>{snapshot.tutorStatus.connected ? 'Voir mes formations' : 'Configurer mes connexions'}<Icon name="arrow" size={14} /></button>} />}
          </div>
        </>}

        {page === 'formations' && <>
          <div className="page-heading-row"><PageHeading eyebrow="Votre bibliothèque" title="Des parcours qui vous ressemblent." description="Retrouvez les formations enregistrées dans Tutor et les intentions préparées sur cet appareil." /><button className="button secondary" onClick={() => editIntent()}><Icon name="plus" size={15} />Nouvelle intention</button></div>
          {overviewError && <Notice kind="error">{overviewError}</Notice>}
          {newFormationOpen && <NewFormation busy={!!busy} onCreate={(name, description, key) => void createInstitutionFormation(name, description, key)} onCancel={() => setNewFormationOpen(false)} />}
          {reviewFormationId && <FormationReviewPanel key={`${snapshot.profile?.id || 'principal'}:${reviewFormationId}`} formationId={reviewFormationId} api={api} canPublish={canRequestAuthoring} busy={!!busy || !!pendingConversationId} onClose={() => setReviewFormationId(undefined)} onDiscuss={(review) => void discussInstitutionFormation(review)} onPublish={publishInstitutionFormation} onClone={cloneInstitutionFormation} />}
          {staff && !overview?.capabilities?.training && <section className="card authoring-panel" aria-label="Création de formations"><div className="formation-meta"><span className="tag purple">Équipe de l’institution</span></div><h2>Les formations de votre institution</h2><p>{canRequestAuthoring ? 'Créez un brouillon, construisez son programme avec votre agent, puis examinez la version et décidez de sa publication pour les membres.' : 'Votre rôle permet de consulter les formations qui vous sont accessibles.'}</p><button className="button primary" disabled={!canRequestAuthoring || !!busy || !!pendingConversationId} onClick={() => setNewFormationOpen(true)}>Créer une formation<Icon name="plus" size={14} /></button><p className="subtle-note">Votre rôle d’équipe ne crée pas de profil apprenant. Les inscriptions restent volontaires.</p></section>}
          {overview?.capabilities?.training && <section className="card" aria-label="Mes groupes de formation"><h2>Mes groupes de formation</h2>{overview.trainerCohorts?.items.length ? overview.trainerCohorts.items.map((cohort) => <div key={cohort.cohort_id}><h3>{cohort.cohort_name}</h3><p>{cohort.formation_name}</p><button className="button secondary" disabled={!!busy || !!pendingConversationId} onClick={() => void startLearning(undefined, undefined, `Consulte le groupe qui m’est affecté : cohort_id=${cohort.cohort_id}, formation_id=${cohort.formation_id}. Présente les progrès réels avec les outils autorisés. Ne crée aucun profil apprenant ni inscription.`)}>Examiner le suivi</button></div>) : <p>Aucun groupe ne vous est actuellement affecté. Un responsable de l’institution peut vous affecter comme formateur.</p>}</section>}
          <div className="section-heading"><h2>Mes formations</h2><button className="link-button" disabled={!snapshot.tutorStatus.connected || !!busy} onClick={() => void refreshOverview()}><Icon name="refresh" size={13} />Actualiser</button></div>
          {formations.length ? <div className="formation-grid">{formations.map((formation) => <FormationCard key={formation.id} formation={formation} onOpen={() => openFormation(formation)} disabled={!!busy || !!pendingConversationId} />)}</div> : <EmptyState icon="book" title={snapshot.tutorStatus.connected ? 'Aucune formation enregistrée' : 'Retrouvez vos formations'} description={snapshot.tutorStatus.connected ? 'Commencez par une intention personnelle ou explorez les formations accessibles dans votre institution.' : 'Connectez Tutor pour lire vos parcours. Les données de votre institution restent la référence.'} action={!snapshot.tutorStatus.connected ? <button className="button secondary" onClick={() => setPage('connections')}>Connecter Tutor<Icon name="arrow" size={14} /></button> : undefined} />}
          {!!authoredFormations.length && <><div className="section-heading"><h2>Formations de l’institution</h2><span className="section-caption">Brouillons et publications autorisés</span></div><div className="formation-grid">{authoredFormations.map((formation) => <FormationCard key={formation.id} formation={formation} onOpen={() => openFormation(formation)} disabled={!!busy || !!pendingConversationId} />)}</div></>}
          {!!snapshot.intents.length && <><div className="section-heading"><h2>Mes intentions</h2><span className="section-caption">Conservées sur cet appareil</span></div><div className="formation-grid">{snapshot.intents.map((intent) => <section className="card formation-card" key={intent.id}><div className="formation-meta"><span className="tag purple">Intention personnelle</span><span className="section-caption">Révision {intent.revision}</span></div><h3>{intent.title}</h3>{intent.goals && <p>{intent.goals}</p>}<div className="button-row"><button className="button secondary small" onClick={() => editIntent(intent)}>Examiner et modifier</button><button className="button primary small" disabled={!!busy || !!pendingConversationId || !intent.validated} onClick={() => void startLearning(intent, undefined, 'Retrouvons mon intention validée et l’état enregistré dans Tutor. Aide-moi à poursuivre la conception ou l’apprentissage, en distinguant les étapes déjà confirmées des propositions à valider.')}>Poursuivre<Icon name="arrow" size={13} /></button></div></section>)}</div></>}
          {!!availableFormations.length && <><div className="section-heading"><h2>Formations accessibles</h2><span className="section-caption">Catalogue de votre institution</span></div><div className="formation-grid">{availableFormations.map((formation) => <FormationCard key={formation.id} formation={formation} onOpen={() => openFormation(formation)} disabled={!!busy || !!pendingConversationId} catalog />)}</div></>}
          <p className="subtle-note">Les inscriptions, les versions et la progression proviennent de Tutor. Une intention enregistrée ici devient un parcours lorsque sa conception est effectivement enregistrée dans le moteur.</p>
        </>}

        {page === 'session' && <>
          <h1 className="visually-hidden">Votre séance, votre rythme.</h1>
          {!!snapshot.conversations.length && <div className="session-picker" aria-label="Séances enregistrées">{snapshot.conversations.map((item) => <button key={item.id} className="session-chip" aria-pressed={conversation?.id === item.id} disabled={!!pendingConversationId || !!busy} onClick={() => { setActiveConversationId(item.id); setSelectedIntentId(item.intentId); setTools([]); setStreamText(''); }}>{item.title || 'Séance'} · {displayDate(item.updatedAt)}</button>)}</div>}
          <div className="session-layout" style={{ marginTop: 20 }}>
            <section className="conversation-panel" aria-label="Conversation avec votre tuteur">
              <div className="conversation-heading"><div><h2>{conversation?.title || 'Votre tuteur'}</h2><p>{pendingConversationId ? 'Votre agent prépare sa réponse…' : conversation ? 'Échanges conservés dans ce compte' : 'Une nouvelle séance commence par votre intention'}</p></div><button ref={contextButton} className="button secondary" aria-controls="session-context" aria-expanded={contextOpen} onClick={() => setContextOpen(!contextOpen)}>Repères de la séance</button></div>
              {conversation && !conversationReady && !pendingConversationId && <button className="button secondary resume-session" disabled={!!busy} onClick={() => void startLearning(undefined, conversation)}>Reprendre cette séance</button>}
              <ConversationMessages conversation={conversation} streamText={streamText} streaming={!!pendingConversationId} openLink={api.openExternalLink} />
              {permissions.map((permission, index) => <PermissionCard key={JSON.stringify(permission.id)} permission={permission} disabled={permissionBusy || index > 0} onAnswer={(optionId) => void answerPermission(permission, optionId)} />)}
              {pendingConversationId && <div className="session-strip" role="status"><span className="spinner" />{permissions.length ? 'Votre autorisation est attendue.' : tools.some((tool) => tool.status === 'in_progress') ? 'Le tuteur utilise les outils autorisés.' : 'Réponse en cours…'}</div>}
              <form className="composer" onSubmit={(event) => { event.preventDefault(); if (conversation && promptDraft.trim()) { const value = promptDraft; setPromptDraft(''); void sendText(conversation.id, value); } }}>
                <label className="visually-hidden" htmlFor="prompt">Votre message au tuteur</label>
                <textarea id="prompt" value={promptDraft} onChange={(event) => setPromptDraft(event.target.value)} disabled={!conversationReady || !!pendingConversationId} placeholder={conversationReady ? 'Posez une question, proposez un ajustement, partagez votre réponse…' : 'Commencez ou reprenez une séance pour échanger avec votre tuteur.'} onKeyDown={(event) => { if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); } }} />
                <div className="composer-footer"><span className="composer-hint">Ctrl + Entrée pour envoyer · L’IA peut se tromper.</span>{pendingConversationId ? <button type="button" className="button secondary" onClick={() => void cancel()}><Icon name="pause" size={14} />Interrompre</button> : <button className="button primary" disabled={!conversationReady || !promptDraft.trim() || !!busy}>Envoyer<Icon name="arrow" size={14} /></button>}</div>
              </form>
            </section>
            <aside ref={contextPanel} id="session-context" className="session-context" aria-label="Repères de votre séance" hidden={!contextOpen} onKeyDown={(event) => { if (event.key === 'Escape') { setContextOpen(false); contextButton.current?.focus(); } }}>
              <button className="button secondary context-close" onClick={() => { setContextOpen(false); contextButton.current?.focus(); }}>Fermer les repères</button>
              <section className="card context-card"><h3>Votre intention</h3>{currentIntent ? <><p>{currentIntent.title}</p>{currentIntent.constraints && <p className="subtle-note">{currentIntent.constraints}</p>}<button className="link-button" disabled={!!pendingConversationId} onClick={() => editIntent(currentIntent)}>Revoir mon intention</button></> : <><p>Un objectif clair vous aide à garder le fil de votre apprentissage.</p><button className="link-button" onClick={() => editIntent()}>Définir mon intention<Icon name="arrow" size={12} /></button></>}</section>
              <section className="card context-card"><h3>Activité du tuteur</h3>{tools.length ? <ul className="context-list">{tools.map((tool) => <li key={tool.id} className="tool-entry"><strong>{tool.title}</strong><span className="tool-status">{statusLabel(tool.status)}</span></li>)}</ul> : <p>Les actions réalisées pendant votre séance apparaîtront ici.</p>}</section>

            </aside>
          </div>
        </>}

        {page === 'connections' && <Connections key={snapshot.profile?.id || 'principal'} settings={snapshot.settings} targets={targets} connected={snapshot.agentConnected} tutorStatus={snapshot.tutorStatus} connection={agentConnection} identity={snapshot.identity} api={api} onIdentityAction={identityAction} busy={!!busy || !!pendingConversationId} onDiscover={() => void discover()} onSave={(settings) => void saveSettings(settings)} onConnect={(target, settings) => void connect(target, settings)} onAuthenticate={(methodId) => void authenticate(methodId)} onDisconnectAgent={() => void disconnectAgent()} onConnectTutor={(settings, authoring) => void connectTutor(settings, authoring)} onDisconnectTutor={() => void disconnectTutor()} />}
      </main>
    </div>
  </div>;
}

function PageHeading({ eyebrow, title, description }: { eyebrow: string; title: string; description: string }) {
  return <div className="page-heading"><p className="page-eyebrow">{eyebrow}</p><h1 className="page-title">{title}</h1><p className="page-description">{description}</p></div>;
}

function Notice({ children, kind, onDismiss }: { children: ReactNode; kind: 'error' | 'info'; onDismiss?: () => void }) {
  return <div className={`notice ${kind === 'error' ? 'error' : ''}`} role={kind === 'error' ? 'alert' : 'status'}><Icon name={kind === 'error' ? 'shield' : 'spark'} size={16} /><div>{children}</div>{onDismiss && <button className="icon-button" aria-label="Fermer le message" onClick={onDismiss}><Icon name="close" size={13} /></button>}</div>;
}

function EmptyState({ icon, title, description, action }: { icon: 'chat' | 'book'; title: string; description: string; action?: ReactNode }) {
  return <section className="empty-state"><div className="empty-icon"><Icon name={icon} size={22} /></div><h3>{title}</h3><p>{description}</p>{action}</section>;
}

function GuideStep({ number, title, description }: { number: string; title: string; description: string }) {
  return <div className="guide-step"><span className="step-number">{number}</span><div><h3>{title}</h3><p>{description}</p></div></div>;
}

function IntentEditor({ draft, setDraft, onSubmit, busy }: { draft: Intent; setDraft: (value: Intent) => void; onSubmit: (event: FormEvent) => void; busy: boolean }) {
  const setField = (field: keyof Intent, value: string) => setDraft({ ...draft, [field]: value, validated: false });
  const fields: { key: keyof Intent; label: string; placeholder: string }[] = [
    { key: 'motivation', label: 'Pourquoi cela vous intéresse', placeholder: 'Ce que vous aimeriez en retirer' },
    { key: 'startingPoint', label: 'Votre point de départ', placeholder: 'Vos acquis, vos questions, vos incertitudes' },
    { key: 'goals', label: 'Ce que vous aimeriez savoir faire ou expliquer', placeholder: 'Des résultats concrets, à préciser ensemble' },
    { key: 'approach', label: 'Votre manière d’apprendre', placeholder: 'Lectures, exemples, discussion, pratique…' },
    { key: 'constraints', label: 'Votre cadre', placeholder: 'Temps disponible, langue, ressources, accessibilité…' },
    { key: 'designCriteria', label: 'Ce qui compte pour vous', placeholder: 'Les critères à respecter dans la conception' },
  ];
  return <form className="card intent-card" onSubmit={onSubmit}>
    <h2 className="card-title"><Icon name="spark" />Qu’avez-vous envie d’apprendre ?</h2>
    <label className="visually-hidden" htmlFor="intent-title">Ce que je souhaite apprendre</label>
    <textarea id="intent-title" className="intent-input" value={draft.title} onChange={(event) => setField('title', event.target.value)} placeholder="Une question qui vous intrigue, une compétence à explorer…" maxLength={2000} />
    <details><summary>Préciser mon intention <span className="field-optional">· facultatif</span></summary><div className="form-grid">{fields.map((field) => <label className="field" key={field.key}>{field.label}<textarea rows={2} value={String(draft[field.key] || '')} onChange={(event) => setField(field.key, event.target.value)} placeholder={field.placeholder} maxLength={5000} /></label>)}</div></details>
    <label className="checkbox-label" style={{ marginTop: 22 }}><input type="checkbox" checked={draft.validated} onChange={(event) => setDraft({ ...draft, validated: event.target.checked })} />Cette intention correspond à ce que je souhaite apprendre. Je pourrai la modifier ensuite.</label>
    <div className="intent-footer"><span className="privacy-note"><Icon name="shield" size={13} />Enregistrée sur cet appareil</span><button className="button primary" disabled={busy}>Valider mon intention<Icon name="arrow" size={15} /></button></div>
  </form>;
}

function FormationCard({ formation, onOpen, disabled, catalog = false }: { formation: FormationView; onOpen: () => void; disabled: boolean; catalog?: boolean }) {
  return <section className="card formation-card"><div className="formation-meta"><span className={`tag ${formation.kind === 'personal' ? 'purple' : ''}`}>{formation.kind === 'personal' ? 'Parcours personnel' : formation.kind === 'authored' ? 'Formation institutionnelle' : 'Formation commune'}</span>{formation.version !== undefined && <span className="section-caption">Version {formation.version}</span>}</div><h3>{formation.name}</h3>{formation.description && <p>{formation.description}</p>}{formation.conceptCount !== undefined && <div className="data-pair"><span>Concepts du parcours</span><strong>{formation.conceptCount}</strong></div>}{formation.publicationStatus && <div className="data-pair"><span>État du serveur</span><strong>{statusLabel(formation.publicationStatus)}</strong></div>}{formation.enrollmentStatus && <div className="data-pair"><span>Votre inscription</span><strong>{statusLabel(formation.enrollmentStatus)}</strong></div>}<div className="button-row"><button className="button secondary small" disabled={disabled} onClick={onOpen}>{formation.kind === 'authored' ? 'Examiner et publier' : catalog || !formation.domainId ? 'Découvrir avec mon tuteur' : 'Reprendre mon parcours'}<Icon name="arrow" size={13} /></button></div></section>;
}

function ConversationMessages({ conversation, streamText, streaming, openLink }: { conversation?: Conversation; streamText: string; streaming: boolean; openLink: ClientBridge['openExternalLink'] }) {
  const endRef = useRef<HTMLDivElement>(null);
  const nearEnd = useRef(true);
  const [unread, setUnread] = useState(false);
  useEffect(() => { nearEnd.current = true; setUnread(false); endRef.current?.scrollIntoView?.({ block: 'nearest' }); }, [conversation?.id]);
  useEffect(() => { if (nearEnd.current) endRef.current?.scrollIntoView?.({ block: 'nearest' }); else setUnread(true); }, [conversation?.messages.length, streamText]);
  const messages = conversation?.messages || [];
  return <div className="conversation-messages" role="log" aria-label="Messages de la séance" aria-live={streaming ? 'off' : 'polite'} aria-relevant="additions" tabIndex={0} onScroll={(event) => { const element = event.currentTarget; nearEnd.current = element.scrollHeight - element.scrollTop - element.clientHeight < 96; if (nearEnd.current) setUnread(false); }}>
    {!messages.length && !streamText && <div className="conversation-welcome"><Icon name="spark" size={29} /><h3>Un espace pour explorer ensemble.</h3><p>{conversation ? 'Posez votre première question ou expliquez ce que vous souhaitez travailler aujourd’hui.' : 'Définissez une intention depuis Aujourd’hui, ou reprenez une séance enregistrée.'}</p></div>}
    {messages.map((message) => {
      // The native snapshot creates the durable assistant placeholder before
      // the first ACP chunk. Display live chunks inside that same message;
      // completion/interruption snapshots replace it without a second bubble.
      const visibleText = message.status === 'streaming' && streamText.length > message.text.length ? streamText : message.text;
      return <article className={`message ${message.role}`} key={message.id}><div className="message-avatar" aria-hidden="true">{message.role === 'user' ? 'V' : message.role === 'assistant' ? <Icon name="spark" size={14} /> : 'i'}</div><div className="message-content"><p className="message-label">{message.role === 'user' ? 'Vous' : message.role === 'assistant' ? 'Votre tuteur' : 'Tutor'}</p><div className="message-text"><MessageBody text={visibleText} openLink={openLink} />{message.status === 'streaming' && <span className="streaming-cursor" aria-hidden="true" />}</div>{message.status === 'interrupted' && <span className="message-interrupted">Réponse interrompue — les éléments affichés peuvent être incomplets.</span>}{message.status === 'error' && <span className="message-interrupted">Ce message n’a pas été confirmé comme terminé.</span>}</div></article>;
    })}
    {!!streamText && messages.at(-1)?.role === 'user' && <div className="message assistant"><div className="message-avatar" aria-hidden="true"><Icon name="spark" size={14} /></div><div className="message-content"><p className="message-label">Votre tuteur</p><div className="message-text"><MessageBody text={streamText} openLink={openLink} />{streaming && <span className="streaming-cursor" aria-hidden="true" />}</div></div></div>}
    {unread && <button className="button secondary jump-to-latest" onClick={() => { nearEnd.current = true; setUnread(false); endRef.current?.scrollIntoView?.({ block: 'nearest' }); }}>Aller aux derniers messages</button>}
    <div ref={endRef} />
  </div>;
}

function PermissionCard({ permission, disabled, onAnswer }: { permission: PermissionRequest; disabled: boolean; onAnswer: (optionId: string | null) => void }) {
  const options = permission.options;
  return <section className="permission-card" aria-label="Autorisation demandée"><h3><Icon name="shield" size={16} />{permission.title}</h3>{permission.description && <p>{permission.description}</p>}<p>Cette action attend votre décision.</p><div className="button-row"><button className="button secondary small" disabled={disabled} onClick={() => onAnswer(null)}>Annuler la demande</button>{options.map((option) => <button key={option.optionId} className={`button small ${option.kind.startsWith('allow') ? 'primary' : 'secondary'}`} disabled={disabled} onClick={() => onAnswer(option.optionId)}>{permissionLabel(option.kind, option.name)}</button>)}</div></section>;
}

interface ConnectionsProps {
  settings: Settings; targets: AgentTarget[]; connected: boolean; tutorStatus: Snapshot['tutorStatus']; connection: AgentConnection | null; identity: Snapshot['identity']; api: ClientBridge; onIdentityAction: (label: string, action: () => Promise<unknown>) => Promise<boolean>; busy: boolean;
  onDiscover: () => void; onSave: (settings: Settings) => void; onConnect: (target: AgentTarget, settings: Settings) => void; onAuthenticate: (methodId: string) => void; onDisconnectAgent: () => void;
  onConnectTutor: (settings: Settings, authoring: boolean) => void; onDisconnectTutor: () => void;
}

function Connections(props: ConnectionsProps) {
  const { targets, connected, tutorStatus, connection, busy } = props;
  const [settings, setSettings] = useState<Settings>(() => structuredClone(props.settings));
  const [manual, setManual] = useState(!!props.settings.manualTarget);
  const [manualEnvironment, setManualEnvironment] = useState<'native' | 'wsl'>(props.settings.manualTarget?.environment || 'native');
  const [adapterPath, setAdapterPath] = useState(props.settings.manualTarget?.adapterPath || '');
  const [codexPath, setCodexPath] = useState(props.settings.manualTarget?.codexPath || '');
  const [distribution, setDistribution] = useState(props.settings.manualTarget?.distribution || '');
  const [wslUser, setWslUser] = useState(props.settings.manualTarget?.wslUser || '');
  const [methodId, setMethodId] = useState('');
  const [institution, setInstitution] = useState<Institution | null>(null);
  const [authoring, setAuthoring] = useState(false);
  const [argsText, setArgsText] = useState(JSON.stringify(props.settings.tutor.args));
  const [argsError, setArgsError] = useState('');
  const selected = targets.find((target) => target.id === settings.targetId);
  const agentName=settings.targetId==='bundled:claude'?'Claude':'Codex';
  useEffect(() => {if(!settings.targetId && targets.some(t=>t.id.startsWith('bundled:'))) setSettings(previous=>({...previous,targetId:targets.find(t=>t.id.startsWith('bundled:'))!.id}));},[targets,settings.targetId]);
  useEffect(() => {if(settings.tutor.mode!=='remote'||!settings.tutor.url)return;let live=true;const timer=setTimeout(()=>{props.api.discoverInstitution(settings.tutor.url).then(value=>{if(live)setInstitution(value);}).catch(()=>{});},400);return()=>{live=false;clearTimeout(timer);};},[props.api,settings.tutor.mode,settings.tutor.url]);
  const tutorBlockedReason = busy ? 'Une opération est en cours. Attendez sa fin.'
    : settings.tutor.mode === 'remote'
      ? !props.identity.exists ? 'Créez ou restaurez votre identité dans « Votre identité locale ».'
        : !props.identity.backupConfirmed ? 'Confirmez votre sauvegarde dans « Votre identité locale », ou vérifiez votre fichier déjà exporté.'
          : !settings.tutor.url.trim() ? 'Saisissez l’adresse de votre institution.'
            : !institution ? 'Cliquez sur « Vérifier l’institution » avant de vous connecter.' : ''
      : !settings.tutor.command.trim() ? 'Indiquez le programme Tutor local.'
        : !settings.cwd.trim() ? 'Indiquez un dossier de travail existant.' : '';

  function preparedSettings(): Settings | null {
    let args: unknown;
    try { args = JSON.parse(argsText); } catch { setArgsError('Les arguments doivent être un tableau JSON de textes, par exemple ["--local"].'); return null; }
    if (!Array.isArray(args) || args.some((value) => typeof value !== 'string')) { setArgsError('Chaque argument doit être un texte dans un tableau JSON.'); return null; }
    setArgsError('');
    const manualTarget: AgentTarget | undefined = manual ? {
      id: 'manual', label: manualEnvironment === 'wsl' ? `Codex — ${distribution || 'WSL'}` : 'Codex — installation manuelle', environment: manualEnvironment,
      adapterPath: adapterPath.trim() || undefined, codexPath: codexPath.trim() || undefined, distribution: manualEnvironment === 'wsl' ? distribution.trim() || undefined : undefined,
      wslUser: manualEnvironment === 'wsl' ? wslUser.trim() || undefined : undefined, available: !!adapterPath.trim(), detail: 'Installation sélectionnée manuellement',
    } : undefined;
    return { ...settings, targetId: manual ? 'manual' : settings.targetId, manualTarget, tutor: { ...settings.tutor, args: args as string[] } };
  }

  function connectAgent() {
    const next = preparedSettings();
    if (!next) return;
    const target = manual ? next.manualTarget : selected;
    if (target) props.onConnect(target, next);
  }

  return <>
    <IdentityPanel identity={props.identity} api={props.api} busy={busy} connected={tutorStatus.connected && settings.tutor.mode === 'remote'} institutionUrl={settings.tutor.url} run={props.onIdentityAction} />
    <PageHeading eyebrow="Votre environnement" title="Vos outils, réunis dans Tutor." description="Connectez votre agent IA et votre moteur pédagogique. Chacun conserve son rôle et son propre accès." />
    <section className="card connection-card">
      <div className="connection-heading"><div className="connection-heading-main"><span className="connection-number">02</span><div><h2 className="card-title">Votre agent IA</h2><p>Choisissez Codex ou Claude intégrés. Votre compte fournisseur reste personnel.</p></div></div><span className={`tag ${connected ? 'green' : ''}`}>{connected ? 'Connecté' : 'À connecter'}</span></div>
      <div className="button-row" style={{ marginBottom: 18 }}><button className="button secondary" disabled={busy || connected} onClick={props.onDiscover}><Icon name="refresh" size={14} />Rechercher les installations</button><span className="inline-status">Windows natif et distributions WSL2</span></div>
      {!!targets.length && <fieldset style={{ border: 0, padding: 0, margin: 0 }}><legend className="visually-hidden">Installation de Codex</legend><div className="target-list">{targets.map((target) => <label className="target-option" key={target.id}><input type="radio" name="target" checked={!manual && settings.targetId === target.id} disabled={busy || connected || !target.available} onChange={() => { setManual(false); setSettings({ ...settings, targetId: target.id }); }} /><div className="target-option-main"><strong>{target.label}</strong><small>{target.detail}{target.codexVersion ? ` · Codex ${target.codexVersion}` : ''}{target.adapterVersion ? ` · Adaptateur ${target.adapterVersion}` : ''}</small></div><span className={`tag ${target.available ? 'green' : ''}`}>{target.available ? 'Disponible' : 'À compléter'}</span></label>)}</div></fieldset>}
      <details className="advanced-workspace"><summary>Dossier de séance et réglages avancés</summary><div className="form-grid"><label className="field full">Dossier de travail<input value={settings.cwd} onChange={(event) => setSettings({ ...settings, cwd: event.target.value })} disabled={busy || connected} placeholder={selected?.environment === 'wsl' || (manual && manualEnvironment === 'wsl') ? '~' : 'C:\\Users\\vous\\Tutor'} /><small>Un dossier existant dans l’environnement choisi. Dans WSL, ~ désigne votre dossier personnel. Un dossier privé est déjà préparé pour les agents intégrés.</small></label></div></details>
      <details open={manual || undefined}><summary>Choisir une installation manuellement</summary><div className="form-grid">
        <label className="checkbox-label field full"><input type="checkbox" checked={manual} disabled={busy || connected} onChange={(event) => setManual(event.target.checked)} />Utiliser les chemins ci-dessous</label>
        <label className="field">Environnement<select value={manualEnvironment} disabled={busy || connected} onChange={(event) => setManualEnvironment(event.target.value as 'native' | 'wsl')}><option value="native">Windows natif</option><option value="wsl">WSL2</option></select></label>
        {manualEnvironment === 'wsl' && <><label className="field">Distribution WSL<input value={distribution} disabled={busy || connected} onChange={(event) => setDistribution(event.target.value)} placeholder="Ubuntu" /></label><label className="field">Utilisateur Linux <span className="field-optional">facultatif</span><input value={wslUser} disabled={busy || connected} onChange={(event) => setWslUser(event.target.value)} placeholder="Utilisateur par défaut de la distribution" /></label></>}
        <label className="field full">Chemin de l’adaptateur ACP<input value={adapterPath} disabled={busy || connected} onChange={(event) => setAdapterPath(event.target.value)} placeholder={manualEnvironment === 'wsl' ? '/home/vous/.local/bin/codex-acp' : 'C:\\Outils\\codex-acp.exe'} /><small>Le programme codex-acp, qui lancera Codex pour cette application.</small></label>
        <label className="field full">Chemin de Codex <span className="field-optional">facultatif</span><input value={codexPath} disabled={busy || connected} onChange={(event) => setCodexPath(event.target.value)} placeholder="Laisser vide pour le Codex fourni par l’adaptateur" /><small>{manualEnvironment === 'wsl' ? 'Pour utiliser votre propre installation, choisissez un exécutable ou un script Linux exécutable dans cette distribution.' : 'Pour utiliser votre propre installation, choisissez son exécutable Codex .exe ; les wrappers npm .cmd ne conviennent pas à ce champ.'} Laissez vide pour conserver la version compatible fournie par l’adaptateur.</small></label>
      </div></details>
      <div className="connection-actions"><span className="inline-status">{connection ? `${connection.agentInfo.title || connection.agentInfo.name || 'Codex'}${connection.agentInfo.version ? ` · ${connection.agentInfo.version}` : ''}` : 'Codex Desktop n’est pas nécessaire.'}</span><div className="button-row">{connected ? <button className="button secondary" disabled={busy} onClick={props.onDisconnectAgent}>Déconnecter {agentName}</button> : <button className="button primary" disabled={busy || !settings.cwd.trim() || (manual ? !adapterPath.trim() || (manualEnvironment === 'wsl' && !distribution.trim()) : !selected?.available)} onClick={connectAgent}>Connecter {agentName}<Icon name="arrow" size={14} /></button>}</div></div>
      {!!connection?.authMethods.length && <div className="form-grid" style={{ marginTop: 19 }}><label className="field">Identification {agentName}<select value={methodId || connection.authMethods[0]?.id} disabled={busy} onChange={(event) => setMethodId(event.target.value)}>{connection.authMethods.map((method) => <option key={method.id} value={method.id}>{method.name}</option>)}</select></label><div style={{ alignSelf: 'end' }}><button className="button secondary" disabled={busy} onClick={() => props.onAuthenticate(methodId || connection.authMethods[0].id)}><Icon name="external" size={13} />S’identifier auprès de {agentName}</button></div></div>}
    </section>

    <section className="card connection-card">
      <div className="connection-heading"><div className="connection-heading-main"><span className="connection-number">03</span><div><h2 className="card-title">Votre moteur Tutor</h2><p>Le serveur qui conserve vos formations et votre progression.</p></div></div><span className={`tag ${tutorStatus.connected ? 'green' : ''}`}>{tutorStatus.connected ? 'Connecté' : 'À connecter'}</span></div>
      <div className="form-grid"><label className="field full">Emplacement<select value={settings.tutor.mode} disabled={busy || tutorStatus.connected} onChange={(event) => setSettings({ ...settings, tutor: { ...settings.tutor, mode: event.target.value as 'remote' | 'local' } })}><option value="remote">Institution ou serveur distant</option><option value="local">Tutor local sur cet ordinateur</option></select></label>
        {settings.tutor.mode === 'remote' ? <label className="field full">Adresse de Tutor<input type="url" value={settings.tutor.url} disabled={busy || tutorStatus.connected} onChange={(event) => { setSettings({ ...settings, tutor: { ...settings.tutor, url: event.target.value } }); setInstitution(null); setAuthoring(false); }} placeholder="https://tutor.votre-institution.fr" /><small>Votre application signera un défi unique. L’institution doit avoir autorisé vos clés publiques ; aucun compte par e-mail n’est créé ici.</small></label> : <><label className="field full">Programme Tutor<input value={settings.tutor.command} disabled={busy || tutorStatus.connected} onChange={(event) => setSettings({ ...settings, tutor: { ...settings.tutor, command: event.target.value } })} placeholder="tutor-mcp" /><small>Le programme doit être disponible dans le même environnement que Codex : Windows ou la distribution WSL choisie.</small></label><label className="field full">Arguments du programme<input className="mono" value={argsText} disabled={busy || tutorStatus.connected} onChange={(event) => setArgsText(event.target.value)} placeholder={'["--local"]'} /><small>Tableau JSON : chaque élément correspond à un argument distinct.</small></label></>}
      </div>
      {settings.tutor.mode === 'remote' && !tutorStatus.connected && <><button className="button secondary" disabled={busy || !settings.tutor.url.trim()} onClick={() => void props.onIdentityAction('Vérification de l’institution…', async () => { setInstitution(await props.api.discoverInstitution(settings.tutor.url)); })}>Vérifier l’institution</button>{institution && <section className="institution-preview" aria-label="Institution vérifiée"><h3>{institution.name}</h3><p className="mono">{institution.issuer}</p><p className="subtle-note">Institution : {institution.institution_id}</p><label className="confirmation"><input type="checkbox" checked={authoring} disabled={busy} onChange={(event) => setAuthoring(event.target.checked)} />Demander la création et la publication de formations</label><p className="subtle-note">Accès réservé aux rôles autorisés par l’institution. Sans cette option, la session demande uniquement les permissions habituelles de votre rôle.</p></section>}</>}
      {argsError && <p role="alert" className="notice error" style={{ marginTop: 15 }}>{argsError}</p>}
      {tutorStatus.message && <p className="subtle-note">{tutorStatus.message}</p>}
      {!tutorStatus.connected && tutorBlockedReason && <p id="tutor-connect-help" className="subtle-note">{tutorBlockedReason}</p>}
      <div className="connection-actions"><span className="inline-status">Votre accès Tutor est distinct de votre compte fournisseur.</span>{tutorStatus.connected ? <button className="button secondary" disabled={busy} onClick={props.onDisconnectTutor}>Déconnecter Tutor</button> : <button className="button primary" disabled={!!tutorBlockedReason} aria-describedby={tutorBlockedReason ? 'tutor-connect-help' : undefined} onClick={() => { const next = preparedSettings(); if (next) { props.onConnectTutor(next, authoring); } }}>{settings.tutor.mode === 'remote' ? <>Connecter avec ma clé<Icon name="arrow" size={14} /></> : <>Vérifier Tutor local<Icon name="arrow" size={14} /></>}</button>}</div>
    </section>
    <div className="button-row"><button className="button secondary" disabled={busy} onClick={() => { const next = preparedSettings(); if (next) props.onSave(next); }}><Icon name="check" size={14} />Enregistrer les réglages</button></div>
    <p className="subtle-note">Codex et son adaptateur doivent être compatibles. Les fonctionnalités réellement disponibles sont vérifiées à la connexion ; une installation détectée ne garantit pas encore le fonctionnement complet d’une séance.</p>
  </>;
}
