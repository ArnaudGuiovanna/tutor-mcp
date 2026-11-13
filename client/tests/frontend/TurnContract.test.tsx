import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import App from '../../src/App';
import { HarnessStatus } from '../../src/HarnessStatus';
import { setInterfaceLanguage } from '../../src/i18n';
import { errorMessage } from '../../src/model';
import { acceptTurnEvent, eventForTurn, turnLimitFollowUp, turnLimitFromEvent, turnLimitFromResult, turnLockReason } from '../../src/turnEvents';
import type { Conversation, PromptResult } from '../../src/types';
import { client, conversation, initialState } from './clientFixture';

const refused = 'Interrompez la réponse en cours avant de modifier les réglages de l’agent.';
const limitText = 'Le tuteur a atteint la limite de 12 actions pour cette réponse.';
const limitEvent = (conversationId: string, enforcedBy: 'client' | 'agent' = 'client') =>
  ({type:'turn_limit',conversationId,stopReason:'max_turn_requests',limit:12,enforcedBy,message:'Le tuteur a atteint la limite de 12 actions pour cette réponse. Vous pouvez lui demander de poursuivre.'});
const chunk = (sessionId: string, text: string) =>
  ({method:'session/update',params:{sessionId,update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text}}}});
/** A harness event as the native backend emits it during a turn. */
const tagged = (sessionId: string, text: string, conversationId: string, turnId?: string) =>
  ({...chunk(sessionId, text), conversationId, ...(turnId ? {turnId} : {})});

async function send(text: string) {
  await userEvent.type(screen.getByLabelText('Message au tuteur'), text);
  await userEvent.click(screen.getByRole('button', {name:'Envoyer (Ctrl Entrée)'}));
}

afterEach(() => setInterfaceLanguage('fr'));

describe('Limite d’actions d’un tour', () => {
  it('rattache un seul avis à la réponse interrompue, sans alerte, et « Continuer » relance le tuteur', async () => {
    const state = initialState(true); state.conversations = [conversation]; state.activeConversationId = conversation.id;
    const fixture = client(state);
    vi.mocked(fixture.api.sendPrompt).mockImplementationOnce(async (id, text) => {
      const next = fixture.getState(); next.pendingMessage = null;
      next.conversations.find(item => item.id === id)!.messages.push(
        {id:'user-2',role:'user',text,createdAt:'2026-10-24T11:00:00Z',status:'complete'},
        {id:'answer-2',role:'assistant',text:'Je consulte encore votre progression.',createdAt:'2026-10-24T11:00:01Z',status:'interrupted'});
      fixture.setState(next);
      fixture.emit(limitEvent(id, 'agent'));
      return {stopReason:'max_turn_requests', turnLimit:12};
    });
    render(<App api={fixture.api}/>); await screen.findByText('Commençons par observer la Grande Ourse.');
    await send('Fais le point');
    const notice = await screen.findByText(new RegExp(limitText));
    expect(screen.getAllByText(new RegExp(limitText))).toHaveLength(1);
    // With its own response, in place of the generic interruption line.
    expect(notice.closest('article')?.textContent).toContain('Je consulte encore votre progression.');
    expect(screen.queryByText(/Réponse interrompue — /)).toBeNull();
    expect(notice.closest('[role="status"]')).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
    // A late event of the same turn does not duplicate it.
    act(() => fixture.emit(limitEvent(conversation.id)));
    expect(screen.getAllByText(new RegExp(limitText))).toHaveLength(1);
    await userEvent.click(screen.getByRole('button', {name:'Continuer'}));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenLastCalledWith(conversation.id, turnLimitFollowUp));
    await waitFor(() => expect(screen.queryByText(new RegExp(limitText))).toBeNull());
  });

  it('affiche l’avis depuis le résultat de send_prompt quand l’événement manque, et ignore celui d’une autre conversation', async () => {
    const state = initialState(true); state.conversations = [conversation]; state.activeConversationId = conversation.id;
    const fixture = client(state);
    vi.mocked(fixture.api.sendPrompt).mockImplementationOnce(async (id, text) => {
      const next = fixture.getState(); next.pendingMessage = null;
      next.conversations.find(item => item.id === id)!.messages.push(
        {id:'user-2',role:'user',text,createdAt:'2026-10-24T11:00:00Z',status:'complete'},
        {id:'answer-2',role:'assistant',text:'Réponse partielle.',createdAt:'2026-10-24T11:00:01Z',status:'interrupted'});
      fixture.setState(next);
      fixture.emit(limitEvent('conversation-other'));
      return {stopReason:'max_turn_requests', turnLimit:12};
    });
    render(<App api={fixture.api}/>); await screen.findByText('Commençons par observer la Grande Ourse.');
    await send('Continuons');
    const notice = await screen.findByText(new RegExp(limitText));
    expect(screen.getAllByText(new RegExp(limitText))).toHaveLength(1);
    expect(notice.closest('article')?.textContent).toContain('Réponse partielle.');
    // An event for another conversation, after the turn, never adds a notice here.
    act(() => fixture.emit(limitEvent('conversation-other')));
    expect(screen.getAllByText(new RegExp(limitText))).toHaveLength(1);
  });

  it('lit le contrat du résultat et de l’événement', () => {
    expect(turnLimitFromResult('c', {stopReason:'max_turn_requests', turnLimit:12})).toEqual({conversationId:'c', limit:12});
    expect(turnLimitFromResult('c', {stopReason:'end_turn'} as PromptResult)).toBeUndefined();
    expect(turnLimitFromEvent(limitEvent('c', 'agent'))).toEqual({conversationId:'c', limit:12, enforcedBy:'agent'});
    expect(turnLimitFromEvent({type:'turn_limit', conversationId:'c'})).toBeUndefined();
  });
});

