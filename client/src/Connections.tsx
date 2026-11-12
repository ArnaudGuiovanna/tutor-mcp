import { t } from './i18n';
import { useEffect, useRef, useState } from 'react';
import { IdentityPanel } from './IdentityPanel';
import { Icon } from './icons';
import type { ClientBridge } from './bridge';
import type { Settings, AgentTarget, Snapshot, AgentConnection, Institution } from './types';

interface ConnectionsProps {
  section: 'account' | 'agents' | 'institution'; settings: Settings; targets: AgentTarget[]; connected: boolean; connecting?: boolean; agentStatus?: Snapshot['agentStatus']; tutorStatus: Snapshot['tutorStatus']; connection: AgentConnection | null; identity: Snapshot['identity']; api: ClientBridge; onIdentityAction: (label: string, action: () => Promise<unknown>) => Promise<boolean>; busy: boolean;
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
  const [institution, setInstitution] = useState<Institution | null>(null);
  const authoring = false;
  const [argsText, setArgsText] = useState(JSON.stringify(props.settings.tutor.args));
  const [argsError, setArgsError] = useState('');
  // Snapshots refresh the form only where the learner has no unsaved edit.
  const tutorEdited = useRef(false);
  const cwdEdited = useRef(false);
  useEffect(() => { setSettings(previous => ({...previous, targetId:props.settings.targetId, cwd:cwdEdited.current ? previous.cwd : props.settings.cwd})); }, [props.settings.targetId,props.settings.cwd]);
  useEffect(() => { if (tutorEdited.current) return; setSettings(previous => ({...previous,tutor:{...props.settings.tutor}}));setArgsText(JSON.stringify(props.settings.tutor.args)); }, [props.settings.tutor.mode,props.settings.tutor.url,props.settings.tutor.command,JSON.stringify(props.settings.tutor.args)]);
  function editTutor(tutor: Partial<Settings['tutor']>) { tutorEdited.current = true; setSettings(previous => ({ ...previous, tutor: { ...previous.tutor, ...tutor } })); }
  const selected = targets.find((target) => target.id === settings.targetId);
  const agentName=!manual&&settings.targetId==='local:qwen'?'Qwen':(settings.targetId.includes('claude') || (manual && adapterPath.includes('claude')))?t("Claude"):t("Codex");
  useEffect(() => {if(!settings.targetId && targets.some(t=>t.id.startsWith('bundled:'))) setSettings(previous=>({...previous,targetId:targets.find(t=>t.id.startsWith('bundled:'))!.id}));},[targets,settings.targetId]);
  useEffect(() => {if(props.section!=='institution'||settings.tutor.mode!=='remote'||!settings.tutor.url)return;setInstitution(null);let live=true;const timer=setTimeout(()=>{props.api.discoverInstitution(settings.tutor.url).then(value=>{if(live)setInstitution(value);}).catch(()=>{});},400);return()=>{live=false;clearTimeout(timer);};},[props.api,props.section,settings.tutor.mode,settings.tutor.url]);
  const tutorBlockedReason = busy ? t("Une opération est en cours. Attendez sa fin.")
    : settings.tutor.mode === 'remote'
      ? !props.identity.exists ? t("Créez ou restaurez votre identité dans « Votre identité locale ».")
        : !props.identity.backupConfirmed ? t("Confirmez votre sauvegarde dans « Votre identité locale », ou vérifiez votre fichier déjà exporté.")
          : !settings.tutor.url.trim() ? t("Saisissez l’adresse de votre institution.")
            : !institution ? t("Cliquez sur « Vérifier l’institution » avant de vous connecter.") : ''
      : !settings.tutor.command.trim() ? t("Indiquez le programme Tutor local.")
        : !settings.cwd.trim() ? t("Indiquez un dossier de travail existant.") : '';

  function preparedSettings(): Settings | null {
    // Each form owns only its own settings. A Tutor save must never revert the agent, or vice versa.
    let args: unknown;
    try { args = JSON.parse(argsText); } catch { setArgsError('Les arguments doivent être un tableau JSON de textes, par exemple ["--local"].'); return null; }
    if (!Array.isArray(args) || args.some((value) => typeof value !== 'string')) { setArgsError('Chaque argument doit être un texte dans un tableau JSON.'); return null; }
    setArgsError('');
    const manualTarget: AgentTarget | undefined = manual ? {
      id: 'manual', label: manualEnvironment === 'wsl' ? `${agentName} — ${distribution || 'WSL'}` : `${agentName} — ${t("Installation manuelle")}`, environment: manualEnvironment,
      adapterPath: adapterPath.trim() || undefined, codexPath: codexPath.trim() || undefined, distribution: manualEnvironment === 'wsl' ? distribution.trim() || undefined : undefined,
      wslUser: manualEnvironment === 'wsl' ? wslUser.trim() || undefined : undefined, available: !!adapterPath.trim(), detail: 'Installation sélectionnée manuellement',
    } : undefined;
    return props.section==='agents'
      ? {...props.settings,cwd:settings.cwd,targetId:manual?'manual':settings.targetId,manualTarget}
      : {...props.settings,tutor:{...settings.tutor,args:args as string[]}};
  }

