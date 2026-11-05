import { t } from './i18n';
import { learningProtocolFences } from './learningActivityModel';

/** A resume intent; the server chooses the activity and its presentation format. */
export interface ResumeRequest {
  version: 1;
  trainingId?: string;
  trainingName?: string;
  domainId?: string;
}

export function parseResumeRequest(text: string): ResumeRequest | undefined {
  const blocks = learningProtocolFences(text).filter(block => block.tag === 'tutor-resume-request');
  if (blocks.length !== 1 || !blocks[0].closed || new TextEncoder().encode(blocks[0].body).length > 8000) return;
  try {
    const value: unknown = JSON.parse(blocks[0].body);
    if (!value || typeof value !== 'object' || Array.isArray(value)) return;
    const request = value as Record<string, unknown>;
    if (request.version !== 1 || Object.keys(request).some(key => !['version', 'trainingId', 'trainingName', 'domainId'].includes(key))) return;
    for (const key of ['trainingId', 'trainingName', 'domainId']) {
      const item = request[key];
      if (item !== undefined && (typeof item !== 'string' || !item.trim() || item.includes('\0') || new TextEncoder().encode(item).length > (key === 'trainingName' ? 200 : 256))) return;
    }
    if (request.domainId !== undefined && request.trainingId === undefined) return;
    return request as unknown as ResumeRequest;
  } catch { return; }
}

export function resumeRequestText(request: ResumeRequest): string {
  const payload = { ...request };
  // Explicit identities do not need a name lookup; retain long display labels
  // in prose without sending them to the server's bounded name resolver.
  if (payload.trainingId && payload.trainingName && new TextEncoder().encode(payload.trainingName).length > 200) delete payload.trainingName;
  const block = '```tutor-resume-request\n' + JSON.stringify(payload) + '\n```';
  if (!parseResumeRequest(block)) throw new Error('Demande de reprise invalide.');
  return t('Reprendre {name}.', { name: request.trainingName || t('ma formation') }) + '\n\n' + block;
}