describe('Événements de session rattachés au tour', () => {
  it('accepte le flux étiqueté du tour, même sur une session privée, et jamais un événement non étiqueté', async () => {
    const fresh: Conversation = {...structuredClone(conversation), agentSessionId:null};
    const other: Conversation = {...structuredClone(conversation), id:'conversation-2', title:'Autre', agentSessionId:'agent-2', messages:[]};
    const state = initialState(true); state.conversations = [fresh, other]; state.activeConversationId = fresh.id;
    const fixture = client(state); let finish!: (value: PromptResult) => void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    render(<App api={fixture.api}/>); await screen.findByText('Commençons par observer la Grande Ourse.');
    await send('Bonjour');
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledWith(fresh.id, 'Bonjour'));
    act(() => {
      fixture.emit(tagged('agent-2', 'Fuite d’une autre conversation.', other.id, 'answer-9'));
      fixture.emit(tagged('private-1', 'Réponse du tour privé.', fresh.id, 'answer-1'));
      // Late chunks of a cancelled turn and replayed history carry no tag.
      fixture.emit(chunk('private-1', ' Fin d’un tour annulé.'));
      fixture.emit(chunk('stray-session', ' Session étrangère.'));
    });
    expect(screen.getByText('Réponse du tour privé.')).toBeTruthy();
    expect(screen.queryByText(/Fuite d’une autre conversation/)).toBeNull();
    expect(screen.queryByText(/Fin d’un tour annulé/)).toBeNull();
    expect(screen.queryByText(/Session étrangère/)).toBeNull();
    await act(async () => finish({stopReason:'end_turn'}));
  });
});

describe('Événements nommant leur conversation et leur tour', () => {
  it('s’appuie uniquement sur conversationId et turnId', () => {
    const turn: {conversationId:string; turnId?:string} = {conversationId:conversation.id};
    // The native tag decides, even for a session the snapshot does not know yet.
    expect(acceptTurnEvent(tagged('private-1', 'x', conversation.id, 'answer-1'), turn)).toBe(true);
    expect(turn).toEqual({conversationId:conversation.id, turnId:'answer-1'});
    // Another conversation, even on this conversation's session, is refused.
    expect(acceptTurnEvent(tagged('agent-1', 'x', 'b', 'answer-9'), turn)).toBe(false);
    // A late event of an earlier turn of the same conversation is refused.
    expect(acceptTurnEvent(tagged('agent-1', 'x', conversation.id, 'answer-0'), turn)).toBe(false);
    expect(acceptTurnEvent(tagged('agent-1', 'x', conversation.id, 'answer-1'), turn)).toBe(true);
    // Untagged session events belong to no running turn, whatever their session.
    expect(acceptTurnEvent(chunk('agent-1', 'x'), turn)).toBe(false);
    expect(acceptTurnEvent(chunk('private-1', 'x'), turn)).toBe(false);
    expect(acceptTurnEvent({...chunk('agent-1', 'x'), conversationId:''}, turn)).toBe(false);
    // Turn-level events.
    expect(eventForTurn({type:'turn_error', conversationId:'b'}, conversation.id)).toBe(false);
    expect(eventForTurn({type:'turn_error', conversationId:conversation.id, turnId:'answer-0'}, conversation.id, 'answer-1')).toBe(false);
    expect(eventForTurn({type:'turn_error', conversationId:conversation.id, turnId:'answer-1'}, conversation.id, 'answer-1')).toBe(true);
    expect(eventForTurn({type:'turn_error'}, conversation.id)).toBe(true);
  });

  it('affiche le flux et l’erreur du tour courant et ignore ceux d’une autre conversation', async () => {
    const state = initialState(true); state.conversations = [conversation]; state.activeConversationId = conversation.id;
    const fixture = client(state); let finish!: (value: PromptResult) => void;
    vi.mocked(fixture.api.sendPrompt).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    render(<App api={fixture.api}/>); await screen.findByText('Commençons par observer la Grande Ourse.');
    await send('Bonjour');
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledWith(conversation.id, 'Bonjour'));
    act(() => {
      fixture.emit({...chunk('unknown-session', 'Réponse étiquetée.'), conversationId:conversation.id, turnId:'answer-2'});
      fixture.emit({...chunk(conversation.agentSessionId || 'agent-1', ' Fuite étiquetée.'), conversationId:'conversation-other', turnId:'answer-x'});
      fixture.emit({type:'turn_error', conversationId:'conversation-other', message:'Erreur étrangère.'});
    });
    expect(screen.getByText('Réponse étiquetée.')).toBeTruthy();
    expect(screen.queryByText(/Fuite étiquetée/)).toBeNull();
    act(() => fixture.emit({type:'turn_error', conversationId:conversation.id, turnId:'answer-2', message:'Le tour a échoué.'}));
    await act(async () => finish({stopReason:'end_turn'}));
    expect(screen.queryByText('Erreur étrangère.')).toBeNull();
    expect(await screen.findByText(/Le tour a échoué\./)).toBeTruthy();
  });
});

