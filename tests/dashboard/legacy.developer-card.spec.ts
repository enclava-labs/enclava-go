import { test, expect, login, ADMIN_EMAIL, ADMIN_PASSWORD, MEMBER_EMAIL, MEMBER_PASSWORD } from './fixtures';

// legacy.developer-card.spec.ts — P-12/UX-M03 end to end in a real Chromium:
// a member files "Request API access" from the Account developer card and an
// administrator sees the request on the Home attention feed with the
// create-key link and dismiss. Persistence is asserted through the server's
// re-rendered rows, never a toast claim.
//
// Runs in every project; each test logs in as the role it needs on a fresh
// context (admin.json for the admin half).

test.describe('developer card and API access requests', () => {
  test('member sees the card and files a request; admin sees it on Home and dismisses', async ({ page, browser }) => {
    if (!ADMIN_EMAIL || !ADMIN_PASSWORD) {
      throw new Error('ADMIN_EMAIL and ADMIN_PASSWORD are required — seed the admin and set both');
    }

    // ── Member: the developer card offers the request affordance. ──
    await login(page, MEMBER_EMAIL, MEMBER_PASSWORD);
    await page.goto('/dashboard/account');
    const card = page.locator('#account-dev');
    await expect(card).toBeVisible();
    await expect(card).toContainText('API keys and spend are managed by an administrator');
    const note = `e2e-request-${Date.now()}`;
    await page.locator('#account-api-access-note').fill(note);
    // Wait for the RESPONSE, not just the click — the mutation must land.
    const [request] = await Promise.all([
      page.waitForResponse((r) => r.url().includes('/dashboard/account/api-access-request') && r.request().method() === 'POST'),
      page.getByRole('button', { name: 'Request API access' }).click(),
    ]);
    expect(request.status()).toBe(200);
    // Pending state replaces the form (the page reloads on success).
    await expect(card).toContainText('waiting for an administrator');
    await expect(page.locator('#account-api-access-form')).toHaveCount(0);

    // ── Admin: the request sits on the Home attention feed. ──
    const adminPage = await browser.newPage();
    await login(adminPage, ADMIN_EMAIL, ADMIN_PASSWORD);
    await adminPage.goto('/dashboard');
    const row = adminPage.locator('.attention', { hasText: 'requested API access' }).filter({ hasText: note });
    await expect(row).toBeVisible();
    await expect(row.getByRole('link', { name: 'Create a key' })).toHaveAttribute('href', '/dashboard/access/api-keys?new=1');

    await row.getByRole('button', { name: 'Dismiss' }).click();
    const [dismissed] = await Promise.all([
      adminPage.waitForResponse((r) => r.url().includes('/dismiss') && r.request().method() === 'POST'),
      row.getByRole('button', { name: 'Confirm', exact: true }).click(),
    ]);
    expect(dismissed.status()).toBe(204);
    // The fragment re-renders without the dismissed row.
    await expect(adminPage.locator('.attention', { hasText: 'requested API access' }).filter({ hasText: note })).toHaveCount(0);
    await adminPage.close();
  });
});
