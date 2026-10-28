import { test, expect } from '@playwright/test';
import {
  BASE, ADMIN_EMAIL, MEMBER_EMAIL, MANAGER_EMAIL, MEMBER_PASSWORD,
  GRANTLESS_EMAIL, login, sessionCookies, SESSION_COOKIE, CSRF_COOKIE,
} from './fixtures';

// nav.spec.ts — per-role nav truth (§3.3, flow 9): every visible sidebar
// item opens its page (200 + h1), hidden items are absent AND their URLs
// render the designed 403 page, unknown URLs get the designed 404 page, the
// confidential card is visible in a real seeded state, the theme toggle
// flips body.light, and old dashboard URLs are gone (designed 404, never a
// redirect — migrated tests/dashboard.spec.ts + tests/home.spec.ts cases).
//
// This spec runs under every role project (the role comes from the
// project's stored session); role-neutral session/auth cases run once under
// the admin project.

const PAGES: Record<string, { label: string; href: string; h1: RegExp }[]> = {
  member: [
    { label: 'Home', href: '/dashboard', h1: /home/i },
    { label: 'Chat', href: '/dashboard/chat', h1: /chat/i },
    { label: 'Search', href: '/dashboard/search', h1: /search/i },
    { label: 'Extract', href: '/dashboard/extract', h1: /extract/i },
    { label: 'Knowledge', href: '/dashboard/knowledge', h1: /knowledge/i },
  ],
  manager: [
    { label: 'Home', href: '/dashboard', h1: /home/i },
    { label: 'Chat', href: '/dashboard/chat', h1: /chat/i },
    { label: 'Search', href: '/dashboard/search', h1: /search/i },
    { label: 'Extract', href: '/dashboard/extract', h1: /extract/i },
    { label: 'Assistants', href: '/dashboard/assistants', h1: /assistants/i },
    { label: 'Knowledge', href: '/dashboard/knowledge', h1: /knowledge/i },
    { label: 'Sources', href: '/dashboard/sources', h1: /sources/i },
  ],
  admin: [
    { label: 'Home', href: '/dashboard', h1: /home/i },
    { label: 'Chat', href: '/dashboard/chat', h1: /chat/i },
    { label: 'Search', href: '/dashboard/search', h1: /search/i },
    { label: 'Extract', href: '/dashboard/extract', h1: /extract/i },
    { label: 'Assistants', href: '/dashboard/assistants', h1: /assistants/i },
    { label: 'Knowledge', href: '/dashboard/knowledge', h1: /knowledge/i },
    { label: 'Sources', href: '/dashboard/sources', h1: /sources/i },
    { label: 'Tools', href: '/dashboard/tools', h1: /tools/i },
    { label: 'Models', href: '/dashboard/models', h1: /models/i },
    { label: 'Access', href: '/dashboard/access', h1: /access/i },
    { label: 'Usage & billing', href: '/dashboard/usage', h1: /usage/i },
  ],
};

const HIDDEN: Record<string, { label: string; href: string }[]> = {
  member: [
    { label: 'Assistants', href: '/dashboard/assistants' },
    { label: 'Sources', href: '/dashboard/sources' },
    { label: 'Tools', href: '/dashboard/tools' },
    { label: 'Models', href: '/dashboard/models' },
    { label: 'Access', href: '/dashboard/access' },
    { label: 'Usage & billing', href: '/dashboard/usage' },
  ],
  manager: [
    { label: 'Tools', href: '/dashboard/tools' },
    { label: 'Models', href: '/dashboard/models' },
    { label: 'Access', href: '/dashboard/access' },
    { label: 'Usage & billing', href: '/dashboard/usage' },
  ],
  admin: [],
};


