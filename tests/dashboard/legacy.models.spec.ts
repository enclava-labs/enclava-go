import { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// models.spec.ts — the Models + Pricing Set-price regression (OA-27): the
// old pricing.js was never served, so the popover never submitted. This spec
// proves the REAL flow in Chromium: the popover POSTs the full
// adminPricingCreateRequest contract (the price set the cell shows — the
// model set for an unpriced cell —, the cell's capability + unit, integer
// micros, unit_quantity 1000000) with the CSRF header, and each save shows
// in its own cell on the re-render — never "Not priced" after a save. Change
// reopens a priced cell prefilled with its rate and edits it in place.
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

  const firstSet = page.locator(`tr[data-price-row="${created}|${UNPRICED}|chat/completions"] button[data-action="models:price"][data-unit="input"]`).first();
  if ((await firstSet.count()) === 0) {
    throw new Error(
      `the fixture row ${created}|${UNPRICED} shows no unpriced input cell — ` +
        'the pricing matrix did not render the new endpoint binding',
    );
  }

  const endpoint = created;
  const model = UNPRICED;
  const capability = (await firstSet.getAttribute('data-capability')) as string;
  if (!capability) {
    throw new Error(
      'Set price control carries no cell binding (data-endpoint/data-model/data-capability) — ' +
        'the popover cannot build the pricing contract',
    );
  }

  const row = page.locator(`tr[data-price-row="${endpoint}|${model}|${capability}"]`);
  await expect(row).toHaveCount(1);

  // Intercept (not mock): every POST must carry the exact contract.
  const posts: { body: PricingBody; headers: Record<string, string> }[] = [];
  await page.route('**/api/v1/admin/pricing', async (route) => {
    posts.push({
      body: route.request().postDataJSON() as PricingBody,
      headers: route.request().headers(),
    });
    await route.continue();
  });

  // Each save must show in its own cell right away: the regression wrote a
  // one-unit endpoint price the whole-set rule ignores, so the cell kept
  // reading "Not priced" after "Price saved".
  for (const [i, unit] of ['input', 'cached_input', 'output'].entries()) {
    const setButton = row.locator(`button[data-action="models:price"][data-unit="${unit}"]`).first();
    await setButton.click();
    await page.fill('#models-price-rate', '2.5');
    await page.fill('#models-price-currency', 'usd');
    await page.click('[data-models-price-form] button[type="submit"]');

    await expect
      .poll(() => posts.length, { timeout: 10_000 })
      .toBeGreaterThan(i);
    const captured = posts[posts.length - 1];
    expect(captured.body).toEqual({
      subject_type: 'model',
      subject_id: model,
      capability,
      unit,
      rate_micros: 2500000,
      unit_quantity: 1000000,
      currency: 'USD', // the module upper-cases the input
    } satisfies PricingBody);
    expect(captured.headers['x-csrf-token']).toBeTruthy();

    // The success path closes the popover and re-renders the section; the
    // saved cell is priced before the next unit is touched.
    await expect(page.locator('#pop')).toBeHidden({ timeout: 10_000 });
    await expect(row.locator(`td[data-price-cell="${endpoint}|${unit}"]`)).toContainText('$2.50', { timeout: 10_000 });
  }

  if (posts.length === 0) {
    throw new Error('the Set price popover never submitted — the pricing.js dead-button regression');
  }

  // Persisted outcome: the re-rendered row shows the posted rate and no
  // refusal copy — a dead button would leave the row "Not priced".
  await expect(row).not.toContainText('Not priced — requests refused', { timeout: 15_000 });
  await expect(row).toContainText('$2.50');
  await expect(row).toContainText('via model');

  // Change: the priced cell reopens prefilled, names the price set the save
  // lands in, and the edited rate replaces the old one in that cell — a
  // sub-cent per-1M price stays exact end to end.
  const inputCell = row.locator(`td[data-price-cell="${endpoint}|input"]`);
  await inputCell.locator('button[data-action="models:price"]').click();
  await expect(page.locator('#models-price-rate')).toHaveValue('2.5');
  await expect(page.locator('#pop')).toContainText(`Model price — also applies to ${model}`);
  await page.fill('#models-price-rate', '0.001');
  await page.click('[data-models-price-form] button[type="submit"]');
  await expect(inputCell).toContainText('$0.001', { timeout: 10_000 });
  expect(posts[posts.length - 1].body).toMatchObject({ subject_type: 'model', subject_id: model, unit: 'input', rate_micros: 1000, unit_quantity: 1000000 });
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
