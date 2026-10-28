import { t, useTranslation, setInterfaceLanguage } from './i18n';
import { useEffect, useRef, useState } from 'react';
import type { CSSProperties, ReactNode } from 'react';
import { bridge } from './bridge';
import type { ClientBridge } from './bridge';
import { Icon } from './icons';
import { MessageBody } from './MessageBody';
import { AccountMenu } from './AccountMenu';
import { Connections } from './Connections';
import { TrainingWorkspace } from './TrainingWorkspace';
import { SettingsWorkspace, defaultAppearance } from './SettingsWorkspace';
import { WindowChrome } from './WindowChrome';
import { CommandPalette } from './CommandPalette';
import { StudyComposer } from './StudyComposer';
import { ReviewPlanning } from './ReviewPlanning';
import { reviewPlanKey } from './reviewPlanningModel';
import type { ReviewPlanPreference } from './reviewPlanningModel';
import { LearningActivityCard } from './LearningActivity';
import { parseLearningContent, parseLearningResponse, responseMatchesActivity, serializeLearningResponse } from './learningActivityModel';
import type { LearningActivity, LearningResponse } from './learningActivityModel';
import { parseSlashCommand, parseStudyRequest, reviewScope, selectStudyTraining, studyRequestText, visibleStudyText } from './studyRequests';
import type { StudyFormat } from './studyRequests';
import type { Command } from './CommandPalette';
import { chunkFromEvent, displayDate, errorMessage, permissionFromEvent, permissionLabel, record, text, toolFromEvent } from './model';
import type { AgentConnection, AgentTarget, Conversation, ConversationContext, PermissionRequest, Settings, Snapshot, ToolActivity, Training, TutorOverview } from './types';

type Page = 'conversation' | 'formations' | 'planning' | 'account' | 'settings' | 'agents' | 'institution' | 'members' | 'ergonomics';
const labels: Record<Page, string> = { conversation: 'Conversation', formations: 'Formations', planning: 'Planning', account: 'Mon compte', settings: 'Paramètres', agents: 'Connexion et outils', institution: 'Institution', members: 'Membres', ergonomics: 'Ergonomie' };
const settingsPages: Page[] = ['settings', 'agents', 'institution', 'members', 'ergonomics'];
// Windows is the reference target; macOS users read ⌘ for the same shortcut.
const modifier = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘' : 'Ctrl';
const empty: Snapshot = { settings: { targetId: '', cwd: '', tutor: { mode: 'remote', activity: 'auto', url: '', command: 'tutor-mcp', args: ['--local'] } }, identity: { exists: false, backupConfirmed: false, needsBackup: false }, intents: [], conversations: [], tutorStatus: { connected: false, label: 'Tutor non connecté' }, agentConnected: false };

