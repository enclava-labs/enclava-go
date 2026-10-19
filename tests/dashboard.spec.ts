import { test, expect, Page, APIRequestContext } from '@playwright/test';

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
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', ADMIN_EMAIL);
  await page.fill('input[name="password"]', ADMIN_PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
}

async function goToKeys(page: Page) {
  await login(page);
  await page.goto(`${BASE}/dashboard/keys`);
  await expect(page.locator('#dashboard-keys-list')).not.toContainText('Loading keys...');
}

async function openCreateKeyForm(page: Page) {
  await page.click('button:text("Create API Key")');
  await expect(page.locator('#create-key-form')).toBeVisible();
}

async function createRAGCollection(page: Page, name: string) {
  await page.goto(`${BASE}/dashboard/rag`);
  const mount = page.locator('#dashboard-rag');
  await expect(mount).not.toContainText('Loading');
  const form = mount.locator('form:has(input[name="action"][value="create_collection"])');
  await form.locator('input[name="name"]').fill(name);
  await form.locator('input[name="embedding_model"]').fill('embed-model');
  await form.locator('button:text("Create")').click();
  await expect(mount).toContainText('RAG collection created');
  await expect(mount).toContainText(name);
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
    await page.goto(`${BASE}/dashboard`);
    await expect(page).toHaveURL(/\/dashboard\/login/);
  });

  test('login with a wrong password shows an accessible error', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/login`);
    await page.fill('input[name="email"]', ADMIN_EMAIL);
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
    await page.goto(`${BASE}/dashboard/login`);
    await page.locator('input[name="email"]').fill(ADMIN_EMAIL);
    await page.locator('input[name="password"]').fill(ADMIN_PASSWORD);
    await page.locator('input[name="password"]').press('Enter');
    await page.waitForURL('**/dashboard');
    const { session } = await sessionCookies(page);
    expect(session).toBeTruthy();
  });


  test('unauthenticated partial requests are denied', async ({ request }) => {
    for (const p of ['/dashboard/partials/stats', '/dashboard/partials/keys', '/dashboard/partials/usage']) {
      const res = await request.get(`${BASE}${p}`, { maxRedirects: 0 });
      // GET page partials redirect to login — assert the redirect, not copy.
      expect(res.status()).toBe(303);
      expect(res.headers()['location']).toBe('/dashboard/login');
    }
    expect((await request.get(`${BASE}/api/v1/identities/users`)).status()).toBe(401);
  });

  test('cookie-authenticated logout requires the session CSRF token', async ({ page, request }) => {
    await login(page);
    const { session } = await sessionCookies(page);
    const res = await request.post(`${BASE}/dashboard/logout`, {
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}` },
    });
    // 401 (API-style denial) or 403 (CSRF denial) — never a successful logout.
    expect([401, 403]).toContain(res.status());
  });


  test('OIDC provider link starts the real authorize flow', async ({ page }) => {
    test.skip(!OIDC_AUTHORIZE_SUBSTR, 'OIDC_AUTHORIZE_SUBSTR is required to exercise the configured IdP');
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
    await page.click('summary[title="User menu"]');
    await page.click('button[title="Sign out"]');
    await expect(page).toHaveURL(/\/dashboard\/login/);

    // The revoked cookie is worthless afterwards, even with its CSRF token.
    const res = await request.get(`${BASE}/dashboard/partials/stats`, {
      maxRedirects: 0,
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}; ${CSRF_COOKIE}=${csrf!.value}` },
    });
    expect([303, 401]).toContain(res.status());

    await page.goto(`${BASE}/dashboard`);
    await expect(page).toHaveURL(/\/dashboard\/login/);
  });
});

// ─── Account menu & identity page ──────────────────────────────

test.describe('Identity page', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('account menu shows the signed-in email and identity link', async ({ page }) => {
    await page.click('summary[title="User menu"]');
    const panel = page.locator('.dashboard-account-panel');
    await expect(panel.getByText(ADMIN_EMAIL, { exact: true })).toBeVisible();
    await expect(panel.locator('a[href="/dashboard/identity"]')).toBeVisible();
    await expect(panel.getByText('Logout', { exact: true })).toBeVisible();
  });

  test('identity page loads account, sessions and linked identities', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/identity`);
    await expect(page.locator('h1')).toContainText('Identity');
    await expect(page.locator('#identity-email')).toContainText(ADMIN_EMAIL);
    await expect(page.locator('#identity-sessions')).toContainText('Current session');
    await expect(page.locator('#identity-links')).not.toContainText('Loading');
  });

  test('administration section lists users for an admin session', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/identity`);
    await expect(page.locator('#identity-admin')).toBeVisible();
    await expect(page.locator('#identity-users')).toContainText(ADMIN_EMAIL);
  });

  test('identity page works at a narrow viewport with the keyboard', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${BASE}/dashboard/identity`);
    await expect(page.locator('#identity-email')).toContainText(ADMIN_EMAIL);
    // Keyboard: the password form is reachable and submittable without a mouse.
    const current = page.locator('#identity-current-password');
    await current.focus();
    await expect(current).toBeFocused();
    await current.press('Tab');
    await expect(page.locator('#identity-new-password')).toBeFocused();
  });
});

