import { test, expect, Page, APIRequestContext } from '@playwright/test';

// chat-search.spec.ts — the Chat consumer scenarios (OA-32, plan 10-02):
// real login, real /api/v1 seeding, two-turn run with conversation adoption,
// Stop two-step, mid-run reload resume-polling without a time bail, the three
// evidence surfaces, and grant isolation. Search scenarios (OA-33) are
// appended here by 10-03.
//
// House rule: a missing prerequisite FAILS the case naming what is absent —
// never a skip, never status != 403 as proof.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

function requireAdmin() {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error(
      'chat-search.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for a real logged-in ' +
      'session with explicit mode:all grants — seed the admin and set both; ' +
      'missing prerequisites fail, never skip',
    );
  }
}

async function login(page: Page, email = ADMIN_EMAIL, password = ADMIN_PASSWORD) {
  requireAdmin();
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', password);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
}

// Session-authenticated /api/v1 helper: cookies come from the browser context
// and the CSRF cookie is echoed into X-CSRF-Token like app.js does.
async function api(request: APIRequestContext, page: Page, method: string, path: string, body?: unknown) {
  const csrf = (await page.context().cookies()).find((c) => c.name === 'enclava_csrf');
  const res = await request.fetch(`${BASE}${path}`, {
    method,
    headers: {
      'X-CSRF-Token': csrf ? csrf.value : '',
      ...(body instanceof FormData ? {} : { 'Content-Type': 'application/json' }),
    },
    multipart: body instanceof FormData ? body : undefined,
    data: body instanceof FormData ? undefined : body ? JSON.stringify(body) : undefined,
  });
  return res;
}

// ── Response narrowing (no unchecked casts on network data) ──────────────

async function fieldString(res: { json(): Promise<unknown> }, field: string): Promise<string> {
  const value: unknown = await res.json();
  if (typeof value === 'object' && value !== null && field in value) {
    const v: unknown = value[field];
    if (typeof v === 'string') return v;
  }
  throw new Error(
    `response is missing string field "${field}" — contract drift: ${JSON.stringify(value)?.slice(0, 200)}`,
  );
}

async function firstModelID(res: { json(): Promise<unknown> }): Promise<string> {
  const payload: unknown = await res.json();
  if (typeof payload === 'object' && payload !== null && 'data' in payload) {
    const data: unknown = payload.data;
    if (Array.isArray(data)) {
      for (const model of data) {
        if (typeof model === 'object' && model !== null && 'id' in model && typeof model.id === 'string') {
          return model.id;
        }
      }
    }
  }
  throw new Error('GET /v1/models returned no usable model id — seed a model before running the chat scenarios');
}

async function firstEvidenceID(res: { json(): Promise<unknown> }): Promise<string> {
  const payload: unknown = await res.json();
  if (typeof payload === 'object' && payload !== null && 'results' in payload) {
    const results: unknown = payload.results;
    if (Array.isArray(results)) {
      for (const result of results) {
        if (
          typeof result === 'object' && result !== null && 'evidence_id' in result &&
          typeof result.evidence_id === 'string'
        ) {
          return result.evidence_id;
        }
      }
    }
  }
  throw new Error(
    'search returned no evidence_id — the evidence passage contract (08-03) is not satisfied for the seeded document',
  );
}

async function documentsReadyCount(res: { json(): Promise<unknown> }): Promise<number> {
  const payload: unknown = await res.json();
  if (typeof payload === 'object' && payload !== null && 'documents' in payload) {
    const documents: unknown = payload.documents;
    if (Array.isArray(documents)) return documents.length;
  }
  return 0;
}

// ── Seeding through the real /api/v1 surface ─────────────────────────────

interface Seed {
  assistantID: string;
  collectionID: string;
  evidenceID: string;
}

