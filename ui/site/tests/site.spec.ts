import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';

const snapshot = JSON.parse(readFileSync(new URL('../lib/release-data.json', import.meta.url), 'utf8'));
const api = 'https://api.github.com/repos/ArnaudGuiovanna/tutor-mcp/releases/latest';
const routes = ['/', '/download/', '/docs/', '/guide/', '/docs/install-client/', '/docs/local/', '/docs/hobby/', '/docs/deploy-institution/', '/docs/connect-mcp-client/', '/docs/troubleshooting/', '/docs/first-session/', '/docs/sharing-courses/', '/docs/institutions/', '/docs/backup-and-updates/', '/mentions-legales/'];

for (const width of [390, 1440]) {
  test(`all public pages load without horizontal overflow or browser errors at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.route(api, route => route.abort());
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    for (const route of routes) {
      const response = await page.goto(route);
      expect(response?.status(), route).toBe(200);
      await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), route).toBe(true);
    }
    expect(errors).toEqual([]);
  });
}

test('mobile navigation and guide links are usable with keyboard and touch', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/');
  const menu = page.getByLabel('Open navigation menu');
  await menu.focus();
  await page.keyboard.press('Enter');
  const navigation = page.getByRole('navigation', { name: 'Mobile navigation' });
  await expect(navigation).toBeVisible();
  await navigation.getByRole('link', { name: 'User guide', exact: true }).click();
  await expect(page).toHaveURL(/\/guide\/$/);
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Bring a goal. Take a first step.');
  await page.getByRole('link', { name: 'Share a programme', exact: true }).click();
  await expect(page.locator('#share')).toBeInViewport();
});

test('downloads retain verified fallback, switch operating system and architecture, and copy the matching command', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await page.route(api, route => route.abort());
  await page.goto('/download/');
  await expect(page.getByRole('link', { name: /Download for Windows x64/ })).toHaveAttribute('href', snapshot.client.url);
  await page.getByLabel('Operating system').selectOption('windows');
  await page.getByLabel('Processor').selectOption('arm64');
  await expect(page.getByRole('link', { name: 'Download server' })).toHaveAttribute('href', /tutor-mcp_windows_arm64\.zip$/);
  await page.getByRole('button', { name: 'Copy PowerShell' }).click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toContain(`install-tutor.ps1 -Version ${snapshot.version}`);
  await page.getByLabel('Operating system').selectOption('darwin');
  await expect(page.getByRole('link', { name: 'Download server' })).toHaveAttribute('href', /tutor-mcp_darwin_arm64\.tar\.gz$/);
});

test('latest stable release refreshes server and ACP download links', async ({ page }) => {
  const version = 'v9.0.0';
  await page.route(api, route => route.fulfill({ json: {
    tag_name: version, draft: false, prerelease: false, published_at: '2026-10-26T12:00:00Z',
    assets: snapshot.assets.map((asset: { name: string; size: number }) => {
      const name = asset.name.startsWith('Tutor_ACP_') ? 'Tutor_ACP_2.0.0_windows_x64-setup.exe' : asset.name;
      return { name, size: asset.size, browser_download_url: `https://github.com/ArnaudGuiovanna/tutor-mcp/releases/download/${version}/${name}` };
    }),
  } }));
  await page.goto('/download/');
  await expect(page.getByRole('status')).toContainText('Latest stable release checked on GitHub');
  await expect(page.getByRole('link', { name: /Download for Windows x64/ })).toHaveAttribute('href', /v9\.0\.0\/Tutor_ACP_2\.0\.0_windows_x64-setup\.exe$/);
  await expect(page.getByRole('link', { name: 'Download server' })).toHaveAttribute('href', /v9\.0\.0\/tutor-mcp_linux_amd64\.tar\.gz$/);
});

test('visual references for the home page and the institution section', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto('/');
  await page.screenshot({ path: 'test-results/home-desktop.png' });
  await page.locator('#institutions').screenshot({ path: 'test-results/institution-desktop.png' });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/');
  await page.screenshot({ path: 'test-results/home-mobile.png' });
  await page.route(api, route => route.abort());
  await page.goto('/download/');
  await page.screenshot({ path: 'test-results/download-mobile.png', fullPage: true });
});
