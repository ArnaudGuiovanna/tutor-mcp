import { useState } from 'react';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ReviewPlanning } from '../../src/ReviewPlanning';
import type { ReviewPlanningProps } from '../../src/ReviewPlanning';
import { loadReviewTrainings, reviewGroup, reviewPlanKey, reviewPreference, reviewTimestamp, scheduledReviews } from '../../src/reviewPlanning';
import type { ReviewPlanPreference } from '../../src/reviewPlanning';
import type { Training } from '../../src/types';
import { client, initialState } from './clientFixture';
import { trainingFixture } from './trainingFixture';

const scope = 'profile-alex:institution-one';
const plan: ReviewPlanPreference = { enabled: true, format: 'quiz' };
function formation(id = 'personal:go', name = 'Go', due = '2026-10-28T08:00:00Z'): Training {
  return { ...trainingFixture().training, id, name, domain_id: id.replace('personal:', ''), progress: due ? [{ concept: 'Variables', mastery: 0.4, next_review: due }] : [] };
}
function setup(items: Training[] = [formation()]): ReviewPlanningProps {
  const { api } = client(initialState(true));
  vi.mocked(api.trainingAction).mockResolvedValue({ items, next_after: '' });
  return { api, scope, preferences: { [reviewPlanKey(scope, 'personal:go')]: plan }, connected: true, busy: false, refreshKey: 0, onPreference: vi.fn(async () => {}), onStart: vi.fn() };
}
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: Error) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

describe('server review projection', () => {
  it('paginates, deduplicates and excludes inaccessible, archived and unenrolled formations', async () => {
    const props = setup();
    const go = formation();
    const python = formation('personal:python', 'Python');
    vi.mocked(props.api.trainingAction).mockResolvedValueOnce({ items: [go, { ...python, status: 'archived' }, { ...go, id: 'shared', domain_id: undefined }], next_after: 'page-2' })
      .mockResolvedValueOnce({ items: [go, { ...formation('revoked', 'Révoquée'), can_learn: false }], next_after: '' });
    const result = await loadReviewTrainings(props.api);
    expect(result.map(item => item.id)).toEqual(['personal:go']);
    expect(props.api.trainingAction).toHaveBeenNthCalledWith(2, 'list_training_library', { limit: 100, after: 'page-2' });
  });

  it('rejects a repeated cursor instead of silently showing a partial planning', async () => {
    const props = setup();
    vi.mocked(props.api.trainingAction).mockResolvedValue({ items: [formation()], next_after: 'same' });
    await expect(loadReviewTrainings(props.api)).rejects.toThrow('incomplet');
    expect(props.api.trainingAction).toHaveBeenCalledTimes(2);
  });

  it('does not infer dates from mastery, last review or invalid server dates', () => {
    const go = formation();
    go.progress = [
      { concept: 'No schedule', mastery: 0.9, last_review: '2026-10-28T08:00:00Z' },
      { concept: 'Bad date', mastery: 0.8, next_review: '2026-02-30T08:00:00Z' },
      { concept: 'Zero', mastery: 0.8, next_review: '0001-01-01T00:00:00Z' },
      { concept: 'No zone', mastery: 0.8, next_review: '2026-10-28T08:00:00' },
      { concept: 'Real', mastery: 0.1, next_review: '2026-10-29T08:00:00.123456Z' },
      { concept: 'Real', mastery: 0.2, next_review: '2026-10-30T08:00:00Z' },
    ];
    expect(scheduledReviews(go)).toEqual([{ concept: 'Real', dueAt: '2026-10-29T08:00:00.123456Z', timestamp: Date.parse('2026-10-29T08:00:00.123Z') }]);
    expect(reviewTimestamp('tomorrow')).toBeUndefined();
    expect(reviewTimestamp('2026-13-01T08:00:00Z')).toBeUndefined();
    const now = new Date(2026, 9, 28, 12);
    expect(reviewGroup(new Date(2026, 9, 27, 23).getTime(), now)).toBe('overdue');
    expect(reviewGroup(new Date(2026, 9, 28, 23).getTime(), now)).toBe('today');
    expect(reviewGroup(new Date(2026, 9, 29, 0).getTime(), now)).toBe('upcoming');
    expect(reviewGroup(undefined, now)).toBe('waiting');
  });

  it('keeps preferences disabled by default and isolated by account/institution scope', () => {
    const preferences = { [reviewPlanKey(scope, 'personal:go')]: plan };
    expect(reviewPreference(preferences, scope, 'personal:go')).toEqual(plan);
    expect(reviewPreference(preferences, 'other-institution', 'personal:go')).toEqual({ enabled: false, format: 'quiz' });
    expect(reviewPlanKey('a:b', 'c')).not.toBe(reviewPlanKey('a', 'b:c'));
  });
});

