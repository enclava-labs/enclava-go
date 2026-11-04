import { type Page } from '@playwright/test';
import { test, expect } from './fixtures';

// sources-credentials.spec.ts — the access.manage-gated Credentials tab.
// dashboard_e2e.sh sets ENCLAVA_SECRETS_KEY and PUBLIC_BASE_URL, so the
// forms render: a Teams client secret (no provider probe) is added, shows up
// in the source wizard, and is deleted through the confirm group; the Slack
// OAuth app's redirect URI is shown for registration.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

async function login(page: Page) {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error('sources-credentials.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for the seeded admin; missing prerequisites fail, never skip');
  }
  await page.context().clearCookies();
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', ADMIN_EMAIL);
  await page.fill('input[name="password"]', ADMIN_PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
}

test('admin adds, offers and deletes a connector credential from the Credentials tab', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard/sources/credentials`);
  const tabs = page.locator('nav.tabs');
  await expect(tabs.getByRole('link', { name: 'Credentials' })).toHaveAttribute('aria-current', 'page');
  const section = page.locator('#sources-credentials');
  for (const provider of ['Slack', 'Microsoft Teams', 'GitHub', 'Notion', 'Google Drive']) {
    await expect(section.getByRole('heading', { name: provider })).toBeVisible();
  }
  await expect(section.getByText(/ENCLAVA_SECRETS_KEY/)).toHaveCount(0);
  await expect(section.locator('[data-cred-provider="slack"]')).toContainText(
    `${BASE}/dashboard/connectors/oauth/callback/slack`);

  // Empty secret: inline error, no request.
  const teams = section.locator('[data-cred-provider="teams"]');
  let posted = false;
  page.on('request', (req) => {
    if (req.method() === 'POST' && req.url().includes('/api/v1/connectors/credentials')) posted = true;
  });
  await teams.locator('[data-add-name]').fill('E2E Graph app');
  await teams.locator('[data-action="creds:add"]').click();
  await expect(teams.locator('[data-creds-error]').first()).toContainText('required');
  expect(posted).toBe(false);

  // Real add: the stored credential renders without its secret.
  await teams.locator('[data-add-secret]').fill('e2e-client-secret');
  await teams.locator('[data-action="creds:add"]').click();
  await expect(section.locator('[data-cred-provider="teams"]')).toContainText('E2E Graph app');
  await expect(page.locator('body')).not.toContainText('e2e-client-secret');

  // The add-source wizard offers it by name.
  const drawer = await page.request.get(`${BASE}/dashboard/partials/sources/drawer`);
  expect(await drawer.text()).toContain('<option value="CONNECTOR_TEAMS_E2E_GRAPH_APP">E2E Graph app</option>');

  // Delete through the shared confirm group.
  await section.locator('[data-cred-provider="teams"] [data-cred-del="1"]').click();
  await page.locator('.confirm-group').getByRole('button', { name: 'Confirm' }).click();
  await expect(section.locator('[data-cred-provider="teams"]')).not.toContainText('E2E Graph app');
});

test('oauth return notices render and error text stays escaped', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard/sources/credentials?connected=cred_1`);
  await expect(page.locator('#sources-credentials')).toContainText('Credential connected');
  await page.goto(`${BASE}/dashboard/sources/credentials?error=${encodeURIComponent('<b>bad</b>')}`);
  await expect(page.locator('#sources-credentials [role="alert"]')).toContainText('<b>bad</b>');
});
