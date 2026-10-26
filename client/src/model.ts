import { t, dateLocale } from './i18n';
import type { Intent, JsonRecord, PermissionRequest, ToolActivity, TutorOverview } from './types';

export function record(value: unknown): JsonRecord {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as JsonRecord : {};
}

export function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

export function unwrapToolResult(value: unknown): unknown {
  const result = record(value);
  if (result.structuredContent !== undefined) return result.structuredContent;
  if (Array.isArray(result.content)) {
    const blocks = result.content.map(record).filter((block) => block.type === 'text');
    if (blocks.length === 1) {
      try { return JSON.parse(text(blocks[0].text)); } catch { return { message: text(blocks[0].text) }; }
    }
  }
  return value;
}

export interface FormationView {
  id: string;
  name: string;
  description: string;
  kind: 'personal' | 'common' | 'authored';
  formationId?: string;
  publicationStatus?: string;
  domainId?: string;
  cohortId?: string;
  version?: number;
  enrollmentStatus?: string;
  conceptCount?: number;
}

export function formationViews(overview: TutorOverview | null): FormationView[] {
  if (!overview) return [];
  const context = record(unwrapToolResult(overview.context));
  const enrolled = record(unwrapToolResult(overview.formations));
  const domains = Array.isArray(context.domains) ? context.domains.map(record) : [];
  const items = Array.isArray(enrolled.items) ? enrolled.items.map(record) : [];
  const result: FormationView[] = items.map((item) => {
    const enrollment = record(item.enrollment);
    return {
      id: `common:${text(item.cohort_id)}`,
      name: text(item.name) || text(item.cohort_name) || 'Formation commune',
      description: text(item.description),
      kind: 'common',
      cohortId: text(item.cohort_id),
      domainId: text(enrollment.domain_id) || undefined,
      version: typeof item.version === 'number' ? item.version : undefined,
      enrollmentStatus: text(enrollment.status) || text(item.admission_status),
    };
  });
  for (const domain of domains) {
    if (domain.archived === true) continue;
    const domainId = text(domain.domain_id) || text(domain.id);
    if (!domainId || result.some((item) => item.domainId === domainId)) continue;
    result.push({
      id: `personal:${domainId}`,
      name: text(domain.name) || 'Parcours personnel',
      description: '',
      kind: 'personal',
      domainId,
      conceptCount: typeof domain.concept_count === 'number' ? domain.concept_count : typeof domain.conceptCount === 'number' ? domain.conceptCount : undefined,
    });
  }
  return result;
}

export function authoredFormationViews(overview: TutorOverview | null): FormationView[] {
  const result = record(unwrapToolResult(overview?.authoredFormations));
  if (!Array.isArray(result.items)) return [];
  return result.items.map(record).filter((item) => !!text(item.id)).map((item) => ({
    id: `authored:${text(item.id)}`, formationId: text(item.id), kind: 'authored',
    name: text(item.name) || 'Formation de l’institution', description: text(item.description), publicationStatus: text(item.status),
  }));
}

export function availableFormationViews(overview: TutorOverview | null): FormationView[] {
  if (!overview) return [];
  const result = record(unwrapToolResult(overview.availableFormations));
  if (!Array.isArray(result.items)) return [];
  return result.items.map(record).map((item) => ({
    id: `available:${text(item.cohort_id)}`,
    name: text(item.name) || text(item.cohort_name) || 'Formation commune',
    description: text(item.description),
    kind: 'common',
    cohortId: text(item.cohort_id),
    version: typeof item.version === 'number' ? item.version : undefined,
    enrollmentStatus: text(record(item.enrollment).status) || text(item.admission_status),
  }));
}

export function blankIntent(): Intent {
  return { id: '', title: '', motivation: '', startingPoint: '', goals: '', approach: '', constraints: '', designCriteria: '', validated: false, revision: 0, createdAt: '', updatedAt: '' };
}

export function validateIntent(intent: Intent): string | undefined {
  if (!intent.title.trim()) return t('Indiquez ce que vous souhaitez apprendre.');
  if (!intent.validated) return t('Confirmez que cette intention correspond à ce que vous souhaitez apprendre.');
  return undefined;
}

export function permissionFromEvent(event: JsonRecord, activities: ToolActivity[] = []): PermissionRequest | null {
  if (event.method !== 'session/request_permission') return null;
  const params = record(event.params);
  const tool = record(params.toolCall);
  const correlated = activities.find(activity => activity.id === text(tool.toolCallId));
  const content = Array.isArray(tool.content) ? tool.content.map(record) : [];
  const description = content.map((block) => text(record(block.content).text) || text(block.text)).filter(Boolean).join('\n');
  const options = Array.isArray(params.options) ? params.options.map(record).filter((option) => text(option.optionId)).map((option) => ({ optionId: text(option.optionId), name: text(option.name), kind: text(option.kind) })) : [];
  return { id: event.id, sessionId: text(params.sessionId), title: text(tool.title) || correlated?.title || t('Autorisation demandée par Codex'), description, options };
}

export function toolFromEvent(event: JsonRecord): ToolActivity | null {
  if (event.method !== 'session/update') return null;
  const update = record(record(event.params).update);
  if (update.sessionUpdate !== 'tool_call' && update.sessionUpdate !== 'tool_call_update') return null;
  if (!text(update.toolCallId)) return null;
  return { id: text(update.toolCallId), title: text(update.title), status: text(update.status) };
}

export function chunkFromEvent(event: JsonRecord): string {
  if (event.method !== 'session/update') return '';
  const update = record(record(event.params).update);
  if (update.sessionUpdate !== 'agent_message_chunk') return '';
  const content = record(update.content);
  return content.type === 'text' ? text(content.text) : '';
}

export function permissionLabel(kind: string, name: string): string {
  if (kind === 'allow_always' && name.toLowerCase().includes('session')) return t('Autoriser pour cette conversation');
  const labels: Record<string, string> = { allow_once: 'Autoriser une fois', allow_always: 'Toujours autoriser', reject_once: 'Refuser', reject_always: 'Toujours refuser' };
  return t(labels[kind] || name || 'Choisir');
}

export function statusLabel(status: string, context: 'enrollment' | 'member' | 'training' | 'request' = 'enrollment'): string {
  const labels: Record<string, string> = { draft: 'Brouillon', published: 'Publiée', archived: 'Archivée', pending: 'En attente', in_progress: 'En cours', completed: 'Terminé', failed: 'Échec', active: 'Inscription active', invited: 'Invitation reçue', left: 'Inscription quittée', revoked: 'Accès révoqué', approved: 'Admission approuvée', rejected: 'Admission refusée', requested: 'Admission demandée', queued:'En attente', started:'En cours', running:'En cours', succeeded:'Terminé', cancelled:'Annulé', deleted:'Supprimé', prepared:'Préparée', evaluated:'Évaluée' };
  const contextual:Record<string,Record<string,string>>={member:{active:'Actif',suspended:'Suspendu',invited:'Invité',revoked:'Accès révoqué'},training:{active:'Active',withdrawn:'Publication retirée'},request:{processing:'Traitement en cours',running:'Traitement en cours',completed:'Effacement terminé',failed:'Échec du traitement',blocked:'Traitement bloqué'}};
  return t(contextual[context]?.[status] || labels[status] || status);
}

export function errorMessage(error: unknown): string {
  return t(error instanceof Error ? error.message : typeof error === 'string' ? error : 'L’opération a échoué. Réessayez après avoir vérifié les connexions.');
}

export function displayDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '' : new Intl.DateTimeFormat(dateLocale(), { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }).format(date);
}
