import { test, expect, BASE, login, MANAGER_EMAIL, MEMBER_PASSWORD, seedData } from './fixtures';
import * as path from 'node:path';

test('manager sees a read-only collection without an upload control', async ({ page }) => {
  const { collectionID } = seedData();
  await login(page, MANAGER_EMAIL, MEMBER_PASSWORD);
  await page.goto(`${BASE}/dashboard/knowledge/${collectionID}/documents`);
  const panel = page.locator('#knowledge-documents');
  await expect(panel.getByText('Read-only', { exact: true })).toBeVisible();
  await expect(panel.getByText('Ask a knowledge manager to grant write access.')).toBeVisible();
  await expect(panel.getByText('Upload document')).toHaveCount(0);
  await expect(panel.locator('[data-knowledge-upload-input]')).toHaveCount(0);
  await expect(panel.locator('a[aria-pressed]')).toHaveCount(0);
  await expect(panel.locator('a[aria-current="true"]')).toHaveCount(1);
  await page.evaluate(() => document.body.dispatchEvent(new Event('refresh', { bubbles: true })));
  await expect(panel.getByText('Read-only', { exact: true })).toBeVisible();
});

test('upload denial remains inline after the documents panel refreshes', async ({ page }) => {
  await page.setContent(`<div id="knowledge-documents" hx-get="/documents?collection=c1">
    <p id="knowledge-documents-error" hidden></p>
    <input type="file" data-knowledge-upload-input>
  </div>`);
  await page.addScriptTag({ content: `window.Enclava = {
    api: () => Promise.reject({message: 'Missing permission: write access to this collection'}),
    errorAction: (error) => ({text: error.message}), toast: () => {}, refresh: () => {}
  };` });
  await page.addScriptTag({ path: path.resolve(__dirname, '../../internal/httpx/dashboard/static/pages/knowledge.js') });
  await page.locator('[data-knowledge-upload-input]').setInputFiles({ name: 'test.txt', mimeType: 'text/plain', buffer: Buffer.from('test') });
  await expect(page.locator('#knowledge-documents-error')).toHaveText('Missing permission: write access to this collection');
  await page.evaluate(() => {
    const panel = document.getElementById('knowledge-documents')!;
    panel.innerHTML = '<p id="knowledge-documents-error" hidden></p>';
    document.dispatchEvent(new CustomEvent('htmx:afterSwap', { detail: { target: panel } }));
  });
  await expect(page.locator('#knowledge-documents-error')).toBeVisible();
  await expect(page.locator('#knowledge-documents-error')).toHaveText('Missing permission: write access to this collection');
});
