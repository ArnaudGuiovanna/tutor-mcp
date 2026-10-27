import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App, {TutorActivity} from '../../src/App';
import {client,initialState,conversation} from './clientFixture';
import {SettingsWorkspace} from '../../src/SettingsWorkspace';
import {english} from '../../src/messages.en';
import {t,setInterfaceLanguage} from '../../src/i18n';
import type {InstitutionOverview} from '../../src/types';

const institution:InstitutionOverview={institution:{id:'tenant-1',name:'Pilot'},membership:{id:'member-1',roles:['owner'],status:'active'},capabilities:{manage_members:true,transfer_ownership:true,request_erasure:true,last_owner:false},personal_learning:true,erasure_requests:[]};
async function settings(api:ReturnType<typeof client>['api']){render(<App api={api}/>);await screen.findByRole('heading',{name:'Qu’avez-vous envie d’apprendre ?'});await userEvent.click(screen.getByRole('button',{name:'Paramètres'}));}

describe('Compte et paramètres persistants',()=>{
 it('change réellement l’interface en anglais et retrouve la langue après réouverture sans traduire les messages historiques',async()=>{
  const state=initialState();state.conversations=[conversation];state.activeConversationId=conversation.id;const fixture=client(state);const view=render(<App api={fixture.api}/>);await screen.findByText('Commençons par observer la Grande Ourse.');
  await userEvent.click(screen.getByRole('button',{name:'Paramètres'}));await userEvent.click(screen.getByRole('tab',{name:'Ergonomie'}));await userEvent.selectOptions(screen.getByLabelText('Langue de Tutor'),'en');
  expect(await screen.findByRole('heading',{name:'Reading and appearance'})).toBeTruthy();expect(screen.getByRole('button',{name:'My account'})).toBeTruthy();expect(screen.getByRole('button',{name:'Courses'})).toBeTruthy();expect(document.documentElement.lang).toBe('en');expect(document.title).toBe('Tutor — your learning space');
  expect(fixture.getState().settings.language).toBe('en');view.unmount();render(<App api={fixture.api}/>);await screen.findByRole('button',{name:'Settings'});expect(await screen.findByText('Commençons par observer la Grande Ourse.')).toBeTruthy();expect(screen.getByLabelText('Your message to the tutor')).toBeTruthy();
 });
 it('enregistre thème, taille, interligne et réduction des animations pour le compte',async()=>{
  const fixture=client();await settings(fixture.api);await userEvent.click(screen.getByRole('tab',{name:'Ergonomie'}));await userEvent.selectOptions(screen.getByLabelText('Apparence'),'light');await waitFor(()=>expect(document.documentElement.dataset.theme).toBe('light'));
  await userEvent.selectOptions(screen.getByLabelText('Interligne'),'1.9');await userEvent.click(screen.getByLabelText('Réduire les animations'));await waitFor(()=>expect(document.documentElement.dataset.reduceMotion).toBe('true'));
  expect(fixture.getState().settings.appearance).toEqual({theme:'light',fontSize:20,lineHeight:1.9,reduceMotion:true});
 });
 it('conserve les autorisations réelles dans la portée du compte, serveur et agent',async()=>{
  const state=initialState(true);state.settings.tutor.mode='remote';state.settings.tutor.url='https://institution.example/';const fixture=client(state);
  vi.mocked(fixture.api.tutorOverview).mockResolvedValue({identity:{tenantId:'tenant-1',userId:'u',membershipId:'member-1'},context:{},formations:{},availableFormations:{}});
  vi.mocked(fixture.api.listMcpTools).mockResolvedValue({tools:[{name:'get_next_activity',description:'Next activity'},{name:'record_interaction'}]});await settings(fixture.api);
  await userEvent.click(screen.getByText('Autorisations par outil'));await screen.findByText('get_next_activity');await userEvent.selectOptions(screen.getByLabelText('Règle pour les outils Tutor'),'allow');
  await waitFor(()=>expect(fixture.getState().settings.permissionPolicy).toEqual({origin:'https://institution.example',institutionId:'tenant-1',agentId:state.settings.targetId,defaultDecision:'allow',tools:{}}));
  await userEvent.selectOptions(screen.getByLabelText('Autorisation pour record_interaction'),'deny');await waitFor(()=>expect(fixture.getState().settings.permissionPolicy?.tools.record_interaction).toBe('deny'));
  await userEvent.click(screen.getByRole('tab',{name:'Ergonomie'}));await userEvent.click(screen.getByRole('tab',{name:'Connexion et outils'}));expect((await screen.findByLabelText('Règle pour les outils Tutor')as HTMLSelectElement).value).toBe('allow');
  await userEvent.click(screen.getByText('Autorisations par outil'));expect((screen.getByLabelText('Autorisation pour record_interaction')as HTMLSelectElement).value).toBe('deny');
 });
 it('n’affiche pas comme active une politique d’une autre institution',async()=>{
  const state=initialState(true);state.settings.tutor.mode='remote';state.settings.tutor.url='https://institution.example';state.settings.permissionPolicy={origin:'https://elsewhere.example',institutionId:'tenant-other',agentId:state.settings.targetId,defaultDecision:'allow',tools:{}};const fixture=client(state);
  render(<SettingsWorkspace api={fixture.api} snapshot={state} overview={{identity:{tenantId:'tenant-1',userId:'u',membershipId:'m'},context:{},formations:{},availableFormations:{}}} busy={false} renderConnection={()=>null} onSaved={()=>{}}/>);expect((screen.getByLabelText('Règle pour les outils Tutor')as HTMLSelectElement).value).toBe('ask');
 });
 it('confirme le retrait local sans demander un effacement institutionnel',async()=>{
  const state=initialState();state.profile={id:'profile-1',label:'Compte à retirer'};const fixture=client(state);render(<App api={fixture.api}/>);await screen.findByRole('button',{name:'Mon compte'});await userEvent.click(screen.getByRole('button',{name:'Mon compte'}));await userEvent.click(screen.getByRole('button',{name:'Retirer ce compte de cet appareil'}));
  const button=screen.getByRole('button',{name:'Confirmer le retrait de cet appareil'});expect((button as HTMLButtonElement).disabled).toBe(true);await userEvent.click(screen.getByLabelText(/Je souhaite retirer ce compte/));await userEvent.click(button);await waitFor(()=>expect(fixture.api.deleteProfile).toHaveBeenCalledWith('profile-1','profile-1'));expect(fixture.api.institutionAction).not.toHaveBeenCalledWith('request_erasure',expect.anything());
 });
 it('empêche l’effacement du dernier propriétaire et explique le transfert',async()=>{
  const state=initialState(true);const fixture=client(state);vi.mocked(fixture.api.institutionAction).mockResolvedValue({...institution,capabilities:{...institution.capabilities,last_owner:true}});
  render(<App api={fixture.api}/>);await screen.findByRole('button',{name:'Mon compte'});await userEvent.click(screen.getByRole('button',{name:'Mon compte'}));expect(await screen.findByText(/Vous êtes le dernier propriétaire actif/)).toBeTruthy();expect((screen.getByRole('button',{name:'Demander l’effacement institutionnel'})as HTMLButtonElement).disabled).toBe(true);
 });
 it('charge les membres du serveur et confirme un retrait avec son identifiant réel',async()=>{
  const state=initialState(true);const fixture=client(state);vi.mocked(fixture.api.institutionAction).mockImplementation(async action=>action==='overview'?institution:action==='list_members'?{items:[{membership_id:'member-2',user_id:'user-2',name:'Alice',roles:['learner'],status:'active',public_key:'npub1alice'}],next_after:''}:{status:'removed'});
  await settings(fixture.api);await userEvent.click(screen.getByRole('tab',{name:'Membres'}));await screen.findByRole('heading',{name:'Alice'});await userEvent.click(screen.getByRole('button',{name:'Retirer le membre'}));expect(fixture.api.institutionAction).not.toHaveBeenCalledWith('remove_member',expect.anything());await userEvent.click(screen.getByRole('button',{name:'Confirmer'}));await waitFor(()=>expect(fixture.api.institutionAction).toHaveBeenCalledWith('remove_member',expect.objectContaining({member_id:'member-2',confirm:true,key:expect.any(String)})));
 });
});