  function connectAgent() {
    const next = preparedSettings();
    if (!next) return;
    cwdEdited.current = false;
    const target = manual ? next.manualTarget : selected;
    if (target) props.onConnect(target, next);
  }

  return <>
    {props.section === 'account' && <IdentityPanel identity={props.identity} api={props.api} busy={busy} connected={tutorStatus.connected && settings.tutor.mode === 'remote'} institutionUrl={settings.tutor.url} run={props.onIdentityAction} />}

    {props.section === 'agents' && <section className="card connection-card" aria-label={t("Votre agent IA")}>
      <div className="connection-heading"><div className="connection-heading-main"><div><h2 className="card-title">{t("Votre agent IA")}</h2><p>{t("Choisissez Qwen local, Codex ou Claude.")}</p></div></div><span className={`tag ${connected ? 'green' : ''}`}>{props.connecting ? t("Connexion en cours…") : connected ? t("Agent lancé") : t("À connecter")}</span></div>
      {connected && <p className="connected-summary"><Icon name="check" size={16}/>{agentName} · {t(props.agentStatus?.authentication === "none" ? "Connexion au harness requise" : props.agentStatus?.authentication === "authenticated" || props.agentStatus?.authentication === "not_required" ? "Prêt à vous accompagner" : "Agent lancé · accès géré par le harness")}</p>}
      <details className="connection-details" open={!connected || undefined}><summary>{t("Gérer l’agent")}</summary>
      <div className="button-row discovery-row"><button className="button secondary" disabled={busy} onClick={props.onDiscover}><Icon name="refresh" size={14} />{t("Rechercher les installations")}</button><span className="inline-status">{t("Windows natif et distributions WSL2")}</span></div>
      {!!targets.length && <fieldset className="target-fieldset"><legend className="visually-hidden">{t("Choisir un agent")}</legend><div className="target-list">{targets.map((target) => <label className="target-option" key={target.id}><input type="radio" name="target" checked={!manual && settings.targetId === target.id} disabled={busy || !target.available} onChange={() => { setManual(false); setSettings({ ...settings, targetId: target.id }); props.onConnect(target, {...props.settings, targetId:target.id, manualTarget:undefined, cwd:settings.cwd}); }} /><div className="target-option-main"><strong>{target.id.startsWith('bundled:')?target.id==='bundled:claude'?'Claude':'Codex':target.label}</strong><small>{target.id.startsWith('bundled:')?t('Inclus dans Tutor · votre compte fournisseur'):t(target.detail)}{target.codexVersion ? ` · Codex ${target.codexVersion}` : ''}{target.adapterVersion ? t(' · Adaptateur {version}',{version:target.adapterVersion}) : ''}</small></div><span className="target-check" aria-hidden="true">{!manual && settings.targetId===target.id ? <Icon name="check" size={16}/> : null}</span></label>)}</div></fieldset>}
      <details className="advanced-workspace"><summary>{t("Dossier de séance et réglages avancés")}</summary><div className="form-grid"><label className="field full">{t("Dossier de travail")}<input value={settings.cwd} onChange={(event) => { cwdEdited.current = true; setSettings({ ...settings, cwd: event.target.value }); }} disabled={busy || connected} placeholder={selected?.environment === 'wsl' || (manual && manualEnvironment === 'wsl') ? t("~") : t("C:\\Users\\vous\\Tutor")} /><small>{t("Un dossier existant dans l’environnement choisi. Dans WSL, ~ désigne votre dossier personnel. Un dossier privé est déjà préparé pour les agents intégrés.")}</small></label></div></details>
      <details open={manual || undefined}><summary>{t("Choisir une installation manuellement")}</summary><div className="form-grid">
        <label className="checkbox-label field full"><input type="checkbox" checked={manual} disabled={busy || connected} onChange={(event) => setManual(event.target.checked)} />{t("Utiliser les chemins ci-dessous")}</label>
        <label className="field">{t("Environnement")}<select value={manualEnvironment} disabled={busy || connected} onChange={(event) => setManualEnvironment(event.target.value as 'native' | 'wsl')}><option value="native">{t("Windows natif")}</option><option value="wsl">{t("WSL2")}</option></select></label>
        {manualEnvironment === 'wsl' && <><label className="field">{t("Distribution WSL")}<input value={distribution} disabled={busy || connected} onChange={(event) => setDistribution(event.target.value)} placeholder={t("Ubuntu")} /></label><label className="field">{t("Utilisateur Linux")} <span className="field-optional">{t("facultatif")}</span><input value={wslUser} disabled={busy || connected} onChange={(event) => setWslUser(event.target.value)} placeholder={t("Utilisateur par défaut de la distribution")} /></label></>}
        <label className="field full">{t("Chemin de l’adaptateur ACP")}<input value={adapterPath} disabled={busy || connected} onChange={(event) => setAdapterPath(event.target.value)} placeholder={manualEnvironment === 'wsl' ? t("/home/vous/.local/bin/codex-acp") : t("C:\\Outils\\codex-acp.exe")} /><small>{t("Le programme codex-acp ou claude-agent-acp installé dans cet environnement.")}</small></label>
        <label className="field full">{t("Chemin de Codex")} <span className="field-optional">{t("facultatif")}</span><input value={codexPath} disabled={busy || connected} onChange={(event) => setCodexPath(event.target.value)} placeholder={t("Laisser vide pour le Codex fourni par l’adaptateur")} /><small>{manualEnvironment === 'wsl' ? t("Pour utiliser votre propre installation, choisissez un exécutable ou un script Linux exécutable dans cette distribution.") : t("Pour utiliser votre propre installation, choisissez son exécutable Codex .exe ; les wrappers npm .cmd ne conviennent pas à ce champ.")} {t("Laissez vide pour conserver la version compatible fournie par l’adaptateur.")}</small></label>
      </div></details>
      <div className="connection-actions"><span className="inline-status">{connection ? `${connection.agentInfo.title || connection.agentInfo.name || agentName}${connection.agentInfo.version ? ` · ${connection.agentInfo.version}` : ''}` : !manual && settings.targetId === 'local:qwen' ? t("Modèle sur cet ordinateur, sans compte fournisseur IA.") : t("La connexion existante de votre harness est utilisée automatiquement.")}</span><div className="button-row">{connected ? <button className="button secondary" disabled={busy} onClick={props.onDisconnectAgent}>{t("Déconnecter")} {agentName}</button> : <button className="button primary" disabled={busy || !settings.cwd.trim() || (manual ? !adapterPath.trim() || (manualEnvironment === 'wsl' && !distribution.trim()) : !selected?.available)} onClick={connectAgent}>{t("Connecter")} {agentName}<Icon name="arrow" size={14} /></button>}</div></div>
      <button className="link-button save-connection" disabled={busy || connected} onClick={()=>{const next=preparedSettings();if(next){cwdEdited.current=false;props.onSave(next);}}}>{t("Enregistrer le choix de l’agent")}</button>
      </details>
      {connected && props.agentStatus?.authentication === 'none' && <div className="notice" role="status"><p>{t("Votre harness demande une connexion. Tutor utilise ensuite les accès qu’il gère.")}</p>{connection?.authMethods.some(method => !['api-key','gateway','gateway-bedrock'].includes(method.id)) ? <button className="button secondary" disabled={busy} onClick={() => props.onAuthenticate('')}><Icon name="external" size={13}/>{t("Ouvrir la connexion officielle")} {agentName}</button> : <p>{t("Connectez-vous depuis le terminal de votre harness dans cet environnement, puis reconnectez l’agent.")}</p>}</div>}
      {props.agentStatus?.lastFailure && <details className="subtle-note"><summary>{t("Dernier incident de requête")}</summary><p>{t(props.agentStatus.lastFailure.message)}</p><p>{t("Source :")} {props.agentStatus.lastFailure.source === 'adapter' ? 'ACP · sessionFailure' : 'ACP · JSON-RPC'} · {props.agentStatus.lastFailure.category}</p></details>}
      {props.agentStatus?.environment && <p className="subtle-note">{props.agentStatus.environment === 'wsl' ? `WSL · ${props.agentStatus.distribution || ''} · ${props.agentStatus.wslUser || ''}` : t("Environnement natif")}</p>}
    </section>}

