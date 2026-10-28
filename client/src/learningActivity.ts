// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

/** A presentation contract, not a client-side model of pedagogical mastery. */
export type LearningFormat = 'quiz' | 'flashcard';
export type RecallRating = 'again' | 'hard' | 'easy';

interface ActivityBase {
  version: 1;
  id: string;
  kind: LearningFormat;
  trainingId: string;
  domainId: string;
  concept: string;
  question: string;
  sessionId?: string;
  decisionId?: string;
}
export interface QuizActivity extends ActivityBase {
  kind: 'quiz';
  attemptId?: string;
  activityType?: string;
  rubricJson?: string;
  transferDimension?: 'near' | 'far' | 'debugging' | 'teaching' | 'creative';
  options: { id: string; text: string }[];
  correctOptionId: string;
  explanation: string;
}
export interface FlashcardActivity extends ActivityBase {
  kind: 'flashcard';
  answer: string;
}
export type LearningActivity = QuizActivity | FlashcardActivity;

interface ResponseBase {
  version: 1;
  activityId: string;
  kind: LearningFormat;
  trainingId: string;
  domainId: string;
  concept: string;
  responseTimeSeconds: number;
}
export interface QuizResponse extends ResponseBase {
  kind: 'quiz';
  attemptId?: string;
  selectedOptionId: string;
}
export interface FlashcardResponse extends ResponseBase {
  kind: 'flashcard';
  rating: RecallRating;
}
export type LearningResponse = QuizResponse | FlashcardResponse;

export type LearningContentSegment =
  | { type: 'text'; text: string }
  | { type: 'activity'; activity: LearningActivity }
  | { type: 'invalid' }
  | { type: 'pending' };

const maxBlockBytes = 48 * 1024;
const encoder = new TextEncoder();
const record = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value);
function bounded(value: unknown, max = 256): value is string {
  return typeof value === 'string' && value.trim().length > 0 && !value.includes('\0') && encoder.encode(value).length <= max;
}
function optionalId(value: unknown): boolean { return value === undefined || bounded(value); }
const activityKeys = ['version', 'id', 'kind', 'trainingId', 'domainId', 'concept', 'question', 'sessionId', 'decisionId'];
const responseKeys = ['version', 'activityId', 'kind', 'trainingId', 'domainId', 'concept', 'responseTimeSeconds'];
const assessmentActivityTypes = ['DIAGNOSTIC_ASSESSMENT', 'RECALL_EXERCISE', 'NEW_CONCEPT', 'MASTERY_CHALLENGE', 'DEBUGGING_CASE', 'PRACTICE', 'DEBUG_MISCONCEPTION', 'FEYNMAN_PROMPT', 'TRANSFER_PROBE'];
function onlyKeys(value: Record<string, unknown>, allowed: string[]): boolean {
  return Object.keys(value).every(key => allowed.includes(key));
}

export function validateLearningActivity(value: unknown): LearningActivity | undefined {
  if (!record(value) || value.version !== 1 || !bounded(value.id) || !bounded(value.trainingId) || !bounded(value.domainId)
    || !bounded(value.concept) || !bounded(value.question, 12000) || !optionalId(value.sessionId) || !optionalId(value.decisionId)) return;
  const base = {
    version: 1 as const, id: value.id, trainingId: value.trainingId, domainId: value.domainId,
    concept: value.concept, question: value.question,
    ...(typeof value.sessionId === 'string' ? { sessionId: value.sessionId } : {}),
    ...(typeof value.decisionId === 'string' ? { decisionId: value.decisionId } : {}),
  };
  if (value.kind === 'quiz') {
    if (!onlyKeys(value, [...activityKeys, 'attemptId', 'activityType', 'rubricJson', 'transferDimension', 'options', 'correctOptionId', 'explanation'])
      || !Array.isArray(value.options) || value.options.length < 2 || value.options.length > 6 || !bounded(value.correctOptionId)
      || !bounded(value.explanation, 12000) || !optionalId(value.attemptId)) return;
    if (value.activityType !== undefined && (typeof value.activityType !== 'string' || !assessmentActivityTypes.includes(value.activityType))) return;
    if (value.rubricJson !== undefined && (!bounded(value.rubricJson, 16 * 1024) || !record(parseJson(value.rubricJson)))) return;
    if (value.attemptId !== undefined && (value.activityType === undefined || value.rubricJson === undefined)) return;
    if (value.activityType === 'TRANSFER_PROBE') {
      if (typeof value.transferDimension !== 'string' || !['near', 'far', 'debugging', 'teaching', 'creative'].includes(value.transferDimension)) return;
    } else if (value.transferDimension !== undefined) return;
    const options: QuizActivity['options'] = [];
    for (const item of value.options) {
      if (!record(item) || !onlyKeys(item, ['id', 'text']) || !bounded(item.id) || !bounded(item.text, 2000) || options.some(option => option.id === item.id)) return;
      options.push({ id: item.id, text: item.text });
    }
    if (!options.some(option => option.id === value.correctOptionId)) return;
    return { ...base, kind: 'quiz', options, correctOptionId: value.correctOptionId, explanation: value.explanation,
      ...(typeof value.activityType === 'string' ? { activityType: value.activityType } : {}),
      ...(typeof value.rubricJson === 'string' ? { rubricJson: value.rubricJson } : {}),
      ...(typeof value.transferDimension === 'string' ? { transferDimension: value.transferDimension as QuizActivity['transferDimension'] } : {}),
      ...(typeof value.attemptId === 'string' ? { attemptId: value.attemptId } : {}) };
  }
  if (value.kind === 'flashcard' && onlyKeys(value, [...activityKeys, 'answer']) && bounded(value.answer, 12000)) {
    return { ...base, kind: 'flashcard', answer: value.answer };
  }
}

