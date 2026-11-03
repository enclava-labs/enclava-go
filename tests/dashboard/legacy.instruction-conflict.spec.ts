import { test, expect, seedData, sessionApi } from './fixtures';

test('instruction deletion shows the conflict inline', async ({ page }) => {
  const seed = seedData();
  const suffix = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  const instructionName = `Conflict instructions ${suffix}`;
  const assistantName = `Conflict assistant ${suffix}`;
  let templateID = '';
  let assistantID = '';

  try {
    const templateResponse = await sessionApi(page, 'POST', '/api/v1/prompt-templates', {
      name: instructionName,
      user_prompt: '{{.user_input}}',
      is_active: true,
    });
    expect(templateResponse.status(), await templateResponse.text()).toBe(201);
    templateID = (await templateResponse.json()).id;

    const assistantResponse = await sessionApi(page, 'POST', '/api/v1/chatbots', {
      name: assistantName,
      external_id: `conflict-${suffix}`,
      model: seed.chatModel,
      prompt_template_id: templateID,
      audience: ['*'],
    });
    expect(assistantResponse.status(), await assistantResponse.text()).toBe(201);
    assistantID = (await assistantResponse.json()).id;

    await page.goto('/dashboard/assistants/instructions');
    const card = page.locator('#instructions-list .card').filter({ hasText: instructionName });
    const button = card.locator('[data-template-delete]');
    await expect(button).toBeVisible();
    await button.click();
    await expect(page.locator('.confirm-group .confirm-text')).toContainText(`Delete instructions “${instructionName}”?`);
    const response = page.waitForResponse(r => r.request().method() === 'DELETE' && r.url().includes(`/prompt-templates/${templateID}`));
    await page.locator('.confirm-group [data-action="confirm-commit"]').click();
    expect((await response).status()).toBe(409);
    await expect(card.locator('[data-template-delete-error]')).toContainText(assistantName);
  } finally {
    if (assistantID) {
      const deleted = await sessionApi(page, 'DELETE', `/api/v1/chatbots/${assistantID}`);
      expect(deleted.ok(), await deleted.text()).toBeTruthy();
    }
    if (templateID) {
      const deleted = await sessionApi(page, 'DELETE', `/api/v1/prompt-templates/${templateID}`);
      expect(deleted.ok(), await deleted.text()).toBeTruthy();
    }
  }
});
