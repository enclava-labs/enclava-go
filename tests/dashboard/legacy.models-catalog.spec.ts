import { Page } from '@playwright/test';
import { test, expect, sessionApi, seedData } from './fixtures';

// models-catalog.spec.ts — the Models tab (default / available / fallback)
// regression: every control PUTs /api/v1/models/settings/{model} with CSRF
// through Enclava.api and the table re-renders from stored state. The stack
// is shared, so the seeded settings are restored through the API in a
// finally no matter which assertion trips.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

interface ModelEntry {
  model: string;
  available: boolean;
  default: boolean;
  fallback_model: string;
}

async function login(page: Page) {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error('ADMIN_EMAIL/ADMIN_PASSWORD are unset — run through scripts/dashboard_e2e.sh');
  }
  await page.context().clearCookies();
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', ADMIN_EMAIL);
  await page.fill('input[name="password"]', ADMIN_PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
  await page.context().storageState({ path: 'tests/dashboard/.auth/admin.json' });
}

function put(page: Page, model: string) {
  return page.waitForResponse(
    r => r.url().endsWith(`/api/v1/models/settings/${encodeURIComponent(model)}`) && r.request().method() === 'PUT',
  );
}

test('Models tab changes availability, default and fallback and they persist', async ({ page }) => {
  await login(page);
  const { chatModel, altModel } = seedData();
  const res = await page.goto(`${BASE}/dashboard/models/catalog`);
  if (!res || res.status() !== 200) {
    throw new Error(`the models catalog page is not served (status ${res ? res.status() : 'none'})`);
  }

  try {
    const rowChat = page.locator(`tr[data-model-row="${chatModel}"]`);
    const rowAlt = page.locator(`tr[data-model-row="${altModel}"]`);
    await expect(rowChat).toHaveCount(1);
    await expect(rowAlt).toHaveCount(1);
    await expect(page.locator('#models-catalog')).toContainText('Available to people');

    // Toggle Available off on the alt model.
    let saved = put(page, altModel);
    await rowAlt.locator('[data-models-available]').uncheck();
    expect((await saved).status()).toBe(200);
    await expect(rowAlt.locator('[data-models-available]')).not.toBeChecked();

    // Make the alt model the tenant default.
    saved = put(page, altModel);
    await rowAlt.locator('[data-models-default]').check();
    expect((await saved).status()).toBe(200);
    await expect(rowAlt.locator('[data-models-default]')).toBeChecked();

    // Set the chat model's fallback to the alt model.
    saved = put(page, chatModel);
    await rowChat.locator('[data-models-fallback]').selectOption(altModel);
    expect((await saved).status()).toBe(200);
    await expect(rowChat.locator('[data-models-fallback]')).toHaveValue(altModel);

    // Persisted: a fresh GET of the settings API shows all three writes.
    const settings = await sessionApi(page, 'GET', '/api/v1/models/settings');
    expect(settings.status()).toBe(200);
    const payload = (await settings.json()) as { models: ModelEntry[] };
    const alt = payload.models.find(m => m.model === altModel);
    const chat = payload.models.find(m => m.model === chatModel);
    expect(alt).toMatchObject({ available: false, default: true });
    expect(chat).toMatchObject({ fallback_model: altModel });
  } finally {
    // Restore the seeded state (available, no default, no fallback) — later
    // specs chat on the seeded models and must not inherit a hidden default.
    await sessionApi(page, 'PUT', `/api/v1/models/settings/${encodeURIComponent(altModel)}`, { available: true, default: false });
    await sessionApi(page, 'PUT', `/api/v1/models/settings/${encodeURIComponent(chatModel)}`, { fallback_model: '' });
  }
});
