import { test, expect, Page, Locator, APIRequestContext } from '@playwright/test';

declare global {
  interface Window {
    Enclava: { api(method: string, path: string, body?: unknown): Promise<unknown> };
  }
}

// tests/a11y.spec.ts — shared-chrome accessibility consumer proof (OA-38,
// plan 11-04). INTERIM by contract: this case inventory migrates into
// phase-12's tests/dashboard/ suite (role projects + central fixtures.ts
// storageStates) before playwright.config repoints testDir — a case that
// never lands is lost coverage, not cleanup. One writer per spec file:
// page-flow keyboard cases live in 11-05's tests/a11y-pages.spec.ts.
//
// Every assertion is BEHAVIORAL — real focus, computed styles, real /api/v1
// seeding — never source-text presence and never a direct
// Enclava.errorAction invocation. House rule: a missing prerequisite
// FAILS the case naming what is absent — beforeAll throws, never test.skip.
//
// Required env: BASE_URL, ADMIN_EMAIL, ADMIN_PASSWORD (an administrator whose
// session carries the seeded full-scope admin role incl. pricing.manage —
// the models.manage cap that renders the Set-price action link).
// The worker failure flows additionally require the deployment's job runner
// to execute queued agent-runs/extraction jobs.

const BASE = process.env.BASE_URL || '';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';
const MEMBER_PASSWORD = 'a11y-member-pass-1';

const UNPRICED_MODEL = 'a11y-unpriced-echo';

interface Seed {
  sourceID: string;
  botID: string;
  templateID: string;
  endpointID: string;
  deniedUser: { email: string; roleID: string; userID: string };
  runnerUser: { email: string; roleID: string; userID: string };
}

let seed: Seed;

function requirePrereqs() {
  const missing: string[] = [];
  if (!BASE) missing.push('BASE_URL');
  if (!ADMIN_EMAIL) missing.push('ADMIN_EMAIL');
  if (!ADMIN_PASSWORD) missing.push('ADMIN_PASSWORD');
  if (missing.length) {
    throw new Error(
      `tests/a11y.spec.ts requires ${missing.join(', ')} — a real logged-in admin ` +
      'session is the seeding prerequisite; missing env fails, never skips',
    );
  }
}