// A public collection with one ingested document (for the evidence card) and
// a tenant-wide assistant bound to it.
async function seedChatFixtures(request: APIRequestContext, page: Page): Promise<Seed> {
  const modelRes = await api(request, page, 'GET', '/v1/models');
  if (!modelRes.ok()) {
    throw new Error(`GET /v1/models failed (${modelRes.status()}) — the deployment has no model catalog to seed an assistant from`);
  }
  const model = await firstModelID(modelRes);

  const colRes = await api(request, page, 'POST', '/api/v1/rag/collections', {
    name: `chat-e2e-${Date.now()}`,
    chunk_size: 200,
    chunk_overlap: 20,
    acl: { public: true },
  });
  if (!colRes.ok()) {
    throw new Error(`POST /api/v1/rag/collections failed (${colRes.status()}): ${await colRes.text()}`);
  }
  const collectionID = await fieldString(colRes, 'id');

  const form = new FormData();
  form.append(
    'file',
    new Blob([Buffer.from('The enclave renewal window is the first Monday of each quarter.')], { type: 'text/plain' }),
    'renewals.txt',
  );
  const upRes = await api(request, page, 'POST', `/api/v1/rag/collections/${collectionID}/documents`, form);
  if (!upRes.ok() && upRes.status() !== 202) {
    throw new Error(`document upload failed (${upRes.status()}): ${await upRes.text()}`);
  }

  // Explicit waits, no sleeps: the document must reach ready (08-08 pipeline).
  await expect
    .poll(
      async () => {
        const res = await api(request, page, 'GET', `/api/v1/rag/collections/${collectionID}/documents?status=ready`);
        if (!res.ok()) return 0;
        return documentsReadyCount(res);
      },
      {
        timeout: 60_000,
        message: 'document ingestion never reached ready — check the deployment embedding provider (08-08)',
      },
    )
    .toBeGreaterThan(0);

  const searchRes = await api(request, page, 'POST', '/api/v1/rag/search', {
    query: 'enclave renewal window',
    mode: 'lexical',
    collection_ids: [collectionID],
  });
  if (!searchRes.ok()) {
    throw new Error(`POST /api/v1/rag/search failed (${searchRes.status()}): ${await searchRes.text()}`);
  }
  const evidenceID = await firstEvidenceID(searchRes);

  const botRes = await api(request, page, 'POST', '/api/v1/chatbots', {
    name: `E2E Reviewer ${Date.now()}`,
    model,
    rag_collection_ids: [collectionID],
    audience: ['*'],
  });
  if (!botRes.ok()) {
    throw new Error(`POST /api/v1/chatbots failed (${botRes.status()}): ${await botRes.text()}`);
  }
  const assistantID = await fieldString(botRes, 'id');

  return { assistantID, collectionID, evidenceID };
}

