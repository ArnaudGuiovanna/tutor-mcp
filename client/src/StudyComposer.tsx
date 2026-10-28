import { useLayoutEffect, useRef, useState } from 'react';
import { Icon } from './icons';
import { t } from './i18n';
import './studyComposer.css';

const modifier = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘' : 'Ctrl';
const modes = [
  {command: '/quiz', label: 'Quiz', description: 'Choisir une réponse et comprendre la correction.'},
  {command: '/flashcards', label: 'Flashcards', description: 'Retrouver une réponse, retourner la carte, s’autoévaluer.'},
];

export function StudyComposer({draft,onChange,disabled,canSend,pending,onSend,onCancel}: {
  draft:string; onChange:(value:string)=>void; disabled:boolean; canSend:boolean; pending:boolean; onSend:()=>void; onCancel:()=>void;
}) {
  const input = useRef<HTMLTextAreaElement>(null);
  const choices = useRef<(HTMLButtonElement | null)[]>([]);
  const [open, setOpen] = useState(false);
  const query = /^\/[^\s]*$/.test(draft) ? draft.toLowerCase() : '';
  const visibleModes = modes.filter(mode => !query || mode.command.startsWith(query) || (query === '/quizz' && mode.command === '/quiz'));
  useLayoutEffect(() => {
    const element = input.current;
    if (!element) return;
    const resize = () => { element.style.height = 'auto'; element.style.height = `${element.scrollHeight}px`; };
    resize(); window.addEventListener('resize', resize); return () => window.removeEventListener('resize', resize);
  }, [draft]);
  function choose(command: string) {
    const topic = draft.replace(/^\/\S*\s*/, '').trim();
    onChange(`${command} ${topic}`); setOpen(false); input.current?.focus();
  }
  return <form className="composer" onSubmit={event => { event.preventDefault(); if (canSend && draft.trim() !== '/') {setOpen(false); onSend();} }}>
    {open && !disabled && visibleModes.length > 0 && <div className="study-command-menu" id="study-command-menu" role="menu" aria-label={t('Modalités d’apprentissage')} onKeyDown={event => {
      if (event.key === 'Escape') { event.preventDefault(); setOpen(false); input.current?.focus(); }
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault(); const current = choices.current.findIndex(item => item === document.activeElement);
        choices.current[(current + (event.key === 'ArrowDown' ? 1 : -1) + visibleModes.length) % visibleModes.length]?.focus();
      }
    }}>
      {visibleModes.map((mode, index) => <button key={mode.command} ref={element => {choices.current[index] = element;}} type="button" role="menuitem" onClick={() => choose(mode.command)}>
        <Icon name={mode.command === '/quiz' ? 'check' : 'book'} size={18}/><span><strong>{t(mode.label)}</strong><small>{t(mode.description)}</small></span><kbd>{mode.command}</kbd>
      </button>)}
    </div>}
    <label className="visually-hidden" htmlFor="prompt">{t('Votre message au tuteur')}</label>
    <textarea ref={input} autoFocus id="prompt" rows={1} value={draft} disabled={disabled} maxLength={64000}
      aria-controls={open ? 'study-command-menu' : undefined} aria-expanded={open && !disabled && visibleModes.length > 0}
      placeholder={t('Un message, ou / pour un quiz et des flashcards…')}
      onChange={event => {onChange(event.target.value); setOpen(/^\/[^\s]*$/.test(event.target.value));}}
      onKeyDown={event => {
        if (event.nativeEvent.isComposing) return;
        if (event.key === 'Escape') {setOpen(false); return;}
        if (open && event.key === 'ArrowDown' && visibleModes.length) { event.preventDefault(); choices.current[0]?.focus(); return; }
        if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); }
      }}/>
    <div className="composer-footer">
      <button className="study-command-trigger" type="button" disabled={disabled} aria-label={t('Choisir Quiz ou Flashcards')} aria-haspopup="menu" aria-expanded={open && !disabled && visibleModes.length > 0} onClick={() => setOpen(value => !value)}>/</button>
      <span className="composer-hint"><kbd>{modifier} ↵</kbd>{t('pour envoyer · L’IA peut se tromper.')}</span>
      {pending ? <button type="button" className="button secondary" onClick={onCancel}><Icon name="pause" size={16}/>{t('Interrompre')}</button>
        : <button className="button blue" disabled={!canSend || draft.trim() === '/'}>{t('Envoyer')}<Icon name="arrow" size={16}/></button>}
    </div>
  </form>;
}
