import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { FormationReviewPanel } from '../../src/FormationReviewPanel';
import type { ClientBridge } from '../../src/bridge';
import type { FormationReview } from '../../src/types';

const review: FormationReview = {detail:{formation:{id:'formation-real',name:'Confitures au XIIe siècle',description:'Un programme **sourcé**',status:'draft',enrollment_policy:'invitation'},version:{id:'version-real',formation_id:'formation-real',version:1,status:'draft'},modules:[{id:'m',stable_key:'histoire',title:'Conserver les fruits',position:1}],concepts:[{id:'c',stable_key:'c',module_stable_key:'histoire',label:'Le miel',outcomes:[{id:'o',statement:'Comparer les techniques',observable:'Citer deux différences'}],criteria:[{id:'cr',description:'Sources vérifiables'}]}]},publishable:true,issues:[],catalog_available:false};
function fixture(value=review){
 const api={formationVersions:vi.fn(async()=>({items:[review.detail.version],next_after:''})),formationReview:vi.fn(async()=>structuredClone(value)),openExternalLink:vi.fn(async()=>{})} as unknown as ClientBridge;
 const publish=vi.fn(async()=>true),clone=vi.fn(async()=>undefined),discuss=vi.fn();
 render(<FormationReviewPanel formationId="formation-real" api={api} canPublish busy={false} onClose={()=>{}} onDiscuss={discuss} onPublish={publish} onClone={clone}/>);
 return {api,publish,clone,discuss};
}
describe('Examen et publication de la version réelle',()=>{
 it('montre les objectifs et attend la décision explicite avant publication sans inscription',async()=>{
  const user=userEvent.setup();const {api,publish}=fixture();
  await screen.findByText('Comparer les techniques');
  expect(api.formationReview).toHaveBeenCalledWith('version-real');
  expect(screen.getByText('Sources vérifiables')).toBeTruthy();
  const button=screen.getByRole('button',{name:'Publier dans mon institution'}) as HTMLButtonElement;
  expect(button.disabled).toBe(true);expect(publish).not.toHaveBeenCalled();
  await user.click(screen.getByRole('checkbox',{name:/J’ai examiné cette version/}));await user.click(button);
  await waitFor(()=>expect(publish).toHaveBeenCalledWith('version-real',expect.any(String)));
  await screen.findByText(/personne n’a été inscrit automatiquement/);
  expect(screen.queryByRole('button',{name:'Publier dans mon institution'})).toBeNull();
 });
 it('refuse un état de version différent de celui demandé',async()=>{
  const wrong=structuredClone(review);wrong.detail.version.id='another-version';const {publish}=fixture(wrong);
  expect(await screen.findByRole('alert')).toHaveProperty('textContent',expect.stringContaining('ne correspond pas'));
  expect(screen.queryByRole('checkbox')).toBeNull();expect(publish).not.toHaveBeenCalled();
 });
 it('affiche les blocages du moteur avant de proposer de publier',async()=>{
  const empty=structuredClone(review);empty.publishable=false;empty.issues=['Ajoutez des critères observables.'];const {publish}=fixture(empty);
  await screen.findByText('Ajoutez des critères observables.');expect(screen.queryByRole('button',{name:'Publier dans mon institution'})).toBeNull();expect(publish).not.toHaveBeenCalled();
 });
});