export default function App({ api = bridge }: { api?: ClientBridge }) {
  useTranslation();
  const [state, setState] = useState<Snapshot>(empty);
  const stateRef = useRef(state); stateRef.current = state;
  const [page, setPage] = useState<Page>('conversation');
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [targets, setTargets] = useState<AgentTarget[]>([]);
  const [connection, setConnection] = useState<AgentConnection | null>(null);
  const [overviewValue, setOverview] = useState<TutorOverview | null>(null);
  const [overviewBinding, setOverviewBinding] = useState('');
  const [overviewFailure, setOverviewFailure] = useState(false);
  const [overviewAttempt, setOverviewAttempt] = useState(0);
  const [draft, setDraft] = useState('');
  const [activeId, setActiveId] = useState<string>();
  const [newConversation, setNewConversation] = useState(false);
  const [pending, setPending] = useState<string>();
  const sending = useRef(false);
  const pendingRef = useRef<string | undefined>(undefined);
  const [stream, setStream] = useState('');
  const [providerStatus, setProviderStatus] = useState('');
  const [permissions, setPermissions] = useState<PermissionRequest[]>([]);
  const [permissionBusy, setPermissionBusy] = useState(false);
  const [tools, setTools] = useState<ToolActivity[]>([]);
  const toolEvents=useRef<ToolActivity[]>([]);
  const [trainingId, setTrainingId] = useState<string>();
  const [libraryEpoch, setLibraryEpoch] = useState(0);
  const appearance = {...defaultAppearance,...state.settings.appearance};
  const [preparing,setPreparing] = useState('');
  const [paletteOpen,setPaletteOpen] = useState(false);
  const conversation = newConversation ? undefined : state.conversations.find(item => item.id === (activeId || state.activeConversationId)) || [...state.conversations].sort((a,b) => b.updatedAt.localeCompare(a.updatedAt))[0];
  const activeTraining = conversation?.activeTraining;
  const locked = !!busy || !!pending || sending.current;
  const connectionScope = reviewScope(state, null);
  const overview = overviewBinding === connectionScope ? overviewValue : null;
  const planningScope = reviewScope(state, overview);
  const structuredOutbox = !!state.pendingMessage && (!!parseStudyRequest(state.pendingMessage.text) || !!parseLearningResponse(state.pendingMessage.text));

  function applyProfile(next: Snapshot) {
    stateRef.current = next; setState(next); setActiveId(next.activeConversationId); setNewConversation(false);
    setOverview(null); setConnection(null); setDraft(''); setPending(undefined); pendingRef.current = undefined; sending.current = false;
    setStream(''); setProviderStatus(''); setPermissions([]); setTools([]); toolEvents.current=[]; setTrainingId(undefined); setLibraryEpoch(value => value + 1);
    setNotice(next.profileSwitchWarning || ''); setPreparing(''); setPage('conversation');
  }

  useEffect(() => {
    let live = true; const disposers: (() => void)[] = [];
    const register = (request: Promise<() => void>) => void request.then(dispose => live ? disposers.push(dispose) : dispose()).catch(() => {});
    void api.bundledAgents().then(found => { if (live) setTargets(found); }).catch(() => {});
    void api.loadState().then(next => { if (live) { setState(next); setActiveId(next.activeConversationId); } }).catch(reason => { if (live) setError(errorMessage(reason)); }).finally(() => { if (live) setLoading(false); });
    register(api.listenState(next => {
      if (!live || (next.profileEpoch || 0) < (stateRef.current.profileEpoch || 0)) return;
      if (next.profile?.id && stateRef.current.profile?.id && next.profile.id !== stateRef.current.profile.id) applyProfile(next); else setState(next);
    }));
    register(api.listenTutor(status => { if (live) setState(previous => ({ ...previous, tutorStatus: status })); }));
    register(api.listenAgent(event => {
      if (!live || (typeof event.profileEpoch === 'number' && event.profileEpoch < (stateRef.current.profileEpoch || 0))) return;
      if (event.type === 'connection' && (event.status === 'closed' || event.status === 'error')) {
        setState(previous => ({ ...previous, agentConnected: false })); setConnection(null); setPermissions([]); setProviderStatus('');
        if (text(event.message)) setError(text(event.message)); return;
      }
      const permission = permissionFromEvent(event,toolEvents.current);
      if (permission) { setProviderStatus(''); setPage('conversation'); setPermissions(previous => previous.some(item => JSON.stringify(item.id) === JSON.stringify(permission.id)) ? previous : [...previous, permission]); return; }
      if (!pendingRef.current) return;
      const expected = stateRef.current.conversations.find(item => item.id === pendingRef.current)?.agentSessionId;
      const received = text(record(event.params).sessionId); if (expected && received && expected !== received) return;
      if (event.type === 'provider_status') {
        setProviderStatus(event.severity === 'warning' ? text(event.message) : '');
        return;
      }
      const delta = chunkFromEvent(event); if (delta) { setProviderStatus(''); setStream(previous => previous + delta); }
      const tool = toolFromEvent(event); if (tool) {
        setProviderStatus('');
        const previous=toolEvents.current;
        const next=previous.some(item=>item.id===tool.id)?previous.map(item=>item.id===tool.id?{...item,title:tool.title||item.title,status:tool.status||item.status}:item):[...previous,tool];
        toolEvents.current=next;setTools(next);
      }
    }));
    return () => { live = false; disposers.forEach(dispose => dispose()); };
  }, [api]);

  useEffect(() => {
    setOverviewFailure(false);
    if (!state.tutorStatus.connected) { setOverview(null); return; }
    setOverview(null);
    let live = true; void api.tutorOverview().then(value => { if (live) {setOverview(value); setOverviewBinding(connectionScope);} }).catch(reason => { if (live) {setOverviewFailure(true); setError(errorMessage(reason));} });
    return () => { live = false; };
  }, [api, state.tutorStatus.connected, connectionScope, overviewAttempt]);

  async function perform<T>(label: string, action: () => Promise<T>): Promise<T | undefined> {
    setBusy(t(label)); setError('');
    try { return await action(); } catch (reason) { setError(errorMessage(reason)); return undefined; } finally { setBusy(''); }
  }

  async function ensureConversation(resume?: Conversation, allowNewForStudy = false): Promise<Conversation> {
    let current = stateRef.current;
    if (!current.tutorStatus.connected) throw new Error('Connectez Tutor pour envoyer votre message. Il reste enregistré « À envoyer ».');
    setPreparing(t('Je consulte votre progression…'));
    let info = await api.tutorOverview();
    if (current.settings.tutor.mode === 'remote' && info.capabilities?.canEnableLearning) {
      current = await api.enableLearning(); setState(current); stateRef.current = current; setConnection(null); info = await api.tutorOverview();
    }
    if (current.settings.tutor.mode === 'remote' && current.settings.tutor.activity !== 'auto') {
      const settings = {...current.settings,tutor:{...current.settings.tutor,activity:'auto' as const}};
      await api.saveSettings(settings);current=await api.connectTutor(settings.tutor.url,false);setState(current);stateRef.current=current;setConnection(null);info=await api.tutorOverview();
    }
    setOverview(info); setOverviewBinding(reviewScope(current, null));
    // Sessions carry the applicable live capabilities together. No role switch.
    if (!current.agentConnected) {
      let selected = current.settings.manualTarget || targets.find(target => target.id === current.settings.targetId);
      if (!selected) { const found = await api.discoverAgents(); setTargets(found); selected = found.find(target => target.id === current.settings.targetId); }
      if (!selected) throw new Error('Choisissez votre agent pour envoyer le message conservé.');
      setPreparing(t('Je prépare votre agent…'));
      setConnection(await api.connectAgent(selected, current.settings.cwd));
    }
    setPreparing(t('Je retrouve votre conversation…'));
    let next: Conversation;
    try { next = await api.startConversation(undefined, resume?.id); }
    catch (reason) {
      // A planning launch may follow a deliberate institution change. Never
      // carry the old server's history into the newly selected institution.
      if (!allowNewForStudy || !resume || errorMessage(reason) !== 'Cette séance appartient à une autre connexion Tutor. Rétablissez le compte et l’institution d’origine.') throw reason;
      next = await api.startConversation();
      const queued = stateRef.current.pendingMessage;
      if (queued) await api.queuePrompt(queued.text, next.id);
    }
    setActiveId(next.id); setNewConversation(false);
    const snapshot = await api.loadState(); setState(snapshot); stateRef.current = snapshot;
    return next;
  }

  async function dispatch(value: string, resume?: Conversation, alreadyQueued = false): Promise<boolean> {
    if (sending.current || !value.trim() || (stateRef.current.pendingMessage && !alreadyQueued)) return false;
    let accepted = alreadyQueued;
    sending.current = true; setError(''); setNotice(''); setProviderStatus(''); setPreparing(t('Je prépare votre séance…'));
    try {
      if (!alreadyQueued) { const next = await api.queuePrompt(value.trim(), resume?.id); setState(next); stateRef.current = next; accepted = true; }
      setDraft('');
      if (!stateRef.current.tutorStatus.connected) { setPage('agents'); setNotice(t('Votre message est conservé « À envoyer ». Terminez la connexion pour reprendre.')); return accepted; }
      const request = parseStudyRequest(value);
      const next = await ensureConversation(resume, !!request);
      if (request?.trainingId) {
        setPreparing(t('Je retrouve votre formation…'));
        const confirmed = await selectStudyTraining(api, next.id, request);
        const snapshot = {...stateRef.current, conversations: stateRef.current.conversations.map(item => item.id === next.id ? {...item, activeTraining: confirmed} : item)};
        stateRef.current = snapshot; setState(snapshot);
      }
      pendingRef.current = next.id; setPending(next.id); setPreparing(''); setStream(''); setTools([]); toolEvents.current=[]; setPage('conversation');
      setState(previous => ({...previous,conversations:previous.conversations.map(item=>item.id===next.id?{...item,messages:[...item.messages,{id:crypto.randomUUID(),role:'user',text:value.trim(),status:'complete',createdAt:new Date().toISOString()}]}:item)}));
      const result = await api.sendPrompt(next.id, value.trim());
      if (result.stopReason === 'cancelled') setNotice('Réponse interrompue. Vos échanges et votre progression sont conservés.');
    } catch (reason) { setError(errorMessage(reason)); }
    finally {
      try { const next = await api.loadState(); setState(next); stateRef.current = next; } catch (reason) { setError(errorMessage(reason)); }
      pendingRef.current = undefined; setPending(undefined); setStream(''); setProviderStatus(''); setPermissions([]); sending.current = false; setPreparing('');
      setLibraryEpoch(value => value + 1);
    }
    return accepted;
  }

  function sendDraft() {
    const command = parseSlashCommand(draft);
    if (!command) {void dispatch(draft, conversation); return;}
    void dispatch(studyRequestText({version:1, format:command.format, intent:'practice',
      trainingId:activeTraining?.training_id, trainingName:activeTraining?.name, domainId:activeTraining?.domain_id}, command.topic), conversation);
  }

  async function saveReviewPreference(trainingId: string, preference: ReviewPlanPreference) {
    const scope = planningScope;
    const current = stateRef.current;
    setBusy(t('Enregistrement des réglages…')); setError('');
    try {
      const next = await api.saveSettings({...current.settings, reviewPlans:{...current.settings.reviewPlans, [reviewPlanKey(scope, trainingId)]:preference}});
      saved(next);
    } catch (reason) {setError(errorMessage(reason)); throw reason;} finally {setBusy('');}
  }

  function startReview(training: Training, format: StudyFormat, concept?: string) {
    if (locked || state.pendingMessage) return;
    // Resume an existing conversation on this server when it already has this
    // training. Otherwise use the current conversation and switch explicitly.
    const resume = [...state.conversations].sort((a,b) => b.updatedAt.localeCompare(a.updatedAt)).find(item =>
      item.activeTraining?.training_id === training.id && (!conversation || item.tutorBinding === conversation.tutorBinding)) || conversation;
    setPage('conversation');
    void dispatch(studyRequestText({version:1, format, intent:'review', trainingId:training.id, trainingName:training.name, domainId:training.domain_id, concept}), resume);
  }

  async function respondToActivity(activity: LearningActivity, response: LearningResponse) {
    if (locked || state.pendingMessage || !conversation || !responseMatchesActivity(activity, response) ||
        activeTraining?.training_id !== activity.trainingId || activeTraining?.domain_id !== activity.domainId) return false;
    const summary = response.kind === 'quiz' && activity.kind === 'quiz'
      ? t('Ma réponse : {answer}', {answer:activity.options.find(option => option.id === response.selectedOptionId)?.text || ''})
      : t('Mon autoévaluation : {rating}', {rating:t(response.kind === 'flashcard' && response.rating === 'again' ? 'Pas retrouvé' : response.kind === 'flashcard' && response.rating === 'hard' ? 'Retrouvé avec effort' : 'Retrouvé facilement')});
    return dispatch(`${summary}\n\n${serializeLearningResponse(response)}`, conversation);
  }

  function continueActivity(activity: LearningActivity) {
    const lastRequest = [...(conversation?.messages || [])].reverse().filter(item => item.role === 'user').map(item => parseStudyRequest(item.text)).find(Boolean);
    void dispatch(studyRequestText({version:1, format:activity.kind, intent:lastRequest?.intent || 'practice', trainingId:activity.trainingId, domainId:activity.domainId, trainingName:activeTraining?.name}), conversation);
  }

  // A queued request resumes only on a successful configuration transition.
  // Native persistence consumes the outbox atomically with the new user turn.
  const previousReady = useRef(false);
  useEffect(() => {
    const ready = state.tutorStatus.connected && state.agentConnected;
    const transitioned = ready && !previousReady.current; previousReady.current = ready;
    if (transitioned && state.pendingMessage && !sending.current) {
      const queued = state.pendingMessage;
      const resume = queued.conversationId ? state.conversations.find(item => item.id === queued.conversationId) : undefined;
      void dispatch(queued.text, resume, true);
    }
  }, [state.tutorStatus.connected, state.agentConnected, state.pendingMessage]);

  async function learn(training: { id: string; name: string; domain_id?: string }) {
    const message = training.domain_id ? t('Reprenons la formation « {name} » ({id}). Sélectionne-la explicitement dans cette conversation, puis propose la prochaine activité.',{name:training.name,id:training.id}) : t('Présente-moi la formation « {name} » ({id}) et ses conditions d’inscription.',{name:training.name,id:training.id});
    setTrainingId(undefined); await dispatch(message, conversation);
  }

  async function discuss(id: string) {
    await dispatch(t('Je souhaite modifier le programme de la formation {id}. Lis son état réel avec get_training, puis prépare une proposition ciblée avec preview_training_edit et propose_training_edit. Je l’examinerai et la retoucherai dans le même éditeur avant validation. Demande-moi le changement souhaité si je ne l’ai pas encore précisé.',{id}), conversation);
  }

  async function configureTutor(settings: Settings, authoring: boolean) {
    await perform('Connexion à Tutor…', async () => {
      const automatic = { ...settings, tutor: { ...settings.tutor, activity: 'auto' as const } };
      await api.saveSettings(automatic);
      if (settings.tutor.mode === 'remote') await api.connectTutor(settings.tutor.url, authoring); else await api.tutorOverview();
      const next = await api.loadState(); setState(next); stateRef.current = next;
      setOverview(await api.tutorOverview()); setOverviewBinding(reviewScope(next, null)); setNotice(next.pendingMessage ? t("Votre message est conservé. Connectez l’agent pour poursuivre.") : t("Tutor est connecté."));
      // Stay in the connection workspace; the outbox resumes when both connections are ready.
    });
  }

  const settingsPage = settingsPages.includes(page);
  useEffect(()=>{const onKey=(event:KeyboardEvent)=>{if((event.ctrlKey||event.metaKey)&&!event.altKey&&event.key.toLowerCase()==='k'){event.preventDefault();setPaletteOpen(value=>!value);}};window.addEventListener('keydown',onKey);return()=>window.removeEventListener('keydown',onKey);},[]);
  async function openNewConversation(){await perform('Nouvelle conversation…', async () => { await api.selectConversation(); setNewConversation(true); setActiveId(undefined); setPage('conversation'); setTools([]); setDraft(''); });}
  async function openConversation(id:string){await perform('Lecture de la conversation…', async () => { const next = await api.selectConversation(id); setState(next); setActiveId(id); setNewConversation(false); setPage('conversation'); setDraft(''); setTools([]); });}
  const commands:Command[]=[
    ...(['conversation','formations','planning','account','settings','agents','institution','members','ergonomics'] as Page[]).map(item=>({id:`page-${item}`,group:t('Aller à'),label:t(labels[item]),icon:(item==='conversation'?'chat':item==='formations'?'book':item==='planning'?'clock':item==='account'?'shield':item==='members'?'users':item==='agents'?'plug':'gear') as Command['icon'],hint:settingsPages.includes(item)&&item!=='settings'?t('Paramètres'):undefined,run:()=>{setPage(item);if(item==='formations')setTrainingId(undefined);}})),
    {id:'new-conversation',group:t('Conversations'),label:t('Nouvelle conversation'),icon:'plus',disabled:locked||!!state.pendingMessage,run:()=>void openNewConversation()},
    ...[...state.conversations].sort((a,b)=>b.updatedAt.localeCompare(a.updatedAt)).map(item=>({id:`conversation-${item.id}`,group:t('Conversations'),label:item.title,icon:'clock' as const,hint:displayDate(item.updatedAt),disabled:locked||!!state.pendingMessage,run:()=>void openConversation(item.id)})),
    ...([['dark','Sombre'],['light','Clair'],['system','Selon le système']] as const).map(([theme,label])=>({id:`theme-${theme}`,group:t('Apparence'),label:t(label),icon:'sun' as const,disabled:locked||appearance.theme===theme,run:()=>void perform('Enregistrement des réglages…',async()=>saved(await api.saveSettings({...state.settings,appearance:{...appearance,theme}})))})),
    ...([['fr','Français'],['en','English']] as const).map(([language,label])=>({id:`language-${language}`,group:t('Langue de Tutor'),label,disabled:locked||(state.settings.language||'fr')===language,run:()=>void perform('Enregistrement des réglages…',async()=>saved(await api.saveSettings({...state.settings,language})))})),
  ];
  useEffect(()=>{setInterfaceLanguage(state.settings.language||'fr');},[state.settings.language]);
  useEffect(()=>{const media=window.matchMedia?.('(prefers-color-scheme: light)');const update=()=>{document.documentElement.dataset.theme=appearance.theme==='system'?(media?.matches?'light':'dark'):appearance.theme;document.documentElement.dataset.reduceMotion=String(appearance.reduceMotion);};update();media?.addEventListener?.('change',update);return()=>media?.removeEventListener?.('change',update);},[appearance.theme,appearance.reduceMotion]);
  function saved(next:Snapshot){if((next.profileEpoch||0)<(stateRef.current.profileEpoch||0)||(next.profile?.id&&stateRef.current.profile?.id&&next.profile.id!==stateRef.current.profile.id))return;stateRef.current=next;setState(next);setInterfaceLanguage(next.settings.language||'fr');}
  function connections(section:'account'|'agents'|'institution',sectionBusy=locked){return <Connections key={`${state.profile?.id}:${section}`} section={section} settings={state.settings} targets={targets} connected={state.agentConnected} tutorStatus={state.tutorStatus} connection={connection} identity={state.identity} api={api} busy={sectionBusy}
    onIdentityAction={(label,action)=>perform(label,async()=>{await action();saved(await api.loadState());return true;}).then(result=>result===true)}
    onDiscover={()=>void perform('Recherche des installations…',async()=>setTargets(await api.discoverAgents()))}
    onSave={settings=>void perform('Enregistrement des réglages…',async()=>saved(await api.saveSettings(settings)))}
    onConnect={(target,settings)=>void perform('Connexion à votre agent…',async()=>{await api.saveSettings(settings);setConnection(await api.connectAgent(target,settings.cwd));saved(await api.loadState());})}
    onAuthenticate={method=>void perform('Identification auprès du fournisseur…',async()=>{await api.authenticateAgent(method);saved(await api.loadState());})}
    onDisconnectAgent={()=>void perform('Déconnexion…',async()=>{await api.disconnectAgent();setConnection(null);saved(await api.loadState());})}
    onConnectTutor={(settings,authoring)=>void configureTutor(settings,authoring)}
    onDisconnectTutor={()=>void perform('Déconnexion de Tutor…',async()=>saved(await api.disconnectTutor()))}/>;}
  if (loading) return <div className="app-shell loading-shell"><WindowChrome /><div className="loading-screen" role="status"><span className="spinner" />{t("Ouverture de Tutor…")}</div></div>;
  return <div className="app-shell" style={{ '--reading-size': `${appearance.fontSize}px`, '--reading-line-height':String(appearance.lineHeight) } as CSSProperties}>
    <WindowChrome />
    <a className="skip-link" href="#main-content">{t("Aller au contenu principal")}</a>
    <aside className="sidebar">
      <p className="sidebar-caption sidebar-space">{state.profile?.label || t("Mon espace")}</p>
      <nav className="navigation" aria-label={t("Navigation principale")}>{(['conversation','formations','planning'] as Page[]).map(item => <button className={`nav-item ${page === item ? 'active' : ''}`} aria-label={t(labels[item])} title={t(labels[item])} key={item} aria-current={page === item ? 'page' : undefined} onClick={() => { setPage(item); if (item === 'formations') setTrainingId(undefined); }}><Icon name={item === 'conversation' ? 'chat' : item === 'planning' ? 'clock' : 'book'} /><span>{t(labels[item])}</span></button>)}
      <button className="nav-item" aria-label={t("Palette de commandes")} title={t("Palette de commandes")} aria-keyshortcuts="Control+K Meta+K" onClick={() => setPaletteOpen(true)}><Icon name="search" /><span>{t("Rechercher")}</span><span className="kbd" aria-hidden="true">{modifier} K</span></button></nav>
      <button className="new-conversation" aria-label={t("Nouvelle conversation")} title={t("Nouvelle conversation")} disabled={locked || !!state.pendingMessage} onClick={() => void openNewConversation()}><Icon name="plus" size={16} /><span>{t("Nouvelle conversation")}</span></button>
      <nav className="conversation-history" aria-label={t("Historique des conversations")}><p className="sidebar-caption">{t("Conversations")}</p>{[...state.conversations].sort((a,b) => b.updatedAt.localeCompare(a.updatedAt)).map(item => <button key={item.id} className="history-item" aria-label={item.title} title={item.title} aria-current={conversation?.id === item.id ? 'true' : undefined} disabled={locked || !!state.pendingMessage} onClick={() => void openConversation(item.id)}><span>{item.title}</span><small>{displayDate(item.updatedAt)}</small></button>)}</nav>
      <nav className="sidebar-bottom" aria-label={t("Compte et réglages")}>{(['account','settings'] as Page[]).map(item=><button className={`nav-item ${page===item||(item==='settings'&&settingsPage)?'active':''}`} aria-label={t(labels[item])} title={t(labels[item])} key={item} aria-current={page===item||(item==='settings'&&settingsPage)?'page':undefined} onClick={()=>setPage(item)}><Icon name={item==='account'?'users':'gear'} size={18}/><span>{t(labels[item])}</span></button>)}</nav>
    </aside>
    <CommandPalette open={paletteOpen} commands={commands} onClose={() => setPaletteOpen(false)} />
    <main id="main-content" className={`main-shell ${page === 'conversation' ? 'conversation-main' : ''}`} tabIndex={-1}>
      <header className="workspace-heading"><h1>{t(settingsPage?'Paramètres':labels[page])}</h1>{page === 'conversation' && <><span className="active-training">{activeTraining?.name || t('À votre rythme')}</span>{activeTraining?.training_id && <button className="link-button" onClick={() => { setTrainingId(activeTraining.training_id); setPage('formations'); }}>{t("Voir la formation")}</button>}</>}<button type="button" className="connection-status" aria-label={t("État des connexions")} title={t("Configurer la connexion et les outils")} onClick={() => setPage('agents')}><span className={`dot ${state.tutorStatus.connected && state.agentConnected ? 'online' : ''}`} />{state.tutorStatus.connected && state.agentConnected ? t("Prêt") : t("À connecter")}<Icon name="chevron" size={12} /></button></header>
      {error && <Notice kind="error" onDismiss={() => setError('')}>{t(error)}</Notice>}
      {notice && <Notice kind="info" onDismiss={() => setNotice('')}>{t(notice)}</Notice>}
      {busy && <div className="notice" role="status"><span className="spinner" />{busy}</div>}
      {state.pendingMessage && !pending && <section className="outbox" aria-label={t("Message à envoyer")}><div><strong>{t("À envoyer")}</strong><p>{visibleStudyText(state.pendingMessage.text)}</p></div><div className="button-row"><button className="button secondary" disabled={locked} onClick={() => void perform('Retrait du message…', async () => { if (!structuredOutbox) setDraft(visibleStudyText(state.pendingMessage!.text)); setState(await api.queuePrompt(null)); setPage('conversation'); })}>{structuredOutbox ? t("Retirer") : t("Modifier")}</button><button className="button primary" disabled={locked} onClick={() => { if (!state.tutorStatus.connected) setPage('agents'); else if (!state.agentConnected) setPage('agents'); else void dispatch(state.pendingMessage!.text, state.conversations.find(item => item.id === state.pendingMessage!.conversationId), true); }}>{state.tutorStatus.connected && state.agentConnected ? t("Envoyer le message conservé") : t("Terminer la connexion")}</button></div></section>}
      {page === 'conversation' && <section className="conversation-panel" aria-label={t("Conversation avec votre tuteur")}>
        <ConversationMessages key={`${state.profile?.id}:${conversation?.id}`} conversation={conversation} streamText={stream} streaming={!!pending} disabled={locked || !!state.pendingMessage} pendingText={state.pendingMessage?.conversationId === conversation?.id ? state.pendingMessage?.text : undefined} openLink={api.openExternalLink} onRespond={respondToActivity} onContinue={continueActivity} onSuggestion={value => { setDraft(value); document.getElementById('prompt')?.focus(); }} />
        {permissions.map((permission,index) => <PermissionCard key={JSON.stringify(permission.id)} permission={permission} disabled={permissionBusy || index > 0} onAnswer={optionId => void (async () => { setPermissionBusy(true); try { await api.answerPermission(permission.id,optionId); setPermissions(previous => previous.filter(item => JSON.stringify(item.id) !== JSON.stringify(permission.id))); } catch (reason) { setError(errorMessage(reason)); } finally { setPermissionBusy(false); } })()} />)}
        {(pending||preparing)&&<TutorActivity tools={tools} streaming={!!stream} permission={!!permissions.length} preparing={preparing} providerStatus={providerStatus}/>}
        <StudyComposer draft={draft} onChange={setDraft} disabled={!!pending || !!state.pendingMessage} canSend={!locked && !!draft.trim() && !state.pendingMessage} pending={!!pending} onSend={sendDraft} onCancel={() => void api.cancelPrompt(pending!).catch(reason => setError(errorMessage(reason)))} />
      </section>}
      {page === 'planning' && (state.tutorStatus.connected && !overview ? <div className="page-content"><p role="status">{t(overviewFailure ? 'Le planning est momentanément indisponible.' : 'Lecture du planning…')}</p>{overviewFailure && <button className="button secondary" disabled={locked} onClick={() => {setError(''); setOverviewAttempt(value => value + 1);}}>{t('Réessayer')}</button>}</div> : <ReviewPlanning key={planningScope} api={api} scope={planningScope} preferences={state.settings.reviewPlans || {}} connected={state.tutorStatus.connected} busy={locked || !!state.pendingMessage} refreshKey={libraryEpoch} onPreference={saveReviewPreference} onStart={startReview}/>)}
      {page === 'formations' && <TrainingWorkspace key={`${state.profile?.id}:${state.settings.tutor.url}:${overview?.identity?.tenantId}:${state.tutorStatus.connected}`} api={api} connected={state.tutorStatus.connected} epoch={libraryEpoch} initialId={trainingId} canCreate={overview?.identity?.roles?.includes('owner') === true} busy={locked} onLearn={learn} onDiscuss={discuss} onConnect={() => setPage('agents')} />}
      {page==='account'&&<div className="settings-content"><AccountMenu key={state.profile?.id} snapshot={state} overview={overview} busy={locked} api={api} onSelect={id=>void perform('Changement de compte…',async()=>applyProfile(await api.selectProfile(id)))} onCreate={label=>void perform('Création du compte…',async()=>{applyProfile(await api.createProfile(label));await api.createIdentity();saved(await api.loadState());setPage('account');})} onRename={label=>void perform('Enregistrement du nom…',async()=>saved(await api.renameProfile(label)))} onDelete={id=>void perform('Retrait du compte…',async()=>{applyProfile(await api.deleteProfile(id,id));setPage('account');})}/>{connections('account')}</div>}
      {settingsPage&&<div className="settings-content"><SettingsWorkspace key={`${state.profile?.id}:${page}`} api={api} snapshot={state} overview={overview} busy={locked} initialTab={page==='settings'?'agents':page as 'agents'|'institution'|'members'|'ergonomics'} renderConnection={connections} onSaved={saved}/></div>}

    </main>
  </div>;
}

