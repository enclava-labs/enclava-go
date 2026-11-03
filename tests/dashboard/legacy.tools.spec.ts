import { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// tools.spec.ts — Tools page (09-05, OA-26) in a real Chromium. Proves the
// versioned approve contract end to end: the PUT body carries the persisted
// row's schema_version, and after a real approval the durable state (read
// back through the API) is approved — never just markup. The stale-review
// 409 path is exercised via route fulfillment: toast + queue re-render.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

interface MCPToolRow {
  tool_id: string;
  state: string;
  schema_version: number;
}

function requireSession() {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error(
      'tools.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for a real logged-in session — ' +
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

async function openTools(page: Page) {
  const res = await page.goto(`${BASE}/dashboard/tools`);
  if (!res || res.status() !== 200) {
    throw new Error(
      `the Tools page (09-05) is not served at ${BASE}/dashboard/tools — got status ` +
      `${res ? res.status() : 'none'}`,
    );
  }
}

// Seeds a fresh server + discovery so a reviewable (pending) row always
// exists — the shared e2e-mcp row may already be approved on reused schemas.
async function reviewableTool(page: Page) {
  const csrf = (await page.context().cookies()).find((c) => c.name === 'enclava_csrf');
  const origin = process.env.E2E_MCP_URL || '';
  if (!origin) throw new Error('E2E_MCP_URL unset — the MCP fixture is required to seed a discovery row');
  const hdrs = { 'X-CSRF-Token': csrf ? csrf.value : '', Origin: BASE, 'Content-Type': 'application/json' };
  const srvRes = await page.request.post(`${BASE}/api/v1/mcp/servers`, {
    headers: hdrs,
    data: { id: `e2e-tools-${Date.now()}`, origin, endpoint_path: '/mcp', authorized_private: true, enabled: true, audience_scopes: ['chatbot'] },
  });
  if (!srvRes.ok()) throw new Error(`server seed failed (${srvRes.status()}): ${await srvRes.text()}`);
  const srv = await srvRes.json();
  const disc = await page.request.post(`${BASE}/api/v1/mcp/servers/${srv.id}/discovery`, { headers: hdrs });
  if (!disc.ok()) throw new Error(`discovery failed (${disc.status()}): ${await disc.text()}`);
  const { tools } = await disc.json();
  const row = tools.find((t: MCPToolRow) => t.state === 'pending' || t.state === 'changed');
  if (!row) throw new Error('discovery produced no reviewable tool row');
  return { server: srv.id, tool: row.tool_id };
}

// First reviewable queue button (pending|changed) with its durable identity.
async function firstReviewable(page: Page) {
  const btn = page.locator('#tools-queue [data-action="tools:approve"]').first();
  if ((await btn.count()) === 0) {
    await reviewableTool(page);
    await page.reload();
  }
  const btn2 = page.locator('#tools-queue [data-action="tools:approve"]').first();
  if ((await btn2.count()) === 0) {
    throw new Error(
      'no pending/changed tool in the approval queue — seed an MCP server and run ' +
      'POST /api/v1/mcp/servers/{id}/discovery against a live MCP fixture so a durable ' +
      'discovery row exists; missing seed fails, never skips',
    );
  }
  return {
    server: (await btn2.getAttribute('data-server')) as string,
    tool: (await btn2.getAttribute('data-tool')) as string,
    boundVersion: (await btn2.getAttribute('data-schema-version')) as string,
  };
}

async function persistedRow(page: Page, server: string, tool: string): Promise<MCPToolRow> {
  const res = await page.request.get(`${BASE}/api/v1/mcp/servers/${server}/tools`);
  if (!res.ok()) {
    throw new Error(`cannot read persisted tool rows for server ${server}: status ${res.status()}`);
  }
  const { tools } = (await res.json()) as { tools: MCPToolRow[] };
  const row = tools.find((x) => x.tool_id === tool);
  if (!row) {
    throw new Error(`persisted row for tool ${tool} vanished from server ${server}`);
  }
  return row;
}

// Declared BEFORE the approving scenario so it never consumes the seeded
// pending row: the 409 is fulfilled client-side and the durable row is
// untouched by it.
test('a 409 schema_changed surfaces the re-review toast and re-renders the queue', async ({ page }) => {
  await login(page);
  await openTools(page);
  const { server, tool } = await firstReviewable(page);

  let queueRefetches = 0;
  await page.route('**/dashboard/partials/tools/queue*', (route) => {
    queueRefetches++;
    return route.continue();
  });
  await page.route(`**/api/v1/mcp/servers/${server}/tools/${tool}`, async (route) => {
    if (route.request().method() === 'PUT') {
      await route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({
          error: { message: 'schema changed since review; rediscover and review again', code: 'schema_changed' },
        }),
      });
      return;
    }
    await route.continue();
  });

  await page.locator('#tools-queue [data-action="tools:approve"]').first().click();

  const toast = page.locator('#toast.show');
  await expect(toast).toContainText('schema changed');
  await expect
    .poll(() => queueRefetches, { timeout: 5_000 })
    .toBeGreaterThan(0);
  // The stale row stays reviewable — never silently approved.
  const row = await persistedRow(page, server, tool);
  expect(row.state === 'pending' || row.state === 'changed').toBeTruthy();
  await expect(page.locator('#tools-queue [data-action="tools:approve"][data-tool="' + tool + '"]')).toHaveCount(1);
});

test('Approve PUTs {model_name, caller_scopes, schema_version} bound to the persisted row; the queue then shows it approved', async ({ page }) => {
  await login(page);
  await openTools(page);
  const { server, tool, boundVersion } = await firstReviewable(page);
  const row = await persistedRow(page, server, tool);
  expect(['pending', 'changed']).toContain(row.state);
  // The button's binding matches the durable row the admin reviewed.
  expect(boundVersion).toBe(String(row.schema_version));

  let putBody: { model_name?: string; caller_scopes?: string[]; schema_version?: number } | null = null;
  let putHeaders: Record<string, string> = {};
  await page.route(`**/api/v1/mcp/servers/${server}/tools/${tool}`, async (route) => {
    if (route.request().method() === 'PUT') {
      putBody = route.request().postDataJSON();
      putHeaders = route.request().headers();
    }
    await route.continue();
  });

  await page.locator('#tools-queue [data-action="tools:approve"]').first().click();
  await expect.poll(() => putBody, { timeout: 5_000 }).toBeTruthy();

  // The versioned contract: exact reviewed schema_version in the PUT body,
  // session CSRF header attached, non-empty model name and caller scopes.
  expect(putBody!.schema_version).toBe(row.schema_version);
  expect(Array.isArray(putBody!.caller_scopes)).toBeTruthy();
  expect(putBody!.caller_scopes!.length).toBeGreaterThan(0);
  expect((putBody!.model_name || '').length).toBeGreaterThan(0);
  expect(putHeaders['x-csrf-token']).toBeTruthy();

  // Persisted truth, not markup: the durable row is approved after the PUT,
  // and the re-rendered queue no longer offers it for review.
  await expect
    .poll(async () => (await persistedRow(page, server, tool)).state, { timeout: 5_000 })
    .toBe('approved');
  await expect(page.locator('#tools-queue [data-action="tools:approve"][data-tool="' + tool + '"]')).toHaveCount(0);
});
