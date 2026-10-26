import type { MetadataRoute } from 'next';
import { installationGuides } from '@/lib/installation-guides';
import { learningGuides } from '@/lib/learning-guides';

export const dynamic = 'force-static';

export default function sitemap(): MetadataRoute.Sitemap {
  return ['/', '/download/', '/docs/', '/guide/', '/mentions-legales/',
    ...installationGuides.map(guide => `/docs/${guide.slug}/`),
    ...learningGuides.map(guide => `/docs/${guide.slug}/`),
  ].map(path => ({ url: `https://tutor-mcp.dev${path}`, changeFrequency: 'monthly', priority: path === '/' ? 1 : path === '/mentions-legales/' ? 0.3 : 0.8 }));
}
