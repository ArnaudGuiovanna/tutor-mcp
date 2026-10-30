import { fireEvent, render, screen } from '@testing-library/react';
import type { ComponentProps } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { Connections } from '../../src/Connections';
import { setInterfaceLanguage } from '../../src/i18n';
import type { AgentTarget } from '../../src/types';
import { client, initialState, target } from './clientFixture';

const qwen: AgentTarget = {id:'local:qwen', label:'Qwen — local (expérimental)', environment:'native', available:true, detail:'Modèle vérifié'};
function props(): ComponentProps<typeof Connections> {
  const state = initialState();
  return {
    section:'agents', settings:state.settings, targets:[target, qwen], connected:false,
    tutorStatus:state.tutorStatus, connection:null, identity:state.identity, api:client(state).api, busy:false,
    onIdentityAction:vi.fn(async () => true), onDiscover:vi.fn(), onSave:vi.fn(), onConnect:vi.fn(),
    onAuthenticate:vi.fn(), onDisconnectAgent:vi.fn(), onConnectTutor:vi.fn(), onDisconnectTutor:vi.fn(),
  };
}

describe('Nom de l’agent sélectionné', () => {
  it.each(['fr', 'en'] as const)('nomme Qwen dans la connexion, la déconnexion et le résumé (%s)', language => {
    setInterfaceLanguage(language);
    const current = props();
    const view = render(<Connections {...current}/>);
    fireEvent.click(screen.getByRole('radio', {name:/Qwen — local/}));
    const connect = language === 'fr' ? 'Connecter' : 'Connect';
    const disconnect = language === 'fr' ? 'Déconnecter' : 'Disconnect';
    fireEvent.click(screen.getByRole('button', {name:`${connect} Qwen`}));
    expect(current.onConnect).toHaveBeenCalledWith(qwen, expect.objectContaining({targetId:'local:qwen'}));
    expect(screen.queryByRole('button', {name:`${connect} Codex`})).toBeNull();

    view.rerender(<Connections {...current} settings={{...current.settings, targetId:qwen.id}} connected
      connection={{protocolVersion:1, agentInfo:{}, agentCapabilities:{}, authMethods:[]}}/>);
    expect(view.container.querySelector('.connected-summary')?.textContent).toMatch(/^Qwen · /);
    expect(view.container.querySelector('.connection-actions .inline-status')?.textContent).toBe('Qwen');
    fireEvent.click(screen.getByRole('button', {name:`${disconnect} Qwen`}));
    expect(current.onDisconnectAgent).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('button', {name:`${disconnect} Codex`})).toBeNull();
  });

  it('revient au nom Codex pour une installation manuelle après sélection de Qwen', () => {
    const current = props();
    render(<Connections {...current} settings={{...current.settings, targetId:qwen.id}}/>);
    expect(screen.getByRole('button', {name:'Connecter Qwen'})).toBeTruthy();
    fireEvent.click(screen.getByLabelText('Utiliser les chemins ci-dessous'));
    expect(screen.getByRole('button', {name:'Connecter Codex'})).toBeTruthy();
    expect(screen.queryByRole('button', {name:'Connecter Qwen'})).toBeNull();
    expect(current.onConnect).not.toHaveBeenCalled();
  });
});
