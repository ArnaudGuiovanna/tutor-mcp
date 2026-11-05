import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { LearningActivityCard } from '../../src/LearningActivity';
import type { ExerciseActivity, FlashcardActivity, LearningResponse, QuizActivity } from '../../src/learningActivityModel';

const quiz: QuizActivity = { version: 1, id: 'quiz-1', kind: 'quiz', trainingId: 'personal:go', domainId: 'go', concept: 'variables',
  question: 'Quelle valeur contient x ?', options: [{ id: 'a', text: '3' }, { id: 'b', text: '5' }], correctOptionId: 'b', explanation: 'On ajoute deux à trois.', attemptId: 'attempt-1', activityType: 'PRACTICE', rubricJson: '{"criteria":[{"id":"value","max_score":1}],"passing_score":1}' };
const flashcard: FlashcardActivity = { version: 1, id: 'flashcard-1', kind: 'flashcard', trainingId: 'personal:english', domainId: 'english', concept: 'past',
  question: 'Comment dit-on « hier » en anglais ?', answer: '**Yesterday**' };

describe('activités visuelles dans la conversation', () => {
  it('sépare l’aide et la réponse d’exercice, puis verrouille la réponse enregistrée sans inventer de correction', async () => {
    const exercise: ExerciseActivity = { version: 1, id: 'exercise-1', kind: 'exercise', trainingId: 'personal:go', domainId: 'go', concept: 'variables', question: 'Écrivez une addition.', attemptId: 'attempt-1', activityType: 'PRACTICE', rubricJson: '{}' };
    const user = userEvent.setup(); const submit = vi.fn(async (_response:LearningResponse) => true); const hint = vi.fn();
    const view = render(<LearningActivityCard activity={exercise} onRespond={submit} onHint={hint}/>);
    expect(screen.getByRole('button', { name: 'Valider' }).matches(':disabled')).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Un indice' }));
    expect(hint).toHaveBeenCalledTimes(1); expect(submit).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText('Votre réponse à l’exercice'), 'x := 3\nx += 2');
    await user.click(screen.getByRole('button', { name: 'Valider' }));
    expect(submit).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({kind:'exercise',attemptId:'attempt-1',answer:'x := 3\nx += 2'}));
    expect(submit.mock.calls[0][0]).not.toHaveProperty('success');
    expect(screen.queryByText('Bonne réponse')).toBeNull();
    view.rerender(<LearningActivityCard activity={exercise} onRespond={submit} response={submit.mock.calls[0][0]}/>);
    expect(screen.getByLabelText('Votre réponse à l’exercice').matches(':disabled')).toBe(true);
    expect(screen.getByRole('status').textContent).toContain('Réponse enregistrée');
  });
  it('attend une réponse validée pour montrer la correction, puis conserve une sélection en lecture seule', async () => {
    const user = userEvent.setup(); const submit = vi.fn(async (_response:LearningResponse) => true); const next = vi.fn();
    const view = render(<LearningActivityCard activity={quiz} onRespond={submit} onContinue={next} />);
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    expect((screen.getByRole('button', { name: 'Valider' }) as HTMLButtonElement).disabled).toBe(true);
    await user.click(screen.getByRole('radio', { name: '3' }));
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Valider' }));
    expect(submit).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ activityId: quiz.id, kind: 'quiz', selectedOptionId: 'a', attemptId: 'attempt-1' }));
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    view.rerender(<LearningActivityCard activity={quiz} onRespond={submit} onContinue={next} response={submit.mock.calls[0][0]} />);
    expect(await screen.findByText(quiz.explanation)).toBeTruthy();
    expect(screen.getByText('Reprenons ce point')).toBeTruthy();
    expect(screen.getByRole('radio', { name: '3' }).matches(':disabled')).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Question suivante' }));
    expect(next).toHaveBeenCalledTimes(1);
  });

  it('supporte la réponse au clavier avec les contrôles natifs', async () => {
    // jsdom lacks CSS.escape, used by user-event for native radio navigation.
    vi.stubGlobal('CSS', { escape: (value: string) => value.replace(/[^\w-]/g, character => '\\' + character.codePointAt(0)!.toString(16) + ' ') });
    const user = userEvent.setup(); const submit = vi.fn(async (_response:LearningResponse) => true);
    const view = render(<LearningActivityCard activity={quiz} onRespond={submit} />);
    await user.tab(); expect(document.activeElement).toBe(screen.getByRole('radio', { name: '3' }));
    await user.keyboard('{ArrowRight}');
    await user.tab(); await user.keyboard('{Enter}');
    expect(submit).toHaveBeenCalledWith(expect.objectContaining({ selectedOptionId: 'b' }));
    expect(screen.queryByText('Bonne réponse')).toBeNull();
    view.rerender(<LearningActivityCard activity={quiz} onRespond={submit} response={submit.mock.calls[0][0]}/>);
    expect(await screen.findByText('Bonne réponse')).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it('ne soumet qu’une fois pendant l’envoi et permet de réessayer après un échec sans révéler la correction', async () => {
    const user = userEvent.setup(); let resolve!: (accepted: boolean) => void;
    const submit = vi.fn((_response:LearningResponse) => new Promise<boolean>(done => { resolve = done; }));
    const view = render(<LearningActivityCard activity={quiz} onRespond={submit} />);
    await user.click(screen.getByRole('radio', { name: '5' }));
    await user.dblClick(screen.getByRole('button', { name: 'Valider' }));
    expect(submit).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    await act(async () => resolve(false));
    expect(screen.getByRole('alert')).toBeTruthy(); expect(screen.queryByText(quiz.explanation)).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Valider' }));
    await act(async () => resolve(true));
    expect(submit).toHaveBeenCalledTimes(2); expect(screen.queryByText(quiz.explanation)).toBeNull();
    view.rerender(<LearningActivityCard activity={quiz} onRespond={submit} response={submit.mock.calls[1][0]}/>);
    expect(await screen.findByText(quiz.explanation)).toBeTruthy();
  });

  it('révèle une flashcard seulement au clic et envoie une autoévaluation sans succès inventé', async () => {
    const user = userEvent.setup(); const submit = vi.fn(async (_response: LearningResponse) => true);
    const view = render(<LearningActivityCard activity={flashcard} onRespond={submit} />);
    expect(screen.queryByText('Yesterday')).toBeNull(); expect(screen.queryByRole('button', { name: 'À revoir' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Retourner la carte' }));
    expect(await screen.findByText('Yesterday')).toBeTruthy(); expect(submit).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Retrouvé avec effort' }));
    expect(submit).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ kind: 'flashcard', rating: 'hard' }));
    expect(submit.mock.calls[0][0]).not.toHaveProperty('attemptId');
    expect(submit.mock.calls[0][0]).not.toHaveProperty('success');
    view.rerender(<LearningActivityCard activity={flashcard} onRespond={submit} response={submit.mock.calls[0][0]}/>);
    expect(screen.getByText('Votre déclaration reste distincte d’une réponse vérifiée dans un quiz.')).toBeTruthy();
  });

  it('reconstitue une réponse historique sans la renvoyer et ignore une réponse d’une autre formation', () => {
    const submit = vi.fn(async (_response:LearningResponse) => true);
    const response: LearningResponse = { version: 1, activityId: quiz.id, trainingId: quiz.trainingId, domainId: quiz.domainId, concept: quiz.concept,
      kind: 'quiz', selectedOptionId: 'b', attemptId: 'attempt-1', responseTimeSeconds: 10 };
    const view = render(<LearningActivityCard activity={quiz} response={response} onRespond={submit} />);
    expect(screen.getByText(quiz.explanation)).toBeTruthy(); expect(submit).not.toHaveBeenCalled();
    view.rerender(<LearningActivityCard activity={quiz} response={{ ...response, trainingId: 'other' }} onRespond={submit} disabled />);
    expect(screen.queryByText(quiz.explanation)).toBeNull(); expect(screen.getByRole('radio', { name: '5' }).matches(':disabled')).toBe(true);
  });

  it('ne conserve pas un verso révélé quand l’activité change et n’exécute pas de contenu HTML', async () => {
    const user = userEvent.setup(); const submit = vi.fn(async (_response:LearningResponse) => true);
    const view = render(<LearningActivityCard activity={flashcard} onRespond={submit} />);
    await user.click(screen.getByRole('button', { name: 'Retourner la carte' }));
    view.rerender(<LearningActivityCard activity={{ ...flashcard, id: 'another', question: '<script>alert(1)</script> Question', answer: '<img src="https://example.org/pixel">Hidden' }} onRespond={submit} />);
    expect(screen.queryByText('Yesterday')).toBeNull(); expect(view.container.querySelector('script,img')).toBeNull(); expect(screen.queryByText('Hidden')).toBeNull();
    expect(screen.getByRole('button', { name: 'Retourner la carte' })).toBeTruthy();
  });
});