describe('Activité textuelle du tuteur',()=>{
 it('suit les appels réels puis la rédaction sans dépliant',()=>{
  const view=render(<TutorActivity tools={[{id:'tool-1',title:'tutor/get_next_activity',status:'in_progress'}]} streaming={false} permission={false} preparing=""/>);expect(screen.getByRole('status').textContent).toContain('Je prépare la prochaine activité…');expect(document.querySelector('details')).toBeNull();
  view.rerender(<TutorActivity tools={[{id:'tool-1',title:'tutor/get_next_activity',status:'completed'}]} streaming permission={false} preparing=""/>);expect(screen.getByRole('status').textContent).toContain('Je rédige la réponse…');
  view.rerender(<TutorActivity tools={[]} streaming={false} permission preparing=""/>);expect(screen.getByRole('status').textContent).toContain('Votre autorisation est attendue.');
 });
 it('conserve les paramètres des messages traduits',()=>{setInterfaceLanguage('en');expect(t('{count} apprenant(s) inscrit(s) conservent leur version.',{count:4})).toBe('4 enrolled learner(s) keep their version.');expect(english['Langue de Tutor']).toBe('Tutor language');});
});

it('propose seulement les options de modèle, réflexion et vitesse réellement exposées par l’agent',async()=>{
 const state=initialState(true);const fixture=client(state);
 const config={connected:true,configOptions:[{id:'model',name:'Model',category:'model',type:'select',currentValue:'model-a',options:[{group:'Provider',options:[{value:'model-a',name:'Model A'},{value:'model-b',name:'Model B'}]}]},{id:'thought_level',name:'Reasoning',category:'thought_level',type:'select',currentValue:'low',options:[{value:'low',name:'Low'},{value:'high',name:'High'}]}],current:{model:'model-a',reasoningEffort:'low',speed:'balanced'},supported:{model:true,reasoningEffort:true,speed:true},speedOptions:['balanced','fast']};
 vi.mocked(fixture.api.agentConfiguration).mockResolvedValue(config);vi.mocked(fixture.api.setAgentConfiguration).mockImplementation(async configuration=>({...config,current:configuration}));await settings(fixture.api);
 await userEvent.click(screen.getAllByText('Modèle et rapidité')[0]);const select=await screen.findByLabelText('Modèle');await userEvent.selectOptions(select,'model-b');await waitFor(()=>expect(fixture.api.setAgentConfiguration).toHaveBeenCalledWith({model:'model-b',reasoningEffort:'low',speed:'balanced'}));
 expect([...screen.getByLabelText('Préférence de réponse').querySelectorAll('option')].map(option=>(option as HTMLOptionElement).value)).toEqual(['balanced','fast']);
});

