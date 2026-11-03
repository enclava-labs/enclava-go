import { Page } from '@playwright/test';
import * as path from 'node:path';
import { test, expect, BASE, sessionApi, seedData, awaitTerminal } from './fixtures';

// chat-continuity.spec.ts — the unit-2A Chat regressions:
//   · AST-I02: a reopened conversation posts its OWN assistant on follow-ups
//   · AT-VIS-I01: assistant bubbles render a safe Markdown subset + evidence
//     citations, with no innerHTML path for model text
//   · AST-I03: history rows rename inline and delete through the armed
//     data-confirm — owner-scoped PATCH/DELETE on /api/v1/conversations/{id}
//
// Renderer cases drive the REAL pages/chat.js headlessly (setContent +
// addScriptTag, same contract as legacy.usage-dst.spec.ts); the page flows
// use the admin project's stored session.

const CHAT_JS = path.resolve(__dirname, '../../internal/httpx/dashboard/static/pages/chat.js');

interface Run { id: string; conversation_id: string; status: string }

// The page module exposes a test seam on window.__enclavaChat.
declare global {
  interface Window { __enclavaChat?: { render: (b: HTMLElement, t: string, i: string[], k: boolean) => void } }
}

// startRun posts a real agent run through the browser session and waits for
// the worker to settle — a follow-up in the same conversation must never
// race an in-flight turn.
async function startRun(page: Page, assistantID: string, input: string, conversationID?: string): Promise<Run> {
  const res = await sessionApi(page, 'POST', '/api/v1/agent-runs', {
    assistant_id: assistantID,
    input,
    ...(conversationID ? { conversation_id: conversationID } : {}),
  });
  expect(res.status(), `agent-run start: ${await res.text()}`).toBe(202);
  const run = (await res.json()) as Run;
  await expect
    .poll(async () => {
      const check = await sessionApi(page, 'GET', `/api/v1/agent-runs/${run.id}`);
      if (!check.ok()) return 'pending';
      return ((await check.json()) as Run).status;
    }, { timeout: 90_000, intervals: [800] })
    .not.toMatch(/^(queued|running)$/);
  return run;
}

// mountChat loads the real page module against DOM markup — the DOMContentLoaded
// listener already ran under setContent, so rendering goes through the exposed
// __enclavaChat.render seam.
async function mountChat(page: Page) {
  await page.setContent('<div class="thread-body"></div>');
  await page.addScriptTag({ content: 'window.Enclava = { api: () => Promise.resolve({}), toast: () => {}, refresh: () => {}, errorAction: () => ({}) };' });
  await page.addScriptTag({ path: CHAT_JS });
}

async function renderBubble(page: Page, opts: { text: string; ids?: string[]; knowledge?: boolean; user?: boolean }): Promise<void> {
  await page.evaluate(({ text, ids, knowledge, user }) => {
    const thread = document.querySelector('.thread-body')!;
    if (knowledge) (thread as HTMLElement).dataset.hasKnowledge = '1';
    const article = document.createElement('article');
    article.className = 'message' + (user ? ' user' : '');
    if (ids && ids.length) article.dataset.evidenceIds = ids.join(',');
    const bubble = document.createElement('div');
    bubble.className = 'bubble';
    article.appendChild(bubble);
    thread.appendChild(article);
    window.__enclavaChat!.render(bubble, text, ids || [], knowledge);
  }, opts);
}

test.describe('Chat continuity (AST-I02)', () => {
  test('a reopened conversation posts its own assistant on the follow-up', async ({ page }) => {
    const seed = seedData();
    const run = await startRun(page, seed.assistantID, 'Continuity first turn');
    await page.goto(`${BASE}/dashboard/chat/${run.conversation_id}`);

    // The picker renders the conversation's assistant selected; switch it to
    // prove the thread still posts its own (the switch starts a new chat —
    // navigate back to reopen the thread).
    await expect(page.locator('#assistantSelect')).toHaveValue(seed.assistantID);
    await page.locator('#assistantSelect').selectOption(seed.assistant2ID);
    await page.goto(`${BASE}/dashboard/chat/${run.conversation_id}`);
    await expect(page.locator('#assistantSelect')).toHaveValue(seed.assistantID);

    const sent = page.waitForRequest((req) =>
      req.url().includes('/api/v1/agent-runs') && req.method() === 'POST');
    await page.locator('#composer').fill('Continuity follow-up');
    await page.locator('[data-action="chat:send"]').click();
    const postData = (await sent).postDataJSON() as { assistant_id: string; conversation_id: string };
    expect(postData.assistant_id).toBe(seed.assistantID);
    expect(postData.conversation_id).toBe(run.conversation_id);
    await awaitTerminal(page);
  });
});

