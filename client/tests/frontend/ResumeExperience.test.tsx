import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App from '../../src/App';
import { parseLearningResponse } from '../../src/learningActivityModel';
import { parseResumeRequest, resumeRequestText } from '../../src/resumeRequests';
import { parseStudyRequest } from '../../src/studyRequests';
import type { ConversationContext } from '../../src/types';
import { client, conversation, initialState } from './clientFixture';
import { trainingFixture } from './trainingFixture';

const context: ConversationContext = { conversation_id: conversation.id, training_id: 'personal:go', domain_id: 'go', name: 'Go', revision: '1', selection_version: 1 };
const exercise = { version: 1, id: 'exercise-go', kind: 'exercise', trainingId: 'personal:go', domainId: 'go', concept: 'variables', question: 'Écrivez une addition en Go.', attemptId: 'attempt-go', activityType: 'PRACTICE', rubricJson: '{"criteria":[],"answer_key":"PRIVATE_RUBRIC"}' };
const activityText = '```tutor-activity\n' + JSON.stringify(exercise) + '\n```';

describe('reprise préparée avant formulation par le harness', () => {
  it('transmet la reprise explicite depuis le bouton sans sélection MCP ni quiz imposé côté interface', async () => {
    const fixture = client(initialState(true)); const doc = trainingFixture();
    doc.training.progress = [{ concept: 'variables', mastery: .6, last_review: '2026-11-04T10:00:00Z' }];
    vi.mocked(fixture.api.trainingAction).mockImplementation(async action => {
      if (action === 'list_training_library') return { items: [doc.training], next_after: '' };
      throw new Error('Unexpected frontend tool: ' + action);
    });
    render(<App api={fixture.api}/>);
    await userEvent.click(await screen.findByRole('button', { name: 'Reprendre' }, { timeout: 5000 }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    const sent = vi.mocked(fixture.api.sendPrompt).mock.calls[0][1];
    expect(parseResumeRequest(sent)).toEqual({ version: 1, trainingId: 'personal:go', domainId: 'go', trainingName: 'Go' });
    expect(sent).not.toContain('format');
    expect(vi.mocked(fixture.api.trainingAction).mock.calls.every(([action]) => action === 'list_training_library')).toBe(true);
    expect(fixture.api.queuePrompt).toHaveBeenCalledWith(sent, undefined);
    expect(screen.queryByText(/tutor-resume-request/)).toBeNull();
  });

  it('transmet les reprises en conversation telles quelles pour résolution serveur sans dépendre de la page locale de formations', async () => {
    const fixture = client(initialState(true)); render(<App api={fixture.api}/>);
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Message au tuteur'), 'Reprendre la formation harness');
    await user.click(screen.getByRole('button', { name: 'Envoyer (Ctrl Entrée)' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledWith(conversation.id, 'Reprendre la formation harness'));
    expect(vi.mocked(fixture.api.trainingAction).mock.calls.some(([action]) => action === 'select_conversation_training')).toBe(false);
  });

  it('préserve une reprise en attente comme intention structurée sans offrir de modifier son identité', async () => {
    const state = initialState();
    state.pendingMessage = { text: resumeRequestText({ version: 1, trainingId: 'personal:go', trainingName: 'Go' }), paused: true };
    const fixture = client(state); render(<App api={fixture.api}/>);
    const outbox = await screen.findByRole('region', { name: 'Message à envoyer' });
    expect(within(outbox).getByText('Reprendre Go.')).toBeTruthy();
    expect(within(outbox).queryByRole('button', { name: 'Modifier' })).toBeNull();
    expect(outbox.textContent).not.toMatch(/trainingId|tutor-resume-request/);
    await userEvent.click(within(outbox).getByRole('button', { name: 'Retirer' }));
    await waitFor(() => expect(fixture.getState().pendingMessage).toBeNull());
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('rend le choix renvoyé par le serveur et resoumet son identité explicite sans noter la clarification', async () => {
    const state = initialState(true); state.activeConversationId = conversation.id;
    const choice = { version: 1, requestText: 'Reprendre Go', choices: [{ trainingId: 'go-advanced', trainingName: 'Go avancé', domainId: 'advanced' }], hasMore: true };
    state.conversations = [{ ...conversation, messages: [{ id: 'choice', role: 'assistant', status: 'complete', createdAt: conversation.createdAt, text: '```tutor-training-choice\n' + JSON.stringify(choice) + '\n```' }] }];
    const fixture = client(state); render(<App api={fixture.api}/>);
    await userEvent.click(await screen.findByRole('button', { name: 'Go avancé' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    const sent = vi.mocked(fixture.api.sendPrompt).mock.calls[0][1];
    expect(parseResumeRequest(sent)).toEqual({ version: 1, ...choice.choices[0] });
    expect(parseLearningResponse(sent)).toBeUndefined();
    expect(screen.getByRole('button', { name: 'Go avancé' }).matches(':disabled')).toBe(true);
    expect(fixture.api.acknowledgeLearningDelivery).not.toHaveBeenCalled();
  });

  it('répond à l’exercice depuis le seul composeur, sépare une question au tuteur, puis poursuit selon une nouvelle décision serveur', async () => {
    const state = initialState(true); state.activeConversationId = conversation.id;
    state.conversations = [{ ...conversation, activeTraining: context, messages: [{ id: 'exercise', role: 'assistant', status: 'complete', createdAt: conversation.createdAt, trainingContext: context, text: activityText }] }];
    const fixture = client(state); render(<App api={fixture.api}/>); const user = userEvent.setup();
    await screen.findByText('Écrivez une addition en Go.');
    expect(screen.queryByText(/PRIVATE_RUBRIC/)).toBeNull();
    // One input only: the exercise card no longer duplicates the composer.
    expect(screen.getAllByRole('textbox')).toHaveLength(1);
    expect(document.querySelectorAll('textarea')).toHaveLength(1);
    expect((screen.getByRole('radio', { name: 'Réponse à l’exercice' }) as HTMLInputElement).checked).toBe(true);
    expect(screen.getByLabelText('Message au tuteur').getAttribute('placeholder')).toBe('Votre réponse à l’exercice…');

    await user.click(screen.getByRole('radio', { name: 'Question au tuteur' }));
    expect(document.activeElement).toBe(screen.getByLabelText('Message au tuteur'));
    await user.type(screen.getByLabelText('Message au tuteur'), 'Je ne comprends pas cette consigne');
    await user.click(screen.getByRole('button', { name: 'Envoyer (Ctrl Entrée)' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    expect(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1]).toBe('Je ne comprends pas cette consigne');
    expect(parseLearningResponse(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1])).toBeUndefined();

    // The answer mode is the default again for the next message.
    await waitFor(() => expect((screen.getByRole('radio', { name: 'Réponse à l’exercice' }) as HTMLInputElement).checked).toBe(true));
    await waitFor(() => expect(screen.getByLabelText('Message au tuteur').matches(':disabled')).toBe(false));
    await user.type(screen.getByLabelText('Message au tuteur'), 'x := 3\nx += 2');
    await user.click(screen.getByRole('button', { name: 'Envoyer (Ctrl Entrée)' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(2));
    const answer = parseLearningResponse(vi.mocked(fixture.api.sendPrompt).mock.calls[1][1]);
    expect(answer).toMatchObject({ version: 1, kind: 'exercise', activityId: 'exercise-go', trainingId: 'personal:go', domainId: 'go', concept: 'variables', attemptId: 'attempt-go', answer: 'x := 3\nx += 2' });
    expect(answer?.responseTimeSeconds).toBeGreaterThanOrEqual(0);
    expect(answer).not.toHaveProperty('success');
    expect(await screen.findByText('Retour confirmé')).toBeTruthy();
    expect(screen.getByText(/Réponse enregistrée/)).toBeTruthy();
    // Once answered, the composer is an ordinary message field again.
    expect(screen.queryByRole('radio', { name: 'Réponse à l’exercice' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Activité suivante' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(3));
    expect(parseResumeRequest(vi.mocked(fixture.api.sendPrompt).mock.calls[2][1])).toEqual({ version: 1, trainingId: context.training_id, domainId: context.domain_id, trainingName: 'Go' });
  });

  it('note une réponse commençant par une barre oblique qui n’est pas une commande reconnue', async () => {
    const state = initialState(true); state.activeConversationId = conversation.id;
    state.conversations = [{ ...conversation, activeTraining: context, messages: [{ id: 'exercise', role: 'assistant', status: 'complete', createdAt: conversation.createdAt, trainingContext: context, text: activityText }] }];
    const fixture = client(state); render(<App api={fixture.api}/>); const user = userEvent.setup();
    await screen.findByText('Écrivez une addition en Go.');
    expect((screen.getByRole('radio', { name: 'Réponse à l’exercice' }) as HTMLInputElement).checked).toBe(true);
    await user.click(screen.getByLabelText('Message au tuteur'));
    await user.paste('/^a+$/');
    await user.click(screen.getByRole('button', { name: 'Envoyer (Ctrl Entrée)' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    expect(parseLearningResponse(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1])).toMatchObject({ kind: 'exercise', activityId: 'exercise-go', answer: '/^a+$/' });
  });

  it('sort de la voie réponse pour une commande reconnue comme /quiz', async () => {
    const state = initialState(true); state.activeConversationId = conversation.id;
    state.conversations = [{ ...conversation, activeTraining: context, messages: [{ id: 'exercise', role: 'assistant', status: 'complete', createdAt: conversation.createdAt, trainingContext: context, text: activityText }] }];
    const fixture = client(state);
    vi.mocked(fixture.api.trainingAction).mockImplementation(async action => {
      if (action === 'get_conversation_context' || action === 'select_conversation_training') return context;
      throw new Error('Unexpected frontend tool: ' + action);
    });
    render(<App api={fixture.api}/>); const user = userEvent.setup();
    await screen.findByText('Écrivez une addition en Go.');
    await user.click(screen.getByLabelText('Message au tuteur'));
    await user.paste('/quiz variables');
    await user.click(screen.getByRole('button', { name: 'Envoyer (Ctrl Entrée)' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    const sent = vi.mocked(fixture.api.sendPrompt).mock.calls[0][1];
    expect(parseLearningResponse(sent)).toBeUndefined();
    expect(parseStudyRequest(sent)).toMatchObject({ format: 'quiz', intent: 'practice' });
  });

  it('demande un indice depuis la barre du composeur sans soumettre de réponse', async () => {
    const state = initialState(true); state.activeConversationId = conversation.id;
    state.conversations = [{ ...conversation, activeTraining: context, messages: [{ id: 'exercise', role: 'assistant', status: 'complete', createdAt: conversation.createdAt, trainingContext: context, text: activityText }] }];
    const fixture = client(state); render(<App api={fixture.api}/>);
    await userEvent.click(await screen.findByRole('button', { name: 'Un indice' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    const sent = vi.mocked(fixture.api.sendPrompt).mock.calls[0][1];
    expect(sent).toContain('Écrivez une addition en Go.');
    expect(parseLearningResponse(sent)).toBeUndefined();
  });

  it('ne traite pas comme réponse un exercice d’une autre formation que la formation active', async () => {
    const state = initialState(true); state.activeConversationId = conversation.id;
    const other: ConversationContext = { ...context, training_id: 'personal:rust', domain_id: 'rust', name: 'Rust' };
    state.conversations = [{ ...conversation, activeTraining: other, messages: [{ id: 'exercise', role: 'assistant', status: 'complete', createdAt: conversation.createdAt, trainingContext: context, text: activityText }] }];
    const fixture = client(state); render(<App api={fixture.api}/>); const user = userEvent.setup();
    await screen.findByText('Écrivez une addition en Go.');
    expect(screen.queryByRole('radio', { name: 'Réponse à l’exercice' })).toBeNull();
    await user.type(screen.getByLabelText('Message au tuteur'), 'x := 3');
    await user.click(screen.getByRole('button', { name: 'Envoyer (Ctrl Entrée)' }));
    await waitFor(() => expect(fixture.api.sendPrompt).toHaveBeenCalledTimes(1));
    expect(parseLearningResponse(vi.mocked(fixture.api.sendPrompt).mock.calls[0][1])).toBeUndefined();
  });

  it('ne rend pas une activité privée ou en cours de validation comme formulaire de réponse', async () => {
    const state = initialState(true); state.activeConversationId = conversation.id;
    state.conversations = [{ ...conversation, activeTraining: context, messages: [{ id: 'exercise', role: 'assistant', status: 'streaming', privateOutput: true, createdAt: conversation.createdAt, trainingContext: context, text: activityText }] }];
    const fixture = client(state); render(<App api={fixture.api}/>);
    await screen.findByLabelText('Message au tuteur');
    expect(screen.queryByRole('radio', { name: 'Réponse à l’exercice' })).toBeNull();
    expect(screen.queryByText('Écrivez une addition en Go.')).toBeNull();
    const completed = fixture.getState(); completed.conversations[0].messages[0].status = 'complete'; completed.conversations[0].messages[0].privateOutput = false;
    act(() => fixture.broadcast(completed));
    expect(await screen.findByRole('radio', { name: 'Réponse à l’exercice' })).toBeTruthy();
    expect(screen.getAllByRole('textbox')).toHaveLength(1);
  });
});
