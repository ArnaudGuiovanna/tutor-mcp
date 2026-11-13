import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
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

  it('laisse la supervision native reconnecter après une déconnexion involontaire et affiche le décompte', async () => {
    const fixture=client(initialState(true));await ready(fixture.api);
    const disconnected=fixture.getState();disconnected.agentConnected=false;fixture.setState(disconnected);
    act(()=>fixture.emit({type:'connection',status:'closed',message:'Processus arrêté'}));
    act(()=>fixture.emit({type:'agent_reconnect',state:'waiting',attempt:1,nextAttemptAt:new Date(Date.now()+4000).toISOString(),cause:'Processus arrêté'}));
    expect(await screen.findByText(/Reconnexion à l’agent dans [34] s \(tentative 1\)/)).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
    act(()=>fixture.emit({type:'agent_reconnect',state:'connecting',attempt:1,nextAttemptAt:null,cause:'Processus arrêté'}));
    expect(screen.getByText('Reconnexion à l’agent en cours…')).toBeTruthy();
    act(()=>fixture.broadcast({...fixture.getState(),agentConnected:true,agentStatus:{phase:'running',authentication:'authenticated',reconnect:{state:'idle',attempt:1,nextAttemptAt:null,cause:null}}}));
    await waitFor(()=>expect(screen.queryByRole('region',{name:'État de l’agent'})).toBeNull());
    // Reconnections belong to the native supervisor: the interface never relaunches.
    expect(fixture.api.connectAgent).not.toHaveBeenCalled();
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('button',{name:'Paramètres'}));
    await userEvent.click(screen.getByRole('button',{name:'Déconnecter Codex'}));
    await waitFor(()=>expect(fixture.getState().agentConnected).toBe(false));
    vi.useFakeTimers();
    try { await act(async()=>{await vi.advanceTimersByTimeAsync(10000);}); }
    finally { vi.useRealTimers(); }
    expect(fixture.api.connectAgent).not.toHaveBeenCalled();
  });

  it('lance le harness une seule fois puis propose « Reconnecter » après un échec, avec les détails techniques', async () => {
    const fixture=client();vi.mocked(fixture.api.connectAgent).mockRejectedValueOnce(new Error('Harness indisponible'));
    await ready(fixture.api);
    expect((await screen.findByRole('alert')).textContent).toContain('Harness indisponible');
    vi.useFakeTimers();
    try { for(let i=0;i<4;i++) await act(async()=>{await vi.advanceTimersByTimeAsync(4000);}); }
    finally { vi.useRealTimers(); }
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole('button',{name:'Reconnecter'}));
    await waitFor(()=>expect(fixture.getState().agentConnected).toBe(true));
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(2);
    await waitFor(()=>expect(screen.queryByRole('button',{name:'Reconnecter'})).toBeNull());
    // The supervisor gave up after repeated exits: its cause and the stderr tail are shown.
    const stopped=fixture.getState();stopped.agentConnected=false;fixture.setState(stopped);
    act(()=>fixture.emit({type:'connection',status:'error',message:'L’agent s’est arrêté.',stderrTail:'panic: adaptateur'}));
    act(()=>fixture.broadcast({...fixture.getState(),agentStatus:{phase:'stopped',authentication:'unknown',reconnect:{state:'failed',attempt:5,nextAttemptAt:null,cause:'Adaptateur introuvable'}}}));
    expect((await screen.findByRole('alert')).textContent).toBe('La connexion à l’agent a échoué. Adaptateur introuvable');
    expect(screen.getByText('Détails techniques')).toBeTruthy();expect(screen.getByText('panic: adaptateur')).toBeTruthy();
    expect(screen.queryByText('L’agent s’est arrêté.')).toBeNull();
    await userEvent.click(screen.getByRole('button',{name:'Reconnecter'}));
    expect(fixture.api.connectAgent).toHaveBeenCalledTimes(3);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('suspend l’envoi pendant une limite du fournisseur puis le réactive à l’heure dite', async () => {
    const fixture=client(initialState(true));await ready(fixture.api);
    vi.useFakeTimers({toFake:['setTimeout','clearTimeout','setInterval','clearInterval','Date']});
    try {
      const until=new Date(Date.now()+90_000);
      act(()=>fixture.emit({type:'agent_limited',kind:'usage_limit',until:until.toISOString(),message:'Limite'}));
      const time=new Intl.DateTimeFormat('fr-FR',{hour:'2-digit',minute:'2-digit'}).format(until);
      expect(within(screen.getByRole('region',{name:'État de l’agent'})).getByText(`Limite atteinte (quota d’usage), reprise à ${time}`,{exact:false})).toBeTruthy();
      expect(screen.getByText(/dans 2 min/)).toBeTruthy();
      const input=screen.getByLabelText('Message au tuteur') as HTMLTextAreaElement;
      act(()=>{fireEvent.change(input,{target:{value:'Bonjour'}});});
      expect(input.value).toBe('Bonjour');expect(input.disabled).toBe(false);
      const send=screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}) as HTMLButtonElement;
      expect(send.disabled).toBe(true);
      expect(send.getAttribute('aria-describedby')).toBe('send-blocked-reason');
      expect(document.getElementById('send-blocked-reason')?.textContent).toBe(`Envoi suspendu : Limite atteinte (quota d’usage), reprise à ${time}.`);
      await act(async()=>{await vi.advanceTimersByTimeAsync(60_000);});
      expect(screen.getByText(/dans 30 s/)).toBeTruthy();
      await act(async()=>{await vi.advanceTimersByTimeAsync(31_000);});
      expect(screen.queryByText(/Limite atteinte/)).toBeNull();
      expect((screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}) as HTMLButtonElement).disabled).toBe(false);
      expect(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}).getAttribute('aria-describedby')).toBeNull();
    } finally { vi.useRealTimers(); }
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('rattache l’échec d’un tour à la conversation et propose de le réessayer', async () => {
    const fixture=client(initialState(true));
    vi.mocked(fixture.api.sendPrompt).mockImplementationOnce(async()=>{const consumed=fixture.getState();consumed.pendingMessage=null;fixture.setState(consumed);fixture.emit({type:'turn_error',message:'La réponse n’a pas pu être enregistrée : disque plein'});throw new Error('La réponse n’a pas pu être enregistrée : disque plein');});
    await ready(fixture.api);
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Bonjour');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    const failure=await screen.findByText('La réponse a échoué : La réponse n’a pas pu être enregistrée : disque plein');
    expect(screen.getByRole('region',{name:'Messages de la conversation'}).contains(failure)).toBe(true);
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(document.querySelector('.notice.error')).toBeNull();
    await userEvent.click(screen.getByRole('button',{name:'Réessayer'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(2));
    expect(fixture.api.sendPrompt).toHaveBeenLastCalledWith('conversation-1','Bonjour');
    await waitFor(()=>expect(screen.queryByText(/La réponse a échoué/)).toBeNull());
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
    const state=initialState(true);state.settings.tutor.mode='remote';state.settings.setupCompleted=true;
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
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;const fixture=client(state);const {api}=fixture;render(<App api={api}/>);
    await screen.findByText('Commençons par observer la Grande Ourse.');expect(api.sendPrompt).not.toHaveBeenCalled();
    await userEvent.type(screen.getByLabelText('Message au tuteur'),'Continuons');await userEvent.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(api.sendPrompt).toHaveBeenCalledWith(conversation.id,'Continuons'));expect(api.startConversation).not.toHaveBeenCalled();expect(api.tutorOverview).toHaveBeenCalledTimes(1);
    expect(screen.getAllByText('Commençons par observer la Grande Ourse.')).toHaveLength(1);
    await waitFor(()=>expect(screen.queryByRole('button',{name:'Interrompre'})).toBeNull());
    expect(vi.mocked(api.trainingAction).mock.calls.filter(([action])=>action==='list_training_library')).toHaveLength(1);
    act(()=>fixture.emit({type:'workspace_invalidated',scope:'training_catalog'}));
    await waitFor(()=>expect(vi.mocked(api.trainingAction).mock.calls.filter(([action])=>action==='list_training_library')).toHaveLength(2));
  });

  it('accepte immédiatement les événements étiquetés de la session recréée et ignore les autres', async () => {
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
      fixture.emit({method:'session/update',conversationId:conversation.id,turnId:'message-answer',params:{sessionId:'agent-new',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'Nouvelle session conservée.'}}}});
      // The old session's late output is no longer tagged with the turn.
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
    const state=initialState();state.settings.targetId='';state.settings.setupCompleted=true;const {api}=client(state);await ready(api);const user=userEvent.setup();await user.click(screen.getByRole('button',{name:'Paramètres'}));
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
    expect(screen.getByRole('region',{name:'Messages de la conversation'}).contains(waiting)).toBe(true);
    expect(screen.getByText('Total : 0 s · Étape : 0 s')).toBeTruthy();
    act(()=>fixture.emit({method:'session/update',conversationId:'conversation-1',params:{sessionId:'agent-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'Une explication progressive.'}}}}));
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
    act(()=>fixture.emit({method:'session/update',conversationId:'conversation-1',params:{sessionId:'agent-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'La réponse reprend.'}}}}));
    expect(await screen.findByText('La réponse reprend.')).toBeTruthy();expect(screen.queryByText(retry.message)).toBeNull();expect(screen.getByText('Je rédige la réponse…')).toBeTruthy();
    act(()=>fixture.emit(retry));expect(screen.getByText(retry.message)).toBeTruthy();
    act(()=>fixture.emit({method:'session/update',conversationId:'conversation-1',params:{sessionId:'agent-1',update:{sessionUpdate:'tool_call',toolCallId:'tool-1',title:'get_learner_state',status:'in_progress'}}}));
    expect(screen.queryByText(retry.message)).toBeNull();expect(screen.getByText('Je consulte votre progression…')).toBeTruthy();
    act(()=>fixture.emit({method:'session/update',conversationId:'conversation-1',params:{sessionId:'agent-1',update:{sessionUpdate:'tool_call_update',toolCallId:'tool-1',status:'completed'}}}));
    expect(screen.getByText('J’attends la réponse de l’agent…')).toBeTruthy();
    expect(screen.getByText('La réponse reprend.')).toBeTruthy();
    act(()=>fixture.emit(retry));
    const done=fixture.getState();done.conversations[0].messages.push({id:'answer',role:'assistant',text:'La réponse reprend.',status:'complete',createdAt:'2026-10-24T11:00:02Z'});fixture.setState(done);
    await act(async()=>finish({stopReason:'end_turn'}));
    expect(screen.queryByText(retry.message)).toBeNull();
    // Only a short notice is announced, never the whole response again.
    await waitFor(()=>expect(screen.getByText('Réponse terminée').getAttribute('role')).toBe('status'));
    expect(screen.getByText('Réponse terminée').closest('[aria-live="polite"]')).toBeNull();
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

describe('Défilement de la conversation', () => {
  it('garde la liste des messages en bas après un redimensionnement si la lecture y était, sans forcer un lecteur remonté', async () => {
    const observers: { callback: ResizeObserverCallback; targets: Element[]; disconnected: boolean }[] = [];
    vi.stubGlobal('ResizeObserver', class {
      record: { callback: ResizeObserverCallback; targets: Element[]; disconnected: boolean };
      constructor(callback: ResizeObserverCallback) { this.record = { callback, targets: [], disconnected: false }; observers.push(this.record); }
      observe(element: Element) { this.record.targets.push(element); }
      unobserve() {}
      disconnect() { this.record.disconnected = true; }
    });
    try {
      const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;
      const {unmount}=render(<App api={client(state).api}/>);
      await screen.findByText('Commençons par observer la Grande Ourse.');
      const region=screen.getByRole('region',{name:'Messages de la conversation'});
      let top=0, height=2000, visible=600;
      Object.defineProperty(region,'scrollTop',{configurable:true,get:()=>top,set:value=>{top=Math.max(0,Math.min(value,height-visible));}});
      Object.defineProperty(region,'scrollHeight',{configurable:true,get:()=>height});
      Object.defineProperty(region,'clientHeight',{configurable:true,get:()=>visible});
      const observer=observers.find(item=>item.targets.includes(region));
      expect(observer).toBeTruthy();
      const resize=()=>act(()=>observer!.callback([],{} as ResizeObserver));
      // At the end, then the window shrinks to 900 px: the region shrinks, no scroll event is fired.
      top=1400;fireEvent.scroll(region);
      visible=420;resize();
      expect(top).toBe(height-visible);
      // A reader who scrolled back up keeps their position.
      top=200;fireEvent.scroll(region);
      visible=300;resize();
      expect(top).toBe(200);
      unmount();
      expect(observer!.disconnected).toBe(true);
    } finally { vi.unstubAllGlobals(); }
  });
});