// Send a first message and wait for the conversation adoption: URL becomes
// /dashboard/chat/{id} and the pending runline renders.
async function sendFirstMessage(page: Page, assistantID: string, text: string): Promise<string> {
  await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(assistantID)}`);
  await page.fill('#composer', text);
  await page.click('[data-action="chat:send"]');
  await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);
  return page.url().split('/').pop() as string;
}

// Terminal is whatever the thread re-render shows once the runline is gone.
async function awaitTerminal(page: Page, timeout = 90_000) {
  await expect
    .poll(() => page.locator('.thread-body .runline').count(), { timeout, interval: 1_000 })
    .toBe(0);
}

test.describe('Chat run flow (OA-32)', () => {
  test('two turns adopt the conversation, render citations and the evidence card, and survive reload', async ({ page, request }) => {
    test.setTimeout(180_000);
    await login(page);
    const seed = await seedChatFixtures(request, page);

    const convID = await sendFirstMessage(
      page,
      seed.assistantID,
      'What is the enclave renewal window? Use the knowledge base and cite your source as [1].',
    );

    // A second message posts with the adopted conversation_id (two-turn proof).
    await page.fill('#composer', 'And who approves it? Answer with [1] again if you use the source.');
    await page.click('[data-action="chat:send"]');
    await awaitTerminal(page);

    const citations = page.locator('article.message .bubble button.citation');
    if ((await citations.count()) === 0) {
      throw new Error(
        'the assistant answer carried no [n] citation markers — the citation→evidence contract needs a grounded answer; ' +
        'check the seeded assistant is bound to the seeded collection and the model cites sources',
      );
    }

    // Clicking a citation opens the evidence panel: document + both links.
    await citations.first().click();
    const panel = page.locator('.evidence-panel');
    await expect(panel).toContainText('renewals.txt');
    await expect(panel).toContainText('Open in source');
    await expect(panel).toContainText('View retained copy');

    // Reload renders the same conversation server-side at the adopted URL.
    await page.reload();
    await expect(page).toHaveURL(new RegExp(`/dashboard/chat/${convID}$`));
    await expect(page.locator('#chatThread .bubble').first()).toBeVisible();
  });

  test('Stop arms through the shared confirm, DELETEs the run and renders Stopped by you', async ({ page, request }) => {
    test.setTimeout(120_000);
    await login(page);
    const seed = await seedChatFixtures(request, page);

    await sendFirstMessage(page, seed.assistantID, 'Long-running question — summarise everything in the knowledge base.');

    const stop = page.locator('[data-run-id][data-action="confirm"]');
    await expect(stop).toBeVisible();
    await stop.click(); // arm: shared data-confirm rewrites the label
    await expect(stop).toHaveText(/Stop run\?/);
    const deleteRun = page.waitForRequest((r) => r.method() === 'DELETE' && /\/api\/v1\/agent-runs\/.+/.test(r.url()));
    await stop.click(); // second click fires enclava:confirm → DELETE
    await deleteRun;
    await expect(page.locator('.thread-body')).toContainText('Stopped by you', { timeout: 30_000 });
  });

  test('mid-run reload resumes polling until terminal with no time bail', async ({ page, request }) => {
    test.setTimeout(120_000);
    await login(page);
    const seed = await seedChatFixtures(request, page);

    const convID = await sendFirstMessage(page, seed.assistantID, 'Count the documents you can see.');
    // Reload mid-run: the server-rendered pending marker must resume polling.
    await page.reload();
    await expect(
      page
        .locator('article.message[data-run-id][data-run-status="queued"], article.message[data-run-id][data-run-status="running"]')
        .first(),
    ).toBeVisible();

    let polls = 0;
    page.on('request', (r) => {
      if (r.method() === 'GET' && /\/api\/v1\/agent-runs\/[^/]+$/.test(r.url())) polls++;
    });
    // Continued request traffic over ~7s — a bailing implementation stops
    // updating; this one keeps polling every 2s until the terminal render.
    await page.waitForTimeout(7_000);
    if (polls < 2) {
      throw new Error(
        `expected resumed 2s polling after mid-run reload, saw ${polls} polls — the no-time-cutoff poll contract is broken`,
      );
    }
    await awaitTerminal(page);
    expect(await page.locator('#chatThread .bubble').count()).toBeGreaterThan(0);
    expect(page.url()).toContain(`/dashboard/chat/${convID}`);
  });

  test('New chat resets composer, thread and URL, and the next conversation is reloadable', async ({ page, request }) => {
    test.setTimeout(180_000);
    await login(page);
    const seed = await seedChatFixtures(request, page);

    const convID = await sendFirstMessage(page, seed.assistantID, 'First conversation question.');
    await page.fill('#composer', 'draft that must not survive');
    await page.click('.head [data-action="chat:new"]');
    await expect(page).toHaveURL(new RegExp('/dashboard/chat$'));
    await expect(page.locator('#composer')).toHaveValue('');
    await expect(page.locator('#chatThread')).toContainText('No conversation open yet');

    // The previous conversation is still reloadable at its adopted URL.
    await page.goto(`${BASE}/dashboard/chat/${convID}`);
    await expect(page.locator('#chatThread .bubble').first()).toBeVisible();

    // A fresh first send adopts a NEW conversation id.
    await page.fill('#composer', 'Second conversation question.');
    await page.click('[data-action="chat:send"]');
    await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);
    expect(page.url()).not.toContain(convID);
    await awaitTerminal(page);
  });
});

test.describe('Chat surfaces and grants (OA-32)', () => {
  test('at 390x844 history hides behind the Chats button and evidence opens as a bottom sheet', async ({ page, request }) => {
    test.setTimeout(120_000);
    await page.setViewportSize({ width: 390, height: 844 });
    await login(page);
    const seed = await seedChatFixtures(request, page);

    await page.goto(`${BASE}/dashboard/chat`);
    await expect(page.locator('.history')).toBeHidden();
    const chats = page.locator('.head .mobile-chats');
    await expect(chats).toBeVisible();
    await chats.click();
    await expect(page.locator('#mobileChats')).toBeVisible();
    await page.locator('#mobileChats .mobile-chats').first().click(); // Close chats
    await expect(page.locator('#mobileChats')).toBeHidden();

    // Opening evidence <760 routes into the .evidence-panel.sheet bottom sheet.
    await page.evaluate((id) => window.Enclava.evidence.open(id), seed.evidenceID);
    const sheet = page.locator('.evidence-panel.sheet');
    await expect(sheet).toBeVisible();
    await expect(sheet).toContainText('renewals.txt');
    await sheet.locator('[data-action="evidence:close"]').click();
    await expect(page.locator('.evidence-panel.sheet')).toHaveCount(0);
  });

  test('member grants isolate history and the picker; grantless members see the truthful empty state', async ({ page, request }) => {
    test.setTimeout(180_000);
    requireAdmin();
    await login(page);

    // Admin (explicit mode:all) owns a conversation on the seeded assistant.
    const seed = await seedChatFixtures(request, page);
    const adminConv = await api(request, page, 'POST', '/api/v1/agent-runs', {
      assistant_id: seed.assistantID,
      input: 'admin own conversation question',
    });
    if (!adminConv.ok() && adminConv.status() !== 202) {
      throw new Error(`admin run start failed (${adminConv.status()}): ${await adminConv.text()}`);
    }
    const adminConvID = await fieldString(adminConv, 'conversation_id');

    // Member role: explicit mode:list grants for exactly the seeded assistant.
    const roleRes = await api(request, page, 'POST', '/api/v1/identities/roles', {
      name: `chat-member-${Date.now()}`,
      scopes: ['chatbot', 'rag'],
      grants: {
        assistants: { mode: 'list', items: [seed.assistantID] },
        models: { mode: 'list', items: [] },
      },
    });
    if (!roleRes.ok()) {
      throw new Error(`role create failed (${roleRes.status()}): ${await roleRes.text()}`);
    }
    const roleID = await fieldString(roleRes, 'id');
    const email = `chat-member-${Date.now()}@example.test`;
    const userRes = await api(request, page, 'POST', '/api/v1/identities/users', {
      email,
      display_name: 'Chat Member',
      password: 'chat-member-pass',
      role_ids: [roleID],
    });
    if (!userRes.ok()) {
      throw new Error(`member create failed (${userRes.status()}): ${await userRes.text()}`);
    }

    // The member's deep link to the admin's conversation is the uniform 404.
    await page.context().clearCookies();
    await login(page, email, 'chat-member-pass');
    await page.goto(`${BASE}/dashboard/chat/${adminConvID}`);
    await expect(page.locator('.authwrap h1')).toHaveText('Page not found');

    // Empty own history, only the granted assistant, none of the admin's rows.
    await page.goto(`${BASE}/dashboard/chat`);
    await expect(page.locator('#chatHistory .history-item')).toHaveCount(0);
    await expect(page.locator('#assistantSelect option')).toHaveCount(1);
    await expect(page.locator('#assistantSelect')).not.toBeDisabled();

    // Grantless member: empty picker, grant-needed help, disabled Send.
    const gRole = await api(request, page, 'POST', '/api/v1/identities/roles', {
      name: `chat-grantless-${Date.now()}`,
      scopes: ['chatbot', 'rag'],
      grants: {
        assistants: { mode: 'list', items: [] },
        models: { mode: 'list', items: [] },
      },
    });
    if (!gRole.ok()) {
      throw new Error(`grantless role create failed (${gRole.status()}): ${await gRole.text()}`);
    }
    const gRoleID = await fieldString(gRole, 'id');
    const gEmail = `chat-grantless-${Date.now()}@example.test`;
    const gUser = await api(request, page, 'POST', '/api/v1/identities/users', {
      email: gEmail,
      display_name: 'Grantless Member',
      password: 'grantless-pass',
      role_ids: [gRoleID],
    });
    if (!gUser.ok()) {
      throw new Error(`grantless member create failed (${gUser.status()}): ${await gUser.text()}`);
    }
    await page.context().clearCookies();
    await login(page, gEmail, 'grantless-pass');
    await page.goto(`${BASE}/dashboard/chat`);
    await expect(page.locator('#assistantSelect')).toBeDisabled();
    await expect(page.locator('#assistantSelect option')).toHaveCount(0);
    await expect(page.locator('.thread-head')).toContainText('ask an admin to grant access');
    await expect(page.locator('[data-action="chat:send"]')).toBeDisabled();
  });
});
