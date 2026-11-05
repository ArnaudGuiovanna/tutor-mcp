import { useEffect, useRef } from 'react';

export interface PendingResponseRender {
  conversationId: string;
  assistantMessageId: string;
  operationId?: string;
  startedAt: number;
}
export interface ResponseRenderTiming {
  version: 1;
  conversationId: string;
  assistantMessageId: string;
  operationId?: string;
  elapsedMs: number;
  recordedAt: string;
  uiRenderMeasured: true;
  measurement: 'after-two-animation-frames';
}

const measurements: ResponseRenderTiming[] = [];
declare global {
  interface Window {
    tutorResponseTimings?: { export: () => string; clear: () => void };
  }
}

/** Local, bounded diagnostics. No prompt, answer, rubric, endpoint or credential is collected. */
export function installResponseTimingCollector() {
  const collector = Object.freeze({
    export: () => JSON.stringify({ version: 1, measurement: 'dom-render-marker', samples: measurements }, null, 2),
    clear: () => { measurements.length = 0; },
  });
  window.tutorResponseTimings = collector;
  return () => { if (window.tutorResponseTimings === collector) delete window.tutorResponseTimings; };
}

/** Two animation frames follow the DOM commit; this is not proof of a physical screen paint. */
export function ResponseRendered({ timing }: { timing: PendingResponseRender }) {
  const marker = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    let frame = 0, done = false;
    const schedule = () => {
      if (done || frame || document.visibilityState !== 'visible') return;
      frame = requestAnimationFrame(() => {
        frame = requestAnimationFrame(() => {
          frame = 0;
          if (done || !marker.current?.isConnected || document.visibilityState !== 'visible') return;
          done = true;
          if (measurements.some(item => item.conversationId === timing.conversationId && item.assistantMessageId === timing.assistantMessageId)) return;
          const measurement: ResponseRenderTiming = {
            version: 1, conversationId: timing.conversationId, assistantMessageId: timing.assistantMessageId,
            ...(timing.operationId ? { operationId: timing.operationId } : {}),
            elapsedMs: Math.max(0, performance.now() - timing.startedAt), recordedAt: new Date().toISOString(),
            uiRenderMeasured: true, measurement: 'after-two-animation-frames',
          };
          measurements.push(measurement);
          if (measurements.length > 100) measurements.shift();
          window.dispatchEvent(new CustomEvent<ResponseRenderTiming>('tutor:response-rendered', { detail: { ...measurement } }));
        });
      });
    };
    schedule(); document.addEventListener('visibilitychange', schedule);
    return () => { done = true; cancelAnimationFrame(frame); document.removeEventListener('visibilitychange', schedule); };
  }, [timing]);
  return <span ref={marker} aria-hidden="true"/>;
}
