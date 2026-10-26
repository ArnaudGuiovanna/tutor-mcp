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
import { CommandPalette } from './CommandPalette';
import type { Command } from './CommandPalette';
import { chunkFromEvent, displayDate, errorMessage, permissionFromEvent, permissionLabel, record, text, toolFromEvent } from './model';
import type { AgentConnection, AgentTarget, Conversation, ConversationContext, PermissionRequest, Settings, Snapshot, ToolActivity, TutorOverview } from './types';

type Page = 'conversation' | 'formations' | 'account' | 'settings' | 'agents' | 'institution' | 'members' | 'ergonomics';
const labels: Record<Page, string> = { conversation: 'Conversation', formations: 'Formations', account: 'Mon compte', settings: 'Paramètres', agents: 'Agents', institution: 'Institution', members: 'Membres', ergonomics: 'Ergonomie' };
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
  const [overview, setOverview] = useState<TutorOverview | null>(null);
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
    if (!state.tutorStatus.connected) { setOverview(null); return; }
    let live = true; void api.tutorOverview().then(value => { if (live) setOverview(value); }).catch(reason => { if (live) setError(errorMessage(reason)); });
    return () => { live = false; };
  }, [api, state.tutorStatus.connected, state.profile?.id]);

  async function perform<T>(label: string, action: () => Promise<T>): Promise<T | undefined> {
    setBusy(t(label)); setError('');
    try { return await action(); } catch (reason) { setError(errorMessage(reason)); return undefined; } finally { setBusy(''); }
  }

  async function ensureConversation(resume?: Conversation): Promise<Conversation> {
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
    setOverview(info);
    // Sessions carry the applicable live capabilities together. No role switch.
    if (!current.agentConnected) {
      let selected = current.settings.manualTarget || targets.find(target => target.id === current.settings.targetId);
      if (!selected) { const found = await api.discoverAgents(); setTargets(found); selected = found.find(target => target.id === current.settings.targetId); }
      if (!selected) throw new Error('Choisissez votre agent pour envoyer le message conservé.');
      setPreparing(t('Je prépare votre agent…'));
      setConnection(await api.connectAgent(selected, current.settings.cwd));
    }
    setPreparing(t('Je retrouve votre conversation…'));
    const next = await api.startConversation(undefined, resume?.id); setActiveId(next.id); setNewConversation(false);
    const snapshot = await api.loadState(); setState(snapshot); stateRef.current = snapshot;
    return next;
  }

  async function dispatch(value: string, resume?: Conversation, alreadyQueued = false) {
    if (sending.current || !value.trim()) return;
    sending.current = true; setError(''); setNotice(''); setProviderStatus(''); setPreparing(t('Je prépare votre séance…'));
    try {
      if (!alreadyQueued) { const next = await api.queuePrompt(value.trim(), resume?.id); setState(next); stateRef.current = next; }
      setDraft('');
      if (!stateRef.current.tutorStatus.connected) { setPage('institution'); setNotice(t('Votre message est conservé « À envoyer ». Terminez la connexion pour reprendre.')); return; }
      const next = await ensureConversation(resume);
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
      setOverview(await api.tutorOverview()); setNotice(next.pendingMessage ? t("Votre message est conservé. Connectez l’agent pour poursuivre.") : t("Tutor est connecté."));
      if (next.pendingMessage) setPage(next.agentConnected ? 'conversation' : 'agents');
    });
  }

  const settingsPage = settingsPages.includes(page);
  useEffect(()=>{const onKey=(event:KeyboardEvent)=>{if((event.ctrlKey||event.metaKey)&&!event.altKey&&event.key.toLowerCase()==='k'){event.preventDefault();setPaletteOpen(value=>!value);}};window.addEventListener('keydown',onKey);return()=>window.removeEventListener('keydown',onKey);},[]);
  async function openNewConversation(){await perform('Nouvelle conversation…', async () => { await api.selectConversation(); setNewConversation(true); setActiveId(undefined); setPage('conversation'); setTools([]); setDraft(''); });}
  async function openConversation(id:string){await perform('Lecture de la conversation…', async () => { const next = await api.selectConversation(id); setState(next); setActiveId(id); setNewConversation(false); setPage('conversation'); setDraft(''); setTools([]); });}
  const commands:Command[]=[
    ...(['conversation','formations','account','settings','agents','institution','members','ergonomics'] as Page[]).map(item=>({id:`page-${item}`,group:t('Aller à'),label:t(labels[item]),icon:(item==='conversation'?'chat':item==='formations'?'book':item==='account'?'shield':item==='members'?'users':item==='agents'?'plug':'gear') as Command['icon'],hint:settingsPages.includes(item)&&item!=='settings'?t('Paramètres'):undefined,run:()=>{setPage(item);if(item==='formations')setTrainingId(undefined);}})),
    {id:'new-conversation',group:t('Conversations'),label:t('Nouvelle conversation'),icon:'plus',disabled:locked||!!state.pendingMessage,run:()=>void openNewConversation()},
    ...[...state.conversations].sort((a,b)=>b.updatedAt.localeCompare(a.updatedAt)).map(item=>({id:`conversation-${item.id}`,group:t('Conversations'),label:item.title,icon:'clock' as const,hint:displayDate(item.updatedAt),disabled:locked||!!state.pendingMessage,run:()=>void openConversation(item.id)})),
    ...([['dark','Sombre'],['light','Clair'],['system','Selon le système']] as const).map(([theme,label])=>({id:`theme-${theme}`,group:t('Apparence'),label:t(label),icon:'sun' as const,disabled:locked||appearance.theme===theme,run:()=>void perform('Enregistrement des réglages…',async()=>saved(await api.saveSettings({...state.settings,appearance:{...appearance,theme}})))})),
    ...([['fr','Français'],['en','English']] as const).map(([language,label])=>({id:`language-${language}`,group:t('Langue de Tutor'),label,disabled:locked||(state.settings.language||'fr')===language,run:()=>void perform('Enregistrement des réglages…',async()=>saved(await api.saveSettings({...state.settings,language})))})),
  ];
  useEffect(()=>{setInterfaceLanguage(state.settings.language||'fr');},[state.settings.language]);
  useEffect(()=>{const media=window.matchMedia?.('(prefers-color-scheme: light)');const update=()=>{document.documentElement.dataset.theme=appearance.theme==='system'?(media?.matches?'light':'dark'):appearance.theme;document.documentElement.dataset.reduceMotion=String(appearance.reduceMotion);};update();media?.addEventListener?.('change',update);return()=>media?.removeEventListener?.('change',update);},[appearance.theme,appearance.reduceMotion]);
  function saved(next:Snapshot){stateRef.current=next;setState(next);setInterfaceLanguage(next.settings.language||'fr');}
  function connections(section:'account'|'agents'|'institution'){return <Connections key={`${state.profile?.id}:${section}`} section={section} settings={state.settings} targets={targets} connected={state.agentConnected} tutorStatus={state.tutorStatus} connection={connection} identity={state.identity} api={api} busy={locked}
    onIdentityAction={(label,action)=>perform(label,async()=>{await action();saved(await api.loadState());return true;}).then(result=>result===true)}
    onDiscover={()=>void perform('Recherche des installations…',async()=>setTargets(await api.discoverAgents()))}
    onSave={settings=>void perform('Enregistrement des réglages…',async()=>saved(await api.saveSettings(settings)))}
    onConnect={(target,settings)=>void perform('Connexion à votre agent…',async()=>{await api.saveSettings(settings);setConnection(await api.connectAgent(target,settings.cwd));saved(await api.loadState());})}
    onAuthenticate={method=>void perform('Identification auprès du fournisseur…',async()=>{await api.authenticateAgent(method);saved(await api.loadState());})}
    onDisconnectAgent={()=>void perform('Déconnexion…',async()=>{await api.disconnectAgent();setConnection(null);saved(await api.loadState());})}
    onConnectTutor={(settings,authoring)=>void configureTutor(settings,authoring)}
    onDisconnectTutor={()=>void perform('Déconnexion de Tutor…',async()=>saved(await api.disconnectTutor()))}/>;}
  if (loading) return <div className="loading-screen" role="status">{t("Ouverture de Tutor…")}</div>;
  return <div className="app-shell" style={{ '--reading-size': `${appearance.fontSize}px`, '--reading-line-height':String(appearance.lineHeight) } as CSSProperties}>
    <a className="skip-link" href="#main-content">{t("Aller au contenu principal")}</a>
    <aside className="sidebar">
      <div className="brand"><span className="wm">Tutor-MCP<span className="cursor still" aria-hidden="true" /></span></div>
      <nav className="navigation" aria-label={t("Navigation principale")}>{(['conversation','formations'] as Page[]).map(item => <button className={`nav-item ${page === item ? 'active' : ''}`} aria-label={t(labels[item])} title={t(labels[item])} key={item} aria-current={page === item ? 'page' : undefined} onClick={() => { setPage(item); if (item === 'formations') setTrainingId(undefined); }}><Icon name={item === 'conversation' ? 'chat' : 'book'} /><span>{t(labels[item])}</span></button>)}
      <button className="nav-item" aria-label={t("Palette de commandes")} title={t("Palette de commandes")} aria-keyshortcuts="Control+K Meta+K" onClick={() => setPaletteOpen(true)}><Icon name="search" /><span>{t("Rechercher ou aller à…")}</span><span className="kbd" aria-hidden="true">{modifier} K</span></button></nav>
      <button className="new-conversation" aria-label={t("Nouvelle conversation")} title={t("Nouvelle conversation")} disabled={locked || !!state.pendingMessage} onClick={() => void openNewConversation()}><Icon name="plus" size={16} /><span>{t("Nouvelle conversation")}</span></button>
      <nav className="conversation-history" aria-label={t("Historique des conversations")}><p className="sidebar-caption">{t("Conversations")}</p>{[...state.conversations].sort((a,b) => b.updatedAt.localeCompare(a.updatedAt)).map(item => <button key={item.id} className="history-item" aria-label={item.title} title={item.title} aria-current={conversation?.id === item.id ? 'true' : undefined} disabled={locked || !!state.pendingMessage} onClick={() => void openConversation(item.id)}><span>{item.title}</span><small>{displayDate(item.updatedAt)}</small></button>)}</nav>
      <nav className="sidebar-bottom" aria-label={t("Compte et réglages")}>{(['account','settings'] as Page[]).map(item=><button className={`nav-item ${page===item||(item==='settings'&&settingsPage)?'active':''}`} aria-label={t(labels[item])} title={t(labels[item])} key={item} aria-current={page===item||(item==='settings'&&settingsPage)?'page':undefined} onClick={()=>setPage(item)}><Icon name={item==='account'?'users':'gear'} size={18}/><span>{t(labels[item])}</span></button>)}</nav>
    </aside>
    <CommandPalette open={paletteOpen} commands={commands} onClose={() => setPaletteOpen(false)} />
    <main id="main-content" className={`main-shell ${page === 'conversation' ? 'conversation-main' : ''}`} tabIndex={-1}>
      <header className="workspace-heading"><h1>{t(settingsPage?'Paramètres':labels[page])}</h1>{page === 'conversation' && <><span className="active-training">{activeTraining?.name || t('À votre rythme')}</span>{activeTraining?.training_id && <button className="link-button" onClick={() => { setTrainingId(activeTraining.training_id); setPage('formations'); }}>{t("Voir la formation")}</button>}</>}<span className="connection-status" aria-label={t("État des connexions")}><span className={`dot ${state.tutorStatus.connected && state.agentConnected ? 'online' : ''}`} />{state.tutorStatus.connected && state.agentConnected ? t("Prêt") : t("À connecter")}</span></header>
      {error && <Notice kind="error" onDismiss={() => setError('')}>{t(error)}</Notice>}
      {notice && <Notice kind="info" onDismiss={() => setNotice('')}>{t(notice)}</Notice>}
      {busy && <div className="notice" role="status"><span className="spinner" />{busy}</div>}
      {state.pendingMessage && !pending && <section className="outbox" aria-label={t("Message à envoyer")}><div><strong>{t("À envoyer")}</strong><p>{state.pendingMessage.text}</p></div><div className="button-row"><button className="button secondary" disabled={locked} onClick={() => void perform('Retrait du message…', async () => { setDraft(state.pendingMessage!.text); setState(await api.queuePrompt(null)); setPage('conversation'); })}>{t("Modifier")}</button><button className="button primary" disabled={locked} onClick={() => { if (!state.tutorStatus.connected) setPage('institution'); else if (!state.agentConnected) setPage('agents'); else void dispatch(state.pendingMessage!.text, state.conversations.find(item => item.id === state.pendingMessage!.conversationId), true); }}>{state.tutorStatus.connected && state.agentConnected ? t("Envoyer le message conservé") : t("Terminer la connexion")}</button></div></section>}
      {page === 'conversation' && <section className="conversation-panel" aria-label={t("Conversation avec votre tuteur")}>
        <ConversationMessages conversation={conversation} streamText={stream} streaming={!!pending} openLink={api.openExternalLink} />
        {permissions.map((permission,index) => <PermissionCard key={JSON.stringify(permission.id)} permission={permission} disabled={permissionBusy || index > 0} onAnswer={optionId => void (async () => { setPermissionBusy(true); try { await api.answerPermission(permission.id,optionId); setPermissions(previous => previous.filter(item => JSON.stringify(item.id) !== JSON.stringify(permission.id))); } catch (reason) { setError(errorMessage(reason)); } finally { setPermissionBusy(false); } })()} />)}
        {(pending||preparing)&&<TutorActivity tools={tools} streaming={!!stream} permission={!!permissions.length} preparing={preparing} providerStatus={providerStatus}/>}
        <form className="composer" onSubmit={event => { event.preventDefault(); void dispatch(draft,conversation); }}>
          <label className="visually-hidden" htmlFor="prompt">{t("Votre message au tuteur")}</label><textarea autoFocus id="prompt" value={draft} onChange={event => setDraft(event.target.value)} disabled={!!pending || !!state.pendingMessage} placeholder={t("Je veux apprendre le Go…")} maxLength={64000} onKeyDown={event => { if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); } }} />
          <div className="composer-footer"><span className="composer-hint">{t("Ctrl + Entrée pour envoyer · L’IA peut se tromper.")}</span>{pending ? <button type="button" className="button secondary" onClick={() => void api.cancelPrompt(pending).catch(reason => setError(errorMessage(reason)))}><Icon name="pause" size={16} />{t("Interrompre")}</button> : <button className="button blue" disabled={locked || !draft.trim() || !!state.pendingMessage}>{t("Envoyer")}<span className="kbd" aria-hidden="true">{modifier} ⏎</span></button>}</div>
        </form>
      </section>}
      {page === 'formations' && <TrainingWorkspace key={`${state.profile?.id}:${state.settings.tutor.url}:${overview?.identity?.tenantId}:${state.tutorStatus.connected}`} api={api} connected={state.tutorStatus.connected} epoch={libraryEpoch} initialId={trainingId} canCreate={overview?.identity?.roles?.includes('owner') === true} busy={locked} onLearn={learn} onDiscuss={discuss} onConnect={() => setPage('institution')} />}
      {page==='account'&&<div className="settings-content"><AccountMenu key={state.profile?.id} snapshot={state} overview={overview} busy={locked} api={api} onSelect={id=>void perform('Changement de compte…',async()=>applyProfile(await api.selectProfile(id)))} onCreate={label=>void perform('Création du compte…',async()=>{applyProfile(await api.createProfile(label));await api.createIdentity();saved(await api.loadState());setPage('account');})} onRename={label=>void perform('Enregistrement du nom…',async()=>saved(await api.renameProfile(label)))} onDelete={id=>void perform('Retrait du compte…',async()=>{applyProfile(await api.deleteProfile(id,id));setPage('account');})}/>{connections('account')}</div>}
      {settingsPage&&<div className="settings-content"><SettingsWorkspace key={`${state.profile?.id}:${page}:${state.settings.tutor.url}:${overview?.identity?.tenantId}`} api={api} snapshot={state} overview={overview} busy={locked} initialTab={page==='settings'?'agents':page as 'agents'|'institution'|'members'|'ergonomics'} renderConnection={connections} onSaved={saved}/></div>}

    </main>
  </div>;
}

