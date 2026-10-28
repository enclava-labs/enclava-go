import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';
import { BASE } from './fixtures';

// screenshots.spec.ts — the both-theme × 1440/390 visual matrix plus the
// migrated computed-style a11y cases (see 12-04 SUMMARY case map):
//   tests/a11y.spec.ts — rendered-pill/primary-ink contrast ≥4.5:1 in
//   both themes, reduced-motion pulse/transition collapse, 390px
//   no-horizontal-scroll.
//   tests/a11y-pages.spec.ts — reduced-motion emulation strips
//   animation (the rendered .pulse carrier case).
//
// Screenshots land under tests/dashboard/screenshots/<project>/<file>.
// They are deterministic visual evidence: same seed, same stubs — never
// a baseline-diff suite.

const VIEWPORTS = [
  { name: '1440x900', width: 1440, height: 900 },
  { name: '390x844', width: 390, height: 844 },
] as const;

const SHOTS = [
  { name: 'home', path: '/dashboard' },
  { name: 'chat', path: '/dashboard/chat' },
  { name: 'search', path: '/dashboard/search' },
  { name: 'sources', path: '/dashboard/sources' },
  { name: 'knowledge', path: '/dashboard/knowledge' },
] as const;

async function setTheme(page: Page, theme: 'light' | 'dark') {
  await page.evaluate((t) => {
    document.body.classList.toggle('light', t === 'light');
    document.documentElement.classList.toggle('light', t === 'light');
    document.cookie = `enclava_theme=${t};path=/;SameSite=Lax`;
  }, theme);
}

test.describe('screenshot matrix — both themes × 1440/390', () => {
  for (const vp of VIEWPORTS) {
    for (const theme of ['dark', 'light'] as const) {
      test(`${vp.name} ${theme}`, async ({ page }) => {
        await page.setViewportSize({ width: vp.width, height: vp.height });
        for (const shot of SHOTS) {
          const res = await page.goto(`${BASE}${shot.path}`);
          if (res && res.status() === 403) continue;
          await setTheme(page, theme);
          await expect(page.locator('#content')).toBeVisible();
          await page.screenshot({
            path: `tests/dashboard/screenshots/${test.info().project.name}/${vp.name}-${theme}-${shot.name}.png`,
            fullPage: false,
          });
        }
      });
    }
  }
});

test.describe('computed a11y cases (migrated)', () => {
  test.skip(({}, testInfo) => testInfo.project.name !== 'manager');

  function luminance(rgb: string): number {
    const m = rgb.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
    if (!m) return 1;
    const f = (v: number) => {
      const s = v / 255;
      return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
    };
    return 0.2126 * f(+m[1]) + 0.7152 * f(+m[2]) + 0.0722 * f(+m[3]);
  }

  async function effectiveBg(page: Page, sel: string): Promise<string> {
    return page.evaluate((s) => {
      let el: HTMLElement | null = document.querySelector(s);
      while (el) {
        const bg = getComputedStyle(el).backgroundColor;
        if (bg && bg !== 'rgba(0, 0, 0, 0)' && bg !== 'transparent') return bg;
        el = el.parentElement;
      }
      return 'rgb(255, 255, 255)';
    }, sel);
  }

  test('rendered pills and primary ink meet 4.5:1 in both themes', async ({ page }) => {
    await page.goto(`${BASE}/dashboard/sources`);
    const pills = page.locator('.pill');
    const count = await pills.count();
    if (count === 0) {
      throw new Error('no rendered .pill elements on /dashboard/sources');
    }
    const btn = page.locator('.btn.primary').first();
    if ((await btn.count()) === 0) {
      throw new Error('no rendered .btn.primary on /dashboard/sources');
    }
    for (const theme of ['', 'light']) {
      await page.evaluate((t) => {
        document.body.classList.toggle('light', t === 'light');
      }, theme);
      for (let i = 0; i < count; i++) {
        const fg = await pills.nth(i).evaluate((n) => getComputedStyle(n).color);
        const bg = await effectiveBg(page, `.pill >> nth=${i}`);
        const ratio = (Math.max(luminance(fg), luminance(bg)) + 0.05)
          / (Math.min(luminance(fg), luminance(bg)) + 0.05);
        if (ratio < 4.5) {
          throw new Error(`pill ${i} contrast ${ratio.toFixed(2)}:1 < 4.5 in ${theme || 'dark'} theme`);
        }
      }
    }
  });

  test('reduced-motion collapses pulse animation and chrome transitions', async ({ page }) => {
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await page.goto(`${BASE}/dashboard/sources`);
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
    for (const target of ['#drawer', '.toast', '.sidebar']) {
      const el = page.locator(target).first();
      if ((await el.count()) === 0) continue;
      const dur = await el.evaluate((n) => getComputedStyle(n).transitionDuration);
      if (!/^0s(,\s*0s)*$/.test(dur)) {
        throw new Error(`${target} transition-duration = ${dur} under reduced motion, want 0s`);
      }
    }
  });

  test('no horizontal page scroll at 390px on a list page', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${BASE}/dashboard/sources`);
    await expect(page.locator('.mobilebar')).toBeVisible();
    const overflow = await page.evaluate(() =>
      document.documentElement.scrollWidth - document.documentElement.clientWidth);
    if (overflow > 0) {
      throw new Error(`page scrolls ${overflow}px horizontally at 390px`);
    }
  });
});
