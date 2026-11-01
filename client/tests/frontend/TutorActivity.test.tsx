import { act, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { TutorActivity } from '../../src/App';

afterEach(() => vi.useRealTimers());

it('montre le temps écoulé avant le premier token et garde les permissions explicites', () => {
  vi.useFakeTimers();
  const view = render(<TutorActivity tools={[]} streaming={false} permission={false} preparing=""/>);
  expect(screen.getByRole('status').textContent).toContain('Je prépare la réponse…');
  act(() => vi.advanceTimersByTime(3000));
  expect(screen.getByText('3 s écoulées')).toBeTruthy();
  // This text remains useful when CSS animations are disabled. It is hidden
  // from live announcements so screen readers do not speak every second.
  expect(screen.getByText('3 s écoulées').getAttribute('aria-hidden')).toBe('true');
  view.rerender(<TutorActivity tools={[]} streaming={false} permission preparing=""/>);
  expect(screen.getByRole('status').textContent).toContain('Votre autorisation est attendue.');
  act(() => vi.advanceTimersByTime(2000));
  expect(screen.getByText('5 s écoulées')).toBeTruthy();
  view.rerender(<TutorActivity tools={[]} streaming permission={false} preparing=""/>);
  expect(screen.getByRole('status').textContent).toContain('Je rédige la réponse…');
  view.unmount();
  expect(vi.getTimerCount()).toBe(0);
});
