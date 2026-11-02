import { test, expect, Page } from '@playwright/test';
import * as path from 'path';

// usage-dst.spec.ts — regression for the usage.js skipped-midnight fix:
// when a zone's DST gap swallows local midnight (America/Santiago springs
// forward 00:00→01:00 on 2025-09-07), a budget's date must submit the first
// valid instant of the selected day, never an instant on the previous day.
//
// The case needs no backend or session: it drives the real page module
// against a stubbed window.Enclava and captures the POST body.

const USAGE_JS = path.resolve(__dirname, '../../internal/httpx/dashboard/static/pages/usage.js');

const FORM_HTML = `
<form data-usage-form>
  <input name="name" value="dst-budget">
  <select name="kind"><option value="amount" selected>amount</option></select>
  <input name="limit_value" value="25000000">
  <input name="currency" value="USD">
  <select name="period"><option value="monthly" selected>monthly</option></select>
  <select name="timezone"><option value="America/Santiago" selected>America/Santiago</option></select>
  <input name="starts_at" type="date">
  <button type="submit">Create budget</button>
</form>`;

async function mountForm(page: Page) {
  await page.setContent(FORM_HTML);
  await page.addScriptTag({
    content: `
      window.__posted = null;
      window.Enclava = {
        api: (method, url, body) => {
          window.__posted = { method, url, body };
          return Promise.resolve({});
        },
        closeDrawer: () => {},
        toast: () => {},
        refresh: () => {},
        errorAction: () => ({}),
      };`,
  });
  await page.addScriptTag({ path: USAGE_JS });
}

async function submitStart(page: Page, day: string): Promise<string> {
  await page.locator('input[name="starts_at"]').fill(day);
  await page.locator('form').dispatchEvent('submit');
  await expect.poll(() => page.evaluate('window.__posted')).toBeTruthy();
  const posted: { body: { starts_at: string } } | null = await page.evaluate('window.__posted');
  return posted!.body.starts_at;
}

test('a skipped local midnight submits the first instant of the selected day', async ({ page }) => {
  await mountForm(page);

  // 2025-09-07 has no 00:00 in America/Santiago — the gap jumps to 01:00,
  // i.e. 04:00Z. Before the fix this submitted 03:00Z (Sept 6 23:00 local).
  const iso = await submitStart(page, '2025-09-07');
  expect(iso).toBe('2025-09-07T04:00:00.000Z');
  const localDay = await page.evaluate(
    `new Intl.DateTimeFormat('en-US', {
       timeZone: 'America/Santiago',
       year: 'numeric', month: '2-digit', day: '2-digit',
     }).format(new Date('${iso}'))`,
  );
  expect(localDay).toBe('09/07/2025');
});

test('ordinary dates still submit local midnight', async ({ page }) => {
  await mountForm(page);
  // 2025-09-08 00:00 in Santiago (UTC-3 after the gap) is 03:00Z.
  expect(await submitStart(page, '2025-09-08')).toBe('2025-09-08T03:00:00.000Z');
  // 2025-04-06 00:00 in Santiago (UTC-4 on the fall-back day) is 04:00Z.
  expect(await submitStart(page, '2025-04-06')).toBe('2025-04-06T04:00:00.000Z');
});
