import { test, expect, Page } from '@playwright/test';

// models.spec.ts — the Models + Pricing Set-price regression (OA-27): the
// old pricing.js was never served, so the popover never submitted. This spec
// proves the REAL flow in Chromium: the popover POSTs the full
// adminPricingCreateRequest contract (subject_type endpoint, the cell's
// capability + unit, integer micros, unit_quantity 1000000) with the CSRF
// header, and the matrix re-renders with the persisted rate — the row's
// required units end up priced, never "Not priced".
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
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', ADMIN_EMAIL);
  await page.fill('input[name="password"]', ADMIN_PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
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

test('Set price submits the admin pricing contract and the cell re-renders priced', async ({ page }) => {
  await login(page);

  const res = await page.goto(`${BASE}/dashboard/models/pricing`);
  if (!res || res.status() !== 200) {
    throw new Error(
      `the models pricing page is not served at ${BASE}/dashboard/models/pricing (status ` +
        `${res ? res.status() : 'none'}) — run the server with a seeded models.manage admin`,
    );
  }

  const firstSet = page.locator('button[data-action="models:price"]').first();
  if ((await firstSet.count()) === 0) {
    throw new Error(
      'no unpriced matrix cell found — seed a model binding whose route is missing at ' +
        'least one required unit price (input/cached_input/output for chat/completions)',
    );
  }

  const endpoint = (await firstSet.getAttribute('data-endpoint')) as string;
  const model = (await firstSet.getAttribute('data-model')) as string;
  const capability = (await firstSet.getAttribute('data-capability')) as string;
  if (!endpoint || !model || !capability) {
    throw new Error(
      'Set price control carries no cell binding (data-endpoint/data-model/data-capability) — ' +
        'the popover cannot build the pricing contract',
    );
  }

  const row = page.locator(`tr[data-price-row="${endpoint}|${model}"]`);
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

  // Price the row's missing units until none is left "Not priced" — the
  // whole-set display only shows the new rates once the endpoint set covers
  // every required unit, which is exactly what the matrix promises.
  for (let i = 0; i < 3; i++) {
    const setButton = row.locator('button[data-action="models:price"]').first();
    if ((await setButton.count()) === 0) break;
    const unit = (await setButton.getAttribute('data-unit')) as string;
    await setButton.click();
    await page.fill('#models-price-rate', '2500000');
    await page.fill('#models-price-currency', 'usd');
    await page.click('[data-models-price-form] button[type="submit"]');

    await expect
      .poll(() => posts.length, { timeout: 10_000 })
      .toBeGreaterThan(i);
    const captured = posts[posts.length - 1];
    expect(captured.body).toEqual({
      subject_type: 'endpoint',
      subject_id: endpoint,
      capability,
      unit,
      rate_micros: 2500000,
      unit_quantity: 1000000,
      currency: 'USD', // the module upper-cases the input
    } satisfies PricingBody);
    expect(captured.headers['x-csrf-token']).toBeTruthy();

    // The success path closes the popover and re-renders the section; wait
    // for the swap so the next iteration resolves fresh markup.
    await expect(page.locator('#pop')).toBeHidden({ timeout: 10_000 });
  }

  if (posts.length === 0) {
    throw new Error('the Set price popover never submitted — the pricing.js dead-button regression');
  }

  // Persisted outcome: the re-rendered row shows the posted rate and no
  // refusal copy — a dead button would leave the row "Not priced".
  await expect(row).not.toContainText('Not priced — requests refused', { timeout: 15_000 });
  await expect(row).toContainText('$2.50');
  await expect(row).toContainText('via endpoint');
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
