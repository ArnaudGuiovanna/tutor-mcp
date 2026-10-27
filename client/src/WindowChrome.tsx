import { useEffect, useState } from 'react';
import { isTauri } from '@tauri-apps/api/core';
import { getCurrentWindow } from '@tauri-apps/api/window';
import { t, useTranslation } from './i18n';

export interface WindowControls {
  minimize(): Promise<void>;
  toggleMaximize(): Promise<void>;
  close(): Promise<void>;
  isMaximized(): Promise<boolean>;
  isFocused(): Promise<boolean>;
  onResized(callback: () => void): Promise<() => void>;
  onFocusChanged(callback: (event: { payload: boolean }) => void): Promise<() => void>;
}

const nativeWindow = isTauri() ? getCurrentWindow() : undefined;

/** Only the title's empty surface is draggable; controls keep ordinary pointer/keyboard input. */
export function WindowChrome({ controls = nativeWindow }: { controls?: WindowControls }) {
  useTranslation();
  const [maximized, setMaximized] = useState(false);
  const [focused, setFocused] = useState(true);
  const [error, setError] = useState(false);
  useEffect(() => {
    if (!controls) return;
    let live = true;
    const disposers: (() => void)[] = [];
    const register = (listener: Promise<() => void>) => void listener.then(dispose => live ? disposers.push(dispose) : dispose()).catch(() => { if (live) setError(true); });
    const refresh = () => void controls.isMaximized().then(value => { if (live) setMaximized(value); }).catch(() => { if (live) setError(true); });
    refresh();
    void controls.isFocused().then(value => { if (live) setFocused(value); }).catch(() => {});
    register(controls.onResized(refresh));
    register(controls.onFocusChanged(event => { if (live) setFocused(event.payload); }));
    return () => { live = false; disposers.forEach(dispose => dispose()); };
  }, [controls]);

  async function act(action: 'minimize' | 'toggleMaximize' | 'close') {
    if (!controls) return;
    setError(false);
    try { await controls[action](); if (action === 'toggleMaximize') setMaximized(await controls.isMaximized()); }
    catch { setError(true); }
  }

  return <header className={`window-chrome ${focused ? '' : 'window-inactive'}`} aria-label={t('Fenêtre Tutor')}>
    <div className="window-drag" data-tauri-drag-region>
      <span className="wm window-brand" aria-hidden="true">Tutor-MCP<span className="cursor still" /></span>
      <span className="window-caption">{t('Votre espace d’apprentissage')}</span>
    </div>
    {error && <span className="window-error" role="alert">{t('Impossible de modifier la fenêtre. Réessayez.')}</span>}
    <div className="window-controls" role="group" aria-label={t('Commandes de la fenêtre')}>
      <button type="button" className="window-control" title={t('Réduire la fenêtre')} aria-label={t('Réduire la fenêtre')} disabled={!controls} onClick={() => void act('minimize')}><svg viewBox="0 0 12 12" aria-hidden="true"><path d="M1 6.5h10" /></svg></button>
      <button type="button" className="window-control" title={t(maximized ? 'Restaurer la fenêtre' : 'Agrandir la fenêtre')} aria-label={t(maximized ? 'Restaurer la fenêtre' : 'Agrandir la fenêtre')} disabled={!controls} onClick={() => void act('toggleMaximize')}><svg viewBox="0 0 12 12" aria-hidden="true">{maximized ? <path d="M3.5 3.5v-2h7v7h-2m-7-5h7v7h-7z" /> : <rect x="1.5" y="1.5" width="9" height="9" />}</svg></button>
      <button type="button" className="window-control window-close" title={t('Fermer la fenêtre')} aria-label={t('Fermer la fenêtre')} disabled={!controls} onClick={() => void act('close')}><svg viewBox="0 0 12 12" aria-hidden="true"><path d="m1.5 1.5 9 9m0-9-9 9" /></svg></button>
    </div>
  </header>;
}
