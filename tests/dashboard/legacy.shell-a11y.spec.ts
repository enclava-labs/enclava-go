import type { Page } from '@playwright/test';
import { test, expect, api, login, ADMIN_EMAIL, ADMIN_PASSWORD, BASE } from './fixtures';

// shell-a11y.spec.ts — the P-06/P-07/P-08 shell accessibility regressions:
// navigating filter chips are links carrying aria-current (never
// aria-pressed), landmarks are named and singular, scrollable table regions
// are focusable and labelled, the destructive confirm is an explicit untimed
// Confirm/Cancel group (Escape cancels, focus moves to Cancel and returns),
// and 390px pages never widen the document.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

declare global {
  interface Window {
    __fired?: boolean;
  }
}

async function open(page: Page, path: string) {
  const res = await page.goto(`${BASE}${path}`);
  if (!res || res.status() !== 200) {
    throw new Error(
      `${path} is not served (status ${res ? res.status() : 'none'}) — ` +
      'run the suite through scripts/dashboard_e2e.sh with a seeded tenant',
    );
  }
}

// mountProbe renders a synthetic data-confirm trigger at the top of the
// content column (a page region always visible and unobscured) and arms an
// enclava:confirm listener so cancel paths can assert nothing fired.
async function mountProbe(page: Page, id: string, label: string, consequence: string, click: boolean) {
  await page.evaluate(([probeId, probeLabel, probeConsequence, probeClick]) => {
    window.__fired = false;
    document.addEventListener('enclava:confirm', () => { window.__fired = true; });
    const b = document.createElement('button');
    b.id = probeId;
    b.type = 'button';
    b.className = 'btn danger';
    b.setAttribute('data-action', 'confirm');
    b.setAttribute('data-confirm', probeConsequence);
    b.textContent = probeLabel;
    const host = document.getElementById('content') || document.body;
    host.prepend(b);
    if (probeClick) b.click();
  }, [id, label, consequence, click]);
}

test('filter chips that navigate are links with aria-current, never aria-pressed', async ({ page }) => {
  for (const path of ['/dashboard/sources', '/dashboard/usage', '/dashboard/usage/audit']) {
    await open(page, path);
    await expect(page.locator('a[aria-pressed]')).toHaveCount(0);
    const chips = page.locator('a.chip');
    expect(await chips.count(), `${path} renders no navigating filter chips`).toBeGreaterThan(0);
    await expect(page.locator('a.chip[aria-current="page"]').first()).toBeVisible();
  }
});

test('landmarks are named: Primary nav, named section tabs, exactly one main', async ({ page }) => {
  await open(page, '/dashboard/usage');
  await expect(page.getByRole('navigation', { name: 'Primary', exact: true })).toHaveCount(1);
  await expect(page.locator('nav#nav')).toHaveAttribute('aria-label', 'Primary');
  await expect(page.locator('nav.tabs')).toHaveAttribute('aria-label', 'Sections');
  await expect(page.locator('main')).toHaveCount(1);
});

test('scrollable table regions are focusable, role=region and labelled', async ({ page }) => {
  await open(page, '/dashboard/sources');
  const wrap = page.locator('#sources-table .tablewrap');
  if ((await wrap.count()) === 0) {
    throw new Error('no .tablewrap on /dashboard/sources — the seeded tenant renders no source rows');
  }
  await expect(wrap).toHaveAttribute('role', 'region');
  await expect(wrap).toHaveAttribute('tabindex', '0');
  expect(await wrap.getAttribute('aria-label'), 'scroll region needs an aria-label').toBeTruthy();

  await wrap.evaluate((n: HTMLElement) => n.focus());
  await expect(wrap).toBeFocused();

  await open(page, '/dashboard/usage');
  const overviewWrap = page.locator('#usage-overview .tablewrap').first();
  await expect(overviewWrap).toHaveAttribute('role', 'region');
  await expect(overviewWrap).toHaveAttribute('tabindex', '0');
  expect(await overviewWrap.getAttribute('aria-label')).toBeTruthy();
});

test('armed confirm shows consequence, Cancel fires nothing and restores focus', async ({ page }) => {
  await open(page, '/dashboard/sources');
  await mountProbe(page, 'probe-confirm', 'Delete probe', 'Delete the probe row? Its data is gone for good.', false);

  const trigger = page.locator('#probe-confirm');
  await trigger.focus();
  await trigger.click();

  const group = page.locator('.confirm-group');
  await expect(group).toBeVisible();
  await expect(group).toContainText('Delete the probe row? Its data is gone for good.');
  await expect(group.getByRole('button', { name: 'Confirm' })).toBeVisible();
  // Focus moves into the group, onto Cancel (the safe default).
  await expect(group.getByRole('button', { name: 'Cancel' })).toBeFocused();

  await group.getByRole('button', { name: 'Cancel' }).click();
  await expect(group).toHaveCount(0);
  await expect(trigger).toBeVisible();
  await expect(trigger).toBeFocused();
  expect(await page.evaluate(() => window.__fired)).toBe(false);
});

test('Escape cancels an armed confirm with no action', async ({ page }) => {
  await open(page, '/dashboard/sources');
  await mountProbe(page, 'probe-esc', 'Revoke probe', 'Revoke the probe?', true);

  const group = page.locator('.confirm-group');
  await expect(group).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(group).toHaveCount(0);
  const trigger = page.locator('#probe-esc');
  await expect(trigger).toBeVisible();
  await expect(trigger).toBeFocused();
  expect(await page.evaluate(() => window.__fired)).toBe(false);
});

test('Confirm runs the real destructive action (budget deactivation)', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PASSWORD);
  // Tokens-kind budget: skips the currency check entirely and carries no
  // currency field. The create response is the budgetView envelope —
  // {budget: {id,...}, liability, progress} — not a bare DTO.
  const view = await api<{ budget: { id: string } }>('POST', '/api/v1/admin/budgets', {
    name: `confirm-spec-${Date.now()}`,
    subject_type: 'tenant',
    kind: 'tokens',
    limit_value: 25000000,
    period: 'monthly',
    timezone: 'UTC',
    starts_at: new Date().toISOString(),
  });
  const budgetID = view.budget.id;

  await open(page, '/dashboard/usage/budgets');
  const trigger = page.locator(`button[data-budget-id="${budgetID}"]`);
  await expect(trigger).toBeVisible();
  await trigger.click();

  const group = page.locator('.confirm-group');
  await expect(group).toBeVisible();
  const deleteReq = page.waitForRequest(
    (r) => r.method() === 'DELETE' && r.url().includes(`/api/v1/admin/budgets/${budgetID}`),
  );
  await group.getByRole('button', { name: 'Confirm' }).click();
  await deleteReq;
  // The row re-renders without the deactivate control.
  await expect(trigger).toHaveCount(0, { timeout: 15_000 });
});

test('390x844: usage audit and models pages never widen the document', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  for (const path of ['/dashboard/usage/audit', '/dashboard/models', '/dashboard/models/pricing']) {
    await open(page, path);
    const overflow = await page.evaluate(() =>
      document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow, `${path} overflows the 390px viewport`).toBeLessThanOrEqual(0);
  }
});
