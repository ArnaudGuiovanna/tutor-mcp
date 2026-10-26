import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, it, vi } from 'vitest';
import { TrainingWorkspace } from '../../src/TrainingWorkspace';
import { client, initialState } from './clientFixture';
import { trainingFixture } from './trainingFixture';

it('fait examiner la même proposition IA dans l’éditeur manuel et garde les changements en cas de conflit',async()=>{
 const {api}=client(initialState(true));const doc=trainingFixture();const proposed=structuredClone(doc);proposed.curriculum!.concepts[0].description='Définition proposée par l’agent';doc.proposal={id:'proposal',expected_revision:'1',document:proposed};
 vi.mocked(api.trainingAction).mockImplementation(async(action)=>{if(action==='get_training')return doc;if(action==='preview_training_edit')return {invalidated_concept_ids:['c1'],existing_learners_keep_version:false};if(action==='save_training_edit')throw new Error('Cette version a changé. Actualisez son état.');return {items:[],next_after:''};});
 render(<TrainingWorkspace api={api} connected epoch={0} initialId={doc.training.id} canCreate={false} busy={false} onLearn={vi.fn()} onDiscuss={vi.fn()} onConnect={vi.fn()}/>);
 const user=userEvent.setup();await user.click(await screen.findByRole('button',{name:'Examiner la proposition'}));expect((screen.getByLabelText('Définition')as HTMLTextAreaElement).value).toBe('Définition proposée par l’agent');
 await user.clear(screen.getByLabelText('Définition'));await user.type(screen.getByLabelText('Définition'),'Définition ajustée à la main');await user.click(screen.getByRole('button',{name:'Prévisualiser les conséquences'}));await screen.findByText(/1 concept\(s\) changent/);
 await user.click(screen.getByRole('button',{name:'Valider les modifications'}));expect(await screen.findByRole('alert')).toHaveProperty('textContent',expect.stringContaining('a changé'));
 expect((screen.getByLabelText('Définition')as HTMLTextAreaElement).value).toBe('Définition ajustée à la main');expect(api.trainingAction).toHaveBeenCalledWith('save_training_edit',expect.objectContaining({expected_revision:'1',document:expect.objectContaining({curriculum:expect.objectContaining({concepts:expect.arrayContaining([expect.objectContaining({description:'Définition ajustée à la main'})])})})}));
});
it('recherche et pagine sur le serveur et ne fusionne pas les résultats localement',async()=>{
 const {api}=client(initialState(true));const first=trainingFixture().training;const second={...first,id:'personal:python',name:'Python'};
 vi.mocked(api.trainingAction).mockImplementation(async(action,input)=>action==='list_training_library'?{items:input.after?[second]:[first],next_after:input.after?'':'cursor'}:{personal_learning:true,can_manage:false});
 render(<TrainingWorkspace api={api} connected epoch={0} canCreate={false} busy={false} onLearn={vi.fn()} onDiscuss={vi.fn()} onConnect={vi.fn()}/>);
 await userEvent.click(await screen.findByRole('button',{name:'Voir les formations suivantes'}));await screen.findByRole('button',{name:/Python/});expect(api.trainingAction).toHaveBeenCalledWith('list_training_library',expect.objectContaining({after:'cursor'}));
 await userEvent.type(screen.getByLabelText('Rechercher une formation'),'Go');await waitFor(()=>expect(api.trainingAction).toHaveBeenCalledWith('list_training_library',expect.objectContaining({query:'Go',limit:20})));
});

it('retire une invitation ou une délégation par le service de formation',async()=>{
 const {api}=client(initialState(true));const doc=trainingFixture();doc.curriculum=undefined;
 doc.training={...doc.training,id:'formation:shared',kind:'shared',formation_id:'shared',domain_id:undefined,cohort_id:'offering',status:'published',audience:'invitation'};
 vi.mocked(api.trainingAction).mockImplementation(async(action)=>{
  if(action==='get_training'||action==='set_training_access')return doc;
  if(action==='find_training_members')return [{membership_id:'member',name:'Camille',public_key:'public-key'}];
  if(action==='get_training_policy')return {personal_learning:true,can_manage:false};
  return {items:[],next_after:''};
 });
 render(<TrainingWorkspace api={api} connected epoch={0} initialId={doc.training.id} canCreate={false} busy={false} onLearn={vi.fn()} onDiscuss={vi.fn()} onConnect={vi.fn()}/>);
 const user=userEvent.setup();await user.click(await screen.findByRole('button',{name:'Partage et accès'}));
 await user.type(screen.getByLabelText('Nom ou clé publique'),'Camille');await user.click(screen.getByRole('button',{name:'Rechercher un membre'}));
 await user.click(await screen.findByRole('button',{name:'Retirer l’invitation'}));
 await waitFor(()=>expect(api.trainingAction).toHaveBeenCalledWith('set_training_access',expect.objectContaining({id:'formation:shared',member_id:'member',decision:'revoke',expected_revision:'1'})));
 const remove=screen.getByRole('button',{name:'Retirer l’autorisation de modification'});await waitFor(()=>expect(remove).toHaveProperty('disabled',false));await user.click(remove);
 await waitFor(()=>expect(api.trainingAction).toHaveBeenCalledWith('set_training_access',expect.objectContaining({member_id:'member',decision:'remove_editor'})));
});

it('prévisualise puis supprime une formation avec la révision confirmée du serveur',async()=>{
 const {api}=client(initialState(true));const doc=trainingFixture();doc.training.can_delete=true;
 vi.mocked(api.trainingAction).mockImplementation(async(action)=>{
  if(action==='get_training')return doc;
  if(action==='get_training_policy')return {personal_learning:true,can_manage:false};
  if(action==='preview_training_lifecycle')return {id:doc.training.id,action:'delete_training',revision:'server-revision',can_apply:true,effects:{enrolled_learners:0,conversations_affected:1,preserves_learning_history:true,will_archive:false}};
  if(action==='delete_training')return {deleted:true};
  return {items:[],next_after:''};
 });
 render(<TrainingWorkspace api={api} connected epoch={0} initialId={doc.training.id} canCreate={false} busy={false} onLearn={vi.fn()} onDiscuss={vi.fn()} onConnect={vi.fn()}/>);
 await userEvent.click(await screen.findByRole('button',{name:'Supprimer cette formation'}));await screen.findByRole('region',{name:'Conséquences de cette action'});
 expect(api.trainingAction).not.toHaveBeenCalledWith('delete_training',expect.anything());const confirm=screen.getByRole('button',{name:'Confirmer'});expect(confirm).toHaveProperty('disabled',true);
 await userEvent.click(screen.getByLabelText('J’ai lu les conséquences et je confirme cette action.'));await userEvent.click(confirm);
 await waitFor(()=>expect(api.trainingAction).toHaveBeenCalledWith('delete_training',expect.objectContaining({id:doc.training.id,expected_revision:'server-revision',confirm:true,key:expect.any(String)})));
 expect(await screen.findByText('Aucune formation trouvée')).toBeTruthy();
});
it('retrouve les formations archivées via le filtre serveur',async()=>{
 const {api}=client(initialState(true));render(<TrainingWorkspace api={api} connected epoch={0} canCreate={false} busy={false} onLearn={vi.fn()} onDiscuss={vi.fn()} onConnect={vi.fn()}/>);
 await userEvent.selectOptions(screen.getByLabelText('Afficher'),'archived');await waitFor(()=>expect(api.trainingAction).toHaveBeenCalledWith('list_training_library',expect.objectContaining({filter:'archived'})));
});