it('présente en anglais les statuts de membre et garde la représentation technique de sa clé dans les détails',async()=>{
 const state=initialState(true);const fixture=client(state);setInterfaceLanguage('en');vi.mocked(fixture.api.institutionAction).mockImplementation(async action=>action==='overview'?institution:{items:[{membership_id:'member-2',user_id:'user-2',name:'Alice',roles:['owner'],status:'active',public_key:'a'.repeat(64)}],next_after:''});
 render(<SettingsWorkspace api={fixture.api} snapshot={state} overview={null} busy={false} initialTab="members" renderConnection={()=>null} onSaved={()=>{}}/>);await screen.findByRole('heading',{name:'Alice'});expect(screen.getByText('Owner · Active')).toBeTruthy();const details=screen.getByText('Member public identifier').parentElement as HTMLDetailsElement;expect(details.tagName).toBe('DETAILS');expect(details.open).toBe(false);expect(screen.queryByText(/Active enrolment/)).toBeNull();
});
it('traduit les diagnostics natifs avec code HTTP sans modifier leur code technique',()=>{setInterfaceLanguage('en');expect(t('Clé non autorisée ou permissions insuffisantes : demandez l’accès à votre institution. (HTTP 403).')).toContain('Your account key is not authorised');expect(t('L’agent s’est arrêté avec une erreur (code 1). Vérifiez sa connexion et son installation.')).toContain('error (code 1)');});

it('conserve le reçu pending d’un effacement accepté lorsque le serveur retire ensuite l’accès au suivi',async()=>{
 const state=initialState(true);const fixture=client(state);let requested=false;
 vi.mocked(fixture.api.institutionAction).mockImplementation(async action=>{
  if(action==='request_erasure'){requested=true;return {request:{id:'erasure-1',status:'pending',created_at:'2026-10-26T12:00:00Z'}};}
  if(requested)throw new Error('Vos accès ont changé. Reconnectez ce compte.');
  return institution;
 });
 render(<App api={fixture.api}/>);await screen.findByRole('button',{name:'Mon compte'});await userEvent.click(screen.getByRole('button',{name:'Mon compte'}));await waitFor(()=>expect((screen.getByLabelText('Je demande la suppression de mes données dans cette institution.')as HTMLInputElement).disabled).toBe(false));
 await userEvent.click(screen.getByLabelText('Je demande la suppression de mes données dans cette institution.'));await userEvent.click(screen.getByRole('button',{name:'Demander l’effacement institutionnel'}));
 expect(await screen.findByText('Demande d’effacement : En attente')).toBeTruthy();expect(screen.queryByText(/Effacement terminé/)).toBeNull();expect(await screen.findByRole('alert')).toHaveProperty('textContent',expect.stringContaining('La demande est enregistrée.'));
});

