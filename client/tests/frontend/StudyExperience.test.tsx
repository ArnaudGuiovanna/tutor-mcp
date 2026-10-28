import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App from '../../src/App';
import { parseLearningResponse, serializeLearningResponse } from '../../src/learningActivity';
import { parseStudyRequest, reviewScope, studyRequestText } from '../../src/studyRequests';
import { reviewPlanKey } from '../../src/reviewPlanning';
import type { ConversationContext, Snapshot } from '../../src/types';
import { client, conversation, initialState } from './clientFixture';
import { trainingFixture } from './trainingFixture';

const context: ConversationContext = {conversation_id:conversation.id, training_id:'personal:go', domain_id:'go', name:'Go', revision:'1', selection_version:1};
const quiz = {version:1, id:'q-variables', kind:'quiz', trainingId:'personal:go', domainId:'go', concept:'variables', question:'Quelle valeur contient x après x := 3 puis x += 2 ?',
  options:[{id:'a',text:'3'},{id:'b',text:'5'},{id:'c',text:'6'}], correctOptionId:'b', explanation:'L’addition de 2 à 3 donne 5.', attemptId:'attempt-quiz', activityType:'RECALL_EXERCISE', rubricJson:JSON.stringify({criteria:[{id:'correct',description:'Addition correcte',max_score:1}],passing_score:1,answer_key:'5'})};
const flashcard = {version:1, id:'f-functions', kind:'flashcard', trainingId:'personal:go', domainId:'go', concept:'fonctions', question:'Quel mot-clé déclare une fonction en Go ?', answer:'func'};
const activityText = (value:unknown) => '```tutor-activity\n'+JSON.stringify(value)+'\n```';
function withActivity(activity = quiz): Snapshot {
  const state = initialState(true);
  state.conversations = [{...structuredClone(conversation), activeTraining:context, messages:[{
    id:'card-message', role:'assistant', status:'complete', text:activityText(activity), createdAt:conversation.createdAt, trainingContext:context,
  }]}];
  state.activeConversationId = conversation.id;
  return state;
}

