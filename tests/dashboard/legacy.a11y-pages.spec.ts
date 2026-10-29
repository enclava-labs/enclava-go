import { test, expect, Page, APIRequestContext } from '@playwright/test';

// a11y-pages.spec.ts — plan 11-05's OWNED page-flow keyboard suite (OA-38):
//   · citation button → evidence opens → close returns focus to the button
//   · composer Enter sends, Shift+Enter inserts a newline, focus stays put
//   · Sources wizard Continue lands focus on the next step's first control
//   · data-confirm destructives complete armed → confirmed by keyboard alone
//   · reduced-motion emulation: transitions/animations honor the media query
//
// Shared chrome cases (overlay trap internals, skip-link, contrast, 390px)
// live in 11-04's tests/a11y.spec.ts — nothing duplicated here. Real login,
// real /api/v1 seeding. A missing prerequisite FAILS the case naming what is
// absent — never test.skip on a required flow.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

function requireAdmin() {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error(
      'a11y-pages.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for a real logged-in ' +
      'session — seed the admin and set both; missing prerequisites fail, never skip',
    );
  }
}

async function login(page: Page) {
  requireAdmin();
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

// Session-authenticated /api/v1 helper (same contract as chat-search.spec).
async function api(request: APIRequestContext, page: Page, method: string, path: string, body?: unknown) {
  const csrf = (await page.context().cookies()).find((c) => c.name === 'enclava_csrf');
  const sess = (await page.context().cookies()).find((c) => c.name === 'enclava_session');
  return request.fetch(`${BASE}${path}`, {
    method,
    headers: {
      Cookie: sess ? `enclava_session=${sess.value}` : '',
      'X-CSRF-Token': csrf ? csrf.value : '',
      ...(body instanceof FormData ? {} : { 'Content-Type': 'application/json' }),
    },
    multipart: body instanceof FormData ? body : undefined,
    data: body instanceof FormData ? undefined : body ? JSON.stringify(body) : undefined,
  });
}

async function fieldString(res: { json(): Promise<unknown> }, field: string): Promise<string> {
  const value: unknown = await res.json();
  if (typeof value === 'object' && value !== null && field in value) {
    const v: unknown = value[field];
    if (typeof v === 'string') return v;
  }
  throw new Error(`response is missing string field "${field}"`);
}

// ── Seeding (mirrors chat-search.spec.ts — real API surface only) ────────

interface Seed {
  assistantID: string;
  collectionID: string;
  evidenceID: string;
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
  throw new Error('GET /v1/models returned no usable model id — the deployment has no model catalog');
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
    'search returned no evidence_id — the evidence passage contract is not satisfied for the seeded document',
  );
}

