import { fireEvent, render, screen } from '@testing-library/react';
import type { ComponentProps } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { Connections } from '../../src/Connections';
import { setInterfaceLanguage } from '../../src/i18n';
import type { AgentTarget } from '../../src/types';
import { client, initialState, target } from './clientFixture';

const claude: AgentTarget = {id:'native:claude', label:'Claude — Windows', environment:'native', adapterPath:'C:\\Outils\\claude-agent-acp.cmd', available:true, detail:'Installé sous Windows'};
function props(): ComponentProps<typeof Connections> {
  const state = initialState();
  return {
    section:'agents', settings:state.settings, targets:[target, claude], connected:false,
    tutorStatus:state.tutorStatus, connection:null, identity:state.identity, api:client(state).api, busy:false,
    onIdentityAction:vi.fn(async () => true), onDiscover:vi.fn(), onSave:vi.fn(), onConnect:vi.fn(),
    onAuthenticate:vi.fn(), onDisconnectAgent:vi.fn(), onConnectTutor:vi.fn(), onDisconnectTutor:vi.fn(),
  };
}

describe('Nom de l’agent sélectionné', () => {
  it('connecte le harness choisi immédiatement et réutilise son identification sans choix de facturation', () => {
    const current = props();
    const view = render(<Connections {...current}/>);
    fireEvent.click(screen.getByRole('radio', {name:/Claude — Windows/}));
    expect(current.onConnect).toHaveBeenCalledTimes(1);
    view.rerender(<Connections {...current} connected agentStatus={{phase:'running', authentication:'authenticated'}}
      connection={{protocolVersion:1,agentInfo:{},agentCapabilities:{},authMethods:[{id:'api-key',name:'API key'},{id:'chat-gpt',name:'ChatGPT'}]}}/>);
    expect(screen.queryByText('Identification')).toBeNull();
    expect(screen.queryByRole('button', {name:/Ouvrir la connexion officielle/})).toBeNull();
    view.rerender(<Connections {...current} connected agentStatus={{phase:'running', authentication:'none'}}
      connection={{protocolVersion:1,agentInfo:{},agentCapabilities:{},authMethods:[{id:'api-key',name:'API key'},{id:'chat-gpt',name:'ChatGPT'}]}}/>);
    fireEvent.click(screen.getByRole('button', {name:/Ouvrir la connexion officielle/}));
    expect(current.onAuthenticate).toHaveBeenCalledWith('');
    expect(screen.queryByText('API key')).toBeNull();
  });
  it.each(['fr', 'en'] as const)('nomme Claude dans la connexion, la déconnexion et le résumé (%s)', language => {
    setInterfaceLanguage(language);
    const current = props();
    const view = render(<Connections {...current}/>);
    fireEvent.click(screen.getByRole('radio', {name:/Claude — Windows/}));
    const connect = language === 'fr' ? 'Connecter' : 'Connect';
    const disconnect = language === 'fr' ? 'Déconnecter' : 'Disconnect';
    fireEvent.click(screen.getByRole('button', {name:`${connect} Claude`}));
    expect(current.onConnect).toHaveBeenCalledWith(claude, expect.objectContaining({targetId:'native:claude'}));
    expect(screen.queryByRole('button', {name:`${connect} Codex`})).toBeNull();

    view.rerender(<Connections {...current} settings={{...current.settings, targetId:claude.id}} connected
      connection={{protocolVersion:1, agentInfo:{}, agentCapabilities:{}, authMethods:[]}}/>);
    expect(view.container.querySelector('.connected-summary')?.textContent).toMatch(/^Claude · /);
    expect(view.container.querySelector('.connection-actions .inline-status')?.textContent).toBe('Claude');
    fireEvent.click(screen.getByRole('button', {name:`${disconnect} Claude`}));
    expect(current.onDisconnectAgent).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('button', {name:`${disconnect} Codex`})).toBeNull();
  });

  it('revient au nom Codex pour une installation manuelle après sélection de Claude', () => {
    const current = props();
    render(<Connections {...current} settings={{...current.settings, targetId:claude.id}}/>);
    expect(screen.getByRole('button', {name:'Connecter Claude'})).toBeTruthy();
    fireEvent.click(screen.getByLabelText('Utiliser les chemins ci-dessous'));
    expect(screen.getByRole('button', {name:'Connecter Codex'})).toBeTruthy();
    expect(screen.queryByRole('button', {name:'Connecter Claude'})).toBeNull();
    expect(current.onConnect).not.toHaveBeenCalled();
  });
});

describe('Saisie de la connexion Tutor', () => {
  it('ne remplace pas une adresse en cours de saisie lors d’un nouvel état, puis se resynchronise après enregistrement', () => {
    const current = {...props(), section:'institution' as const};
    current.settings = {...current.settings, tutor:{...current.settings.tutor, mode:'remote', url:'https://ancienne.example'}};
    const view = render(<Connections {...current}/>);
    const address = screen.getByLabelText('Adresse de Tutor') as HTMLInputElement;
    fireEvent.change(address, {target:{value:'https://nouvelle.example'}});
    // A state broadcast changes the saved Tutor settings while the field is being edited.
    view.rerender(<Connections {...current} settings={{...current.settings, tutor:{...current.settings.tutor, url:'https://autre.example', command:'tutor-mcp'}}}/>);
    expect(address.value).toBe('https://nouvelle.example');
    fireEvent.click(screen.getByRole('button', {name:'Enregistrer la connexion Tutor'}));
    expect(current.onSave).toHaveBeenCalledWith(expect.objectContaining({tutor:expect.objectContaining({url:'https://nouvelle.example'})}));
    view.rerender(<Connections {...current} settings={{...current.settings, tutor:{...current.settings.tutor, url:'https://enregistree.example'}}}/>);
    expect(address.value).toBe('https://enregistree.example');
  });
});
