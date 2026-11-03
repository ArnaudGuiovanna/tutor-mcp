import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App from '../../src/App';
import type { ClientBridge } from '../../src/bridge';
import { trainingFixture } from './trainingFixture';
import { client, initialState, target, conversation } from './clientFixture';

async function ready(api: ClientBridge) {
  render(<App api={api} />);
  await screen.findByRole('heading', { name: 'Qu’avez-vous envie d’apprendre ?' });
}

describe('Apprendre depuis la conversation', () => {
  it('conserve le message en attente tant que le harness ne confirme pas son accès', async () => {
    const state=initialState(true);state.pendingMessage={text:'Bonjour'};state.agentStatus={phase:'running',authentication:'checking'};
    const fixture=client(state);await ready(fixture.api);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    act(()=>fixture.broadcast({...fixture.getState(),agentStatus:{phase:'running',authentication:'none'}}));
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    act(()=>fixture.broadcast({...fixture.getState(),agentStatus:{phase:'running',authentication:'authenticated'}}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
  });
  it('retrouve et lance le harness mémorisé avant MCP sans lancer de connexion fournisseur', async () => {
    const fixture=client();await ready(fixture.api);
    await waitFor(()=>expect(fixture.getState().agentConnected).toBe(true));
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(1);
    expect(fixture.api.connectAgent).toHaveBeenCalledWith(target,'/home/lea/apprendre');
    expect(fixture.getState().tutorStatus.connected).toBe(false);
    expect(fixture.api.authenticateAgent).not.toHaveBeenCalled();
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('rétablit le processus après une déconnexion involontaire, sans rejouer de message', async () => {
    const fixture=client(initialState(true));await ready(fixture.api);
    const disconnected=fixture.getState();disconnected.agentConnected=false;fixture.setState(disconnected);
    act(()=>fixture.emit({type:'connection',status:'closed',message:'Processus arrêté'}));
    await waitFor(()=>expect(fixture.getState().agentConnected).toBe(true));
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(1);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('button',{name:'Paramètres'}));
    await userEvent.click(screen.getByRole('button',{name:'Déconnecter Codex'}));
    await waitFor(()=>expect(fixture.getState().agentConnected).toBe(false));
    vi.useFakeTimers();
    try { await act(async()=>{await vi.advanceTimersByTimeAsync(10000);}); }
    finally { vi.useRealTimers(); }
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(1);
  });

  it('borne les tentatives de lancement après des échecs répétés', async () => {
    const fixture=client();vi.mocked(fixture.api.connectAgent).mockRejectedValue(new Error('Harness indisponible'));
    await ready(fixture.api);
    await waitFor(()=>expect(fixture.api.connectAgent).toHaveBeenCalledTimes(3), {timeout:6000});
    vi.useFakeTimers();
    try { for(let i=0;i<4;i++) await act(async()=>{await vi.advanceTimersByTimeAsync(4000);}); }
    finally { vi.useRealTimers(); }
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(3);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });
  it('ouvre directement une conversation et crée sans formulaire d’intention', async () => {
    const {api}=client(initialState(true));await ready(api);const user=userEvent.setup();
    await user.type(screen.getByLabelText('Message au tuteur'),'Je veux apprendre le Go');
    await user.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(api.sendPrompt).toHaveBeenCalledWith('conversation-1','Je veux apprendre le Go'));
    expect(api.startConversation).toHaveBeenCalledWith(undefined,undefined);
    expect(api.saveIntent).not.toHaveBeenCalled();
    expect(screen.queryByLabelText('Ce que je souhaite apprendre')).toBeNull();
  });

  it('conserve un message à envoyer pendant la configuration et le reprend une seule fois', async () => {
    const fixture=client();await ready(fixture.api);const user=userEvent.setup();
    await user.type(screen.getByLabelText('Message au tuteur'),'Je veux apprendre le Go');await user.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    expect(await screen.findByRole('region',{name:'Message à envoyer'})).toBeTruthy();
    expect(fixture.api.queuePrompt).toHaveBeenCalledWith('Je veux apprendre le Go',undefined);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    const connected=fixture.getState();connected.agentConnected=true;connected.tutorStatus.connected=true;
    act(()=>fixture.broadcast(connected));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    act(()=>fixture.broadcast(fixture.getState()));
    expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1);
    await waitFor(()=>expect(screen.queryByRole('region',{name:'Message à envoyer'})).toBeNull());
  });

  it('retrouve un message en attente après fermeture et permet de le modifier', async () => {
    const state=initialState();state.pendingMessage={text:'Apprendre Python'};const {api}=client(state);await ready(api);const user=userEvent.setup();
    await user.click(screen.getByRole('button',{name:'Modifier'}));
    await waitFor(()=>expect((screen.getByLabelText('Message au tuteur')as HTMLTextAreaElement).value).toBe('Apprendre Python'));
    expect(api.queuePrompt).toHaveBeenCalledWith(null);expect(api.sendPrompt).not.toHaveBeenCalled();
  });

  it.each([false,true])('active l’espace apprenant d’un propriétaire au premier message en conservant ses droits (reprise : %s)', async resume => {
    const state=initialState(true);state.settings.tutor.mode='remote';
    if(resume){state.conversations=[conversation];state.activeConversationId=conversation.id;}
    const fixture=client(state);
    vi.mocked(fixture.api.tutorOverview).mockResolvedValue({identity:{tenantId:'t',userId:'u',membershipId:'m',roles:['owner']},capabilities:{canEnableLearning:true},context:{},formations:{items:[]},availableFormations:{items:[]}});
    vi.mocked(fixture.api.enableLearning).mockImplementation(async()=>{const next=fixture.getState();next.agentConnected=false;fixture.setState(next);return next;});
    render(<App api={fixture.api}/>);await screen.findByLabelText('Message au tuteur');
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Je veux apprendre le Go');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));expect(fixture.api.enableLearning).toHaveBeenCalledTimes(1);
    expect(fixture.api.connectAgent).toHaveBeenCalled();
    if(resume)expect(fixture.api.startConversation).not.toHaveBeenCalled();else expect(fixture.api.startConversation).toHaveBeenCalledWith(undefined,undefined);
    expect(screen.queryByText('Mon activité dans l’institution')).toBeNull();
  });

  it.each([false,undefined])('n’active pas l’apprentissage sans capacité positive sur une reprise (capacité : %s)',async canEnableLearning=>{
    const state=initialState(true);state.settings.tutor.mode='remote';state.conversations=[conversation];state.activeConversationId=conversation.id;
    const fixture=client(state);
    vi.mocked(fixture.api.tutorOverview).mockResolvedValue({capabilities:{canEnableLearning},context:{},formations:{items:[]},availableFormations:{items:[]}});
    render(<App api={fixture.api}/>);await screen.findByText('Commençons par observer la Grande Ourse.');
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Continuons');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    expect(fixture.api.enableLearning).not.toHaveBeenCalled();expect(fixture.api.startConversation).not.toHaveBeenCalled();expect(fixture.api.tutorOverview).toHaveBeenCalledTimes(1);
  });

  it('n’utilise pas la capacité d’activation d’une ancienne institution pendant sa réactualisation',async()=>{
    const state=initialState(true);state.settings.tutor={...state.settings.tutor,mode:'remote',url:'https://one.example',activity:'auto'};state.conversations=[conversation];state.activeConversationId=conversation.id;
    const fixture=client(state);
    vi.mocked(fixture.api.tutorOverview).mockResolvedValue({capabilities:{canEnableLearning:true},context:{},formations:{items:[]},availableFormations:{items:[]}});
    render(<App api={fixture.api}/>);await screen.findByText('Commençons par observer la Grande Ourse.');
    await waitFor(()=>expect(fixture.api.tutorOverview).toHaveBeenCalledTimes(1));
    let finishOverview!:(value:Awaited<ReturnType<typeof fixture.api.tutorOverview>>)=>void;
    vi.mocked(fixture.api.tutorOverview).mockImplementation(()=>new Promise(resolve=>{finishOverview=resolve;}));
    const changed=fixture.getState();changed.settings.tutor.url='https://two.example';act(()=>fixture.broadcast(changed));
    await waitFor(()=>expect(fixture.api.tutorOverview).toHaveBeenCalledTimes(2));
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Continuons');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    expect(fixture.api.enableLearning).not.toHaveBeenCalled();expect(fixture.api.tutorOverview).toHaveBeenCalledTimes(2);
    await act(async()=>finishOverview({context:{},formations:{items:[]},availableFormations:{items:[]}}));
  });

  it('reprend la conversation existante sans rejouer un ancien message', async () => {
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;const {api}=client(state);render(<App api={api}/>);
    await screen.findByText('Commençons par observer la Grande Ourse.');expect(api.sendPrompt).not.toHaveBeenCalled();
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Continuons');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(api.sendPrompt).toHaveBeenCalledWith(conversation.id,'Continuons'));expect(api.startConversation).not.toHaveBeenCalled();expect(api.tutorOverview).toHaveBeenCalledTimes(1);
    expect(screen.getAllByText('Commençons par observer la Grande Ourse.')).toHaveLength(1);
  });

  it('accepte immédiatement les événements de la session recréée et ignore ceux de l’ancienne', async () => {
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;
    const fixture=client(state);let finish!:(value:{stopReason:string})=>void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(()=>new Promise(resolve=>{finish=resolve;}));
    render(<App api={fixture.api}/>);await screen.findByText('Commençons par observer la Grande Ourse.');
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Continuons');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledWith(conversation.id,'Continuons'));
    const rebound=fixture.getState();rebound.pendingMessage=null;rebound.conversations[0].agentSessionId='agent-new';
    rebound.conversations[0].messages.push(
      {id:'message-user',role:'user',text:'Continuons',createdAt:'2026-10-24T11:00:00Z',status:'complete'},
      {id:'message-answer',role:'assistant',text:'',createdAt:'2026-10-24T11:00:01Z',status:'streaming'}
    );
    act(()=>{
      fixture.broadcast(rebound);
      fixture.emit({method:'session/update',params:{sessionId:'agent-new',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'Nouvelle session conservée.'}}}});
      fixture.emit({method:'session/update',params:{sessionId:'agent-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'Ancienne session ignorée.'}}}});
    });
    expect(screen.getByText('Nouvelle session conservée.')).toBeTruthy();
    expect(screen.queryByText(/Ancienne session ignorée/)).toBeNull();
    await act(async()=>finish({stopReason:'end_turn'}));
  });

  it('ouvre une nouvelle conversation tout en conservant l’historique vertical', async () => {
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;const {api}=client(state);render(<App api={api}/>);await screen.findByText('Commençons par observer la Grande Ourse.');
    await userEvent.click(screen.getByRole('button',{name:'Nouvelle conversation'}));await screen.findByRole('heading',{name:'Qu’avez-vous envie d’apprendre ?'});
    expect(screen.getByRole('navigation',{name:'Historique des conversations'})).toBeTruthy();expect(api.selectConversation).toHaveBeenCalledWith();
    expect(api.sendPrompt).not.toHaveBeenCalled();
  });

  it('sélectionne une installation WSL et conserve son chemin Codex explicite', async () => {
    const state=initialState();state.settings.targetId='';const {api}=client(state);await ready(api);const user=userEvent.setup();await user.click(screen.getByRole('button',{name:'Paramètres'}));
    await user.click(screen.getByRole('button',{name:'Rechercher les installations'}));await user.click(await screen.findByRole('radio',{name:/Codex — Ubuntu/}));
    await waitFor(()=>expect(api.connectAgent).toHaveBeenCalledWith(target,'/home/lea/apprendre'));
    expect(api.saveSettings).toHaveBeenCalledWith(expect.objectContaining({targetId:target.id}));
  });

  it('affiche les outils et attend la décision de permission pendant le streaming', async () => {
    const fixture=client(initialState(true));let finish!:(value:{stopReason:string})=>void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(()=>new Promise(resolve=>{finish=resolve;}));await ready(fixture.api);
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Bonjour');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalled());
    const waiting = screen.getByText('J’attends la réponse de l’agent…').closest('[role="status"]');
    expect(waiting).toBeTruthy();
    expect(screen.getByRole('log').contains(waiting)).toBe(true);
    expect(screen.getByText('Total : 0 s · Étape : 0 s')).toBeTruthy();
    act(()=>fixture.emit({method:'session/update',params:{sessionId:'agent-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'Une explication progressive.'}}}}));
    expect(await screen.findByText('Une explication progressive.')).toBeTruthy();
    act(()=>fixture.emit({id:7,method:'session/request_permission',params:{sessionId:'agent-1',toolCall:{title:'Enregistrer une observation'},options:[{optionId:'reject',name:'Refuser',kind:'reject_once'}]}}));
    await userEvent.click(await screen.findByRole('button',{name:'Refuser'}));expect(fixture.api.answerPermission).toHaveBeenCalledWith(7,'reject');
    await userEvent.click(screen.getByRole('button',{name:'Interrompre'}));expect(fixture.api.cancelPrompt).toHaveBeenCalledWith('conversation-1');
    await act(async()=>finish({stopReason:'cancelled'}));
    expect(await screen.findByText(/Réponse interrompue. Vos échanges/)).toBeTruthy();
    expect(screen.queryByText(/Total : .* s/)).toBeNull();
  });

  it('signale une nouvelle tentative du fournisseur puis reprend l’activité sans interrompre la réponse', async () => {
    const fixture=client(initialState(true));let finish!:(value:{stopReason:string})=>void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(()=>new Promise(resolve=>{finish=resolve;}));await ready(fixture.api);
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Bonjour');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalled());
    const retry={type:'provider_status',severity:'warning',category:'connection',message:'Le fournisseur réessaie la requête…'};
    act(()=>fixture.emit(retry));
    expect((await screen.findByText(retry.message)).closest('[role="status"]')?.getAttribute('aria-live')).toBe('polite');
    expect(screen.queryByRole('alert')).toBeNull();expect(fixture.api.cancelPrompt).not.toHaveBeenCalled();expect(fixture.getState().agentConnected).toBe(true);
    act(()=>fixture.emit({method:'session/update',params:{sessionId:'agent-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'La réponse reprend.'}}}}));
    expect(await screen.findByText('La réponse reprend.')).toBeTruthy();expect(screen.queryByText(retry.message)).toBeNull();expect(screen.getByText('Je rédige la réponse…')).toBeTruthy();
    act(()=>fixture.emit(retry));expect(screen.getByText(retry.message)).toBeTruthy();
    act(()=>fixture.emit({method:'session/update',params:{sessionId:'agent-1',update:{sessionUpdate:'tool_call',toolCallId:'tool-1',title:'get_learner_context',status:'in_progress'}}}));
    expect(screen.queryByText(retry.message)).toBeNull();expect(screen.getByText('Je consulte votre progression…')).toBeTruthy();
    act(()=>fixture.emit({method:'session/update',params:{sessionId:'agent-1',update:{sessionUpdate:'tool_call_update',toolCallId:'tool-1',status:'completed'}}}));
    expect(screen.getByText('J’attends la réponse de l’agent…')).toBeTruthy();
    expect(screen.getByText('La réponse reprend.')).toBeTruthy();
    act(()=>fixture.emit(retry));await act(async()=>finish({stopReason:'end_turn'}));
    expect(screen.queryByText(retry.message)).toBeNull();
    act(()=>fixture.emit(retry));expect(screen.queryByText(retry.message)).toBeNull();expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1);
  });

  it('garde la saisie et signale une erreur réelle de connexion', async () => {
    const {api}=client();vi.mocked(api.connectAgent).mockRejectedValue(new Error('Adaptateur incompatible'));await ready(api);const user=userEvent.setup();
    await user.click(screen.getByRole('button',{name:'Paramètres'}));await user.click(screen.getByRole('button',{name:'Rechercher les installations'}));await user.click(screen.getByRole('button',{name:'Connecter Codex'}));
    expect(await screen.findByRole('alert')).toHaveProperty('textContent',expect.stringContaining('Adaptateur incompatible'));expect(api.sendPrompt).not.toHaveBeenCalled();
  });

  it('affiche la bibliothèque serveur une seule fois et ouvre la fiche complète', async () => {
    const fixture=client(initialState(true));const doc=trainingFixture();vi.mocked(fixture.api.trainingAction).mockImplementation(async action=>action==='list_training_library'?{items:[doc.training],next_after:''}:action==='get_training_policy'?{personal_learning:true,can_manage:false}:doc);
    await ready(fixture.api);await userEvent.click(screen.getByRole('button',{name:'Formations'}));await userEvent.click(await screen.findByRole('button',{name:/Go/}));
    expect(await screen.findByRole('navigation',{name:'Fiche formation'})).toBeTruthy();expect(screen.getByRole('heading',{name:'Variables'})).toBeTruthy();expect(screen.getByRole('button',{name:'Modifier avec l’agent'})).toBeTruthy();
    expect(screen.queryByText('Intentions à concevoir')).toBeNull();
  });

  it('change de compte sans conserver les droits ni accepter les anciens événements', async () => {
    const state=initialState(true);state.profile={id:'one',label:'Compte un'};state.profileEpoch=1;state.profiles=[state.profile,{id:'two',label:'Compte deux'}];const fixture=client(state);
    vi.mocked(fixture.api.selectProfile).mockImplementation(async()=>{const next=initialState();next.profile={id:'two',label:'Compte deux'};next.profileEpoch=2;fixture.setState(next);return next;});
    await ready(fixture.api);await userEvent.click(screen.getByRole('button',{name:'Mon compte'}));await userEvent.click(screen.getByRole('button',{name:'Compte deux'}));
    await screen.findByRole('heading',{name:'Qu’avez-vous envie d’apprendre ?'});act(()=>fixture.emit({profileEpoch:1,type:'connection',status:'error',message:'Ancien compte'}));expect(screen.queryByText('Ancien compte')).toBeNull();expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });
});

it('ouvre les exercices depuis le rail sans envoyer de message',async()=>{
 const fixture=client(initialState(true));await ready(fixture.api);await userEvent.click(screen.getByRole('button',{name:'Quiz et flashcards'}));
 expect(await screen.findByRole('menu',{name:'Modalités d’apprentissage'})).toBeTruthy();
 expect(document.activeElement).toBe(screen.getByLabelText('Message au tuteur'));expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
});

describe('Annulation de la préparation',()=>{
  it('libère la saisie après Stop pendant la connexion et protège le tour suivant de son retour tardif',async()=>{
    const fixture=client(initialState(true));const queue=fixture.api.queuePrompt;
    fixture.api.queuePrompt=vi.fn(async(text,id)=>{const next=await queue(text,id);next.agentConnected=false;fixture.setState(next);return next;});
    let finishConnection!:(value:Awaited<ReturnType<typeof fixture.api.connectAgent>>)=>void;
    vi.mocked(fixture.api.connectAgent).mockImplementation(()=>new Promise(resolve=>{finishConnection=resolve;}));
    let finishPrompt!:(value:{stopReason:string})=>void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(()=>new Promise(resolve=>{finishPrompt=resolve;}));
    await ready(fixture.api);await userEvent.type(screen.getByLabelText('Message au tuteur'),'Ancien message');
    await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.connectAgent).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole('button',{name:'Interrompre'}));
    await waitFor(()=>expect(screen.getByRole('button',{name:'Modifier'})).toHaveProperty('disabled',false));
    expect(screen.queryByRole('button',{name:'Interrompre'})).toBeNull();
    expect(screen.getByText('La connexion à l’agent continue en arrière-plan.')).toBeTruthy();
    expect(fixture.getState().pendingMessage).toMatchObject({text:'Ancien message',paused:true});
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('button',{name:'Modifier'}));
    await userEvent.clear(screen.getByLabelText('Message au tuteur'));await userEvent.type(screen.getByLabelText('Message au tuteur'),'Nouveau message');
    await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.queuePrompt).toHaveBeenCalledTimes(3));
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(1);
    await act(async()=>{fixture.broadcast({...fixture.getState(),agentConnected:true});finishConnection({protocolVersion:1,authMethods:[],agentInfo:{name:'codex',version:'test'},agentCapabilities:{loadSession:true}});});
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledWith(conversation.id,'Nouveau message'));
    expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1);expect(screen.getByRole('button',{name:'Interrompre'})).toBeTruthy();
    expect(screen.queryByText('La connexion à l’agent continue en arrière-plan.')).toBeNull();
    await act(async()=>finishPrompt({stopReason:'end_turn'}));
  });

  it('libère le tour après Stop pendant la détection sans affecter un nouveau profil au retour tardif',async()=>{
    const state=initialState(true);state.profile={id:'one',label:'Compte un'};state.profileEpoch=1;
    const fixture=client(state);vi.mocked(fixture.api.discoverAgents).mockResolvedValue([]);
    const queue=fixture.api.queuePrompt;
    fixture.api.queuePrompt=vi.fn(async(text,id)=>{const next=await queue(text,id);next.agentConnected=false;fixture.setState(next);return next;});
    await ready(fixture.api);await waitFor(()=>expect(fixture.api.discoverAgents).toHaveBeenCalledTimes(1));
    let finishDiscovery!:(value:Awaited<ReturnType<typeof fixture.api.discoverAgents>>)=>void;
    vi.mocked(fixture.api.discoverAgents).mockImplementation(()=>new Promise(resolve=>{finishDiscovery=resolve;}));
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Ancien message');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.discoverAgents).toHaveBeenCalledTimes(2));
    await userEvent.click(screen.getByRole('button',{name:'Interrompre'}));
    await waitFor(()=>expect(screen.getByRole('button',{name:'Modifier'})).toHaveProperty('disabled',false));
    expect(fixture.getState().pendingMessage?.paused).toBe(true);
    const changed=initialState(true);changed.profile={id:'two',label:'Compte deux'};changed.profileEpoch=2;
    act(()=>fixture.broadcast(changed));
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Brouillon du nouveau compte');
    await act(async()=>finishDiscovery([target]));
    expect(screen.getByLabelText('Message au tuteur')).toHaveProperty('value','Brouillon du nouveau compte');
    expect(fixture.api.connectAgent).not.toHaveBeenCalled();expect(fixture.api.startConversation).not.toHaveBeenCalled();expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    expect(screen.queryByRole('region',{name:'Message à envoyer'})).toBeNull();
  });

  it('propose Stop pendant la mise en attente et conserve le message sans reprise automatique',async()=>{
    const fixture=client(initialState(true));const queue=fixture.api.queuePrompt;
    let release!:()=>void;const gate=new Promise<void>(resolve=>{release=resolve;});
    fixture.api.queuePrompt=vi.fn(async(text,id)=>{await gate;return queue(text,id);});
    await ready(fixture.api);await userEvent.type(screen.getByLabelText('Message au tuteur'),'Préparer ma séance');
    await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await userEvent.click(await screen.findByRole('button',{name:'Interrompre'}));
    expect(fixture.api.cancelPrompt).toHaveBeenCalledWith('');
    await act(async()=>release());
    await screen.findByText('Envoi interrompu. Le message conservé attend votre décision.');
    expect(fixture.getState().pendingMessage).toMatchObject({text:'Préparer ma séance',paused:true});
    expect(fixture.api.startConversation).not.toHaveBeenCalled();expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    act(()=>fixture.broadcast({...fixture.getState(),agentConnected:false}));
    act(()=>fixture.broadcast({...fixture.getState(),agentConnected:true}));
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('arrête une nouvelle conversation en cours de création avant tout envoi au modèle',async()=>{
    const fixture=client(initialState(true));const start=fixture.api.startConversation;
    let release!:()=>void;const gate=new Promise<void>(resolve=>{release=resolve;});
    fixture.api.startConversation=vi.fn(async(...args)=>{await gate;return start(...args);});
    await ready(fixture.api);await userEvent.type(screen.getByLabelText('Message au tuteur'),'Bonjour');
    await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.startConversation).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole('button',{name:'Interrompre'}));
    await act(async()=>release());
    await screen.findByText('Envoi interrompu. Le message conservé attend votre décision.');
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    expect(fixture.api.cancelPrompt).toHaveBeenLastCalledWith(conversation.id);
    expect(fixture.getState().pendingMessage?.paused).toBe(true);
  });

  it('ne reprend pas une outbox interrompue après redémarrage mais permet un envoi explicite',async()=>{
    const state=initialState(true);state.pendingMessage={text:'Message interrompu',paused:true};
    const fixture=client(state);await ready(fixture.api);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    await userEvent.click(await screen.findByRole('button',{name:'Envoyer le message conservé'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
  });

  it('affiche les phases natives et ignore les phases tardives d’une autre opération',async()=>{
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;
    const fixture=client(state);let finish!:(value:{stopReason:string})=>void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(()=>new Promise(resolve=>{finish=resolve;}));
    render(<App api={fixture.api}/>);await screen.findByText('Commençons par observer la Grande Ourse.');
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Continuons');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalled());
    const phase=(operationId:string,name:string)=>({type:'turn_phase',operationId,conversationId:conversation.id,phase:name,state:'started',elapsedMs:0,phaseElapsedMs:0});
    act(()=>fixture.emit(phase('current','preparing')));
    act(()=>fixture.emit(phase('current','loading_session')));
    expect(screen.getByText('Je retrouve votre conversation…')).toBeTruthy();
    act(()=>fixture.emit(phase('old','preparing')));
    act(()=>fixture.emit(phase('old','finalizing')));
    expect(screen.queryByText('J’enregistre la réponse…')).toBeNull();
    act(()=>fixture.emit(phase('current','finalizing')));
    expect(screen.getByText('J’enregistre la réponse…')).toBeTruthy();
    await act(async()=>finish({stopReason:'end_turn'}));
    expect(screen.queryByText('J’enregistre la réponse…')).toBeNull();
  });
});
