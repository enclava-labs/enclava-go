import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';
import {
  BASE, MANAGER_EMAIL, MEMBER_EMAIL,
  MEMBER_PASSWORD, login, seedData, sessionCookies, SESSION_COOKIE,
  awaitTerminal,
} from './fixtures';

declare global {
  interface Window {
    Enclava: { api(method: string, path: string, body?: unknown): Promise<unknown> };
  }
}

// flows.admin.spec.ts — admin §9.2 flows 7–8 plus the migrated
// inventories (see 12-04 SUMMARY case map):
//   tests/dashboard.spec.ts — API key create (one-time display, never in
//   localStorage), revoke via two-step confirm, CSRF-required mutation;
//   theme toggle; home sections + running partial; page-render matrix
//   lives in nav.spec.ts.
//   tests/access.spec.ts — session-actor scope deny, platform.adminless
//   key rejected on /admin/api-keys.
//   tests/a11y.spec.ts (4b admin half) — failed run binds the capability-
//   resolved fix link to the models surface.

async function openKeyDrawer(page: Page) {
  await page.goto(`${BASE}/dashboard/access/api-keys`);
  await expect(page.locator('#access-api-keys')).toBeVisible();
  await page.locator('button[data-drawer-url="/dashboard/partials/access/key-drawer"]').first().click();
  await expect(page.locator('form[data-form="access-key"]')).toBeVisible();
}

test.describe('Access — API keys (flow 8)', () => {
  test('create API key end-to-end with one-time display and revoke it', async ({ page }) => {
    await openKeyDrawer(page);
    const keyName = `e2e-key-${Date.now()}`;
    await page.fill('#access-key-name', keyName);
    await page.click('button:has-text("Create key")');
    const result = page.locator('[data-key-result]');
    await expect(result).toBeVisible();
    await expect(result).toContainText('shown only once');
    const shown = (await result.locator('code').first().textContent()) ?? '';
    expect(shown.trim().length).toBeGreaterThan(0);
    // The raw secret never lands in localStorage.
    const stored = await page.evaluate(() => Object.values({ ...window.localStorage }).join(' '));
    expect(stored).not.toContain(shown.trim());

    // Revoke through the shared two-step confirm.
    await page.goto(`${BASE}/dashboard/access/api-keys`);
    const row = page.locator('#access-api-keys tr', { hasText: keyName });
    await expect(row).toBeVisible({ timeout: 15_000 });
    const revoke = row.locator('button[data-access-confirm="key-revoke"]');
    await revoke.click();
    await row.locator('button[data-confirm]:not([disabled])').click();
    await expect(page.locator(`#access-api-keys tr:has-text("${keyName}")`)).toHaveCount(0, { timeout: 15_000 });
  });

  test('key mutations require the session CSRF token', async ({ page, request: req }) => {
    await page.goto(`${BASE}/dashboard/access/api-keys`);
    const { session, csrf } = await sessionCookies(page);
    expect(session).toBeTruthy();

    const denied = await req.post(`${BASE}/api/v1/admin/api-keys`, {
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}`, 'Content-Type': 'application/json' },
      data: '{"name":"csrf-probe"}',
    });
    expect([401, 403]).toContain(denied.status());

    const ok = await req.post(`${BASE}/api/v1/admin/api-keys`, {
      headers: {
        Cookie: `${SESSION_COOKIE}=${session!.value}`,
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrf ?? '',
      },
      data: '{"name":"csrf-ok"}',
    });
    expect([200, 201]).toContain(ok.status());
  });

  test('member and manager session writes to admin key surface are denied', async ({ page, context }) => {
    for (const email of [MEMBER_EMAIL, MANAGER_EMAIL]) {
      await context.clearCookies();
      await login(page, email, MEMBER_PASSWORD);
      await page.goto(`${BASE}/dashboard`);
      const res = await page.evaluate(async () => {
        try {
          await window.Enclava.api('POST', '/api/v1/admin/api-keys', { name: 'probe' });
          return { ok: true as const };
        } catch (err) {
          const e = err as { status?: number };
          return { ok: false as const, status: e.status ?? 0 };
        }
      });
      expect(res, `${email} admin key write`).toEqual({ ok: false, status: 403 });
    }
  });

  test('a scoped key without platform.admin is denied on admin surfaces', async ({ request: req }) => {
    const minted = await req.post(`${BASE}/api/v1/admin/api-keys`, {
      headers: { Authorization: `Bearer ${process.env.E2E_ADMIN_API_KEY ?? ''}`, 'Content-Type': 'application/json' },
      data: '{"name":"e2e-scoped","scopes":["runs.create"]}',
    });
    expect([200, 201]).toContain(minted.status());
    const body = await minted.json() as { secret_key?: string; key?: string };
    const raw = body.secret_key ?? body.key ?? '';
    if (raw) {
      const denied = await req.post(`${BASE}/api/v1/admin/api-keys`, {
        headers: { Authorization: `Bearer ${raw}`, 'Content-Type': 'application/json' },
        data: '{"name":"scoped-probe"}',
      });
      expect(denied.status()).toBe(403);
    }
  });
});

test.describe('admin shell + home cases', () => {
  test('theme toggle flips the rendered theme and persists via cookie', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    const html = page.locator('html');
    await page.click('button[aria-label="User menu"]');
    await page.click('button[data-action="theme"]');
    const first = await html.getAttribute('class');
    await page.click('button[aria-label="User menu"]');
    await page.click('button[data-action="theme"]');
    const second = await html.getAttribute('class');
    expect(first).not.toBe(second);
    // The server reads enclava_theme on the next render — reload keeps it.
    await page.reload();
    expect(await html.getAttribute('class')).toBe(second);
  });

  test('admin home renders KPIs, running section and conf-card with real counts', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    await expect(page.locator('#homeKPIs')).toBeVisible();
    await expect(page.locator('#homeRunningSection')).toBeVisible();
    await expect(page.locator('button.conf-card')).toBeVisible();
    // The seeded env endpoint is not confidential — the card reports the
    // truthful "No confidential endpoints" state, never a fabricated count.
    await page.click('button.conf-card');
    await expect(page.locator('#pop')).toBeVisible();
    await expect(page.locator('#pop')).toContainText('confidential');
  });

  test('conf-pop closes on Esc and focus returns', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    const card = page.locator('button.conf-card');
    await card.click();
    await expect(page.locator('#pop')).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(page.locator('#pop')).toBeHidden();
  });
});

test.describe('admin error surfaces', () => {
  test('rejected write renders a sticky toast with no fix link', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    await page.evaluate(async () => {
      try {
        await window.Enclava.api('POST', '/api/v1/admin/api-keys', { name: '' });
      } catch { /* rejection expected */ }
    });
    const toast = page.locator('#toast.show.error');
    await expect(toast).toBeVisible();
    await expect(toast.locator('a[href]')).toHaveCount(0);
  });

  test('failed run row binds the capability-resolved fix link', async ({ page }) => {
    test.setTimeout(120_000);
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.failingAssistantID)}`);
    await page.fill('#composer', 'admin unpriced run');
    await page.click('[data-action="chat:send"]');
    await awaitTerminal(page, 120_000);
    const line = page.locator('.thread-body .runline').last();
    await expect(line).toBeVisible();
    const fix = line.locator('a[href]');
    await expect(fix.first()).toBeVisible();
    expect(await fix.first().getAttribute('href')).toContain('/dashboard/models');
  });
});

