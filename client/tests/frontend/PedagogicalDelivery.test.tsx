import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import App from '../../src/App';
import { client, conversation, initialState } from './clientFixture';

afterEach(()=>{vi.restoreAllMocks();vi.unstubAllGlobals();});

it('présente et ferme le consentement natif même avant le démarrage du modèle',async()=>{
  const fixture=client(initialState(true));render(<App api={fixture.api}/>);
  await screen.findByLabelText('Message au tuteur');
  act(()=>fixture.emit({type:'native_permission',id:'native:one',params:{operations:['prepare_pedagogical_turn'],toolCall:{title:'Préparer votre échange pédagogique'},options:[{optionId:'allow_once',kind:'allow_once',name:'Autoriser une fois'}]}}));
  await userEvent.click(await screen.findByRole('button',{name:'Autoriser une fois'}));
  expect(fixture.api.answerPermission).toHaveBeenCalledWith('native:one','allow_once');
  act(()=>fixture.emit({type:'native_permission',id:'native:two',params:{toolCall:{title:'Seconde demande'},options:[]}}));
  expect(await screen.findByText('Seconde demande')).toBeTruthy();
  act(()=>fixture.emit({type:'native_permission_closed',id:'native:two'}));
  expect(screen.queryByText('Seconde demande')).toBeNull();
});

it('acquitte uniquement un retour terminé devenu visible et ne transmet pas le texte privé',async()=>{
  let intersect:IntersectionObserverCallback=()=>{};
  vi.stubGlobal('IntersectionObserver',class {constructor(callback:IntersectionObserverCallback){intersect=callback;}observe(){}disconnect(){}unobserve(){}});
  vi.spyOn(document,'visibilityState','get').mockReturnValue('visible');
  const state=initialState(true);state.activeConversationId=conversation.id;
  state.conversations=[{...conversation,messages:[{id:'feedback',role:'assistant',status:'streaming',text:'PRIVATE_GRADE',privateOutput:true,createdAt:conversation.createdAt}]}];
  const fixture=client(state);render(<App api={fixture.api}/>);
  await screen.findByLabelText('Message au tuteur');
  expect(screen.queryByText('PRIVATE_GRADE')).toBeNull();
  expect(fixture.api.acknowledgeLearningDelivery).not.toHaveBeenCalled();
  const completed=fixture.getState();completed.conversations[0].messages[0]={id:'feedback',role:'assistant',status:'complete',text:'Retour confirmé',createdAt:conversation.createdAt,learningDelivery:{activityId:'activity',domainId:'go',concept:'sum',kind:'feedback',eventKey:'delivery',recorded:false,success:true}};
  act(()=>fixture.broadcast(completed));await screen.findByText('Retour confirmé');
  expect(fixture.api.acknowledgeLearningDelivery).not.toHaveBeenCalled();
  act(()=>intersect([{isIntersecting:true} as IntersectionObserverEntry],{} as IntersectionObserver));
  await waitFor(()=>expect(fixture.api.acknowledgeLearningDelivery).toHaveBeenCalledExactlyOnceWith(conversation.id,'feedback'));
  act(()=>intersect([{isIntersecting:true} as IntersectionObserverEntry],{} as IntersectionObserver));
  expect(fixture.api.acknowledgeLearningDelivery).toHaveBeenCalledTimes(1);
});
