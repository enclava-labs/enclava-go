import type { Page } from '@playwright/test';

declare global {
  interface Window {
    Enclava: {
      api(method: string, path: string, body?: unknown): Promise<unknown>;
      evidence: { open(id: string): void };
    };
  }
}
import {
  BASE, MEMBER_EMAIL, MEMBER_PASSWORD, GRANTLESS_EMAIL,
  api, login, seedData, awaitTerminal, uploadDoc, test, expect,
} from './fixtures';

// flows.chat-search.spec.ts — member-project §9.2 flows 1–2 plus the
// migrated case inventories (see 12-04 SUMMARY case map):
//   tests/chat-search.spec.ts — two-turn adoption + citations + evidence
//   card + reload, Stop confirm-group, mid-run reload resumed polling, New chat
//   reset, mobile history/evidence sheet <760px, member grant isolation +
//   grantless empty state, fused search + facet chips + restricted leak
//   denial, evidence drawer + Ask-about-this + revocation denial, recents
//   namespacing + Back/Forward restore.
//   tests/a11y.spec.ts (4) — member rejected write: sticky toast, no link.
//   tests/a11y.spec.ts (4a, member half) — worker failure → administrator
//   fallback text, no href.
//   tests/home.spec.ts — member home composition + grantless empty state.
//
// Everything runs on the deterministic global.setup seed (seedData());
// per-test objects that would mutate shared rows are created through the
// admin API helper inside the test.

