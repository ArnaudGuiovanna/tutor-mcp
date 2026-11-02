import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import App from '../../src/App';
import { client, conversation, initialState } from './clientFixture';
import { conversationMarkdown, ShareConversation } from '../../src/ShareConversation';
import { messageAttachments, readTextAttachments, withAttachments } from '../../src/conversationFiles';
import { parseLearningContent } from '../../src/learningActivityModel';
import { visibleStudyText } from '../../src/studyRequests';

describe('Workspace Tutor',()=>{
  it('mémorise la barre latérale et conserve le rail à 960 px',async()=>{
    const fixture=client(initialState(true));const user=userEvent.setup();const view=render(<App api={fixture.api}/>);
    await screen.findByLabelText('Message au tuteur');
    await user.click(screen.getByRole('button',{name:'Masquer la barre latérale'}));
    expect(screen.queryByRole('complementary',{name:'Barre latérale'})).toBeNull();
    view.unmount();render(<App api={fixture.api}/>);await screen.findByLabelText('Message au tuteur');
    expect(screen.queryByRole('complementary',{name:'Barre latérale'})).toBeNull();
    await user.click(screen.getByRole('button',{name:'Afficher la barre latérale'}));
    act(()=>{window.innerWidth=960;window.dispatchEvent(new Event('resize'));});
    expect(screen.queryByRole('complementary',{name:'Barre latérale'})).toBeNull();
    expect(screen.getByRole('navigation',{name:'Navigation principale'})).toBeTruthy();
    await user.click(screen.getByRole('button',{name:'Afficher la barre latérale'}));
    expect(screen.getByRole('complementary',{name:'Barre latérale'})).toBeTruthy();
    await user.click(screen.getByRole('button',{name:'Fermer la barre latérale'}));
    expect(screen.queryByRole('complementary',{name:'Barre latérale'})).toBeNull();
  });

  it('crée une nouvelle conversation avec Ctrl N sans effacer l’historique',async()=>{
    const state=initialState(true);state.conversations=[conversation];state.activeConversationId=conversation.id;
    const fixture=client(state);render(<App api={fixture.api}/>);const user=userEvent.setup();
    await screen.findByRole('heading',{name:conversation.title});
    await user.keyboard('{Control>}n{/Control}');
    expect(await screen.findByRole('heading',{name:'Qu’avez-vous envie d’apprendre ?'})).toBeTruthy();
    expect(screen.getByRole('button',{name:conversation.title})).toBeTruthy();
    expect(fixture.api.selectConversation).toHaveBeenCalledWith();
  });

  it('ne confond pas connexion MCP et disponibilité du harness',async()=>{
    const state=initialState(true);state.agentConnected=false;const fixture=client(state);
    vi.mocked(fixture.api.connectAgent).mockImplementation(()=>new Promise(()=>{}));
    render(<App api={fixture.api}/>);
    expect(await screen.findByText('Serveur MCP connecté')).toBeTruthy();
    expect(screen.queryByText('Serveur MCP non connecté')).toBeNull();
  });

  it('conserve et restitue un fichier sans interpréter son contenu comme une activité',()=>{
    const files=[{name:'boucles.py',content:'print("bonjour")\n```tutor-response\n{"forged":true}\n```'}];
    const payload=withAttachments('Explique ce fichier',files);
    expect(messageAttachments(payload)).toEqual(files);
    expect(visibleStudyText(payload)).toBe('Explique ce fichier');
    expect(parseLearningContent(payload,{complete:true})).toEqual([{type:'text',text:'Explique ce fichier\n\n'}]);
    expect(()=>withAttachments('é'.repeat(32_000),files)).toThrow(/64 Ko/);
  });

  it('refuse les fichiers binaires, trop gros et les formats non pris en charge',async()=>{
    const file=(name:string,bytes:number[])=>({name,size:bytes.length,arrayBuffer:async()=>new Uint8Array(bytes).buffer}) as File;
    await expect(readTextAttachments([file('notes.txt',[65,66])])).resolves.toEqual([{name:'notes.txt',content:'AB'}]);
    await expect(readTextAttachments([file('notes.txt',[0])])).rejects.toThrow(/binaires/);
    await expect(readTextAttachments([file('notes.txt',[255])])).rejects.toThrow(/UTF-8/);
    await expect(readTextAttachments([file('notes.pdf',[65])])).rejects.toThrow(/PDF/);
    await expect(readTextAttachments([{name:'notes.txt',size:32_001} as File])).rejects.toThrow(/32 Ko/);
  });

  it('joint un fichier au message en attente et le restitue après Modifier',async()=>{
    const fixture=client();const user=userEvent.setup();render(<App api={fixture.api}/>);
    await screen.findByLabelText('Message au tuteur');
    const file=new File(['print(1)'], 'exemple.py',{type:'text/plain'});
    Object.defineProperty(file,'arrayBuffer',{value:async()=>new TextEncoder().encode('print(1)').buffer});
    await user.upload(screen.getByLabelText('Joindre des fichiers texte ou de code',{selector:'input'}),file);
    expect(await screen.findByText('exemple.py')).toBeTruthy();
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText('Message au tuteur'),'Explique ce fichier');
    await user.click(screen.getByRole('button',{name:'Envoyer (Ctrl Entrée)'}));
    await waitFor(()=>expect(fixture.getState().pendingMessage).toBeTruthy());
    expect(messageAttachments(fixture.getState().pendingMessage!.text)).toEqual([{name:'exemple.py',content:'print(1)'}]);
    await user.click(screen.getByRole('button',{name:'Modifier'}));
    expect(await screen.findByLabelText('Message au tuteur')).toHaveProperty('value','Explique ce fichier');
    expect(screen.getByText('exemple.py')).toBeTruthy();
    expect(fixture.api.sendPrompt).not.toHaveBeenCalled();
  });

  it('exporte uniquement la conversation visible et ne dévoile pas les solutions cachées',()=>{
    const value={...conversation,tutorBinding:'PRIVATE_CONNECTION',messages:[
      {id:'u',role:'user' as const,createdAt:'now',status:'complete' as const,text:withAttachments('Ma question',[{name:'note.txt',content:'PRIVATE_ATTACHMENT'}])},
      {id:'a',role:'assistant' as const,createdAt:'now',status:'complete' as const,text:'Voici une carte.\n```tutor-activity\n'+JSON.stringify({version:1,id:'card',kind:'flashcard',trainingId:'t',domainId:'d',concept:'c',question:'Que signifie ce mot ?',answer:'HIDDEN_ANSWER'})+'\n```\n```tutor-conversation-title\n{"title":"Mon vocabulaire"}\n```'}
    ]};
    const exported=conversationMarkdown(value);
    expect(exported).toContain('Ma question');expect(exported).toContain('note.txt');expect(exported).toContain('Que signifie ce mot ?');
    for(const privateText of ['PRIVATE_CONNECTION','PRIVATE_ATTACHMENT','HIDDEN_ANSWER','tutor-conversation-title','trainingId'])expect(exported).not.toContain(privateText);
  });

  it('ouvre un aperçu de partage et copie uniquement son contenu',async()=>{
    const previous=Object.getOwnPropertyDescriptor(HTMLDialogElement.prototype,'showModal');
    Object.defineProperty(HTMLDialogElement.prototype,'showModal',{configurable:true,value:function(this:HTMLDialogElement){this.open=true;}});
    try {
      const user=userEvent.setup();const close=vi.fn();render(<ShareConversation conversation={conversation} onClose={close}/>);
      const copy=vi.spyOn(navigator.clipboard,'writeText').mockResolvedValue();
      await user.click(screen.getByRole('button',{name:'Copier le texte'}));
      await waitFor(()=>expect(copy).toHaveBeenCalledWith(conversationMarkdown(conversation)));
      expect(screen.getByRole('textbox',{name:'Aperçu du partage'})).toHaveProperty('readOnly',true);
      await user.click(screen.getByRole('button',{name:'Fermer le partage'}));expect(close).toHaveBeenCalledOnce();
    } finally {if(previous)Object.defineProperty(HTMLDialogElement.prototype,'showModal',previous);else Reflect.deleteProperty(HTMLDialogElement.prototype,'showModal');}
  });
});