test.describe('sidebar nav truth (§3.3)', () => {
  test('every visible nav item opens its page; Account link is present', async ({ page }) => {
    const items = PAGES[test.info().project.name];
    await page.goto(`${BASE}/dashboard`);
    for (const item of items) {
      const link = page.locator(`#nav a.navlink[href="${item.href}"]`);
      await expect(link, `nav must show ${item.label}`).toHaveCount(1);
      const res = await page.goto(`${BASE}${item.href}`);
      expect(res?.status(), `${item.href} must serve 200`).toBe(200);
      await expect(page.locator('h1'), `${item.href} heading`).toContainText(item.h1);
    }
    // Every visible link claims exactly its page — nothing extra rendered.
    const labels = await page.locator('#nav a.navlink .navlabel').allTextContents();
    expect(labels).toEqual(items.map((i) => i.label));
    await expect(page.locator('a.navlink[href="/dashboard/account"]')).toHaveCount(1);
  });

  test('hidden items are absent from nav and their URLs 403', async ({ page }) => {
    const hidden = HIDDEN[test.info().project.name];
    if (hidden.length === 0) {
      // The admin sees every item — assert the ADMIN group instead.
      await page.goto(`${BASE}/dashboard`);
      for (const label of ['Tools', 'Models', 'Access', 'Usage & billing']) {
        await expect(page.locator('#nav a.navlink', { hasText: label })).toHaveCount(1);
      }
      return;
    }
    await page.goto(`${BASE}/dashboard`);
    for (const item of hidden) {
      await expect(page.locator('#nav a.navlink', { hasText: item.label })).toHaveCount(0);
      const res = await page.goto(`${BASE}${item.href}`);
      expect(res?.status(), `${item.href} must be denied`).toBe(403);
      await expect(page.locator('h1')).toContainText("You don't have access to this page");
    }
  });

  test('unknown /dashboard URL renders the designed 404 page', async ({ page }) => {
    const res = await page.goto(`${BASE}/dashboard/nope`);
    expect(res?.status()).toBe(404);
    await expect(page.locator('.authwrap h1')).toHaveText('Page not found');
  });

  test('confidential card renders a real seeded endpoint state', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    const card = page.locator('.conf-card');
    await expect(card).toBeVisible();
    await card.click();
    await expect(page.locator('#pop')).toBeVisible();
    await page.keyboard.press('Escape');
  });

  test('theme toggle switches body.light', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    const body = page.locator('body');
    const light = await body.evaluate((b) => b.classList.contains('light'));
    await page.locator('[data-action="theme"]').first().click();
    await expect.poll(() => page.locator('body').evaluate((b) => b.classList.contains('light')))
      .toBe(!light);
    await page.locator('[data-action="theme"]').first().click();
    await expect.poll(() => page.locator('body').evaluate((b) => b.classList.contains('light')))
      .toBe(light);
  });

  test('old dashboard URLs are gone — designed 404, never a redirect', async ({ page, request }) => {
    for (const old of [
      '/dashboard/keys', '/dashboard/budgets', '/dashboard/analytics',
      '/dashboard/providers', '/dashboard/pricing', '/dashboard/rag',
      '/dashboard/chatbots', '/dashboard/mcp', '/dashboard/documents',
    ]) {
      const res = await request.get(`${BASE}${old}`, { maxRedirects: 0 });
      expect([303, 404], `${old} must redirect-to-login or 404, never an old page`).toContain(res.status());
      if (res.status() === 404) continue;
      const page404 = await page.goto(`${BASE}${old}`);
      expect(page404?.status()).toBe(404);
      await expect(page.locator('.authwrap h1')).toHaveText('Page not found');
    }
  });
});