test.describe('Assistant bubble rendering (AT-VIS-I01)', () => {
  test('markdown subset renders real elements', async ({ page }) => {
    await mountChat(page);
    await renderBubble(page, {
      text: '## Summary\n\n- **bold** item\n- `code` item\n\n1. ordered\n\n[docs](https://example.com/x) and *italics*',
    });
    const bubble = page.locator('.bubble');
    await expect(bubble.locator('h4')).toHaveText('Summary');
    await expect(bubble.locator('ul li')).toHaveCount(2);
    await expect(bubble.locator('ol li')).toHaveCount(1);
    await expect(bubble.locator('strong')).toHaveText('bold');
    await expect(bubble.locator('code').first()).toHaveText('code');
    const link = bubble.locator('a');
    await expect(link).toHaveAttribute('href', 'https://example.com/x');
    await expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    await expect(link).toHaveAttribute('target', '_blank');
    await expect(bubble.locator('em')).toHaveText('italics');
  });

  test('model text never becomes markup: injection stays literal text', async ({ page }) => {
    await mountChat(page);
    await renderBubble(page, {
      text: 'Look <img src=x onerror="window.__pwned=1"> here\n\n```\n<b>not bold</b>\n```',
    });
    expect(await page.evaluate(() => { const w = window as unknown as Record<string, unknown>; return w.__pwned ?? null; })).toBeNull();
    expect(await page.locator('.bubble img').count()).toBe(0);
    expect(await page.locator('.bubble b').count()).toBe(0);
    expect(await page.locator('.bubble').innerText()).toContain('<img src=x onerror="window.__pwned=1">');
    await expect(page.locator('.bubble pre code')).toHaveText('<b>not bold</b>');
  });

  test('citations map [n] and evidence_<id>; unknown ids render the unavailable chip', async ({ page }) => {
    await mountChat(page);
    await renderBubble(page, {
      ids: ['evidence_ev_alpha', 'evidence_ev_beta'],
      text: 'Alpha [1] and beta evidence_ev_beta. Missing [3] and evidence_unknown_x.',
    });
    const buttons = page.locator('.bubble button.citation');
    await expect(buttons).toHaveCount(2);
    await expect(buttons.nth(0)).toHaveText('[1]');
    expect(await buttons.nth(0).getAttribute('data-evidence-id')).toBe('evidence_ev_alpha');
    await expect(buttons.nth(1)).toHaveText('[2]');
    expect(await buttons.nth(1).getAttribute('data-evidence-id')).toBe('evidence_ev_beta');
    const chips = page.locator('.bubble span.citation.muted');
    await expect(chips).toHaveCount(2);
    await expect(chips.first()).toHaveText('source unavailable');
  });

  test('a knowledge-backed answer with no evidence shows the muted no-sources line', async ({ page }) => {
    await mountChat(page);
    await renderBubble(page, { text: 'Nothing found.', knowledge: true });
    await expect(page.locator('.bubble .no-sources')).toHaveText('No sources were returned for this answer.');
    // And no line when the answer did cite (or the user wrote the text).
    await renderBubble(page, { ids: ['evidence_a'], text: 'Cited [1].', knowledge: true });
    await renderBubble(page, { text: 'member question', user: true, knowledge: true });
    await expect(page.locator('.no-sources')).toHaveCount(1);
  });
});

test.describe('History rename and delete (AST-I03)', () => {
  test('rename persists through PATCH and delete removes the conversation', async ({ page }) => {
    const seed = seedData();
    const run = await startRun(page, seed.assistantID, 'History row title');
    const convID = run.conversation_id;
    await page.goto(`${BASE}/dashboard/chat/${convID}`);

    // Inline rename from the history row.
    const row = page.locator(`.h-item[data-conv-id="${convID}"]`).first();
    await row.getByRole('button', { name: 'Rename' }).click();
    const input = row.locator('.h-rename');
    await input.fill('Renamed thread');
    await input.press('Enter');
    await expect(page.locator(`.h-item[data-conv-id="${convID}"] .h-title`).first()).toHaveText('Renamed thread');
    const got = await sessionApi(page, 'GET', `/api/v1/conversations/${convID}`);
    const view = (await got.json()) as { conversation: { title: string } };
    expect(view.conversation.title).toBe('Renamed thread');

    // Delete through the shared untimed confirm group: first click arms the
    // consequence group, its Confirm fires the DELETE.
    const del = page.locator(`.h-item[data-conv-id="${convID}"]`).first().getByRole('button', { name: 'Delete' });
    await del.click();
    await expect(page.locator('.confirm-group .confirm-text')).toContainText('Its messages are removed; usage records stay.');
    await page.locator('.confirm-group [data-action="confirm-commit"]').click();
    await expect(page.locator(`.h-item[data-conv-id="${convID}"]`)).toHaveCount(0);
    await page.waitForURL(`${BASE}/dashboard/chat`);
    const gone = await sessionApi(page, 'GET', `/api/v1/conversations/${convID}`);
    expect(gone.status()).toBe(404);
  });
});