// ─── Dashboard shell ───────────────────────────────────────────

test.describe('Dashboard main page (authenticated)', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('stats partial loads with the session', async ({ page }) => {
    await expect(page.locator('#dashboard-stats-mount')).not.toContainText('Loading dashboard...');
    await expect(page.locator('text=Active keys')).toBeVisible();
  });

  test('theme toggle still works', async ({ page }) => {
    const html = page.locator('html');
    await expect(html).toHaveClass(/dark/);
    await page.click('button[title="Toggle theme"]');
    await expect(html).not.toHaveClass(/dark/);
    await page.click('button[title="Toggle theme"]');
    await expect(html).toHaveClass(/dark/);
  });

  test('stats partial API returns HTML with the session cookie', async ({ page, request }) => {
    const { session } = await sessionCookies(page);
    expect(session).toBeTruthy();
    const res = await request.get(`${BASE}/dashboard/partials/stats`, {
      headers: { Cookie: `${SESSION_COOKIE}=${session!.value}` },
    });
    expect(res.status()).toBe(200);
    const text = await res.text();
    expect(text).not.toContain('"error"');
    expect(text).toContain('Active keys');
  });
});

// ─── Resource dashboard pages ─────────────────────────────────

const resourcePages = [
  { path: '/dashboard/providers', mount: '#dashboard-providers' },
  { path: '/dashboard/pricing', mount: '#dashboard-pricing' },
  { path: '/dashboard/billing-audit', mount: '#dashboard-billing-audit' },
  { path: '/dashboard/rag', mount: '#dashboard-rag' },
  { path: '/dashboard/chatbots', mount: '#dashboard-chatbots' },
  { path: '/dashboard/prompt-templates', mount: '#dashboard-prompt-templates' },
  { path: '/dashboard/connectors', mount: '#dashboard-connectors' },
];

test.describe('Resource dashboard pages (authenticated)', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  for (const resource of resourcePages) {
    test(`${resource.path} partial loads`, async ({ page }) => {
      await page.goto(`${BASE}${resource.path}`);
      await expect(page.locator(resource.mount)).not.toContainText('Loading');
      await expect(page.locator(resource.mount)).not.toContainText('"error"');
    });
  }
});

// ─── Resource dashboard workflows ─────────────────────────────

test.describe('Resource dashboard workflows (authenticated)', () => {
  test.beforeEach(async ({ page }) => {
    await login(page);
  });

  test('prompt template create flow works', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/prompt-templates`);
    const mount = page.locator('#dashboard-prompt-templates');
    await expect(mount).not.toContainText('Loading');
    const name = `pw-template-${Date.now()}`;

    const form = mount.locator('form').first();
    await form.locator('input[name="name"]').fill(name);
    await form.locator('textarea[name="system_prompt"]').fill('You answer with operational detail.');
    await form.locator('textarea[name="user_prompt"]').fill('Question: {{.user_input}}');
    await form.locator('button:text("Create")').click();

    await expect(mount).toContainText('Prompt template created');
    await expect(mount).toContainText(name);
  });

  test('RAG collection create and delete flow works', async ({ page }) => {
    const name = `pw-rag-${Date.now()}`;
    await createRAGCollection(page, name);

    const mount = page.locator('#dashboard-rag');
    await expect(mount).toContainText(name);

    page.on('dialog', dialog => dialog.accept());
    await mount.locator(`tr:has-text("${name}") button:text("Delete")`).click();
    await expect(mount).toContainText('RAG collection deleted');
    await expect(mount.locator(`tr:has-text("${name}")`)).not.toBeVisible();
  });

  test('chatbot create flow works', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/chatbots`);
    const mount = page.locator('#dashboard-chatbots');
    await expect(mount).not.toContainText('Loading');
    const name = `pw-chatbot-${Date.now()}`;
    const externalID = `pw_chatbot_${Date.now()}`;

    const form = mount.locator('form').first();
    await form.locator('input[name="name"]').fill(name);
    await form.locator('input[name="model"]').fill('contract-model');
    await form.locator('input[name="external_id"]').fill(externalID);
    await form.locator('button:text("Create")').click();

    await expect(mount).toContainText('Chatbot created');
    await expect(mount).toContainText(`/api/v1/chatbot/external/${externalID}/chat/completions`);

    const row = mount.locator(`tr:has-text("${name}")`);
    await row.locator('button:text("Disable")').click();
    await expect(mount).toContainText('Chatbot disabled');

    page.on('dialog', dialog => dialog.accept());
    await mount.locator(`tr:has-text("${name}") button:text("Delete")`).click();
    await expect(mount).toContainText('Chatbot deleted');
    await expect(mount.locator(`tr:has-text("${name}")`)).not.toBeVisible();
  });
});

