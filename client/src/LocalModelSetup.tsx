import { useEffect, useId, useRef, useState } from 'react';
import type { ClientBridge } from './bridge';
import type { LocalModelStatus } from './types';
import { errorMessage } from './model';
import { useTranslation } from './i18n';

type Operation = 'download' | 'import' | 'cancel';
const running = (phase: LocalModelStatus['phase'] | undefined) =>
  phase === 'checking' || phase === 'downloading' || phase === 'importing';

interface LocalModelSetupProps {
  api: ClientBridge;
  busy: boolean;
  onReady?: () => Promise<void>;
}

export function LocalModelSetup({ api, busy, onReady }: LocalModelSetupProps) {
  const { t, language } = useTranslation();
  const heading = useId();
  const [status, setStatus] = useState<LocalModelStatus | null>(null);
  const [pending, setPending] = useState<Operation | 'refresh' | null>(null);
  const [error, setError] = useState('');
  const onReadyRef = useRef(onReady);
  onReadyRef.current = onReady;
  const controls = useRef<{
    run: (operation: Operation) => Promise<void>;
    refresh: () => Promise<void>;
  } | null>(null);

  useEffect(() => {
    let live = true;
    let serial = 0;
    let operationPending = false;
    let readyAnnounced = false;
    let current: LocalModelStatus | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const valid = (request: number) => live && request === serial;
    const clearTimer = () => { clearTimeout(timer); timer = undefined; };
    const schedule = () => {
      clearTimer();
      if (live && running(current?.phase)) timer = setTimeout(() => void read(false), 1000);
    };

    async function accept(next: LocalModelStatus, request: number) {
      if (!valid(request)) return;
      current = next;
      setStatus(next);
      setError('');
      schedule();
      if (next.phase !== 'ready' || !next.agentAvailable) {
        readyAnnounced = false;
      } else if (!readyAnnounced) {
        readyAnnounced = true;
        try {
          // App owns the agent list. Installing a model never selects/connects
          // an agent or changes the learner's profile or permission policy.
          await onReadyRef.current?.();
        } catch (reason) {
          if (valid(request)) {
            readyAnnounced = false;
            setError(errorMessage(reason));
          }
        }
      }
    }

    async function read(manual: boolean) {
      if (!live || operationPending) return;
      clearTimer();
      const request = ++serial;
      if (manual) setPending('refresh');
      try {
        await accept(await api.localModelStatus(), request);
      } catch (reason) {
        if (valid(request)) {
          setError(errorMessage(reason));
          schedule();
        }
      } finally {
        if (valid(request) && manual) setPending(null);
      }
    }

    async function run(operation: Operation) {
      if (!live || operationPending) return;
      operationPending = true;
      clearTimer();
      // Any read already in flight must not replace this newer command result.
      const request = ++serial;
      setPending(operation);
      setError('');
      try {
        const next = await (operation === 'download' ? api.downloadLocalModel()
          : operation === 'import' ? api.importLocalModel() : api.cancelLocalModel());
        await accept(next, request);
      } catch (reason) {
        if (valid(request)) {
          setError(errorMessage(reason));
          schedule();
        }
      } finally {
        operationPending = false;
        if (valid(request)) setPending(null);
      }
    }

    controls.current = { run, refresh: () => read(true) };
    setStatus(null);
    setError('');
    setPending(null);
    void read(false);
    return () => {
      live = false;
      ++serial;
      clearTimer();
      controls.current = null;
      // The native job continues when leaving this screen. Remounting reads it.
    };
  }, [api]);

  const active = running(status?.phase);
  const phaseText = status ? {
    missing: 'Modèle à installer.',
    checking: 'Vérification du modèle…',
    downloading: 'Téléchargement du modèle…',
    importing: 'Copie et vérification du modèle…',
    ready: 'Modèle vérifié.',
    error: 'La préparation du modèle a échoué. Vous pouvez réessayer.',
    cancelled: 'Préparation annulée. Vous pouvez réessayer.',
  }[status.phase] : 'Lecture du statut du modèle…';
  const total = status && Number.isFinite(status.total) && status.total > 0 ? status.total : undefined;
  const downloaded = status && Number.isFinite(status.downloaded) && status.downloaded >= 0
    ? Math.min(status.downloaded, total ?? status.downloaded) : undefined;
  const knownProgress = total !== undefined && downloaded !== undefined && status?.phase !== 'checking';
  const format = (bytes: number) => new Intl.NumberFormat(language === 'en' ? 'en-GB' : 'fr-FR', {
    maximumFractionDigits: 2,
  }).format(bytes / 1_000_000_000);
  const disabled = busy || !!pending || !status || !status.agentAvailable;
  const issue = error || (status?.phase === 'error' && status.error ? t(status.error) : '');

  return <section className="settings-section" aria-labelledby={heading}>
    <h2 id={heading}>{t('Qwen local (expérimental)')}</h2>
    <p>{t('Préparez le modèle une fois pour converser avec Qwen sur cet appareil.')}</p>
    <p><strong>Qwen3-4B-Instruct-2507 · Q4_K_M</strong> · {t('environ 2,5 Go')}</p>
    <p>{t('Le téléchargement utilise Hugging Face. Vous pouvez aussi choisir le GGUF exact déjà présent sur votre ordinateur : Tutor en conserve une copie vérifiée.')}</p>
    <p>{t('Le réseau sert au téléchargement du modèle. Les réponses sont ensuite calculées sur cet appareil ; le serveur Tutor peut être distant.')}</p>
    <p className="subtle-note">{t('Version CPU expérimentale : conversation et lecture des données Tutor uniquement. Les activités et évaluations ne sont pas encore disponibles. Les réponses peuvent être lentes ; la Quadro P2000 n’est pas encore qualifiée.')}</p>
    <p role="status" aria-live="polite">{pending === 'import' ? t('Choisissez un fichier dans la fenêtre de sélection.')
      : pending === 'cancel' ? t('Annulation en cours…') : t(phaseText)}</p>
    {active && <div className="progress-list"><div>
      <progress aria-label={t('Préparation du modèle')} max={total ?? 1}
        value={knownProgress ? downloaded : undefined} />
      {knownProgress && <small>{t('{downloaded} Go sur {total} Go', {
        downloaded: format(downloaded!), total: format(total!),
      })}</small>}
    </div></div>}
    {status && !status.agentAvailable && <p className="inline-callout">{t('L’agent local est absent de cette installation. Réinstallez Tutor avec son installateur complet.')}</p>}
    {issue && <p role="alert" className="notice error">{issue}</p>}
    {status?.phase === 'ready' && status.agentAvailable && <p className="inline-callout">{t('Choisissez Qwen — local (expérimental) dans la liste des agents, puis connectez-le. Vos autorisations Tutor restent applicables.')}</p>}
    <div className="button-row">
      {active ? <button type="button" className="button secondary" disabled={!!pending}
        onClick={() => void controls.current?.run('cancel')}>{t('Annuler la préparation')}</button>
        : status?.phase !== 'ready' && <>
          <button type="button" className="button primary" disabled={disabled}
            onClick={() => void controls.current?.run('download')}>{t('Télécharger le modèle')}</button>
          <button type="button" className="button secondary" disabled={disabled}
            onClick={() => void controls.current?.run('import')}>{t('Choisir un GGUF existant')}</button>
        </>}
      {!active && <button type="button" className="link-button" disabled={busy || !!pending}
        onClick={() => void controls.current?.refresh()}>{t('Actualiser le statut du modèle')}</button>}
    </div>
  </section>;
}