export function validateLearningResponse(value: unknown): LearningResponse | undefined {
  if (!record(value) || value.version !== 1 || !bounded(value.activityId) || !bounded(value.trainingId)
    || !bounded(value.domainId) || !bounded(value.concept) || typeof value.responseTimeSeconds !== 'number'
    || !Number.isFinite(value.responseTimeSeconds) || value.responseTimeSeconds < 0 || value.responseTimeSeconds > 86400) return;
  const base = { version: 1 as const, activityId: value.activityId, trainingId: value.trainingId, domainId: value.domainId,
    concept: value.concept, responseTimeSeconds: value.responseTimeSeconds };
  if (value.kind === 'quiz' && onlyKeys(value, [...responseKeys, 'attemptId', 'selectedOptionId']) && bounded(value.selectedOptionId) && optionalId(value.attemptId)) {
    return { ...base, kind: 'quiz', selectedOptionId: value.selectedOptionId,
      ...(typeof value.attemptId === 'string' ? { attemptId: value.attemptId } : {}) };
  }
  if (value.kind === 'flashcard' && onlyKeys(value, [...responseKeys, 'rating']) && typeof value.rating === 'string' && ['again', 'hard', 'easy'].includes(value.rating)) {
    return { ...base, kind: 'flashcard', rating: value.rating as RecallRating };
  }
}

function parseJson(raw: string): unknown {
  if (encoder.encode(raw).length > maxBlockBytes) return;
  try { return JSON.parse(raw); } catch { return; }
}

export interface Fence { start: number; end: number; tag: string; body: string; closed: boolean }
/** Scan Markdown fences without treating a protocol example inside another code block as an activity. */
export function learningProtocolFences(source: string): Fence[] {
  const lines = [...source.matchAll(/[^\r\n]*(?:\r?\n|$)/g)].filter(match => match[0].length > 0);
  const found: Fence[] = [];
  let open: { start: number; bodyStart: number; delimiter: string; length: number; tag: string; reserved: boolean } | undefined;
  for (const line of lines) {
    const content = line[0].replace(/\r?\n$/, '');
    if (open) {
      const closing = /^ {0,3}(`{3,}|~{3,})\s*$/.exec(content);
      if (closing && closing[1][0] === open.delimiter && closing[1].length >= open.length) {
        if (open.reserved) found.push({ start: open.start, end: line.index + line[0].length, tag: open.tag,
          body: source.slice(open.bodyStart, line.index), closed: true });
        open = undefined;
      }
      continue;
    }
    const opening = /^ {0,3}(`{3,}|~{3,})\s*([^\r\n]*)$/.exec(content);
    if (!opening) continue;
    const tag = opening[2].trim().toLowerCase();
    // A partially streamed tag is kept out of the Markdown/code renderer too.
    const reserved = /^tutor-(activity|response|study-request)/.test(tag)
      || (!!tag && ['tutor-activity', 'tutor-response', 'tutor-study-request'].some(known => known.startsWith(tag)));
    open = { start: line.index, bodyStart: line.index + line[0].length, delimiter: opening[1][0],
      length: opening[1].length, tag, reserved };
  }
  if (open?.reserved) found.push({ start: open.start, end: source.length, tag: open.tag, body: source.slice(open.bodyStart), closed: false });
  return found;
}

/** Only complete assistant turns may reveal an interactive activity. Reserved payloads never become raw Markdown. */
export function parseLearningContent(source: string, { complete }: { complete: boolean }): LearningContentSegment[] {
  const segments: LearningContentSegment[] = [];
  const fences = learningProtocolFences(source);
  const multipleActivities = fences.filter(fence => fence.tag === 'tutor-activity').length > 1;
  let cursor = 0;
  for (const fence of fences) {
    if (fence.start > cursor) segments.push({ type: 'text', text: source.slice(cursor, fence.start) });
    if (!complete) segments.push({ type: 'pending' });
    else if (fence.closed && fence.tag === 'tutor-activity' && !multipleActivities) {
      const activity = validateLearningActivity(parseJson(fence.body));
      segments.push(activity ? { type: 'activity', activity } : { type: 'invalid' });
    } else if (fence.tag !== 'tutor-response' && fence.tag !== 'tutor-study-request') segments.push({ type: 'invalid' });
    cursor = fence.end;
  }
  if (cursor < source.length) segments.push({ type: 'text', text: source.slice(cursor) });
  return segments;
}

/** User turns carry the chosen answer; they never carry a client-computed success/mastery score. */
export function parseLearningResponse(source: string): LearningResponse | undefined {
  const responses = learningProtocolFences(source).filter(fence => fence.tag === 'tutor-response');
  if (responses.length !== 1 || !responses[0].closed) return;
  return validateLearningResponse(parseJson(responses[0].body));
}

export function serializeLearningResponse(response: LearningResponse): string {
  const validated = validateLearningResponse(response);
  if (!validated) throw new Error('Réponse d’activité invalide.');
  return '```tutor-response\n' + JSON.stringify(validated) + '\n```';
}

export function responseMatchesActivity(activity: LearningActivity, response: LearningResponse | undefined): response is LearningResponse {
  if (!response || response.activityId !== activity.id || response.kind !== activity.kind || response.trainingId !== activity.trainingId
    || response.domainId !== activity.domainId || response.concept !== activity.concept) return false;
  if (activity.kind === 'quiz' && response.kind === 'quiz') {
    return response.attemptId === activity.attemptId && activity.options.some(option => option.id === response.selectedOptionId);
  }
  return activity.kind === 'flashcard' && response.kind === 'flashcard';
}
