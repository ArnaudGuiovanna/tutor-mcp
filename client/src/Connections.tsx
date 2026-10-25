import { useEffect, useState } from 'react';
import { IdentityPanel } from './IdentityPanel';
import { Icon } from './icons';
import type { ClientBridge } from './bridge';
import type { Settings, AgentTarget, Snapshot, AgentConnection, Institution } from './types';

interface ConnectionsProps {
  section: 'account' | 'agents' | 'institution'; settings: Settings; targets: AgentTarget[]; connected: boolean; tutorStatus: Snapshot['tutorStatus']; connection: AgentConnection | null; identity: Snapshot['identity']; api: ClientBridge; onIdentityAction: (label: string, action: () => Promise<unknown>) => Promise<boolean>; busy: boolean;
  onDiscover: () => void; onSave: (settings: Settings) => void; onConnect: (target: AgentTarget, settings: Settings) => void; onAuthenticate: (methodId: string) => void; onDisconnectAgent: () => void;
  onConnectTutor: (settings: Settings, authoring: boolean) => void; onDisconnectTutor: () => void;
}

export function Connections(props: ConnectionsProps) {
  const { targets, connected, tutorStatus, connection, busy } = props;
  const [settings, setSettings] = useState<Settings>(() => structuredClone(props.settings));
  const [manual, setManual] = useState(!!props.settings.manualTarget);
  const [manualEnvironment, setManualEnvironment] = useState<'native' | 'wsl'>(props.settings.manualTarget?.environment || 'native');
  const [adapterPath, setAdapterPath] = useState(props.settings.manualTarget?.adapterPath || '');
  const [codexPath, setCodexPath] = useState(props.settings.manualTarget?.codexPath || '');
  const [distribution, setDistribution] = useState(props.settings.manualTarget?.distribution || '');
  const [wslUser, setWslUser] = useState(props.settings.manualTarget?.wslUser || '');
  const [methodId, setMethodId] = useState('');
  const [institution, setInstitution] = useState<Institution | null>(null);
  const authoring = false;
  const [argsText, setArgsText] = useState(JSON.stringify(props.settings.tutor.args));
  const [argsError, setArgsError] = useState('');
  const selected = targets.find((target) => target.id === settings.targetId);
  const agentName=settings.targetId==='bundled:claude'?'Claude':'Codex';
  useEffect(() => {if(!settings.targetId && targets.some(t=>t.id.startsWith('bundled:'))) setSettings(previous=>({...previous,targetId:targets.find(t=>t.id.startsWith('bundled:'))!.id}));},[targets,settings.targetId]);
  useEffect(() => {if(settings.tutor.mode!=='remote'||!settings.tutor.url)return;let live=true;const timer=setTimeout(()=>{props.api.discoverInstitution(settings.tutor.url).then(value=>{if(live)setInstitution(value);}).catch(()=>{});},400);return()=>{live=false;clearTimeout(timer);};},[props.api,settings.tutor.mode,settings.tutor.url]);
  const tutorBlockedReason = busy ? 'Une opération est en cours. Attendez sa fin.'
    : settings.tutor.mode === 'remote'
      ? !props.identity.exists ? 'Créez ou restaurez votre identité dans « Votre identité locale ».'
        : !props.identity.backupConfirmed ? 'Confirmez votre sauvegarde dans « Votre identité locale », ou vérifiez votre fichier déjà exporté.'
          : !settings.tutor.url.trim() ? 'Saisissez l’adresse de votre institution.'
            : !institution ? 'Cliquez sur « Vérifier l’institution » avant de vous connecter.' : ''
      : !settings.tutor.command.trim() ? 'Indiquez le programme Tutor local.'
        : !settings.cwd.trim() ? 'Indiquez un dossier de travail existant.' : '';

  function preparedSettings(): Settings | null {
    let args: unknown;
    try { args = JSON.parse(argsText); } catch { setArgsError('Les arguments doivent être un tableau JSON de textes, par exemple ["--local"].'); return null; }
    if (!Array.isArray(args) || args.some((value) => typeof value !== 'string')) { setArgsError('Chaque argument doit être un texte dans un tableau JSON.'); return null; }
    setArgsError('');
    const manualTarget: AgentTarget | undefined = manual ? {
      id: 'manual', label: manualEnvironment === 'wsl' ? `Codex — ${distribution || 'WSL'}` : 'Codex — installation manuelle', environment: manualEnvironment,
      adapterPath: adapterPath.trim() || undefined, codexPath: codexPath.trim() || undefined, distribution: manualEnvironment === 'wsl' ? distribution.trim() || undefined : undefined,
      wslUser: manualEnvironment === 'wsl' ? wslUser.trim() || undefined : undefined, available: !!adapterPath.trim(), detail: 'Installation sélectionnée manuellement',
    } : undefined;
    return { ...settings, targetId: manual ? 'manual' : settings.targetId, manualTarget, tutor: { ...settings.tutor, args: args as string[] } };
  }

  function connectAgent() {
    const next = preparedSettings();
    if (!next) return;
    const target = manual ? next.manualTarget : selected;
    if (target) props.onConnect(target, next);
  }

  return <>
    {props.section === 'account' && <IdentityPanel identity={props.identity} api={props.api} busy={busy} connected={tutorStatus.connected && settings.tutor.mode === 'remote'} institutionUrl={settings.tutor.url} run={props.onIdentityAction} />}

    {props.section === 'agents' && <section className="card connection-card">
      <div className="connection-heading"><div className="connection-heading-main"><span className="connection-number">02</span><div><h2 className="card-title">Votre agent IA</h2><p>Choisissez Codex ou Claude intégrés. Votre compte fournisseur reste personnel.</p></div></div><span className={`tag ${connected ? 'green' : ''}`}>{connected ? 'Connecté' : 'À connecter'}</span></div>
      <div className="button-row" style={{ marginBottom: 18 }}><button className="button secondary" disabled={busy || connected} onClick={props.onDiscover}><Icon name="refresh" size={14} />Rechercher les installations</button><span className="inline-status">Windows natif et distributions WSL2</span></div>
      {!!targets.length && <fieldset style={{ border: 0, padding: 0, margin: 0 }}><legend className="visually-hidden">Installation de Codex</legend><div className="target-list">{targets.map((target) => <label className="target-option" key={target.id}><input type="radio" name="target" checked={!manual && settings.targetId === target.id} disabled={busy || connected || !target.available} onChange={() => { setManual(false); setSettings({ ...settings, targetId: target.id }); }} /><div className="target-option-main"><strong>{target.label}</strong><small>{target.detail}{target.codexVersion ? ` · Codex ${target.codexVersion}` : ''}{target.adapterVersion ? ` · Adaptateur ${target.adapterVersion}` : ''}</small></div><span className={`tag ${target.available ? 'green' : ''}`}>{target.available ? 'Disponible' : 'À compléter'}</span></label>)}</div></fieldset>}
      <details className="advanced-workspace"><summary>Dossier de séance et réglages avancés</summary><div className="form-grid"><label className="field full">Dossier de travail<input value={settings.cwd} onChange={(event) => setSettings({ ...settings, cwd: event.target.value })} disabled={busy || connected} placeholder={selected?.environment === 'wsl' || (manual && manualEnvironment === 'wsl') ? '~' : 'C:\\Users\\vous\\Tutor'} /><small>Un dossier existant dans l’environnement choisi. Dans WSL, ~ désigne votre dossier personnel. Un dossier privé est déjà préparé pour les agents intégrés.</small></label></div></details>
      <details open={manual || undefined}><summary>Choisir une installation manuellement</summary><div className="form-grid">
        <label className="checkbox-label field full"><input type="checkbox" checked={manual} disabled={busy || connected} onChange={(event) => setManual(event.target.checked)} />Utiliser les chemins ci-dessous</label>
        <label className="field">Environnement<select value={manualEnvironment} disabled={busy || connected} onChange={(event) => setManualEnvironment(event.target.value as 'native' | 'wsl')}><option value="native">Windows natif</option><option value="wsl">WSL2</option></select></label>
        {manualEnvironment === 'wsl' && <><label className="field">Distribution WSL<input value={distribution} disabled={busy || connected} onChange={(event) => setDistribution(event.target.value)} placeholder="Ubuntu" /></label><label className="field">Utilisateur Linux <span className="field-optional">facultatif</span><input value={wslUser} disabled={busy || connected} onChange={(event) => setWslUser(event.target.value)} placeholder="Utilisateur par défaut de la distribution" /></label></>}
        <label className="field full">Chemin de l’adaptateur ACP<input value={adapterPath} disabled={busy || connected} onChange={(event) => setAdapterPath(event.target.value)} placeholder={manualEnvironment === 'wsl' ? '/home/vous/.local/bin/codex-acp' : 'C:\\Outils\\codex-acp.exe'} /><small>Le programme codex-acp, qui lancera Codex pour cette application.</small></label>
        <label className="field full">Chemin de Codex <span className="field-optional">facultatif</span><input value={codexPath} disabled={busy || connected} onChange={(event) => setCodexPath(event.target.value)} placeholder="Laisser vide pour le Codex fourni par l’adaptateur" /><small>{manualEnvironment === 'wsl' ? 'Pour utiliser votre propre installation, choisissez un exécutable ou un script Linux exécutable dans cette distribution.' : 'Pour utiliser votre propre installation, choisissez son exécutable Codex .exe ; les wrappers npm .cmd ne conviennent pas à ce champ.'} Laissez vide pour conserver la version compatible fournie par l’adaptateur.</small></label>
      </div></details>
      <div className="connection-actions"><span className="inline-status">{connection ? `${connection.agentInfo.title || connection.agentInfo.name || 'Codex'}${connection.agentInfo.version ? ` · ${connection.agentInfo.version}` : ''}` : 'Codex Desktop n’est pas nécessaire.'}</span><div className="button-row">{connected ? <button className="button secondary" disabled={busy} onClick={props.onDisconnectAgent}>Déconnecter {agentName}</button> : <button className="button primary" disabled={busy || !settings.cwd.trim() || (manual ? !adapterPath.trim() || (manualEnvironment === 'wsl' && !distribution.trim()) : !selected?.available)} onClick={connectAgent}>Connecter {agentName}<Icon name="arrow" size={14} /></button>}</div></div>
      {!!connection?.authMethods.length && <div className="form-grid" style={{ marginTop: 19 }}><label className="field">Identification {agentName}<select value={methodId || connection.authMethods[0]?.id} disabled={busy} onChange={(event) => setMethodId(event.target.value)}>{connection.authMethods.map((method) => <option key={method.id} value={method.id}>{method.name}</option>)}</select></label><div style={{ alignSelf: 'end' }}><button className="button secondary" disabled={busy} onClick={() => props.onAuthenticate(methodId || connection.authMethods[0].id)}><Icon name="external" size={13} />S’identifier auprès de {agentName}</button></div></div>}
    </section>}

    {props.section === 'institution' && <section className="card connection-card">
      <div className="connection-heading"><div className="connection-heading-main"><span className="connection-number">03</span><div><h2 className="card-title">Votre moteur Tutor</h2><p>Le serveur qui conserve vos formations et votre progression.</p></div></div><span className={`tag ${tutorStatus.connected ? 'green' : ''}`}>{tutorStatus.connected ? 'Connecté' : 'À connecter'}</span></div>
      <div className="form-grid"><label className="field full">Emplacement<select value={settings.tutor.mode} disabled={busy || tutorStatus.connected} onChange={(event) => setSettings({ ...settings, tutor: { ...settings.tutor, mode: event.target.value as 'remote' | 'local' } })}><option value="remote">Institution ou serveur distant</option><option value="local">Tutor local sur cet ordinateur</option></select></label>
        {settings.tutor.mode === 'remote' ? <label className="field full">Adresse de Tutor<input type="url" value={settings.tutor.url} disabled={busy || tutorStatus.connected} onChange={(event) => { setSettings({ ...settings, tutor: { ...settings.tutor, url: event.target.value } }); setInstitution(null); }} placeholder="https://tutor.votre-institution.fr" /><small>Votre application signera un défi unique. L’institution doit avoir autorisé vos clés publiques ; aucun compte par e-mail n’est créé ici.</small></label> : <><label className="field full">Programme Tutor<input value={settings.tutor.command} disabled={busy || tutorStatus.connected} onChange={(event) => setSettings({ ...settings, tutor: { ...settings.tutor, command: event.target.value } })} placeholder="tutor-mcp" /><small>Le programme doit être disponible dans le même environnement que Codex : Windows ou la distribution WSL choisie.</small></label><label className="field full">Arguments du programme<input className="mono" value={argsText} disabled={busy || tutorStatus.connected} onChange={(event) => setArgsText(event.target.value)} placeholder={'["--local"]'} /><small>Tableau JSON : chaque élément correspond à un argument distinct.</small></label></>}
      </div>
      {settings.tutor.mode === 'remote' && !tutorStatus.connected && <><button className="button secondary" disabled={busy || !settings.tutor.url.trim()} onClick={() => void props.onIdentityAction('Vérification de l’institution…', async () => { setInstitution(await props.api.discoverInstitution(settings.tutor.url)); })}>Vérifier l’institution</button>{institution && <section className="institution-preview" aria-label="Institution vérifiée"><h3>{institution.name}</h3><p className="mono">{institution.issuer}</p><p className="subtle-note">Institution : {institution.institution_id}</p><p className="subtle-note">Les actions disponibles dépendent de vos droits dans cette institution.</p></section>}</>}
      {argsError && <p role="alert" className="notice error" style={{ marginTop: 15 }}>{argsError}</p>}
      {tutorStatus.message && <p className="subtle-note">{tutorStatus.message}</p>}
      {!tutorStatus.connected && tutorBlockedReason && <p id="tutor-connect-help" className="subtle-note">{tutorBlockedReason}</p>}
      <div className="connection-actions"><span className="inline-status">Votre accès Tutor est distinct de votre compte fournisseur.</span>{tutorStatus.connected ? <button className="button secondary" disabled={busy} onClick={props.onDisconnectTutor}>Déconnecter Tutor</button> : <button className="button primary" disabled={!!tutorBlockedReason} aria-describedby={tutorBlockedReason ? 'tutor-connect-help' : undefined} onClick={() => { const next = preparedSettings(); if (next) { props.onConnectTutor(next, authoring); } }}>{settings.tutor.mode === 'remote' ? <>Connecter avec ma clé<Icon name="arrow" size={14} /></> : <>Vérifier Tutor local<Icon name="arrow" size={14} /></>}</button>}</div>
    </section>}
    {props.section !== 'account' && <div className="button-row"><button className="button secondary" disabled={busy} onClick={() => { const next = preparedSettings(); if (next) props.onSave(next); }}><Icon name="check" size={14} />Enregistrer les réglages</button></div>}
    <p className="subtle-note">Codex et son adaptateur doivent être compatibles. Les fonctionnalités réellement disponibles sont vérifiées à la connexion ; une installation détectée ne garantit pas encore le fonctionnement complet d’une séance.</p>
  </>;
}
