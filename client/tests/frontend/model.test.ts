import { describe, expect, it } from 'vitest';
import { authoredFormationViews, availableFormationViews, formationViews } from '../../src/model';

describe('Les objets pédagogiques retournés par Tutor', () => {
  it('présente les vrais brouillons du personnel sans inventer une inscription ou une progression', () => {
    const views = authoredFormationViews({ context: {}, formations: { items: [] }, availableFormations: { items: [] }, authoredFormations: { items: [{ id: 'formation1', name: 'Astronomie institutionnelle', description: 'Objectifs partagés', status: 'draft', owner_membership_id: 'staff1' }] } });
    expect(views).toEqual([{ id: 'authored:formation1', formationId: 'formation1', name: 'Astronomie institutionnelle', description: 'Objectifs partagés', kind: 'authored', publicationStatus: 'draft' }]);
    expect(views[0].domainId).toBeUndefined(); expect(views[0].enrollmentStatus).toBeUndefined();
  });
  it('relie une inscription à son domaine sans dupliquer le parcours et conserve les versions réelles', () => {
    const views = formationViews({
      context: { content: [{ type: 'text', text: JSON.stringify({ domains: [{ domain_id: 'domain-shared', name: 'Astronomie', concept_count: 8 }, { domain_id: 'domain-personal', name: 'Histoire des cartes', concept_count: 4 }] }) }] },
      formations: { structuredContent: { items: [{ name: 'Astronomie', cohort_id: 'cohort-1', version: 3, enrollment: { domain_id: 'domain-shared', status: 'active' } }] } },
      availableFormations: { items: [] },
    });
    expect(views).toHaveLength(2);
    expect(views[0]).toMatchObject({ kind: 'common', domainId: 'domain-shared', version: 3, enrollmentStatus: 'active' });
    expect(views[1]).toMatchObject({ kind: 'personal', domainId: 'domain-personal', conceptCount: 4 });
    expect(views.some((view) => 'mastery' in view)).toBe(false);
  });

  it('ne fabrique pas de formations lorsqu’un outil est indisponible ou renvoie une erreur', () => {
    const overview = { context: { isError: true, content: [{ type: 'text', text: 'access denied' }] }, formations: null, availableFormations: { isError: true, content: [] } };
    expect(formationViews(overview)).toEqual([]);
    expect(availableFormationViews(overview)).toEqual([]);
  });

  it('lit le contrat API natif camelCase et exclut les domaines archivés', () => {
    const views = formationViews({
      context: { domains: [{ id: 'native-active', name: 'Les constellations', conceptCount: 7, archived: false }, { id: 'native-archived', name: 'Un ancien parcours', conceptCount: 5, archived: true }] },
      formations: { items: [] }, availableFormations: { items: [] },
    });
    expect(views).toEqual([{ id: 'personal:native-active', name: 'Les constellations', description: '', kind: 'personal', domainId: 'native-active', conceptCount: 7 }]);
  });

});
