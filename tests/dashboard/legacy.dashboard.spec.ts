import { Page } from '@playwright/test';
import { test, expect, LOCKOUT_EMAIL } from './fixtures';

// Explicit isolated credentials and base URL are required — there are no
// built-in example defaults to fall back to.
const BASE = process.env.BASE_URL || '';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';
const OIDC_AUTHORIZE_SUBSTR = process.env.OIDC_AUTHORIZE_SUBSTR || '';

test.skip(!BASE || !ADMIN_EMAIL || !ADMIN_PASSWORD, 'BASE_URL, ADMIN_EMAIL and ADMIN_PASSWORD are required');

const SESSION_COOKIE = 'enclava_session';
const CSRF_COOKIE = 'enclava_csrf';

async function sessionCookies(page: Page) {
  const cookies = await page.context().cookies();
  const session = cookies.find(c => c.name === SESSION_COOKIE);
  const csrf = cookies.find(c => c.name === CSRF_COOKIE);
  return { session, csrf };
}

// Helper: login via the real local-credential form and return the session.
async function login(page: Page) {
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

async function goToKeys(page: Page) {
  await login(page);
  await page.goto(`${BASE}/dashboard/access/api-keys`);
  await expect(page.locator('#access-api-keys')).toBeVisible();
}

async function openCreateKeyForm(page: Page) {
  await page.locator('button[data-drawer-url="/dashboard/partials/access/key-drawer"]').first().click();
  await expect(page.locator('form[data-form="access-key"]')).toBeVisible();
}

// ─── Health & public endpoints ─────────────────────────────────

test.describe('Health endpoints', () => {
  test('GET /health returns status ok', async ({ request }) => {
    const res = await request.get(`${BASE}/health`);
    expect(res.status()).toBe(200);
    const body = await res.json();
    expect(body.status).toBe('ok');
  });

  test('GET /livez returns 200', async ({ request }) => {
    expect((await request.get(`${BASE}/livez`)).status()).toBe(200);
  });
});

// ─── Authentication ────────────────────────────────────────────

test.describe('Authentication', () => {
  test('GET /dashboard redirects to /dashboard/login when unauthenticated', async ({ page }) => {
    await page.context().clearCookies();
    await page.goto(`${BASE}/dashboard`);
    await expect(page).toHaveURL(/\/dashboard\/login/);
  });

  test('login with a wrong password shows an accessible error', async ({ page }) => {
    await page.context().clearCookies();
    await page.goto(`${BASE}/dashboard/login`);
    // Dedicated lockout account — never spend failed attempts on a shared role.
    await page.fill('input[name="email"]', LOCKOUT_EMAIL);
    await page.fill('input[name="password"]', 'wrong-password');
    await page.click('button[type="submit"]');
    await expect(page).toHaveURL(/\/dashboard\/login/);
    await expect(page.locator('[role="alert"]')).toContainText('Invalid email or password');
  });

  test('login sets a server-side session and no service key in localStorage', async ({ page }) => {
    await login(page);
    const { session, csrf } = await sessionCookies(page);
    expect(session).toBeTruthy();
    expect(csrf).toBeTruthy();
    const stored = await page.evaluate(() => Object.values({ ...window.localStorage }).join(' '));
    expect(stored).not.toContain('enclava-dashboard-api-key');
  });

  test('login form submits with the keyboard only', async ({ page }) => {
    await page.context().clearCookies();
    await page.goto(`${BASE}/dashboard/login`);
    await page.locator('input[name="email"]').fill(ADMIN_EMAIL);
    await page.locator('input[name="password"]').fill(ADMIN_PASSWORD);
    await page.locator('input[name="password"]').press('Enter');
    await page.waitForURL('**/dashboard');
    const { session } = await sessionCookies(page);
    expect(session).toBeTruthy();
  });


  test('unauthenticated partial requests are denied', async ({ playwright }) => {
    // A fresh context: the project's stored admin session must NOT be sent —
    // inside the runner, request.newContext otherwise inherits it.
    const anon = await playwright.request.newContext({ baseURL: BASE, storageState: { cookies: [], origins: [] } });
    for (const p of ['/dashboard/partials/home/running', '/dashboard/partials/access/api-keys', '/dashboard/partials/usage/overview']) {
      const res = await anon.get(`${BASE}${p}`, { maxRedirects: 0 });
      // GET page partials redirect to login — assert the redirect, not copy.
      expect(res.status()).toBe(303);
      expect(res.headers()['location']).toContain('/dashboard/login');
    }
    expect((await anon.get(`${BASE}/api/v1/identities/users`)).status()).toBe(401);
    await anon.dispose();
  });

  test('cookie-authenticated logout requires the session CSRF token', async ({ page, playwright }) => {
    await login(page);
    const { session } = await sessionCookies(page);
    const anon = await playwright.request.newContext({ baseURL: BASE, storageState: { cookies: [], origins: [] } });
    const res = await anon.post(`${BASE}/dashboard/logout`, {
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}` },
    });
    // 401 (API-style denial) or 403 (CSRF denial) — never a successful logout.
    expect([401, 403]).toContain(res.status());
    await anon.dispose();
  });


  test('OIDC provider link starts the real authorize flow', async ({ page }) => {
    test.skip(!OIDC_AUTHORIZE_SUBSTR, 'OIDC_AUTHORIZE_SUBSTR is required to exercise the configured IdP');
    await page.context().clearCookies();
    await page.goto(`${BASE}/dashboard/login`);
    const link = page.locator('a[href*="/auth/oidc/"][href$="/start"], a[href*="/auth/oidc/"][href*="/start?"]').first();
    await expect(link).toBeVisible();
    await link.click();
    await page.waitForURL(new RegExp(OIDC_AUTHORIZE_SUBSTR));
  });
});

// ─── Session lifecycle ─────────────────────────────────────────

test.describe('Session lifecycle', () => {
  test('logout revokes the session and protects pages afterwards', async ({ page, request }) => {
    await login(page);
    const { session, csrf } = await sessionCookies(page);
    await page.click('button[aria-label="User menu"]');
    await page.click('button:has-text("Sign out")');
    await expect(page).toHaveURL(/\/dashboard\/login/);

    // The revoked cookie is worthless afterwards, even with its CSRF token.
    const res = await request.get(`${BASE}/dashboard/partials/home/running`, {
      maxRedirects: 0,
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}; ${CSRF_COOKIE}=${csrf!.value}` },
    });
    expect([303, 401]).toContain(res.status());

    await page.goto(`${BASE}/dashboard`);
    await expect(page).toHaveURL(/\/dashboard\/login/);
  });
});

