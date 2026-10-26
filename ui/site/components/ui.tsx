import type { ReactNode } from 'react';

export const REPO = 'https://github.com/ArnaudGuiovanna/tutor-mcp';
export const VERSION = 'v0.7.0';
export const CLIENT_VERSION = '0.0.1';
export const RELEASE = `${REPO}/releases/tag/${VERSION}`;
export const CLIENT_DOWNLOAD = `${REPO}/releases/download/${VERSION}/Tutor_ACP_${CLIENT_VERSION}_windows_x64-setup.exe`;

export function Wordmark({ size = 17, still = false }: { size?: number; still?: boolean }) {
  return (
    <span className="wm" style={{ fontSize: size }}>
      Tutor-MCP<span className={`cursor${still ? ' still' : ''}`} aria-hidden="true" />
    </span>
  );
}

export function Meter({ label, value, threshold = 0.85, max = 420 }: { label: string; value: number; threshold?: number; max?: number }) {
  const ok = value >= threshold;
  return (
    <div className="meter" style={{ maxWidth: max }} role="img" aria-label={`${label}: estimate ${value.toFixed(2)}, threshold ${threshold.toFixed(2)}`}>
      <div className="r"><span>{label}</span><span>{value.toFixed(2)} / {threshold.toFixed(2)}</span></div>
      <div className="track">
        <div className={`fill${ok ? ' ok' : ''}`} style={{ width: `${Math.round(value * 100)}%` }} />
        <div className="thr" style={{ left: `${Math.round(threshold * 100)}%` }} />
      </div>
    </div>
  );
}

export type Status = 'done' | 'prog' | 'review' | 'lock';
export function StatusIcon({ kind }: { kind: Status }) {
  const common = { className: 'st', viewBox: '0 0 14 14', 'aria-hidden': true } as const;
  if (kind === 'done') return <svg {...common}><circle cx="7" cy="7" r="6.2" fill="#3FCF8E" /><path d="m4.4 7.1 1.8 1.8 3.5-3.6" fill="none" stroke="#0C131F" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" /></svg>;
  if (kind === 'prog') return <svg {...common}><circle cx="7" cy="7" r="5.5" fill="none" stroke="#3D8BFF" strokeWidth="1.5" /><path d="M7 3.2A3.8 3.8 0 0 1 10.8 7 3.8 3.8 0 0 1 7 10.8z" fill="#3D8BFF" /></svg>;
  if (kind === 'review') return <svg {...common}><circle cx="7" cy="7" r="5.5" fill="none" stroke="#F0B44C" strokeWidth="1.5" /><path d="M7 3.2A3.8 3.8 0 1 1 3.2 7H7z" fill="#F0B44C" /></svg>;
  return <svg {...common}><circle cx="7" cy="7" r="5.5" fill="none" stroke="#5E6B82" strokeWidth="1.5" strokeDasharray="2.2 1.6" /></svg>;
}

export function Pill({ kind, children }: { kind?: Status; children: ReactNode }) {
  return <span className="pill">{kind && <StatusIcon kind={kind} />}{children}</span>;
}

export function Kbd({ children }: { children: ReactNode }) {
  return <span className="kbd">{children}</span>;
}

export function Providers({ size = 16 }: { size?: number }) {
  const items = [
    ['claude', 'Claude'],
    ['openai', 'ChatGPT'],
    ['gemini', 'Gemini'],
    ['mistral', 'Le Chat'],
  ];
  return (
    <>
      {items.map(([file, name]) => (
        <span key={file}><img src={`/logos/${file}.svg`} width={size} height={size} alt="" />{name}</span>
      ))}
    </>
  );
}

export function SectionTop({ n, title }: { n: string; title: string }) {
  return <div className="sec-top"><b>{n}</b> {title} <span className="dim">→</span></div>;
}
