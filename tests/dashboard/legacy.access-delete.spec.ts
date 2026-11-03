import { test, expect, sessionApi } from './fixtures';

test('deleting a person requires confirmation and refreshes the people list', async ({ page }) => {
  const email = `delete-${Date.now()}-${Math.random().toString(36).slice(2)}@example.com`;
  const created = await sessionApi(page, 'POST', '/api/v1/identities/users', {
    email,
    display_name: 'Person to delete',
    password: 'spec-pass-123',
    role_ids: [],
  });
  expect(created.status(), await created.text()).toBe(201);
  const { id } = await created.json() as { id: string };
  expect(id).toBeTruthy();
  let deleted = false;

  try {
    await page.goto('/dashboard/access/people');
    const row = page.locator('#access-people tr', { hasText: email });
    await expect(row).toBeVisible();
    await row.getByRole('button', { name: 'Manage' }).click();
    const drawer = page.locator('#drawer');
    const button = drawer.getByRole('button', { name: 'Delete person' });
    await button.click();
    const group = drawer.locator('.confirm-group');
    await expect(group.locator('.confirm-text')).toHaveText('Removes their access now: sessions and API keys are revoked. Past usage and audit history stay.');
    await group.getByRole('button', { name: 'Cancel' }).click();
    expect((await sessionApi(page, 'GET', `/api/v1/identities/users/${id}`)).status()).toBe(200);
    await expect(row).toBeVisible();

    await button.click();
    const response = page.waitForResponse(r => r.request().method() === 'DELETE' && new URL(r.url()).pathname === `/api/v1/identities/users/${id}`);
    await group.getByRole('button', { name: 'Confirm' }).click();
    const result = await response;
    expect(result.status()).toBe(200);
    deleted = true;
    await expect(row).toHaveCount(0);
    expect((await sessionApi(page, 'GET', `/api/v1/identities/users/${id}`)).status()).toBe(404);
  } finally {
    if (!deleted) {
      const cleanup = await sessionApi(page, 'DELETE', `/api/v1/identities/users/${id}`);
      expect(cleanup.ok(), await cleanup.text()).toBeTruthy();
    }
  }
});
