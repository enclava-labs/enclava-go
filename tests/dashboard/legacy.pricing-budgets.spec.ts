import { test, expect, api, seedData } from './fixtures';

test('one price drawer saves all three categories', async ({ page }) => {
  seedData();
  const model = `price-batch-${Date.now()}`;
  const endpoint = await api<{ ID: string }>('POST', '/api/v1/providers', {
    protocol: 'openai_compatible', base_origin: 'https://pricing.example.test',
    capabilities: ['chat/completions', `model:${model}`],
  });
  await page.goto('/dashboard/models/pricing');
  const row = page.locator(`tr[data-price-row="${endpoint.ID}|${model}|chat/completions"]`);
  await expect(row).toContainText('Output not priced');
  await row.getByRole('button', { name: 'Set price' }).click();
  const drawer = page.locator('#drawer');
  await drawer.locator('#models-price-rate-input').fill('2.5');
  await drawer.locator('#models-price-rate-cached_input').fill('1.25');
  await drawer.locator('#models-price-rate-output').fill('5');
  await expect(drawer.locator('[data-models-price-preview="output"]')).toContainText('$5.00');
  const response = page.waitForResponse(r => r.url().endsWith('/api/v1/admin/pricing') && r.request().method() === 'POST');
  await drawer.getByRole('button', { name: 'Save prices' }).click();
  const saved = await response;
  expect(saved.status()).toBe(201);
  expect(saved.request().postDataJSON().prices).toHaveLength(3);
  for (const [unit, price] of [['input', '$2.50'], ['cached_input', '$1.25'], ['output', '$5.00']]) {
    await expect(row.locator(`td[data-price-cell="${endpoint.ID}|${unit}"]`)).toContainText(price);
  }
});

test('decimal amount budget stores micros and displays a human limit', async ({ page }) => {
  seedData();
  const name = `decimal-budget-${Date.now()}`;
  await page.goto('/dashboard/usage/budgets');
  await page.getByRole('button', { name: 'New budget' }).first().click();
  const drawer = page.locator('#drawer');
  await drawer.locator('#budget-name').fill(name);
  await drawer.locator('#budget-limit-value').fill('-1');
  await drawer.getByRole('button', { name: 'Create budget' }).click();
  await expect(drawer.locator('.field-error')).toContainText('greater than zero');
  await drawer.locator('#budget-limit-value').fill('10000000');
  await expect(drawer.locator('[data-budget-warn]')).toBeVisible();
  await drawer.locator('#budget-limit-value').fill('10.00');
  await expect(drawer.locator('[data-budget-hint]')).toContainText('10000000 micro-USD');
  const response = page.waitForResponse(r => r.url().endsWith('/api/v1/admin/budgets') && r.request().method() === 'POST');
  await drawer.getByRole('button', { name: 'Create budget' }).click();
  const saved = await response;
  expect(saved.status()).toBe(201);
  expect(saved.request().postDataJSON().limit_value).toBe(10000000);
  const row = page.locator('#usage-budgets tr', { hasText: name });
  await expect(row.locator('[data-label="Limit"]')).toContainText('$10.00 / month');
  await expect(row.locator('[data-label="Limit"] span')).toHaveAttribute('title', 'stored as 10000000');
});

test('discovery merges confirmations and retains unknown capabilities', async ({ page }) => {
  const seed = seedData();
  await page.route('**/api/v1/providers/discover', route => route.fulfill({
    status: 200, contentType: 'application/json', body: JSON.stringify({ discovered: {
      models: [seed.chatModel], probe_model: seed.chatModel,
      capabilities: { 'chat/completions': 'unknown', embeddings: 'yes', vision: 'unknown' },
      probes: [
        { capability: 'chat/completions', model: seed.chatModel, result: 'unknown', error: 'unavailable' },
        { capability: 'embeddings', model: seed.embedModel, result: 'yes', error: '' },
      ],
    } }),
  }));
  await page.goto('/dashboard/models');
  await page.locator(`[data-action="models:endpoint"][data-endpoint-id="${seed.endpointID}"]`).click();
  const drawer = page.locator('#drawer');
  const caps = drawer.locator('input[name="capabilities"]');
  const before = (await caps.inputValue()).split(',').filter(Boolean);
  expect(before.length).toBeGreaterThan(0);
  const response = page.waitForResponse(r => r.url().endsWith('/api/v1/providers/discover') && r.request().method() === 'POST');
  await drawer.getByRole('button', { name: 'Discover from server' }).click();
  await response;
  await expect(drawer.locator('[data-models-probe-results]')).toContainText(`embeddings: yes · ${seed.embedModel}`);
  await expect(drawer.locator('[data-models-probe-results]')).toContainText(`chat/completions: unknown · ${seed.chatModel}`);
  const after = (await caps.inputValue()).split(',');
  for (const cap of before) expect(after).toContain(cap);
  expect(after).toContain('embeddings');
});