test.describe('session and auth (migrated tests/dashboard.spec.ts)', () => {
  test.skip(({ }, testInfo) => testInfo.project.name !== 'admin', 'auth cases run once, under admin');

  test('GET /health and /livez respond', async ({ request }) => {
    const health = await request.get(`${BASE}/health`);
    expect(health.status()).toBe(200);
    expect((await health.json()).status).toBe('ok');
    expect((await request.get(`${BASE}/livez`)).status()).toBe(200);
  });

  test('unauthenticated /dashboard redirects to login; partials and APIs are denied', async ({ page, request }) => {
    await page.context().clearCookies();
    await page.goto(`${BASE}/dashboard`);
    await expect(page).toHaveURL(/\/dashboard\/login/);
    for (const p of ['/dashboard/partials/home/running', '/dashboard/partials/access/api-keys']) {
      const res = await request.get(`${BASE}${p}`, { maxRedirects: 0 });
      expect(res.status()).toBe(303);
      expect(res.headers()['location']).toBe('/dashboard/login');
    }
    expect((await request.get(`${BASE}/api/v1/identities/users`)).status()).toBe(401);
  });

  test('login with a wrong password shows an accessible error', async ({ page }) => {
    await page.context().clearCookies();
    await page.goto(`${BASE}/dashboard/login`);
    await page.fill('input[name="email"]', ADMIN_EMAIL);
    await page.fill('input[name="password"]', 'wrong-password');
    await page.click('button[type="submit"]');
    await expect(page).toHaveURL(/\/dashboard\/login/);
    await expect(page.locator('[role="alert"]')).toContainText('Invalid email or password');
  });

  test('login sets a server-side session and no service key in localStorage', async ({ page }) => {
    await page.context().clearCookies();
    await login(page, ADMIN_EMAIL, process.env.ADMIN_PASSWORD || '');
    const { session, csrf } = await sessionCookies(page);
    expect(session).toBeTruthy();
    expect(csrf).toBeTruthy();
    const stored = await page.evaluate(() => Object.values({ ...window.localStorage }).join(' '));
    expect(stored).not.toContain('enclava-dashboard-api-key');
  });

  test('cookie-authenticated logout requires the session CSRF token', async ({ page, request }) => {
    const { session } = await sessionCookies(page);
    const res = await request.post(`${BASE}/dashboard/logout`, {
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}` },
    });
    expect([401, 403]).toContain(res.status());
  });

  test('logout revokes the session and protects pages afterwards', async ({ page, request }) => {
    const { session, csrf } = await sessionCookies(page);
    await page.goto(`${BASE}/dashboard`);
    await page.locator('button[aria-label="User menu"]').click();
    await page.locator('button:has-text("Sign out")').click();
    await expect(page).toHaveURL(/\/dashboard\/login/);
    const res = await request.get(`${BASE}/dashboard/partials/home/running`, {
      maxRedirects: 0,
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}; ${CSRF_COOKIE}=${csrf!.value}` },
    });
    expect([303, 401]).toContain(res.status());
  });

  test('user menu links to the account page; account loads profile, sessions and sign-ins', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    await page.locator('button[aria-label="User menu"]').click();
    await expect(page.locator('a[href="/dashboard/account"]')).toBeVisible();
    await page.goto(`${BASE}/dashboard/account`);
    await expect(page.locator('h1')).toContainText('Account');
    await expect(page.locator('#account-profile')).toContainText(ADMIN_EMAIL);
    await expect(page.locator('#account-sessions')).toBeVisible();
    await expect(page.locator('#account-linked')).toBeVisible();
  });

  test('account page works at a narrow viewport with the keyboard', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${BASE}/dashboard/account`);
    await expect(page.locator('#account-profile')).toContainText(ADMIN_EMAIL);
    const current = page.locator('#account-current-password');
    await current.focus();
    await expect(current).toBeFocused();
    await current.press('Tab');
    await expect(page.locator('#account-new-password')).toBeFocused();
  });

  test('home page renders sections and the running partial returns HTML', async ({ page, request }) => {
    await page.goto(`${BASE}/dashboard`);
    await expect(page.locator('#homeKPIs')).toBeVisible();
    await expect(page.locator('#homeRunningSection')).toBeVisible();
    const { session } = await sessionCookies(page);
    const res = await request.get(`${BASE}/dashboard/partials/home/running`, {
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}` },
    });
    expect(res.status()).toBe(200);
    expect(await res.text()).not.toContain('"error"');
  });
});
