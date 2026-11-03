import { test, expect } from './fixtures';

test('GitHub wizard explains collaborator credential requirements', async ({ page }) => {
  await page.goto('/dashboard/sources?new=1');
  const drawer = page.locator('#drawer');
  await expect(drawer).toHaveClass(/open/);
  await drawer.locator('[data-provider="github"]').click();
  await expect(drawer.getByText(/Collaborator and membership lookup still requires/)).toBeVisible();
  await expect(drawer.getByText(/without it only content syncs/).first()).toBeVisible();
});
