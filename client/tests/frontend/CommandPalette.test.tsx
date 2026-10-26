import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import App from '../../src/App';
import { client, conversation, initialState } from './clientFixture';

async function ready(api: ReturnType<typeof client>['api']) {
  render(<App api={api} />);
  await screen.findByRole('heading', { name: 'Qu’avez-vous envie d’apprendre ?' });
}

describe('Palette de commandes', () => {
  it('s’ouvre avec Ctrl K, filtre sans accents et navigue vers les réglages', async () => {
    const {api}=client();await ready(api);
    expect(screen.queryByRole('dialog',{name:'Palette de commandes'})).toBeNull();
    fireEvent.keyDown(window,{key:'k',ctrlKey:true});
    await screen.findByRole('dialog',{name:'Palette de commandes'});
    await waitFor(()=>expect(document.activeElement).toBe(screen.getByRole('combobox',{name:'Rechercher une commande'})));
    await userEvent.keyboard('ergonomie');
    const option=screen.getByRole('option',{name:/Ergonomie/});expect(option.getAttribute('aria-selected')).toBe('true');
    await userEvent.keyboard('{Enter}');
    await waitFor(()=>expect(screen.queryByRole('dialog',{name:'Palette de commandes'})).toBeNull());
    expect(screen.getByRole('tab',{name:'Ergonomie'}).getAttribute('aria-selected')).toBe('true');
    expect(screen.getByRole('heading',{level:1,name:'Paramètres'})).toBeTruthy();
  });

  it('ouvre une conversation existante au clavier et se ferme avec Échap', async () => {
    const state=initialState(true);state.conversations=[conversation];const {api}=client(state);render(<App api={api}/>);
    await screen.findByText('Commençons par observer la Grande Ourse.');
    await userEvent.click(screen.getByRole('button',{name:'Palette de commandes'}));
    await screen.findByRole('dialog',{name:'Palette de commandes'});
    await userEvent.keyboard('{Escape}');
    await waitFor(()=>expect(screen.queryByRole('dialog',{name:'Palette de commandes'})).toBeNull());
    fireEvent.keyDown(window,{key:'K',metaKey:true});
    await screen.findByRole('dialog',{name:'Palette de commandes'});
    await userEvent.keyboard('constellations');
    // A real browser never clicks the refocused sidebar button on a prevented Enter; user-event would.
    fireEvent.keyDown(screen.getByRole('combobox',{name:'Rechercher une commande'}),{key:'Enter'});
    await waitFor(()=>expect(api.selectConversation).toHaveBeenCalledWith(conversation.id));
    expect(screen.queryByRole('dialog',{name:'Palette de commandes'})).toBeNull();
  });

  it('change le thème depuis la palette et conserve les autres réglages', async () => {
    const fixture=client();await ready(fixture.api);
    fireEvent.keyDown(window,{key:'k',ctrlKey:true});await screen.findByRole('dialog',{name:'Palette de commandes'});
    await userEvent.keyboard('clair{Enter}');
    await waitFor(()=>expect(document.documentElement.dataset.theme).toBe('light'));
    expect(fixture.getState().settings.appearance).toEqual({theme:'light',fontSize:20,lineHeight:1.65,reduceMotion:false});
    expect(screen.getByRole('button',{name:'Palette de commandes'})).toBeTruthy();
  });
});
