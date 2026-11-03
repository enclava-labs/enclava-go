import { test, expect, api, seedData } from './fixtures';

test('dedicated enclave repo survives endpoint editing', async ({ page }) => {
  seedData();
  const endpoint = await api<{ ID: string }>('POST', '/api/v1/providers', {
    protocol: 'tinfoil', base_origin: 'https://enclave.example.test',
    enclave_repo: 'owner/original', capabilities: [],
  });
  await page.goto('/dashboard/models');
  await page.locator(`[data-action="models:endpoint"][data-endpoint-id="${endpoint.ID}"]`).click();
  const drawer = page.locator('#drawer');
  await expect(drawer.locator('[name="enclave_repo"]')).toHaveValue('owner/original');
  await drawer.locator('[name="enclave_repo"]').fill('owner/updated');
  const response = page.waitForResponse(r => r.url().endsWith('/api/v1/providers/' + endpoint.ID) && r.request().method() === 'PUT');
  await drawer.getByRole('button', { name: 'Save changes' }).click();
  const saved = await response;
  expect(saved.ok()).toBeTruthy();
  expect(saved.request().postDataJSON().enclave_repo).toBe('owner/updated');
});
