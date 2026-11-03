import { test, expect, api, apiRaw, seedData } from './fixtures';

type Reservation = { id: string; model: string };

async function unknownReservations(): Promise<Reservation[]> {
  const rows: Reservation[] = [];
  let cursor = '';
  do {
    const query = new URLSearchParams({ state: 'unknown' });
    if (cursor) query.set('cursor', cursor);
    const page = await api<{ reservations: Reservation[]; next_cursor: string }>(
      'GET', `/api/v1/admin/reservations?${query}`,
    );
    rows.push(...page.reservations);
    cursor = page.next_cursor;
  } while (cursor);
  return rows;
}

test('unknown reservation is listed and reconciled through the drawer', async ({ page }) => {
  const seed = seedData();
  const before = new Set((await unknownReservations()).map(row => row.id));
  const completion = await apiRaw('POST', '/v1/chat/completions', {
    model: seed.chatModel,
    messages: [{ role: 'user', content: 'E2E reservation without usage' }],
    max_tokens: 16,
  });
  expect(completion.status, completion.body).toBe(200);
  const created = (await unknownReservations()).filter(row => !before.has(row.id) && row.model === seed.chatModel);
  expect(created).toHaveLength(1);
  const id = created[0].id;

  const view = await page.goto('/dashboard/usage/reservations?state=unknown');
  expect(view?.status()).toBe(200);
  const row = page.locator('#usage-reservations tr', { hasText: id });
  const drawerResponse = page.waitForResponse(r => r.url().includes('/dashboard/partials/usage/reservation-drawer?') && r.request().method() === 'GET');
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: 'Resolve' }).click();
  expect((await drawerResponse).status()).toBe(200);
  const drawer = page.locator('#drawer');
  await expect(drawer).toContainText(id);
  const form = drawer.locator('[data-reservation-form]');
  await form.locator('[name="input_tokens"]').fill('12');
  await form.locator('[name="cached_input_tokens"]').fill('4');
  await form.locator('[name="output_tokens"]').fill('8');
  await form.getByRole('button', { name: 'Resolve reservation' }).click();
  const confirm = drawer.locator('.confirm-group');
  await expect(confirm).toBeVisible();
  const resolved = page.waitForResponse(r => r.url().endsWith(`/api/v1/admin/reservations/${id}/resolve`) && r.request().method() === 'POST');
  const refreshed = page.waitForResponse(r => r.url().includes('/dashboard/partials/usage/reservations?') && r.request().method() === 'GET');
  await confirm.locator('[data-action="confirm-commit"]').click();
  const response = await resolved;
  expect(response.status()).toBe(200);
  expect(response.request().postDataJSON()).toEqual({ input_tokens: 12, cached_input_tokens: 4, output_tokens: 8 });
  await expect(row).toHaveCount(0);
  expect((await unknownReservations()).some(item => item.id === id)).toBe(false);
  expect((await refreshed).status()).toBe(200);
});
