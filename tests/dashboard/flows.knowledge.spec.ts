import type { Page } from '@playwright/test';

import {
  BASE, MANAGER_EMAIL, MEMBER_PASSWORD, CSRF_COOKIE, login, seedData, api, sessionApi,
  test, expect,
} from './fixtures';

// flows.knowledge.spec.ts — v1.2 knowledge-surface coverage:
//   - the Chunking select on the collection create drawer and the settings
//     edit form actually drives chunk_strategy end to end;
//   - the collection-level "Retry failed" action POSTs to
//     /api/v1/rag/collections/{id}/reprocess-failed and reports the count;
//   - a search result title deep-links to the retained-copy URL.
// The retry and deep-link cases need package F's endpoint and package B's
// link fields — they run on the integrated v1.2 branch.

async function newCollectionViaUI(page: Page, chunking: string) {
  const name = `e2e-chunk-${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
  await page.goto(`${BASE}/dashboard/knowledge?new=1`);
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/);
  await drawer.locator('#knc-name').fill(name);
  await drawer.locator('#knc-chunking').selectOption(chunking);
  const created = page.waitForResponse(
    (r) => r.request().method() === 'POST' && r.url().endsWith('/api/v1/rag/collections'));
  await drawer.getByRole('button', { name: 'Create collection' }).click();
  const res = await created;
  expect(res.status(), await res.text()).toBe(201);
  const body = (await res.json()) as { id?: string };
  expect(body.id, 'create response carries the collection id').toBeTruthy();
  return { id: body.id as string, name };
}

test.describe('Knowledge — chunking controls (v1.2)', () => {
  test('Chunking select drives chunk_strategy on create and edit', async ({ page }) => {
    await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
    const col = await newCollectionViaUI(page, 'fixed');

    // The create POST carried fixed — read it back for truth.
    const created = await api<{ chunk_strategy?: string }>(
      'GET', `/api/v1/rag/collections/${col.id}`);
    expect(created.chunk_strategy).toBe('fixed');

    // Edit on the settings tab: the select shows the stored value; flipping
    // to adaptive and saving persists it.
    await page.goto(`${BASE}/dashboard/knowledge/${col.id}/settings`);
    const select = page.locator('#kcs-chunking');
    await expect(select).toHaveValue('fixed');
    await select.selectOption('auto');
    const saved = page.waitForResponse(
      (r) => r.request().method() === 'PUT' && r.url().endsWith(`/api/v1/rag/collections/${col.id}`));
    await page.locator('[data-knowledge-settings-form]').getByRole('button', { name: 'Save changes' }).click();
    const put = await saved;
    expect(put.status(), await put.text()).toBe(200);
    await expect
      .poll(async () => (await api<{ chunk_strategy?: string }>(
        'GET', `/api/v1/rag/collections/${col.id}`)).chunk_strategy)
      .toBe('auto');
  });
});

test.describe('Knowledge — retry failed (v1.2)', () => {
  test('Retry failed re-queues the collection\'s failed documents', async ({ page }) => {
    test.setTimeout(180_000);
    await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);

    // A collection bound to a model no endpoint serves: the embed job fails
    // deterministically and the document lands in failed — no worker tricks.
    const colRes = await sessionApi(page, 'POST', '/api/v1/rag/collections', {
      name: `e2e-fail-${Date.now()}`,
      embedding_model: 'e2e-embed-unbound',
      acl: {},
    });
    expect(colRes.ok(), `collection create: ${colRes.status()}`).toBeTruthy();
    const col = (await colRes.json()) as { id: string };

    const csrf = (await page.context().cookies()).find((c) => c.name === CSRF_COOKIE);
    const up = await page.request.fetch(
      `${BASE}/api/v1/rag/collections/${col.id}/documents`, {
        method: 'POST',
        headers: { 'X-CSRF-Token': csrf ? csrf.value : '', Origin: BASE },
        multipart: {
          file: {
            name: 'e2e-fail.txt',
            mimeType: 'text/plain',
            buffer: Buffer.from('document whose embedding model is unbound'),
          },
        },
      });
    expect(up.ok(), `upload: ${up.status()}`).toBeTruthy();

    await expect
      .poll(async () => {
        const res = await sessionApi(
          page, 'GET', `/api/v1/rag/collections/${col.id}/documents?status=failed`);
        const body = (await res.json()) as { documents?: Array<{ file_name: string }> };
        return (body.documents || []).some((d) => d.file_name === 'e2e-fail.txt');
      }, { timeout: 120_000, message: 'document never reached failed — the embed job did not fail' })
      .toBe(true);

    await page.goto(`${BASE}/dashboard/knowledge/${col.id}/documents`);
    const button = page.locator('[data-action="knowledge:retry-failed"]');
    await expect(button).toBeVisible();
    await expect(button).toContainText('Retry failed');

    const reprocess = page.waitForResponse(
      (r) => r.request().method() === 'POST'
        && r.url().endsWith(`/api/v1/rag/collections/${col.id}/reprocess-failed`));
    await button.click();
    const res = await reprocess;
    expect(res.status(), await res.text()).toBe(200);
    const body = (await res.json()) as { requeued?: number };
    expect(body.requeued ?? 0).toBeGreaterThanOrEqual(1);
    await expect(page.locator('#toast')).toContainText('re-queued');
  });
});

test.describe('Search — deep links (v1.2)', () => {
  test('result title links to the retained-copy deep link', async ({ page }) => {
    await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
    const seed = seedData();
    await page.goto(
      `${BASE}/dashboard/search?q=enclave+renewal+window&collection=${seed.collectionID}`);
    const row = page.locator('#searchResultRows .result').first();
    await expect(row).toBeVisible({ timeout: 30_000 });
    const link = row.locator('.result-title a');
    await expect(link).toBeVisible();
    const href = await link.getAttribute('href');
    // Uploads deep-link to the retained copy (optionally #page=N).
    expect(href).toMatch(/^\/api\/v1\/rag\/evidence\/[^/?#]+\/copy/);
    await expect(link).toHaveAttribute('target', '_blank');
    await expect(link).toHaveAttribute('rel', /noopener noreferrer/);
  });
});