async function login(page: Page, email = ADMIN_EMAIL, password = ADMIN_PASSWORD) {
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

async function api(request: APIRequestContext, page: Page, method: string, path: string, body?: unknown) {
  const csrf = (await page.context().cookies()).find((c) => c.name === 'enclava_csrf');
  const sess = (await page.context().cookies()).find((c) => c.name === 'enclava_session');
  return request.fetch(`${BASE}${path}`, {
    method,
    headers: {
      Cookie: sess ? `enclava_session=${sess.value}` : '',
      'X-CSRF-Token': csrf ? csrf.value : '',
      'Content-Type': 'application/json',
    },
    data: body === undefined ? undefined : JSON.stringify(body),
  });
}

async function jsonField(res: { ok(): boolean; status(): number; json(): Promise<unknown>; text(): Promise<string> }, what: string): Promise<string> {
  if (!res.ok()) {
    throw new Error(`${what} failed (${res.status()}): ${await res.text()} — seed prerequisite unmet`);
  }
  const payload: unknown = await res.json();
  if (typeof payload === 'object' && payload !== null) {
    const id = (payload as Record<string, unknown>).id ?? (payload as Record<string, unknown>).ID;
    if (typeof id === 'string') return id;
  }
  throw new Error(`${what} carried no id — contract drift: ${JSON.stringify(payload).slice(0, 200)}`);
}

// beforeAll provisions the whole fixture through the real /api/v1 surface on
// a one-off admin context: an unpriced endpoint (dispatch fails the price
// reserve before ever calling upstream → deterministic `required price
// missing: {unit}` worker failures), an assistant + extraction template
// bound to it, a drawerable source, and the two member sessions the
// capability-off cases need (denied writer and granted runner).
test.beforeAll(async ({ browser }) => {
  requirePrereqs();
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  const request = context.request;
  await login(page);

  const endpointID = await jsonField(await api(request, page, 'POST', '/api/v1/providers', {
    protocol: 'openai_compatible',
    base_origin: 'https://unreachable.a11y-spec.invalid',
    // chat/completions must be advertised: submission-time routing rejects a
    // model whose endpoint lacks the operation capability — the worker-side
    // price-reserve failure the cases below assert never gets a chance.
    capabilities: ['chat/completions', `model:${UNPRICED_MODEL}=echo`],
  }), 'unpriced endpoint create (POST /api/v1/providers)');

  const sourceID = await jsonField(await api(request, page, 'POST', '/api/v1/connectors/sources', {
    type: 'github',
    name: `a11y source ${Date.now()}`,
    config: { owner: 'enclava', repo: 'docs' },
  }), 'source create (POST /api/v1/connectors/sources)');

  const botID = await jsonField(await api(request, page, 'POST', '/api/v1/chatbots', {
    name: `a11y assistant ${Date.now()}`,
    model: UNPRICED_MODEL,
    rag_collection_ids: [],
    audience: ['*'],
  }), 'assistant create (POST /api/v1/chatbots)');

  const templateID = await jsonField(await api(request, page, 'POST', '/api/v1/extract/templates', {
    id: `a11y-tpl-${Date.now()}`,
    name: `a11y template ${Date.now()}`,
    system_prompt: 'Extract fields.',
    user_prompt: 'Return JSON.',
    model: UNPRICED_MODEL,
    is_active: true,
    output_schema: { type: 'object' },
  }), 'extract template create (POST /api/v1/extract/templates)');

  // memberDenied: chatbot scope but empty model grant → POST agent-runs is
  // a synchronous 403 (rejected-write case). memberRunner: granted to the
  // seeded assistant + template + the unpriced model → accepted submissions
  // fail at the worker's price reserve (async-failure cases).
  async function memberUser(name: string, scopes: string[], grants: Record<string, unknown>) {
    const email = `a11y-${name}-${Date.now()}@example.test`;
    const roleID = await jsonField(await api(request, page, 'POST', '/api/v1/identities/roles', {
      name: `a11y-${name}-${Date.now()}`,
      scopes,
      grants: {
        models: { mode: 'list', items: [] },
        endpoints: { mode: 'list', items: [] },
        assistants: { mode: 'list', items: [] },
        extract_templates: { mode: 'list', items: [] },
        ...grants,
      },
    }), `member role create (${name})`);
    const userID = await jsonField(await api(request, page, 'POST', '/api/v1/identities/users', {
      email,
      display_name: `a11y ${name}`,
      password: MEMBER_PASSWORD,
      role_ids: [roleID],
    }), `member user create (${name})`);
    return { email, roleID, userID };
  }

  const deniedUser = await memberUser('denied', ['chatbot', 'rag'], {
    assistants: { mode: 'list', items: [botID] },
  });
  const runnerUser = await memberUser('runner', ['chatbot', 'rag', 'extract'], {
    assistants: { mode: 'list', items: [botID] },
    extract_templates: { mode: 'list', items: [templateID] },
    models: { mode: 'list', items: [UNPRICED_MODEL] },
  });

  seed = { sourceID, botID, templateID, endpointID, deniedUser, runnerUser };
  await context.close();
});

// ── helpers ──────────────────────────────────────────────────────────────


function luminance(rgb: string): number {
  const m = rgb.match(/rgba?\(([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)/);
  if (!m) throw new Error(`unparseable color ${rgb}`);
  const channel = (v: number) => {
    const s = v / 255;
    return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * channel(+m[1]) + 0.7152 * channel(+m[2]) + 0.0722 * channel(+m[3]);
}

function contrastRatio(fg: string, bg: string): number {
  const l1 = luminance(fg);
  const l2 = luminance(bg);
  const [hi, lo] = l1 >= l2 ? [l1, l2] : [l2, l1];
  return (hi + 0.05) / (lo + 0.05);
}

// resolveOpaqueBg walks up the DOM from a node to its first opaque
// background — pills tone their fg over --raised surfaces.
async function effectiveBg(el: Locator): Promise<string> {
  return el.evaluate((node) => {
    let n: HTMLElement | null = node;
    while (n) {
      const bg = getComputedStyle(n).backgroundColor;
      const m = bg.match(/rgba?\(([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)(?:[,\s/]+([\d.]+))?/);
      if (m && (m[4] === undefined || +m[4] >= 1)) return bg;
      n = n.parentElement;
    }
    return 'rgb(255, 255, 255)';
  });
}

// Wait for the dashboard session to be ready on this page.
async function openChatWithBot(page: Page) {
  await page.goto(`${BASE}/dashboard/chat?assistant=${encodeURIComponent(seed.botID)}`);
  await expect(page.locator('#composer')).toBeVisible({ timeout: 15000 });
}

// Submits one message through the real composer and returns once the poll
// loop has settled into a terminal render (runline gone) — or throws naming
// the prerequisite (a deployment worker that never processes the queue).
async function submitAndAwaitFailure(page: Page, text: string) {
  await page.fill('#composer', text);
  await page.click('[data-action="chat:send"]');
  await page.waitForURL(/\/dashboard\/chat\/[^/]+$/);
  await expect
    .poll(async () => {
      const live = await page.locator('.thread-body .runline:not(.muted)').count();
      if (live > 0) return 'running';
      const muted = await page.locator('.thread-body .runline.muted').count();
      return muted > 0 ? 'done' : 'waiting';
    }, {
      timeout: 90_000,
      interval: 1_000,
      message: 'agent run never reached a terminal render — the deployment needs a running job worker for the 202→worker-failure flow',
    })
    .toBe('done');
}

// ── (1) drawer focus trap ────────────────────────────────────────────────

test('(1) drawer traps Tab, cycles last→first, never releases focus to the page', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard/sources?new=1`);
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/, { timeout: 15000 });

  // Focus lands inside the dialog, never on the backdrop/body.
  await expect.poll(() =>
    page.evaluate(() => document.getElementById('drawer')!.contains(document.activeElement)),
  ).toBe(true);

  // Walk the tab cycle forward; every stop must stay inside the drawer, and
  // at least one Tab must visibly wrap (last → first) to prove the cycle.
  // The trap cycles VISIBLE controls (app.js visibleFocusables) — later
  // wizard steps are [hidden] and Back/Continue start disabled, so the
  // selector is filtered to visible controls the same way.
  const vis = page.locator(
    '#drawer a[href], #drawer button:not([disabled]), #drawer input:not([disabled]):not([type="hidden"]), #drawer select:not([disabled]), #drawer textarea:not([disabled]), #drawer [tabindex]:not([tabindex="-1"])',
  ).filter({ visible: true });
  const n = await vis.count();
  if (n < 2) {
    throw new Error(`drawer renders ${n} focusable controls — the wizard needs at least 2 for the wrap proof`);
  }
  // Focus the last visible control, then Tab once: the trap must wrap to
  // the first visible control instead of leaving the dialog.
  await vis.last().focus();
  await page.keyboard.press('Tab');
  await expect(vis.first()).toBeFocused();
  // Shift+Tab from the first control wraps back to the last.
  await page.keyboard.down('Shift');
  await page.keyboard.press('Tab');
  await page.keyboard.up('Shift');
  await expect(vis.last()).toBeFocused();
  // And a longer walk never escapes the dialog.
  for (let i = 0; i < Math.max(12, n); i++) {
    await page.keyboard.press('Tab');
    const inside = await page.evaluate(() =>
      document.getElementById('drawer')!.contains(document.activeElement));
    if (!inside) {
      throw new Error(`Tab escaped the open drawer on press ${i + 1} — focus reached ${await page.evaluate(() => document.activeElement?.tagName)}`);
    }
  }
});

// ── (2) Esc + backdrop close, focus returns to opener ────────────────────

test('(2) Esc and backdrop close the drawer and return focus to the opener', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard/sources`);
  const addSource = page.locator('[data-action="open-drawer"]').first();
  if ((await addSource.count()) === 0) {
    throw new Error('no data-action="open-drawer" control on /dashboard/sources — the empty-state Add source contract (11-03) is missing');
  }
  await addSource.focus();
  await addSource.click();
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/, { timeout: 15000 });

  await page.keyboard.press('Escape');
  await expect(drawer).not.toHaveClass(/open/);
  await expect.poll(() =>
    page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.dataset.action === 'open-drawer'),
  ).toBe(true);

  // Backdrop close returns focus the same way.
  await addSource.focus();
  await addSource.click();
  await expect(drawer).toHaveClass(/open/);
  await page.locator('#backdrop').click({ position: { x: 8, y: 8 } });
  await expect(drawer).not.toHaveClass(/open/);
  await expect.poll(() =>
    page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.dataset.action === 'open-drawer'),
  ).toBe(true);
});

// ── (3) one overlay at a time ────────────────────────────────────────────

test('(3) popover opens with focus inside, Escape restores, and a drawer replaces it', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard`);
  const pop = page.locator('#pop');
  const userMenu = page.locator('.user-menu');

  // The popover opens from the user menu with focus landed inside it.
  await userMenu.click();
  await expect(pop).toBeVisible();
  await expect.poll(() =>
    page.evaluate(() => document.getElementById('pop')!.contains(document.activeElement)),
  ).toBe(true);

  // Escape closes the popover and returns focus to its opener.
  await page.keyboard.press('Escape');
  await expect(pop).toBeHidden();
  await expect.poll(() =>
    page.evaluate(() => document.activeElement === document.querySelector('.user-menu')),
  ).toBe(true);

  // One overlay at a time, the workable direction: while a popover is open,
  // opening a drawer closes it in-document (a navigation would hide #pop on
  // its own and prove nothing). The reverse is blocked by design — the modal
  // drawer's backdrop makes outside controls like the user menu unclickable.
  await page.goto(`${BASE}/dashboard/sources`);
  const addSource = page.locator('[data-action="open-drawer"]').first();
  await userMenu.click();
  await expect(pop).toBeVisible();
  await addSource.dispatchEvent('click');
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/, { timeout: 15000 });
  await expect(pop).toBeHidden();
  await expect.poll(() =>
    page.evaluate(() => document.getElementById('drawer')!.contains(document.activeElement)),
  ).toBe(true);
});

// ── (4) error-toast action — rejected write + accepted-async failures ────

test('(4) member rejected write renders a sticky toast with no fix link', async ({ page }) => {
  await login(page, seed.deniedUser.email, MEMBER_PASSWORD);
  await page.goto(`${BASE}/dashboard/chat`);
  // A denied operation through the real client surface: the member's model
  // grant is empty, so POST agent-runs is a synchronous 403 and api()
  // renders the sticky toast — sticky (persistent) and carrying NO action
  // link (no href to a destination the member may not reach).
  await page.evaluate(async (botID) => {
    try {
      await window.Enclava.api('POST', '/api/v1/agent-runs', {
        assistant_id: botID,
        input: 'a11y denied run',
      });
    } catch { /* rejection is the expected outcome */ }
  }, seed.botID);
  const toast = page.locator('#toast.show.error');
  await expect(toast).toBeVisible();
  await expect(toast.locator('a[href]')).toHaveCount(0);
});

test('(4a) chat worker failure surfaces the Set price fix for admins and the fallback for members', async ({ page }) => {
  test.setTimeout(120_000);
  await login(page);
  await openChatWithBot(page);
  await submitAndAwaitFailure(page, 'a11y unpriced run');

  // Capability-on: sticky toast carries the mapped Set price link and the
  // failed runline repeats it in place.
  const toast = page.locator('#toast.show.error');
  await expect(toast).toBeVisible();
  await expect(toast.locator('a[href="/dashboard/models/pricing"]')).toContainText('Set price');
  const runline = page.locator('.thread-body .runline').last();
  await expect(runline.locator('a[href="/dashboard/models/pricing"]')).toContainText('Set price');

  // Capability-off: identical flow under a member session → administrator
  // fallback text and never an href.
  await page.context().clearCookies();
  await login(page, seed.runnerUser.email, MEMBER_PASSWORD);
  await openChatWithBot(page);
  await submitAndAwaitFailure(page, 'a11y member unpriced run');
  const memberToast = page.locator('#toast.show.error');
  await expect(memberToast).toBeVisible();
  await expect(memberToast).toContainText('administrator');
  await expect(memberToast.locator('a[href]')).toHaveCount(0);
  const memberLine = page.locator('.thread-body .runline').last();
  await expect(memberLine).toContainText('administrator');
  await expect(memberLine.locator('a[href]')).toHaveCount(0);
});

test('(4b) extract worker failure surfaces the same capability-filtered fix on the failed job row', async ({ page }) => {
  test.setTimeout(120_000);

  async function submitExtractAs(email: string | null, fileName: string) {
    if (email) {
      await page.context().clearCookies();
      await login(page, email, MEMBER_PASSWORD);
    } else {
      await login(page);
    }
    await page.goto(`${BASE}/dashboard/extract`);
    const select = page.locator('[data-extract-template-select]');
    await select.selectOption(seed.templateID);
    await page.setInputFiles('[data-extract-file]', {
      name: fileName,
      mimeType: 'text/plain',
      buffer: Buffer.from(`a11y extract probe ${Date.now()}`),
    });
    await page.click('[data-action="extract:submit"]');
    await page.goto(`${BASE}/dashboard/extract?tab=jobs`);
    // The accepted job lands in the live jobs table and reaches a terminal
    // render once the worker fails it (the htmx fragment polls every 3s).
    const failed = page.locator('#extract-jobs tr', { hasText: fileName }).first();
    await expect(failed.locator('.pill.bad')).toBeVisible({ timeout: 90_000 });
    return failed;
  }

  // Capability-on (admin holds models.manage): the failed row's subline
  // carries the Set price link.
  const adminRow = await submitExtractAs(null, 'a11y-admin.txt');
  await expect(adminRow.locator('a[href="/dashboard/models/pricing"]')).toContainText('Set price', { timeout: 90_000 });
  // Capability-off (member): fallback sentence, no href.
  const memberRow = await submitExtractAs(seed.runnerUser.email, 'a11y-member.txt');
  await expect(memberRow.locator('.subline.bad')).toContainText('administrator', { timeout: 90_000 });
  await expect(memberRow.locator('a[href]')).toHaveCount(0);
});

// ── (5) skip link is the first tab stop and activates to #content ────────

test('(5) skip link is the first Tab stop and activates focus into #content', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard`);
  await page.keyboard.press('Tab');
  const skip = page.locator('a.skip');
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

// ── (6) Reconnect deep link opens the edit drawer on its credential step ──

test('(6) /dashboard/sources?edit= opens the credential step; members never see it', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard/sources?edit=${encodeURIComponent(seed.sourceID)}`);
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/, { timeout: 15000 });
  await expect(drawer.locator('[data-source-edit]')).toBeVisible();
  await expect(drawer.locator('[data-edit-credential]')).toBeVisible();

  await page.context().clearCookies();
  await login(page, seed.runnerUser.email, MEMBER_PASSWORD);
  const res = await page.goto(`${BASE}/dashboard/sources?edit=${encodeURIComponent(seed.sourceID)}`);
  // Members lack connectors.manage — the page itself is denied and the
  // drawer never opens (no Reconnect affordance anywhere in the DOM).
  if (res && res.status() === 200) {
    await expect(drawer).not.toHaveClass(/open/);
    await expect(page.locator('[data-source-edit]')).toHaveCount(0);
  } else {
    expect([403, 404]).toContain(res!.status());
  }
});

// ── contrast: computed styles on rendered pills + primary button ─────────

test('contrast: rendered pills and primary ink meet 4.5:1 in both themes', async ({ page }) => {
  await login(page);
  await page.goto(`${BASE}/dashboard/sources`);
  const pills = page.locator('.pill');
  const count = await pills.count();
  if (count === 0) {
    throw new Error('no rendered .pill elements on /dashboard/sources — the seeded source should render a status pill; an empty sample never counts as passing');
  }
  const btn = page.locator('.btn.primary').first();
  if ((await btn.count()) === 0) {
    throw new Error('no rendered .btn.primary on /dashboard/sources — the page-primary-action contract is unmet');
  }

  for (const theme of ['', 'light']) {
    await page.evaluate((t) => {
      document.body.classList.toggle('light', t === 'light');
    }, theme);
    for (let i = 0; i < count; i++) {
      const fg = await pills.nth(i).evaluate((n) => getComputedStyle(n).color);
      const bg = await effectiveBg(pills.nth(i));
      const ratio = contrastRatio(fg, bg);
      if (ratio < 4.5) {
        throw new Error(`pill ${i} contrast ${ratio.toFixed(2)}:1 < 4.5 in ${theme || 'dark'} theme (fg ${fg} on ${bg})`);
      }
    }
    const btnFg = await btn.evaluate((n) => getComputedStyle(n).color);
    const btnBg = await btn.evaluate((n) => getComputedStyle(n).backgroundColor);
    const btnRatio = contrastRatio(btnFg, btnBg);
    if (btnRatio < 4.5) {
      throw new Error(`.btn.primary ink/bg contrast ${btnRatio.toFixed(2)}:1 < 4.5 in ${theme || 'dark'} theme`);
    }
  }
});

// ── reduced motion: emulated media kills pulse + overlay transitions ─────

test('motion: prefers-reduced-motion collapses pulse animation and chrome transitions', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await login(page);
  await page.goto(`${BASE}/dashboard/sources`);

  // Render a real .pulse if the list has none — the rule, not the row, is
  // under test, but a live rendered one is preferred.
  let sel = '.pulse';
  if ((await page.locator('.pulse').count()) === 0) {
    await page.evaluate(() => {
      const d = document.createElement('div');
      d.className = 'pulse';
      d.id = 'a11y-pulse-probe';
      document.body.appendChild(d);
    });
    sel = '#a11y-pulse-probe';
  }
  const anim = await page.locator(sel).first().evaluate((n) => getComputedStyle(n).animationName);
  if (anim !== 'none') {
    throw new Error(`.pulse animation-name = ${anim} under reduced motion, want none`);
  }
  for (const target of ['.drawer', '.toast', '.sidebar']) {
    const dur = await page.locator(target).first().evaluate((n) => getComputedStyle(n).transitionDuration);
    if (!/^0s(,\s*0s)*$/.test(dur)) {
      throw new Error(`${target} transition-duration = ${dur} under reduced motion, want 0s`);
    }
  }
});

// ── 390px: no page-level horizontal scroll ───────────────────────────────

test('overflow: no horizontal page scroll at 390px on a list page', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page);
  await page.goto(`${BASE}/dashboard/sources`);
  await expect(page.locator('.mobilebar')).toBeVisible();
  const overflow = await page.evaluate(() =>
    document.documentElement.scrollWidth - document.documentElement.clientWidth);
  if (overflow > 0) {
    throw new Error(`page scrolls ${overflow}px horizontally at 390px — the D-23/390px contract is broken`);
  }
});
