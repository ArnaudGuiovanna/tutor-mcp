import { act, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { TutorActivity } from '../../src/App';
import { mergeToolActivity } from '../../src/model';

afterEach(() => vi.useRealTimers());

it('montre le temps écoulé avant le premier token et garde les permissions explicites', () => {
  vi.useFakeTimers();
  const view = render(<TutorActivity tools={[]} streaming={false} permission={false} preparing=""/>);
  expect(screen.getByRole('status').textContent).toContain('J’attends la réponse de l’agent…');
  act(() => vi.advanceTimersByTime(3000));
  expect(screen.getByText('Total : 3 s · Étape : 3 s')).toBeTruthy();
  // This text remains useful when CSS animations are disabled. It is hidden
  // from live announcements so screen readers do not speak every second.
  expect(screen.getByText('Total : 3 s · Étape : 3 s').getAttribute('aria-hidden')).toBe('true');
  view.rerender(<TutorActivity tools={[]} streaming={false} permission preparing=""/>);
  expect(screen.getByRole('status').textContent).toContain('Votre autorisation est attendue.');
  act(() => vi.advanceTimersByTime(2000));
  expect(screen.getByText('Total : 5 s · Étape : 2 s')).toBeTruthy();
  view.rerender(<TutorActivity tools={[]} streaming permission={false} preparing=""/>);
  expect(screen.getByRole('status').textContent).toContain('Je rédige la réponse…');
  view.unmount();
  expect(vi.getTimerCount()).toBe(0);
});


it('distingue le temps total de celui d’un nouvel outil et du suivant', () => {
  vi.useFakeTimers();
  const view=render(<TutorActivity tools={[]} streaming={false} permission={false} preparing="Préparation"/>);
  act(()=>vi.advanceTimersByTime(20000));
  view.rerender(<TutorActivity tools={[{id:'one',title:'get_next_activity',status:'in_progress'}]} streaming={false} permission={false} preparing=""/>);
  expect(screen.getByText('Total : 20 s · Outil : 0 s')).toBeTruthy();
  act(()=>vi.advanceTimersByTime(3000));
  expect(screen.getByText('Total : 23 s · Outil : 3 s')).toBeTruthy();
  view.rerender(<TutorActivity tools={[{id:'one',title:'get_next_activity',status:'completed'},{id:'two',title:'get_learner_context',status:'in_progress'}]} streaming={false} permission={false} preparing=""/>);
  expect(screen.getByText('Total : 23 s · Outil : 0 s')).toBeTruthy();
});

it('conserve le total lorsqu’une conversation nouvellement créée remonte le composant',()=>{
  vi.useFakeTimers();const startedAt=performance.now();act(()=>vi.advanceTimersByTime(4000));
  render(<TutorActivity startedAt={startedAt} tools={[]} streaming={false} permission={false} preparing=""/>);
  expect(screen.getByText('Total : 4 s · Étape : 0 s')).toBeTruthy();
});

it('garde des durées continues quand l’horloge système avance puis recule',()=>{
  vi.useFakeTimers();
  const view=render(<TutorActivity tools={[]} streaming={false} permission={false} preparing="Préparation"/>);
  act(()=>vi.advanceTimersByTime(2000));
  act(()=>{vi.setSystemTime(Date.now()+3600000);vi.advanceTimersByTime(1000);});
  expect(screen.getByText('Total : 3 s · Étape : 3 s')).toBeTruthy();
  const tools=mergeToolActivity([],{id:'one',title:'get_learner_context',status:'in_progress'});
  view.rerender(<TutorActivity tools={tools} streaming={false} permission={false} preparing=""/>);
  act(()=>{vi.setSystemTime(Date.now()-7200000);vi.advanceTimersByTime(2000);});
  expect(screen.getByText('Total : 5 s · Outil : 2 s')).toBeTruthy();
  const completed=mergeToolActivity(tools,{id:'one',title:'',status:'completed'})[0];
  expect(completed.completedAt!-completed.startedAt!).toBe(2000);
});
