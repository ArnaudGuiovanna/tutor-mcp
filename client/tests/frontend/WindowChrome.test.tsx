import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, it, vi } from 'vitest';
import { WindowChrome } from '../../src/WindowChrome';
import type { WindowControls } from '../../src/WindowChrome';

it('commande la vraie fenêtre via son contrôleur et suit aussi les changements natifs', async () => {
  let maximized = false;
  let resize = () => {};
  let focus = (_: {payload:boolean}) => {};
  const dispose = vi.fn();
  const controls: WindowControls = {
    minimize: vi.fn(async () => {}), close: vi.fn(async () => {}),
    toggleMaximize: vi.fn(async () => { maximized = !maximized; }),
    isMaximized: vi.fn(async () => maximized), isFocused: vi.fn(async () => true),
    onResized: vi.fn(async callback => { resize = callback; return dispose; }),
    onFocusChanged: vi.fn(async callback => { focus = callback; return dispose; }),
  };
  const view = render(<WindowChrome controls={controls}/>);
  const user = userEvent.setup();
  await user.click(screen.getByRole('button',{name:'Réduire la fenêtre'}));
  expect(controls.minimize).toHaveBeenCalledTimes(1);
  await user.click(screen.getByRole('button',{name:'Agrandir la fenêtre'}));
  expect(await screen.findByRole('button',{name:'Restaurer la fenêtre'})).toBeTruthy();
  maximized = false;
  act(()=>resize());
  await screen.findByRole('button',{name:'Agrandir la fenêtre'});
  act(()=>focus({payload:false}));
  expect(view.container.querySelector('.window-inactive')).toBeTruthy();
  await user.click(screen.getByRole('button',{name:'Fermer la fenêtre'}));
  expect(controls.close).toHaveBeenCalledTimes(1);
  expect(view.container.querySelector('.window-controls')?.closest('[data-tauri-drag-region]')).toBeNull();
  view.unmount();
  await waitFor(()=>expect(dispose).toHaveBeenCalledTimes(2));
});

it('signale un refus de commande native et permet de réessayer', async () => {
  const controls: WindowControls = {
    minimize: vi.fn().mockRejectedValueOnce(new Error('denied')).mockResolvedValue(undefined),
    close: vi.fn(), toggleMaximize: vi.fn(), isMaximized: vi.fn(async()=>false), isFocused: vi.fn(async()=>true),
    onResized: vi.fn(async()=>()=>{}), onFocusChanged: vi.fn(async()=>()=>{}),
  };
  render(<WindowChrome controls={controls}/>);
  await userEvent.click(screen.getByRole('button',{name:'Réduire la fenêtre'}));
  expect(await screen.findByRole('alert')).toHaveProperty('textContent','Impossible de modifier la fenêtre. Réessayez.');
  await userEvent.click(screen.getByRole('button',{name:'Réduire la fenêtre'}));
  await waitFor(()=>expect(screen.queryByRole('alert')).toBeNull());
});
