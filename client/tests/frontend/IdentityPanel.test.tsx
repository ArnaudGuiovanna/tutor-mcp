import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, it, vi } from 'vitest';
import { IdentityPanel } from '../../src/IdentityPanel';
import { client } from './clientFixture';
const identity={exists:true,npub:'npub1identity',recoveryNpub:'npub1recovery',backupConfirmed:false,needsBackup:true};
it('confirme la sauvegarde uniquement après une écriture réussie et efface les phrases',async()=>{
 const {api}=client();vi.mocked(api.backupIdentity).mockRejectedValue(new Error('Disque indisponible'));
 render(<IdentityPanel identity={identity} api={api} busy={false} connected={false} institutionUrl="" run={async(_label,action)=>{try{await action();return true;}catch{return false;}}}/>);
 const user=userEvent.setup();await user.type(screen.getByLabelText(/Phrase pour chiffrer/),'Une longue phrase privée');await user.type(screen.getByLabelText('Confirmer la phrase de sauvegarde'),'Une longue phrase privée');await user.click(screen.getByRole('button',{name:'Enregistrer ma sauvegarde chiffrée'}));
 expect(api.backupIdentity).toHaveBeenCalledWith('Une longue phrase privée');expect(api.confirmIdentityBackup).not.toHaveBeenCalled();expect((screen.getByLabelText(/Phrase pour chiffrer/)as HTMLInputElement).value).toBe('');expect((screen.getByLabelText('Confirmer la phrase de sauvegarde')as HTMLInputElement).value).toBe('');
});
it('retrouve un export existant sans générer de nouvelles clés',async()=>{
 const {api}=client();render(<IdentityPanel identity={{...identity,backupExported:true}} api={api} busy={false} connected={false} institutionUrl="" run={async(_label,action)=>{await action();return true;}}/>);
 await userEvent.click(screen.getByRole('button',{name:'Terminer ma sauvegarde'}));expect(api.confirmIdentityBackup).toHaveBeenCalledTimes(1);expect(api.createIdentity).not.toHaveBeenCalled();expect(api.backupIdentity).not.toHaveBeenCalled();
});
it('restaure une sauvegarde localement sans transmettre la phrase à Tutor',async()=>{
 const {api}=client();render(<IdentityPanel identity={{exists:false,backupConfirmed:false,needsBackup:false}} api={api} busy={false} connected={false} institutionUrl="" run={async(_label,action)=>{await action();return true;}}/>);
 await userEvent.click(screen.getByText('Restaurer une sauvegarde Tutor'));await userEvent.type(screen.getByLabelText('Phrase de la sauvegarde existante'),'Une phrase secrète');await userEvent.click(screen.getByRole('button',{name:'Choisir et restaurer la sauvegarde'}));expect(api.restoreIdentity).toHaveBeenCalledWith('Une phrase secrète');expect(api.connectTutor).not.toHaveBeenCalled();expect((screen.getByLabelText('Phrase de la sauvegarde existante')as HTMLInputElement).value).toBe('');
});