describe('Planning screen', () => {
  it('opens the correct formation and modality in one click and exposes server dates', async () => {
    const go = formation();
    const props = setup([go]);
    props.preferences[reviewPlanKey(scope, go.id)] = { enabled: true, format: 'flashcard' };
    render(<ReviewPlanning {...props} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Réviser Go' }));
    expect(props.onStart).toHaveBeenCalledExactlyOnceWith(go, 'flashcard', 'Variables');
    const details = screen.getByText('Voir les révisions programmées (1)');
    await userEvent.click(details);
    expect(screen.getByText('Variables')).toBeTruthy();
    expect(document.querySelector('time')?.getAttribute('dateTime')).toBe(go.progress[0].next_review);
  });

  it('persists activation and format before making the revision available', async () => {
    const props = setup();
    const save = vi.fn(async () => {});
    function Harness() {
      const [preferences, setPreferences] = useState<Record<string, ReviewPlanPreference>>({});
      return <ReviewPlanning {...props} preferences={preferences} onPreference={async (id, next) => {
        await save(); setPreferences(previous => ({ ...previous, [reviewPlanKey(scope, id)]: next }));
      }} />;
    }
    render(<Harness />);
    const toggle = await screen.findByRole('switch', { name: 'Répétition espacée pour Go' });
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect(screen.queryByRole('button', { name: 'Réviser Go' })).toBeNull();
    await userEvent.selectOptions(screen.getByLabelText('Format de révision pour Go'), 'flashcard');
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    await userEvent.click(toggle);
    await waitFor(() => expect(screen.getByRole('switch', { name: 'Répétition espacée pour Go' }).getAttribute('aria-checked')).toBe('true'));
    expect(document.activeElement).toBe(screen.getByRole('switch', { name: 'Répétition espacée pour Go' }));
    await userEvent.click(screen.getByRole('button', { name: 'Réviser Go' }));
    expect(props.onStart).toHaveBeenCalledWith(expect.objectContaining({ id: 'personal:go' }), 'flashcard', 'Variables');
    expect(save).toHaveBeenCalledTimes(2);
  });

  it('retains the saved setting and allows retry if persistence fails', async () => {
    const props = setup(); props.preferences = {};
    props.onPreference = vi.fn(async () => { throw new Error('Disque indisponible'); });
    render(<ReviewPlanning {...props} />);
    const toggle = await screen.findByRole('switch', { name: 'Répétition espacée pour Go' });
    await userEvent.click(toggle);
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Disque indisponible');
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    expect(toggle).toHaveProperty('disabled', false);
    expect(screen.queryByRole('button', { name: 'Réviser Go' })).toBeNull();
  });

  it('shows an enabled plan without inventing an activity or date before any learning', async () => {
    const props = setup([formation('personal:go', 'Go', '')]);
    render(<ReviewPlanning {...props} />);
    expect(await screen.findByText('Pas encore de révision programmée')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Réviser Go' })).toBeNull();
    expect(document.querySelector('time')).toBeNull();
    expect(props.onStart).not.toHaveBeenCalled();
  });

  it('distinguishes an unavailable date from a formation with no schedule', async () => {
    render(<ReviewPlanning {...setup([formation('personal:go', 'Go', 'wrong-date')])} />);
    expect(await screen.findByText('Échéance indisponible')).toBeTruthy();
    expect(screen.queryByText('Pas encore de révision programmée')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Réviser Go' })).toBeNull();
  });

  it('discards stale responses across accounts and stops following the previous pagination', async () => {
    const props = setup();
    const first = deferred<unknown>();
    vi.mocked(props.api.trainingAction).mockReturnValueOnce(first.promise).mockResolvedValueOnce({ items: [formation('personal:python', 'Python')], next_after: '' });
    const view = render(<ReviewPlanning {...props} />);
    await waitFor(() => expect(props.api.trainingAction).toHaveBeenCalledTimes(1));
    view.rerender(<ReviewPlanning {...props} scope="other-institution" preferences={{}} />);
    await screen.findByRole('heading', { name: 'Python' });
    await act(async () => first.resolve({ items: [formation()], next_after: 'private-page-2' }));
    expect(screen.queryByRole('heading', { name: 'Go' })).toBeNull();
    expect(props.api.trainingAction).toHaveBeenCalledTimes(2);
  });

  it('hides loaded private rows immediately on disconnection', async () => {
    const props = setup();
    const view = render(<ReviewPlanning {...props} />);
    await screen.findByRole('heading', { name: 'Go' });
    view.rerender(<ReviewPlanning {...props} connected={false} />);
    expect(screen.queryByRole('heading', { name: 'Go' })).toBeNull();
    expect(screen.getByRole('status').textContent).toContain('Connectez Tutor');
  });

  it('does not let a previous account save block or show errors in the new account', async () => {
    const props = setup();
    const oldSave = deferred<void>();
    props.onPreference = vi.fn().mockReturnValueOnce(oldSave.promise).mockResolvedValueOnce(undefined);
    const view = render(<ReviewPlanning {...props} />);
    await userEvent.click(await screen.findByRole('switch', { name: 'Répétition espacée pour Go' }));
    view.rerender(<ReviewPlanning {...props} scope="other-institution" preferences={{}} />);
    const nextToggle = await screen.findByRole('switch', { name: 'Répétition espacée pour Go' });
    expect(nextToggle).toHaveProperty('disabled', false);
    await userEvent.click(nextToggle);
    expect(props.onPreference).toHaveBeenCalledTimes(2);
    await act(async () => oldSave.reject(new Error('Old account failure')));
    expect(screen.queryByText('Old account failure')).toBeNull();
    expect(nextToggle).toHaveProperty('disabled', false);
  });

  it('refreshes dates from the server after a revision instead of updating them locally', async () => {
    const props = setup();
    const view = render(<ReviewPlanning {...props} />);
    await screen.findByRole('button', { name: 'Réviser Go' });
    expect(document.querySelector('time')?.getAttribute('dateTime')).toBe('2026-10-28T08:00:00Z');
    vi.mocked(props.api.trainingAction).mockResolvedValue({ items: [formation('personal:go', 'Go', '2026-11-02T10:00:00Z')], next_after: '' });
    view.rerender(<ReviewPlanning {...props} refreshKey={1} />);
    await waitFor(() => expect(document.querySelector('time')?.getAttribute('dateTime')).toBe('2026-11-02T10:00:00Z'));
    expect(props.api.trainingAction).toHaveBeenCalledTimes(2);
  });

  it('offers a retry after failure and an honest empty state', async () => {
    const props = setup();
    vi.mocked(props.api.trainingAction).mockRejectedValueOnce(new Error('Tutor indisponible')).mockResolvedValueOnce({ items: [], next_after: '' });
    render(<ReviewPlanning {...props} />);
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Tutor indisponible');
    await userEvent.click(screen.getByRole('button', { name: 'Actualiser le planning' }));
    expect(await screen.findByRole('heading', { name: 'Votre planning se remplira avec vos apprentissages' })).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('supports keyboard activation and disables action during an in-flight save', async () => {
    const props = setup();
    const save = deferred<void>(); props.onPreference = vi.fn(() => save.promise);
    render(<ReviewPlanning {...props} />);
    const toggle = await screen.findByRole('switch', { name: 'Répétition espacée pour Go' });
    toggle.focus(); await userEvent.keyboard(' ');
    await waitFor(() => expect(props.onPreference).toHaveBeenCalledTimes(1));
    const row = screen.getByRole('article', { name: 'Go' });
    expect(within(row).getByRole('button', { name: 'Réviser Go' })).toHaveProperty('disabled', true);
    expect(toggle).toHaveProperty('disabled', true);
    await act(async () => save.resolve());
    expect(toggle).toHaveProperty('disabled', false);
  });
});
