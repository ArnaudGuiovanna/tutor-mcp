import { useState } from 'react';
import type { ClientBridge } from './bridge';
import type { PublicIdentity } from './types';
import { Icon } from './icons';

export function IdentityPanel({ identity, api, busy, connected, institutionUrl, run }: { identity: PublicIdentity; api: ClientBridge; busy: boolean; connected: boolean; institutionUrl: string; run: (label: string, action: () => Promise<unknown>) => Promise<boolean> }) {
  const [nsec, setNsec] = useState('');
  const [phrase, setPhrase] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [oldPhrase, setOldPhrase] = useState('');
  const exported = identity.backupExported === true;
  const [revoke, setRevoke] = useState(false);
  const [validation, setValidation] = useState('');
  const [copy, setCopy] = useState('');
  function checkPhrase() {
    if (phrase.length < 12) { setValidation('Choisissez une phrase de sauvegarde d’au moins 12 caractères.'); return false; }
    if (phrase !== confirmation) { setValidation('Les deux phrases de sauvegarde doivent être identiques.'); return false; }
    setValidation(''); return true;
  }
  async function exportBackup() {
    if (!checkPhrase()) return;
    const secret = phrase; setPhrase(''); setConfirmation('');
    await run('Enregistrement de votre sauvegarde chiffrée…', async () => {
      await api.backupIdentity(secret);
      await api.confirmIdentityBackup();
    });
  }
  async function restore() {
    const secret = oldPhrase; setOldPhrase('');
    await run(identity.exists ? 'Vérification de la sauvegarde existante…' : 'Restauration de l’identité locale…', () => api.restoreIdentity(secret));
  }
  async function renew(recovery: boolean) {
    if (!checkPhrase() || !revoke) { if (!revoke) setValidation('Confirmez la révocation des anciens accès avant de continuer.'); return; }
    const current = oldPhrase; const next = phrase; setOldPhrase(''); setPhrase(''); setConfirmation('');
    const success = await run(recovery ? 'Récupération de votre identité…' : 'Renouvellement de votre identité…', () => recovery ? api.recoverIdentity(institutionUrl, current, next) : api.rotateIdentity(current, next));
    if (success) { setRevoke(false); }
  }
  const phraseFields = <><label className="field">Phrase pour chiffrer la sauvegarde<input type="password" value={phrase} disabled={busy} autoComplete="new-password" onChange={(event) => setPhrase(event.target.value)} /><small>Au moins 12 caractères. Cette phrase protège le fichier ; elle ne sert pas à vous connecter à Tutor.</small></label><label className="field">Confirmer la phrase de sauvegarde<input type="password" value={confirmation} disabled={busy} autoComplete="new-password" onChange={(event) => setConfirmation(event.target.value)} /></label></>;
  return <section className="card connection-card identity-card" aria-label="Identité locale">
    <div className="connection-heading"><div className="connection-heading-main"><span className="connection-number">01</span><div><h2 className="card-title">Votre identité locale</h2><p>Une clé personnelle pour les institutions qui vous autorisent.</p></div></div><span className={`tag ${identity.backupConfirmed ? 'green' : ''}`}>{identity.backupConfirmed ? 'Sauvegardée' : exported ? 'Exportée · à confirmer' : identity.exists ? 'À sauvegarder' : 'À créer'}</span></div>
    <p>Votre identifiant public peut être partagé. Vos clés privées restent sur cet appareil, protégées par Windows. Une sauvegarde chiffrée vous permet de retrouver ce compte.</p>
    {!identity.exists ? <>
      <div className="button-row"><button className="button primary" disabled={busy} onClick={() => void run('Création de votre identité…', () => api.createIdentity())}><Icon name="plus" size={14} />Créer mon identité</button></div>
      <details><summary>J’ai déjà une identité</summary><div className="form-grid"><label className="field full">Clé privée Nostr à importer (nsec)<input type="password" value={nsec} disabled={busy} autoComplete="off" spellCheck={false} onChange={(event) => setNsec(event.target.value)} /><small>Import local uniquement. La clé ne sera envoyée ni à Tutor ni à Codex. Une nouvelle clé de récupération sera créée.</small></label></div><button className="button secondary" disabled={busy || !nsec.startsWith('nsec1')} onClick={() => { const secret = nsec; setNsec(''); void run('Import local de l’identité…', () => api.createIdentity(secret)); }}>Importer la clé nsec</button></details>
    </> : <div className="public-keys"><label className="field full">Identité publique (npub)<input aria-label="Identité publique (npub)" readOnly className="mono" value={identity.npub || ''} onFocus={(event) => event.currentTarget.select()} /><small>Cet identifiant public permet à l’institution d’autoriser votre compte.</small></label><div className="button-row"><button className="button secondary" onClick={()=>{void navigator.clipboard?.writeText(identity.npub || '').then(()=>setCopy('Identifiant public copié.')).catch(()=>setCopy('Sélectionnez votre identifiant puis utilisez Ctrl + C.'));}}>Copier mon identifiant</button><button className="button secondary" onClick={() => { void navigator.clipboard?.writeText(`${identity.npub}\n${identity.recoveryNpub}`).then(() => setCopy('Clés publiques copiées.')).catch(() => setCopy('Sélectionnez les clés publiques puis utilisez Ctrl + C.')); }}>Copier les clés publiques</button><span role="status" className="inline-status">{copy}</span></div><details><summary>Informations de récupération</summary><label className="field full">Clé publique de récupération (npub)<input readOnly className="mono" value={identity.recoveryNpub || ''} onFocus={(event) => event.currentTarget.select()} /></label><p>Le bouton de copie fournit les deux valeurs publiques nécessaires à l’administrateur. Il ne copie aucune clé privée.</p></details></div>}
    {identity.needsBackup && <section className="identity-backup" aria-label="Sauvegarde obligatoire"><h3>Protéger ce compte</h3>{identity.needsRecoveryRestore ? <p>Ce compte a déjà des clés publiques. Vérifiez votre fichier de sauvegarde existant ci-dessous pour continuer avec les mêmes clés.</p> : <><p>Choisissez une phrase, puis enregistrez le fichier chiffré sur un support sûr, séparé de cet ordinateur. Gardez la phrase à part. Une fois le fichier écrit et vérifié, le compte passe à « Sauvegardée ».</p>{exported && <section className="backup-confirmation" aria-label="Confirmation de la sauvegarde"><p>Le fichier est exporté. Son enregistrement peut être terminé sans refaire l’export.</p><button className="button primary" disabled={busy} onClick={() => void run('Confirmation de la sauvegarde…', () => api.confirmIdentityBackup())}><Icon name="check" size={14} />Terminer ma sauvegarde</button></section>}<div className="form-grid">{phraseFields}</div><button className="button primary" disabled={busy} onClick={() => void exportBackup()}>Enregistrer ma sauvegarde chiffrée</button></>}</section>}
    {(!identity.exists || identity.needsBackup) && <details open={identity.needsRecoveryRestore || undefined}><summary>{identity.exists ? 'J’ai déjà exporté ma sauvegarde' : 'Restaurer une sauvegarde Tutor'}</summary><p>{identity.exists ? 'Choisissez votre fichier et sa phrase pour vérifier la sauvegarde existante et terminer la confirmation. L’identité du fichier doit correspondre à celle de cet appareil.' : 'Sélectionnez votre fichier de sauvegarde chiffrée. Seule la clé active sera conservée sur cet appareil.'}</p><label className="field">Phrase de la sauvegarde existante<input type="password" value={oldPhrase} disabled={busy} autoComplete="off" onChange={(event) => setOldPhrase(event.target.value)} /></label><button className="button secondary" disabled={busy || !oldPhrase} onClick={() => void restore()}>{identity.exists ? 'Vérifier ma sauvegarde existante' : 'Choisir et restaurer la sauvegarde'}</button></details>}
    {identity.backupConfirmed && <details><summary>Renouveler ou récupérer mes clés</summary><p>Sélectionnez votre sauvegarde actuelle pour approuver le changement avec votre clé hors ligne. Une nouvelle sauvegarde sera créée avant le changement. Le serveur révoquera les anciennes clés et toutes les sessions associées à cette identité dans ses institutions. Conservez les deux sauvegardes jusqu’à la reconnexion réussie.</p><div className="form-grid">{phraseFields}<label className="field full">Phrase de l’ancienne sauvegarde<input type="password" value={oldPhrase} disabled={busy} autoComplete="off" onChange={(event) => setOldPhrase(event.target.value)} /></label></div><label className="confirmation"><input type="checkbox" checked={revoke} disabled={busy} onChange={(event) => setRevoke(event.target.checked)} />Je confirme le renouvellement, la nouvelle sauvegarde et la révocation des anciens accès.</label><div className="button-row"><button className="button secondary" disabled={busy || !connected || !oldPhrase || !revoke} onClick={() => void renew(false)}>Créer de nouvelles clés</button><button className="button secondary" disabled={busy || !institutionUrl || !oldPhrase || !revoke} onClick={() => void renew(true)}>Récupérer avec ma clé hors ligne</button></div></details>}
    {!identity.exists && institutionUrl && <details><summary>Récupérer une identité compromise ou perdue</summary><p>La récupération révoque les anciennes clés et toutes les sessions associées à cette identité sur ce serveur. Vous sélectionnerez l’ancienne sauvegarde puis un fichier neuf pour la nouvelle sauvegarde.</p><label className="field">Phrase de l’ancienne sauvegarde<input type="password" value={oldPhrase} autoComplete="off" disabled={busy} onChange={(event) => setOldPhrase(event.target.value)} /></label><div className="form-grid">{phraseFields}</div><label className="confirmation"><input type="checkbox" checked={revoke} disabled={busy} onChange={(event) => setRevoke(event.target.checked)} />Je confirme la nouvelle sauvegarde et la révocation des anciens accès.</label><button className="button secondary" disabled={busy || !oldPhrase || !revoke} onClick={() => void renew(true)}>Récupérer avec ma clé hors ligne</button></details>}
    {validation && <p role="alert" className="notice error">{validation}</p>}
  </section>;
}