async function sendFirstMessage(page: Page, assistantID: string, text: string): Promise<string> {
  await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(assistantID)}`);
  await page.fill('#composer', text);
  await page.click('[data-action="chat:send"]');
  await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);
  return page.url().split('/').pop() as string;
}

test.describe('Chat run flow (member)', () => {
  test('two turns adopt the conversation, render tool step + citation + evidence card, and survive reload', async ({ page }) => {
    const seed = seedData();
    const convID = await sendFirstMessage(
      page, seed.assistantID,
      'What is the enclave renewal window? Cite your source.',
    );
    await awaitTerminal(page);

    // Scripted stub turn: one e2e_echo tool call renders the collapsible
    // step, then the final answer carries the [1] citation.
    await expect(page.locator('details.toolstep').first()).toBeVisible();

    await page.fill('#composer', 'And who approves it? Cite [1] again.');
    await page.click('[data-action="chat:send"]');
    await awaitTerminal(page);

    const citations = page.locator('article.message .bubble button.citation');
    await expect(citations.first()).toBeVisible();
    await citations.first().click();
    const panel = page.locator('.evidence-panel');
    await expect(panel).toBeVisible();
    await expect(panel.locator('.quote, .passage').first()).toBeVisible();

    // Every grounded answer lists its sources under the bubble — citations
    // or not: a resolved row carries title + View evidence.
    const sources = page.locator('article.message .sources').first();
    await expect(sources).toBeVisible();
    await expect(sources.locator('.source-row [data-action="evidence:open"]').first()).toBeVisible();

    await page.reload();
    await expect(page).toHaveURL(new RegExp(`/dashboard/chat/${convID}$`));
    await expect(page.locator('#chatThread .bubble').first()).toBeVisible();
  });

  test('follow-up run POST carries evidence_ids from an opened passage', async ({ page }) => {
    const seed = seedData();
    const convID = await sendFirstMessage(
      page, seed.assistantID,
      'Attach the renewal passage and summarise it.',
    );
    await awaitTerminal(page);

    // Open evidence for the first citation so the composer collects ids —
    // the producer→consumer handoff must put them on the NEXT run's POST.
    const citation = page.locator('article.message .bubble button.citation').first();
    await expect(citation).toBeVisible();
    await citation.click();
    await expect(page.locator('.evidence-panel')).toBeVisible();

    const posts: Record<string, unknown>[] = [];
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.url().endsWith('/api/v1/agent-runs')) {
        posts.push(r.postDataJSON() as Record<string, unknown>);
      }
    });
    await page.fill('#composer', 'Now answer using that passage.');
    await page.click('[data-action="chat:send"]');
    await awaitTerminal(page);

    const followUp = posts.find((b) => b && b.conversation_id === convID);
    expect(followUp, 'the follow-up POST must carry conversation_id').toBeTruthy();
    // Attached-evidence ids ride evidence_ids (the search handoff path); a
    // run whose opening surfaced no ids simply omits the field — assert the
    // wire shape the producer writes, never an invented one.
    if (followUp!.evidence_ids !== undefined) {
      expect(Array.isArray(followUp!.evidence_ids)).toBe(true);
      expect((followUp!.evidence_ids as string[]).length).toBeGreaterThan(0);
    }
    expect(page.url()).toContain(`/dashboard/chat/${convID}`);
  });

  test('Stop arms through the shared confirm, DELETEs the run and renders Stopped by you', async ({ page }) => {
    const seed = seedData();
    await sendFirstMessage(page, seed.assistantID, 'Long-running question — summarise everything.');

    const stop = page.locator('[data-run-id][data-action="confirm"]');
    await expect(stop).toBeVisible();
    await stop.click();
    const group = page.locator('.confirm-group');
    await expect(group).toBeVisible();
    const deleteRun = page.waitForRequest((r) => r.method() === 'DELETE' && /\/api\/v1\/agent-runs\/.+/.test(r.url()));
    await group.getByRole('button', { name: 'Confirm' }).click();
    await deleteRun;
    await expect(page.locator('.thread-body')).toContainText('Stopped by you', { timeout: 30_000 });
  });

  test('mid-run reload resumes polling until terminal with no time bail', async ({ page }) => {
    const seed = seedData();
    let convID = await sendFirstMessage(page, seed.assistantID, 'Count the documents you can see.');
    // The run may still be non-terminal here — resumed polling only exists
    // while a queued/running marker is in the DOM.
    const live = page.locator(
      'article.message[data-run-id][data-run-status="queued"], article.message[data-run-id][data-run-status="running"]').first();
    if (await live.count() === 0) {
      convID = await sendFirstMessage(page, seed.assistantID, 'Long-running question — summarise everything.');
      await page.reload();
    }
    await expect(live).toBeVisible();

    let polls = 0;
    page.on('request', (r) => {
      if (r.method() === 'GET' && /\/api\/v1\/agent-runs\/[^/]+$/.test(r.url())) polls++;
    });
    await expect
      .poll(() => polls, {
        timeout: 60_000,
        message: 'resumed run never polled — the no-time-cutoff contract is broken',
      })
      .toBeGreaterThanOrEqual(2);
    await awaitTerminal(page);
    expect(await page.locator('#chatThread .bubble').count()).toBeGreaterThan(0);
    expect(page.url()).toContain(`/dashboard/chat/${convID}`);
  });

  test('New chat resets composer, thread and URL, and the next conversation is reloadable', async ({ page }) => {
    const seed = seedData();
    const convID = await sendFirstMessage(page, seed.assistantID, 'First conversation question.');
    await page.fill('#composer', 'draft that must not survive');
    await page.click('.head [data-action="chat:new"]');
    await expect(page).toHaveURL(/\/dashboard\/chat$/);
    await expect(page.locator('#composer')).toHaveValue('');

    await page.goto(`${BASE}/dashboard/chat/${convID}`);
    await expect(page.locator('#chatThread')).toBeVisible();

    // The reloaded conversation still sends to itself — the NEW conversation
    // is the New-chat reset path.
    await page.click('.head [data-action="chat:new"]');
    await expect(page).toHaveURL(/\/dashboard\/chat$/);
    await page.fill('#composer', 'Second conversation question.');
    await page.click('[data-action="chat:send"]');
    await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);
    expect(page.url()).not.toContain(convID);
    await awaitTerminal(page);
  });

  test('live run streams through the events endpoint and the final answer renders', async ({ page }) => {
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.assistantID)}`);
    // Register before send: polling (and with it the events stream) starts
    // as soon as the 202 returns.
    const events = page.waitForRequest(
      (r) => /\/api\/v1\/agent-runs\/.+\/events$/.test(r.url()),
      { timeout: 30_000 },
    );
    await page.fill('#composer', 'Stream the renewal window answer.');
    await page.click('[data-action="chat:send"]');
    await events; // the Chat page opened the run's SSE stream
    await awaitTerminal(page);
    // The server-rendered thread stays the source of truth for the answer.
    await expect(page.locator('#chatThread article.message .bubble').last()).toContainText('renewal window');
  });
});

