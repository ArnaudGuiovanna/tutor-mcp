import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { ClientBridge } from './bridge';
import { Icon } from './icons';
import { dateLocale, t, useTranslation } from './i18n';
import { errorMessage } from './model';
import type { Training } from './types';
import { loadReviewTrainings, reviewGroup, reviewPreference, reviewTimestamp, scheduledReviews } from './reviewPlanning';
import type { ReviewFormat, ReviewGroup, ReviewPlanPreference } from './reviewPlanning';
import './reviewPlanning.css';

export interface ReviewPlanningProps {
  api: ClientBridge;
  scope: string;
  preferences: Record<string, ReviewPlanPreference>;
  connected: boolean;
  busy: boolean;
  refreshKey: number;
  onPreference: (trainingId: string, preference: ReviewPlanPreference) => Promise<void>;
  onStart: (training: Training, format: ReviewFormat, concept?: string) => void;
}

interface LoadedTrainings { scope: string; refreshKey: number; trainings: Training[] }
const groupLabels: Record<ReviewGroup, string> = {
  overdue: 'À reprendre', today: 'Aujourd’hui', upcoming: 'À venir', waiting: 'En attente d’une échéance',
};
const groupOrder: ReviewGroup[] = ['overdue', 'today', 'upcoming', 'waiting'];

