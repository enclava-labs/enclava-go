import { test, expect, api, BASE, seedData } from './fixtures';

test('assistant coverage can repair missing member grants', async ({ page }) => {
  const seed = seedData();
  const roleURL = `/api/v1/identities/roles/${seed.memberRoleID}`;
  const role = await api<{ grants: unknown }>('GET', roleURL);
  const bot = await api<{ id: string }>('POST', '/api/v1/chatbots', {
    name: 'Coverage repair test', model: seed.chatModel, provider_id: seed.endpointID,
    rag_collection_ids: [seed.collectionID], audience: ['*'],
  });
  try {
    await page.goto(`${BASE}/dashboard/assistants`);
    const row = page.locator('tr').filter({ hasText: 'Coverage repair test' });
    const member = row.locator('.avail-line').filter({ hasText: 'member' });
    await expect(member.getByText('Not granted', { exact: true })).toBeVisible();
    await row.getByRole('button', { name: 'Edit' }).click();
    await page.getByRole('button', { name: 'Fix for member', exact: true }).click();
    await expect(page.locator('.grant-suggested-tag').first()).toBeVisible();
    const response = page.waitForResponse(res => res.url().endsWith(roleURL) && res.request().method() === 'PATCH');
    await page.getByRole('button', { name: 'Save role' }).click();
    expect((await response).ok()).toBeTruthy();
    await expect(member.getByText('Reachable', { exact: true })).toBeVisible();
  } finally {
    await api('PATCH', roleURL, { grants: role.grants });
    await api('DELETE', `/api/v1/chatbots/${bot.id}`);
  }
});

test('empty specific audience requires explicit confirmation', async ({ page }) => {
  seedData();
  await page.goto(`${BASE}/dashboard/assistants?new=1`);
  const form = page.locator('#assistant-form');
  await form.locator('[name="name"]').fill('Audience guard test');
  await form.getByText('Specific members').click();
  await form.getByRole('button', { name: 'Save assistant' }).click();
  await expect(form.getByText('Nobody can use this assistant yet')).toBeVisible();
  await expect(form.getByRole('button', { name: 'Save anyway' })).toBeVisible();
});