describe('Opérations refusées pendant un tour', () => {
  it('désactive la connexion, Tutor et les réglages du modèle pendant une réponse en expliquant pourquoi', async () => {
    const state = initialState(true); state.conversations = [conversation]; state.activeConversationId = conversation.id;
    const fixture = client(state); let finish!: (value: PromptResult) => void;
    vi.mocked(fixture.api.agentConfiguration).mockResolvedValue({connected:true,configOptions:[],current:{model:'',reasoningEffort:'',speed:'balanced'},supported:{model:false,reasoningEffort:false,speed:true},speedOptions:['balanced','fast']});
    vi.mocked(fixture.api.sendPrompt).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    render(<App api={fixture.api}/>); await screen.findByText('Commençons par observer la Grande Ourse.');
    await send('Continuons');
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalled());
    await userEvent.click(screen.getByRole('button', {name:'Paramètres'}));
    const disconnect = await screen.findByRole('button', {name:/Déconnecter Codex/});
    expect((disconnect as HTMLButtonElement).disabled).toBe(true);
    expect(disconnect.getAttribute('aria-describedby')).toBe('agent-turn-lock');
    expect(document.getElementById('agent-turn-lock')?.textContent).toBe(turnLockReason);
    const tutor = screen.getByRole('button', {name:'Déconnecter Tutor'});
    expect((tutor as HTMLButtonElement).disabled).toBe(true);
    expect(document.getElementById(tutor.getAttribute('aria-describedby') || '')?.textContent).toBe(turnLockReason);
    const speed = await screen.findByLabelText('Préférence de réponse');
    expect((speed as HTMLSelectElement).disabled).toBe(true);
    expect(document.getElementById(speed.getAttribute('aria-describedby') || '')?.textContent).toBe(turnLockReason);
    await act(async () => finish({stopReason:'end_turn'}));
    await waitFor(() => expect((screen.getByRole('button', {name:/Déconnecter Codex/}) as HTMLButtonElement).disabled).toBe(false));
    expect(document.getElementById('agent-turn-lock')).toBeNull();
    expect(screen.getByLabelText('Préférence de réponse').getAttribute('aria-describedby')).toBeNull();
  });

  it('affiche proprement le refus natif s’il survient malgré tout, traduit', async () => {
    const state = initialState(true);
    const fixture = client(state);
    vi.mocked(fixture.api.agentConfiguration).mockResolvedValue({connected:true,configOptions:[],current:{model:'',reasoningEffort:'',speed:'balanced'},supported:{model:false,reasoningEffort:false,speed:true},speedOptions:['balanced','fast']});
    vi.mocked(fixture.api.setAgentConfiguration).mockRejectedValueOnce(new Error(refused));
    render(<App api={fixture.api}/>); await screen.findByRole('heading', {name:'Qu’avez-vous envie d’apprendre ?'});
    await userEvent.click(screen.getByRole('button', {name:'Paramètres'}));
    await userEvent.selectOptions(await screen.findByLabelText('Préférence de réponse'), 'fast');
    expect((await screen.findByRole('alert')).textContent).toBe(refused);
    setInterfaceLanguage('en');
    expect(errorMessage(new Error(refused))).toBe('Stop the current response before changing the agent settings.');
    expect(errorMessage(new Error('Interrompez la réponse en cours avant de modifier la connexion.'))).toBe('Stop the current response before changing the connection.');
  });

  it('décrit pourquoi la reconnexion manuelle est indisponible', () => {
    render(<HarnessStatus connected={false} reconnect={{state:'failed',attempt:5,nextAttemptAt:null,cause:null}} limit={null} loss={{message:'Arrêt'}} busy={false} blockedReason={turnLockReason} onReconnect={() => {}}/>);
    const button = screen.getByRole('button', {name:'Reconnecter'}) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(document.getElementById(button.getAttribute('aria-describedby') || '')?.textContent).toBe(turnLockReason);
  });
});