export function ReviewPlanning({ api, scope, preferences, connected, busy, refreshKey, onPreference, onStart }: ReviewPlanningProps) {
  useTranslation();
  const [loaded, setLoaded] = useState<LoadedTrainings>();
  const [loading, setLoading] = useState(false);
  const [retry, setRetry] = useState(0);
  const [error, setError] = useState<{ scope: string; message: string }>();
  const [saving, setSaving] = useState<{ scope: string; trainingId: string }>();
  const [saved, setSaved] = useState<{ scope: string; message: string }>();
  const [now, setNow] = useState(() => new Date());
  const [focusAfterSave, setFocusAfterSave] = useState<{ scope: string; trainingId: string; control: string }>();
  const root = useRef<HTMLElement>(null);
  const currentScope = useRef(scope); currentScope.current = scope;
  const requestVersion = useRef(0);
  const savingRef = useRef(false);
  const saveVersion = useRef(0);

  useLayoutEffect(() => {
    if (!focusAfterSave || focusAfterSave.scope !== scope) return;
    if (document.activeElement === document.body) {
      const control = [...(root.current?.querySelectorAll<HTMLElement>('[data-review-control]') || [])]
        .find(element => element.dataset.reviewTraining === focusAfterSave.trainingId && element.dataset.reviewControl === focusAfterSave.control);
      control?.focus();
    }
    setFocusAfterSave(undefined);
  }, [focusAfterSave, preferences, scope]);

  useEffect(() => {
    saveVersion.current++; savingRef.current = false; setSaving(undefined);
    return () => { saveVersion.current++; savingRef.current = false; };
  }, [scope]);

  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 60_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    const request = ++requestVersion.current;
    setLoaded(undefined); setError(undefined); setSaved(undefined);
    if (!connected) { setLoading(false); return; }
    setLoading(true);
    const isCurrent = () => request === requestVersion.current;
    void loadReviewTrainings(api, isCurrent).then(trainings => {
      if (isCurrent()) setLoaded({ scope, refreshKey, trainings });
    }).catch(reason => {
      if (isCurrent()) setError({ scope, message: errorMessage(reason) });
    }).finally(() => { if (isCurrent()) setLoading(false); });
    return () => { requestVersion.current++; };
  }, [api, scope, connected, refreshKey, retry]);

  async function savePreference(training: Training, next: ReviewPlanPreference) {
    if (busy || savingRef.current || !connected) return;
    const requestedScope = scope;
    const save = ++saveVersion.current;
    const focusedControl = document.activeElement instanceof HTMLElement ? document.activeElement.dataset.reviewControl : undefined;
    savingRef.current = true; setSaving({ scope, trainingId: training.id }); setError(undefined); setSaved(undefined);
    try {
      // Controlled fields only change after native persistence has succeeded.
      await onPreference(training.id, next);
      if (currentScope.current === requestedScope && saveVersion.current === save) {
        setSaved({ scope, message: t('Préférences de révision enregistrées.') });
        if (focusedControl) setFocusAfterSave({ scope, trainingId: training.id, control: focusedControl });
      }
    } catch (reason) {
      if (currentScope.current === requestedScope && saveVersion.current === save) setError({ scope, message: errorMessage(reason) });
    } finally {
      if (saveVersion.current === save) {
        savingRef.current = false;
        if (currentScope.current === requestedScope) setSaving(undefined);
      }
    }
  }

  const trainings = connected && loaded?.scope === scope && loaded.refreshKey === refreshKey ? loaded.trainings : [];
  const locked = busy || saving?.scope === scope;
  const active = trainings.filter(training => reviewPreference(preferences, scope, training.id).enabled);
  const inactive = trainings.filter(training => !reviewPreference(preferences, scope, training.id).enabled);
  const groups = groupOrder.map(group => ({ group, trainings: active.filter(training => reviewGroup(scheduledReviews(training)[0]?.timestamp, now) === group)
    .sort((a, b) => (scheduledReviews(a)[0]?.timestamp ?? Infinity) - (scheduledReviews(b)[0]?.timestamp ?? Infinity) || a.name.localeCompare(b.name)) }));

  function renderTraining(training: Training) {
    const preference = reviewPreference(preferences, scope, training.id);
    const reviews = scheduledReviews(training);
    const next = reviews[0];
    const invalidDate = (training.progress || []).some(state => !!state?.next_review && reviewTimestamp(state.next_review) === undefined);
    const isSaving = saving?.scope === scope && saving.trainingId === training.id;
    const date = next ? new Intl.DateTimeFormat(dateLocale(), { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' }).format(next.timestamp) : '';
    return <article className={`review-plan ${preference.enabled ? 'enabled' : ''}`} key={training.id} aria-label={training.name}>
      <div className="review-plan-heading">
        <div className="review-plan-title"><h4>{training.name}</h4><p>{preference.enabled
          ? next ? t('Prochaine révision : {date}', { date }) : invalidDate ? t('Échéance indisponible') : t('Pas encore de révision programmée')
          : t('Répétition espacée désactivée dans ce planning')}</p></div>
        {preference.enabled && next && <button className="button blue review-start" disabled={locked} aria-label={t('Réviser {name}', { name: training.name })} onClick={() => onStart(training, preference.format, next.concept)}>
          {t('Réviser maintenant')}<Icon name="arrow" size={16} />
        </button>}
      </div>
      <div className="review-plan-controls">
        <label className="review-format"><span>{t('Format')}</span><select data-review-control="format" data-review-training={training.id} aria-label={t('Format de révision pour {name}', { name: training.name })} value={preference.format} disabled={locked} onChange={event => void savePreference(training, { ...preference, format: event.target.value as ReviewFormat })}>
          <option value="quiz">{t('Quiz')}</option><option value="flashcard">{t('Flashcards')}</option>
        </select></label>
        <button type="button" role="switch" data-review-control="toggle" data-review-training={training.id} aria-checked={preference.enabled} aria-label={t('Répétition espacée pour {name}', { name: training.name })} className="review-toggle" disabled={locked} onClick={() => void savePreference(training, { ...preference, enabled: !preference.enabled })}>
          <span className="review-switch-track" aria-hidden="true"><span /></span><span>{preference.enabled ? t('Activée') : t('Activer')}</span>
        </button>
        {isSaving && <span className="subtle-note" role="status">{t('Enregistrement…')}</span>}
      </div>
      {preference.enabled && reviews.length > 0 && <details className="review-plan-dates"><summary>{t('Voir les révisions programmées ({count})', { count: reviews.length })}</summary>
        <ul>{reviews.map(review => <li key={review.concept}><span>{review.concept}</span><time dateTime={review.dueAt}>{new Intl.DateTimeFormat(dateLocale(), { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' }).format(review.timestamp)}</time></li>)}</ul>
      </details>}
      {preference.enabled && !next && !invalidDate && <p className="review-plan-note">{t('Les prochaines dates apparaîtront après vos activités d’apprentissage.')}</p>}
      {preference.enabled && invalidDate && <p className="review-plan-note">{t('Certaines échéances sont indisponibles. Actualisez le planning pour les relire.')}</p>}
    </article>;
  }

  return <section ref={root} className="page-content review-planning" aria-label={t('Planning des révisions')}>
    <header className="review-planning-heading"><div><p className="eyebrow">{t('Répétition espacée')}</p><h2>{t('Votre planning')}</h2></div>
      {connected && <button className="button secondary" disabled={locked || loading} onClick={() => setRetry(value => value + 1)}><Icon name="refresh" size={16} />{t('Actualiser le planning')}</button>}
    </header>
    <p className="review-planning-intro">{t('Un clic sur une révision ouvre votre conversation et démarre l’activité.')}</p>
    {!connected ? <p role="status" className="review-empty">{t('Connectez Tutor dans les paramètres pour retrouver vos révisions.')}</p> : <>
      {error?.scope === scope && <p role="alert" className="notice error">{t(error.message)}</p>}
      {saved?.scope === scope && <p role="status" className="review-save-status">{saved.message}</p>}
      {loading && <p role="status" className="review-empty">{t('Lecture du planning…')}</p>}
      {!loading && loaded?.scope === scope && loaded.refreshKey === refreshKey && !trainings.length && <div className="review-empty"><h3>{t('Votre planning se remplira avec vos apprentissages')}</h3><p>{t('Commencez une formation dans Conversation ou rejoignez une formation pour retrouver ses révisions ici.')}</p></div>}
      {!loading && trainings.length > 0 && <>
        <p className="review-planning-note">{t('Activez les formations à retrouver ici. Les dates viennent de Tutor-MCP ; désactiver ce planning conserve votre suivi pédagogique.')}</p>
        {!active.length && <p className="review-empty">{t('Aucune répétition espacée active. Activez une formation ci-dessous.')}</p>}
        {groups.filter(group => group.trainings.length).map(({ group, trainings: grouped }) => <section className={`review-group review-group-${group}`} key={group} aria-label={t(groupLabels[group])}><h3>{t(groupLabels[group])}<span>{grouped.length}</span></h3>{grouped.map(renderTraining)}</section>)}
        {inactive.length > 0 && <section className="review-group" aria-label={t('Activer d’autres formations')}><h3>{active.length ? t('Activer d’autres formations') : t('Vos formations')}</h3>{inactive.map(renderTraining)}</section>}
      </>}
    </>}
  </section>;
}
