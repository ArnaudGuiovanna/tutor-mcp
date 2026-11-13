import { useCallback, useEffect, useRef, useState } from 'react';
import type { ClientBridge } from './bridge';
import { Icon } from './icons';
import { t } from './i18n';
import { LoadError, LoadingState } from './LoadState';
import type { Training, TrainingDocument, TrainingPage } from './types';

export interface WorkspaceTrainings { items:Training[]; error:boolean; loading:boolean; retry:()=>void }

export function useWorkspaceTrainings(api:ClientBridge, connected:boolean, scope:string, epoch:number):WorkspaceTrainings {
  const [result,setResult] = useState<{scope:string;attempt:number;items:Training[];error:boolean}>();
  const [attempt,setAttempt] = useState(0);
  const retry = useCallback(() => setAttempt(value => value + 1), []);
  useEffect(() => {
    if (!connected) return;
    let live = true;
    void api.trainingAction('list_training_library', {limit:100}).then(value => {
      const page = value as TrainingPage;
      if (live) setResult({scope,attempt,items:(page?.items || []).filter(item => item.domain_id && item.status !== 'archived'),error:false});
    }).catch(() => {if (live) setResult({scope,attempt,items:[],error:true});});
    return () => {live = false;};
  }, [api,connected,scope,epoch,attempt]);
  if (!connected) return {items:[],error:false,loading:false,retry};
  // A retry keeps the previous list visible; only a first read or a failed one shows the reading state.
  if (result?.scope !== scope) return {items:[],error:false,loading:true,retry};
  if (result.error && result.attempt !== attempt) return {items:[],error:false,loading:true,retry};
  return {items:result.items,error:result.error,loading:false,retry};
}

export function ResumeTraining({training,disabled,onResume,onDismiss}: {training:Training;disabled:boolean;onResume:()=>void;onDismiss:()=>void}) {
  // The API provides estimates per concept, not a course completion percentage.
  // Do not turn a mastery estimate into an invented chapter/completion count.
  return <section className="resume-card" aria-label={t('Reprendre votre formation')}>
    <span className="resume-icon"><Icon name="clock" size={16}/></span><div className="resume-copy"><strong>{t('Reprendre {name}',{name:training.name})}</strong><p>{t('Poursuivre votre dernière activité')}</p></div>
    <button className="workspace-primary" disabled={disabled} onClick={onResume}>{t('Reprendre')}</button>
    <button className="workspace-icon" aria-label={t('Masquer la reprise pour cette session')} title={t('Masquer')} onClick={onDismiss}><Icon name="close" size={16}/></button>
  </section>;
}

export function WorkspaceTrainingPanel({api,id,scope,onClose,onOpenLibrary}: {api:ClientBridge;id?:string;scope:string;onClose:()=>void;onOpenLibrary:()=>void}) {
  const [doc,setDoc] = useState<TrainingDocument>(); const [error,setError] = useState(false); const [attempt,setAttempt] = useState(0); const close = useRef<HTMLButtonElement>(null);
  useEffect(() => {const previous = document.activeElement as HTMLElement; close.current?.focus(); return () => {if(previous?.isConnected)previous.focus();};},[]);
  useEffect(() => {
    setDoc(undefined); setError(false); if (!id) return; let live = true;
    void api.trainingAction('get_training',{id}).then(value => {if(live)setDoc(value as TrainingDocument);}).catch(() => {if(live)setError(true);});
    return () => {live = false;};
  },[api,id,scope,attempt]);
  return <aside className="workspace-training-panel" aria-label={t('Panneau de la formation')} onKeyDown={event => {if(event.key === 'Escape'){event.stopPropagation();onClose();}}}>
    <header><h2>{t('Votre formation')}</h2><button ref={close} className="workspace-icon" aria-label={t('Fermer le panneau de la formation')} onClick={onClose}><Icon name="close" size={18}/></button></header>
    {!id ? <p>{t('Choisissez une formation pour retrouver son programme.')}</p> : error ? <LoadError message="Le programme est momentanément indisponible." onRetry={() => setAttempt(value => value + 1)}/> : !doc ? <LoadingState label="Lecture du programme…"/> : <>
      <h3>{doc.training.name}</h3><p>{doc.training.description}</p>
      {!!doc.modules?.length && <ol>{[...doc.modules].sort((a,b)=>a.position-b.position).map(module => <li key={module.stable_key}>{module.title}</li>)}</ol>}
      {!!doc.curriculum?.concepts?.length && <ul>{doc.curriculum.concepts.map(concept => <li key={concept.key}>{concept.label}</li>)}</ul>}
    </>}
    <button className="workspace-outline" onClick={onOpenLibrary}>{t('Voir la formation')}</button>
  </aside>;
}
