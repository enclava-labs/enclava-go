import { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// access.spec.ts — Access page browser scenarios (09-07, OA-28) against the
// real server: the roles editor's PATCH must carry all four AI-resource grant
// kinds in the canonical stored shape — {"mode":"all"} or
// {"mode":"list","items":[…]} with no "*" sentinels and no {all,items}
// conversion — and the saved grants must round-trip through the roles API;
// the people drawer must assign a role through the bounded
// PUT /api/v1/identities/users/{id}/roles/{roleID} subroute with the
// membership persisted. Seeding happens through the real /api/v1 surface; a
// missing prerequisite FAILS the case naming what is absent — never a skip.
//
// Required env: BASE_URL, ADMIN_EMAIL, ADMIN_PASSWORD (an administrator with
// the seeded Admin role; its own scopes bound the editor's pickers).

const BASE = process.env.BASE_URL || '';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';

test.skip(!BASE || !ADMIN_EMAIL || !ADMIN_PASSWORD,
  'BASE_URL, ADMIN_EMAIL and ADMIN_PASSWORD are required');

test.use({ baseURL: BASE });

async function login(page: Page) {
  await page.context().clearCookies();
  await page.goto('/dashboard/login');
  await page.fill('input[name="email"]', ADMIN_EMAIL);
  await page.fill('input[name="password"]', ADMIN_PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
  // The app revokes a user's older session on a new login — persist the
  // fresh one so the project's stored state never goes stale mid-file.
  await page.context().storageState({ path: 'tests/dashboard/.auth/admin.json' });
}

// api drives the same JSON surface the page module uses, with the browser
// context's session + CSRF cookies.
async function api(page: Page, method: string, path: string, body?: unknown): Promise<unknown> {
  const cookies = await page.context().cookies();
  const csrf = cookies.find((c) => c.name === 'enclava_csrf');
  const res = await page.request.fetch(path, {
    method,
    data: body === undefined ? undefined : JSON.stringify(body),
    headers: {
      'Content-Type': 'application/json',
      Origin: BASE,
      ...(csrf ? { 'X-CSRF-Token': csrf.value } : {}),
    },
  });
  if (!res.ok()) {
    throw new Error(`${method} ${path} failed: ${res.status()} ${await res.text()}`);
  }
  if (res.status() === 204) return {};
  return (await res.json()) as unknown;
}

// jsonID reads the id off a created/stored identity document — the API shape
// is a JSON object whose id is a non-empty string; anything else is a failed
// prerequisite and fails the case naming it.
function jsonID(doc: unknown, what: string): string {
  const d = doc as { id?: unknown } | null;
  if (!d || typeof d.id !== 'string' || d.id === '') {
    throw new Error(`missing prerequisite: ${what} carried no id: ${JSON.stringify(doc)}`);
  }
  return d.id;
}

// assertCanonicalGrant checks one grant document against the canonical stored
// Allowlist wire shape (D-25): mode all with no items, or mode list with a
// string item array — never a "*" sentinel, never an {all, items} pair.
function assertCanonicalGrant(kind: string, doc: unknown) {
  const d = doc as { mode?: unknown; items?: unknown } | null;
  if (!d || typeof d.mode !== 'string') {
    throw new Error(`grants.${kind} has no canonical mode: ${JSON.stringify(doc)}`);
  }
  if (d.mode === 'all') {
    if ('items' in d) throw new Error(`grants.${kind} mode:all carries items: ${JSON.stringify(d)}`);
    return;
  }
  if (d.mode === 'list') {
    if (!Array.isArray(d.items) || d.items.some((i) => typeof i !== 'string')) {
      throw new Error(`grants.${kind} items is not a string array: ${JSON.stringify(d)}`);
    }
    if (d.items.includes('*')) throw new Error(`grants.${kind} carries a "*" sentinel`);
    return;
  }
  throw new Error(`grants.${kind} has neither canonical mode: ${JSON.stringify(d)}`);
}

test.describe('Access roles editor', () => {
  test('PATCH carries all four canonical grant kinds and persists', async ({ page }) => {
    await login(page);

    const roleName = `spec-role-${Date.now()}`;
    const roleID = jsonID(await api(page, 'POST', '/api/v1/identities/roles', {
      name: roleName,
      scopes: ['rag'],
      grants: {
        models: { mode: 'list', items: [] },
        endpoints: { mode: 'list', items: [] },
        assistants: { mode: 'list', items: [] },
        extract_templates: { mode: 'list', items: [] },
      },
    }), 'access spec role create');

    try {
      await page.goto('/dashboard/access/roles');
      const row = page.locator('tr', { hasText: roleName });
      await expect(row).toBeVisible();
      await row.getByRole('button', { name: 'Edit' }).click();

      const form = page.locator('#drawer form[data-form="access-role"]');
      await expect(form).toBeVisible();

      // Edit a grant section for real: check the first grantable assistant
      // item when the picker has any (bounded by the admin's own grants);
      // otherwise the save still proves the wire shape with an explicit
      // empty list.
      const assistantItems = form.locator('fieldset[data-grant="assistants"] [data-grant-item]');
      const itemCount = await assistantItems.count();
      let expectedAssistant: unknown = { mode: 'list', items: [] };
      if (itemCount > 0) {
        await assistantItems.first().check();
        expectedAssistant = { mode: 'list', items: [await assistantItems.first().getAttribute('value')] };
      }

      const patchPromise = page.waitForRequest(
        (r) => r.method() === 'PATCH' && r.url().includes(`/api/v1/identities/roles/${roleID}`),
      );
      await form.getByRole('button', { name: 'Save role' }).click();
      const patch = await patchPromise;
      const body = patch.postDataJSON() as {
        name?: unknown; scopes?: unknown; grants?: Record<string, unknown>;
      };

      // The UI always sends every grant kind explicitly — an omitted field
      // would leave a stored grant unchanged instead of clearing it.
      const grants = body.grants ?? {};
      for (const kind of ['models', 'endpoints', 'assistants', 'extract_templates']) {
        if (!(kind in grants)) throw new Error(`PATCH body omitted grants.${kind}: ${JSON.stringify(body)}`);
        assertCanonicalGrant(kind, grants[kind]);
      }
      expect(grants.assistants).toEqual(expectedAssistant);
      expect(body.name).toBe(roleName);
      expect(Array.isArray(body.scopes)).toBe(true);
      expect(body.scopes as unknown[]).toContain('rag');

      // Persisted outcome: the re-read role carries exactly what was saved.
      const stored = (await api(page, 'GET', `/api/v1/identities/roles/${roleID}`)) as {
        grants?: Record<string, unknown>;
      };
      expect(stored.grants?.assistants).toEqual(expectedAssistant);
      for (const kind of ['models', 'endpoints', 'extract_templates']) {
        assertCanonicalGrant(kind, stored.grants?.[kind]);
      }
    } finally {
      // A membered role refuses deletion; this role has none.
      await api(page, 'DELETE', `/api/v1/identities/roles/${roleID}`);
    }
  });
});

test.describe('Access people roles', () => {
  test('assigning a role emits PUT users/{id}/roles/{roleID} and persists', async ({ page }) => {
    await login(page);

    const roleName = `spec-assign-role-${Date.now()}`;
    const roleID = jsonID(await api(page, 'POST', '/api/v1/identities/roles', {
      name: roleName,
      scopes: ['rag'],
      grants: {
        models: { mode: 'list', items: [] },
        endpoints: { mode: 'list', items: [] },
        assistants: { mode: 'list', items: [] },
        extract_templates: { mode: 'list', items: [] },
      },
    }), 'access spec assign role create');

    const email = `spec-user-${Date.now()}@example.com`;
    const userID = jsonID(await api(page, 'POST', '/api/v1/identities/users', {
      email,
      display_name: 'Access spec user',
      password: 'spec-pass-123',
      role_ids: [],
    }), 'access spec user create');

    try {
      await page.goto('/dashboard/access/people');
      const row = page.locator('tr', { hasText: email });
      await expect(row).toBeVisible();
      await row.getByRole('button', { name: 'Manage' }).click();

      const form = page.locator('#drawer form[data-form="access-user-roles"]');
      await expect(form).toBeVisible();
      await form.locator(`input[name="role"][value="${roleID}"]`).check();

      const putPromise = page.waitForRequest(
        (r) => r.method() === 'PUT' &&
          r.url().includes(`/api/v1/identities/users/${userID}/roles/${roleID}`),
      );
      await form.getByRole('button', { name: 'Save roles' }).click();
      const put = await putPromise;
      expect(put.method()).toBe('PUT');

      // Persisted outcome: the user's roles list carries the assignment.
      const stored = (await api(page, 'GET', `/api/v1/identities/users/${userID}`)) as {
        roles?: Array<{ id?: unknown }>;
      };
      const assigned = (stored.roles ?? []).some((r) => r.id === roleID);
      expect(assigned, `expected role ${roleID} in ${JSON.stringify(stored.roles)}`).toBe(true);
    } finally {
      // Unassign before deleting — a membered role refuses deletion (409).
      await api(page, 'DELETE', `/api/v1/identities/users/${userID}/roles/${roleID}`);
      await api(page, 'DELETE', `/api/v1/identities/roles/${roleID}`);
    }
  });
});
