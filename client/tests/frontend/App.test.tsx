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
  it('ouvre directement une conversation et crée sans formulaire d’intention', async () => {
    const {api}=client(initialState(true));await ready(api);const user=userEvent.setup();
    await user.type(screen.getByLabelText('Votre message au tuteur'),'Je veux apprendre le Go');
    await user.click(screen.getByRole('button',{name:'Envoyer'}));
    await waitFor(()=>expect(api.sendPrompt).toHaveBeenCalledWith('conversation-1','Je veux apprendre le Go'));
    expect(api.startConversation).toHaveBeenCalledWith(undefined,undefined);
    expect(api.saveIntent).not.toHaveBeenCalled();
    expect(screen.queryByLabelText('Ce que je souhaite apprendre')).toBeNull();
  });

  it('conserve un message à envoyer pendant la configuration et le reprend une seule fois', async () => {
    const fixture=client();await ready(fixture.api);const user=userEvent.setup();
    await user.type(screen.getByLabelText('Votre message au tuteur'),'Je veux apprendre le Go');await user.click(screen.getByRole('button',{name:'Envoyer'}));
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
    await waitFor(()=>expect((screen.getByLabelText('Votre message au tuteur')as HTMLTextAreaElement).value).toBe('Apprendre Python'));
    expect(api.queuePrompt).toHaveBeenCalledWith(null);expect(api.sendPrompt).not.toHaveBeenCalled();
  });

  it('active l’espace apprenant d’un propriétaire au premier message en conservant ses droits', async () => {
    const state=initialState(true);state.settings.tutor.mode='remote';const fixture=client(state);
    vi.mocked(fixture.api.tutorOverview).mockResolvedValue({identity:{tenantId:'t',userId:'u',membershipId:'m',roles:['owner']},capabilities:{canEnableLearning:true},context:{},formations:{items:[]},availableFormations:{items:[]}});
    vi.mocked(fixture.api.enableLearning).mockImplementation(async()=>{const next=fixture.getState();next.agentConnected=false;fixture.setState(next);return next;});
    await ready(fixture.api);await userEvent.type(screen.getByLabelText('Votre message au tuteur'),'Je veux apprendre le Go');await userEvent.click(screen.getByRole('button',{name:'Envoyer'}));
    await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));expect(fixture.api.enableLearning).toHaveBeenCalledTimes(1);
    expect(fixture.api.connectAgent).toHaveBeenCalled();expect(fixture.api.startConversation).toHaveBeenCalledWith(undefined,undefined);
    expect(screen.queryByText('Mon activité dans l’institution')).toBeNull();
  });

  it('reprend la conversation existante sans rejouer un ancien message', async () => {
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;const {api}=client(state);render(<App api={api}/>);
    await screen.findByText('Commençons par observer la Grande Ourse.');expect(api.sendPrompt).not.toHaveBeenCalled();
    await userEvent.type(screen.getByLabelText('Votre message au tuteur'),'Continuons');await userEvent.click(screen.getByRole('button',{name:'Envoyer'}));
    await waitFor(()=>expect(api.startConversation).toHaveBeenCalledWith(undefined,conversation.id));expect(api.sendPrompt).toHaveBeenCalledWith(conversation.id,'Continuons');
    expect(screen.getAllByText('Commençons par observer la Grande Ourse.')).toHaveLength(1);
  });

  it('ouvre une nouvelle conversation tout en conservant l’historique vertical', async () => {
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;const {api}=client(state);render(<App api={api}/>);await screen.findByText('Commençons par observer la Grande Ourse.');
    await userEvent.click(screen.getByRole('button',{name:'Nouvelle conversation'}));await screen.findByRole('heading',{name:'Qu’avez-vous envie d’apprendre ?'});
    expect(screen.getByRole('navigation',{name:'Historique des conversations'})).toBeTruthy();expect(api.selectConversation).toHaveBeenCalledWith();
    expect(api.sendPrompt).not.toHaveBeenCalled();
  });

  it('sélectionne une installation WSL et conserve son chemin Codex explicite', async () => {
    const {api}=client();await ready(api);const user=userEvent.setup();await user.click(screen.getByRole('button',{name:'Agents'}));
    await user.click(screen.getByRole('button',{name:'Rechercher les installations'}));await user.click(await screen.findByRole('radio',{name:/Codex — Ubuntu/}));
    await user.click(screen.getByRole('button',{name:'Connecter Codex'}));await waitFor(()=>expect(api.connectAgent).toHaveBeenCalledWith(target,'/home/lea/apprendre'));
    expect(api.saveSettings).toHaveBeenCalledWith(expect.objectContaining({targetId:target.id}));
  });

  it('affiche les outils et attend la décision de permission pendant le streaming', async () => {
    const fixture=client(initialState(true));let finish!:(value:{stopReason:string})=>void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(()=>new Promise(resolve=>{finish=resolve;}));await ready(fixture.api);
    await userEvent.type(screen.getByLabelText('Votre message au tuteur'),'Bonjour');await userEvent.click(screen.getByRole('button',{name:'Envoyer'}));await waitFor(()=>expect(fixture.api.sendPrompt).toHaveBeenCalled());
    act(()=>fixture.emit({method:'session/update',params:{sessionId:'agent-1',update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text:'Une explication progressive.'}}}}));
    expect(await screen.findByText('Une explication progressive.')).toBeTruthy();
    act(()=>fixture.emit({id:7,method:'session/request_permission',params:{sessionId:'agent-1',toolCall:{title:'Enregistrer une observation'},options:[{optionId:'reject',name:'Refuser',kind:'reject_once'}]}}));
    await userEvent.click(await screen.findByRole('button',{name:'Refuser'}));expect(fixture.api.answerPermission).toHaveBeenCalledWith(7,'reject');
    await userEvent.click(screen.getByRole('button',{name:'Interrompre'}));expect(fixture.api.cancelPrompt).toHaveBeenCalledWith('conversation-1');
    await act(async()=>finish({stopReason:'cancelled'}));
    expect(await screen.findByText(/Réponse interrompue. Vos échanges/)).toBeTruthy();
  });

  it('garde la saisie et signale une erreur réelle de connexion', async () => {
    const {api}=client();vi.mocked(api.connectAgent).mockRejectedValue(new Error('Adaptateur incompatible'));await ready(api);const user=userEvent.setup();
    await user.click(screen.getByRole('button',{name:'Agents'}));await user.click(screen.getByRole('button',{name:'Rechercher les installations'}));await user.click(screen.getByRole('button',{name:'Connecter Codex'}));
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
    await ready(fixture.api);await userEvent.click(screen.getByRole('button',{name:'Compte un'}));await userEvent.click(screen.getByRole('button',{name:'Compte deux'}));
    await screen.findByRole('heading',{name:'Qu’avez-vous envie d’apprendre ?'});act(()=>fixture.emit({profileEpoch:1,type:'connection',status:'error',message:'Ancien compte'}));expect(screen.queryByText('Ancien compte')).toBeNull();expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });
});
