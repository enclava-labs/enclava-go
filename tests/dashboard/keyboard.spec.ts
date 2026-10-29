import { test, expect } from '@playwright/test';
import {
  BASE, ADMIN_EMAIL, ADMIN_PASSWORD,
  seedData, awaitTerminal,
} from './fixtures';

declare global {
  interface Window {
    Enclava: {
      api(method: string, path: string, body?: unknown): Promise<unknown>;
    };
  }
}

// keyboard.spec.ts — the migrated overlay/focus/keyboard inventories
// (see 12-04 SUMMARY case map):
//   tests/a11y.spec.ts — (1) drawer Tab-trap + wrap, (2) Esc/backdrop
//   close + focus return, (3) popover replaces drawer, (5) skip link
//   first tab stop, login keyboard submit.
//   tests/a11y-pages.spec.ts — citation Enter→focus-return, composer
//   Enter/Shift+Enter, wizard-step focus discipline, keyboard-armed
//   data-confirm, reduced-motion emulation (computed-style half lives in
//   screenshots.spec.ts).
//   tests/chat-search.spec.ts — mobile-chats sheet focus.
//
// Owner projects: member for the member-page cases, manager for the
// Sources wizard focus case, admin for the login + admin-surface cases.

test.describe('overlays — drawer focus discipline (admin)', () => {
  test.skip(({ storageState }) => !String(storageState).includes('admin.json'));

  test('drawer traps Tab, cycles last→first, never releases focus to the page', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/sources?new=1`);
    const drawer = page.locator('#drawer');
    await expect(drawer).toHaveClass(/open/, { timeout: 15_000 });
    await expect.poll(() =>
      page.evaluate(() => document.getElementById('drawer')!.contains(document.activeElement)),
    ).toBe(true);

    // The trap cycles VISIBLE controls (app.js visibleFocusables): a wizard
    // drawer keeps later steps and the disabled Back/Continue out of the
    // cycle, so the selector is filtered to visible controls the same way.
    const vis = page.locator(
      '#drawer a[href], #drawer button:not([disabled]), #drawer input:not([disabled]):not([type="hidden"]), #drawer select:not([disabled]), #drawer textarea:not([disabled]), #drawer [tabindex]:not([tabindex="-1"])',
    ).filter({ visible: true });
    const n = await vis.count();
    if (n < 2) {
      throw new Error(`drawer renders ${n} focusable controls — the wizard needs at least 2 for the wrap proof`);
    }
    await vis.last().focus();
    await page.keyboard.press('Tab');
    await expect(vis.first()).toBeFocused();
    await page.keyboard.down('Shift');
    await page.keyboard.press('Tab');
    await page.keyboard.up('Shift');
    await expect(vis.last()).toBeFocused();
    for (let i = 0; i < Math.max(12, n); i++) {
      await page.keyboard.press('Tab');
      const inside = await page.evaluate(() =>
        document.getElementById('drawer')!.contains(document.activeElement));
      if (!inside) {
        throw new Error(`Tab escaped the open drawer on press ${i + 1}`);
      }
    }
  });

  test('Esc and backdrop close the drawer and return focus to the opener', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/sources`);
    const addSource = page.locator('[data-action="open-drawer"]').first();
    if ((await addSource.count()) === 0) {
      throw new Error('no data-action="open-drawer" control on /dashboard/sources');
    }
    await addSource.focus();
    await addSource.click();
    const drawer = page.locator('#drawer');
    await expect(drawer).toHaveClass(/open/, { timeout: 15_000 });
    await page.keyboard.press('Escape');
    await expect(drawer).not.toHaveClass(/open/);
    await expect.poll(() =>
      page.evaluate(() =>
        document.activeElement instanceof HTMLElement && document.activeElement.dataset.action === 'open-drawer'),
    ).toBe(true);

    await addSource.focus();
    await addSource.click();
    await expect(drawer).toHaveClass(/open/);
    await page.locator('#backdrop').click({ position: { x: 8, y: 8 } });
    await expect(drawer).not.toHaveClass(/open/);
    await expect.poll(() =>
      page.evaluate(() =>
        document.activeElement instanceof HTMLElement && document.activeElement.dataset.action === 'open-drawer'),
    ).toBe(true);
  });

  test('opening a popover replaces an open drawer and focus lands inside it', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/sources?new=1`);
    const drawer = page.locator('#drawer');
    await expect(drawer).toHaveClass(/open/, { timeout: 15_000 });
    const userMenu = page.locator('.user-menu');
    // The drawer backdrop covers the sidebar menu button (modal overlay) —
    // a DOM click exercises the same data-action handler a pointer would hit
    // once the popover supersedes the drawer.
    await userMenu.evaluate((el) => el.click());
    const pop = page.locator('#pop');
    await expect(pop).toBeVisible();
    await expect(drawer).not.toHaveClass(/open/);
    await expect.poll(() =>
      page.evaluate(() => document.getElementById('pop')!.contains(document.activeElement)),
    ).toBe(true);
    await page.keyboard.press('Escape');
    await expect(pop).toBeHidden();
    await expect.poll(() =>
      page.evaluate(() => document.activeElement === document.querySelector('.user-menu')),
    ).toBe(true);
  });

  test('skip link is the first Tab stop and activates focus into #content', async ({ page }) => {
    await page.goto(`${BASE}/dashboard`);
    await page.keyboard.press('Tab');
    await expect.poll(() =>
      page.evaluate(() => document.activeElement === document.querySelector('a.skip')),
    ).toBe(true);
    await page.keyboard.press('Enter');
    await expect.poll(() =>
      page.evaluate(() => {
        const c = document.getElementById('content');
        return c === document.activeElement || c!.contains(document.activeElement);
      }),
    ).toBe(true);
  });

  test('the login form submits with the keyboard only', async ({ page }) => {
    await page.context().clearCookies();
    await page.goto(`${BASE}/dashboard/login`);
    // Tab past the skip link to the email control before typing.
    for (let i = 0; i < 5 && await page.evaluate(() => (document.activeElement as HTMLElement | null)?.getAttribute('name') !== 'email'); i++) {
      await page.keyboard.press('Tab');
    }
    await page.keyboard.type(ADMIN_EMAIL);
    await page.keyboard.press('Tab');
    await page.keyboard.type(ADMIN_PASSWORD);
    await page.keyboard.press('Enter');
    await page.waitForURL(/\/dashboard(?!\/login)/, { timeout: 15_000 });
    await expect(page.locator('#nav')).toBeVisible();
  });
});

test.describe('page-surface keyboard flows (member)', () => {
  test.skip(({ storageState }) => !String(storageState).includes('member.json'));

  test('citation opens evidence by keyboard and close returns focus to it', async ({ page }) => {
    test.setTimeout(180_000);
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.assistantID)}`);
    await page.fill('#composer', 'What is the enclave renewal window? Cite your source as [1].');
    await page.click('[data-action="chat:send"]');
    await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);
    await awaitTerminal(page);

    const citations = page.locator('article.message .bubble button.citation');
    if ((await citations.count()) === 0) {
      throw new Error('the assistant answer carried no [n] citation markers');
    }
    const citation = citations.first();
    await citation.focus();
    await expect(citation).toBeFocused();
    await citation.press('Enter');
    await expect(page.locator('.evidence-panel')).toBeVisible({ timeout: 15_000 });

    const close = page.locator('.evidence-panel [data-action="evidence:close"]');
    await expect(close).toBeVisible();
    await close.focus();
    await close.press('Enter');
    await expect(citation).toBeFocused({ timeout: 5_000 });
  });

  test('composer Enter sends, Shift+Enter inserts a newline without moving focus', async ({ page }) => {
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.assistantID)}`);
    const composer = page.locator('#composer');
    await composer.focus();
    await composer.pressSequentially('line one');
    await composer.press('Shift+Enter');
    await composer.pressSequentially('line two');
    await expect(composer).toHaveValue(/line one\nline two/);
    await expect(composer).toBeFocused();

    const runPost = page.waitForRequest(
      (r) => r.method() === 'POST' && r.url().endsWith('/api/v1/agent-runs'));
    await composer.press('Enter');
    await runPost;
    await expect(composer).toHaveValue('');
    await awaitTerminal(page);
  });

  test('data-confirm destructives arm and confirm entirely by keyboard', async ({ page }) => {
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.assistantID)}`);
    await page.fill('#composer', 'Long-running question — summarise everything in the knowledge base.');
    await page.click('[data-action="chat:send"]');
    await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);

    const stop = page.locator('[data-run-id][data-action="confirm"]');
    await expect(stop).toBeVisible();
    await stop.focus();
    await stop.press('Enter');
    await expect(stop).toHaveText(/Stop run\?/);
    await expect(stop).toBeFocused();
    const deleteRun = page.waitForRequest(
      (r) => r.method() === 'DELETE' && /\/api\/v1\/agent-runs\/.+/.test(r.url()));
    await stop.press('Enter');
    await deleteRun;
    await expect(page.locator('.thread-body')).toContainText('Stopped by you', { timeout: 30_000 });
  });

  test('member session writes to admin surfaces are denied through api()', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/chat`);
    await page.evaluate(async () => {
      try {
        // An admin-surface write: a real 403 (an unknown assistant is the
        // uniform 404, which api() deliberately does not toast).
        await window.Enclava.api('POST', '/api/v1/admin/budgets', {
          name: 'denied', subject_type: 'tenant', kind: 'amount',
          limit_value: 1, currency: 'USD', period: 'monthly',
          starts_at: new Date().toISOString(),
        });
      } catch { /* rejection is the expected outcome */ }
    });
    const toast = page.locator('#toast.show.error');
    await expect(toast).toBeVisible();
    await expect(toast.locator('a[href]')).toHaveCount(0);
  });
});

test.describe('wizard focus discipline (manager)', () => {
  test.skip(({ storageState }) => !String(storageState).includes('manager.json'));

  test('Sources wizard Continue lands focus on the next step’s first control', async ({ page }) => {
    test.setTimeout(90_000);
    const seed = seedData();
    await page.goto(`${BASE}/dashboard/sources?new=1`);
    const drawer = page.locator('#drawer');
    await expect(drawer).toHaveClass(/open/);

    const tile = drawer.locator('.tile[data-provider="github"]');
    await tile.focus();
    await tile.press('Enter');
    await expect(tile).toHaveAttribute('aria-pressed', 'true');

    const step2First = drawer.locator('[data-provider-fields="github"]:not([hidden]) input').first();
    await expect(step2First).toBeFocused({ timeout: 5_000 });

    const cont = drawer.getByRole('button', { name: 'Continue' });
    await drawer.locator('#github-owner').fill('acme');
    await drawer.locator('#github-repo').fill('repo');
    await expect(cont).toBeEnabled();
    await cont.focus();
    await cont.press('Enter');
    await expect(drawer.locator('[data-step-pane="3"]')).toBeVisible();

    await cont.focus();
    await cont.press('Enter');
    await expect(drawer.locator('#source-name')).toBeFocused({ timeout: 5_000 });

    await drawer.locator('#source-name').fill(`a11y src ${Date.now()}`);
    await drawer.locator('#source-collection').selectOption(seed.collectionID);
    await cont.focus();
    await cont.press('Enter');
    const submit = drawer.getByRole('button', { name: 'Create source and sync' });
    await expect(submit).toBeVisible();
    await expect(submit).toBeFocused({ timeout: 5_000 });

    const back = drawer.getByRole('button', { name: 'Back' });
    await back.focus();
    await back.press('Enter');
    await expect(drawer.locator('#source-name')).toBeFocused({ timeout: 5_000 });
  });
});