function Notice({ children,kind,onDismiss }: { children: ReactNode;kind:'error'|'info';onDismiss:()=>void }) { return <div className={`notice ${kind}`} role={kind === 'error' ? 'alert' : 'status'}><div>{children}</div><button className="icon-button" aria-label={t("Fermer le message")} onClick={onDismiss}><Icon name="close" size={16} /></button></div>; }

function ConversationMessages({ conversation,streamText,streaming,disabled,pendingText,openLink,onRespond,onContinue,onSuggestion }: {
  conversation?: Conversation;streamText:string;streaming:boolean;disabled:boolean;pendingText?:string;openLink:ClientBridge['openExternalLink'];
  onRespond:(activity:LearningActivity,response:LearningResponse)=>Promise<boolean>;onContinue:(activity:LearningActivity)=>void;onSuggestion:(value:string)=>void;
}) {
  const end = useRef<HTMLDivElement>(null); const nearEnd = useRef(true); const [unread,setUnread] = useState(false);
  const shownActivity = useRef<string | undefined>(undefined);
  const messages = conversation?.messages || [];
  const responses = messages.filter(message => message.role === 'user' && message.status === 'complete').map(message => parseLearningResponse(message.text));
  if (pendingText) responses.push(parseLearningResponse(pendingText));
  const allActivities = [...messages].reverse().filter(message => message.role === 'assistant' && message.status === 'complete')
    .flatMap(message => parseLearningContent(message.text, {complete:true}).filter(segment => segment.type === 'activity').map(segment => ({messageId:message.id, activity:segment.activity})));
  const lastActivity = allActivities[0];
  const lastActivityIndex = messages.findIndex(message => message.id === lastActivity?.messageId);
  const continued = lastActivityIndex >= 0 && messages.slice(lastActivityIndex + 1).some(message => message.role === 'user' && parseStudyRequest(message.text));
  useEffect(() => { nearEnd.current = true; shownActivity.current = undefined; setUnread(false); end.current?.scrollIntoView?.({block:'nearest'}); },[conversation?.id]);
  useEffect(() => {
    if (!nearEnd.current) {setUnread(true); return;}
    if (!streaming && lastActivity && shownActivity.current !== lastActivity.messageId) {
      shownActivity.current = lastActivity.messageId;
      // Start a new visual activity at its question. Scrolling to the end of a
      // tall card would show only the answer controls on small screens.
      const cards = end.current?.parentElement?.querySelectorAll('.learning-card');
      const card = cards?.[cards.length - 1];
      if (card) {card.scrollIntoView?.({block:'start'}); return;}
    }
    end.current?.scrollIntoView?.({block:'nearest'});
  },[conversation?.id,messages.length,streamText,streaming,lastActivity?.messageId]);
  function assistantContent(value: string, complete: boolean, messageId?: string, context?: ConversationContext) {
    return parseLearningContent(value, {complete}).map((segment, index) => {
      if (segment.type === 'text') return <MessageBody key={index} text={segment.text} openLink={openLink}/>;
      if (segment.type === 'pending') return <p key={index} className="learning-content-state" role="status">{t('Préparation de l’activité visuelle…')}</p>;
      if (segment.type === 'invalid') return <p key={index} className="learning-content-state" role="status">{t('Cette activité ne peut pas être affichée. Demandez au tuteur de la reformuler.')}</p>;
      const activity = segment.activity;
      if (allActivities.filter(item => item.activity.id === activity.id).length !== 1) return <p key={index} className="learning-content-state" role="status">{t('Cette activité ne peut pas être affichée. Demandez au tuteur de la reformuler.')}</p>;
      const response = responses.find(value => value && responseMatchesActivity(activity, value));
      const matchesContext = context?.training_id === activity.trainingId && context.domain_id === activity.domainId;
      const matchesActive = conversation?.activeTraining?.training_id === activity.trainingId && conversation.activeTraining.domain_id === activity.domainId && context?.revision === conversation.activeTraining.revision;
      const latest = lastActivity?.messageId === messageId && lastActivity.activity.id === activity.id;
      return <div key={`${index}:${activity.id}`}>
        <LearningActivityCard activity={activity} response={response} disabled={disabled || !matchesContext || !matchesActive || !latest}
          openLink={openLink} onRespond={value => onRespond(activity, value)} onContinue={latest && matchesActive && matchesContext && !continued ? () => onContinue(activity) : undefined}/>
        {(!matchesContext || !matchesActive) && <p className="learning-context-warning">{t('Cette activité appartient à une autre formation ou son contexte n’est pas confirmé.')}</p>}
      </div>;
    });
  }
  return <div className="conversation-messages" role="log" aria-label={t("Messages de la conversation")} aria-live={streaming ? 'off' : 'polite'} aria-relevant="additions" tabIndex={0} onScroll={event => { const element = event.currentTarget; nearEnd.current = element.scrollHeight - element.scrollTop - element.clientHeight < 96; if (nearEnd.current) setUnread(false); }}>
    {!messages.length && !streamText && <div className="conversation-welcome"><span className="welcome-mark" aria-hidden="true"><span className="wm">T<span className="cursor still" /></span></span><h2>{t("Qu’avez-vous envie d’apprendre ?")}</h2><p>{t("Un sujet, une question, une envie.")}<br />{t("Votre formation commence ici.")}</p><div className="welcome-examples">{["Je veux apprendre le Go", "Reprenons ma formation Python"].map(value=><button type="button" key={value} onClick={()=>onSuggestion(t(value))}><Icon name={value.startsWith("Je")?"plus":"clock"} size={16}/>{t(value)}<Icon name="arrow" size={14}/></button>)}</div></div>}
    {messages.map((message,index) => {
      const visible = message.status === 'streaming' && streamText.length > message.text.length ? streamText : message.text;
      const context: ConversationContext | undefined = message.trainingContext;
      const changed = context?.training_id && context.training_id !== messages[index - 1]?.trainingContext?.training_id;
      return <article className={`message ${message.role}`} key={message.id}>{changed && <p className="training-turn">{context.name}</p>}<p className="message-label">{message.role === 'user' ? t("Vous") : message.role === 'assistant' ? t("Tutor") : t("Information")}</p><div className="message-text">{message.role === 'assistant' ? assistantContent(visible, message.status === 'complete', message.id, context) : <MessageBody text={visibleStudyText(visible)} openLink={openLink}/>}{message.status === 'streaming' && <span className="streaming-cursor" aria-hidden="true" />}</div>{message.status === 'interrupted' && <p className="message-interrupted">{t("Réponse interrompue — les éléments affichés peuvent être incomplets.")}</p>}{message.status === 'error' && <p className="message-interrupted">{t("La réponse n’a pas été confirmée comme terminée.")}</p>}</article>;
    })}
    {!!streamText && messages.at(-1)?.role !== 'assistant' && <article className="message assistant"><p className="message-label">{t("Tutor")}</p><div className="message-text">{assistantContent(streamText, false)}</div></article>}
    {unread && <button className="button secondary jump-to-latest" onClick={() => { nearEnd.current = true; setUnread(false); end.current?.scrollIntoView?.({block:'nearest'}); }}>{t("Aller aux derniers messages")}</button>}
    <div ref={end} />
  </div>;
}

