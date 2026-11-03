import { test, expect, api, seedData } from './fixtures';

// legacy.extract-flow.spec.ts — the Extract Work page regressions (EXT-I01,
// P-03/UX-M02/UX-H09): the template editor serialized schema-row values as
// DOM properties before innerHTML, so open→save wiped both schemas; and the
// Process card reset the form with no confirmation, no job id, no link.
//
// House rule: a missing app/seed prerequisite FAILS the case naming what is
// absent — a skip never counts as passing.

const BASE = process.env.BASE_URL || 'http://localhost:8080';

interface TemplateBody {
  id: string;
  name: string;
  system_prompt: string;
  user_prompt: string;
  model?: string;
  context_schema?: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
}

test('open an existing template and save unchanged keeps both schemas identical', async ({ page }) => {
  // A purpose-built template with rows the labeled editor must carry: a
  // required output field with a description, and a context hint object.
  const id = `tpl-e2e-roundtrip-${Date.now()}`;
  const created = await api<TemplateBody>('POST', '/api/v1/extract/templates', {
    id,
    name: 'Roundtrip Probe',
    system_prompt: 'Extract fields.',
    user_prompt: 'Return JSON.',
    context_schema: {
      fiscal_year: { type: 'number', description: 'Reported year' },
      region: { type: 'string' },
    },
    output_schema: {
      type: 'object',
      properties: {
        invoice_number: { type: 'string', description: 'Invoice id' },
        amount: { type: 'number' },
      },
      required: ['invoice_number'],
    },
  });
  expect(created.id).toBe(id);

  await page.goto(`${BASE}/dashboard/extract?tab=templates`);
  const edit = page.locator(`[data-action="extract:edit-template"][data-template-id="${id}"]`);
  await expect(edit).toBeVisible({ timeout: 15000 });
  await edit.click();

  const drawer = page.locator('#drawer');
  await expect(drawer.locator('[data-editor-save]')).toBeVisible({ timeout: 10000 });
  // Rows render with their serialized values — the exact controls that used
  // to come back empty after the innerHTML round trip. Property order is not
  // contractual (the stored schema is a map), so assert the name/value sets.
  const outNames = await drawer.locator('[data-editor-schema="output"] [data-row-name]').all();
  const outNameValues = await Promise.all(outNames.map((el) => el.inputValue()));
  expect(outNameValues.sort()).toEqual(['amount', 'invoice_number']);
  const ctxNames = await drawer.locator('[data-editor-schema="context"] [data-row-name]').all();
  const ctxNameValues = await Promise.all(ctxNames.map((el) => el.inputValue()));
  expect(ctxNameValues.sort()).toEqual(['fiscal_year', 'region']);

  await drawer.locator('[data-editor-save]').click();
  await expect(drawer).toBeHidden({ timeout: 10000 });

  const saved = await api<TemplateBody>('GET', `/api/v1/extract/templates/${id}`);
  expect(saved.output_schema).toEqual({
    type: 'object',
    properties: {
      invoice_number: { type: 'string', description: 'Invoice id' },
      amount: { type: 'number' },
    },
    required: ['invoice_number'],
  });
  expect(saved.context_schema).toEqual({
    fiscal_year: { type: 'number', description: 'Reported year' },
    region: { type: 'string' },
  });
});

test('submitting a document shows the queued band with a working View job link', async ({ page }) => {
  const templateID = seedData().extractTemplateID;
  if (!templateID) throw new Error('seed.json lacks extractTemplateID — global.setup seeds tpl-e2e-fields');

  await page.goto(`${BASE}/dashboard/extract?tab=process`);
  await page.locator('#extract-template').selectOption(templateID);
  await page.locator('#extract-file').setInputFiles({
    name: 'e2e-invoice.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('invoice e2e body'),
  });
  await page.locator('[data-action="extract:submit"]').click();

  // The seeded template routes an unpriced model — the preflight warn toast
  // fires first, then the band replaces the form.
  const band = page.locator('[data-extract-band]');
  await expect(band).toBeVisible({ timeout: 15000 });
  await expect(band).toContainText('Extraction queued');
  await expect(band).toContainText('e2e-invoice.txt');
  const strip = band.locator('[data-extract-strip]');
  await expect(strip).toBeVisible();
  const jobID = await strip.getAttribute('data-job-id');
  expect(jobID).toBeTruthy();

  // View job deep-links the Jobs tab and highlights the new row.
  await band.locator('a', { hasText: 'View job' }).click();
  await expect(page.locator(`#extract-jobs tr.job-highlight[data-job-id="${jobID}"]`)).toBeVisible({ timeout: 15000 });

  // Run another restores the form; the strip clears with the band.
  await page.goto(`${BASE}/dashboard/extract?tab=process`);
  await expect(page.locator('[data-extract-band]')).toBeHidden();
});