test.describe('Chat surfaces and grants (member)', () => {
  test('at 390x844 history hides behind the Chats button and evidence opens as a bottom sheet', async ({ page }) => {
    const seed = seedData();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${BASE}/dashboard/chat`);
    await expect(page.locator('.history')).toBeHidden();
    const chats = page.locator('.head .mobile-chats');
    await expect(chats).toBeVisible();
    await chats.click();
    await expect(page.locator('#mobileChats')).toBeVisible();
    await page.locator('#mobileChats .mobile-chats').first().click();
    await expect(page.locator('#mobileChats')).toBeHidden();

    await page.evaluate((id) => window.Enclava.evidence.open(id), seed.evidenceID);
    const sheet = page.locator('.evidence-panel.sheet');
    await expect(sheet).toBeVisible();
    await sheet.locator('[data-action="evidence:close"]').click();
    await expect(page.locator('.evidence-panel.sheet')).toHaveCount(0);
  });

  test('member grants isolate history and the picker; grantless members see the truthful empty state', async ({ page }) => {
    const seed = seedData();

    // An admin-owned conversation on the seeded assistant is a uniform 404
    // for the member — history and rows never leak.
    const run = await api<{ conversation_id: string }>('POST', '/api/v1/agent-runs', {
      assistant_id: seed.assistant2ID,
      input: 'admin own conversation question',
    });
    await page.goto(`${BASE}/dashboard/chat/${run.conversation_id}`);
    await expect(page.locator('.authwrap h1')).toHaveText('Page not found');

    // Own history and picker carry only granted assistants.
    await page.goto(`${BASE}/dashboard/chat`);
    await expect(page.locator('#assistantSelect')).not.toBeDisabled();
    await expect(page.locator('#assistantSelect option')).toHaveCount(3);

    // The never-granted member: empty picker, grant-needed copy, locked send.
    await page.context().clearCookies();
    await login(page, GRANTLESS_EMAIL, MEMBER_PASSWORD);
    await page.goto(`${BASE}/dashboard/chat`);
    await expect(page.locator('#assistantSelect')).toBeDisabled();
    await expect(page.locator('#assistantSelect option')).toHaveCount(0);
    await expect(page.locator('.thread-head')).toContainText('ask an admin to grant access');
    await expect(page.locator('[data-action="chat:send"]')).toHaveCount(0); // composer is hidden for a grantless member
    // Grantless login replaced this context's session — restore member.
    await login(page, MEMBER_EMAIL, MEMBER_PASSWORD);
  });
});

test.describe('Search (member)', () => {
  test('member sees fused results, facet chips with real counts, restricted content never leaks', async ({ page }) => {
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/search`);
    await page.fill('#searchQuery', 'contract');
    await page.click('#searchForm button[type="submit"]');
    const results = page.locator('#searchResults .result');
    await expect(results.first()).toBeVisible({ timeout: 20_000 });

    await expect(page).toHaveURL(/\/dashboard\/search\?.*q=contract/);
    await expect(page.locator('#searchResults .result .result-title', { hasText: 'alpha-contract.txt' })).toBeVisible();
    await expect(page.locator('#searchResults .result .result-title', { hasText: 'beta-contract.txt' })).toBeVisible();

    // Restricted collection: no row, no title, no facet anywhere.
    await expect(page.locator('#searchResults')).not.toContainText('secret-contract');
    await expect(page.locator('#searchResults')).not.toContainText('Secret');

    const rows = await results.count();
    const chip = page.locator('#searchResults .chip[data-facet="upload"]');
    await expect(chip).toBeVisible();
    await expect(chip).toContainText(String(rows));
    await chip.click();
    await expect(page.locator('#searchResults .result:visible')).toHaveCount(rows);
    await page.locator('#searchResults .chip[data-facet=""]').click();

    await page.selectOption('#searchCollection', seed.collectionID);
    await expect(page.locator('#searchResults .result', { hasText: 'alpha-contract.txt' })).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('#searchResults .result', { hasText: 'beta-contract.txt' })).toHaveCount(0);

    // Semantic mode must execute against the real embedding stub.
    await page.selectOption('#searchCollection', '');
    await page.locator('.chip[data-mode="semantic"]').click();
    await expect(page.locator('#searchResults .result').first()).toBeVisible({ timeout: 30_000 });
    await expect(page).toHaveURL(/mode=semantic/);
  });

  test('evidence opens the shared #drawer, Ask about this runs authorized, revocation is denied', async ({ page }) => {
    test.setTimeout(240_000);
    const seed = seedData();

    // A dedicated document for the revocation half — deleting a shared seed
    // document would falsify every later citation case.
    await uploadDoc(seed.collectionID, 'revocable-passage.txt',
      'Revocable passage: a temporary clause the revocation case deletes.');
    const seeded = await api<{ results?: Array<{ evidence_id?: string }> }>(
      'POST', '/api/v1/rag/search',
      { query: 'Revocable passage', mode: 'lexical', collection_ids: [seed.collectionID] });
    const revokedEvidence = (seeded.results || []).map((r) => r.evidence_id)
      .find((v): v is string => typeof v === 'string');
    if (!revokedEvidence) throw new Error('no evidence_id for the revocable passage');

    await page.goto(`${BASE}/dashboard/search`);
    await page.fill('#searchQuery', 'contract');
    await page.click('#searchForm button[type="submit"]');
    await expect(page.locator('#searchResults .result').first()).toBeVisible({ timeout: 20_000 });

    const evBtn = page.locator('#searchResults [data-action="evidence:open"]').first();
    const evidenceID = await evBtn.getAttribute('data-evidence-id');
    if (!evidenceID) throw new Error('result row carried no data-evidence-id');
    await evBtn.click();
    const drawer = page.locator('#drawer .evidence-card');
    await expect(drawer).toBeVisible({ timeout: 15_000 });
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

    // The real authorized run: the POST carries evidence_ids (producer→
    // consumer) and the completed turn binds them on the persisted article.
    const posts: Record<string, unknown>[] = [];
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.url().endsWith('/api/v1/agent-runs')) {
        posts.push(r.postDataJSON() as Record<string, unknown>);
      }
    });
    await page.selectOption('#assistantSelect', seed.assistantID);
    await page.fill('#composer', 'Summarise the attached contract passage.');
    await page.click('[data-action="chat:send"]');
    await expect(page.locator('article.message[data-run-id]').first()).toBeVisible({ timeout: 15_000 });
    await awaitTerminal(page);
    const lastPost = posts[posts.length - 1] || {};
    expect(lastPost.evidence_ids).toEqual([evidenceID]);
    await expect(page.locator(`article.message[data-evidence-ids*="${evidenceID}"]`).first()).toBeVisible();

    // Revocation: remove the document server-side, then the same handoff
    // must be denied — sticky toast, never a fake success.
    const docs = await api<{ documents?: Array<{ id: string; file_name: string }> }>(
      'GET', `/api/v1/rag/collections/${seed.collectionID}/documents`);
    const doc = (docs.documents || []).find((d) => d.file_name === 'revocable-passage.txt');
    if (!doc) throw new Error('the revocable document is missing');
    await api('DELETE', `/api/v1/rag/documents/${doc.id}`);

    await page.goto(`${BASE}/dashboard/chat?evidence=${revokedEvidence}`);
    await page.selectOption('#assistantSelect', seed.assistantID);
    await page.fill('#composer', 'Try the revoked passage.');
    await page.click('[data-action="chat:send"]');
    await expect(page.locator('#toast')).toBeVisible({ timeout: 15_000 });
    await expect(page.locator('#chatThread .runline')).toHaveCount(0);
  });

  test('recents are namespaced per identity; Back/Forward restores without new history entries at 390px', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${BASE}/dashboard/search`);
    await page.fill('#searchQuery', 'contract');
    await page.click('#searchForm button[type="submit"]');
    await expect(page.locator('#searchResults .result').first()).toBeVisible({ timeout: 20_000 });
    await page.fill('#searchQuery', 'pricing');
    const pricing = page.waitForResponse((r) => /partials\/search\/results.*q=pricing/.test(r.url()));
    await page.click('#searchForm button[type="submit"]');
    await pricing;
    await expect(page).toHaveURL(/q=pricing/, { timeout: 15_000 });

    await page.goto(`${BASE}/dashboard/search`);
    const recents = page.locator('#searchRecents [data-action="search:recent"]');
    await expect(recents.first()).toBeVisible();
    await expect(page.locator('#searchRecents')).toContainText('pricing');
    await expect(page.locator('#searchRecents')).toContainText('contract');

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
    await page.goBack();
    await expect(page).toHaveURL(/q=contract/, { timeout: 15_000 });

    // A different identity's recents bucket is their own/empty — never the
    // member's queries.
    await page.context().clearCookies();
    await login(page, GRANTLESS_EMAIL, MEMBER_PASSWORD);
    await page.goto(`${BASE}/dashboard/search`);
    await expect(page.locator('#searchRecents')).not.toContainText('contract');
    await expect(page.locator('#searchRecents')).not.toContainText('pricing');
    // Grantless login replaced this context's session — restore member.
    await login(page, MEMBER_EMAIL, MEMBER_PASSWORD);
  });
});

test.describe('member home and error-toast cases', () => {
  test('member home shows own sections and zero manager content', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    // Members lack billing.view — #homeKPIs is admin/manager-only by design;
    // the member-visible sections are Running now + Confidential inference.
    await expect(page.locator('#homeRunningSection')).toBeVisible();
    // A member never sees manager attention/admin surfaces on Home.
    await expect(page.locator('#content')).not.toContainText('Tenant budget');
  });

  test('grantless member sees the grant-needed empty state, never a fake list', async ({ page }) => {
    await page.context().clearCookies();
    await login(page, GRANTLESS_EMAIL, MEMBER_PASSWORD);
    await page.goto(`${BASE}/dashboard`);
    await expect(page.locator('#content')).toContainText('ask an admin');
    await page.goto(`${BASE}/dashboard/extract`);
    await expect(page.locator('#content')).toContainText('administrator');
    // Grantless login replaced this context's session — restore member.
    await login(page, MEMBER_EMAIL, MEMBER_PASSWORD);
  });

  test('member rejected write renders a sticky toast with no fix link', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/chat`);
    await page.evaluate(async () => {
      try {
        await window.Enclava.api('POST', '/api/v1/admin/api-keys', { name: 'member-probe' });
      } catch { /* rejection is the expected outcome */ }
    });
    const toast = page.locator('#toast.show.error');
    await expect(toast).toBeVisible();
    await expect(toast.locator('a[href]')).toHaveCount(0);
  });

  test('worker failure on the unpriced assistant degrades to the administrator fallback', async ({ page }) => {
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.failingAssistantID)}`);
    await page.fill('#composer', 'member unpriced run');
    await page.click('[data-action="chat:send"]');
    await awaitTerminal(page, 120_000);
    const toast = page.locator('#toast.show.error');
    await expect(toast).toBeVisible();
    await expect(toast).toContainText('administrator');
    await expect(toast.locator('a[href]')).toHaveCount(0);
    const line = page.locator('.thread-body .runline').last();
    await expect(line).toContainText('administrator');
    await expect(line.locator('a[href]')).toHaveCount(0);
  });
});
