import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { LearningActivityCard } from '../../src/LearningActivity';
import type { FlashcardActivity, LearningResponse, QuizActivity } from '../../src/learningActivity';

const quiz: QuizActivity = { version: 1, id: 'quiz-1', kind: 'quiz', trainingId: 'personal:go', domainId: 'go', concept: 'variables',
  question: 'Quelle valeur contient x ?', options: [{ id: 'a', text: '3' }, { id: 'b', text: '5' }], correctOptionId: 'b', explanation: 'On ajoute deux à trois.', attemptId: 'attempt-1', activityType: 'PRACTICE', rubricJson: '{"criteria":[{"id":"value","max_score":1}],"passing_score":1}' };
const flashcard: FlashcardActivity = { version: 1, id: 'flashcard-1', kind: 'flashcard', trainingId: 'personal:english', domainId: 'english', concept: 'past',
  question: 'Comment dit-on « hier » en anglais ?', answer: '**Yesterday**' };

describe('activités visuelles dans la conversation', () => {
  it('attend une réponse validée pour montrer la correction, puis conserve une sélection en lecture seule', async () => {
    const user = userEvent.setup(); const submit = vi.fn(async () => true); const next = vi.fn();
    render(<LearningActivityCard activity={quiz} onRespond={submit} onContinue={next} />);
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    expect((screen.getByRole('button', { name: 'Valider ma réponse' }) as HTMLButtonElement).disabled).toBe(true);
    await user.click(screen.getByRole('radio', { name: '3' }));
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Valider ma réponse' }));
    expect(submit).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ activityId: quiz.id, kind: 'quiz', selectedOptionId: 'a', attemptId: 'attempt-1' }));
    expect(await screen.findByText(quiz.explanation)).toBeTruthy();
    expect(screen.getByText('Reprenons ce point')).toBeTruthy();
    expect(screen.getByRole('radio', { name: '3' }).matches(':disabled')).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Continuer' }));
    expect(next).toHaveBeenCalledTimes(1);
  });

  it('supporte la réponse au clavier avec les contrôles natifs', async () => {
    // jsdom lacks CSS.escape, used by user-event for native radio navigation.
    vi.stubGlobal('CSS', { escape: (value: string) => value.replace(/[^\w-]/g, character => '\\' + character.codePointAt(0)!.toString(16) + ' ') });
    const user = userEvent.setup(); const submit = vi.fn(async () => true);
    render(<LearningActivityCard activity={quiz} onRespond={submit} />);
    await user.tab(); expect(document.activeElement).toBe(screen.getByRole('radio', { name: '3' }));
    await user.keyboard('{ArrowRight}');
    await user.tab(); await user.keyboard('{Enter}');
    expect(submit).toHaveBeenCalledWith(expect.objectContaining({ selectedOptionId: 'b' }));
    expect(await screen.findByText('Réponse correcte')).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it('ne soumet qu’une fois pendant l’envoi et permet de réessayer après un échec sans révéler la correction', async () => {
    const user = userEvent.setup(); let resolve!: (accepted: boolean) => void;
    const submit = vi.fn(() => new Promise<boolean>(done => { resolve = done; }));
    render(<LearningActivityCard activity={quiz} onRespond={submit} />);
    await user.click(screen.getByRole('radio', { name: '5' }));
    await user.dblClick(screen.getByRole('button', { name: 'Valider ma réponse' }));
    expect(submit).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(quiz.explanation)).toBeNull();
    await act(async () => resolve(false));
    expect(screen.getByRole('alert')).toBeTruthy(); expect(screen.queryByText(quiz.explanation)).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Valider ma réponse' }));
    await act(async () => resolve(true));
    expect(submit).toHaveBeenCalledTimes(2); expect(await screen.findByText(quiz.explanation)).toBeTruthy();
  });

  it('révèle une flashcard seulement au clic et envoie une autoévaluation sans succès inventé', async () => {
    const user = userEvent.setup(); const submit = vi.fn(async (_response: LearningResponse) => true);
    render(<LearningActivityCard activity={flashcard} onRespond={submit} />);
    expect(screen.queryByText('Yesterday')).toBeNull(); expect(screen.queryByRole('button', { name: 'Pas retrouvé' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Retourner la carte' }));
    expect(await screen.findByText('Yesterday')).toBeTruthy(); expect(submit).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Retrouvé avec effort' }));
    expect(submit).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ kind: 'flashcard', rating: 'hard' }));
    expect(submit.mock.calls[0][0]).not.toHaveProperty('attemptId');
    expect(submit.mock.calls[0][0]).not.toHaveProperty('success');
    expect(screen.getByText('Votre déclaration reste distincte d’une réponse vérifiée dans un quiz.')).toBeTruthy();
  });

  it('reconstitue une réponse historique sans la renvoyer et ignore une réponse d’une autre formation', () => {
    const submit = vi.fn(async () => true);
    const response: LearningResponse = { version: 1, activityId: quiz.id, trainingId: quiz.trainingId, domainId: quiz.domainId, concept: quiz.concept,
      kind: 'quiz', selectedOptionId: 'b', attemptId: 'attempt-1', responseTimeSeconds: 10 };
    const view = render(<LearningActivityCard activity={quiz} response={response} onRespond={submit} />);
    expect(screen.getByText(quiz.explanation)).toBeTruthy(); expect(submit).not.toHaveBeenCalled();
    view.rerender(<LearningActivityCard activity={quiz} response={{ ...response, trainingId: 'other' }} onRespond={submit} disabled />);
    expect(screen.queryByText(quiz.explanation)).toBeNull(); expect(screen.getByRole('radio', { name: '5' }).matches(':disabled')).toBe(true);
  });

  it('ne conserve pas un verso révélé quand l’activité change et n’exécute pas de contenu HTML', async () => {
    const user = userEvent.setup(); const submit = vi.fn(async () => true);
    const view = render(<LearningActivityCard activity={flashcard} onRespond={submit} />);
    await user.click(screen.getByRole('button', { name: 'Retourner la carte' }));
    view.rerender(<LearningActivityCard activity={{ ...flashcard, id: 'another', question: '<script>alert(1)</script> Question', answer: '<img src="https://example.org/pixel">Hidden' }} onRespond={submit} />);
    expect(screen.queryByText('Yesterday')).toBeNull(); expect(view.container.querySelector('script,img')).toBeNull(); expect(screen.queryByText('Hidden')).toBeNull();
    expect(screen.getByRole('button', { name: 'Retourner la carte' })).toBeTruthy();
  });
});
