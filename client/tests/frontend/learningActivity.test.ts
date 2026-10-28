import { describe, expect, it } from 'vitest';
import { parseLearningContent, parseLearningResponse, responseMatchesActivity, serializeLearningResponse, validateLearningActivity, validateLearningResponse } from '../../src/learningActivity';
import type { LearningActivity, LearningResponse } from '../../src/learningActivity';

const activity: LearningActivity = { version: 1, id: 'activity-1', kind: 'quiz', trainingId: 'personal:go', domainId: 'go', concept: 'variables',
  question: 'Quelle valeur contient x ?', attemptId: 'attempt-1', activityType: 'PRACTICE', rubricJson: '{"criteria":[{"id":"value","max_score":1}],"passing_score":1}', options: [{ id: 'a', text: '3' }, { id: 'b', text: '5' }], correctOptionId: 'b', explanation: 'On ajoute deux à trois.' };
const response: LearningResponse = { version: 1, activityId: activity.id, kind: 'quiz', trainingId: activity.trainingId, domainId: activity.domainId,
  concept: activity.concept, attemptId: 'attempt-1', selectedOptionId: 'b', responseTimeSeconds: 12 };
const block = (value: unknown) => '```tutor-activity\n' + JSON.stringify(value) + '\n```';

describe('contrat visuel de récupération', () => {
  it('conserve le texte autour des activités validées et préserve les extraits de code ordinaires', () => {
    const before = 'À vous.\n\n```go\nx := 3\n```\n\n';
    expect(parseLearningContent(before + block(activity) + '\nAprès.', { complete: true })).toEqual([
      { type: 'text', text: before }, { type: 'activity', activity }, { type: 'text', text: 'Après.' },
    ]);
    const example = '````markdown\n' + block(activity) + '\n````';
    expect(parseLearningContent(example, { complete: true })).toEqual([{ type: 'text', text: example }]);
  });

  it('ne révèle ni correction ni JSON quand un bloc est incomplet, invalide ou encore en streaming', () => {
    for (const source of [block(activity), '```tutor-activity\n{"answer":"secret"', '```tutor-act', '~~~tutor-activity\n{"answer":"secret"}\n~~~']) {
      const result = parseLearningContent(source, { complete: false });
      expect(result).toEqual([{ type: 'pending' }]);
      expect(JSON.stringify(result)).not.toContain('secret');
      expect(JSON.stringify(result)).not.toContain('correctOptionId');
    }
    for (const source of ['```tutor-activity\n{"answer":"secret"}\n```', '```tutor-activity\n{"answer":"secret"', block({ ...activity, options: [{ id: 'a', text: '1' }] })]) {
      expect(parseLearningContent(source, { complete: true })).toEqual([{ type: 'invalid' }]);
    }
  });

  it('refuse les payloads ambigus, les identifiants dupliqués et les contenus non bornés', () => {
    for (const invalid of [
      { ...activity, version: 2 }, { ...activity, success: true }, { ...activity, trainingId: '' }, { ...activity, options: [{ id: 'a', text: '1' }, { id: 'a', text: '2' }] },
      { ...activity, correctOptionId: 'absent' }, { ...activity, question: 'x'.repeat(12001) }, { ...activity, sessionId: 42 },
      { ...activity, options: [{ id: 'a', text: 'x'.repeat(2001) }, { id: 'b', text: '2' }] },
      { ...activity, id: 'é'.repeat(129) },
      { ...activity, concept: 'var\0iables' },
      { ...activity, kind: 'flashcard', answer: 'func', attemptId: 'not-an-exam' },
    ]) expect(validateLearningActivity(invalid)).toBeUndefined();
    expect(parseLearningContent(block({ ...activity, extra: 'x'.repeat(48 * 1024) }), { complete: true })).toEqual([{ type: 'invalid' }]);
    expect(parseLearningContent(block(activity) + '\n' + block({ ...activity, id: 'second' }), { complete: true })).toEqual([{ type: 'invalid' }, { type: 'invalid' }]);
  });

  it('conserve le contrat de notation pour une reprise et exige la rubrique lorsqu’une tentative est liée', () => {
    expect(validateLearningActivity(activity)).toEqual(activity);
    for (const invalid of [
      { ...activity, activityType: undefined }, { ...activity, rubricJson: undefined }, { ...activity, activityType: 'REST' },
      { ...activity, rubricJson: '[]' }, { ...activity, rubricJson: 'null' }, { ...activity, rubricJson: '{broken' },
      { ...activity, rubricJson: '{"long":"' + 'x'.repeat(16 * 1024) + '"}' },
      { ...activity, activityType: 'TRANSFER_PROBE' }, { ...activity, activityType: 'TRANSFER_PROBE', transferDimension: 'unknown' },
      { ...activity, transferDimension: 'near' },
    ]) expect(validateLearningActivity(invalid)).toBeUndefined();
    expect(validateLearningActivity({ ...activity, activityType: 'TRANSFER_PROBE', transferDimension: 'near' })).toMatchObject({ activityType: 'TRANSFER_PROBE', transferDimension: 'near' });
    const unbound = { ...activity, attemptId: undefined, activityType: undefined, rubricJson: undefined };
    expect(validateLearningActivity(unbound)?.kind).toBe('quiz');
    const card = { version: 1, id: 'card', kind: 'flashcard', trainingId: 'go', domainId: 'go', concept: 'functions', question: 'Question', answer: 'Réponse' };
    expect(validateLearningActivity({ ...card, activityType: 'RECALL_EXERCISE' })).toBeUndefined();
    expect(validateLearningActivity({ ...card, rubricJson: '{}' })).toBeUndefined();
  });

  it('conserve les réponses sans score client et vérifie leur formation, tentative et option', () => {
    const serialized = serializeLearningResponse(response);
    expect(parseLearningResponse('Ma réponse : 5\n\n' + serialized)).toEqual(response);
    expect(serialized).not.toMatch(/success|mastery|confidence/);
    expect(responseMatchesActivity(activity, response)).toBe(true);
    for (const other of [{ ...response, trainingId: 'personal:python' }, { ...response, domainId: 'python' }, { ...response, attemptId: 'other' },
      { ...response, selectedOptionId: 'not-an-option' }, { ...response, activityId: 'other' }]) expect(responseMatchesActivity(activity, other)).toBe(false);
    expect(parseLearningResponse(serialized + '\n' + serialized)).toBeUndefined();
    expect(parseLearningContent('Ma réponse : 5\n\n' + serialized, { complete: true })).toEqual([{ type: 'text', text: 'Ma réponse : 5\n\n' }]);
  });

  it('distingue autoévaluation et réponse vérifiable, sans durée négative ou infinie', () => {
    const flashcard = { version: 1, id: 'card', kind: 'flashcard', trainingId: 'personal:go', domainId: 'go', concept: 'functions', question: 'Quel mot-clé ?', answer: 'func' };
    const answer = { version: 1, activityId: 'card', kind: 'flashcard', trainingId: 'personal:go', domainId: 'go', concept: 'functions', rating: 'hard', responseTimeSeconds: 4 };
    expect(validateLearningActivity(flashcard)?.kind).toBe('flashcard');
    expect(validateLearningResponse(answer)).toEqual(answer);
    for (const invalid of [{ ...answer, success: true }, { ...answer, rating: 'correct' }, { ...answer, attemptId: 'exam' }, { ...answer, selectedOptionId: 'a' },
      { ...response, rating: 'easy' }, { ...answer, responseTimeSeconds: -1 }, { ...answer, responseTimeSeconds: Infinity }, { ...answer, responseTimeSeconds: 86401 }]) {
      expect(validateLearningResponse(invalid)).toBeUndefined();
    }
  });
});
