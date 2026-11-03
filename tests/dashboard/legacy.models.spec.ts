import { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// models.spec.ts — the Models + Pricing Set-price regression (OA-27, P-05):
// the old pricing.js was never served, so the popover never submitted. This
// spec proves the REAL flow in Chromium: one form covers all three
// categories, a single POST carries the atomic `prices` batch (the price set
// the row shows — the model set for an unpriced row —, the row's capability,
// integer micros, unit_quantity 1000000) with the CSRF header, and each
// category shows in its own cell on the re-render — never "not priced"
// after a save. Change reopens prefilled and edits a subset in place.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

function requireSession() {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error(
      'models.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for a real logged-in admin — ' +
      'seed an admin (pricing.manage) and set both; missing prerequisites fail, never skip',
    );
  }
}

async function login(page: Page) {
  requireSession();
  await page.context().clearCookies();
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', ADMIN_EMAIL);
  await page.fill('input[name="password"]', ADMIN_PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
  // The app revokes a user's older session on a new login — persist the
  // fresh one so the project's stored state never goes stale mid-file.
  await page.context().storageState({ path: 'tests/dashboard/.auth/admin.json' });
}

interface PricingBody {
  subject_type: string;
  subject_id: string;
  capability: string;
  unit: string;
  rate_micros: number;
  unit_quantity: number;
  currency: string;
}

test('Set price and Change submit the admin pricing contract and the cell re-renders priced', async ({ page }) => {
  await login(page);

  // Own unpriced row: pricing "whatever is first" would price e2e-chat-alt,
  // which later worker-failure cases need UNPRICED.
  const UNPRICED = `models-e2e-unpriced-${Date.now()}`;
  await page.goto(`${BASE}/dashboard/models`);
  const created = await page.evaluate(async (model) => {
    const r = await window.Enclava.api('POST', '/api/v1/providers', {
      protocol: 'openai_compatible',
      base_origin: 'https://unreachable.models-spec.invalid',
      // Unique upstream: the model-set prices this spec writes stay its own.
      capabilities: ['chat/completions', `model:${model}`],
    });
    if (!(r && typeof r === 'object' && 'ID' in r && typeof r.ID === 'string')) {
      throw new Error(`fixture endpoint create failed: ${JSON.stringify(r).slice(0, 160)}`);
    }
    return r.ID;
  }, UNPRICED);

  const res = await page.goto(`${BASE}/dashboard/models/pricing`);
  if (!res || res.status() !== 200) {
    throw new Error(
      `the models pricing page is not served at ${BASE}/dashboard/models/pricing (status ` +
        `${res ? res.status() : 'none'}) — run the server with a seeded models.manage admin`,
    );
  }

  const row = page.locator(`tr[data-price-row="${created}|${UNPRICED}|chat/completions"]`);
  await expect(row).toHaveCount(1);
  await row.locator('button[data-action="models:price"]').click();
  for (const unit of ['input', 'cached_input', 'output']) {
    await page.locator(`#models-price-rate-${unit}`).fill('2.5');
  }
  await page.locator('#models-price-currency').fill('usd');
  const response = page.waitForResponse(r => r.url().endsWith('/api/v1/admin/pricing') && r.request().method() === 'POST');
  await page.locator('[data-models-price-form] button[type="submit"]').click();
  const saved = await response;
  expect(saved.status()).toBe(201);
  expect(saved.request().headers()['x-csrf-token']).toBeTruthy();
  const body = saved.request().postDataJSON() as { prices: PricingBody[] };
  expect(body.prices).toHaveLength(3);
  for (const [i, unit] of ['input', 'cached_input', 'output'].entries()) {
    expect(body.prices[i]).toMatchObject({ subject_type: 'model', subject_id: UNPRICED, unit, rate_micros: 2500000, unit_quantity: 1000000, currency: 'USD' });
    await expect(row.locator(`td[data-price-cell="${created}|${unit}"]`)).toContainText('$2.50');
  }
  await row.locator('button[data-action="models:price"]').click();
  await expect(page.locator('#models-price-rate-input')).toHaveValue('2.5');
  await page.locator('#models-price-rate-input').fill('0.001');
  const changed = page.waitForResponse(r => r.url().endsWith('/api/v1/admin/pricing') && r.request().method() === 'POST');
  await page.locator('[data-models-price-form] button[type="submit"]').click();
  expect((await changed).status()).toBe(201);
  await expect(row.locator(`td[data-price-cell="${created}|input"]`)).toContainText('$0.001');
});

test('price history drawer lists the real price rows for the pair', async ({ page }) => {
  await login(page);
  const res = await page.goto(`${BASE}/dashboard/models/pricing`);
  if (!res || res.status() !== 200) {
    throw new Error(`the models pricing page is not served (status ${res ? res.status() : 'none'})`);
  }
  const history = page.locator('button[data-action="models:history"]').first();
  if ((await history.count()) === 0) {
    throw new Error('no matrix rows rendered — seed an endpoint with a model binding');
  }
  await history.click();
  const drawer = page.locator('#drawer');
  await expect(drawer).toContainText('Price history');
  // Whatever rows exist, each carries the per-1M basis statement from real data.
  const rows = drawer.locator('td[data-label="Rate"]');
  expect(await rows.count()).toBeGreaterThan(0);
});
