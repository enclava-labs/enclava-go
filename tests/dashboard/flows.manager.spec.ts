import { test, expect, Route } from '@playwright/test';
import type { Locator, Page } from '@playwright/test';

declare global {
  interface Window {
    Enclava: { api(method: string, path: string, body?: unknown): Promise<unknown> };
  }
}

import {
  BASE, MANAGER_EMAIL, MEMBER_EMAIL, MEMBER_PASSWORD, login, seedData, api, sessionApi,
} from './fixtures';

// flows.manager.spec.ts — knowledge-manager §9.2 flows 3–4 plus the
// migrated inventories (see 12-04 SUMMARY case map):
//   tests/sources.spec.ts (09-02) — the five-step wizard create→chained
//   first-sync chain, the Teams channel-subset payload, the
//   ?new=1&collection= preset.
//   tests/a11y.spec.ts (4b) — extract worker failure: the capability-
//   filtered fix on the failed job row (manager lacks models.manage →
//   administrator fallback; the admin side lives in flows.admin).
//   tests/a11y.spec.ts (6) — the Reconnect ?edit= deep link: visible to
//   connectors.manage, absent for a member.
//
// Flow 4 is the mandatory SUCCESSFUL source creation: the wizard drives the
// real POST /api/v1/connectors/sources → 201 through the declared
// CONNECTOR_GITHUB_E2E credential fixture (HTTPS repo-API stub + test CA),
// and the chained first sync hits the stub for real — an errored submit
// never counts.

async function newWritableCollection(page: Page, seed: { embedModel: string }) {
  // The wizard's destination select offers manager-owned collections only
  // (collectionManageable) — seed collections are admin-owned, so tests
  // mint a writable scratch collection through the manager session.
  const res = await sessionApi(page, 'POST', '/api/v1/rag/collections', {
    name: `e2e-wiz-${Date.now()}`,
    embedding_model: seed.embedModel,
    // Owner-only ACL — a public collection inflates every member's
    // unscoped-search work bound (SearchWorkExceeded past ~20 readable).
    acl: {},
  });
  if (!res.ok()) throw new Error(`wizard collection create failed: ${res.status()}`);
  return (await res.json()) as { id: string };
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
  await expect(drawer.locator(`[data-provider-fields="${provider}"]`)).toBeVisible({ timeout: 15_000 });
}

test.describe('Knowledge — collection documents (flow 3)', () => {
  test('upload, retry and delete a real document', async ({ page }) => {
    // Self-sufficient session: the setup-minted storage state may have been
    // revoked by earlier logins — never depend on it.
    await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
    const seed = seedData();
    // A scratch collection so the seed set stays stable for search/chat.
    // Create through the manager session — collectionManageable is
    // owner-only, so the admin-key principal cannot upload into it.
    const colRes = await sessionApi(page, 'POST', '/api/v1/rag/collections', {
      name: `e2e-scratch-${Date.now()}`,
      embedding_model: seed.embedModel,
      acl: {},
    });
    expect(colRes.ok(), 'scratch collection create').toBeTruthy();
    const col = await colRes.json() as { id: string };
    await page.goto(`${BASE}/dashboard/knowledge/${col.id}`);
    await expect(page.locator('h1')).toBeVisible();

    // Upload through the real file input the drawer path uses.
    await page.setInputFiles('#knowledge-upload-file', {
      name: 'e2e-upload.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from('Uploaded through the dashboard e2e harness.'),
    });
    const row = page.locator('#knowledge-documents tr', { hasText: 'e2e-upload.txt' });
    await expect(row).toBeVisible({ timeout: 15_000 });
    // Processing → Ready through the worker, polled by the fragment.
    await expect(row.locator('.pill')).toContainText(/Ready|Processing|Queued/, { timeout: 15_000 });
    await expect
      .poll(async () => row.locator('.pill').textContent(), { timeout: 90_000 })
      .toMatch(/Ready/);

    // Retry issues the real reprocess write.
    const reprocess = page.waitForRequest(
      (r) => r.method() === 'POST' && /\/api\/v1\/rag\/documents\/.+\/reprocess$/.test(r.url()));
    await row.locator('[data-action="knowledge:retry"]').click();
    await reprocess;

    // Delete through the shared two-step confirm.
    await expect
      .poll(async () => row.locator('.pill').textContent(), { timeout: 90_000 })
      .toMatch(/Ready/);
    const del = row.locator('[data-knowledge-doc-del]');
    const deleteReq = page.waitForRequest(
      (r) => r.method() === 'DELETE' && /\/api\/v1\/rag\/documents\/.+/.test(r.url()));
    await del.click();
    await del.click();
    await deleteReq;
    await expect(row).toHaveCount(0, { timeout: 15_000 });
  });
});

