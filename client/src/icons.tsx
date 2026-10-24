import type { CSSProperties } from 'react';

type IconName = 'sun' | 'book' | 'chat' | 'plug' | 'arrow' | 'check' | 'close' | 'refresh' | 'pause' | 'spark' | 'shield' | 'external' | 'clock' | 'plus' | 'chevron';

const paths: Record<IconName, React.ReactNode> = {
  sun: <><circle cx="12" cy="12" r="4" /><path d="M12 2v2m0 16v2M2 12h2m16 0h2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" /></>,
  book: <><path d="M12 5.5C9 3.5 5.5 3.5 3 4.5v15c3-1 6-1 9 1 3-2 6-2 9-1v-15c-2.5-1-6-1-9 1Z" /><path d="M12 5.5v15" /></>,
  chat: <path d="M20 11.5a8 8 0 0 1-8 8 9 9 0 0 1-4-.9L3 20l1.4-4.8a8 8 0 1 1 15.6-3.7Z" />,
  plug: <><path d="M8 3v5m8-5v5M6 8h12v3a6 6 0 0 1-6 6v4M4 8h16" /></>,
  arrow: <><path d="M4 12h16m-6-6 6 6-6 6" /></>,
  check: <path d="m5 12 4 4L19 6" />,
  close: <path d="m6 6 12 12M6 18 18 6" />,
  refresh: <><path d="M20 7v5h-5M4 17v-5h5" /><path d="M6 6a8 8 0 0 1 13 2M5 16a8 8 0 0 0 13 2" /></>,
  pause: <><path d="M8 5v14m8-14v14" /></>,
  spark: <><path d="m12 3 2.7 6.3L21 12l-6.3 2.7L12 21l-2.7-6.3L3 12l6.3-2.7L12 3Z" /></>,
  shield: <><path d="m12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3Z" /><path d="m8 12 3 3 5-5" /></>,
  external: <><path d="M14 3h7v7m-1-6L10 14" /><path d="M10 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5" /></>,
  clock: <><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></>,
  plus: <path d="M12 5v14M5 12h14" />,
  chevron: <path d="m9 5 7 7-7 7" />,
};

export function Icon({ name, size = 20, style }: { name: IconName; size?: number; style?: CSSProperties }) {
  return <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" style={style}>{paths[name]}</svg>;
}
