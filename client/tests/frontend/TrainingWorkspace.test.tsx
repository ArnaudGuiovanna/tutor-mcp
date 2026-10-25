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
