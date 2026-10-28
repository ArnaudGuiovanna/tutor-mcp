import { describe, expect, it, vi } from 'vitest';
import { parseSlashCommand, parseStudyRequest, reviewScope, selectStudyTraining, studyRequestText, visibleStudyText } from '../../src/studyRequests';
import { client, initialState } from './clientFixture';

describe('Demandes structurées de pratique', () => {
  it('reconnaît les alias et conserve le sujet', () => {
    expect(parseSlashCommand('/quizz les fonctions')).toEqual({format:'quiz',topic:'les fonctions'});
    expect(parseSlashCommand('/flashcard les temps du passé')).toEqual({format:'flashcard',topic:'les temps du passé'});
    expect(parseSlashCommand('/quizzer')).toBeUndefined();
  });
  it('utilise le même scanner que les composants et masque les enveloppes dans le fil', () => {
    const request = {version:1 as const,format:'quiz' as const,intent:'review' as const,trainingId:'personal:go'};
    const source = `Réviser Go\n\n~~~~TUTOR-STUDY-REQUEST\n${JSON.stringify(request)}\n~~~~`;
    expect(parseStudyRequest(source)).toEqual(request);
    expect(visibleStudyText(source)).toBe('Réviser Go');
    expect(parseStudyRequest('```text\n'+studyRequestText(request)+'\n```')).toBeUndefined();
    expect(parseStudyRequest(studyRequestText(request)+'\n'+studyRequestText(request))).toBeUndefined();
    expect(visibleStudyText('Réviser\n```tutor-response\n{"secret":')).toBe('Réviser');
  });
  it('isole les préférences entre comptes et institutions sans intégrer de secret URL', () => {
    const first = initialState(true); first.profile={id:'alice',label:'Alice'};
    first.settings.tutor={...first.settings.tutor,mode:'remote',url:'https://user:password@school.test/mcp?token=secret'};
    const key = reviewScope(first,null); expect(key).not.toContain('password'); expect(key).not.toContain('secret');
    expect(reviewScope({...first,profile:{id:'bob',label:'Bob'}},null)).not.toBe(key);
    expect(reviewScope(first,{identity:{tenantId:'new',userId:'a',membershipId:'m'},context:{},formations:{},availableFormations:{}})).not.toBe(key);
  });
  it('arrête la révision sur une réponse serveur de domaine inattendue', async () => {
    const fixture = client(); vi.mocked(fixture.api.trainingAction).mockResolvedValue({conversation_id:'chat',training_id:'personal:go',domain_id:'python',selection_version:1});
    await expect(selectStudyTraining(fixture.api,'chat',{version:1,format:'quiz',intent:'review',trainingId:'personal:go',domainId:'go'})).rejects.toThrow('ne correspond pas');
  });
});
