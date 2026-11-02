import { useEffect, useRef, useState } from 'react';
import { invoke, isTauri } from '@tauri-apps/api/core';
import type { Conversation } from './types';
import { parseLearningContent } from './learningActivityModel';
import { messageAttachments } from './conversationFiles';
import { visibleStudyText } from './studyRequests';
import { Icon } from './icons';
import { t } from './i18n';

/** Export only the chosen conversation's visible content, never connection metadata or credentials. */
export function conversationMarkdown(conversation:Conversation):string {
  return `# ${conversation.title || t('Conversation')}\n\n`+conversation.messages.filter(message=>message.role==='user'||message.role==='assistant').map(message=>{
    const content=message.role==='user'?visibleStudyText(message.text):parseLearningContent(message.text,{complete:message.status==='complete'}).map(segment=>{
      if(segment.type==='text')return segment.text;
      if(segment.type!=='activity')return '';
      const activity=segment.activity;
      // Export the question as seen in the thread, without revealing unsubmitted answers.
      return `${activity.kind==='quiz'?'Quiz':'Flashcard'} — ${activity.question}${activity.kind==='quiz'?'\n'+activity.options.map(option=>`- ${option.text}`).join('\n'):''}`;
    }).join('\n');
    const files=messageAttachments(message.text);
    return `## ${message.role==='user'?t('Vous'):t('Tutor')}\n\n${content.trim()}${files.length?'\n\n'+t('Fichiers joints (noms uniquement) : {names}',{names:files.map(file=>file.name).join(', ')}):''}${message.status!=='complete'?'\n\n'+t('Réponse incomplète'):''}`;
  }).join('\n\n')+'\n';
}

export async function saveConversationExport(content:string) {
  if(isTauri()) {await invoke('export_conversation',{content});return;}
  const url=URL.createObjectURL(new Blob([content],{type:'text/markdown;charset=utf-8'}));
  const link=document.createElement('a');link.href=url;link.download='conversation-tutor.md';document.body.append(link);link.click();link.remove();
  setTimeout(()=>URL.revokeObjectURL(url),1000);
}

export function ShareConversation({conversation,onClose}:{conversation:Conversation;onClose:()=>void}) {
  const dialog=useRef<HTMLDialogElement>(null);
  const [status,setStatus]=useState(''); const [error,setError]=useState('');
  const content=conversationMarkdown(conversation);
  useEffect(()=>{const previous=document.activeElement as HTMLElement;dialog.current?.showModal();return()=>{if(previous?.isConnected)previous.focus();};},[]);
  async function share(save:boolean) {
    setError('');setStatus('');
    try {if(save)await saveConversationExport(content);else {await navigator.clipboard.writeText(content);setStatus(t('Conversation copiée.'));}}
    catch {setError(t('Le partage a échoué. Réessayez ou sélectionnez le texte ci-dessous.'));}
  }
  return <dialog ref={dialog} className="share-dialog" aria-labelledby="share-title" onCancel={event=>{event.preventDefault();onClose();}} onClick={event=>{if(event.target===event.currentTarget)onClose();}}>
    <header><h2 id="share-title">{t('Partager la conversation')}</h2><button className="workspace-icon" aria-label={t('Fermer le partage')} onClick={onClose}><Icon name="close" size={18}/></button></header>
    <p>{t('Copiez le texte ou exportez un fichier Markdown à partager. Les pièces jointes sont indiquées par leur nom.')}</p>
    <label className="visually-hidden" htmlFor="share-content">{t('Aperçu du partage')}</label><textarea id="share-content" value={content} readOnly rows={12}/>
    {error&&<p role="alert">{error}</p>}{status&&<p role="status">{status}</p>}
    <footer><button className="workspace-outline" onClick={()=>void share(true)}>{t('Exporter en Markdown')}</button><button className="workspace-primary" onClick={()=>void share(false)}>{t('Copier le texte')}</button></footer>
  </dialog>;
}
