import type { ClientBridge } from './bridge';
import type { Training, TrainingPage } from './types';

export type ReviewFormat = 'quiz' | 'flashcard';
export interface ReviewPlanPreference { enabled: boolean; format: ReviewFormat }
export const defaultPlan: Readonly<ReviewPlanPreference> = Object.freeze({ enabled: false, format: 'quiz' });
export type ReviewGroup = 'overdue' | 'today' | 'upcoming' | 'waiting';
export interface ScheduledReview { concept: string; dueAt: string; timestamp: number }

/** Include the authenticated account/institution/endpoint in scope, never just a training id. */
export function reviewPlanKey(scope: string, trainingId: string): string {
  return JSON.stringify([scope, trainingId]);
}

export function reviewPreference(preferences: Record<string, ReviewPlanPreference>, scope: string, trainingId: string): ReviewPlanPreference {
  const candidate = preferences[reviewPlanKey(scope, trainingId)];
  return candidate && typeof candidate.enabled === 'boolean' && (candidate.format === 'quiz' || candidate.format === 'flashcard')
    ? candidate : { ...defaultPlan };
}

/** Only accept real server timestamps with an explicit timezone; never invent a due date. */
export function reviewTimestamp(value: unknown): number | undefined {
  if (typeof value !== 'string' || value.length > 48) return undefined;
  const match = /^(\d{4})-(\d{2})-(\d{2})T([01]\d|2[0-3]):([0-5]\d):([0-5]\d)(?:\.\d{1,9})?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/.exec(value);
  if (!match) return undefined;
  const year = Number(match[1]), month = Number(match[2]), day = Number(match[3]);
  if (year < 1970 || month < 1 || month > 12 || day < 1 || day > new Date(Date.UTC(year, month, 0)).getUTCDate()) return undefined;
  const timestamp = Date.parse(value);
  return Number.isFinite(timestamp) ? timestamp : undefined;
}

export function scheduledReviews(training: Training): ScheduledReview[] {
  const byConcept = new Map<string, ScheduledReview>();
  for (const state of Array.isArray(training.progress) ? training.progress : []) {
    if (!state || typeof state.concept !== 'string' || !state.concept.trim()) continue;
    const timestamp = reviewTimestamp(state.next_review);
    if (timestamp === undefined) continue;
    const previous = byConcept.get(state.concept);
    if (!previous || timestamp < previous.timestamp) {
      byConcept.set(state.concept, { concept: state.concept, dueAt: state.next_review!, timestamp });
    }
  }
  return [...byConcept.values()].sort((a, b) => a.timestamp - b.timestamp || a.concept.localeCompare(b.concept));
}

export function reviewGroup(timestamp: number | undefined, now = new Date()): ReviewGroup {
  if (timestamp === undefined) return 'waiting';
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const tomorrow = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
  return timestamp < today.getTime() ? 'overdue' : timestamp < tomorrow.getTime() ? 'today' : 'upcoming';
}

function isTraining(value: unknown): value is Training {
  if (!value || typeof value !== 'object') return false;
  const item = value as Partial<Training>;
  return typeof item.id === 'string' && !!item.id && typeof item.name === 'string' && typeof item.can_learn === 'boolean'
    && typeof item.status === 'string' && Array.isArray(item.progress);
}

/** Walk the existing projection, without adding a private curriculum or scheduling model. */
export async function loadReviewTrainings(api: ClientBridge, isCurrent: () => boolean = () => true): Promise<Training[]> {
  const seenCursors = new Set<string>();
  const items = new Map<string, Training>();
  let after = '';
  for (let pageIndex = 0; pageIndex < 100; pageIndex++) {
    if (!isCurrent()) return [];
    const value = await api.trainingAction('list_training_library', { limit: 100, ...(after ? { after } : {}) }) as TrainingPage;
    if (!isCurrent()) return [];
    if (!value || !Array.isArray(value.items) || typeof value.next_after !== 'string' || value.items.some(item => !isTraining(item))) {
      throw new Error('Le planning reçu est incomplet. Actualisez pour réessayer.');
    }
    for (const item of value.items) items.set(item.id, item);
    if (!value.next_after) {
      return [...items.values()].filter(item => item.can_learn && typeof item.domain_id === 'string' && !!item.domain_id.trim()
        && !['archived', 'deleted', 'withdrawn', 'unavailable'].includes(item.status))
        .sort((a, b) => a.name.localeCompare(b.name));
    }
    if (seenCursors.has(value.next_after)) throw new Error('Le planning reçu est incomplet. Actualisez pour réessayer.');
    seenCursors.add(value.next_after);
    after = value.next_after;
  }
  throw new Error('Le planning reçu est incomplet. Actualisez pour réessayer.');
}
