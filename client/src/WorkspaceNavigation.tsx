import { useEffect, useState } from 'react';
import { Icon } from './icons';
import { t } from './i18n';
import type { Conversation, Snapshot, Training } from './types';

const sidebarPreference = 'tutor.workspace.sidebar';
export function useWorkspaceSidebar() {
  const [wide, setWide] = useState(() => window.innerWidth >= 1100);
  const [preference, setPreference] = useState<boolean | undefined>(() => {
    try { const value = localStorage.getItem(sidebarPreference); return value === null ? undefined : value === 'open'; } catch { return undefined; }
  });
  const [overlay, setOverlay] = useState(false);
  useEffect(() => {
    const resize = () => { setWide(window.innerWidth >= 1100); setOverlay(false); };
    window.addEventListener('resize', resize); return () => window.removeEventListener('resize', resize);
  }, []);
  const open = wide ? preference ?? true : overlay;
  useEffect(()=>{
    if(!overlay || wide)return;
    const escape=(event:KeyboardEvent)=>{if(event.key==='Escape'){setOverlay(false);document.querySelector<HTMLButtonElement>('[aria-controls="workspace-sidebar"]')?.focus();}};
    window.addEventListener('keydown',escape);return()=>window.removeEventListener('keydown',escape);
  },[overlay,wide]);
  const closeOverlay = () => { if (!wide) setOverlay(false); };
  function toggle() {
    if (!wide) { setOverlay(value => !value); return; }
    setPreference(!open);
    try { localStorage.setItem(sidebarPreference, open ? 'closed' : 'open'); } catch { /* Layout remains usable when storage is unavailable. */ }
  }
  return { open, toggle, closeOverlay, overlay:!wide && open };
}

export function TrainingColor({ name }: {name:string}) {
  return <span className={`training-color ${/\bgo\b/i.test(name) ? 'cyan' : ''}`} aria-hidden="true"/>;
}

export function WorkspaceNavigation({state,page,settingsPage,conversation,trainings,libraryError,locked,sidebarOpen,onNavigate,onNew,onConversation,onTraining,onSearch,onPractice}: {
  state:Snapshot; page:string; settingsPage:boolean; conversation?:Conversation; trainings:Training[]; libraryError:boolean; locked:boolean; sidebarOpen:boolean;
  onNavigate:(page:'conversation'|'formations'|'planning'|'account'|'settings'|'agents')=>void; onNew:()=>void; onConversation:(id:string)=>void; onTraining:(id:string)=>void; onSearch:()=>void; onPractice:()=>void;
}) {
  const words = state.profile?.label.trim().split(/\s+/) || [];
  const initials = (words.length > 1 ? words.slice(0,2).map(word=>Array.from(word)[0]).join('') : Array.from(words[0] || 'T').slice(0,2).join('')).toLocaleUpperCase();
  const spaces = [{page:'conversation',label:'Conversation',icon:'chat'}, {page:'formations',label:'Formations',icon:'book'}, {page:'planning',label:'Planning',icon:'calendar'}] as const;
  return <>
    <nav className="workspace-rail" aria-label={t('Navigation principale')}>
      {spaces.map(space => <button key={space.page} className="rail-button" aria-label={t(space.label)} title={t(space.label)} aria-current={page === space.page ? 'page' : undefined} onClick={() => onNavigate(space.page)}><Icon name={space.icon} size={18}/></button>)}
      <button className="rail-button" aria-label={t('Quiz et flashcards')} title={t('Quiz et flashcards')} onClick={onPractice}><Icon name="copy" size={18}/></button>
      <div className="rail-bottom">
        <button className="rail-button" aria-label={t('Paramètres')} title={t('Paramètres')} aria-current={settingsPage ? 'page' : undefined} onClick={() => onNavigate('settings')}><Icon name="gear" size={18}/></button>
        <button className="rail-button account-avatar" aria-label={t('Mon compte')} title={state.profile?.label || t('Mon compte')} aria-current={page === 'account' ? 'page' : undefined} onClick={() => onNavigate('account')}>{initials}</button>
      </div>
    </nav>
    <aside id="workspace-sidebar" className="sidebar" aria-label={t('Barre latérale')} hidden={!sidebarOpen}>
      <header className="sidebar-heading">
        <button className="workspace-brand" aria-label={t('Ouvrir mon espace')} onClick={() => onNavigate('account')}>Tutor-MCP<Icon name="chevron" size={12}/></button>
        <button className="workspace-icon" aria-label={t('Rechercher')} title={t('Rechercher (Ctrl K)')} aria-keyshortcuts="Control+K Meta+K" onClick={onSearch}><Icon name="search" size={18}/></button>
      </header>
      <button className={`new-conversation ${page === 'conversation' && !conversation?.messages.length ? 'active' : ''}`} aria-label={t('Nouvelle conversation')} title={t('Nouvelle conversation (Ctrl N)')} aria-keyshortcuts="Control+N Meta+N" disabled={locked} onClick={onNew}><Icon name="edit" size={16}/><span>{t('Nouvelle conversation')}</span><span className="shortcut" aria-hidden="true">Ctrl N</span></button>
      <section className="sidebar-section" aria-labelledby="sidebar-trainings"><h2 className="sidebar-caption" id="sidebar-trainings">{t('Formations')}</h2>
        {trainings.slice(0,5).map(training => <button key={training.id} className="sidebar-training" title={training.name} onClick={() => onTraining(training.id)}><TrainingColor name={training.name}/><span className="training-name">{training.name}</span><small>{t(training.progress?.some(item => item.last_review) ? 'En cours' : 'Nouveau')}</small></button>)}
        {!trainings.length && <p className="sidebar-empty">{t(!state.tutorStatus.connected ? 'Vos formations après connexion' : libraryError ? 'Formations indisponibles' : 'Aucune formation suivie')}</p>}
      </section>
      <section className="sidebar-recents" aria-labelledby="sidebar-recents"><h2 className="sidebar-caption" id="sidebar-recents">{t('Récents')}</h2>
        <nav className="conversation-history" aria-label={t('Historique des conversations')}>
          {[...state.conversations].sort((a,b) => b.updatedAt.localeCompare(a.updatedAt)).map(item => <button key={item.id} className="history-item" title={item.title} aria-current={page === 'conversation' && conversation?.id === item.id ? 'page' : undefined} disabled={locked} onClick={() => onConversation(item.id)}><span>{item.title || t('Nouvelle conversation')}</span></button>)}
        </nav>
      </section>
      <div className="mcp-status" role="status"><span className={`dot ${state.tutorStatus.connected ? 'online' : ''}`} aria-hidden="true"/><span>{t(state.tutorStatus.connected ? 'Serveur MCP connecté' : 'Serveur MCP non connecté')}</span>{!state.tutorStatus.connected && <button onClick={() => onNavigate('agents')}>{t('Connecter')}</button>}</div>
    </aside>
  </>;
}
