import { t } from './i18n';
import { useEffect, useMemo, useRef, useState } from 'react';
import { Icon } from './icons';

export interface Command { id: string; group: string; label: string; hint?: string; icon?: 'chat' | 'book' | 'users' | 'gear' | 'shield' | 'plus' | 'sun' | 'plug' | 'clock'; disabled?: boolean; run: () => void }

// Accent-insensitive matching so « Paramètres » answers to "parametres".
const fold = (value: string) => value.normalize('NFD').replace(/[̀-ͯ]/g, '').toLowerCase();

export function CommandPalette({ open, commands, onClose }: { open: boolean; commands: Command[]; onClose: () => void }) {
  const [query, setQuery] = useState('');
  const [index, setIndex] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const matches = useMemo(() => { const needle = fold(query.trim()); const words = needle.split(/\s+/).filter(Boolean); return commands.filter(command => !words.length || words.every(word => fold(`${command.group} ${command.label}`).includes(word))); }, [commands, query]);
  const close = useRef(onClose); close.current = onClose;
  useEffect(() => {
    if (!open) return;
    setQuery(''); setIndex(0);
    const previous = document.activeElement as HTMLElement | null; input.current?.focus();
    // Escape closes the palette wherever focus sits, including before the input took it.
    const onKey = (event: KeyboardEvent) => { if (event.key === 'Escape') { event.preventDefault(); close.current(); } };
    window.addEventListener('keydown', onKey);
    return () => { window.removeEventListener('keydown', onKey); previous?.focus?.(); };
  }, [open]);
  useEffect(() => { setIndex(0); }, [query]);
  useEffect(() => { list.current?.querySelector<HTMLElement>('[aria-selected="true"]')?.scrollIntoView?.({ block: 'nearest' }); }, [index, matches]);
  if (!open) return null;
  const selected = matches[Math.min(index, matches.length - 1)];
  function choose(command: Command | undefined) { if (!command || command.disabled) return; onClose(); command.run(); }
  const groups = matches.reduce<[string, Command[]][]>((acc, command) => { const last = acc.at(-1); if (last && last[0] === command.group) last[1].push(command); else acc.push([command.group, [command]]); return acc; }, []);
  return <div className="palette-backdrop" onMouseDown={event => { if (event.target === event.currentTarget) onClose(); }}>
    <div className="palette" role="dialog" aria-modal="true" aria-label={t('Palette de commandes')} onKeyDown={event => {
      if (event.key === 'Escape') { event.preventDefault(); onClose(); }
      // The combobox owns keyboard focus; arrows select its listbox options.
      // Tab must stay inside the modal instead of reaching the background UI.
      else if (event.key === 'Tab') { event.preventDefault(); input.current?.focus(); }
      else if (event.key === 'ArrowDown') { event.preventDefault(); setIndex(value => matches.length ? (value + 1) % matches.length : 0); }
      else if (event.key === 'ArrowUp') { event.preventDefault(); setIndex(value => matches.length ? (value + matches.length - 1) % matches.length : 0); }
      else if (event.key === 'Enter') { event.preventDefault(); choose(selected); }
    }}>
      <div className="query"><Icon name="search" size={16} /><input ref={input} type="text" value={query} onChange={event => setQuery(event.target.value)} placeholder={t('Aller à, ouvrir, changer…')} aria-label={t('Rechercher une commande')} role="combobox" aria-expanded="true" aria-controls="palette-results" aria-activedescendant={selected ? `palette-${selected.id}` : undefined} autoComplete="off" spellCheck={false} /><span className="kbd">Esc</span></div>
      <ul className="results" id="palette-results" role="listbox" ref={list} aria-label={t('Commandes')}>
        {groups.map(([group, items]) => <li key={group} role="presentation"><div className="group">{group}</div>{items.map(command => <button type="button" key={command.id} id={`palette-${command.id}`} role="option" tabIndex={-1} aria-selected={command === selected} aria-disabled={command.disabled || undefined} className="item" disabled={command.disabled} onMouseEnter={() => setIndex(matches.indexOf(command))} onClick={() => choose(command)}>{command.icon && <Icon name={command.icon} size={15} />}<span>{command.label}</span>{command.hint && <small>{command.hint}</small>}</button>)}</li>)}
        {!matches.length && <li className="empty" role="presentation">{t('Aucune commande ne correspond.')}</li>}
      </ul>
    </div>
  </div>;
}