    {props.section === 'institution' && <section className="card connection-card" aria-label={t("Votre moteur Tutor")}>
      <div className="connection-heading"><div className="connection-heading-main"><div><h2 className="card-title">{t("Votre moteur Tutor")}</h2><p>{t("Le serveur qui conserve vos formations et votre progression.")}</p></div></div><span className={`tag ${tutorStatus.connected ? 'green' : ''}`}>{tutorStatus.connected ? t("Connecté") : t("À connecter")}</span></div>
      {tutorStatus.connected && <p className="connected-summary"><Icon name="check" size={16}/>{tutorStatus.label}</p>}
      <details className="connection-details" open={!tutorStatus.connected || undefined}><summary>{t("Gérer la connexion Tutor")}</summary>
      {settings.tutor.mode==='remote' && !props.identity.backupConfirmed && <IdentityPanel identity={props.identity} api={props.api} busy={busy} connected={false} institutionUrl={settings.tutor.url} run={props.onIdentityAction}/>}
      <div className="form-grid"><label className="field full">{t("Emplacement")}<select value={settings.tutor.mode} disabled={busy || tutorStatus.connected} onChange={(event) => editTutor({ mode: event.target.value as 'remote' | 'local' })}><option value="remote">{t("Institution ou serveur distant")}</option><option value="local">{t("Tutor local sur cet ordinateur")}</option></select></label>
        {settings.tutor.mode === 'remote' ? <label className="field full">{t("Adresse de Tutor")}<input type="url" aria-label={t("Adresse de Tutor")} aria-describedby="tutor-address-help" value={settings.tutor.url} disabled={busy || tutorStatus.connected} onChange={(event) => { editTutor({ url: event.target.value }); setInstitution(null); }} placeholder={t("https://tutor.votre-institution.fr")} /><small id="tutor-address-help">{t("Utilisez l’adresse fournie par votre institution. Votre identité enregistrée permet de vous connecter.")}</small></label> : <><label className="field full">{t("Programme Tutor")}<input value={settings.tutor.command} disabled={busy || tutorStatus.connected} onChange={(event) => editTutor({ command: event.target.value })} placeholder={t("tutor-mcp")} /><small>{t("Le programme doit être disponible dans le même environnement que votre agent : Windows ou la distribution WSL choisie.")}</small></label><label className="field full">{t("Arguments du programme")}<input className="mono" value={argsText} disabled={busy || tutorStatus.connected} onChange={(event) => { tutorEdited.current = true; setArgsText(event.target.value); }} placeholder={t("[\"--local\"]")} /><small>{t("Tableau JSON : chaque élément correspond à un argument distinct.")}</small></label></>}
      </div>
      {settings.tutor.mode === 'remote' && !tutorStatus.connected && <><button className="button secondary" disabled={busy || !settings.tutor.url.trim()} onClick={() => void props.onIdentityAction('Vérification de l’institution…', async () => { setInstitution(await props.api.discoverInstitution(settings.tutor.url)); })}>{t("Vérifier l’institution")}</button>{institution && <section className="institution-preview" aria-label={t("Institution vérifiée")}><h3>{institution.name}</h3><p className="mono">{institution.issuer}</p><p className="subtle-note">{t("Institution :")} {institution.institution_id}</p><p className="subtle-note">{t("Les actions disponibles dépendent de vos droits dans cette institution.")}</p></section>}</>}
      {argsError && <p role="alert" className="notice error">{t(argsError)}</p>}
      {tutorStatus.message && <p className="subtle-note">{t(tutorStatus.message)}</p>}
      {!tutorStatus.connected && tutorBlockedReason && <p id="tutor-connect-help" className="subtle-note">{tutorBlockedReason}</p>}
      <div className="connection-actions"><span className="inline-status">{t("Votre accès Tutor est distinct de votre compte fournisseur.")}</span>{tutorStatus.connected ? <button className="button secondary" disabled={busy} onClick={props.onDisconnectTutor}>{t("Déconnecter Tutor")}</button> : <button className="button primary" disabled={!!tutorBlockedReason} aria-describedby={tutorBlockedReason ? 'tutor-connect-help' : undefined} onClick={() => { const next = preparedSettings(); if (next) { tutorEdited.current = false; props.onConnectTutor(next, authoring); } }}>{settings.tutor.mode === 'remote' ? <>{t("Connecter avec ma clé")}<Icon name="arrow" size={14} /></> : <>{t("Vérifier Tutor local")}<Icon name="arrow" size={14} /></>}</button>}</div>
      <button className="link-button save-connection" disabled={busy || tutorStatus.connected} onClick={()=>{const next=preparedSettings();if(next){tutorEdited.current=false;props.onSave(next);}}}>{t("Enregistrer la connexion Tutor")}</button>
      </details>
    </section>}
  </>;
}
