import type { ReactNode } from 'react';

export type Guide = {
  slug: string;
  title: string;
  description: string;
  category: 'Get started' | 'Learn and share' | 'Run Tutor';
  readTime: string;
  audience: string;
  sections: { id: string; title: string; content: ReactNode }[];
  sources: { label: string; path: string }[];
  related: string[];
};
