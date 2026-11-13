import { Icon } from './icons';
import { t } from './i18n';

/**
 * Reading in progress: an indicator with a visible label, or a compact
 * skeleton whose label stays available to screen readers.
 * Labels are French source strings, translated here.
 */
export function LoadingState({ label, skeleton = 0, className = '' }: { label: string; skeleton?: number; className?: string }) {
  if (skeleton > 0) return <div className={`load-state load-skeleton ${className}`} role="status">
    <span className="visually-hidden">{t(label)}</span>
    {Array.from({ length: skeleton }, (_, index) => <span key={index} className="load-skeleton-line" aria-hidden="true"/>)}
  </div>;
  return <p className={`load-state ${className}`} role="status"><span className="spinner" aria-hidden="true"/>{t(label)}</p>;
}

/** Failed read: the cause, then an explicit retry. */
export function LoadError({ message, detail, busy = false, onRetry, className = '' }: { message: string; detail?: string; busy?: boolean; onRetry: () => void; className?: string }) {
  return <div className={`load-error-state ${className}`} role="alert">
    <p>{t(message)}{detail ? <> <span className="load-error-detail">{t(detail)}</span></> : null}</p>
    <button type="button" className="button secondary" disabled={busy} onClick={onRetry}><Icon name="refresh" size={14}/>{t('Réessayer')}</button>
  </div>;
}
