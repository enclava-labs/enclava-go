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
  await page.context().clearCookies();
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', password);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
  // The app revokes a user's older session on a new login — persist the
  // fresh one so the project's stored state never goes stale mid-file.
  if (email === ADMIN_EMAIL) await page.context().storageState({ path: 'tests/dashboard/.auth/admin.json' });
}

// Session-authenticated /api/v1 helper: cookies come from the browser context
// and the CSRF cookie is echoed into X-CSRF-Token like app.js does.
async function api(request: APIRequestContext, page: Page, method: string, path: string, body?: unknown) {
  const csrf = (await page.context().cookies()).find((c) => c.name === 'enclava_csrf');
  const sess = (await page.context().cookies()).find((c) => c.name === 'enclava_session');
  const res = await request.fetch(`${BASE}${path}`, {
    method,
    headers: {
      Cookie: sess ? `enclava_session=${sess.value}` : '',
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
  // Pin the seeded chat model: the catalog lists every bound alias sorted,
  // so "whatever is first" changes with later specs' endpoints (e.g. the
  // a11y suite's deliberately unpriced alias sorts before e2e-chat).
  const payload: unknown = await res.json();
  const ids: string[] = [];
  if (typeof payload === 'object' && payload !== null && 'data' in payload) {
    const data: unknown = payload.data;
    if (Array.isArray(data)) {
      for (const model of data) {
        if (typeof model === 'object' && model !== null && 'id' in model && typeof model.id === 'string') {
          ids.push(model.id);
        }
      }
    }
  }
  if (!ids.includes('e2e-chat')) {
    throw new Error(`GET /v1/models lacks the seeded chat model 'e2e-chat' (${ids.join(', ')}) — the chat scenarios cannot run`);
  }
  return 'e2e-chat';
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
    acl: {}, // owner-only: public ACLs inflate the member unscoped-search work bound
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
    .poll(async () => {
      const live = await page.locator('.thread-body .runline:not(.muted)').count();
      if (live > 0) return 'running';
      const answers = await page.locator('.thread-body article.message').count();
      const muted = await page.locator('.thread-body .runline.muted').count();
      return answers >= 2 || muted > 0 ? 'done' : 'waiting';
    }, { timeout, interval: 1_000 })
    .toBe('done');
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

    // Clicking a citation opens the evidence panel: the document and its
    // retained copy. 'Open in source' exists only for synced sources — an
    // uploaded document has no external source to open.
    await citations.first().click();
    const panel = page.locator('.evidence-panel');
    await expect(panel).toContainText('renewals.txt');
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

    const convID = await sendFirstMessage(page, seed.assistantID, 'Long-running question — summarise everything in the knowledge base.');
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

    // A fresh first send adopts a NEW conversation id — from the assistant's
    // clean URL, not the revisited one (a send there would adopt it, which
    // the two-turns case above already covers).
    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.assistantID)}`);
    await page.fill('#composer', 'Second conversation question.');
    await page.click('[data-action="chat:send"]');
    await page.waitForURL((u) => /\/dashboard\/chat\/[^/]+$/.test(u.pathname) && !u.pathname.endsWith(convID));
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

    // Grantless user seeded while the page still holds the ADMIN session —
    // role/user writes are platform.admin APIs, not member calls.
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
    await page.context().clearCookies();
    await login(page, gEmail, 'grantless-pass');
    await page.goto(`${BASE}/dashboard/chat`);
    await expect(page.locator('#assistantSelect')).toBeDisabled();
    await expect(page.locator('#assistantSelect option')).toHaveCount(0);
    await expect(page.locator('.thread-head')).toContainText('ask an admin to grant access');
    await expect(page.locator('[data-action="chat:send"]')).toBeDisabled();
  });
});
// ── Search scenarios (OA-33, plan 10-03) ─────────────────────────────────
//
// The results fragment GET /dashboard/partials/search/results is the single
// renderer — these drive the page surface only (D-24). Members read through
// real ACL audiences (collection acl.audience entries); an absent contract
// fails naming it, never a skip.

interface SearchSeed {
  colA: string;
  colB: string;
  colSecret: string;
  docA: string;
  evidenceID: string;
  memberEmail: string;
  memberPass: string;
  memberID: string;
  assistantID: string;
}

// Create a member user (explicit empty grants — deny-default per D-25) plus a
// role carrying the scopes the scenario needs; returns its actor id.
async function seedMember(request: APIRequestContext, page: Page, scopes: string[], grants: Record<string, unknown>) {
  const roleRes = await api(request, page, 'POST', '/api/v1/identities/roles', {
    name: `search-member-${Date.now()}-${Math.floor(Math.random() * 1e6)}`, scopes, grants,
  });
  if (!roleRes.ok()) throw new Error(`member role create failed (${roleRes.status()}): ${await roleRes.text()}`);
  const roleID = await fieldString(roleRes, 'id');
  const email = `search-member-${Date.now()}@example.test`;
  const pass = 'search-member-pass';
  const userRes = await api(request, page, 'POST', '/api/v1/identities/users', {
    email, display_name: 'Search Member', password: pass, role_ids: [roleID],
  });
  if (!userRes.ok()) throw new Error(`member create failed (${userRes.status()}): ${await userRes.text()}`);
  const memberID = await fieldString(userRes, 'id');
  return { email, pass, memberID };
}

// Upload one document through the multipart API and poll until ready.
async function uploadDoc(request: APIRequestContext, page: Page, collectionID: string, name: string, content: string) {
  const form = new FormData();
  form.append('file', new Blob([Buffer.from(content)], { type: 'text/plain' }), name);
  const res = await api(request, page, 'POST', `/api/v1/rag/collections/${collectionID}/documents`, form);
  if (!res.ok() && res.status() !== 202) {
    throw new Error(`upload ${name} failed (${res.status()}): ${await res.text()}`);
  }
  await expect
    .poll(
      async () => {
        const list = await api(request, page, 'GET', `/api/v1/rag/collections/${collectionID}/documents?status=ready`);
        return list.ok() ? documentsReadyCount(list) : 0;
      },
      { timeout: 60_000, message: 'document never reached ready — check the deployment embedding provider (08-08)' },
    )
    .toBeGreaterThan(0);
}

// Two member-readable collections (each with a matching document), one
// collection restricted to an outsider, plus an assistant the member's stored
// grants name — the Ask-about-this run must be real and server-authorized.
async function seedSearchFixtures(request: APIRequestContext, page: Page): Promise<SearchSeed> {
  const member = await seedMember(request, page, ['rag', 'chatbot'], {
    assistants: { mode: 'list', items: [] },
    models: { mode: 'list', items: ['e2e-embed', 'e2e-chat'] }, // hybrid search embeds the query; Ask-about-this runs the chat assistant
    endpoints: { mode: 'list', items: ['openai_compatible'] }, // an empty endpoint grant denies inference routing outright
  });

  const mkCol = async (name: string, audience: string[]) => {
    const res = await api(request, page, 'POST', '/api/v1/rag/collections', {
      name, chunk_size: 200, chunk_overlap: 20, acl: { audience },
    });
    if (!res.ok()) throw new Error(`collection ${name} failed (${res.status()}): ${await res.text()}`);
    return fieldString(res, 'id');
  };
  const stamp = `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
  const colA = await mkCol(`search-a-${stamp}`, [member.memberID]);
  const colB = await mkCol(`search-b-${stamp}`, [member.memberID]);
  const colSecret = await mkCol(`search-secret-${stamp}`, ['actor_outsider']);
  // Unique filenames: the member also reads setup's public collections, whose
  // own alpha/beta documents would otherwise collide with these rows.
  const docA = `alpha-contract-${stamp}.txt`;
  const docB = `beta-contract-${stamp}.txt`;
  const docSecret = `secret-contract-${stamp}.txt`;

  await uploadDoc(request, page, colA, docA,
    'The contract renewal clause requires notice ninety days before expiry.');
  await uploadDoc(request, page, colB, docB,
    'Contract pricing terms are reviewed each quarter by the committee.');
  await uploadDoc(request, page, colSecret, docSecret,
    'Secret contract annex the member must never see.');

  const modelRes = await api(request, page, 'GET', '/v1/models');
  if (!modelRes.ok()) throw new Error(`GET /v1/models failed (${modelRes.status()}) — no model catalog to seed an assistant`);
  const model = await firstModelID(modelRes);
  const botRes = await api(request, page, 'POST', '/api/v1/chatbots', {
    name: `Search E2E ${Date.now()}`, model,
    rag_collection_ids: [colA, colB], audience: ['*'],
  });
  if (!botRes.ok()) throw new Error(`assistant create failed (${botRes.status()}): ${await botRes.text()}`);
  const assistantID = await fieldString(botRes, 'id');

  // Grant the member this assistant (stored mode:list grants — D-25).
  const roleRes = await api(request, page, 'POST', '/api/v1/identities/roles', {
    name: `search-ask-${Date.now()}-${Math.floor(Math.random() * 1e6)}`, scopes: ['rag', 'chatbot'],
    grants: { assistants: { mode: 'list', items: [assistantID] } },
  });
  if (!roleRes.ok()) throw new Error(`granted role create failed (${roleRes.status()}): ${await roleRes.text()}`);
  const grantRoleID = await fieldString(roleRes, 'id');
  const assign = await api(request, page, 'PUT', `/api/v1/identities/users/${member.memberID}/roles/${grantRoleID}`);
  if (!assign.ok()) throw new Error(`role assign failed (${assign.status()}): ${await assign.text()}`);

  // A first evidence id from the JSON search API (admin view).
  const searchRes = await api(request, page, 'POST', '/api/v1/rag/search', {
    query: 'contract', mode: 'lexical', collection_ids: [colA],
  });
  if (!searchRes.ok()) throw new Error(`seed search failed (${searchRes.status()}): ${await searchRes.text()}`);
  const evidenceID = await firstEvidenceID(searchRes);
  return { colA, colB, colSecret, docA, docB, docSecret, evidenceID, memberEmail: member.email, memberPass: member.pass, memberID: member.memberID, assistantID };
}

test.describe('Search (OA-33)', () => {
  test('member sees fused results from both readable collections, facet chips filter with real counts, and restricted content never leaks', async ({ page, request }) => {
    test.setTimeout(180_000);
    await login(page);
    const seed = await seedSearchFixtures(request, page);
    await page.context().clearCookies();
    await login(page, seed.memberEmail, seed.memberPass);

    await page.goto(`${BASE}/dashboard/search`);
    await page.fill('#searchQuery', 'contract');
    await page.click('#searchForm button[type="submit"]');
    const results = page.locator('#searchResults .result');
    await expect(results.first()).toBeVisible({ timeout: 15_000 });

    // URL state round-trips: the module pushStates ?q=&mode= (D-19).
    await expect(page).toHaveURL(/\/dashboard\/search\?.*q=contract.*mode=hybrid/);

    // Multi-collection fusion: rows from both readable collections.
    await expect(page.locator('#searchResults .result .result-title', { hasText: seed.docA })).toBeVisible();
    await expect(page.locator('#searchResults .result .result-title', { hasText: seed.docB })).toBeVisible();

    // Restricted collection: no row, no title, no facet for it anywhere.
    await expect(page.locator('#searchResults')).not.toContainText('secret-contract');
    await expect(page.locator('#searchResults')).not.toContainText('Secret');

    // Facet chip: real count matches the visible filtered set.
    const rows = await results.count();
    const chip = page.locator('#searchResults .chip[data-facet="upload"]');
    await expect(chip).toBeVisible();
    await expect(chip).toContainText(String(rows));
    await chip.click();
    const visible = page.locator('#searchResults .result:visible');
    await expect(visible).toHaveCount(rows);
    await page.locator('#searchResults .chip[data-facet=""]').click(); // All resets

    // Collection-scoped fragment — the 09-01 contract shape.
    await page.selectOption('#searchCollection', seed.colA);
    await expect(page.locator('#searchResults .result', { hasText: seed.docA })).toBeVisible({ timeout: 15_000 });
    await expect(page.locator('#searchResults .result', { hasText: seed.docB })).toHaveCount(0);

    // Semantic mode executes — a 5xx/empty fragment here fails naming the
    // embedding prerequisite, never a skip.
    await page.selectOption('#searchCollection', '');
    await page.locator('.chip[data-mode="semantic"]').click();
    await expect(page.locator('#searchResults .result').first()).toBeVisible({ timeout: 30_000 });
    await expect(page).toHaveURL(/mode=semantic/);
  });

  test('evidence opens the shared #drawer at desktop width and Ask about this runs authorized — plus the revocation denial', async ({ page, request }) => {
    test.setTimeout(300_000);
    await login(page);
    const seed = await seedSearchFixtures(request, page);
    await page.context().clearCookies();
    await login(page, seed.memberEmail, seed.memberPass);

    await page.goto(`${BASE}/dashboard/search`);
    await page.fill('#searchQuery', 'contract');
    await page.click('#searchForm button[type="submit"]');
    await expect(page.locator('#searchResults .result').first()).toBeVisible({ timeout: 15_000 });

    // Evidence drawer: shared #drawer at every width — no Search container.
    const evBtn = page.locator('#searchResults [data-action="evidence:open"]').first();
    await expect(evBtn).toBeVisible();
    const evidenceID = await evBtn.getAttribute('data-evidence-id');
    if (!evidenceID) throw new Error('result row carried no data-evidence-id — the evidence persistence contract (08-08) is missing');
    await evBtn.click();
    const drawer = page.locator('#drawer .evidence-card');
    await expect(drawer).toBeVisible({ timeout: 15_000 });
    await expect(drawer).toContainText('alpha-contract');
    await expect(drawer.locator('.quote')).toBeVisible();
    await drawer.locator('[data-action="evidence:close"]').click();

    // Ask about this: a real link carrying ONLY the evidence id.
    const ask = page.locator('#searchResults a[data-action="search:ask-about"]').first();
    const href = await ask.getAttribute('href');
    if (!href || !href.startsWith('/dashboard/chat?evidence=') || href.includes('contract')) {
      throw new Error(`ask-about link must carry only the evidence id, got ${href}`);
    }
    await ask.click();
    await page.waitForURL(/\/dashboard\/chat/);
    await expect(page.locator('#chatComposer')).toHaveAttribute('data-evidence', evidenceID);

    // The real authorized run: complete and assert the persisted turn carries
    // the evidence ids (server re-authorized at StartRun — never an
    // intercepted payload).
    await page.selectOption('#assistantSelect', seed.assistantID);
    await page.fill('#composer', 'Summarise the attached contract passage.');
    await page.click('[data-action="chat:send"]');
    await expect(page.locator('article.message[data-run-id]').first()).toBeVisible({ timeout: 15_000 });
    await awaitTerminal(page);
    const turn = page.locator(`article.message[data-evidence-ids*="${evidenceID}"]`);
    await expect(turn.first()).toBeVisible();

    // Revocation: remove the document, then the same handoff must be denied
    // server-side — surfaced via the shared error toast, never a fake success.
    await page.context().clearCookies();
    await login(page);
    const docsRes = await api(request, page, 'GET', `/api/v1/rag/collections/${seed.colA}/documents`);
    const docsPayload: unknown = await docsRes.json();
    let docID = '';
    if (typeof docsPayload === 'object' && docsPayload !== null && 'documents' in docsPayload) {
      const docs: unknown = (docsPayload as { documents: unknown }).documents;
      if (Array.isArray(docs)) {
        for (const d of docs) {
          if (typeof d === 'object' && d !== null && 'id' in d && typeof (d as { id: unknown }).id === 'string') {
            docID = (d as { id: string }).id;
          }
        }
      }
    }
    if (!docID) throw new Error('could not resolve the seeded document id for the revocation case');
    const del = await api(request, page, 'DELETE', `/api/v1/rag/documents/${docID}`);
    if (!del.ok()) throw new Error(`document delete failed (${del.status()}): ${await del.text()}`);

    await page.context().clearCookies();
    await login(page, seed.memberEmail, seed.memberPass);
    await page.goto(`${BASE}/dashboard/chat?evidence=${evidenceID}`);
    await page.selectOption('#assistantSelect', seed.assistantID);
    await page.fill('#composer', 'Try the revoked passage.');
    await page.click('[data-action="chat:send"]');
    await expect(page.locator('#toast')).toBeVisible({ timeout: 15_000 });
    // The denied send never adopts a conversation or starts a runline.
    await expect(page.locator('#chatThread .runline')).toHaveCount(0);
  });

  test('recents are namespaced per identity across logout and Back/Forward restores without new history entries at 390px', async ({ page, request }) => {
    test.setTimeout(240_000);
    await page.setViewportSize({ width: 390, height: 844 });
    await login(page);
    const seed = await seedSearchFixtures(request, page);
    await page.context().clearCookies();
    await login(page, seed.memberEmail, seed.memberPass);

    // Two searches seed the member's recents bucket.
    await page.goto(`${BASE}/dashboard/search`);
    await page.fill('#searchQuery', 'contract');
    await page.click('#searchForm button[type="submit"]');
    await expect(page.locator('#searchResults .result').first()).toBeVisible({ timeout: 15_000 });
    await page.fill('#searchQuery', 'pricing');
    const pricedResults = page.waitForResponse((r) => r.request().method() === 'GET' && r.url().includes('q=pricing'));
    await page.click('#searchForm button[type="submit"]');
    await expect(page).toHaveURL(/q=pricing/, { timeout: 15_000 });
    await pricedResults; // navigating earlier aborts the search before recents record

    // Reload with no ?q shows this user's recents; clicking refills + submits.
    await page.goto(`${BASE}/dashboard/search`);
    const recents = page.locator('#searchRecents [data-action="search:recent"]');
    await expect(recents.first()).toBeVisible();
    await expect(page.locator('#searchRecents')).toContainText('pricing');
    await expect(page.locator('#searchRecents')).toContainText('contract');

    // Back/Forward across distinct states restores results without pushing.
    await page.goto(`${BASE}/dashboard/search`);
    await page.fill('#searchQuery', 'contract');
    await page.click('#searchForm button[type="submit"]');
    await expect(page).toHaveURL(/q=contract/, { timeout: 15_000 });
    await page.fill('#searchQuery', 'pricing');
    await page.click('#searchForm button[type="submit"]');
    await expect(page).toHaveURL(/q=pricing/, { timeout: 15_000 });
    await page.goBack();
    await expect(page).toHaveURL(/q=contract/, { timeout: 15_000 });
    await expect(page.locator('#searchResults .result').first()).toBeVisible();
    await page.goForward();
    await expect(page).toHaveURL(/q=pricing/, { timeout: 15_000 });
    // A restoration never pushes: going Back again must not loop forward.
    await page.goBack();
    await expect(page).toHaveURL(/q=contract/, { timeout: 15_000 });

    // Shared browser: a different user's recents bucket is their own/empty —
    // never the first user's queries.
    await page.context().clearCookies();
    await login(page);
    await page.goto(`${BASE}/dashboard/search`);
    await expect(page.locator('#searchRecents')).not.toContainText('contract');
    await expect(page.locator('#searchRecents')).not.toContainText('pricing');
  });
});
