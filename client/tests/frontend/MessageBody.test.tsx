import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, it, vi } from 'vitest';
import { MessageBody } from '../../src/MessageBody';

it('structure le texte et les tableaux sans exécuter HTML ni charger les images distantes',async()=>{
 const open=vi.fn(async()=>{});
 const {container}=render(<MessageBody openLink={open} text={'## Un point important\n\n**À retenir**\n\n- Premier\n- Second\n\n| Technique | Fruit |\n|---|---|\n| Miel | Coing |\n\n[Source](https://example.org/source)\n\n![Illustration](https://tracker.example/image)\n\n<script>alert(1)</script>'}/>);
 expect(screen.getByRole('heading',{name:'Un point important',level:3})).toBeTruthy();expect(container.querySelector('strong')?.textContent).toBe('À retenir');expect(screen.getAllByRole('listitem')).toHaveLength(2);
 expect(screen.getByRole('region',{name:'Tableau de la réponse'}).tabIndex).toBe(0);expect(container.querySelector('img,script')).toBeNull();
 await userEvent.click(screen.getByRole('link',{name:'Source'}));expect(open).toHaveBeenCalledWith('https://example.org/source');
});

it('copie le code exact malgré son retour visuel à la ligne et présente les tableaux larges verticalement',async()=>{
 const user=userEvent.setup();const copy=vi.spyOn(navigator.clipboard,'writeText').mockResolvedValue();
 const code='const long = "'+ 'x'.repeat(600)+'";\n\n  // espaces conservés\n';
 const {container}=render(<MessageBody text={'```go\n'+code+'```\n\n| A | B | C | D | E |\n|---|---|---|---|---|\n| un | deux | trois | quatre | cinq |'}/>);
 await user.click(screen.getByRole('button',{name:'Copier le code'}));expect(copy).toHaveBeenCalledWith(code);expect(container.querySelector('.stacked-table-row')).toBeTruthy();expect(screen.getByText('cinq')).toBeTruthy();expect(container.querySelector('.message-table table')).toBeNull();
});
