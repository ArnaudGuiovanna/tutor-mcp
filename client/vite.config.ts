import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  clearScreen: false,
  server: { port: 1420, strictPort: true },
  envPrefix: ['VITE_', 'TAURI_ENV_'],
  build: { target: 'es2022', sourcemap: false },
  test: {
    environment: 'jsdom',
    include: ['tests/frontend/**/*.test.ts', 'tests/frontend/**/*.test.tsx'],
    setupFiles: ['tests/frontend/setup.ts'],
    restoreMocks: true,
  },
});
