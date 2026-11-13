import { t, useTranslation, setInterfaceLanguage } from './i18n';
import { useCallback, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { bridge } from './bridge';
import type { ClientBridge } from './bridge';
import { Icon } from './icons';
import { MessageBody } from './MessageBody';
import { AccountMenu } from './AccountMenu';
import { Connections } from './Connections';
import { TrainingWorkspace } from './TrainingWorkspace';
import { SettingsWorkspace, defaultAppearance } from './SettingsWorkspace';
import type { SettingsTab } from './SettingsWorkspace';
import { LoadError, LoadingState } from './LoadState';
import { WindowChrome } from './WindowChrome';
import { CommandPalette } from './CommandPalette';
import { StudyComposer } from './StudyComposer';
import { ShareConversation } from './ShareConversation';
import { withAttachments, messageAttachments } from './conversationFiles';
import type { TextAttachment } from './conversationFiles';
import { WorkspaceNavigation, useWorkspaceSidebar, TrainingColor } from './WorkspaceNavigation';
import { useWorkspaceTrainings, ResumeTraining, WorkspaceTrainingPanel } from './WorkspaceTraining';
import { ReviewPlanning } from './ReviewPlanning';
import { reviewPlanKey } from './reviewPlanningModel';
import type { ReviewPlanPreference } from './reviewPlanningModel';
import { LearningActivityCard } from './LearningActivity';
import { parseLearningContent, parseLearningResponse, responseMatchesActivity, serializeLearningResponse } from './learningActivityModel';
import type { ExerciseActivity, LearningActivity, LearningResponse } from './learningActivityModel';
import { Onboarding } from './Onboarding';
import { onboardingStep } from './onboardingModel';
import { parseSlashCommand, parseStudyRequest, reviewScope, selectStudyTraining, studyRequestText, visibleStudyText } from './studyRequests';
import type { StudyFormat } from './studyRequests';
import { parseResumeRequest, resumeRequestText } from './resumeRequests';
import type { ResumeRequest } from './resumeRequests';
import { installResponseTimingCollector, ResponseRendered } from './responseTiming';
import type { PendingResponseRender } from './responseTiming';
import type { Command } from './CommandPalette';
import { agentReconnect, chunkFromEvent, displayDate, errorMessage, failureText, limitFromEvent, mergeToolActivity, permissionFromEvent, permissionLabel, record, text, toolFromEvent } from './model';
import { HarnessStatus, limitLabel, useAgentLimit } from './HarnessStatus';
import type { AgentLoss } from './HarnessStatus';
import type { AgentConnection, AgentTarget, Conversation, ConversationContext, PermissionRequest, Settings, Snapshot, ToolActivity, Training, TutorOverview } from './types';
import { acceptTurnEvent, eventForTurn, turnLimitFollowUp, turnLimitFromEvent, turnLimitFromResult, turnLockReason } from './turnEvents';
import type { TurnLimit, TurnSessionState } from './turnEvents';

type Page = 'conversation' | 'formations' | 'planning' | 'account' | 'settings' | 'agents' | 'institution' | 'members' | 'ergonomics';
const labels: Record<Page, string> = { conversation: 'Conversation', formations: 'Formations', planning: 'Planning', account: 'Mon compte', settings: 'Paramètres', agents: 'Connexion et outils', institution: 'Institution', members: 'Membres', ergonomics: 'Ergonomie' };
const settingsPages: Page[] = ['settings', 'agents', 'institution', 'members', 'ergonomics'];
const empty: Snapshot = { settings: { targetId: '', cwd: '', tutor: { mode: 'remote', activity: 'auto', url: '', command: 'tutor-mcp', args: ['--local'] } }, identity: { exists: false, backupConfirmed: false, needsBackup: false }, intents: [], conversations: [], tutorStatus: { connected: false, label: 'Tutor non connecté' }, agentConnected: false };
type ActiveTurn = {cancelled:boolean; controller:AbortController; conversationId?:string; operationId?:string; promptOperationId?:string; completedOperations?:Set<string>; text?:string; promptSent?:boolean;
  /** Turn identity its harness events must carry, set once its conversation is known. */
  sessions?:TurnSessionState};
/** A model turn that failed on its own: shown with that turn, never as a global banner. */
export type TurnError = {conversationId?:string; message:string; retryText:string};
type PendingAgentLaunch = {scope:string; promise:Promise<AgentConnection>};
const cancelledTurn = Symbol('cancelled turn');

export default function App({ api = bridge }: { api?: ClientBridge }) {
  useTranslation();
  const [state, setState] = useState<Snapshot>(empty);
  const stateRef = useRef(state); stateRef.current = state;
  const [page, setPage] = useState<Page>('conversation');
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  // A guided setup stays open until it is explicitly finished, even when the
  // snapshot already satisfies every step (for example right after connecting).
  const [setupSession, setSetupSession] = useState<false | 'auto' | 'review'>(false);
  const [composerTarget, setComposerTarget] = useState<'answer' | 'question'>('answer');
  const exerciseShownAt = useRef<{ id: string; at: number } | undefined>(undefined);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [targets, setTargets] = useState<AgentTarget[]>([]);
  const discoveryRequest = useRef<Promise<AgentTarget[]> | null>(null);
  const [connection, setConnection] = useState<AgentConnection | null>(null);
  const [autoConnecting, setAutoConnecting] = useState(false);
  const [launchPending,setLaunchPending] = useState(false);
  const pendingLaunch = useRef<PendingAgentLaunch | undefined>(undefined);
  const agentPaused = useRef(false);
  // The single automatic launch per profile, agent and folder. Later
  // reconnections are supervised natively (agent_reconnect events).
  const launchedScope = useRef<string | undefined>(undefined);
  const [agentLoss, setAgentLoss] = useState<AgentLoss>();
  const [turnError, setTurnError] = useState<TurnError>();
  const [turnLimit, setTurnLimit] = useState<TurnLimit>();
  // Harness sessions seen in snapshots or turns: their late events never reach another conversation.
  const [overviewValue, setOverview] = useState<TutorOverview | null>(null);
  const [overviewBinding, setOverviewBinding] = useState('');
  const [overviewFailure, setOverviewFailure] = useState(false);
  const [overviewAttempt, setOverviewAttempt] = useState(0);
  const [draft, setDraft] = useState('');
  const [attachments,setAttachments] = useState<TextAttachment[]>([]);
  const [activeId, setActiveId] = useState<string>();
  const [newConversation, setNewConversation] = useState(false);
  const [pending, setPending] = useState<string>();
  const sending = useRef(false);
  const activeTurn = useRef<ActiveTurn | undefined>(undefined);
  const [turnStartedAt,setTurnStartedAt] = useState<number>();
  const [responseRender,setResponseRender] = useState<PendingResponseRender>();
  useEffect(installResponseTimingCollector, []);
  const pendingRef = useRef<string | undefined>(undefined);
  const [stream, setStream] = useState('');
  const [writing, setWriting] = useState(false);
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
  const sidebar = useWorkspaceSidebar();
  const [trainingPanel,setTrainingPanel] = useState(false);
  const [shareOpen,setShareOpen] = useState(false);
  const [resumeHidden,setResumeHidden] = useState(false);
  const [practiceMenu,setPracticeMenu] = useState(0);
  const conversation = newConversation ? undefined : state.conversations.find(item => item.id === (activeId || state.activeConversationId)) || [...state.conversations].sort((a,b) => b.updatedAt.localeCompare(a.updatedAt))[0];
  const activeTraining = conversation?.activeTraining;
  const agentReady = state.agentConnected && state.agentStatus?.authentication !== 'none' && state.agentStatus?.authentication !== 'checking';
  const reconnect = agentReconnect(state.agentStatus?.reconnect);
  const limit = useAgentLimit(state.agentStatus);
  const sendBlockedReason = limit ? t('Envoi suspendu : {reason}.', {reason:limitLabel(limit.kind, limit.until)}) : '';
  const locked = !!busy || !!pending || sending.current || autoConnecting;
  // During a model turn, native connection and configuration changes are refused at once.
  const turnActive = turnStartedAt !== undefined;
  const turnLockText = turnActive ? t(turnLockReason) : '';
  const connectionScope = reviewScope(state, null);
  const overview = overviewBinding === connectionScope ? overviewValue : null;
  const planningScope = reviewScope(state, overview);
  const library = useWorkspaceTrainings(api,state.tutorStatus.connected && !!overview,planningScope,libraryEpoch);
  const home = !conversation?.messages.length;
  const currentActivity = [...(conversation?.messages || [])].reverse().filter(item=>item.role==='assistant' && !item.privateOutput).flatMap(item=>parseLearningContent(item.text,{complete:item.status==='complete'})).find(item=>item.type==='activity' && item.activity.trainingId===activeTraining?.training_id && item.activity.domainId===activeTraining?.domain_id);
  const pendingExercise = latestPendingExercise(conversation);
  const setupStep = loading ? 'done' : onboardingStep(state, { ignoreCompletion: setupSession === 'review' });
  const setupOpen = !loading && !loadError && (!!setupSession || setupStep !== 'done');
  const setupOpenRef = useRef(setupOpen); setupOpenRef.current = setupOpen;
  const recentTrainingId = [...state.conversations].sort((a,b)=>b.updatedAt.localeCompare(a.updatedAt)).find(item=>item.activeTraining)?.activeTraining?.training_id;
  const resumeTraining = library.items.find(item=>item.id === recentTrainingId) || [...library.items].filter(item=>item.progress?.some(progress=>progress.last_review)).sort((a,b)=>latestReview(b).localeCompare(latestReview(a)))[0];
  const structuredOutbox = !!state.pendingMessage && (!!parseStudyRequest(state.pendingMessage.text) || !!parseResumeRequest(state.pendingMessage.text) || !!parseLearningResponse(state.pendingMessage.text));

  function applyProfile(next: Snapshot) {
    if (activeTurn.current) {activeTurn.current.cancelled = true;activeTurn.current.controller.abort();}
    activeTurn.current = undefined; setTurnStartedAt(undefined); setResponseRender(undefined); window.tutorResponseTimings?.clear();
    pendingLaunch.current=undefined;setLaunchPending(false);
    stateRef.current = next; setState(next); setActiveId(next.activeConversationId); setNewConversation(false);
    setOverview(null); setConnection(null); setDraft(''); setAttachments([]); setPending(undefined); pendingRef.current = undefined; sending.current = false;
    setStream(''); setWriting(false); setProviderStatus(''); setPermissions([]); setTools([]); toolEvents.current=[]; setTrainingId(undefined); setLibraryEpoch(value => value + 1);
    setShareOpen(false); setResumeHidden(false); setTrainingPanel(false); setTurnError(undefined); setTurnLimit(undefined); setAgentLoss(undefined);
    setNotice(next.profileSwitchWarning ? t(next.profileSwitchWarning) : ''); setPreparing(''); setPage('conversation');
  }

  useEffect(() => {
    let live = true; const disposers: (() => void)[] = [];
    const register = (request: Promise<() => void>) => void request.then(dispose => live ? disposers.push(dispose) : dispose()).catch(() => {});
    const rememberTargets = (found: AgentTarget[]) => { if (live) setTargets(previous => [...new Map([...previous,...found].map(target => [target.id,target])).values()]); };
    void api.bundledAgents().then(rememberTargets).catch(() => {});
    discoveryRequest.current = api.discoverAgents();
    void discoveryRequest.current.then(rememberTargets).catch(() => {});
    void api.loadState().then(next => { if (live) { setState(next); setActiveId(next.activeConversationId); } }).catch(reason => { if (live) setLoadError(errorMessage(reason)); }).finally(() => { if (live) setLoading(false); });
    register(api.listenState(next => {
      if (!live || (next.profileEpoch || 0) < (stateRef.current.profileEpoch || 0)) return;
      if (next.profile?.id && stateRef.current.profile?.id && next.profile.id !== stateRef.current.profile.id) applyProfile(next);
      else { stateRef.current = next; setState(next); }
    }));
    register(api.listenTutor(status => { if (live) setState(previous => ({ ...previous, tutorStatus: status })); }));
    register(api.listenAgent(event => {
      if (!live || (typeof event.profileEpoch === 'number' && event.profileEpoch < (stateRef.current.profileEpoch || 0))) return;
      if (event.type === 'connection' && (event.status === 'closed' || event.status === 'error')) {
        if (event.intentional === true) agentPaused.current = true;
        setState(previous => ({ ...previous, agentConnected: false })); setConnection(null); setPermissions([]); setProviderStatus('');
        // An unexpected stop is reported near the composer while the native
        // supervisor reconnects; it is not a global error.
        if (event.intentional !== true && text(event.message)) setAgentLoss({message:text(event.message), stderrTail:text(event.stderrTail) || undefined});
        return;
      }
      if (event.type === 'agent_reconnect') {
        const next = agentReconnect(event);
        setState(previous => ({...previous, agentStatus:{...(previous.agentStatus ?? {phase:'stopped', authentication:'unknown'}), reconnect:next}}));
        return;
      }
      if (event.type === 'agent_limited') {
        const next = limitFromEvent(event);
        if (next) setState(previous => ({...previous, agentStatus:{...(previous.agentStatus ?? {phase:'stopped', authentication:'unknown'}), limitedUntil:next.until, limitKind:next.kind}}));
        return;
      }
      if (event.type === 'turn_error') {
        const turn = activeTurn.current;
        if (turn && !turn.cancelled && text(event.message) && eventForTurn(event, turn.conversationId, turn.sessions?.turnId)) setTurnError({conversationId:turn.conversationId, message:text(event.message), retryText:turn.text || ''});
        return;
      }
      if (event.type === 'turn_limit') {
        // Attached to its turn; a late event only completes the notice already shown from the send result.
        const reached = turnLimitFromEvent(event); if (!reached) return;
        const turn = activeTurn.current;
        if (turn && !turn.cancelled && turn.conversationId === reached.conversationId) setTurnLimit(previous => previous?.conversationId === reached.conversationId ? {...previous,...reached,messageId:previous.messageId} : reached);
        else setTurnLimit(previous => previous?.conversationId === reached.conversationId ? {...previous,...reached,messageId:previous.messageId} : previous);
        return;
      }
      if (event.type === 'turn_phase') {
        const turn=activeTurn.current;
        if (!turn || turn.cancelled || (event.conversationId && turn.conversationId && event.conversationId !== turn.conversationId)) return;
        const operationId=text(event.operationId);
        if (!operationId || turn.completedOperations?.has(operationId)) return;
        if (!turn.operationId && event.state === 'started' && event.phase === 'preparing') turn.operationId=operationId;
        if (turn.operationId !== operationId) return;
        if (event.state === 'completed' && event.outcome) {
          (turn.completedOperations ??= new Set()).add(operationId);turn.operationId=undefined;return;
        }
        if (event.state !== 'started') return;
        if (event.phase === 'waiting_model' && event.conversationId === turn.conversationId) turn.promptOperationId = operationId;
        const labels:Record<string,string>={preparing:'Je prépare votre séance…',waiting_lock:'J’attends la fin de l’opération précédente…',context:'Je consulte votre progression…',loading_session:'Je retrouve votre conversation…',waiting_model:'',finalizing:'J’enregistre la réponse…'};
        if (Object.hasOwn(labels,text(event.phase))) {setPreparing(t(labels[text(event.phase)]));setWriting(false);}
        return;
      }
      if (event.type === 'workspace_invalidated' && event.scope === 'training_catalog') {
        setLibraryEpoch(value=>value+1);return;
      }
      if (event.type === 'native_permission_closed') {
        setPermissions(previous=>previous.filter(item=>JSON.stringify(item.id)!==JSON.stringify(event.id)));return;
      }
      const permission = permissionFromEvent(event,toolEvents.current);
      if (permission) { setWriting(false); setProviderStatus(''); setPage('conversation'); setPermissions(previous => previous.some(item => JSON.stringify(item.id) === JSON.stringify(permission.id)) ? previous : [...previous, permission]); return; }
      if (!pendingRef.current) return;
      // Session events of the running turn name its conversation and turn;
      // an untagged session event belongs to no running turn.
      const turn = activeTurn.current;
      const received = text(record(event.params).sessionId);
      if (received || typeof event.conversationId === 'string') {
        const sessions = turn?.sessions;
        if (!turn || turn.cancelled || !sessions || sessions.conversationId !== pendingRef.current) return;
        if (!acceptTurnEvent(event, sessions)) return;
      }
      if (event.type === 'provider_status') {
        setWriting(false);
        setProviderStatus(event.severity === 'warning' ? text(event.message) : '');
        return;
      }
      const delta = chunkFromEvent(event); if (delta) { setWriting(true); setProviderStatus(''); setStream(previous => previous + delta); }
      const tool = toolFromEvent(event); if (tool) {
        setWriting(false);
        setProviderStatus('');
        const next=mergeToolActivity(toolEvents.current,tool);
        toolEvents.current=next;setTools(next);
      }
    }));
    return () => { live = false; disposers.forEach(dispose => dispose()); };
  }, [api]);

  const launchScope = JSON.stringify([state.profile?.id, state.settings.targetId, state.settings.cwd, state.settings.manualTarget]);
  useEffect(() => { agentPaused.current = false; }, [launchScope]);

  useEffect(() => { if (state.agentConnected) setAgentLoss(undefined); }, [state.agentConnected]);

  // Initialization is independent of MCP and never authenticates or sends a
  // prompt. It launches the remembered harness once; the native supervisor
  // owns reconnections after an unexpected stop, and a manual disconnect stays
  // stopped. A failed launch is reported with a manual « Reconnecter ».
  useEffect(() => {
    // A harness already running for this scope needs no launch.
    if (state.agentConnected && !loading) launchedScope.current = launchScope;
    if (loading || loadError || setupOpen || state.agentConnected || agentPaused.current || busy || pending || turnStartedAt !== undefined || launchPending || state.pendingMessage?.paused || !state.settings.targetId) return;
    if (launchedScope.current === launchScope || reconnect.state !== 'idle') return;
    const settings = state.settings;
    const scope = launchScope;
    // Deferred so that an immediately discarded render never launches.
    const timer = setTimeout(() => {
      launchedScope.current = scope;
      setAutoConnecting(true);
      void (async () => {
        const known = settings.manualTarget || targets.find(target => target.id === settings.targetId && target.available);
        const discovery = discoveryRequest.current || api.discoverAgents();
        void discovery.then(found => { if (launchedScope.current === scope) setTargets(found); }).catch(() => {});
        const target = known || (await discovery).find(target => target.id === settings.targetId);
        if (launchedScope.current !== scope) return;
        if (!target?.available) throw new Error('L’installation choisie est indisponible. Choisissez un agent détecté ou vérifiez son installation.');
        const connected = await api.connectAgent(target, settings.cwd);
        if (launchedScope.current === scope) { setConnection(connected); saved(await api.loadState()); }
      })().catch(reason => {
        if (launchedScope.current === scope) setAgentLoss({message:failureText(reason)});
      }).finally(() => setAutoConnecting(false));
    }, 0);
    return () => clearTimeout(timer);
  }, [api, loading, loadError, setupOpen, state.agentConnected, launchScope, reconnect.state, busy, pending, turnStartedAt, launchPending, state.pendingMessage?.paused]);

  /** Manual retry after a failed launch or once the supervisor gave up. */
  async function reconnectAgent() {
    const current = stateRef.current;
    agentPaused.current = false; setAgentLoss(undefined); setBusy(t('Connexion à votre agent…'));
    try {
      let target = current.settings.manualTarget || targets.find(item => item.id === current.settings.targetId && item.available);
      if (!target) { const found = await api.discoverAgents(); setTargets(found); target = found.find(item => item.id === current.settings.targetId); }
      if (!target?.available) throw new Error('L’installation choisie est indisponible. Choisissez un agent détecté ou vérifiez son installation.');
      setConnection(await api.connectAgent(target, current.settings.cwd));
      saved(await api.loadState());
    } catch (reason) { setAgentLoss({message:failureText(reason)}); }
    finally { setBusy(''); }
  }

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

  function checkTurn(turn: ActiveTurn) {
    if (turn.cancelled || activeTurn.current !== turn) throw cancelledTurn;
  }

  function waitForTurn<T>(turn:ActiveTurn,request:Promise<T>):Promise<T> {
    checkTurn(turn);
    return new Promise((resolve,reject)=>{
      const stop=()=>reject(cancelledTurn);
      turn.controller.signal.addEventListener('abort',stop,{once:true});
      request.then(value=>{
        turn.controller.signal.removeEventListener('abort',stop);
        try {checkTurn(turn);resolve(value);} catch(reason){reject(reason);}
      },reason=>{turn.controller.signal.removeEventListener('abort',stop);reject(reason);});
    });
  }

  function connectForTurn(turn:ActiveTurn,target:AgentTarget,current:Snapshot):Promise<AgentConnection> {
    const scope=JSON.stringify([current.profile?.id,current.profileEpoch,target,current.settings.cwd]);
    let launch=pendingLaunch.current;
    if(!launch || launch.scope!==scope){
      launch={scope,promise:api.connectAgent(target,current.settings.cwd)};
      pendingLaunch.current=launch;setLaunchPending(true);
      const request=launch;
      const settled=()=>{if(pendingLaunch.current===request){pendingLaunch.current=undefined;setLaunchPending(false);}};
      // Stop detaches this turn; another turn can still use the same launch.
      void request.promise.then(settled,settled);
    }
    return waitForTurn(turn,launch.promise);
  }

  async function stopTurn() {
    const turn = activeTurn.current;
    if (!turn || turn.cancelled) return;
    turn.cancelled = true;
    turn.controller.abort();
    setPreparing(t('Interruption…'));
    try { await api.cancelPrompt(turn.conversationId || ''); }
    catch (reason) { if (activeTurn.current === turn) setError(errorMessage(reason)); }
  }

  async function ensureConversation(turn: ActiveTurn, resume?: Conversation, allowNewForStudy = false): Promise<Conversation> {
    let current = stateRef.current;
    if (current.agentStatus?.authentication === 'none') { setPage('agents'); throw new Error('Connexion au harness requise'); }
    if (!current.tutorStatus.connected) throw new Error('Connectez Tutor pour envoyer votre message. Il reste enregistré « À envoyer ».');
    // An existing conversation needs no catalog read. The native send path
    // validates its principal, training and live runtime before submitting.
    let info = overviewBinding === reviewScope(current, null) ? overviewValue : null;
    if (!resume || (current.settings.tutor.mode === 'remote' && info?.capabilities?.canEnableLearning === true)) {
      setPreparing(t('Je consulte votre progression…'));
      if (!info) { info = await api.tutorOverview(); checkTurn(turn); }
      if (current.settings.tutor.mode === 'remote' && info.capabilities?.canEnableLearning === true) {
        current = await api.enableLearning(); checkTurn(turn);
        setState(current); stateRef.current = current; setConnection(null);
        info = await api.tutorOverview(); checkTurn(turn);
      }
      setOverview(info); setOverviewBinding(reviewScope(current, null));
    }
    if (current.settings.tutor.mode === 'remote' && current.settings.tutor.activity !== 'auto') {
      const settings = {...current.settings,tutor:{...current.settings.tutor,activity:'auto' as const}};
      await api.saveSettings(settings); checkTurn(turn);
      current=await api.connectTutor(settings.tutor.url,false); checkTurn(turn);
      setState(current);stateRef.current=current;setConnection(null);
    }
    if (!current.agentConnected) {
      let selected = current.settings.manualTarget || targets.find(target => target.id === current.settings.targetId);
      if (!selected) { const found = await waitForTurn(turn,api.discoverAgents()); checkTurn(turn); setTargets(found); selected = found.find(target => target.id === current.settings.targetId); }
      if (!selected) throw new Error('Choisissez votre agent pour envoyer le message conservé.');
      setPreparing(t('Je prépare votre agent…'));
      const connected=await connectForTurn(turn,selected,current); checkTurn(turn);setConnection(connected);
    }
    if (resume && !allowNewForStudy) return resume;
    setPreparing(t('Je retrouve votre conversation…'));
    let next: Conversation;
    try { next = await api.startConversation(undefined, resume?.id); }
    catch (reason) {
      checkTurn(turn);
      // An explicit study launch may follow an institution change. Its new
      // conversation must not inherit another institution's messages.
      if (!allowNewForStudy || !resume || errorMessage(reason) !== 'Cette séance appartient à une autre connexion Tutor. Rétablissez le compte et l’institution d’origine.') throw reason;
      turn.conversationId=undefined;
      next = await api.startConversation();
      turn.conversationId=next.id; checkTurn(turn);
      const queued = stateRef.current.pendingMessage;
      if (queued) { await api.queuePrompt(queued.text, next.id); checkTurn(turn); }
    }
    turn.conversationId=next.id; checkTurn(turn);
    setActiveId(next.id); setNewConversation(false);
    const snapshot = await api.loadState(); checkTurn(turn); setState(snapshot); stateRef.current = snapshot;
    return next;
  }

  async function dispatch(value: string, resume?: Conversation, alreadyQueued = false): Promise<boolean> {
    if (sending.current || !value.trim() || (stateRef.current.pendingMessage && !alreadyQueued)) return false;
    const turn:ActiveTurn={cancelled:false,controller:new AbortController(),conversationId:resume?.id,text:value};
    const startedAt=performance.now();
    const previousAssistantIds=new Set<string>();
    let promptCompleted=false;
    activeTurn.current=turn;setTurnStartedAt(startedAt);setResponseRender(undefined);
    let accepted = alreadyQueued;
    sending.current = true; setError(''); setNotice(''); setTurnError(undefined); setTurnLimit(undefined); setProviderStatus(''); setPreparing(t('Je prépare votre séance…'));
    setStream('');setWriting(false);setTools([]);toolEvents.current=[];setPage('conversation');
    try {
      if (!alreadyQueued) {
        const next = await api.queuePrompt(value.trim(), resume?.id);
        accepted = true; checkTurn(turn);setState(next);stateRef.current=next;
      }
      checkTurn(turn); setDraft(''); setAttachments([]);
      if (!stateRef.current.tutorStatus.connected) { setPage('agents'); setNotice(t('Votre message est conservé « À envoyer ». Terminez la connexion pour reprendre.')); return accepted; }
      const request = parseStudyRequest(value);
      const resumeRequest = parseResumeRequest(value);
      const next = await ensureConversation(turn,resume, !!request || !!resumeRequest); checkTurn(turn);
      turn.conversationId=next.id;
      if (request?.trainingId) {
        setPreparing(t('Je retrouve votre formation…'));
        const confirmed = await selectStudyTraining(api, next.id, request, () => checkTurn(turn)); checkTurn(turn);
        const snapshot = {...stateRef.current, conversations: stateRef.current.conversations.map(item => item.id === next.id ? {...item, activeTraining: confirmed} : item)};
        stateRef.current = snapshot; setState(snapshot);
      }
      checkTurn(turn);
      for (const message of next.messages) if (message.role === 'assistant') previousAssistantIds.add(message.id);
      turn.sessions = {conversationId:next.id};
      pendingRef.current = next.id; setPending(next.id); setPreparing('');
      setState(previous => ({...previous,conversations:previous.conversations.map(item=>item.id===next.id?{...item,messages:[...item.messages,{id:crypto.randomUUID(),role:'user',text:value.trim(),status:'complete',createdAt:new Date().toISOString()}]}:item)}));
      turn.promptSent=true;
      const result = await api.sendPrompt(next.id, value.trim());
      promptCompleted = result.stopReason !== 'cancelled';
      if (activeTurn.current === turn && result.stopReason === 'cancelled') setNotice(t('Réponse interrompue. Vos échanges et votre progression sont conservés.'));
      // The same notice as the turn_limit event, which may arrive before or after this result.
      const reached = turnLimitFromResult(next.id, result);
      if (reached && activeTurn.current === turn && !turn.cancelled) setTurnLimit(previous => previous?.conversationId === next.id ? previous : reached);
    } catch (reason) {
      if (activeTurn.current === turn) {
        if (turn.cancelled || reason === cancelledTurn) {
          // queue/start may have completed after Stop. Pause the durable outbox
          // again so a future configuration transition cannot resend it.
          try { await api.cancelPrompt(turn.conversationId || ''); } catch (error) {setError(errorMessage(error));}
          setNotice(t('Envoi interrompu. Le message conservé attend votre décision.'));
        } else if (turn.promptSent) {
          // The model turn itself failed: keep the error with that turn.
          setTurnError({conversationId:turn.conversationId, message:failureText(reason), retryText:value});
        } else setError(errorMessage(reason));
      }
    } finally {
      if (activeTurn.current === turn) {
        try {
          const next = await api.loadState();
          if (activeTurn.current === turn) {
            setState(next);stateRef.current=next;
            const completed = next.conversations.find(item=>item.id===turn.conversationId)?.messages.at(-1);
            if (completed?.role === 'assistant' && !previousAssistantIds.has(completed.id))
              setTurnLimit(previous => previous && previous.conversationId === turn.conversationId && !previous.messageId ? {...previous,messageId:completed.id} : previous);
            if (promptCompleted && !turn.cancelled && turn.conversationId && completed?.role==='assistant' && completed.status==='complete'
              && !completed.privateOutput && !completed.canRetryCommit && !previousAssistantIds.has(completed.id)) {
              setResponseRender({conversationId:turn.conversationId,assistantMessageId:completed.id,startedAt,operationId:turn.promptOperationId});
            }
          }
        } catch (reason) { if(activeTurn.current===turn)setError(errorMessage(reason)); }
        if (activeTurn.current === turn) {
          activeTurn.current=undefined;setTurnStartedAt(undefined);
          pendingRef.current = undefined; setPending(undefined); setStream(''); setWriting(false); setProviderStatus(''); setPermissions([]); sending.current = false; setPreparing('');
        }
      }
    }
    return accepted;
  }

  function sendDraft() {
    // Only a recognized slash command leaves the answer lane: answers such as
    // `/^a+$/` or `/etc/hosts` are still graded exercise answers.
    const command = parseSlashCommand(draft);
    if (pendingExercise && composerTarget === 'answer' && !command) { void answerExercise(pendingExercise); return; }
    // A question does not change the default: the next message answers the exercise.
    if (pendingExercise) setComposerTarget('answer');
    const value = command ? studyRequestText({version:1,format:command.format,intent:'practice',trainingId:activeTraining?.training_id,trainingName:activeTraining?.name,domainId:activeTraining?.domain_id},command.topic) : draft;
    try {void dispatch(withAttachments(value,attachments),conversation);} catch(reason) {setError(errorMessage(reason));}
  }

  // The main composer is the only exercise input. Its answer is serialized as
  // the same tutor-response block the native layer already validates.
  async function answerExercise(activity: ExerciseActivity) {
    if (attachments.length) { setError(t('Une réponse d’exercice ne peut pas contenir de pièce jointe. Retirez les fichiers ou passez en « Question au tuteur ».')); return; }
    const answer = draft.trim();
    if (!answer || answer.includes('\0') || new TextEncoder().encode(answer).length > 12000) { setError(t('Votre réponse doit contenir du texte, sans caractère nul, dans la limite de 12 000 octets.')); return; }
    const shown = exerciseShownAt.current?.id === activity.id ? exerciseShownAt.current.at : Date.now();
    const response: LearningResponse = { version:1, kind:'exercise', activityId:activity.id, trainingId:activity.trainingId, domainId:activity.domainId,
      concept:activity.concept, attemptId:activity.attemptId, answer, responseTimeSeconds:Math.min(86400, Math.max(0, (Date.now() - shown) / 1000)) };
    try { if (!await respondToActivity(activity, response)) setError(t('Votre réponse n’a pas pu être envoyée. Réessayez.')); }
    catch (reason) { setError(errorMessage(reason)); }
  }

  function requestHint(activity: LearningActivity) {
    void dispatch(t('Donne-moi un indice pour cette question, sans révéler la réponse : {question}',{question:activity.question}),conversation);
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
      : response.kind === 'exercise' ? t('Ma réponse : {answer}', {answer:response.answer})
      : t('Mon autoévaluation : {rating}', {rating:t(response.kind === 'flashcard' && response.rating === 'again' ? 'Pas retrouvé' : response.kind === 'flashcard' && response.rating === 'hard' ? 'Retrouvé avec effort' : 'Retrouvé facilement')});
    return dispatch(`${summary}\n\n${serializeLearningResponse(response)}`, conversation);
  }

  function continueActivity(activity: LearningActivity) {
    if (activity.kind === 'exercise') {
      void dispatch(resumeRequestText({version:1,trainingId:activity.trainingId,domainId:activity.domainId,trainingName:activeTraining?.name}),conversation);
      return;
    }
    const lastRequest = [...(conversation?.messages || [])].reverse().filter(item => item.role === 'user').map(item => parseStudyRequest(item.text)).find(Boolean);
    void dispatch(studyRequestText({version:1, format:activity.kind, intent:lastRequest?.intent || 'practice', trainingId:activity.trainingId, domainId:activity.domainId, trainingName:activeTraining?.name}), conversation);
  }

  // A queued request resumes only on a successful configuration transition.
  // Native persistence consumes the outbox atomically with the new user turn.
  const previousReady = useRef(false);
  useEffect(() => {
    const ready = state.tutorStatus.connected && agentReady;
    const transitioned = ready && !previousReady.current; previousReady.current = ready;
    if (transitioned && !setupOpenRef.current && state.pendingMessage && !state.pendingMessage.paused && !sending.current) {
      const queued = state.pendingMessage;
      const resume = queued.conversationId ? state.conversations.find(item => item.id === queued.conversationId) : undefined;
      void dispatch(queued.text, resume, true);
    }
  }, [state.tutorStatus.connected, agentReady, state.pendingMessage]);

  async function learn(training: { id: string; name: string; domain_id?: string }) {
    const message = training.domain_id ? resumeRequestText({version:1,trainingId:training.id,trainingName:training.name,domainId:training.domain_id}) : t('Présente-moi la formation « {name} » ({id}) et ses conditions d’inscription.',{name:training.name,id:training.id});
    setTrainingId(undefined); await dispatch(message, conversation);
  }

  async function discuss(id: string) {
    await dispatch(t('Je souhaite modifier le programme de la formation {id}. Lis son état réel avec get_training, puis prépare une proposition ciblée avec edit_training(action=preview) puis edit_training(action=propose). Je l’examinerai et la retoucherai dans le même éditeur avant validation. Demande-moi le changement souhaité si je ne l’ai pas encore précisé.',{id}), conversation);
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
  function retryOverview() { setError(''); setOverviewAttempt(value => value + 1); }
  useEffect(() => {
    const onKey = (event:KeyboardEvent) => {
      if (setupOpenRef.current || event.isComposing || event.altKey || !(event.ctrlKey || event.metaKey)) return;
      if (event.key.toLowerCase() === 'k') {event.preventDefault();setPaletteOpen(value=>!value);}
      if (event.key.toLowerCase() === 'n') {event.preventDefault();if(!locked && !state.pendingMessage)void openNewConversation();}
    };
    window.addEventListener('keydown',onKey);return()=>window.removeEventListener('keydown',onKey);
  },[locked,state.pendingMessage]);
  useEffect(() => {setTrainingPanel(false);sidebar.closeOverlay();},[page,activeId]);
  useEffect(() => {
    if (!pendingExercise) { exerciseShownAt.current = undefined; return; }
    if (exerciseShownAt.current?.id !== pendingExercise.id) { exerciseShownAt.current = { id: pendingExercise.id, at: Date.now() }; setComposerTarget('answer'); }
  }, [pendingExercise?.id]);
  useEffect(() => { if (!loading && !loadError && setupStep !== 'done' && !setupSession) setSetupSession('auto'); }, [loading, loadError, setupStep, setupSession]);
  async function retryLoad() {
    setLoadError(''); setLoading(true);
    try { const next = await api.loadState(); stateRef.current = next; setState(next); setActiveId(next.activeConversationId); }
    catch (reason) { setLoadError(errorMessage(reason)); }
    finally { setLoading(false); }
  }
  async function chooseTraining(training:Training) {
    if(locked || state.pendingMessage)return;
    const turn:ActiveTurn={cancelled:false,controller:new AbortController(),conversationId:conversation?.id};activeTurn.current=turn;
    try {
      await perform('Sélection de la formation…',async()=>{
        const current=await ensureConversation(turn,conversation,true);
        const selected=await selectStudyTraining(api,current.id,{version:1,format:'quiz',intent:'practice',trainingId:training.id,domainId:training.domain_id},()=>checkTurn(turn));
        const next=await api.loadState();checkTurn(turn);
        setState({...next,conversations:next.conversations.map(item=>item.id===current.id?{...item,activeTraining:selected}:item)});
      });
    } finally {if(activeTurn.current===turn){activeTurn.current=undefined;setPreparing('');}}
  }

  async function openNewConversation(){await perform('Nouvelle conversation…', async () => { await api.selectConversation(); setNewConversation(true); setActiveId(undefined); setPage('conversation'); setTools([]); setDraft(''); setAttachments([]); });}
  async function openConversation(id:string){await perform('Lecture de la conversation…', async () => { const next = await api.selectConversation(id); setState(next); setActiveId(id); setNewConversation(false); setPage('conversation'); setDraft(''); setAttachments([]); setTools([]); });}
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
  const localModelReady = useCallback(async () => {
    const bundled = await api.bundledAgents();
    setTargets(previous => {
      const refreshed = new Map(previous.map(target => [target.id, target]));
      for (const target of bundled) refreshed.set(target.id, target);
      return [...refreshed.values()];
    });
  }, [api]);
  function connections(section:'account'|'agents'|'institution',sectionBusy=locked){return <Connections key={`${state.profile?.id}:${section}`} section={section} settings={state.settings} targets={targets} connected={state.agentConnected} connecting={autoConnecting || launchPending} agentStatus={state.agentStatus} tutorStatus={state.tutorStatus} connection={state.agentConnected ? state.agentConnection ?? connection : null} identity={state.identity} api={api} busy={sectionBusy} turnLockReason={turnLockText}
    onIdentityAction={(label,action)=>perform(label,async()=>{await action();saved(await api.loadState());return true;}).then(result=>result===true)}
    onDiscover={()=>void perform('Recherche des installations…',async()=>setTargets(await api.discoverAgents()))}
    onSave={settings=>void perform('Enregistrement des réglages…',async()=>saved(await api.saveSettings(settings)))}
    onConnect={(target,settings)=>void perform('Connexion à votre agent…',async()=>{const restoreLocal=stateRef.current.tutorStatus.connected && settings.tutor.mode==='local';agentPaused.current=false;setAgentLoss(undefined);await api.saveSettings(settings);setConnection(await api.connectAgent(target,settings.cwd));try{if(restoreLocal)await api.tutorOverview();}finally{saved(await api.loadState());}})}
    onAuthenticate={method=>void perform('Identification auprès du fournisseur…',async()=>{await api.authenticateAgent(method);saved(await api.loadState());})}
    onDisconnectAgent={()=>{if(activeTurn.current){setError(t(turnLockReason));return;}void perform('Déconnexion…',async()=>{agentPaused.current=true;setAgentLoss(undefined);await api.disconnectAgent();setConnection(null);saved(await api.loadState());});}}
    onConnectTutor={(settings,authoring)=>void configureTutor(settings,authoring)}
    onDisconnectTutor={()=>{if(activeTurn.current){setError(t(turnLockReason));return;}void perform('Déconnexion de Tutor…',async()=>{agentPaused.current=true;saved(await api.disconnectTutor());});}}/>;}
  if (loading) return <div className="app-shell loading-shell"><WindowChrome /><div className="loading-screen" role="status"><span className="spinner" />{t("Ouverture de Tutor…")}</div></div>;
  if (loadError) return <div className="app-shell loading-shell"><WindowChrome /><div className="load-error" role="alert"><h1>{t('Tutor n’a pas pu ouvrir votre espace.')}</h1><p>{t(loadError)}</p><button className="button primary" onClick={() => void retryLoad()}>{t('Réessayer')}</button></div></div>;
  if (setupOpen) return <Onboarding key={state.profile?.id || 'profile'} api={api} snapshot={state} review={setupSession === 'review'} targets={targets}
    onSnapshot={saved} onTargets={found => setTargets(previous => [...new Map([...previous, ...found].map(target => [target.id, target])).values()])}
    onFinished={() => { agentPaused.current = false; launchedScope.current = undefined; setSetupSession(false); setPage('conversation'); }}
    onExit={() => setSetupSession(false)}
    onProfile={next => { applyProfile(next); setSetupSession(onboardingStep(next) === 'done' ? false : 'auto'); }}/>;
  return <div className={`app-shell ${sidebar.open ? '' : 'sidebar-collapsed'}`} data-reading-size={appearance.fontSize} data-reading-line-height={appearance.lineHeight}>
    <WindowChrome sidebarOpen={sidebar.open} onToggleSidebar={sidebar.toggle}/>
    <a className="skip-link" href="#main-content">{t("Aller au contenu principal")}</a>
    {sidebar.overlay && <button className="sidebar-scrim" aria-label={t('Fermer la barre latérale')} tabIndex={-1} onClick={sidebar.closeOverlay}/>}
    <WorkspaceNavigation state={state} page={page} settingsPage={settingsPage} conversation={conversation} trainings={library.items} libraryError={library.error || (state.tutorStatus.connected && !overview && overviewFailure)} libraryLoading={library.loading || (state.tutorStatus.connected && !overview && !overviewFailure)} onRetryLibrary={overview ? library.retry : retryOverview} locked={locked || !!state.pendingMessage} sidebarOpen={sidebar.open}
      onNavigate={value=>{setPage(value);if(value==='formations')setTrainingId(undefined);sidebar.closeOverlay();}}
      onNew={()=>void openNewConversation()} onConversation={id=>void openConversation(id)} onTraining={id=>{setTrainingId(id);setPage('formations');}}
      onSearch={()=>setPaletteOpen(true)} onPractice={()=>{setPage('conversation');setPracticeMenu(value=>value+1);}}/>
    <CommandPalette open={paletteOpen} commands={commands} onClose={() => setPaletteOpen(false)} />
    <main id="main-content" className={`main-shell ${page === 'conversation' ? 'conversation-main' : ''}`} tabIndex={-1}>
      <header className={`workspace-heading ${page === 'conversation' && home ? 'home-heading' : ''}`}>
        {page === 'conversation' ? <>
          {!home && <h1>{conversation?.title || t('Nouvelle conversation')}</h1>}
          {!home && activeTraining && <button className="training-chip" onClick={()=>setTrainingPanel(value=>!value)}><TrainingColor name={activeTraining.name}/><span>{activeTraining.name}</span></button>}
          <div className="workspace-heading-actions">
            {!home && <button className="workspace-icon" aria-label={t('Partager la conversation')} title={t('Partager')} onClick={()=>setShareOpen(true)}><Icon name="share" size={18}/></button>}
            <button className="workspace-icon" aria-label={t('Ouvrir le panneau de la formation')} title={t('Ouvrir le panneau de la formation')} aria-expanded={trainingPanel} onClick={()=>setTrainingPanel(value=>!value)}><Icon name="panel" size={18}/></button>
          </div>
        </> : <h1>{t(settingsPage?'Paramètres':labels[page])}</h1>}
      </header>
      {/* Messages are translated when they are set; never translate them twice. */}
      {error && <Notice kind="error" onDismiss={() => setError('')}>{error}</Notice>}
      {notice && <Notice kind="info" onDismiss={() => setNotice('')}>{notice}</Notice>}
      {busy && <div className="notice" role="status"><span className="spinner" />{busy}</div>}
      {launchPending && turnStartedAt===undefined && <div className="notice" role="status"><span className="spinner" />{t('La connexion à l’agent continue en arrière-plan.')}</div>}
      {state.pendingMessage && !pending && <section className="outbox" aria-label={t("Message à envoyer")}><div><strong>{t("À envoyer")}</strong><p>{visibleStudyText(state.pendingMessage.text)}</p></div><div className="button-row"><button className="button secondary" disabled={locked} onClick={() => void perform('Retrait du message…', async () => { if (!structuredOutbox) {setDraft(visibleStudyText(state.pendingMessage!.text));setAttachments(messageAttachments(state.pendingMessage!.text));} setState(await api.queuePrompt(null)); setPage('conversation'); })}>{structuredOutbox ? t("Retirer") : t("Modifier")}</button><button className="button primary" disabled={locked || (!!limit && state.tutorStatus.connected && agentReady)} aria-describedby={limit && state.tutorStatus.connected && agentReady ? 'outbox-blocked-reason' : undefined} onClick={() => { if (!state.tutorStatus.connected) setPage('agents'); else if (!agentReady) setPage('agents'); else void dispatch(state.pendingMessage!.text, state.conversations.find(item => item.id === state.pendingMessage!.conversationId), true); }}>{state.tutorStatus.connected && agentReady ? t("Envoyer le message conservé") : t("Terminer la connexion")}</button>{limit && <span id="outbox-blocked-reason" className="visually-hidden">{sendBlockedReason}</span>}</div></section>}
      {page === 'conversation' && <section className="conversation-panel" aria-label={t("Conversation avec votre tuteur")}>
        <ConversationMessages key={`${state.profile?.id}:${conversation?.id}`} conversation={conversation} responseRender={responseRender} streamText={stream} streaming={!!pending} toolActivity={tools} turnError={turnError?.conversationId === conversation?.id ? turnError : undefined} onRetryTurn={value=>void dispatch(value,conversation)} turnLimit={turnLimit && turnLimit.conversationId === conversation?.id ? turnLimit : undefined} onContinueTurn={()=>void dispatch(t(turnLimitFollowUp),conversation)} activity={turnStartedAt !== undefined ? <TutorActivity startedAt={turnStartedAt} tools={tools} streaming={writing} permission={!!permissions.length} preparing={preparing} providerStatus={providerStatus}/> : undefined} disabled={locked || !!state.pendingMessage || !!limit} pendingText={state.pendingMessage?.conversationId === conversation?.id ? state.pendingMessage?.text : undefined} openLink={api.openExternalLink} onDelivery={api.acknowledgeLearningDelivery} onRetryCommit={value=>void dispatch(value,conversation)} onRespond={respondToActivity} onContinue={continueActivity} onResumeChoice={request=>void dispatch(resumeRequestText(request),conversation)} onHint={requestHint} />
        {permissions.map((permission,index) => <PermissionCard key={JSON.stringify(permission.id)} permission={permission} disabled={permissionBusy || index > 0} onAnswer={optionId => void (async () => { setPermissionBusy(true); try { await api.answerPermission(permission.id,optionId); setPermissions(previous => previous.filter(item => JSON.stringify(item.id) !== JSON.stringify(permission.id))); } catch (reason) { setError(errorMessage(reason)); } finally { setPermissionBusy(false); } })()} />)}
        <div className="conversation-compose">
        <HarnessStatus connected={state.agentConnected} reconnect={reconnect} limit={limit} loss={agentLoss} busy={locked} blockedReason={turnLockText} onReconnect={()=>void reconnectAgent()}/>
        {home && !resumeHidden && resumeTraining && <ResumeTraining training={resumeTraining} disabled={locked || !!state.pendingMessage} onResume={()=>void learn(resumeTraining)} onDismiss={()=>setResumeHidden(true)}/>}
        <StudyComposer key={`${state.profile?.id}:${newConversation?'new':conversation?.id}`} attachments={attachments} onAttachments={setAttachments} home={home} trainingName={activeTraining?.name} trainings={library.items} onChooseTraining={item=>void chooseTraining(item)} onOpenLibrary={()=>{setTrainingId(undefined);setPage('formations');}} mode={currentActivity?.type==='activity'?currentActivity.activity.kind:undefined} menuRequest={practiceMenu}
          exercise={pendingExercise ? {target:composerTarget,onTarget:setComposerTarget,onHint:()=>requestHint(pendingExercise),hintDisabled:locked || !!state.pendingMessage} : undefined} draft={draft} onChange={setDraft} disabled={turnStartedAt !== undefined || !!state.pendingMessage} canSend={!locked && (!!draft.trim() || !!attachments.length) && !state.pendingMessage} sendBlockedReason={sendBlockedReason} pending={turnStartedAt !== undefined} onSend={sendDraft} onCancel={() => void stopTurn()} />
        {home && <p className="composer-disclaimer">{t('L’IA peut se tromper. Vérifiez les points importants.')}</p>}
        </div>
      </section>}
      {page === 'planning' && (state.tutorStatus.connected && !overview ? <div className="page-content">{overviewFailure ? <LoadError message="Le planning est momentanément indisponible." busy={locked} onRetry={retryOverview}/> : <LoadingState label="Lecture du planning…"/>}</div> : <ReviewPlanning key={planningScope} api={api} scope={planningScope} preferences={state.settings.reviewPlans || {}} connected={state.tutorStatus.connected} busy={locked || !!state.pendingMessage} refreshKey={libraryEpoch} onPreference={saveReviewPreference} onStart={startReview}/>)}
      {page === 'formations' && <TrainingWorkspace key={`${state.profile?.id}:${state.settings.tutor.url}:${overview?.identity?.tenantId}:${state.tutorStatus.connected}`} api={api} connected={state.tutorStatus.connected} epoch={libraryEpoch} initialId={trainingId} canCreate={overview?.identity?.roles?.includes('owner') === true} busy={locked} onLearn={learn} onDiscuss={discuss} onConnect={() => setPage('agents')} />}
      {page==='account'&&<div className="settings-content"><AccountMenu key={state.profile?.id} snapshot={state} overview={overview} busy={locked} api={api} onSelect={id=>void perform('Changement de compte…',async()=>applyProfile(await api.selectProfile(id)))} onCreate={label=>void perform('Création du compte…',async()=>{applyProfile(await api.createProfile(label));await api.createIdentity();saved(await api.loadState());setPage('account');setSetupSession('auto');})} onRename={label=>void perform('Enregistrement du nom…',async()=>saved(await api.renameProfile(label)))} onDelete={id=>void perform('Retrait du compte…',async()=>{applyProfile(await api.deleteProfile(id,id));setPage('account');})}/>{connections('account')}</div>}
      {settingsPage&&<div className="settings-content"><SettingsWorkspace key={state.profile?.id} api={api} snapshot={state} overview={overview} busy={locked} turnLockReason={turnLockText} tab={page==='settings'?'agents':page as SettingsTab} onTabChange={setPage} renderConnection={connections} onSaved={saved} onLocalModelReady={localModelReady} onRestartSetup={locked ? undefined : () => setSetupSession('review')}/></div>}

      {shareOpen && conversation && <ShareConversation conversation={conversation} onClose={()=>setShareOpen(false)}/>}
      {trainingPanel && page === 'conversation' && <WorkspaceTrainingPanel api={api} id={activeTraining?.training_id} scope={planningScope} onClose={()=>setTrainingPanel(false)} onOpenLibrary={()=>{setTrainingId(activeTraining?.training_id);setPage('formations');}}/>}
    </main>
  </div>;
}

function Notice({ children,kind,onDismiss }: { children: ReactNode;kind:'error'|'info';onDismiss:()=>void }) { return <div className={`notice ${kind}`} role={kind === 'error' ? 'alert' : 'status'}><div>{children}</div><button className="icon-button" aria-label={t("Fermer le message")} onClick={onDismiss}><Icon name="close" size={16} /></button></div>; }

function ConversationMessages({ conversation,streamText,streaming,activity,toolActivity,disabled,openLink,onDelivery,onRetryCommit,onRespond,onContinue,onHint,onResumeChoice,responseRender,turnError,onRetryTurn,turnLimit,onContinueTurn }: {
  turnError?:TurnError;onRetryTurn:(text:string)=>void;turnLimit?:TurnLimit;onContinueTurn:()=>void;
  responseRender?:PendingResponseRender;conversation?: Conversation;streamText:string;streaming:boolean;activity?:ReactNode;toolActivity:ToolActivity[];disabled:boolean;pendingText?:string;openLink:ClientBridge['openExternalLink'];
  onDelivery:ClientBridge['acknowledgeLearningDelivery'];onRetryCommit:(text:string)=>void;onResumeChoice:(request:ResumeRequest)=>void;
  onRespond:(activity:LearningActivity,response:LearningResponse)=>Promise<boolean>;onContinue:(activity:LearningActivity)=>void;onHint:(activity:LearningActivity)=>void;
}) {
  const end = useRef<HTMLDivElement>(null); const nearEnd = useRef(true); const [unread,setUnread] = useState(false);
  const scroller = useRef<HTMLDivElement>(null);
  // A window resize or a taller composer shrinks this region without any scroll
  // event: a reader who was at the end stays there, so the latest card is not
  // left hidden behind the composer.
  useEffect(() => {
    const element = scroller.current;
    if (!element || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => { if (nearEnd.current) element.scrollTop = element.scrollHeight; });
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  const shownActivity = useRef<string | undefined>(undefined);
  // Only a short notice is announced when a response ends, never the response itself.
  const [announcement,setAnnouncement] = useState('');
  const wasStreaming = useRef(streaming);
  const messages = conversation?.messages || [];
  const lastMessage = messages.at(-1);
  useEffect(() => {
    if (streaming) setAnnouncement('');
    else if (wasStreaming.current && lastMessage?.role === 'assistant' && lastMessage.status === 'complete') setAnnouncement(t('Réponse terminée'));
    wasStreaming.current = streaming;
  }, [streaming]);
  const responses = messages.filter((message,index) => message.role === 'user' && message.status === 'complete' && messages[index+1]?.role === 'assistant' && messages[index+1]?.status === 'complete' && !messages[index+1]?.privateOutput).map(message => parseLearningResponse(message.text));
  const allActivities = [...messages].reverse().filter(message => message.role === 'assistant' && message.status === 'complete' && !message.privateOutput)
    .flatMap(message => parseLearningContent(message.text, {complete:true}).filter(segment => segment.type === 'activity').map(segment => ({messageId:message.id, activity:segment.activity})));
  const lastActivity = allActivities[0];
  const lastActivityIndex = messages.findIndex(message => message.id === lastActivity?.messageId);
  // The limit notice belongs to its assistant message; before that message is known, it follows the conversation.
  const limitMessage = turnLimit?.messageId ? messages.find(message => message.id === turnLimit.messageId) : undefined;
  const limitNotice = turnLimit && <TurnLimitNotice limit={turnLimit.limit} disabled={disabled || streaming} onContinue={onContinueTurn}/>;
  const continued = lastActivityIndex >= 0 && messages.slice(lastActivityIndex + 1).some(message => message.role === 'user' && (parseStudyRequest(message.text) || parseResumeRequest(message.text)));
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
  },[conversation?.id,messages.length,streamText,streaming,!!activity,lastActivity?.messageId]);
  function assistantContent(value: string, complete: boolean, messageId?: string, context?: ConversationContext) {
    return parseLearningContent(value, {complete}).map((segment, index) => {
      if (segment.type === 'text') return <MessageBody key={index} text={segment.text} openLink={openLink}/>;
      if (segment.type === 'pending') return <p key={index} className="learning-content-state" role="status">{t('Préparation de l’activité visuelle…')}</p>;
      if (segment.type === 'invalid') return <p key={index} className="learning-content-state" role="status">{t('Cette activité ne peut pas être affichée. Demandez au tuteur de la reformuler.')}</p>;
      if (segment.type === 'training-choice') {
        const messageIndex = messages.findIndex(message => message.id === messageId);
        const actionable = complete && messageIndex >= 0 && !messages.slice(messageIndex + 1).some(message => message.role === 'user');
        return <section key={index} className="learning-training-choice" aria-label={t('Choisissez la formation à reprendre.')}>
          <p>{t('Choisissez la formation à reprendre.')}</p>
          <div className="learning-training-choices">{segment.choice.choices.map(choice=><button key={choice.trainingId} className="workspace-outline" disabled={disabled || !actionable}
            onClick={()=>onResumeChoice({version:1,...choice})}>{choice.trainingName}</button>)}</div>
          {segment.choice.hasMore && <p className="learning-card-hint">{t('D’autres formations correspondent. Précisez le nom dans votre message si nécessaire.')}</p>}
        </section>;
      }
      const activity = segment.activity;
      if (allActivities.filter(item => item.activity.id === activity.id).length !== 1) return <p key={index} className="learning-content-state" role="status">{t('Cette activité ne peut pas être affichée. Demandez au tuteur de la reformuler.')}</p>;
      const response = responses.find(value => value && responseMatchesActivity(activity, value));
      const matchesContext = context?.training_id === activity.trainingId && context.domain_id === activity.domainId;
      const matchesActive = conversation?.activeTraining?.training_id === activity.trainingId && conversation.activeTraining.domain_id === activity.domainId && context?.revision === conversation.activeTraining.revision;
      const latest = lastActivity?.messageId === messageId && lastActivity.activity.id === activity.id;
      return <div key={`${index}:${activity.id}`}>
        <LearningActivityCard questionNumber={[...allActivities].reverse().filter(item=>item.activity.kind===activity.kind && item.activity.trainingId===activity.trainingId && item.activity.sessionId===activity.sessionId).findIndex(item=>item.activity.id===activity.id)+1} activity={activity} response={response} disabled={disabled || !matchesContext || !matchesActive || !latest}
          openLink={openLink} onReveal={messageId && conversation && messages.find(m=>m.id===messageId)?.learningDelivery?.kind === 'instruction' ? ()=>{void onDelivery(conversation.id,messageId).catch(()=>{});} : undefined} onHint={latest && matchesActive && matchesContext ? ()=>onHint(activity) : undefined} onRespond={value => onRespond(activity, value)} onContinue={latest && matchesActive && matchesContext && !continued ? () => onContinue(activity) : undefined}/>
        {(!matchesContext || !matchesActive) && <p className="learning-context-warning">{t('Cette activité appartient à une autre formation ou son contexte n’est pas confirmé.')}</p>}
      </div>;
    });
  }
  // A labelled, keyboard-scrollable region: it holds interactive activities,
  // so it is not a live log that would read every streamed fragment.
  return <div ref={scroller} className="conversation-messages" role="region" aria-label={t("Messages de la conversation")} tabIndex={0} onScroll={event => { const element = event.currentTarget; nearEnd.current = element.scrollHeight - element.scrollTop - element.clientHeight < 96; if (nearEnd.current) setUnread(false); }}>
    {!messages.length && !streamText && <div className="conversation-welcome"><span className="welcome-mark" aria-hidden="true"><span className="wm">T<span className="cursor still" /></span></span><h2>{t("Qu’avez-vous envie d’apprendre ?")}</h2></div>}
    {messages.map((message,index) => {
      const visible = message.privateOutput ? '' : message.status === 'streaming' && streamText.length > message.text.length ? streamText : message.text;
      const context: ConversationContext | undefined = message.trainingContext;
      const timing = responseRender?.conversationId === conversation?.id && responseRender?.assistantMessageId === message.id && message.status === 'complete' && !message.privateOutput ? responseRender : undefined;
      const renderedContent = timing ? parseLearningContent(visible,{complete:true}) : [];
      const validRender = renderedContent.some(segment=>segment.type !== 'text' || !!segment.text.trim()) && renderedContent.every(segment=>
        segment.type==='text' || segment.type==='training-choice' || (segment.type==='activity' && allActivities.filter(item=>item.activity.id===segment.activity.id).length===1
          && context?.training_id===segment.activity.trainingId && context.domain_id===segment.activity.domainId));
      const instructionContent = message.learningDelivery?.kind === 'instruction' ? parseLearningContent(visible,{complete:message.status==='complete'}) : [];
      const proseInstruction = instructionContent.some(segment=>segment.type==='text' && !!segment.text.trim()) && instructionContent.every(segment=>segment.type==='text');
      const changed = context?.training_id && context.training_id !== messages[index - 1]?.trainingContext?.training_id;
      return <article className={`message ${message.role}`} key={message.id}>{timing && validRender && <ResponseRendered timing={timing}/>} {conversation && message.status === 'complete' && !message.privateOutput && (message.learningDelivery?.kind === 'feedback' || proseInstruction) && !message.learningDelivery?.recorded && <DeliveryAcknowledgment conversationId={conversation.id} messageId={message.id} onDelivery={onDelivery}/>}{message.role === 'assistant' && index === messages.length - 1 && <ToolActivityLines tools={toolActivity}/>}{changed && <p className="training-turn">{context.name}</p>}<p className="message-label visually-hidden">{message.role === 'user' ? t("Vous") : message.role === 'assistant' ? t("Tutor") : t("Information")}</p><div className="message-text">{message.role === 'assistant' ? assistantContent(visible, message.status === 'complete', message.id, context) : <><MessageBody text={visibleStudyText(visible)} openLink={openLink}/>{messageAttachments(visible).map((file,i)=><details className="message-attachment" key={i}><summary><Icon name="book" size={14}/>{file.name}</summary><pre>{file.content}</pre></details>)}</>}{message.status === 'streaming' && <span className="streaming-cursor" aria-hidden="true" />}</div>{limitMessage?.id === message.id ? limitNotice : message.status === 'interrupted' && <p className="message-interrupted">{t("Réponse interrompue — les éléments affichés peuvent être incomplets.")}</p>}{(message.canRetryCommit || (message.privateOutput && ['error','interrupted'].includes(message.status))) && messages[index-1]?.role === 'user' && <button className="workspace-outline" disabled={disabled} onClick={()=>onRetryCommit(messages[index-1].text)}>{t(message.canRetryCommit ? 'Réessayer l’enregistrement' : 'Réessayer la réponse')}</button>}{message.status === 'error' && <p className="message-interrupted">{t("La réponse n’a pas été confirmée comme terminée.")}</p>}</article>;
    })}
    {!!toolActivity.length && messages.at(-1)?.role !== 'assistant' && <div className="conversation-activity"><ToolActivityLines tools={toolActivity}/></div>}
    {!!streamText && messages.at(-1)?.role !== 'assistant' && <article className="message assistant"><p className="message-label visually-hidden">{t("Tutor")}</p><div className="message-text">{assistantContent(streamText, false)}</div></article>}
    {turnLimit && !limitMessage && !streaming && limitNotice}
    {turnError && !streaming && <div className="turn-error"><p role="alert">{t('La réponse a échoué : {message}', {message:t(turnError.message)})}</p>{turnError.retryText && <button type="button" className="workspace-outline" disabled={disabled} onClick={()=>onRetryTurn(turnError.retryText)}>{t('Réessayer')}</button>}</div>}
    {activity && <div className="conversation-activity">{activity}</div>}
    <p className="visually-hidden" role="status">{announcement}</p>
    {unread && <button className="button secondary jump-to-latest" onClick={() => { nearEnd.current = true; setUnread(false); end.current?.scrollIntoView?.({block:'nearest'}); }}>{t("Aller aux derniers messages")}</button>}
    <div ref={end} />
  </div>;
}

/** A calm notice: the turn stopped at its action budget, and the learner may let the tutor continue. */
function TurnLimitNotice({limit,disabled,onContinue}:{limit:number;disabled:boolean;onContinue:()=>void}) {
  return <div className="turn-limit" role="status"><p>{t('Le tuteur a atteint la limite de {limit} actions pour cette réponse.',{limit})} {t('Vous pouvez lui demander de poursuivre.')}</p><button type="button" className="workspace-outline" disabled={disabled} onClick={onContinue}>{t('Continuer')}</button></div>;
}

function DeliveryAcknowledgment({conversationId,messageId,onDelivery}:{conversationId:string;messageId:string;onDelivery:ClientBridge['acknowledgeLearningDelivery']}) {
  const marker=useRef<HTMLSpanElement>(null);
  useEffect(()=>{
    const parent=marker.current?.parentElement;if(!parent||typeof IntersectionObserver==='undefined')return;
    let visible=false,done=false,busy=false;let timer:ReturnType<typeof setTimeout>|undefined;
    const deliver=()=>{if(!visible||done||busy||document.visibilityState!=='visible')return;busy=true;void onDelivery(conversationId,messageId).then(()=>{done=true;}).catch(()=>{timer=setTimeout(deliver,5000);}).finally(()=>{busy=false;});};
    const observer=new IntersectionObserver(entries=>{visible=entries.some(entry=>entry.isIntersecting);deliver();});
    observer.observe(parent);document.addEventListener('visibilitychange',deliver);
    return()=>{done=true;observer.disconnect();document.removeEventListener('visibilitychange',deliver);if(timer)clearTimeout(timer);};
  },[conversationId,messageId,onDelivery]);
  return <span ref={marker} aria-hidden="true"/>;
}

function PermissionCard({ permission,disabled,onAnswer }: { permission:PermissionRequest;disabled:boolean;onAnswer:(id:string|null)=>void }) {
  return <section className="permission-card" aria-label={t("Autorisation demandée")}><h2>{permission.title}</h2>{permission.description && <p>{permission.description}</p>}<p>{t("Cette action attend votre décision.")}</p><div className="button-row"><button className="button secondary" disabled={disabled} onClick={() => onAnswer(null)}>{t("Annuler la demande")}</button>{permission.options.map(option => <button className={`button ${option.kind.startsWith('allow') ? 'primary' : 'secondary'}`} key={option.optionId} disabled={disabled} onClick={() => onAnswer(option.optionId)}>{permissionLabel(option.kind,option.name)}</button>)}</div></section>;
}

export function TutorActivity({tools,streaming,permission,preparing,providerStatus='',startedAt}:{tools:ToolActivity[];streaming:boolean;permission:boolean;preparing:string;providerStatus?:string;startedAt?:number}){
 const mountedAt=useRef(performance.now());const [now,setNow]=useState(performance.now());
 useEffect(()=>{const timer=window.setInterval(()=>setNow(performance.now()),1000);return()=>window.clearInterval(timer);},[]);
 const observedTools=useRef(new Map<string,number>());
 for(const tool of tools)if(!observedTools.current.has(tool.id))observedTools.current.set(tool.id,tool.startedAt??performance.now());
 const finished=tools.filter(tool=>['completed','failed','cancelled'].includes(tool.status)).length;
 const active=[...tools].reverse().find(tool=>!['completed','failed','cancelled'].includes(tool.status));const latest=tools.at(-1);const name=(active?.title||'').toLowerCase();
 let message=preparing||t('J’attends la réponse de l’agent…');
 if(permission)message=t('Votre autorisation est attendue.');
 else if(providerStatus)message=providerStatus;
 else if(!preparing&&active){message=/pedagogical_turn|assessment|curriculum/.test(name)?t('Je prépare la prochaine activité…'):/progress|interaction|mastery|learner_state/.test(name)?t('Je consulte votre progression…'):/training|domain|formation|library/.test(name)?t('Je retrouve votre formation…'):t('Je consulte le serveur Tutor…');}
 else if(!preparing&&streaming)message=t('Je rédige la réponse…');else if(!preparing&&latest?.status==='failed')message=t('Un outil a rencontré une erreur. Le tuteur examine le résultat…');
 const phase=useRef({message,startedAt:performance.now()});if(phase.current.message!==message)phase.current={message,startedAt:performance.now()};
 const elapsed=(start:number)=>Math.max(0,Math.floor((now-start)/1000));
 const detail=active&&!permission&&!providerStatus&&!preparing?t('Outil : {seconds} s',{seconds:elapsed(active.startedAt??observedTools.current.get(active.id)??now)}):t('Étape : {seconds} s',{seconds:elapsed(phase.current.startedAt)});
 return <div className="tutor-activity" role="status" aria-live="polite" aria-atomic="true"><span className={`activity-mark ${permission?'waiting':''}`} aria-hidden="true"></span><div><p key={message} className="activity-text">{message}</p><small className="activity-elapsed" aria-hidden="true">{t('Total : {seconds} s',{seconds:elapsed(startedAt??mountedAt.current)})} · {detail}</small>{finished>0&&<small>{t('{count} appel(s) terminé(s)',{count:finished})}</small>}</div></div>;
}

/** The latest visible activity, when it is an unanswered exercise of the active training. */
function latestPendingExercise(conversation?: Conversation): ExerciseActivity | undefined {
  const messages = conversation?.messages || [];
  const active = conversation?.activeTraining;
  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index];
    if (message.role !== 'assistant' || message.status !== 'complete' || message.privateOutput) continue;
    const activity = parseLearningContent(message.text, {complete:true}).flatMap(segment => segment.type === 'activity' ? [segment.activity] : []).at(-1);
    if (!activity) continue;
    const context = message.trainingContext;
    if (activity.kind !== 'exercise' || !active || activity.trainingId !== active.training_id || activity.domainId !== active.domain_id
      || context?.training_id !== activity.trainingId || context.domain_id !== activity.domainId || context.revision !== active.revision) return;
    // An answer, or an explicit request for another activity, closes the exercise.
    const closed = messages.slice(index + 1).some(item => item.role === 'user' && (responseMatchesActivity(activity, parseLearningResponse(item.text)) || !!parseStudyRequest(item.text) || !!parseResumeRequest(item.text)));
    return closed ? undefined : activity;
  }
}

