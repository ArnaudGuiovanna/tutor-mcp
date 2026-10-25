import { useRef, useState } from 'react';
import type { Snapshot, TutorOverview } from './types';

const roles: Record<string, string> = { owner: 'Propriétaire', admin: 'Administrateur', pedagogy_manager: 'Responsable pédagogique', trainer: 'Formateur', learner: 'Apprenant', auditor: 'Auditeur', billing_admin: 'Gestionnaire de facturation' };

export function AccountMenu({ snapshot, overview, busy, onSelect, onCreate, onRename, onActivity, onEnableLearning }: { snapshot: Snapshot; overview: TutorOverview | null; busy: boolean; onSelect: (id: string) => void; onCreate: (label: string) => void; onRename: (label: string) => void; onActivity: (activity: 'author' | 'learn' | 'train') => void; onEnableLearning: () => void }) {
  const [label, setLabel] = useState('');
  const [rename, setRename] = useState('');
  const [confirmLearning, setConfirmLearning] = useState(false);
  const menu = useRef<HTMLDetailsElement>(null);
  const profile = snapshot.profile;
  const roleNames = overview?.identity?.roles?.map((role) => roles[role] || role) || [];
  return <details className="account-menu" ref={menu} onKeyDown={(event) => { if (event.key === 'Escape' && menu.current) { menu.current.open = false; menu.current.querySelector('summary')?.focus(); } }}><summary aria-label="Mon compte et mes rôles"><strong>{profile?.label || 'Mon compte'}</strong><span>{roleNames.join(' · ') || 'Identité locale'}</span></summary>
    <div className="account-popover">
      <h2>Compte actif</h2><p><strong>{profile?.label || 'Mon compte'}</strong></p>
      <p>{snapshot.tutorStatus.connected ? snapshot.tutorStatus.label : 'Pas encore connecté à une institution'}</p>
      {roleNames.length > 0 && <p>Vos rôles : <strong>{roleNames.join(', ')}</strong></p>}
      {snapshot.identity.npub && <p className="mono">{snapshot.identity.npub}</p>}
      {snapshot.tutorStatus.connected && <fieldset disabled={busy}><legend>Mon activité dans l’institution</legend>
        {overview?.capabilities?.canRequestAuthoring && <button className="account-choice" aria-pressed={overview.capabilities.formationAuthoring === true} onClick={() => onActivity('author')}>Concevoir et publier</button>}
        {overview?.session?.allowedScopes.includes('progress:read') && <button className="account-choice" aria-pressed={overview.capabilities?.training === true} onClick={() => onActivity('train')}>Accompagner mes groupes</button>}
        {overview?.capabilities?.canLearn && <button className="account-choice" aria-pressed={overview.capabilities.learning === true} onClick={() => onActivity('learn')}>Apprendre</button>}
        {overview?.capabilities?.canEnableLearning && <><label className="confirmation"><input type="checkbox" checked={confirmLearning} onChange={(event) => setConfirmLearning(event.target.checked)} />Je souhaite aussi disposer de mon propre espace apprenant.</label><button className="button secondary" disabled={!confirmLearning} onClick={() => { setConfirmLearning(false); onEnableLearning(); }}>Activer mon espace apprenant</button><p className="subtle-note">Vos droits d’équipe sont conservés. Cette action ne vous inscrit à aucune formation.</p></>}
      </fieldset>}
      <fieldset disabled={busy}><legend>Changer de compte</legend>{(snapshot.profiles || []).map((item) => <button key={item.id} className="account-choice" aria-pressed={item.id === profile?.id} onClick={() => { if (menu.current) menu.current.open = false; onSelect(item.id); }}><strong>{item.label}</strong>{item.npub && <small>{item.npub.slice(0, 17)}…</small>}</button>)}</fieldset>
      <form onSubmit={(event) => { event.preventDefault(); if (label.trim()) { onCreate(label.trim()); setLabel(''); if (menu.current) menu.current.open = false; } }}><label className="field">Nom du nouveau compte<input value={label} onChange={(event) => setLabel(event.target.value)} maxLength={80} disabled={busy} placeholder="Par exemple : mon compte apprenant" /></label><button className="button primary" disabled={busy || !label.trim()}>Créer un compte et ses clés</button><p className="subtle-note">Chaque compte conserve ses propres clés et conversations. Les rôles sont accordés par votre institution.</p></form>
      <details><summary>Renommer ce compte</summary><form onSubmit={(event) => { event.preventDefault(); if (rename.trim()) onRename(rename.trim()); }}><label className="field">Nouveau nom<input value={rename} onChange={(event) => setRename(event.target.value)} maxLength={80} disabled={busy} /></label><button className="button secondary" disabled={busy || !rename.trim()}>Enregistrer le nom</button></form></details>
    </div>
  </details>;
}
