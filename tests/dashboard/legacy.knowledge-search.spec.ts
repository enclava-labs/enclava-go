import { test, expect, BASE, login, MANAGER_EMAIL, MEMBER_PASSWORD, seedData } from './fixtures';

// KC-I02: the collection Search tab's mode chips carry only data-mode —
// before the fix the module returned early on the missing [data-action]
// ancestor, so Keywords/Meaning never switched the hidden mode input.
test('collection search mode chips switch mode and aria-pressed', async ({ page }) => {
  const { collectionID } = seedData();
  await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
  await page.goto(`${BASE}/dashboard/knowledge/${collectionID}/search`);

  const hybrid = page.locator('.chip[data-mode="hybrid"]');
  const lexical = page.locator('.chip[data-mode="lexical"]');
  const semantic = page.locator('.chip[data-mode="semantic"]');
  const modeInput = page.locator('#knowledgeSearchMode');

  await expect(hybrid).toHaveAttribute('aria-pressed', 'true');
  await expect(modeInput).toHaveValue('hybrid');

  await lexical.click();
  await expect(modeInput).toHaveValue('lexical');
  await expect(lexical).toHaveAttribute('aria-pressed', 'true');
  await expect(lexical).toHaveClass(/active/);
  await expect(hybrid).toHaveAttribute('aria-pressed', 'false');
  await expect(hybrid).not.toHaveClass(/active/);

  await semantic.click();
  await expect(modeInput).toHaveValue('semantic');
  await expect(semantic).toHaveAttribute('aria-pressed', 'true');
  await expect(lexical).toHaveAttribute('aria-pressed', 'false');
  await hybrid.click();
  await expect(modeInput).toHaveValue('hybrid');
  await expect(hybrid).toHaveAttribute('aria-pressed', 'true');
  await expect(semantic).toHaveAttribute('aria-pressed', 'false');


  // The selected mode reaches the shared results fragment on submit.
  await lexical.click();
  await page.locator('#knowledgeSearchQuery').fill('enclava');
  const responsePromise = page.waitForResponse((r) =>
    r.url().includes('/dashboard/partials/search/results') && r.url().includes('mode=lexical'));
  await page.locator('#knowledgeSearchForm button[type="submit"]').click();
  const response = await responsePromise;
  expect(response.ok()).toBeTruthy();
});