it('connecte Tutor puis l’agent et règle les outils sur le même écran, sans écraser la nouvelle adresse',async()=>{
 const state=initialState();state.settings.tutor={mode:'remote',url:'https://old.example',command:'tutor-mcp',args:['--local']};state.identity={exists:true,backupConfirmed:true,needsBackup:false};
 const fixture=client(state);vi.mocked(fixture.api.tutorOverview).mockResolvedValue({identity:{tenantId:'tenant-1',userId:'u',membershipId:'member-1'},context:{},formations:{},availableFormations:{}});
 vi.mocked(fixture.api.listMcpTools).mockResolvedValue({tools:[{name:'record_interaction'}]});
 await settings(fixture.api);const user=userEvent.setup();
 const address=screen.getByLabelText('Adresse de Tutor');await user.clear(address);await user.type(address,'https://new.example');
 await user.click(screen.getByRole('button',{name:'Vérifier l’institution'}));await waitFor(()=>expect((screen.getByRole('button',{name:'Connecter avec ma clé'})as HTMLButtonElement).disabled).toBe(false));
 await user.click(screen.getByRole('button',{name:'Connecter avec ma clé'}));await waitFor(()=>expect(fixture.api.connectTutor).toHaveBeenCalledWith('https://new.example',false));
 expect(screen.getByRole('tab',{name:'Connexion et outils'}).getAttribute('aria-selected')).toBe('true');
 await waitFor(()=>expect((screen.getByRole('button',{name:'Rechercher les installations'})as HTMLButtonElement).disabled).toBe(false));
 await user.click(screen.getByRole('button',{name:'Rechercher les installations'}));await screen.findByRole('radio',{name:/Codex — Ubuntu/});
 await user.click(screen.getByRole('button',{name:'Connecter Codex'}));await waitFor(()=>expect(fixture.getState().agentConnected).toBe(true));
 expect(fixture.getState().settings.tutor.url).toBe('https://new.example');
 await user.selectOptions(screen.getByLabelText('Règle pour les outils Tutor'),'allow');
 await waitFor(()=>expect(fixture.getState().settings.permissionPolicy).toMatchObject({origin:'https://new.example',institutionId:'tenant-1',agentId:state.settings.targetId,defaultDecision:'allow'}));
 expect(screen.getByRole('tab',{name:'Connexion et outils'}).getAttribute('aria-selected')).toBe('true');
 await user.click(screen.getByRole('tab',{name:'Institution'}));expect(screen.queryByLabelText('Adresse de Tutor')).toBeNull();
 await user.click(screen.getByRole('button',{name:'Gérer la connexion et les outils'}));expect(screen.getByRole('tab',{name:'Connexion et outils'}).getAttribute('aria-selected')).toBe('true');
});

it('préserve une exception refusée lorsque la règle générale change',async()=>{
 const state=initialState(true);state.settings.tutor={mode:'remote',url:'https://institution.example',command:'',args:[]};state.settings.permissionPolicy={origin:'https://institution.example',institutionId:'tenant-1',agentId:state.settings.targetId,defaultDecision:'ask',tools:{record_interaction:'deny'}};
 const fixture=client(state);vi.mocked(fixture.api.tutorOverview).mockResolvedValue({identity:{tenantId:'tenant-1',userId:'u',membershipId:'m'},context:{},formations:{},availableFormations:{}});
 await settings(fixture.api);await userEvent.selectOptions(screen.getByLabelText('Règle pour les outils Tutor'),'allow');
 await waitFor(()=>expect(fixture.getState().settings.permissionPolicy?.defaultDecision).toBe('allow'));expect(fixture.getState().settings.permissionPolicy?.tools.record_interaction).toBe('deny');
});

it('crée et sauvegarde l’identité nécessaire directement dans la connexion Tutor',async()=>{
 const state=initialState();state.settings.tutor={mode:'remote',url:'https://institution.example',command:'',args:[]};const fixture=client(state);await settings(fixture.api);
 await userEvent.click(screen.getByRole('button',{name:'Créer mon identité'}));
 expect(await screen.findByRole('region',{name:'Sauvegarde obligatoire'})).toBeTruthy();expect(screen.getByRole('tab',{name:'Connexion et outils'}).getAttribute('aria-selected')).toBe('true');
 await userEvent.type(screen.getByLabelText('Phrase pour chiffrer la sauvegarde'),'une phrase de test');await userEvent.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'),'une phrase de test');
 await userEvent.click(screen.getByRole('button',{name:'Enregistrer ma sauvegarde chiffrée'}));await waitFor(()=>expect(fixture.getState().identity.backupConfirmed).toBe(true));
 expect(await screen.findByRole('button',{name:'Connecter avec ma clé'})).toBeTruthy();expect(screen.queryByRole('region',{name:'Sauvegarde obligatoire'})).toBeNull();
});