// ─── API Keys management (service credentials) ─────────────────

test.describe('API Keys management', () => {
  test.beforeEach(async ({ page }) => {
    await goToKeys(page);
  });

  test('keys partial loads via HTMX', async ({ page }) => {
    await expect(page.locator('#dashboard-keys-list')).not.toContainText('Loading keys...');
    const content = await page.locator('#dashboard-keys-list').textContent();
    expect(content?.includes('Active') || content?.includes('Inactive') || content?.includes('No API keys')).toBeTruthy();
  });

  test('create API key end-to-end with one-time display', async ({ page }) => {
    await openCreateKeyForm(page);
    const keyName = 'pw-key-' + Date.now();
    await page.fill('input[name="name"]', keyName);
    await page.fill('input[name="description"]', 'Playwright test key');
    await page.click('button:text("Create key")');

    await expect(page.locator('#dashboard-keys-list')).toContainText('API key created');
    await expect(page.locator('#dashboard-keys-list')).toContainText('Store this key now');
    await expect(page.locator('#dashboard-keys-list')).toContainText(keyName);
    const shown = await page.locator('#dashboard-keys-list .font-mono code').first().textContent();
    const stored = await page.evaluate(() => Object.values({ ...window.localStorage }).join(' '));
    expect(shown).toBeTruthy();
    expect(stored).not.toContain(shown!.trim());
  });

  test('toggle and delete API key', async ({ page }) => {
    await openCreateKeyForm(page);
    const keyName = 'pw-toggle-' + Date.now();
    await page.fill('input[name="name"]', keyName);
    await page.click('button:text("Create key")');
    await expect(page.locator('#dashboard-keys-list')).toContainText(keyName);

    const row = page.locator(`tr:has-text("${keyName}")`);
    await expect(row).toBeVisible();
    await Promise.all([
      page.waitForResponse((r) => r.url().includes('/dashboard/partials/keys') && r.request().method() === 'POST'),
      row.locator('button[title="Disable key"]').click(),
    ]);
    await expect(page.locator(`tr:has-text("${keyName}")`)).toContainText('Inactive');

    page.on('dialog', dialog => dialog.accept());
    await page.locator(`tr:has-text("${keyName}") button[title="Delete key"]`).click();
    await expect(page.locator('#dashboard-keys-list')).toContainText('API key deleted');
    await expect(page.locator(`tr:has-text("${keyName}")`)).not.toBeVisible();
  });

  test('key mutations require the session CSRF token', async ({ page, request }) => {
    const { session, csrf } = await sessionCookies(page);
    expect(session).toBeTruthy();

    // Session cookie without the CSRF token must not mutate key state.
    const denied = await request.post(`${BASE}/dashboard/partials/keys`, {
      headers: {
        Cookie: `${SESSION_COOKIE}=${session!.value}`,
        'Content-Type': 'application/x-www-form-urlencoded',
      },
      data: 'action=create&name=csrf-probe',
    });
    expect(denied.status()).toBe(403);

    // With the session's CSRF token the same mutation is authorized.
    const allowed = await request.post(`${BASE}/dashboard/partials/keys`, {
      headers: {
        Cookie: `${SESSION_COOKIE}=${session!.value}`,
        'Content-Type': 'application/x-www-form-urlencoded',
        'X-CSRF-Token': csrf!.value,
      },
      data: 'action=create&name=csrf-ok-' + Date.now(),
    });
    expect(allowed.status()).toBe(200);
  });
});
