import { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// The served app.js surface (internal/httpx/dashboard/static/app.js).
interface EnclavaApi {
  api(method: string, path: string, body?: unknown): Promise<unknown>;
  toast(kind: string, msg: string, opts?: { action?: { label: string; href: string } }): void;
  openDrawer(urlOrHTML: string): Promise<void>;
  closeDrawer(): void;
  openModal(html: string): void;
  closeModal(): void;
  popover(anchor: Element, source: string): void;
  refresh(): void;
  copy(textOrSelector: string): Promise<void>;
  evidence: { open(id: string, opts?: { container?: string }): Promise<void>; close(): void };
  version: string;
}

declare global {
  interface Window {
    Enclava: EnclavaApi;
    htmx: { process(elt: Element): void };
  }
}


// shell.spec.ts — phase-7 shell behavior cases (P7-13, OA-11) against the
// SERVED app.js in a real Chromium: cookie-CSRF header sourcing, 401
// return_to redirect, 403 non-insertion, refresh dispatch/re-fetch, drawer
// focus semantics and evidence → #drawer routing.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

function requireSession() {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error(
      'shell.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for a real logged-in session — ' +
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

// Any rendered shell page carries the #toast/#drawer surface these cases
// exercise; Home is the always-registered one (the dev gallery is deleted).
async function openHome(page: Page) {
  const res = await page.goto(`${BASE}/dashboard`);
  if (!res || res.status() !== 200) {
    throw new Error(
      `the dashboard Home page is not served at ${BASE}/dashboard — run the ` +
      `server with a seeded tenant; got status ${res ? res.status() : 'none'}`,
    );
  }
  await expect(page.locator('#toast')).toHaveCount(1);
  await expect(page.locator('#drawer')).toHaveCount(1);
}

test('serves app.js as a non-empty JavaScript asset', async ({ request }) => {
  const res = await request.get(`${BASE}/dashboard/static/app.js`);
  expect(res.status()).toBe(200);
  expect(res.headers()['content-type']).toContain('text/javascript');
  expect((await res.text()).length).toBeGreaterThan(0);
});

test('Enclava.api sources X-CSRF-Token from the enclava_csrf cookie; FormData goes raw', async ({ page }) => {
  await login(page);
  await openHome(page);

  let headers: Record<string, string> = {};
  await page.route('**/api/v1/__shell-probe', async (route) => {
    headers = route.request().headers();
    await route.fulfill({ status: 200, contentType: 'application/json', body: '{"ok":true}' });
  });

  const data = await page.evaluate(() =>
    window.Enclava.api('POST', '/api/v1/__shell-probe', { a: 1 }),
  );
  expect(data).toEqual({ ok: true });

  const csrf = (await page.context().cookies()).find((c) => c.name === 'enclava_csrf');
  expect(csrf, 'enclava_csrf cookie must exist on a logged-in session').toBeTruthy();
  expect(headers['x-csrf-token']).toBe(csrf!.value);
  expect(headers['content-type']).toBe('application/json');

  await page.evaluate(() => {
    const fd = new FormData();
    fd.append('a', 'b');
    return window.Enclava.api('POST', '/api/v1/__shell-probe', fd);
  });
  // app.js must not stamp JSON on a FormData body — the browser adds multipart.
  expect(headers['content-type']).toContain('multipart/form-data');
});

test('api() on 401 navigates to login with the decoded return_to target', async ({ page }) => {
  await login(page);
  await openHome(page);

  await page.route('**/api/v1/__shell-probe', (route) =>
    route.fulfill({ status: 401, contentType: 'application/json', body: '{"error":{"message":"expired"}}' }),
  );
  // The 401 client contract runs under a dead session — clear it so the
  // login page doesn't 303 the still-valid session back to /dashboard.
  await page.context().clearCookies();
  await page.evaluate(() =>
    window.Enclava.api('POST', '/api/v1/__shell-probe', {}).catch(() => {}),
  );
  await page.waitForURL(/\/dashboard\/login/);
});

test('403 and network failures toast and never inject a rejected fragment', async ({ page }) => {
  await login(page);
  await openHome(page);

  await page.route('**/dashboard/partials/__shell-nope', (route) =>
    route.fulfill({ status: 403, contentType: 'application/json', body: '{"error":{"message":"nope"}}' }),
  );
  await page.evaluate(() =>
    window.Enclava.openDrawer('/dashboard/partials/__shell-nope').catch(() => {}),
  );
  const toast = page.locator('#toast');
  await expect(toast).toHaveClass(/show/);
  await expect(toast).toContainText('nope');
  expect(await page.evaluate(() => document.getElementById('drawer')!.innerHTML)).not.toContain('nope');
  expect(await page.evaluate(() => document.getElementById('drawer')!.classList.contains('open'))).toBe(false);

  await page.route('**/dashboard/partials/__shell-net', (route) => route.abort());
  await page.evaluate(() =>
    window.Enclava.openDrawer('/dashboard/partials/__shell-net').catch(() => {}),
  );
  await expect(toast).toContainText('Network error');
});

test('refresh() dispatches on body and hx-trigger="refresh from:body" re-issues its GET', async ({ page }) => {
  await login(page);
  await openHome(page);

  const fired = await page.evaluate(() =>
    new Promise<number>((resolve) => {
      document.body.addEventListener('refresh', () => resolve(1), { once: true });
      window.Enclava.refresh();
    }),
  );
  expect(fired).toBe(1);

  let fetches = 0;
  await page.route('**/api/v1/__shell-refresh-probe', async (route) => {
    fetches++;
    await route.fulfill({ status: 200, contentType: 'text/html', body: '<div>ok</div>' });
  });
  await page.evaluate(() => {
    const div = document.createElement('div');
    div.setAttribute('hx-get', '/api/v1/__shell-refresh-probe');
    div.setAttribute('hx-trigger', 'refresh from:body');
    div.setAttribute('hx-swap', 'innerHTML');
    document.body.appendChild(div);
    window.htmx.process(div);
    window.Enclava.refresh();
  });
  await expect.poll(() => fetches, { timeout: 5000 }).toBeGreaterThanOrEqual(1);
});

test('drawer traps focus, Esc closes and focus returns to the opener', async ({ page }) => {
  await login(page);
  await openHome(page);

  await page.evaluate(() => {
    const opener = document.createElement('button');
    opener.id = 'shell-drawer-opener';
    opener.textContent = 'open';
    document.body.appendChild(opener);
    opener.focus();
  });
  await page.evaluate(() =>
    window.Enclava.openDrawer('<p>x</p><button id="shell-drawer-ok">ok</button>'),
  );
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/);
  await expect(page.locator('#shell-drawer-ok')).toBeFocused();

  for (let i = 0; i < 4; i++) await page.keyboard.press('Tab');
  expect(await page.evaluate(() => document.getElementById('drawer')!.contains(document.activeElement))).toBe(true);
  await page.keyboard.press('Shift+Tab');
  expect(await page.evaluate(() => document.getElementById('drawer')!.contains(document.activeElement))).toBe(true);

  await page.keyboard.press('Escape');
  await expect(drawer).not.toHaveClass(/open/);
  await expect(page.locator('#shell-drawer-opener')).toBeFocused();
});

test('evidence:open routes into the shared #drawer with htmx headers; evidence:close clears it', async ({ page }) => {
  await login(page);
  await openHome(page);
  // Default routing case: no chat evidence column exists on this page.
  await page.evaluate(() =>
    document.querySelectorAll('.evidence-panel').forEach((n) => n.remove()),
  );

  let headers: Record<string, string> = {};
  await page.route('**/dashboard/partials/evidence/e1', async (route) => {
    headers = route.request().headers();
    await route.fulfill({ status: 200, contentType: 'text/html', body: '<div id="shell-evidence">Evidence 1</div>' });
  });

  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-action', 'evidence:open');
    b.setAttribute('data-evidence-id', 'e1');
    b.textContent = 'ev';
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#drawer')).toContainText('Evidence 1');
  expect(headers['hx-request']).toBe('true');
  expect(headers['hx-current-url']).toBeTruthy();

  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-action', 'evidence:close');
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#drawer')).not.toContainText('Evidence 1');
});
