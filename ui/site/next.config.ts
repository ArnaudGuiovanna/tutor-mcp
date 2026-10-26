import type { NextConfig } from 'next';

// Static export for plain file hosting: every route becomes <route>/index.html in out/.
const nextConfig: NextConfig = {
  output: 'export',
  trailingSlash: true,
  images: { unoptimized: true },
  reactStrictMode: true,
};

export default nextConfig;
