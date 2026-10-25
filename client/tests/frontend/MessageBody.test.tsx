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
