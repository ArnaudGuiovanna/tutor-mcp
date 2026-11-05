import { describe, expect, it } from 'vitest';
import { parseResumeRequest, resumeRequestText } from '../../src/resumeRequests';
import { parseLearningContent } from '../../src/learningActivityModel';
import { visibleStudyText } from '../../src/studyRequests';

describe('reprise native sans format imposé', () => {
  const block = (value: unknown) => '```tutor-resume-request\n' + JSON.stringify(value) + '\n```';
  it('conserve l’identité de formation dans la requête durable sans montrer le transport', () => {
    const request = { version: 1 as const, trainingId: 'personal:go', domainId: 'go', trainingName: 'Go' };
    const text = resumeRequestText(request);
    expect(parseResumeRequest(text)).toEqual(request);
    expect(visibleStudyText(text)).toBe('Reprendre Go.');
    expect(parseLearningContent(text, { complete: true })).toEqual([{ type: 'text', text: 'Reprendre Go.\n\n' }]);
    expect(parseResumeRequest(block({ version: 1 }))).toEqual({ version: 1 });
    const longName = 'Formation '.repeat(30);
    const identified = resumeRequestText({ version: 1, trainingId: 'go', trainingName: longName });
    expect(parseResumeRequest(identified)).toEqual({ version: 1, trainingId: 'go' });
    expect(visibleStudyText(identified)).toContain(longName);
  });
  it('rejette format client, versions inconnues, identité ambiguë et données non bornées', () => {
    for (const value of [{ version: 2 }, { version: 1, format: 'quiz' }, { version: 1, trainingId: '' }, { version: 1, domainId: 'go' }, { version: 1, domainId: 'a\0b' }, { version: 1, trainingName: 'é'.repeat(101) }, { version: 1, trainingId: 'é'.repeat(129) }]) {
      expect(parseResumeRequest(block(value))).toBeUndefined();
    }
    const text = block({ version: 1, trainingId: 'go' });
    expect(parseResumeRequest(text + '\n' + text)).toBeUndefined();
    expect(parseResumeRequest('````markdown\n' + text + '\n````')).toBeUndefined();
    expect(visibleStudyText('Reprendre\n```tutor-resume-request\n{"trainingId":')).toBe('Reprendre');
  });
});
