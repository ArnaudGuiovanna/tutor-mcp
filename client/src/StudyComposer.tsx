import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { Icon } from './icons';
import { t } from './i18n';
import { shortcutModifier } from './platform';
import './studyComposer.css';
import type { Training } from './types';
import { TrainingColor } from './WorkspaceNavigation';
import { attachmentAccept, readTextAttachments, withAttachments } from './conversationFiles';
import type { TextAttachment } from './conversationFiles';

const modes = [
  {command: '/quiz', label: 'Quiz', description: 'Choisir une réponse et comprendre la correction.'},
  {command: '/flashcards', label: 'Flashcards', description: 'Retrouver une réponse, retourner la carte, s’autoévaluer.'},
];

export interface ExerciseComposer { target:'answer'|'question'; onTarget:(target:'answer'|'question')=>void; onHint:()=>void; hintDisabled?:boolean }

export function StudyComposer({draft,onChange,disabled,canSend,sendBlockedReason='',pending,onSend,onCancel,home=false,trainingName,trainings=[],onChooseTraining,onOpenLibrary,menuRequest=0,mode,attachments=[],onAttachments,exercise}: {
  draft:string; onChange:(value:string)=>void; disabled:boolean; canSend:boolean; pending:boolean; onSend:()=>void; onCancel:()=>void;
  /** Why sending is temporarily refused (provider limit). The message stays editable. */
  sendBlockedReason?:string;
  mode?:'quiz'|'flashcard'|'exercise'; home?:boolean; trainingName?:string; trainings?:Training[]; onChooseTraining?:(item:Training)=>void; onOpenLibrary?:()=>void; menuRequest?:number;
  attachments?:TextAttachment[]; onAttachments?:(files:TextAttachment[])=>void;
  /** Present while an exercise waits for an answer: the composer is its only input. */
  exercise?:ExerciseComposer;
}) {
  const answering = exercise?.target === 'answer';
  const modifier = shortcutModifier();
  const sendable = canSend && !sendBlockedReason;
  const targetName = useId();
  const input = useRef<HTMLTextAreaElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const [fileError,setFileError] = useState('');
  const [readingFiles,setReadingFiles] = useState(false);
  const readEpoch = useRef(0);
  useEffect(()=>()=>{readEpoch.current++;},[]);
  async function attach(files:File[]) {
    const epoch=++readEpoch.current;setReadingFiles(true);setFileError('');
    try {
      const next=[...attachments,...await readTextAttachments(files)];
      withAttachments(draft,next);
      if(epoch===readEpoch.current)onAttachments?.(next);
    } catch(reason) {if(epoch===readEpoch.current)setFileError(reason instanceof Error?reason.message:String(reason));}
    finally {if(epoch===readEpoch.current)setReadingFiles(false);}
  }
  const choices = useRef<(HTMLButtonElement | null)[]>([]);
  const [open, setOpen] = useState(false);
  const [trainingOpen,setTrainingOpen] = useState(false);
  const trainingButton = useRef<HTMLButtonElement>(null);
  const trainingChoices = useRef<HTMLDivElement>(null);
  useEffect(()=>{if(menuRequest){setOpen(true);setTrainingOpen(false);input.current?.focus();}},[menuRequest]);
  useEffect(()=>{if(trainingOpen)trainingChoices.current?.querySelector('button')?.focus();},[trainingOpen]);
  const query = /^\/[^\s]*$/.test(draft) ? draft.toLowerCase() : '';
  const visibleModes = modes.filter(mode => !query || mode.command.startsWith(query) || (query === '/quizz' && mode.command === '/quiz'));
  useLayoutEffect(() => {
    const element = input.current;
    if (!element) return;
    const resize = () => { element.style.height = 'auto'; element.style.height = `${element.scrollHeight}px`; };
    resize(); window.addEventListener('resize', resize); return () => window.removeEventListener('resize', resize);
  }, [draft,home]);
  function choose(command: string) {
    const topic = draft.replace(/^\/\S*\s*/, '').trim();
    onChange(`${command} ${topic}`); setOpen(false); input.current?.focus();
  }
  return <form className="composer" onSubmit={event => { event.preventDefault(); if (sendable && !readingFiles && draft.trim() !== '/') {setOpen(false); onSend();} }}>
    {trainingOpen && <div ref={trainingChoices} className="training-picker" role="group" aria-label={t('Choisir une formation')} onBlur={event=>{if(!event.currentTarget.contains(event.relatedTarget)&&event.relatedTarget!==trainingButton.current)setTrainingOpen(false);}} onKeyDown={event=>{if(event.key==='Escape'){event.preventDefault();setTrainingOpen(false);trainingButton.current?.focus();}}}>
      {trainings.map(item=><button type="button" key={item.id} onClick={()=>{setTrainingOpen(false);onChooseTraining?.(item);input.current?.focus();}}><TrainingColor name={item.name}/><span>{item.name}</span></button>)}
      {!trainings.length && <p>{t('Retrouvez vos formations dans la bibliothèque.')}</p>}
      <button type="button" onClick={()=>{setTrainingOpen(false);onOpenLibrary?.();}}><Icon name="book" size={14}/>{t('Ouvrir la bibliothèque')}</button>
    </div>}
    <div className="composer-context">
      <button ref={trainingButton} type="button" className={trainingName?'has-training':''} disabled={disabled} aria-expanded={trainingOpen} onClick={()=>{setTrainingOpen(value=>!value);setOpen(false);}}><Icon name="book" size={14}/><span>{trainingName || t('Choisir une formation')}</span><Icon name="chevron" size={10}/></button>
      <button type="button" disabled={disabled} onClick={()=>{setOpen(value=>!value);setTrainingOpen(false);}} aria-haspopup="menu" aria-expanded={open}><Icon name="clock" size={14}/><span>{t((draft.startsWith('/quiz')||mode==='quiz')?'Mode quiz':(draft.startsWith('/flashcard')||mode==='flashcard')?'Mode flashcards':mode==='exercise'?'Mode exercice':'À votre rythme')}</span></button>
    </div>
    {exercise && <div className="composer-exercise" role="group" aria-label={t('Exercice en cours')}>
      <fieldset className="composer-target" disabled={disabled}>
        <legend className="visually-hidden">{t('Destination du message')}</legend>
        {([['answer','Réponse à l’exercice'],['question','Question au tuteur']] as const).map(([value,label])=><label key={value} className={exercise.target===value?'selected':''}>
          <input type="radio" name={targetName} value={value} checked={exercise.target===value} onChange={()=>{exercise.onTarget(value);input.current?.focus();}}/>
          <span>{t(label)}</span>
        </label>)}
      </fieldset>
      <button type="button" className="workspace-outline composer-hint" disabled={disabled || exercise.hintDisabled} onClick={exercise.onHint}>{t('Un indice')}</button>
    </div>}
    <div className="composer-box">
    {!!attachments.length && <ul className="composer-attachments" aria-label={t('Fichiers joints')}>{attachments.map((file,index)=><li key={index}><Icon name="book" size={14}/><span>{file.name}</span><button className="workspace-icon" type="button" disabled={disabled || readingFiles} aria-label={t('Retirer {name}',{name:file.name})} onClick={()=>onAttachments?.(attachments.filter((_,i)=>i!==index))}><Icon name="close" size={14}/></button></li>)}</ul>}
    {fileError && <p className="attachment-error" role="alert">{t(fileError)}</p>}
    {readingFiles && <p role="status">{t('Lecture des fichiers…')}</p>}
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
    <label className="visually-hidden" htmlFor="prompt">{t('Message au tuteur')}</label>
    {exercise && <p id="composer-target-help" className="visually-hidden">{t(answering?'Ce message sera enregistré comme réponse à l’exercice.':'Ce message est une question au tuteur ; il ne sera pas noté.')}</p>}
    <textarea ref={input} autoFocus id="prompt" rows={home?2:1} value={draft} disabled={disabled} maxLength={answering?12000:64000} aria-describedby={exercise?'composer-target-help':undefined}
      aria-controls={open ? 'study-command-menu' : undefined} aria-expanded={open && !disabled && visibleModes.length > 0}
      placeholder={t(answering?'Votre réponse à l’exercice…':home?'Posez une question, ou / pour un quiz et des flashcards':'Répondre, demander un indice, ou / pour changer d’exercice')}
      onChange={event => {onChange(event.target.value); setOpen(/^\/[^\s]*$/.test(event.target.value));}}
      onKeyDown={event => {
        if (event.nativeEvent.isComposing) return;
        if (event.key === 'Escape') {setOpen(false); return;}
        if (open && event.key === 'ArrowDown' && visibleModes.length) { event.preventDefault(); choices.current[0]?.focus(); return; }
        if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); }
      }}/>
    <div className="composer-footer">
      <input ref={fileInput} type="file" className="visually-hidden" tabIndex={-1} aria-label={t('Joindre des fichiers texte ou de code')} accept={attachmentAccept} multiple disabled={disabled || readingFiles} onChange={event=>{void attach(Array.from(event.target.files || []));event.target.value='';}}/>
      <button className="workspace-icon" type="button" disabled={disabled || readingFiles || answering} aria-label={t('Joindre des fichiers texte ou de code')} title={t('Joindre des fichiers texte ou de code (32 Ko par fichier)')} onClick={()=>fileInput.current?.click()}><Icon name="plus" size={18}/></button>
      <button className="study-command-trigger" type="button" disabled={disabled} aria-label={t('Choisir Quiz ou Flashcards')} aria-haspopup="menu" aria-expanded={open && !disabled && visibleModes.length > 0} onClick={() => {setOpen(value => !value);setTrainingOpen(false);}}><span className="slash" aria-hidden="true">/</span><span>{t('Quiz & flashcards')}</span></button>
      {pending ? <button type="button" className="composer-send" aria-label={t('Interrompre')} title={t('Interrompre')} onClick={onCancel}><Icon name="stop" size={16}/></button>
        : <button className="composer-send" aria-label={t('Envoyer ({shortcut} Entrée)',{shortcut:modifier})} title={t('Envoyer ({shortcut} Entrée)',{shortcut:modifier})} aria-keyshortcuts="Control+Enter Meta+Enter" aria-describedby={sendBlockedReason ? 'send-blocked-reason' : undefined} disabled={!sendable || readingFiles || draft.trim() === '/'}><span className="send-arrow"><Icon name="arrow" size={18}/></span></button>}
      {sendBlockedReason && <span id="send-blocked-reason" className="visually-hidden">{sendBlockedReason}</span>}
    </div>
    </div>
  </form>;
}