function Notice({ children,kind,onDismiss }: { children: ReactNode;kind:'error'|'info';onDismiss:()=>void }) { return <div className={`notice ${kind}`} role={kind === 'error' ? 'alert' : 'status'}><div>{children}</div><button className="icon-button" aria-label={t("Fermer le message")} onClick={onDismiss}><Icon name="close" size={16} /></button></div>; }

function ConversationMessages({ conversation,streamText,streaming,openLink }: { conversation?: Conversation;streamText:string;streaming:boolean;openLink:ClientBridge['openExternalLink'] }) {
  const end = useRef<HTMLDivElement>(null); const nearEnd = useRef(true); const [unread,setUnread] = useState(false);
  useEffect(() => { nearEnd.current = true; setUnread(false); end.current?.scrollIntoView?.({block:'nearest'}); },[conversation?.id]);
  useEffect(() => { if (nearEnd.current) end.current?.scrollIntoView?.({block:'nearest'}); else setUnread(true); },[conversation?.messages.length,streamText]);
  const messages = conversation?.messages || [];
  return <div className="conversation-messages" role="log" aria-label={t("Messages de la conversation")} aria-live={streaming ? 'off' : 'polite'} aria-relevant="additions" tabIndex={0} onScroll={event => { const element = event.currentTarget; nearEnd.current = element.scrollHeight - element.scrollTop - element.clientHeight < 96; if (nearEnd.current) setUnread(false); }}>
    {!messages.length && !streamText && <div className="conversation-welcome"><span className="welcome-mark" aria-hidden="true"><span className="wm">T<span className="cursor" /></span></span><h2>{t("Qu’avez-vous envie d’apprendre ?")}</h2><p>{t("Un sujet, une question, une envie.")}<br />{t("Votre formation commence ici.")}</p><div className="welcome-examples"><span>{t("« Je veux apprendre le Go »")}</span><span>{t("« Reprenons ma formation Python »")}</span></div></div>}
    {messages.map((message,index) => {
      const visible = message.status === 'streaming' && streamText.length > message.text.length ? streamText : message.text;
      const context: ConversationContext | undefined = message.trainingContext;
      const changed = context?.training_id && context.training_id !== messages[index - 1]?.trainingContext?.training_id;
      return <article className={`message ${message.role}`} key={message.id}>{changed && <p className="training-turn">{context.name}</p>}<p className="message-label">{message.role === 'user' ? t("Vous") : message.role === 'assistant' ? t("Tutor") : t("Information")}</p><div className="message-text"><MessageBody text={visible} openLink={openLink} />{message.status === 'streaming' && <span className="streaming-cursor" aria-hidden="true" />}</div>{message.status === 'interrupted' && <p className="message-interrupted">{t("Réponse interrompue — les éléments affichés peuvent être incomplets.")}</p>}{message.status === 'error' && <p className="message-interrupted">{t("La réponse n’a pas été confirmée comme terminée.")}</p>}</article>;
    })}
    {!!streamText && messages.at(-1)?.role !== 'assistant' && <article className="message assistant"><p className="message-label">{t("Tutor")}</p><div className="message-text"><MessageBody text={streamText} openLink={openLink} /></div></article>}
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
 return <div className="tutor-activity" role="status" aria-live="polite" aria-atomic="true"><span className={`activity-mark ${permission?'waiting':''}`} aria-hidden="true" /><div><p>{message}</p>{finished>0&&<small>{t('{count} appel(s) terminé(s)',{count:finished})}</small>}</div></div>;
}