// ─── Account menu & account page ───────────────────────────────

test.describe('Account page', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('user menu links to the account page', async ({ page }) => {
    await page.click('button[aria-label="User menu"]');
    await expect(page.locator('a[href="/dashboard/account"]')).toBeVisible();
  });

  test('account page loads profile, sessions and linked sign-ins', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/account`);
    await expect(page.locator('h1')).toContainText('Account');
    await expect(page.locator('#account-profile')).toContainText(ADMIN_EMAIL);
    await expect(page.locator('#account-sessions')).toBeVisible();
    await expect(page.locator('#account-linked')).toBeVisible();
  });

  test('access page lists people for an admin session', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/access`);
    await expect(page.locator('h1')).toContainText('Access');
    await expect(page.locator('#access-people')).toContainText(ADMIN_EMAIL);
  });

  test('account page works at a narrow viewport with the keyboard', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${BASE}/dashboard/account`);
    await expect(page.locator('#account-profile')).toContainText(ADMIN_EMAIL);
    // Keyboard: the password form is reachable and submittable without a mouse.
    const current = page.locator('#account-current-password');
    await current.focus();
    await expect(current).toBeFocused();
    await current.press('Tab');
    await expect(page.locator('#account-new-password')).toBeFocused();
  });
});

// ─── Dashboard shell ───────────────────────────────────────────

test.describe('Dashboard main page (authenticated)', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('home page renders its sections with the session', async ({ page }) => {
    await expect(page.locator('#homeKPIs')).toBeVisible();
    await expect(page.locator('#homeRunningSection')).toBeVisible();
  });

  test('theme toggle still works', async ({ page }) => {
    // The theme cookie drives <body class="light">; dark is the default.
    const body = page.locator('body');
    const wasLight = await body.evaluate((b) => b.classList.contains('light'));
    await page.click('button[aria-label="User menu"]');
    await page.click('button[data-action="theme"]');
    await expect.poll(() => body.evaluate((b) => b.classList.contains('light'))).toBe(!wasLight);
    await page.keyboard.press('Escape'); // the theme click detaches the popover
    await page.click('button[aria-label="User menu"]');
    await page.click('button[data-action="theme"]');
    await expect.poll(() => body.evaluate((b) => b.classList.contains('light'))).toBe(wasLight);
  });

  test('home running partial returns HTML with the session cookie', async ({ page, request }) => {
    const { session } = await sessionCookies(page);
    expect(session).toBeTruthy();
    const res = await request.get(`${BASE}/dashboard/partials/home/running`, {
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}` },
    });
    expect(res.status()).toBe(200);
    const text = await res.text();
    expect(text).not.toContain('"error"');
  });
});

