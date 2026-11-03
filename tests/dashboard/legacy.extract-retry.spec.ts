import { test, expect } from './fixtures';

test('interrupted extraction retry warns before sending the mutation', async ({ page }) => {
  await page.route('**/dashboard/partials/extract/jobs**', route => route.fulfill({
    status: 200,
    contentType: 'text/html',
    body: `<div id="extract-jobs" hx-get="/dashboard/partials/extract/jobs" hx-trigger="refresh from:body" hx-swap="outerHTML">
      <table><tbody><tr data-job-id="job-interrupted"><td>unknown</td><td>
        <button type="button" data-action="confirm" data-confirm="Retry this interrupted extraction? The previous attempt may already have been billed — retrying can bill again." data-extract-retry="job-interrupted">Retry</button>
      </td></tr></tbody></table>
    </div>`,
  }));
  await page.route('**/api/v1/extract/jobs/job-interrupted/retry', route => route.fulfill({
    status: 202,
    contentType: 'application/json',
    body: JSON.stringify({ job_id: 'job-new', status: 'queued' }),
  }));

  await page.goto('/dashboard/extract?tab=jobs', { waitUntil: 'domcontentloaded' });
  await expect(page.locator('#extract-jobs')).toBeVisible({ timeout: 15_000 });
  await page.waitForFunction(() => 'htmx' in window && 'Enclava' in window, null, { timeout: 15_000 });
  const fragment = page.waitForResponse(r => r.url().includes('/dashboard/partials/extract/jobs'), { timeout: 15_000 });
  await page.evaluate(() => document.body.dispatchEvent(new Event('refresh')));
  expect((await fragment).status()).toBe(200);

  const retry = page.locator('[data-extract-retry="job-interrupted"]');
  await expect(retry).toBeVisible({ timeout: 15_000 });
  await retry.click();
  await expect(page.locator('.confirm-group .confirm-text')).toContainText('previous attempt may already have been billed');
  const response = page.waitForResponse(
    r => r.url().includes('/api/v1/extract/jobs/job-interrupted/retry') && r.request().method() === 'POST',
    { timeout: 15_000 },
  );
  await page.locator('.confirm-group [data-action="confirm-commit"]').click();
  expect((await response).status()).toBe(202);
});