function latestReview(training:Training) {return training.progress?.map(item=>item.last_review || '').sort().at(-1) || '';}

function ToolActivityLines({tools}:{tools:ToolActivity[]}) {
  return <>{tools.map(tool=><ToolActivityLine key={tool.id} tool={tool}/>)}</>;
}
function ToolActivityLine({tool}:{tool:ToolActivity}) {
  const [expanded,setExpanded]=useState(false);
  const label=/profile|learner_context/.test(tool.title)?'Profil chargé':/training|formation|library|curriculum/.test(tool.title)?'Formation consultée':'Outil MCP';
  const duration=tool.startedAt!==undefined&&tool.completedAt!==undefined?Math.max(0,(tool.completedAt-tool.startedAt)/1000).toFixed(1):undefined;
  return <div className="tool-line"><button type="button" aria-expanded={expanded} onClick={()=>setExpanded(value=>!value)}><Icon name="chevron" size={12}/><span>{t(label)}</span><span className="tool-object">{tool.title}</span>{tool.status==='failed'&&<Icon name="close" size={14}/>}</button>{expanded&&<div className="tool-line-detail">{tool.title} · {t(tool.status==='completed'?'Terminé':tool.status==='failed'?'Échec':tool.status==='cancelled'?'Interrompu':'En cours')}{duration!==undefined&&<> · {t('Durée observée : {seconds} s',{seconds:duration})}</>}</div>}</div>;
}
