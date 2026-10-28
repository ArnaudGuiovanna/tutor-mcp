// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
import { useEffect, useId, useRef, useState } from 'react';
import { t } from './i18n';
import { Icon } from './icons';
import { MessageBody } from './MessageBody';
import type { ClientBridge } from './bridge';
import { responseMatchesActivity } from './learningActivity';
import type { LearningActivity, LearningResponse, RecallRating } from './learningActivity';
import './learningActivity.css';

export interface LearningActivityCardProps {
  activity: LearningActivity;
  response?: LearningResponse;
  disabled?: boolean;
  onRespond: (response: LearningResponse) => Promise<boolean>;
  openLink?: ClientBridge['openExternalLink'];
  onContinue?: () => void;
}

const ratings: { value: RecallRating; label: string }[] = [
  { value: 'again', label: 'Pas retrouvé' },
  { value: 'hard', label: 'Retrouvé avec effort' },
  { value: 'easy', label: 'Retrouvé facilement' },
];

export function LearningActivityCard(props: LearningActivityCardProps) {
  // A context change must never carry a revealed answer/selection into another card.
  const { activity } = props;
  return <ActivityContent key={JSON.stringify([activity.trainingId, activity.domainId, activity.id])} {...props} />;
}

function ActivityContent({ activity, response, disabled = false, onRespond, openLink, onContinue }: LearningActivityCardProps) {
  const inputName = useId();
  const titleId = useId();
  const [selected, setSelected] = useState('');
  const [revealed, setRevealed] = useState(false);
  const [accepted, setAccepted] = useState<LearningResponse>();
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');
  const startedAt = useRef(Date.now());
  const recallSeconds = useRef<number | undefined>(undefined);
  const submissionLock = useRef(false);
  const feedback = useRef<HTMLDivElement>(null);
  const flashcardAnswer = useRef<HTMLDivElement>(null);
  const committed = responseMatchesActivity(activity, response) ? response : accepted;
  const locked = disabled || submitting || !!committed;
  const answerShown = revealed || !!committed;
  const quizCorrect = activity.kind === 'quiz' && committed?.kind === 'quiz' && activity.correctOptionId === committed.selectedOptionId;
  const selectedId = committed?.kind === 'quiz' ? committed.selectedOptionId : selected;
  const elapsed = () => Math.min(86400, Math.max(0, (Date.now() - startedAt.current) / 1000));

  useEffect(() => { if (accepted) feedback.current?.focus(); }, [accepted]);
  useEffect(() => { if (revealed) flashcardAnswer.current?.focus(); }, [revealed]);

  async function respond(rating?: RecallRating) {
    if (locked || submissionLock.current || (activity.kind === 'quiz' && !selected) || (activity.kind === 'flashcard' && (!revealed || !rating))) return;
    const base = { version: 1 as const, activityId: activity.id, trainingId: activity.trainingId, domainId: activity.domainId,
      concept: activity.concept, responseTimeSeconds: activity.kind === 'flashcard' ? recallSeconds.current ?? elapsed() : elapsed() };
    const answer: LearningResponse = activity.kind === 'quiz'
      ? { ...base, kind: 'quiz', selectedOptionId: selected, ...(activity.attemptId ? { attemptId: activity.attemptId } : {}) }
      : { ...base, kind: 'flashcard', rating: rating! };
    submissionLock.current = true; setSubmitting(true); setError('');
    try {
      if (await onRespond(answer)) setAccepted(answer);
      else setError(t('Votre réponse n’a pas pu être envoyée. Réessayez.'));
    } catch {
      setError(t('Votre réponse n’a pas pu être envoyée. Réessayez.'));
    } finally { submissionLock.current = false; setSubmitting(false); }
  }

  return <section className={`learning-card ${activity.kind}`} aria-labelledby={titleId} aria-busy={submitting}>
    <header className="learning-card-heading"><span className="learning-card-kind"><Icon name={activity.kind === 'quiz' ? 'check' : 'copy'} size={18} />{activity.kind === 'quiz' ? t('Quiz de récupération') : t('Flashcard')}</span><span className="learning-card-concept">{activity.concept}</span></header>
    <div id={titleId} className="learning-question"><MessageBody text={activity.question} openLink={openLink} /></div>
    {activity.kind === 'quiz' ? <>
      <fieldset className="learning-quiz-options" disabled={locked}>
        <legend className="visually-hidden">{t('Choisissez une réponse')}</legend>
        {activity.options.map((option, index) => <label className={`learning-quiz-option ${selectedId === option.id ? 'selected' : ''} ${committed && option.id === activity.correctOptionId ? 'correct' : ''} ${committed && selectedId === option.id && !quizCorrect ? 'incorrect' : ''}`} key={option.id}>
          <input type="radio" name={inputName} value={option.id} checked={selectedId === option.id} onChange={() => setSelected(option.id)} />
          <span className="learning-option-letter" aria-hidden="true">{String.fromCharCode(65 + index)}</span>
          <span className="learning-option-text">{option.text}</span>
          {committed && option.id === activity.correctOptionId && <span className="learning-answer-mark"><Icon name="check" size={18} /><span className="visually-hidden">{t('Réponse proposée par la correction')}</span></span>}
        </label>)}
      </fieldset>
      {!committed && <div className="learning-card-actions"><span className="learning-card-hint">{t('Répondez avant de consulter la correction.')}</span><button className="button blue" disabled={locked || !selected} onClick={() => void respond()}>{submitting ? t('Envoi…') : t('Valider ma réponse')}</button></div>}
      {committed && <div ref={feedback} className={`learning-feedback ${quizCorrect ? 'correct' : 'incorrect'}`} tabIndex={-1} role="status">
        <p className="learning-feedback-title"><Icon name={quizCorrect ? 'check' : 'refresh'} size={18} />{quizCorrect ? t('Réponse correcte') : t('Reprenons ce point')}</p>
        <MessageBody text={activity.explanation} openLink={openLink} />
        <p className="learning-card-hint">{t('Correction proposée par le tuteur. Le suivi pédagogique reste géré par Tutor-MCP.')}</p>
      </div>}
    </> : <>
      {!answerShown && <div className="learning-flashcard-front"><p>{t('Cherchez la réponse de mémoire, puis retournez la carte.')}</p><button className="button blue" disabled={locked} onClick={() => { recallSeconds.current = elapsed(); setRevealed(true); }}>{t('Retourner la carte')}<Icon name="refresh" size={16} /></button></div>}
      {answerShown && <div ref={flashcardAnswer} className="learning-flashcard-back" role="region" aria-label={t('Réponse de la flashcard')} tabIndex={-1}><p className="learning-answer-label">{t('Réponse')}</p><MessageBody text={activity.answer} openLink={openLink} /></div>}
      {answerShown && !committed && <fieldset className="learning-recall" disabled={locked}><legend>{t('Aviez-vous retrouvé la réponse ?')}</legend><div className="learning-recall-buttons">{ratings.map(rating => <button className="button secondary" key={rating.value} onClick={() => void respond(rating.value)}>{t(rating.label)}</button>)}</div></fieldset>}
      {committed?.kind === 'flashcard' && <div ref={feedback} className="learning-feedback self-report" tabIndex={-1} role="status"><p className="learning-feedback-title">{t('Votre autoévaluation : {rating}', { rating: t(ratings.find(rating => rating.value === committed.rating)!.label) })}</p><p>{t('Votre déclaration reste distincte d’une réponse vérifiée dans un quiz.')}</p></div>}
    </>}
    {error && <p className="learning-card-error" role="alert">{error}</p>}
    {committed && onContinue && <div className="learning-card-actions"><span className="learning-card-hint">{activity.kind === 'flashcard' ? t('Autoévaluation') : t('Réponse donnée')}</span><button className="button blue" disabled={disabled || submitting} onClick={onContinue}>{t('Continuer')}<Icon name="arrow" size={16} /></button></div>}
  </section>;
}