test.describe('Sources — add-source wizard (flow 4)', () => {
  test('wizard creates a GitHub source for real and the first sync reaches the fixture', async ({ page }) => {
    test.setTimeout(180_000);
    await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
    const seed = seedData();
    const connector = process.env.E2E_CONNECTOR_URL || '';
    if (!connector) throw new Error('E2E_CONNECTOR_URL unset — the credential fixture is missing');
    const name = `E2E GitHub source ${Date.now()}`;
    const col = await newWritableCollection(page, seed);

    const drawer = await openWizard(page);
    await expect(drawer.getByRole('button', { name: 'Continue' })).toBeDisabled();
    await pickProvider(drawer, 'github');
    await drawer.locator('#github-owner').fill('e2e');
    await drawer.locator('#github-repo').fill('e2e-docs');
    // config.api_url — never base_url: loopback base_url is rejected and
    // the adapter ignores it; the fixture's HTTPS origin is the real one.
    await drawer.locator('#github-api').fill(connector);
    // The declared credential: the run's SECRET_BINDINGS entry offers it.
    const cred = drawer.locator('#github-credential');
    await expect(drawer.locator('#cred-github option[value="CONNECTOR_GITHUB_E2E"]')).toHaveCount(1);
    await cred.fill('CONNECTOR_GITHUB_E2E');
    await expect(drawer.locator('[data-provider-access="github"]')).toContainText('collaborator');
    await drawer.getByRole('button', { name: 'Continue' }).click();
    await drawer.getByRole('button', { name: 'Continue' }).click();

    const createBodies: Record<string, unknown>[] = [];
    let createdID = '';
    page.on('response', (res) => {
      if (res.request().method() === 'POST' && res.url().endsWith('/api/v1/connectors/sources')) {
        res.json().then((data) => {
          createBodies.push(res.request().postDataJSON() as Record<string, unknown>);
          if (data && data.id) createdID = data.id;
        }).catch(() => undefined);
      }
    });
    const syncPosts: string[] = [];
    page.on('request', (r) => {
      if (r.method() === 'POST' && /\/api\/v1\/connectors\/sources\/.+\/sync$/.test(r.url())) {
        syncPosts.push(r.url());
      }
    });

    await drawer.locator('[data-source-name]').fill(name);
    await drawer.locator('[data-source-collection]').selectOption(col.id);
    await drawer.locator('[data-source-interval]').fill('30m');
    await drawer.getByRole('button', { name: 'Continue' }).click();
    await drawer.getByRole('button', { name: 'Create source and sync' }).click();
    await expect(drawer).not.toHaveClass(/open/, { timeout: 15_000 });

    // Real 201 + toast + the Syncing row — the mandatory success bar.
    await expect(page.locator('#toast')).toContainText('first sync started');
    expect(createBodies).toHaveLength(1);
    const body = createBodies[0];
    expect(body.type).toBe('github');
    expect(body.name).toBe(name);
    expect(body.target_collection_id).toBe(col.id);
    expect(body.is_enabled).toBe(true);
    expect(body.credential_ref).toBe('CONNECTOR_GITHUB_E2E');
    const config = body.config as Record<string, unknown>;
    expect(config.owner).toBe('e2e');
    expect(config.repo).toBe('e2e-docs');
    expect(config.api_url).toBe(connector);
    expect(config.base_url).toBeUndefined();
    expect(syncPosts.length).toBeGreaterThanOrEqual(1);
    expect(syncPosts[0]).toContain(`/api/v1/connectors/sources/`);

    const row = page.locator('#sources-table tr', { hasText: name });
    await expect(row).toContainText('GitHub', { timeout: 15_000 });
    await expect(row).toContainText('e2e/e2e-docs@HEAD');

    // The first sync must talk to the stub for real: poll the API until the
    // row leaves running — success means the adapter fetched the repo API.
    if (!createdID) {
      await expect
        .poll(async () => (await api<{ sources?: Array<{ id: string; name: string }> }>(
          'GET', '/api/v1/connectors/sources'))
          .sources?.find((s) => s.name === name)?.id ?? '', { timeout: 15_000 })
        .not.toBe('');
      createdID = (await api<{ sources?: Array<{ id: string; name: string }> }>(
        'GET', '/api/v1/connectors/sources')).sources?.find((s) => s.name === name)?.id ?? '';
    }
    const syncStatus = await expect
      .poll(async () => {
        const src = await api<{ last_error?: string; last_sync_at?: string | null }>(
          'GET', `/api/v1/connectors/sources/${createdID}`);
        return src.last_sync_at ? 'done' : (src.last_error ? `failed: ${src.last_error}` : 'running');
      }, { timeout: 120_000, message: 'first sync never reached a terminal state' })
      .toBe('done');
    void syncStatus;
  });

  test('Teams wizard keeps the team and channel subset in the payload', async ({ page }) => {
    await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
    const seed = seedData();
    // exist before the wizard opens.
    const col = await newWritableCollection(page, seed);
    const drawer = await openWizard(page);
    await pickProvider(drawer, 'teams');
    await drawer.locator('#teams-tenant').fill('tenant-1');
    await drawer.locator('#teams-client').fill('client-1');
    await drawer.locator('#teams-team').fill('team-a');
    await drawer.locator('#teams-channels').fill('team-a:chan-1\nteam-a:chan-2');
    await drawer.locator('#teams-chats').fill('chat-9');
    await expect(drawer.locator('[data-provider-fields="teams"]')).toContainText('No Teams credentials configured');
    await drawer.locator('#teams-credential').fill('CONNECTOR_TEAMS_SPEC');
    await drawer.getByRole('button', { name: 'Continue' }).click(); // → Access
    await drawer.getByRole('button', { name: 'Continue' }).click(); // → Destination


    const createBodies: Record<string, unknown>[] = [];
    const syncURLs: string[] = [];
    await page.route('**/api/v1/connectors/sources', async (route: Route) => {
      createBodies.push(JSON.parse(route.request().postData() ?? '{}'));
      // Payload-shape case: no Teams fixture exists — fulfilling locally
      // keeps the assertion on the wire body, never a fake success claim.
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
    await drawer.locator('[data-source-collection]').selectOption(col.id);
    await drawer.getByRole('button', { name: 'Continue' }).click();
    await drawer.getByRole('button', { name: 'Create source and sync' }).click();
    await expect(drawer).not.toHaveClass(/open/, { timeout: 15_000 });

    expect(createBodies).toHaveLength(1);
    const config = createBodies[0].config as Record<string, unknown>;
    expect(config.team_id).toBe('team-a');
    expect(config.team_ids).toBeUndefined();
    expect(config.channel_ids).toEqual({ 'team-a': ['chan-1', 'chan-2'] });
    expect(config.chat_ids).toEqual(['chat-9']);
    expect(config.tenant_id).toBe('tenant-1');
    expect(config.client_id).toBe('client-1');
    expect(Object.keys(config).sort()).toEqual(
      ['channel_ids', 'chat_ids', 'client_id', 'team_id', 'tenant_id']);
    expect(syncURLs).toEqual([`${BASE}/api/v1/connectors/sources/connsrc_spec_fixture/sync`]);
  });

  test('?new=1&collection={id} presets the destination step', async ({ page }) => {
    const seed = seedData();
    const col = await newWritableCollection(page, seed);
    const drawer = await openWizard(page, `?new=1&collection=${col.id}`);
    await pickProvider(drawer, 'github');
    await drawer.locator('#github-owner').fill('e2e');
    await drawer.locator('#github-repo').fill('preset');
    await drawer.getByRole('button', { name: 'Continue' }).click();
    await expect(drawer.locator('[data-source-collection]')).toHaveValue(col.id);
  });

  test('?edit= opens the credential step; members never see it', async ({ page }) => {
    const connector = process.env.E2E_CONNECTOR_URL || '';
    const src = await api<{ id: string }>('POST', '/api/v1/connectors/sources', {
      type: 'github',
      name: `e2e-edit-${Date.now()}`,
      credential_ref: 'CONNECTOR_GITHUB_E2E',
      config: { owner: 'e2e', repo: 'e2e-docs', api_url: connector },
    });
    await page.goto(`${BASE}/dashboard/sources?edit=${encodeURIComponent(src.id)}`);
    const drawer = page.locator('#drawer');
    await expect(drawer).toHaveClass(/open/, { timeout: 15_000 });
    await expect(drawer.locator('[data-source-edit]')).toBeVisible();
    await expect(drawer.locator('[data-edit-credential]')).toBeVisible();

    // Members lack connectors.manage — the page denies or renders no
    // Reconnect affordance.
    await page.context().clearCookies();
    await login(page, MEMBER_EMAIL, MEMBER_PASSWORD);
    const res = await page.goto(`${BASE}/dashboard/sources?edit=${encodeURIComponent(src.id)}`);
    if (res && res.status() === 200) {
      await expect(drawer).not.toHaveClass(/open/);
      await expect(page.locator('[data-source-edit]')).toHaveCount(0);
    } else {
      expect([403, 404]).toContain(res!.status());
    }
  });
});

test.describe('manager error surfaces', () => {
  test('manager rejected write renders a sticky toast with no fix link', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/sources`);
    await page.evaluate(async () => {
      try {
        await window.Enclava.api('POST', '/api/v1/admin/api-keys', { name: 'manager-probe' });
      } catch { /* rejection is the expected outcome */ }
    });
    const toast = page.locator('#toast.show.error');
    await expect(toast).toBeVisible();
    await expect(toast.locator('a[href]')).toHaveCount(0);
  });

  test('the template editor drawer opens from the Templates tab', async ({ page }) => {
    await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
    await page.goto(`${BASE}/dashboard/extract?tab=templates`);
    await page.locator('[data-action="extract:edit-template"][data-template-id]').first().click();
    const drawer = page.locator('#drawer');
    await expect(drawer).toHaveClass(/open/);
    // Regression: the editor markup begins with whitespace, which defeated
    // openDrawer's inline-HTML sniff and fetched the markup as a URL (404).
    await expect(drawer.locator('[data-editor-title]')).toHaveText('Edit template');
    await expect(drawer.locator('[data-editor-save]')).toBeVisible();
  });

  test('extract worker failure surfaces the administrator fallback on the failed job row', async ({ page }) => {
    test.setTimeout(120_000);
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/extract`);
    const select = page.locator('[data-extract-template-select]');
    await select.selectOption(seed.extractTemplateID);
    await page.setInputFiles('[data-extract-file]', {
      name: 'e2e-manager.txt',
      buffer: Buffer.from(`manager extract probe ${Date.now()}`),
    });
    // Wait for the submit's POST: navigating away first would abort it.
    const submitted = page.waitForResponse((r) => r.request().method() === 'POST' && r.url().includes('/api/v1/extract/jobs'));
    await page.click('[data-action="extract:submit"]');
    const res = await submitted;
    expect(res.status(), await res.text()).toBe(202);
    await page.goto(`${BASE}/dashboard/extract?tab=jobs`);
    // Accepted job fails at the worker's price reserve (the template's
    // model is deliberately unpriced) — capability-off copy, no href.
    const failed = page.locator('#extract-jobs tr', { hasText: 'e2e-manager.txt' }).first();
    await expect(failed.locator('.pill.bad')).toBeVisible({ timeout: 90_000 });
    await expect(failed.locator('.subline.bad')).toContainText('administrator');
    await expect(failed.locator('a[href]')).toHaveCount(0);
  });
});
