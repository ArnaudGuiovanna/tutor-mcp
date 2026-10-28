import type { ClientBridge } from './bridge';
import { t } from './i18n';
import { learningProtocolFences } from './learningActivity';
import type { ConversationContext, Settings, Snapshot, TutorOverview } from './types';

export type StudyFormat = 'quiz' | 'flashcard';
export interface StudyRequest {
  version: 1;
  format: StudyFormat;
  intent: 'review' | 'practice';
  trainingId?: string;
  trainingName?: string;
  domainId?: string;
  concept?: string;
}

export function studyRequestText(request: StudyRequest, topic = ''): string {
  const label = request.format === 'quiz' ? t('Quiz') : t('Flashcards');
  const title = request.intent === 'review' ? t('Réviser {name} avec {format}.', {name: request.trainingName || t('ma formation'), format: label})
    : t('Pratiquer avec {format}.', {format: label});
  return `${title}${topic.trim() ? `\n${topic.trim()}` : ''}\n\n\`\`\`tutor-study-request\n${JSON.stringify(request)}\n\`\`\``;
}

export function parseStudyRequest(text: string): StudyRequest | undefined {
  const blocks = learningProtocolFences(text).filter(block => block.tag === 'tutor-study-request');
  if (blocks.length !== 1 || !blocks[0].closed || new TextEncoder().encode(blocks[0].body).length > 8000) return;
  try {
    const value = JSON.parse(blocks[0].body);
    if (!value || value.version !== 1 || !['quiz', 'flashcard'].includes(value.format) || !['review', 'practice'].includes(value.intent)) return;
    if (Object.keys(value).some(key => !['version', 'format', 'intent', 'trainingId', 'trainingName', 'domainId', 'concept'].includes(key))) return;
    for (const key of ['trainingId', 'trainingName', 'domainId', 'concept']) {
      if (value[key] !== undefined && (typeof value[key] !== 'string' || !value[key].trim() || value[key].includes('\0') || new TextEncoder().encode(value[key]).length > (key === 'trainingName' ? 1000 : 256))) return;
    }
    if (value.intent === 'review' && !value.trainingId) return;
    return value as StudyRequest;
  } catch { return; }
}

export function parseSlashCommand(draft: string): { format: StudyFormat; topic: string } | undefined {
  const match = /^\/(quiz|quizz|flashcards?|cartes)(?:\s+([\s\S]*))?$/i.exec(draft.trim());
  if (!match) return;
  return {format: /^qui/i.test(match[1]) ? 'quiz' : 'flashcard', topic: match[2] || ''};
}

// The same profile can connect to several institutions or local servers.
// Include identity and endpoint, but never any credential or bearer token.
export function reviewScope(state: Pick<Snapshot, 'profile' | 'identity' | 'settings'>, overview: TutorOverview | null): string {
  const tutor = state.settings.tutor;
  let endpoint = tutor.url;
  if (tutor.mode === 'remote') {
    try { const url = new URL(endpoint); endpoint = `${url.origin}${url.pathname.replace(/\/+$/, '')}`; } catch { endpoint = ''; }
  }
  return JSON.stringify([state.profile?.id || 'default', state.identity.npub || '', tutor.mode,
    tutor.mode === 'remote' ? endpoint : [tutor.command, tutor.args], overview?.identity?.tenantId || '', overview?.identity?.userId || '']);
}

export function reviewPreferences(settings: Settings) { return settings.reviewPlans || {}; }

export async function selectStudyTraining(api: ClientBridge, conversationId: string, request: StudyRequest): Promise<ConversationContext | undefined> {
  if (!request.trainingId) return;
  const previous = await api.trainingAction('get_conversation_context', {conversation_id: conversationId}) as ConversationContext;
  if (!previous || !Number.isSafeInteger(previous.selection_version) || previous.selection_version < 0 || previous.conversation_id !== conversationId) {
    throw new Error('Impossible de confirmer la formation de cette conversation. Rechargez le planning.');
  }
  const selected = previous.training_id === request.trainingId ? previous : await api.trainingAction('select_conversation_training', {
    id: request.trainingId, conversation_id: conversationId, expected_selection: previous.selection_version, key: crypto.randomUUID(),
  }) as ConversationContext;
  if (selected?.conversation_id !== conversationId || selected?.training_id !== request.trainingId || !selected.domain_id ||
      (request.domainId && selected.domain_id !== request.domainId)) {
    throw new Error('La formation sélectionnée ne correspond pas à cette révision. Aucun exercice n’a été lancé.');
  }
  return selected;
}

export function visibleStudyText(text: string): string {
  // Reserved envelopes are transport data, never product copy (including outbox).
  let visible = '', cursor = 0;
  for (const fence of learningProtocolFences(text)) {
    visible += text.slice(cursor, fence.start); cursor = fence.end;
  }
  return (visible + text.slice(cursor)).trim();
}