async function seedChatFixtures(request: APIRequestContext, page: Page): Promise<Seed> {
  const modelRes = await api(request, page, 'GET', '/v1/models');
  if (!modelRes.ok()) {
    throw new Error(`GET /v1/models failed (${modelRes.status()}) — no model catalog to seed an assistant from`);
  }
  const model = await firstModelID(modelRes);

  const colRes = await api(request, page, 'POST', '/api/v1/rag/collections', {
    name: `a11y-e2e-${Date.now()}`,
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

  await expect
    .poll(
      async () => {
        const res = await api(request, page, 'GET', `/api/v1/rag/collections/${collectionID}/documents?status=ready`);
        if (!res.ok()) return 0;
        const payload: unknown = await res.json();
        if (typeof payload === 'object' && payload !== null && 'documents' in payload) {
          const docs: unknown = payload.documents;
          if (Array.isArray(docs)) return docs.length;
        }
        return 0;
      },
      { timeout: 60_000, message: 'document ingestion never reached ready — check the embedding provider' },
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
    name: `A11Y Reviewer ${Date.now()}`,
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

async function sendFirstMessage(page: Page, assistantID: string, text: string): Promise<string> {
  await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(assistantID)}`);
  await page.fill('#composer', text);
  await page.click('[data-action="chat:send"]');
  await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);
  return page.url().split('/').pop() as string;
}

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

// ── Cases ────────────────────────────────────────────────────────────────

test.describe('Page-surface keyboard flows (OA-38 / 11-05)', () => {
  test('citation button opens evidence and close returns focus to that button', async ({ page, request }) => {
    test.setTimeout(180_000);
    await login(page);
    const seed = await seedChatFixtures(request, page);

    await sendFirstMessage(
      page,
      seed.assistantID,
      'What is the enclave renewal window? Cite your source as [1].',
    );
    await awaitTerminal(page);

    const citations = page.locator('article.message .bubble button.citation');
    if ((await citations.count()) === 0) {
      throw new Error(
        'the assistant answer carried no [n] citation markers — the citation→evidence contract needs a grounded answer',
      );
    }
    const citation = citations.first();

    // Keyboard-only open: Tab-reachable real button, Enter activates.
    await citation.focus();
    await expect(citation).toBeFocused();
    await citation.press('Enter');
    await expect(page.locator('.evidence-panel')).toContainText('renewals.txt', { timeout: 15000 });

    // Keyboard close (Escape or the named close control) returns focus to
    // the activating citation button.
    const close = page.locator('.evidence-panel [data-action="evidence:close"]');
    await expect(close).toBeVisible();
    await close.focus();
    await close.press('Enter');
    await expect(citation).toBeFocused({ timeout: 5000 });
  });

  test('composer Enter sends and Shift+Enter inserts a newline without moving focus', async ({ page, request }) => {
    test.setTimeout(180_000);
    await login(page);
    const seed = await seedChatFixtures(request, page);

    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.assistantID)}`);
    const composer = page.locator('#composer');
    await composer.focus();

    // Shift+Enter keeps focus and grows the value with a real newline.
    await composer.pressSequentially('line one');
    await composer.press('Shift+Enter');
    await composer.pressSequentially('line two');
    await expect(composer).toHaveValue(/line one\nline two/);
    await expect(composer).toBeFocused();

    // Enter alone submits — the run POST fires and the composer clears,
    // focus never leaving the page flow (it stays on the composer or moves
    // into the thread's own content, never off-page chrome).
    const runPost = page.waitForRequest((r) => r.method() === 'POST' && r.url().endsWith('/api/v1/agent-runs'));
    await composer.press('Enter');
    await runPost;
    await expect(composer).toHaveValue('');
    await awaitTerminal(page);
  });

  test('Sources wizard Continue lands focus on the next step’s first control', async ({ page, request }) => {
    test.setTimeout(90_000);
    await login(page);

    // A writable collection is the step-4 prerequisite — missing rag.manage
    // in the seeded admin fails loudly, never skips.
    const colRes = await api(request, page, 'POST', '/api/v1/rag/collections', {
      name: `a11y-wizard-${Date.now()}`,
      acl: {}, // owner-only: public ACLs inflate the member unscoped-search work bound
    });
    if (!colRes.ok()) {
      throw new Error(
        `POST /api/v1/rag/collections failed (${colRes.status()}) — the seeded admin needs rag.manage for the wizard destination step`,
      );
    }
    const collectionID = await fieldString(colRes, 'id');

    await page.goto(`${BASE}/dashboard/sources?new=1`);
    const drawer = page.locator('#drawer');
    await expect(drawer).toHaveClass(/open/);

    // Step 1: provider tiles are keyboard-operable toggle buttons.
    const tile = drawer.locator('.tile[data-provider="github"]');
    await tile.focus();
    await tile.press('Enter');
    await expect(tile).toHaveAttribute('aria-pressed', 'true');

    // Step 1→2 happens on the tile pick: focus lands on the Connect form's
    // first control. Continue stays disabled until the required fields fill.
    const step2First = drawer.locator('[data-provider-fields="github"]:not([hidden]) input').first();
    await expect(step2First).toBeFocused({ timeout: 5000 });

    const cont = drawer.getByRole('button', { name: 'Continue' });
    // Fill required step-2 fields and continue — focus lands in step 3
    // (GitHub access is informational, so the step's own Continue is the
    // reachable control inside the pane flow).
    await drawer.locator('#github-owner').fill('acme');
    await drawer.locator('#github-repo').fill('repo');
    await expect(cont).toBeEnabled();
    await cont.focus();
    await cont.press('Enter');
    await expect(drawer.locator('[data-step-pane="3"]')).toBeVisible();

    // Step 3 → 4: focus reaches the destination pane's first control.
    await cont.focus();
    await cont.press('Enter');
    await expect(drawer.locator('#source-name')).toBeFocused({ timeout: 5000 });

    // Step 4 → 5: review pane is read-only — focus falls back to the
    // submit control so keyboard users land inside the step.
    await drawer.locator('#source-name').fill(`a11y src ${Date.now()}`);
    await drawer.locator('#source-collection').selectOption(collectionID);
    await cont.focus();
    await cont.press('Enter');
    const submit = drawer.getByRole('button', { name: 'Create source and sync' });
    await expect(submit).toBeVisible();
    await expect(submit).toBeFocused({ timeout: 5000 });

    // Back returns focus into the previous step's first control.
    const back = drawer.getByRole('button', { name: 'Back' });
    await back.focus();
    await back.press('Enter');
    await expect(drawer.locator('#source-name')).toBeFocused({ timeout: 5000 });
  });

  test('data-confirm destructives arm and confirm entirely by keyboard', async ({ page, request }) => {
    test.setTimeout(120_000);
    await login(page);
    const seed = await seedChatFixtures(request, page);

    await sendFirstMessage(page, seed.assistantID, 'Long-running question — summarise everything in the knowledge base.');

    const stop = page.locator('[data-run-id][data-action="confirm"]');
    await expect(stop).toBeVisible();

    // Keyboard-only: focus, Enter arms (visible armed text — no tooltip),
    // second Enter fires enclava:confirm → DELETE.
    await stop.focus();
    await stop.press('Enter');
    await expect(stop).toHaveText(/Stop run\?/);
    await expect(stop).toBeFocused();
    const deleteRun = page.waitForRequest((r) => r.method() === 'DELETE' && /\/api\/v1\/agent-runs\/.+/.test(r.url()));
    await stop.press('Enter');
    await deleteRun;
    await expect(page.locator('.thread-body')).toContainText('Stopped by you', { timeout: 30_000 });
  });

  test('reduced-motion emulation strips decorative animation and transitions', async ({ page }) => {
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await login(page);
    // Seed a live run so the Running-now row actually renders its pulse —
    // the stub holds 'Long-running' answers open ~20s.
    const runSeed = await seedChatFixtures(request, page);
    const runRes = await api(request, page, 'POST', '/api/v1/agent-runs', {
      assistant_id: runSeed.assistantID,
      input: 'Long-running question — summarise everything in the knowledge base.',
    });
    if (!runRes.ok() && runRes.status() !== 202) {
      throw new Error(`run seed for the reduced-motion row failed (${runRes.status()}): ${await runRes.text()}`);
    }
    await page.goto(`${BASE}/dashboard`);
    // under reduce, and must still be readable (the pill, not motion,
    // carries the state).
    const pulse = page.locator('.pulse').first();
    if ((await pulse.count()) === 0) {
      throw new Error('no .pulse rendered on Home — seed or render a non-terminal row to exercise reduced motion');
    }
    const pulseAnim = await pulse.evaluate((el) => getComputedStyle(el).animationName);
    expect(['none', '']).toContain(pulseAnim);

    const drawerTransition = await page.locator('#drawer').evaluate((el) => getComputedStyle(el).transitionProperty);
    expect(['none', 'all', '']).toContain(drawerTransition);
  });
});
