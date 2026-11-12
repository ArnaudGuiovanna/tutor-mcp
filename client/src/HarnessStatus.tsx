import { useEffect, useState } from 'react';
import { t, dateLocale } from './i18n';
import { activeLimit } from './model';
import type { AgentLimitKind, AgentReconnect, Snapshot } from './types';

/** Largest delay accepted by setTimeout. */
const maxTimeout = 2 ** 31 - 1;

/** An unexpected stop reported by the harness or a failed launch. Messages stay untranslated until rendered. */
export interface AgentLoss { message: string; stderrTail?: string }

/** Current time, refreshed every second while `active`. */
export function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [active]);
  return now;
}

/** The provider limit in force. The component re-renders by itself when it ends. */
export function useAgentLimit(status: Snapshot['agentStatus']) {
  const until = Date.parse(status?.limitedUntil || '');
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const wait = until - Date.now();
    if (!Number.isFinite(wait) || wait <= 0) return;
    const timer = setTimeout(() => setTick(value => value + 1), Math.min(wait + 1, maxTimeout));
    return () => clearTimeout(timer);
  }, [until, tick]);
  return activeLimit(status);
}

export function clockTime(at: number): string {
  return new Intl.DateTimeFormat(dateLocale(), { hour: '2-digit', minute: '2-digit' }).format(at);
}

export function remainingTime(milliseconds: number): string {
  const seconds = Math.max(0, Math.ceil(milliseconds / 1000));
  if (seconds < 60) return t('{seconds} s', { seconds });
  const minutes = Math.ceil(seconds / 60);
  if (minutes < 60) return t('{minutes} min', { minutes });
  return t('{hours} h {minutes} min', { hours: Math.floor(minutes / 60), minutes: minutes % 60 });
}

const limitLabels: Record<AgentLimitKind, string> = {
  rate_limit: 'Limite atteinte (requêtes), reprise à {time}',
  overloaded: 'Limite atteinte (surcharge du fournisseur), reprise à {time}',
  usage_limit: 'Limite atteinte (quota d’usage), reprise à {time}',
};

export function limitLabel(kind: AgentLimitKind, until: number): string {
  return t(limitLabels[kind], { time: clockTime(until) });
}

/**
 * Discreet harness state near the composer. Reconnections are driven by the
 * native supervisor; this view only reports them and offers a manual retry
 * once it has given up.
 */
export function HarnessStatus({ connected, reconnect, limit, loss, busy, onReconnect }: {
  connected: boolean; reconnect: AgentReconnect; limit: { kind: AgentLimitKind; until: number } | null;
  loss?: AgentLoss; busy: boolean; onReconnect: () => void;
}) {
  const waiting = !connected && reconnect.state === 'waiting';
  const connecting = !connected && reconnect.state === 'connecting';
  const failed = !connected && (reconnect.state === 'failed' || (reconnect.state === 'idle' && !!loss));
  const now = useNow(waiting || !!limit);
  if (!waiting && !connecting && !failed && !limit) return null;
  const nextAttempt = Date.parse(reconnect.nextAttemptAt || '');
  const cause = reconnect.state === 'failed' ? reconnect.cause || loss?.message : loss?.message;
  return <section className="harness-status" aria-label={t('État de l’agent')}>
    {waiting && <div className="harness-status-line" role="status">
      <span className="harness-status-mark" aria-hidden="true"/>
      <p>{Number.isFinite(nextAttempt)
        ? <><span aria-hidden="true">{t('Reconnexion à l’agent dans {seconds} s (tentative {attempt})', { seconds: Math.max(0, Math.ceil((nextAttempt - now) / 1000)), attempt: reconnect.attempt })}</span>
          <span className="visually-hidden">{t('Reconnexion à l’agent programmée (tentative {attempt})', { attempt: reconnect.attempt })}</span></>
        : t('Reconnexion à l’agent programmée (tentative {attempt})', { attempt: reconnect.attempt })}</p>
    </div>}
    {connecting && <div className="harness-status-line" role="status"><span className="spinner" aria-hidden="true"/><p>{t('Reconnexion à l’agent en cours…')}</p></div>}
    {failed && <div className="harness-status-line failed">
      <div>
        <p role="alert">{t('La connexion à l’agent a échoué.')}{cause ? <> {t(cause)}</> : null}</p>
        {loss?.stderrTail && <details className="harness-status-details"><summary>{t('Détails techniques')}</summary><pre>{loss.stderrTail}</pre></details>}
      </div>
      <button type="button" className="button secondary" disabled={busy} onClick={onReconnect}>{t('Reconnecter')}</button>
    </div>}
    {limit && <div className="harness-status-line limited" role="status">
      <span className="harness-status-mark" aria-hidden="true"/>
      <p>{limitLabel(limit.kind, limit.until)}<span aria-hidden="true"> · {t('dans {duration}', { duration: remainingTime(limit.until - now) })}</span></p>
    </div>}
  </section>;
}
