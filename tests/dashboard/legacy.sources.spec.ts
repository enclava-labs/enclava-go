import { Page, Route, type Locator } from '@playwright/test';
import { test, expect } from './fixtures';

// sources.spec.ts — Sources add-source wizard (OA-23, plan 09-02) against the
// served dashboard: the wizard create→chained-first-sync request chain, the
// Teams channel-subset payload shape, the ?new=1[&collection=] deep link and
// the refreshed row state in real markup.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

// Seeded admin (bootstrap role_admin) carries platform.admin + rag.manage +
// connectors.manage — the Sources page needs connectors.manage explicitly
// (platform.admin implies no feature caps) and rag.manage for the
// destination collection.
function requireSession() {
  if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
    throw new Error(
      'sources.spec needs ADMIN_EMAIL and ADMIN_PASSWORD for a real logged-in ' +
      'session with the seeded admin role (connectors.manage + rag.manage) — ' +
      'seed the app and set both; missing prerequisites fail, never skip',
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

interface Created {
  csrf: string;
  collectionID: string;
}

// A writable destination collection for the wizard, made through the real
// API the server exposes (rag.manage on the seeded admin).
async function ensureCollection(page: Page): Promise<Created> {
  const csrf = (await page.context().cookies())
    .find((c) => c.name === 'enclava_csrf')?.value ?? '';
  const res = await page.request.post(`${BASE}/api/v1/rag/collections`, {
    headers: { 'X-CSRF-Token': csrf, Origin: BASE },
    data: { name: `Sources spec ${Date.now()}` },
  });
  if (res.status() !== 201) {
    throw new Error(
      `could not create a destination collection (POST /api/v1/rag/collections ` +
      `→ ${res.status()}) — the seeded admin needs rag.manage; body: ${await res.text()}`,
    );
  }
  return { csrf, collectionID: (await res.json()).id };
}

async function openWizard(page: Page, query = '?new=1') {
  await page.goto(`${BASE}/dashboard/sources${query}`);
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/);
  await expect(drawer.locator('.tile')).toHaveCount(5);
  return drawer;
}

async function pickProvider(drawer: Locator, provider: string) {
  // Tile selection advances straight into the provider's Connect pane.
  await drawer.locator(`.tile[data-provider="${provider}"]`).click();
  await expect(drawer.locator(`[data-provider-fields="${provider}"]`)).toBeVisible();
}

// Walks step 4 (destination + schedule) and step 5 (review), submits, and
// returns the captured create/sync requests.
async function submitWizard(
  page: Page,
  drawer: Locator,
  opts: { name: string; collectionID: string; interval?: string; enabled?: boolean },
) {
  await drawer.getByRole('button', { name: 'Continue' }).click(); // step 3 → destination
  await drawer.locator('[data-source-name]').fill(opts.name);
  await drawer.locator('[data-source-collection]').selectOption(opts.collectionID);
  if (opts.interval !== undefined) await drawer.locator('[data-source-interval]').fill(opts.interval);
  if (opts.enabled === false) await drawer.locator('[data-source-enabled]').uncheck();

  const createBody: { body?: Record<string, unknown> } = {};
  const syncURLs: string[] = [];
  await page.route('**/api/v1/connectors/sources', async (route: Route) => {
    createBody.body = JSON.parse(route.request().postData() ?? '{}');
    await route.continue();
  });
  await page.route('**/api/v1/connectors/sources/*/sync', async (route: Route) => {
    syncURLs.push(route.request().url());
    await route.continue();
  });

  await drawer.getByRole('button', { name: 'Continue' }).click();
  await drawer.getByRole('button', { name: 'Create source and sync' }).click();
  await expect(drawer).not.toHaveClass(/open/, { timeout: 15000 });
  return { createBody, syncURLs };
}

test('wizard creates a GitHub source and chains the real first sync', async ({ page }) => {
  await login(page);
  const { collectionID } = await ensureCollection(page);
  const name = `Spec repo ${Date.now()}`;

  const drawer = await openWizard(page);
  // Step 1 gate: Continue stays disabled until a provider tile is chosen.
  await expect(drawer.getByRole('button', { name: 'Continue' })).toBeDisabled();
  await pickProvider(drawer, 'github');
  await drawer.locator('#github-owner').fill('enclava');
  await drawer.locator('#github-repo').fill('docs');
  // Step 3 states the collection read rule for GitHub, then destination.
  await expect(drawer.locator('[data-provider-access="github"]')).toContainText('Everyone who can open the target collection');
  await drawer.getByRole('button', { name: 'Continue' }).click();

  const createdID = observeCreatedSource(page);
  const { createBody, syncURLs } = await submitWizard(page, drawer, {
    name, collectionID, interval: '30m',
  });

  // The create POST carries the provider's keys only — no Slack/Teams/Notion
  // keys may leak — with sync_interval inside config and is_enabled on top.
  const body = createBody.body ?? {};
  expect(body.type).toBe('github');
  expect(body.name).toBe(name);
  expect(body.target_collection_id).toBe(collectionID);
  expect(body.is_enabled).toBe(true);
  expect(body.credential_ref).toBe('');
  const config = body.config as Record<string, unknown>;
  expect(config.owner).toBe('enclava');
  expect(config.repo).toBe('docs');
  expect(config.sync_interval).toBe('30m');
  expect(Object.keys(config).sort()).toEqual(['owner', 'repo', 'sync_interval']);

  // Create success ALWAYS chains the real first sync before toast/refresh.
  expect(syncURLs).toHaveLength(1);
  const id = await createdID(name);
  expect(syncURLs[0]).toContain(`/api/v1/connectors/sources/${id}/sync`);
  await expect(page.locator('#toast')).toContainText('first sync started');

  // The refreshed table shows the real persisted row from real markup.
  await expect(page.locator('#sources-table')).toContainText(name, { timeout: 15000 });
  const row = page.locator('#sources-table tr', { hasText: name });
  await expect(row).toContainText('GitHub');
  await expect(row).toContainText('enclava/docs@HEAD');
  await expect(row.getByRole('button', { name: 'Sync now' })).toBeAttached();

  // Cleanup so the spec is rerunnable against the same seed.
  const del = await page.request.delete(`${BASE}/api/v1/connectors/sources/${id}`, {
    headers: { 'X-CSRF-Token': (await page.context().cookies()).find((c) => c.name === 'enclava_csrf')?.value ?? '', Origin: BASE },
  });
  expect(del.status()).toBe(200);
});

// Records the create response body (the route continues to the server, so
// the row really persists) and resolves the created ID once observed.
function observeCreatedSource(page: Page) {
  const ids: Record<string, string> = {};
  const { promise, resolve } = Promise.withResolvers<void>();
  page.on('response', (res) => {
    if (res.request().method() === 'POST' && res.url().endsWith('/api/v1/connectors/sources')) {
      res.json().then((data) => {
        if (data && data.id && data.name && !ids[data.name]) {
          ids[data.name] = data.id;
          resolve();
        }
      }).catch(() => { /* non-JSON response — nothing to record */ });
    }
  });
  return async (name: string): Promise<string> => {
    while (!ids[name]) {
      await Promise.race([promise, delay(500)]);
    }
    return ids[name];
  };
}

const delay = (ms: number): Promise<void> => {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
};

test('Teams wizard keeps the team and channel subset in the payload', async ({ page }) => {
  await login(page);
  const { collectionID } = await ensureCollection(page);

  const drawer = await openWizard(page);
  await pickProvider(drawer, 'teams');
  await drawer.locator('#teams-tenant').fill('tenant-1');
  await drawer.locator('#teams-client').fill('client-1');
  await drawer.locator('#teams-team').fill('team-a');
  await drawer.locator('#teams-channels').fill('team-a:chan-1\nteam-a:chan-2');
  await drawer.locator('#teams-chats').fill('chat-9');
  // Zero refs bound in this environment → the truthful empty state shows and
  // the reference is typed (the server validates the CONNECTOR_TEAMS_* shape).
  await expect(drawer.locator('[data-provider-fields="teams"]')).toContainText('No Teams credentials configured');
  await drawer.locator('#teams-credential').fill('CONNECTOR_TEAMS_SPEC');
  await drawer.getByRole('button', { name: 'Continue' }).click();
  await drawer.getByRole('button', { name: 'Continue' }).click();
  // (Destination is the second Continue's pane; the review pane's submit
  // control only un-hides after a third.)


  const createBodies: Record<string, unknown>[] = [];
  const syncURLs: string[] = [];
  await page.route('**/api/v1/connectors/sources', async (route: Route) => {
    createBodies.push(JSON.parse(route.request().postData() ?? '{}'));
    // Fulfill locally: the payload shape is the assertion — nothing persists.
    await route.fulfill({
      status: 201,
      contentType: 'application/json',
      body: JSON.stringify({ id: 'connsrc_spec_fixture', type: 'teams', name: 'Spec teams' }),
    });
  });
  await page.route('**/api/v1/connectors/sources/*/sync', async (route: Route) => {
    syncURLs.push(route.request().url());
    await route.fulfill({
      status: 202,
      contentType: 'application/json',
      body: JSON.stringify({ id: 'jobspec', status: 'queued' }),
    });
  });

  await drawer.locator('[data-source-name]').fill('Spec teams');
  await drawer.locator('[data-source-collection]').selectOption(collectionID);
  await drawer.getByRole('button', { name: 'Continue' }).click(); // → review
  await drawer.getByRole('button', { name: 'Create source and sync' }).click();
  await expect(drawer).not.toHaveClass(/open/, { timeout: 15000 });

  expect(createBodies).toHaveLength(1);
  const config = createBodies[0].config as Record<string, unknown>;
  // Single team → team_id (never a one-element team_ids[]); the channel
  // subset is RETAINED in the adapter's allowedTeamChannel map; chat-backed
  // content rides chat_ids[]. Other providers' keys are absent.
  expect(config.team_id).toBe('team-a');
  expect(config.team_ids).toBeUndefined();
  expect(config.channel_ids).toEqual({ 'team-a': ['chan-1', 'chan-2'] });
  expect(config.chat_ids).toEqual(['chat-9']);
  expect(config.tenant_id).toBe('tenant-1');
  expect(config.client_id).toBe('client-1');
  expect(Object.keys(config).sort()).toEqual(
    ['channel_ids', 'chat_ids', 'client_id', 'team_id', 'tenant_id'],
  );
  // The fulfilled create still chains the sync POST — same success path.
  expect(syncURLs).toEqual([`${BASE}/api/v1/connectors/sources/connsrc_spec_fixture/sync`]);
});

test('?new=1&collection={id} presets the destination step', async ({ page }) => {
  await login(page);
  const { collectionID } = await ensureCollection(page);

  const drawer = await openWizard(page, `?new=1&collection=${collectionID}`);
  await pickProvider(drawer, 'github');
  await drawer.locator('#github-owner').fill('enclava');
  await drawer.locator('#github-repo').fill('preset');
  await drawer.getByRole('button', { name: 'Continue' }).click();
  await expect(drawer.locator('[data-source-collection]')).toHaveValue(collectionID);
});

test('Slack wizard offers the credential\'s channels in a dropdown', async ({ page }) => {
  await login(page);
  const { collectionID } = await ensureCollection(page);
  // The channel list is Slack's answer for the credential; fulfil it locally.
  const listed: string[] = [];
  await page.route('**/api/v1/connectors/slack/channels*', async (route: Route) => {
    listed.push(route.request().url());
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ channels: [
        { id: 'C1', name: 'general', is_private: false },
        { id: 'C2', name: 'random', is_private: true },
      ] }),
    });
  });

  const drawer = await openWizard(page);
  await pickProvider(drawer, 'slack');
  const pick = drawer.locator('#slack-channel');
  await expect(pick.locator('option')).toHaveText(['All channels the credential is a member of']);
  await drawer.locator('#slack-credential').fill('CONNECTOR_SLACK_SPEC');
  await expect(pick.locator('option')).toHaveText(['All channels the credential is a member of', '#general', '#random (private)']);
  await expect(drawer.locator('[data-slack-channels-status]')).toContainText('2 channels');
  expect(listed.at(-1)).toContain('credential_ref=CONNECTOR_SLACK_SPEC');
  await pick.selectOption('C2');

  await drawer.getByRole('button', { name: 'Continue' }).click();
  await drawer.getByRole('button', { name: 'Continue' }).click();
  const createBodies: Record<string, unknown>[] = [];
  await page.route('**/api/v1/connectors/sources', async (route: Route) => {
    createBodies.push(JSON.parse(route.request().postData() ?? '{}'));
    await route.fulfill({ status: 201, contentType: 'application/json', body: JSON.stringify({ id: 'connsrc_slack_spec', type: 'slack', name: 'Spec slack' }) });
  });
  await page.route('**/api/v1/connectors/sources/*/sync', (route: Route) =>
    route.fulfill({ status: 202, contentType: 'application/json', body: JSON.stringify({ id: 'jobspec', status: 'queued' }) }));
  await drawer.locator('[data-source-name]').fill('Spec slack');
  await drawer.locator('[data-source-collection]').selectOption(collectionID);
  await drawer.getByRole('button', { name: 'Continue' }).click(); // → review
  await expect(drawer.locator('[data-source-review]')).toContainText('channel: #random (private)');
  await drawer.getByRole('button', { name: 'Create source and sync' }).click();
  await expect(drawer).not.toHaveClass(/open/, { timeout: 15000 });
  expect(createBodies).toHaveLength(1);
  expect(createBodies[0].credential_ref).toBe('CONNECTOR_SLACK_SPEC');
  expect((createBodies[0].config as Record<string, unknown>).channel_id).toBe('C2');
});
