import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import App from '../../src/App';
import { ResponseRendered, installResponseTimingCollector } from '../../src/responseTiming';
import type { ResponseRenderTiming } from '../../src/responseTiming';
import { client, conversation, initialState } from './clientFixture';

afterEach(()=>{window.tutorResponseTimings?.clear();vi.restoreAllMocks();vi.unstubAllGlobals();});

it('mesure seulement après deux images du DOM visible et exporte des métadonnées sans contenu',()=>{
  const cleanup=installResponseTimingCollector();window.tutorResponseTimings?.clear();
  const frames:FrameRequestCallback[]=[];vi.stubGlobal('requestAnimationFrame',vi.fn((callback:FrameRequestCallback)=>{frames.push(callback);return frames.length;}));
  vi.stubGlobal('cancelAnimationFrame',vi.fn());
  let visible=false;vi.spyOn(document,'visibilityState','get').mockImplementation(()=>visible?'visible':'hidden');
  const events:ResponseRenderTiming[]=[];const listener=(event:Event)=>events.push((event as CustomEvent<ResponseRenderTiming>).detail);
  window.addEventListener('tutor:response-rendered',listener);
  const view=render(<ResponseRendered timing={{conversationId:'timing-1',assistantMessageId:'answer-1',startedAt:performance.now()}}/>);
  expect(frames).toHaveLength(0);expect(events).toHaveLength(0);
  act(()=>{visible=true;document.dispatchEvent(new Event('visibilitychange'));});
  act(()=>frames.shift()!(performance.now()));expect(events).toHaveLength(0);
  act(()=>frames.shift()!(performance.now()));expect(events).toHaveLength(1);
  expect(events[0]).toMatchObject({conversationId:'timing-1',assistantMessageId:'answer-1',uiRenderMeasured:true,measurement:'after-two-animation-frames'});
  expect(events[0].elapsedMs).toBeGreaterThanOrEqual(0);expect(events[0]).not.toHaveProperty('operationId');
  const exported=JSON.parse(window.tutorResponseTimings!.export());
  expect(exported.samples).toEqual(events);expect(exported.measurement).toBe('dom-render-marker');
  view.unmount();window.removeEventListener('tutor:response-rendered',listener);cleanup();
});

it('corrèle une réponse persistée à l’opération modèle réelle puis au DOM, sans confondre le chargement de session',async()=>{
  vi.spyOn(document,'visibilityState','get').mockReturnValue('visible');
  const fixture=client(initialState(true));const originalSend=fixture.api.sendPrompt;
  fixture.api.sendPrompt=vi.fn(async(id,value)=>{
    fixture.emit({type:'turn_phase',conversationId:id,operationId:'session-loading',state:'started',phase:'preparing'});
    fixture.emit({type:'turn_phase',conversationId:id,operationId:'session-loading',state:'completed',phase:'loading_session',outcome:'completed'});
    fixture.emit({type:'turn_phase',conversationId:id,operationId:'prompt-real',state:'started',phase:'preparing'});
    fixture.emit({type:'turn_phase',conversationId:'other',operationId:'foreign',state:'started',phase:'waiting_model'});
    fixture.emit({type:'turn_phase',conversationId:id,operationId:'prompt-real',state:'started',phase:'waiting_model'});
    await originalSend(id,value);
    const state=fixture.getState();state.conversations[0].messages.push({id:'final-rendered-answer',role:'assistant',status:'complete',text:'Explication finale persistée.',createdAt:'2026-11-05T10:00:00Z'});fixture.setState(state);
    return {stopReason:'end_turn'};
  });
  const listener=vi.fn();window.addEventListener('tutor:response-rendered',listener);
  render(<App api={fixture.api}/>);const user=userEvent.setup();
  await user.type(await screen.findByLabelText('Message au tuteur'),'Explique cette règle');window.tutorResponseTimings?.clear();
  await user.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
  await screen.findByText('Explication finale persistée.');
  await waitFor(()=>expect(listener).toHaveBeenCalledTimes(1));
  const detail=(listener.mock.calls[0][0] as CustomEvent<ResponseRenderTiming>).detail;
  expect(detail).toMatchObject({conversationId:conversation.id,assistantMessageId:'final-rendered-answer',operationId:'prompt-real',uiRenderMeasured:true});
  expect(window.tutorResponseTimings!.export()).not.toMatch(/Explique|Explication|session-loading|foreign/);
  window.removeEventListener('tutor:response-rendered',listener);
});

it.each(['private','invalid','old'] as const)('ne qualifie pas une sortie %s comme réponse finale affichée',async(mode)=>{
  const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;
  const fixture=client(state);const originalSend=fixture.api.sendPrompt;
  fixture.api.sendPrompt=vi.fn(async(id,value)=>{
    await originalSend(id,value);const next=fixture.getState();
    if(mode==='old')next.conversations[0].messages.pop();
    else next.conversations[0].messages.push({id:'unrenderable',role:'assistant',status:'complete',privateOutput:mode==='private',text:mode==='private'?'PRIVATE_GRADE':'```tutor-activity\n{"invalid":true}\n```',createdAt:'2026-11-05T10:00:00Z'});
    fixture.setState(next);return{stopReason:'end_turn'};
  });
  render(<App api={fixture.api}/>);const user=userEvent.setup();
  await user.type(await screen.findByLabelText('Message au tuteur'),'Continue');window.tutorResponseTimings?.clear();
  await user.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
  await waitFor(()=>expect(screen.queryByRole('button',{name:'Interrompre'})).toBeNull());
  expect(JSON.parse(window.tutorResponseTimings!.export()).samples).toEqual([]);
});
