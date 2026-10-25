import { useState } from 'react';
import type { Snapshot, TutorOverview } from './types';

const roles: Record<string, string> = { owner: 'Propriétaire', admin: 'Administrateur', pedagogy_manager: 'Responsable pédagogique', trainer: 'Formateur', learner: 'Apprenant', auditor: 'Auditeur', billing_admin: 'Gestionnaire de facturation' };

export function AccountMenu({ snapshot, overview, busy, onSelect, onCreate, onRename }: { snapshot: Snapshot; overview: TutorOverview | null; busy: boolean; onSelect: (id: string) => void; onCreate: (label: string) => void; onRename: (label: string) => void }) {
  const [label, setLabel] = useState('');
  const [rename, setRename] = useState('');
  return <section className="account-page" aria-label="Mon compte et mes rôles">
    <h2>{snapshot.profile?.label || 'Mon compte'}</h2>
    <p>{snapshot.tutorStatus.connected ? snapshot.tutorStatus.label : 'Aucune institution connectée'}</p>
    {!!overview?.identity?.roles?.length && <p>{overview.identity.roles.map(role => roles[role] || role).join(' · ')}</p>}
    <fieldset disabled={busy}><legend>Changer de compte</legend>{(snapshot.profiles || []).map(item => <button key={item.id} className="account-choice" aria-pressed={item.id === snapshot.profile?.id} onClick={() => onSelect(item.id)}><strong>{item.label}</strong>{item.npub && <small className="mono">{item.npub}</small>}</button>)}</fieldset>
    <form onSubmit={event => { event.preventDefault(); if (label.trim()) { onCreate(label.trim()); setLabel(''); } }}><label className="field">Nom du nouveau compte<input value={label} onChange={event => setLabel(event.target.value)} maxLength={80} disabled={busy} /></label><button className="button primary" disabled={busy || !label.trim()}>Créer un compte et ses clés</button></form>
    <form onSubmit={event => { event.preventDefault(); if (rename.trim()) onRename(rename.trim()); }}><label className="field">Renommer ce compte<input value={rename} onChange={event => setRename(event.target.value)} maxLength={80} disabled={busy} /></label><button className="button secondary" disabled={busy || !rename.trim()}>Enregistrer le nom</button></form>
    {!!snapshot.intents.length && <details><summary>Notes historiques de conception</summary><p>Vos anciennes intentions sont conservées avec les conversations auxquelles elles étaient liées.</p>{snapshot.intents.map(intent => <article key={intent.id}><h3>{intent.title}</h3>{[intent.motivation, intent.startingPoint, intent.goals, intent.approach, intent.constraints, intent.designCriteria].filter(Boolean).map((value, index) => <p key={index}>{value}</p>)}</article>)}</details>}
  </section>;
}
