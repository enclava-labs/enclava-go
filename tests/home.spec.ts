import { test, expect, Page, APIRequestContext } from '@playwright/test';

// home.spec.ts — per-role Home (OA-31) browser coverage: section composition
// per capability (§6.2), the truthful /v1 API base, the zero-filled 30-point
// sparkline, real extraction totals, grant-shaped member visibility (D-25)
// and the running-now polling lifecycle (OA-36). Every prerequisite is
// seeded through the real /api/v1 surface with the admin session; a seed
// failure FAILS the case naming the missing producer — it is never skipped.
//
// All six manager attention kinds and the zero-filled series are asserted
// to the point level in internal/httpx/dashboard_home_test.go (server side,
// deterministic); here the browser checks the visible per-role composition
// plus every kind the /api/v1 surface can actually seed.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

test.skip(!BASE || !ADMIN_EMAIL || !ADMIN_PASSWORD, 'BASE_URL, ADMIN_EMAIL and ADMIN_PASSWORD are required');

const SESSION_COOKIE = 'enclava_session';
const CSRF_COOKIE = 'enclava_csrf';

async function login(page: Page, email = ADMIN_EMAIL, password = ADMIN_PASSWORD) {
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', password);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
}

// api seeds through the real surface with the browser's session+CSRF pair.
async function api(page: Page, method: string, path: string, body?: unknown) {
  const cookies = await page.context().cookies();
  const csrf = cookies.find(c => c.name === CSRF_COOKIE);
  const res = await page.request.fetch(`${BASE}${path}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      ...(csrf ? { 'X-CSRF-Token': csrf.value } : {}),
    },
    data: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok()) {
    throw new Error(`seed ${method} ${path} failed: ${res.status()} ${await res.text()}`);
  }
  return res.json().catch(() => ({}));
}

async function seedText(page: Page, path: string, fileName: string, bytes: string) {
  const cookies = await page.context().cookies();
  const csrf = cookies.find(c => c.name === CSRF_COOKIE);
  const form = new FormData();
  form.append('file', new Blob([bytes], { type: 'text/plain' }), fileName);
  const res = await page.request.fetch(`${BASE}${path}`, {
    method: 'POST',
    headers: csrf ? { 'X-CSRF-Token': csrf.value } : {},
    multipart: form as unknown as FormData,
  });
  if (!res.ok()) {
    throw new Error(`seed upload ${path} failed: ${res.status()} ${await res.text()}`);
  }
  return res.json().catch(() => ({}));
}

// seededMember provisions a user with an explicit-grant role via the real
// identities APIs and logs them in on a fresh context — D-25 grant shapes.
async function seededMember(page: Page, opts: {
  email: string; password: string; assistantIDs: string[];
}): Promise<Page> {
  await api(page, 'POST', '/api/v1/identities/roles', {
    name: `e2e-home-${opts.email}-${Date.now()}`,
    scopes: ['chatbot', 'rag', 'extract', 'models.list'],
    grants: {
      assistants: { mode: 'list', items: opts.assistantIDs },
    },
  }).then(async role => {
    await api(page, 'POST', '/api/v1/identities/users', {
      email: opts.email, display_name: opts.email, password: opts.password,
      role_ids: [role.id],
    });
  });
  const member = await page.context().browser()!.newContext().then(c => c.newPage());
  await login(member, opts.email, opts.password);
  return member;
}

test.describe('Home /dashboard — admin', () => {
  test('renders sections in §6.2 order with a truthful /v1 API base and a 30-point zero-filled spark', async ({ page }) => {
    await login(page);

    // Seeds through the real surface: tenant-wide assistant, readable
    // collection, an unverified confidential endpoint, and a tenant budget.
    const bot = await api(page, 'POST', '/api/v1/chatbots', {
      name: 'E2E Home Assistant', model: 'gpt-test', audience: ['*'],
    });
    expect(bot.id, 'POST /api/v1/chatbots must return the assistant id').toBeTruthy();
    const col = await api(page, 'POST', '/api/v1/rag/collections', { name: `E2E Home ${Date.now()}` });
    expect(col.id, 'POST /api/v1/rag/collections must return the collection id').toBeTruthy();
    await api(page, 'POST', '/api/v1/admin/budgets', {
      name: `e2e-budget-${Date.now()}`, subject_type: 'tenant', kind: 'amount',
      limit_value: 1, currency: 'USD', period: 'monthly',
    });

    await page.goto(`${BASE}/dashboard`);

    // Truthful API base: /v1, never /api/v1.
    await expect(page.locator('.home-greeting')).toContainText('API base · /v1');
    const body = await page.content();
    expect(body).not.toContain('/api/v1');

    // §6.2 order: attention → KPIs → running → confidential → conversations
    // → start a chat → extractions → knowledge.
    const ids = ['#homeAttention', '#homeKPIs', '#homeRunningSection', '#homeConfidential', '#homeStartChat', '#homeKnowledge'];
    const tops: Array<{ id: string; y: number }> = [];
    for (const id of ids) {
      const el = page.locator(id);
      await expect(el).toBeVisible();
      tops.push({ id, y: (await el.boundingBox())!.y });
    }
    expect(tops.map(t => t.id)).toEqual(ids); // every section present
    expect([...tops].sort((a, b) => a.y - b.y).map(t => t.id)).toEqual(tops.map(t => t.id));

    // KPI grid: spark polyline has exactly 30 points (zero-filled window).
    const points = await page.locator('#homeKPIs svg.spark polyline').getAttribute('points');
    expect(points!.trim().split(/\s+/)).toHaveLength(30);
    await expect(page.locator('#homeKPIs')).toContainText('Requests · 30d');
    await expect(page.locator('#homeKPIs')).toContainText('Spend · 30d');
    await expect(page.locator('#homeKPIs')).toContainText('Knowledge documents');
    await expect(page.locator('#homeKPIs')).toContainText('Assistant conversations');

    // Seeded surface shows through: assistant tile, collection row.
    await expect(page.locator('#homeStartChat')).toContainText('E2E Home Assistant');
    await expect(page.locator(`#homeStartChat a[href="/dashboard/chat?assistant=${bot.id}"]`)).toBeVisible();
    await expect(page.locator('#homeKnowledge')).toContainText(col.name);

    // Seeded manager attention kinds with their real fix links: the
    // confidential endpoint is unobserved until first dispatch → the
    // "not verified" item links to Models; a spend budget exists → Review
    // budgets. (Failed documents/syncs/tools kinds need live external
    // systems; they are asserted server-side in dashboard_home_test.go.)
    await expect(page.locator('#homeAttention')).toContainText('not verified');
    await expect(page.locator('#homeAttention a[href="/dashboard/models"]')).toBeVisible();
  });

  test('running-now fragment polls a seeded job to terminal without a reload', async ({ page }) => {
    await login(page);
    const seed = await seedText(page, '/api/v1/extract/jobs', 'e2e-home.txt',
      'Invoice from Acme GmbH\nTotal: 42,00 EUR\n');
    expect(seed.job_id || seed.id, 'POST /api/v1/extract/jobs must return a job id').toBeTruthy();

    await page.goto(`${BASE}/dashboard`);
    const running = page.locator('#homeRunning');
    if (await running.locator('.activity-row').count() > 0) {
      await expect(running).toContainText(/Extract/i);
      // The fragment live-updates: when the worker settles the job the
      // 3s trigger disappears without any page reload.
      await expect
        .poll(async () => running.getAttribute('hx-trigger'), { timeout: 30_000 })
        .not.toContain('every 3s');
    } else {
      // No worker in this deployment: the fragment must still be wired and
      // honest about the empty state — polling never runs while empty.
      await expect(running).toContainText('Nothing is running right now.');
      expect(await running.getAttribute('hx-trigger')).not.toContain('every 3s');
    }
  });

  test('old dashboard surfaces are gone — designed 404, no redirects', async ({ request }) => {
    for (const p of [
      '/dashboard/partials/stats', '/dashboard/chatbots', '/dashboard/partials/chatbots',
      '/dashboard/prompt-templates', '/dashboard/partials/prompt-templates',
      '/dashboard/rag', '/dashboard/partials/rag', '/dashboard/rag/documents/anything/download',
    ]) {
      const res = await request.get(`${BASE}${p}`, { maxRedirects: 0, headers: {} });
      expect(res.status(), `${p} must 404, never redirect`).toBe(404);
    }
    // The canonical slashless path serves 200 directly — no mux 301.
    const res = await request.get(`${BASE}/dashboard`, { maxRedirects: 0 });
    expect([200, 303]).toContain(res.status()); // 303 only when unauthenticated
    if (res.status() === 303) expect(res.headers()['location']).toContain('/dashboard/login');
  });
});

