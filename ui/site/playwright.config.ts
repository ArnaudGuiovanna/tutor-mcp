import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  testMatch: '**/site.spec.ts',
  workers: 2,
  timeout: 30000,
  use: {
    baseURL: 'http://127.0.0.1:18765',
    headless: true,
    launchOptions: { executablePath: process.env.CHROME_BIN || (process.env.CI ? undefined : '/usr/bin/google-chrome'), args: ['--no-sandbox'] },
    screenshot: 'only-on-failure',
  },
  webServer: {
    command: 'python3 -m http.server 18765 --bind 127.0.0.1 --directory out',
    url: 'http://127.0.0.1:18765',
    reuseExistingServer: !process.env.CI,
    timeout: 10000,
    stdout: 'ignore',
    stderr: 'ignore',
  },
});