function PermissionCard({ permission,disabled,onAnswer }: { permission:PermissionRequest;disabled:boolean;onAnswer:(id:string|null)=>void }) {
  return <section className="permission-card" aria-label={t("Autorisation demandée")}><h2>{permission.title}</h2>{permission.description && <p>{permission.description}</p>}<p>{t("Cette action attend votre décision.")}</p><div className="button-row"><button className="button secondary" disabled={disabled} onClick={() => onAnswer(null)}>{t("Annuler la demande")}</button>{permission.options.map(option => <button className={`button ${option.kind.startsWith('allow') ? 'primary' : 'secondary'}`} key={option.optionId} disabled={disabled} onClick={() => onAnswer(option.optionId)}>{permissionLabel(option.kind,option.name)}</button>)}</div></section>;
}

export function TutorActivity({tools,streaming,permission,preparing,providerStatus=''}:{tools:ToolActivity[];streaming:boolean;permission:boolean;preparing:string;providerStatus?:string}){
 const finished=tools.filter(tool=>['completed','failed','cancelled'].includes(tool.status)).length;const active=[...tools].reverse().find(tool=>!['completed','failed','cancelled'].includes(tool.status));const latest=tools.at(-1);const name=(active?.title||'').toLowerCase();
 let message=preparing||t('Je prépare la prochaine étape…');
 if(permission)message=t('Votre autorisation est attendue.');
 else if(providerStatus)message=providerStatus;
 else if(active){message=/next_activity|assessment|curriculum/.test(name)?t('Je prépare la prochaine activité…'):/progress|interaction|mastery|learner_context/.test(name)?t('Je consulte votre progression…'):/training|domain|formation|library/.test(name)?t('Je retrouve votre formation…'):t('Je consulte le serveur Tutor…');}
 else if(streaming)message=t('Je rédige la réponse…');else if(latest?.status==='failed')message=t('Un outil a rencontré une erreur. Le tuteur examine le résultat…');
 return <div className="tutor-activity" role="status" aria-live="polite" aria-atomic="true"><span className={`activity-mark ${permission?'waiting':''}`} aria-hidden="true"><i/><i/><i/></span><div><p key={message} className="activity-text">{message}</p>{finished>0&&<small>{t('{count} appel(s) terminé(s)',{count:finished})}</small>}</div></div>;
}