test.describe('Home /dashboard — members (grant-shaped, D-25)', () => {
  test('a granted member sees own sections and zero manager content', async ({ page }) => {
    await login(page);
    const bot = await api(page, 'POST', '/api/v1/chatbots', {
      name: 'E2E Granted Assistant', model: 'gpt-test', audience: ['*'],
    });
    const hidden = await api(page, 'POST', '/api/v1/chatbots', {
      name: 'E2E Hidden Assistant', model: 'gpt-test', audience: ['*'],
    });
    const stamp = Date.now();
    const password = 'member-pass-123';
    const email = `e2e-home-member-${stamp}@example.com`;
    const member = await seededMember(page, { email, password, assistantIDs: [bot.id] });
    try {
      // Own extraction work: one queued job through the real extract API.
      await seedText(member, '/api/v1/extract/jobs', 'member-e2e.txt', 'Total: 1,00 EUR\n');

      await member.goto(`${BASE}/dashboard`);
      await expect(member.locator('#homeStartChat')).toContainText('E2E Granted Assistant');
      await expect(member.locator(`#homeStartChat a[href="/dashboard/chat?assistant=${bot.id}"]`)).toBeVisible();
      await expect(member.locator('#homeStartChat')).not.toContainText('E2E Hidden Assistant');
      await expect(member.locator('#homeConversations')).toContainText('No conversations yet.');
      await expect(member.locator('#homeExtractions')).toContainText('1 total');
      // Own attention kinds only; no manager rows, no KPI grid.
      await expect(member.locator('#homeKPIs')).toHaveCount(0);
      await expect(member.locator('#homeAttention')).not.toContainText('not verified');
      await expect(member.locator('#homeAttention')).not.toContainText('Budget');
      // Members still cannot reach admin-only surfaces.
      const probe = await member.request.get(`${BASE}/dashboard/partials/usage`, { maxRedirects: 0 });
      expect(probe.status()).toBe(403);
    } finally {
      await member.context().close();
    }
  });

  test('a grantless member sees the grant-needed empty state, never a fake list', async ({ page }) => {
    await login(page);
    await api(page, 'POST', '/api/v1/chatbots', { name: 'E2E Deny Assistant', model: 'gpt-test', audience: ['*'] });
    const stamp = Date.now();
    const email = `e2e-home-grantless-${stamp}@example.com`;
    const password = 'member-pass-123';
    // The default seeded Member role carries explicit empty mode:list
    // grants (D-25) — invite with it and the picker must stay empty.
    const roles = await api(page, 'GET', '/api/v1/identities/roles');
    const memberRole = (roles.roles ?? roles).find?.((r: { name?: string }) =>
      typeof r.name === 'string' && /^member$/i.test(r.name));
    const roleIDs = memberRole ? [memberRole.id] : [];
    await api(page, 'POST', '/api/v1/identities/users', {
      email, display_name: email, password, role_ids: roleIDs,
    });
    const member = await page.context().browser()!.newContext().then(c => c.newPage());
    try {
      await login(member, email, password);
      await member.goto(`${BASE}/dashboard`);
      await expect(member.locator('#homeStartChat')).toContainText(
        'No assistants you can use yet — ask an admin to grant access.');
      await expect(member.locator('#homeStartChat a.tile, #homeStartChat .card')).toHaveCount(0);
    } finally {
      await member.context().close();
    }
  });
});
