import { test, expect, Page } from '@playwright/test';

// usage.spec.ts — Usage & billing page (09-08, OA-29) in a real Chromium.
// Proves the 08-07 budget write contracts end to end: a create POST carries
// the full contract and the row appears re-rendered from the server; a limit
// edit sends a PATCH body of exactly {limit_value} (nothing else — liability
// can never be reset from the drawer) and the re-rendered row shows the new
// limit. Persistence is asserted against the real backend response, never a
// toast claim.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

function requireSession() {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error(
      'usage.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for a real logged-in session — ' +
      'seed the admin user and set both; missing prerequisites fail, never skip',
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

async function openBudgets(page: Page) {
  const res = await page.goto(`${BASE}/dashboard/usage/budgets`);
  if (!res || res.status() !== 200) {
    throw new Error(
      `the Usage & billing page (09-08) is not served at ${BASE}/dashboard/usage/budgets — got ` +
      `status ${res ? res.status() : 'none'}`,
    );
  }
}

// money() renders micros with the currency symbol once (e.g. 25000000 → $25.00).
function microsDisplay(micros: number): string {
  const cents = Math.round(micros / 10000);
  return '$' + (cents / 100).toFixed(2);
}

test('admin creates a budget via the drawer; the POST carries the full 08-07 contract and the row persists', async ({ page }) => {
  await login(page);
  await openBudgets(page);

  const name = `e2e-budget-${Date.now()}`;
  const limitMicros = 25_000_000; // $25.00

  let postBody: Record<string, unknown> | null = null;
  let postStatus: number | null = null;
  await page.route('**/api/v1/admin/budgets', async (route) => {
    if (route.request().method() !== 'POST') return route.continue();
    postBody = route.request().postDataJSON();
    const response = await route.fetch();
    postStatus = response.status();
    await route.fulfill({ response });
  });

  await page.getByRole('button', { name: 'New budget' }).click();
  const drawer = page.locator('#drawer');
  await expect(drawer).toContainText('New budget');

  await drawer.locator('#budget-name').fill(name);
  await drawer.locator('#budget-subject-type').selectOption('tenant');
  await drawer.locator('#budget-kind').selectOption('amount');
  await drawer.locator('#budget-limit-value').fill(String(limitMicros));
  await drawer.locator('#budget-currency').fill('USD');
  await drawer.locator('#budget-period').selectOption('monthly');
  await drawer.getByRole('button', { name: 'Create budget' }).click();

  // The write must have reached the real backend — the toast resolves the
  // promise, the route handler's status lands first.
  await expect.poll(() => postStatus, { timeout: 10_000 }).toBe(201);
  expect(postBody).toBeTruthy();
  // The full 08-07 contract: name, subject, kind, integer limit, currency,
  // period and an RFC3339 starts_at.
  expect(postBody!.name).toBe(name);
  expect(postBody!.subject_type).toBe('tenant');
  expect(postBody!.kind).toBe('amount');
  expect(postBody!.limit_value).toBe(limitMicros);
  expect(postBody!.currency).toBe('USD');
  expect(postBody!.period).toBe('monthly');
  expect(String(postBody!.starts_at)).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);

  // The refresh re-render comes from the server — the row shows the new limit.
  const row = page.locator('#usage-budgets tr', { hasText: name });
  await expect(row).toContainText(microsDisplay(limitMicros));
});

test('admin edits a budget limit via the drawer; the PATCH body is exactly {limit_value} and the row re-renders with it', async ({ page }) => {
  await login(page);
  await openBudgets(page);

  // Seed a real budget through the same API the page writes through.
  const seedName = `e2e-edit-${Date.now()}`;
  const created = await page.request.post(`${BASE}/api/v1/admin/budgets`, {
    data: {
      name: seedName,
      subject_type: 'tenant',
      kind: 'amount',
      limit_value: 10_000_000,
      currency: 'USD',
      period: 'monthly',
      starts_at: new Date().toISOString(),
    },
  });
  if (created.status() !== 201) {
    throw new Error(
      `could not seed a budget for the edit scenario — POST /api/v1/admin/budgets returned ` +
      `${created.status()}: ${await created.text()}`,
    );
  }
  const view = await created.json();
  const budgetId: string = view.budget?.id;
  if (!budgetId) throw new Error(`seeded budget has no id: ${JSON.stringify(view)}`);
  await page.reload();
  const seededRow = page.locator('#usage-budgets tr', { hasText: seedName });
  await expect(seededRow).toContainText('$10.00');

  const newLimit = 50_000_000; // $50.00
  let patchBody: Record<string, unknown> | null = null;
  let patchStatus: number | null = null;
  await page.route(`**/api/v1/admin/budgets/${budgetId}`, async (route) => {
    if (route.request().method() !== 'PATCH') return route.continue();
    patchBody = route.request().postDataJSON();
    const response = await route.fetch();
    patchStatus = response.status();
    await route.fulfill({ response });
  });

  await seededRow.getByRole('button', { name: 'Edit limit' }).click();
  const drawer = page.locator('#drawer');
  await expect(drawer).toContainText('Edit budget limit');
  await expect(drawer).toContainText(seedName);

  await drawer.locator('#budget-limit').fill(String(newLimit));
  await drawer.getByRole('button', { name: 'Save limit' }).click();

  // Exactly {limit_value} — a wider body could reset liability or rewrite
  // the budget's window/kind.
  await expect.poll(() => patchStatus, { timeout: 10_000 }).toBe(200);
  expect(patchBody).toEqual({ limit_value: newLimit });

  // The re-rendered row (server state, not the toast) shows the new limit.
  await expect(seededRow).toContainText(microsDisplay(newLimit));
});