describe('Modalités visuelles et planning intégrés', () => {
  it('place le menu des modalités puis Envoyer dans l’ordre de tabulation de Windows', async () => {
    const fixture = client(initialState(true)); render(<App api={fixture.api}/>);
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Votre message au tuteur'), 'Apprendre Go');
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', {name:'Choisir Quiz ou Flashcards'}));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', {name:'Envoyer'}));
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('ouvre le menu avec / et choisit le quiz au clavier sans envoyer avant validation', async () => {
    const fixture = client(initialState(true)); render(<App api={fixture.api}/>);
    const input = await screen.findByLabelText('Votre message au tuteur'); const user = userEvent.setup();
    await user.type(input, '/');
    expect(screen.getByRole('menu', {name:'Modalités d’apprentissage'})).toBeTruthy();
    await user.keyboard('{ArrowDown}{Enter}');
    expect((input as HTMLTextAreaElement).value).toBe('/quiz ');
    expect(document.activeElement).toBe(input);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    await user.type(input, 'les variables en Go');
    await user.click(screen.getByRole('button', {name:'Envoyer'}));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    const sent = vi.mocked(fixture.api.sendPrompt).mock.calls[0][1];
    expect(parseStudyRequest(sent)).toEqual({version:1,format:'quiz',intent:'practice'});
    expect(sent).toContain('les variables en Go');
    expect(screen.queryByText(/tutor-study-request/)).toBeNull();
  });

  it('active un planning, conserve le format puis sélectionne Go avant de démarrer depuis Python', async () => {
    const state = initialState(true);
    let selected = {...context, training_id:'personal:python', domain_id:'python', name:'Python'};
    state.conversations = [{...structuredClone(conversation), activeTraining:selected}]; state.activeConversationId = conversation.id;
    const fixture = client(state); const doc = trainingFixture();
    doc.training.progress = [{concept:'variables', mastery:.6, next_review:'2026-10-01T10:00:00Z'}];
    const order:string[] = [];
    vi.mocked(fixture.api.trainingAction).mockImplementation(async (action) => {
      order.push(action);
      if (action === 'list_training_library') return {items:[doc.training], next_after:''};
      if (action === 'get_conversation_context') return selected;
      if (action === 'select_conversation_training') {
        selected = {...context, selection_version:2};
        const next = fixture.getState(); next.conversations[0].activeTraining = selected; fixture.setState(next);
        return selected;
      }
      throw new Error(action);
    });
    const send = fixture.api.sendPrompt;
    fixture.api.sendPrompt = vi.fn(async (id,value) => {order.push('send'); return send(id,value);});
    const view = render(<App api={fixture.api}/>); const user = userEvent.setup();
    await user.click(await screen.findByRole('button', {name:'Planning'}));
    await user.click(await screen.findByRole('switch', {name:'Répétition espacée pour Go'}));
    await waitFor(() => expect(screen.getByRole('switch', {name:'Répétition espacée pour Go'}).getAttribute('aria-checked')).toBe('true'));
    await user.selectOptions(screen.getByLabelText('Format de révision pour Go'), 'flashcard');
    await waitFor(() => expect((screen.getByLabelText('Format de révision pour Go') as HTMLSelectElement).value).toBe('flashcard'));
    const key = reviewPlanKey(reviewScope(fixture.getState(), null), 'personal:go');
    expect(fixture.getState().settings.reviewPlans?.[key]).toEqual({enabled:true,format:'flashcard'});
    view.unmount(); render(<App api={fixture.api}/>);
    await user.click(await screen.findByRole('button', {name:'Planning'}));
    await user.click(await screen.findByRole('button', {name:'Réviser Go'}));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    expect(order.indexOf('select_conversation_training')).toBeLessThan(order.indexOf('send'));
    expect(fixture.api.trainingAction).toHaveBeenCalledWith('select_conversation_training', expect.objectContaining({id:'personal:go',conversation_id:conversation.id,expected_selection:1}));
    expect(parseStudyRequest(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1])).toMatchObject({format:'flashcard',intent:'review',trainingId:'personal:go',domainId:'go'});
    expect(screen.getByRole('region', {name:'Conversation avec votre tuteur'})).toBeTruthy();
  });

  it('conserve le choix du quiz et empêche de soumettre à nouveau après réouverture', async () => {
    const fixture = client(withActivity()); const user = userEvent.setup(); const view = render(<App api={fixture.api}/>);
    expect(await screen.findByText(quiz.question)).toBeTruthy();
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    await user.click(screen.getByRole('radio', {name:/5/}));
    await user.click(screen.getByRole('button', {name:'Valider ma réponse'}));
    await screen.findByText(quiz.explanation);
    expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1);
    expect(parseLearningResponse(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1])).toMatchObject({activityId:quiz.id,selectedOptionId:'b',attemptId:'attempt-quiz'});
    view.unmount(); render(<App api={fixture.api}/>);
    await screen.findByText(quiz.explanation);
    expect(screen.queryByRole('button', {name:'Valider ma réponse'})).toBeNull();
    expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1);
  });

  it('retourne la flashcard puis envoie une autoévaluation explicitement distincte du quiz', async () => {
    const state = withActivity(); state.conversations[0].messages[0].text = activityText(flashcard);
    const fixture = client(state); render(<App api={fixture.api}/>); const user = userEvent.setup();
    await screen.findByText(flashcard.question);
    expect(screen.queryByText('func')).toBeNull();
    expect(screen.queryByRole('button', {name:'Retrouvé avec effort'})).toBeNull();
    await user.click(screen.getByRole('button', {name:'Retourner la carte'}));
    expect(screen.getByText('func')).toBeTruthy();
    await user.click(screen.getByRole('button', {name:'Retrouvé avec effort'}));
    await screen.findByText('Votre déclaration reste distincte d’une réponse vérifiée dans un quiz.');
    expect(parseLearningResponse(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1])).toMatchObject({kind:'flashcard',rating:'hard'});
    expect(parseLearningResponse(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1])).not.toHaveProperty('attemptId');
  });

  it('conserve une révision hors ligne puis la reprend une seule fois sur la formation prévue', async () => {
    const state = initialState();
    state.pendingMessage = {text:studyRequestText({version:1,format:'quiz',intent:'review',trainingId:'personal:go',domainId:'go',trainingName:'Go'})};
    const fixture = client(state);
    vi.mocked(fixture.api.trainingAction).mockResolvedValue(context);
    render(<App api={fixture.api}/>);
    const outbox = await screen.findByRole('region', {name:'Message à envoyer'});
    expect(within(outbox).queryByText(/tutor-study-request/)).toBeNull();
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    const connected = fixture.getState(); connected.agentConnected = true; connected.tutorStatus.connected = true;
    act(() => fixture.broadcast(connected));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    act(() => fixture.broadcast(fixture.getState()));
    await waitFor(() => expect(screen.queryByRole('region', {name:'Message à envoyer'})).toBeNull());
    expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1);
  });

  it('ne lance aucun exercice si la sélection serveur est en conflit', async () => {
    const state = initialState(true);
    state.pendingMessage = {text:studyRequestText({version:1,format:'quiz',intent:'review',trainingId:'personal:go',domainId:'go'})};
    const fixture = client(state);
    vi.mocked(fixture.api.trainingAction).mockImplementation(async action => {
      if (action === 'get_conversation_context') return {...context,training_id:'personal:python',domain_id:'python'};
      throw new Error('La formation a changé. Rechargez son contexte.');
    });
    render(<App api={fixture.api}/>);
    expect(await screen.findByRole('alert')).toHaveProperty('textContent',expect.stringContaining('La formation a changé'));
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    expect(fixture.getState().pendingMessage).toBeTruthy();
  });

  it('attend l’identité institutionnelle avant de charger et activer le planning', async () => {
    const state = initialState(true); state.settings.tutor = {...state.settings.tutor,mode:'remote',url:'https://institution.example'};
    const fixture = client(state); const doc = trainingFixture();
    let resolveOverview!:(value:Awaited<ReturnType<typeof fixture.api.tutorOverview>>)=>void;
    vi.mocked(fixture.api.tutorOverview).mockImplementation(() => new Promise(resolve => {resolveOverview = resolve;}));
    vi.mocked(fixture.api.trainingAction).mockResolvedValue({items:[doc.training],next_after:''});
    render(<App api={fixture.api}/>); const user = userEvent.setup();
    await user.click(await screen.findByRole('button', {name:'Planning'}));
    expect(screen.getByText('Lecture du planning…')).toBeTruthy();
    expect(fixture.api.trainingAction).not.toHaveBeenCalled();
    const overview = {identity:{tenantId:'tenant-a',userId:'learner-a',membershipId:'member-a'},context:{},formations:{},availableFormations:{}};
    await act(async () => resolveOverview(overview));
    await user.click(await screen.findByRole('switch',{name:'Répétition espacée pour Go'}));
    await waitFor(() => expect(fixture.api.saveSettings).toHaveBeenCalled());
    const preferences = fixture.getState().settings.reviewPlans!;
    expect(preferences[reviewPlanKey(reviewScope(state,overview),'personal:go')]).toEqual({enabled:true,format:'quiz'});
    expect(preferences[reviewPlanKey(reviewScope(state,null),'personal:go')]).toBeUndefined();
  });

  it('ouvre un nouveau chat pour une révision après changement de connexion, sans y copier l’ancien historique', async () => {
    const state = withActivity();
    state.pendingMessage = {text:studyRequestText({version:1,format:'quiz',intent:'review',trainingId:'personal:go',domainId:'go'}),conversationId:conversation.id};
    const fixture = client(state);
    vi.mocked(fixture.api.startConversation).mockImplementation(async (_intentId,resumeId) => {
      if (resumeId) throw new Error('Cette séance appartient à une autre connexion Tutor. Rétablissez le compte et l’institution d’origine.');
      const next = fixture.getState(); const created = {...structuredClone(conversation),id:'new-chat',messages:[],activeTraining:{...context,conversation_id:'new-chat'}};
      next.conversations.push(created); next.activeConversationId = created.id; fixture.setState(next); return created;
    });
    vi.mocked(fixture.api.trainingAction).mockResolvedValue({...context,conversation_id:'new-chat'});
    render(<App api={fixture.api}/>);
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    expect(vi.mocked(fixture.api.sendPrompt).mock.calls[0][0]).toBe('new-chat');
    expect(fixture.getState().conversations.find(item => item.id === 'new-chat')!.messages).toHaveLength(1);
    expect(fixture.getState().conversations.find(item => item.id === conversation.id)!.messages[0].text).toBe(activityText(quiz));
  });

  it('refuse visuellement une carte devenue ancienne après une révision du programme', async () => {
    const state = withActivity(); state.conversations[0].activeTraining = {...context,revision:'2'};
    const fixture = client(state); render(<App api={fixture.api}/>);
    const input = await screen.findByRole('radio',{name:/5/});
    expect((input.closest('fieldset') as HTMLFieldSetElement).disabled).toBe(true);
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('ne laisse pas continuer une ancienne carte après une réponse sans nouvelle activité', async () => {
    const state = withActivity();
    state.conversations[0].messages.push({id:'answer',role:'user',status:'complete',createdAt:conversation.createdAt,trainingContext:context,
      text:serializeLearningResponse({version:1,kind:'quiz',activityId:quiz.id,trainingId:context.training_id,domainId:'go',concept:'variables',selectedOptionId:'b',attemptId:quiz.attemptId,responseTimeSeconds:8})},
      {id:'continued',role:'user',status:'complete',createdAt:conversation.createdAt,text:studyRequestText({version:1,format:'quiz',intent:'review',trainingId:context.training_id})},
      {id:'no-review',role:'assistant',status:'complete',createdAt:conversation.createdAt,text:'Aucune révision disponible pour cette formation.'});
    const fixture = client(state); render(<App api={fixture.api}/>);
    await screen.findByText('Aucune révision disponible pour cette formation.');
    expect(screen.queryByRole('button',{name:'Continuer'})).toBeNull();
  });

  it('masque deux cartes ayant le même identifiant au lieu de reprendre une réponse ambiguë', async () => {
    const state = withActivity(); state.conversations[0].messages.push({...state.conversations[0].messages[0],id:'duplicate'});
    const fixture = client(state); render(<App api={fixture.api}/>);
    expect(await screen.findAllByText('Cette activité ne peut pas être affichée. Demandez au tuteur de la reformuler.')).toHaveLength(2);
    expect(screen.queryByRole('radio')).toBeNull();
  });

  it('permet de relire le contexte du planning après une erreur transitoire', async () => {
    const fixture = client(initialState(true));
    vi.mocked(fixture.api.tutorOverview).mockRejectedValueOnce(new Error('Serveur momentanément indisponible.'));
    render(<App api={fixture.api}/>); const user = userEvent.setup();
    await user.click(await screen.findByRole('button',{name:'Planning'}));
    await user.click(await screen.findByRole('button',{name:'Réessayer'}));
    await screen.findByText('Votre planning se remplira avec vos apprentissages');
    expect(fixture.api.tutorOverview).toHaveBeenCalledTimes(2);
  });
});