// ─── Dashboard pages ───────────────────────────────────────────

const dashboardPages = [
  { path: '/dashboard/models', heading: 'Models' },
  { path: '/dashboard/usage', heading: 'Usage' },
  { path: '/dashboard/sources', heading: 'Sources' },
  { path: '/dashboard/tools', heading: 'Tools' },
  { path: '/dashboard/assistants', heading: 'Assistants' },
  { path: '/dashboard/search', heading: 'Search' },
  { path: '/dashboard/extract', heading: 'Extract' },
];

test.describe('Dashboard pages (authenticated)', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  for (const resource of dashboardPages) {
    test(`${resource.path} renders`, async ({ page }) => {
      await page.goto(`${BASE}${resource.path}`);
      await expect(page.locator('h1')).toContainText(resource.heading);
      await expect(page.locator('#content')).not.toContainText('"error"');
    });
  }
});

// ─── API Keys management (service credentials) ─────────────────

test.describe('API Keys management', () => {
  test.beforeEach(async ({ page }) => {
    await goToKeys(page);
  });

  test('keys tab loads', async ({ page }) => {
    const content = await page.locator('#access-api-keys').textContent();
    // Status column uses the glossary labels (active → "Ready").
    expect(content?.includes('Ready') || content?.includes('Disabled') || content?.includes('No API keys')).toBeTruthy();
  });

  test('create API key end-to-end with one-time display', async ({ page }) => {
    await openCreateKeyForm(page);
    const keyName = 'pw-key-' + Date.now();
    await page.fill('#access-key-name', keyName);
    await page.click('button:has-text("Create key")');

    await expect(page.locator('[data-key-result]')).toBeVisible();
    await expect(page.locator('[data-key-result]')).toContainText('shown only once');
    const shown = await page.locator('[data-key-result] code').first().textContent();
    const stored = await page.evaluate(() => Object.values({ ...window.localStorage }).join(' '));
    expect(shown).toBeTruthy();
    expect(stored).not.toContain(shown!.trim());
  });

  test('revoke API key', async ({ page }) => {
    await openCreateKeyForm(page);
    const keyName = 'pw-revoke-' + Date.now();
    await page.fill('#access-key-name', keyName);
    await page.click('button:has-text("Create key")');
    await expect(page.locator('#access-api-keys')).toContainText(keyName);

    await page.keyboard.press('Escape'); // the key-result drawer is modal — close it before row actions
    const row = page.locator(`tr:has-text("${keyName}")`);
    await expect(row).toBeVisible();
    const revoke = row.locator('button[data-access-confirm="key-revoke"]');
    await revoke.click();
    // Two-step confirm: the button arms, the second click revokes.
    await revoke.click();
    await expect(page.locator(`tr:has-text("${keyName}")`)).not.toBeVisible();
  });

  test('key mutations require the session CSRF token', async ({ page, request }) => {
    const { session, csrf } = await sessionCookies(page);
    expect(session).toBeTruthy();

    // Session cookie without the CSRF token must not mutate key state.
    const denied = await request.post(`${BASE}/api/v1/admin/api-keys`, {
      headers: {
        Cookie: `${SESSION_COOKIE}=${session!.value}`,
        'Content-Type': 'application/json',
      },
      data: '{"name":"csrf-probe"}',
    });
    expect([401, 403]).toContain(denied.status());

    // With the session's CSRF token (and the same-origin header
    // APIRequestContext does not send on its own) the mutation is authorized.
    const allowed = await request.post(`${BASE}/api/v1/admin/api-keys`, {
      headers: {
        Cookie: `${SESSION_COOKIE}=${session!.value}`,
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrf!.value,
        Origin: BASE,
      },
      data: '{"name":"csrf-ok-' + Date.now() + '","scopes":["chatbot"],"allowed_models":[],"allowed_endpoints":[],"allowed_chatbots":[],"allowed_agents":[],"allowed_extract_templates":[]}',
    });
    expect(allowed.status()).toBe(201);
  });
});
