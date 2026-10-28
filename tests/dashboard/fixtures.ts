import { expect, Page } from '@playwright/test';
import * as fs from 'node:fs';
import * as path from 'node:path';

// fixtures.ts — shared helpers for the dashboard-e2e suite. The run-wide
// stub servers are OWNED by scripts/dashboard_e2e.sh (started before the
// app, killed at teardown); this module only consumes them through the
// E2E_* env vars — no in-process servers anywhere in the suite.

export const BASE = process.env.BASE_URL || '';
export const ADMIN_EMAIL = process.env.ADMIN_EMAIL || '';
export const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || '';
export const MANAGER_EMAIL = 'manager@e2e.local';
export const MEMBER_EMAIL = 'member@e2e.local';
export const GRANTLESS_EMAIL = 'grantless@e2e.local';
export const MEMBER_PASSWORD = 'E2e-pass-1234!';
export const SESSION_COOKIE = 'enclava_session';
export const CSRF_COOKIE = 'enclava_csrf';
export const AUTH_DIR = path.join(__dirname, '.auth');
export const SEED_FILE = path.join(AUTH_DIR, 'seed.json');

export interface Seed {
  endpointID: string;
  chatModel: string;
  altModel: string;
  embedModel: string;
  collectionID: string;
  collectionBID: string;
  secretCollectionID: string;
  evidenceID: string;
  assistantID: string;
  assistant2ID: string;
  failingAssistantID: string;
  extractTemplateID: string;
  mcpServerID: string;
  memberRoleID: string;
  managerRoleID: string;
  memberUserID: string;
}

let seedCache: Seed | null = null;

// seedData reads the deterministic object set global.setup.ts persisted —
// the suite never re-derives names or ids.
export function seedData(): Seed {
  if (seedCache) return seedCache;
  if (!fs.existsSync(SEED_FILE)) {
    throw new Error(
      `seed.json is missing at ${SEED_FILE} — run the suite through ` +
      'scripts/dashboard_e2e.sh so the setup project seeds the tenant first',
    );
  }
  seedCache = JSON.parse(fs.readFileSync(SEED_FILE, 'utf8')) as Seed;
  return seedCache;
}

export function stubOrigins() {
  return {
    llm: process.env.E2E_LLM_URL || '',
    mcp: process.env.E2E_MCP_URL || '',
    connector: process.env.E2E_CONNECTOR_URL || '',
  };
}

// api is the admin-API helper: Bearer bootstrap key, never a session cookie.
export async function api<T = unknown>(method: string, path: string, body?: unknown | FormData): Promise<T> {
  const key = process.env.E2E_ADMIN_API_KEY || '';
  if (!key) {
    throw new Error('E2E_ADMIN_API_KEY is unset — the wrapper exports the bootstrap key');
  }
  const isForm = typeof FormData !== 'undefined' && body instanceof FormData;
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${key}`,
      ...(isForm ? {} : { 'Content-Type': 'application/json' }),
    },
    body: body === undefined ? undefined : isForm ? (body as FormData) : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) {
    throw new Error(`${method} ${path} → ${res.status}: ${text.slice(0, 400)}`);
  }
  if (res.status === 204 || text === '') return undefined as T;
  return JSON.parse(text) as T;
}

// apiRaw is the same surface without the ok-check, for denial assertions.
export async function apiRaw(method: string, path: string, body?: unknown) {
  const key = process.env.E2E_ADMIN_API_KEY || '';
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${key}`,
      'Content-Type': 'application/json',
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return { status: res.status, body: await res.text() };
}

// sessionApi calls /api/v1 through the page's real browser session: context
// cookies plus the enclava_csrf echo app.js sends on mutations (D-17).
export async function sessionApi(page: Page, method: string, url: string, body?: unknown) {
  const csrf = (await page.context().cookies()).find((c) => c.name === CSRF_COOKIE);
  const res = await page.request.fetch(`${BASE}${url}`, {
    method,
    headers: {
      'X-CSRF-Token': csrf ? csrf.value : '',
      'Content-Type': 'application/json',
    },
    data: body === undefined ? undefined : JSON.stringify(body),
  });
  return res;
}

export async function login(page: Page, email: string, password: string) {
  await page.goto(`${BASE}/dashboard/login`);
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', password);
  await page.click('button[type="submit"]');
  await page.waitForURL('**/dashboard');
}

// loginAndSaveState performs a real form login and persists the storage
// state — setup uses it to mint each role's .auth/<role>.json.
export async function loginAndSaveState(page: Page, email: string, password: string, file: string) {
  await login(page, email, password);
  const session = (await page.context().cookies()).find((c) => c.name === SESSION_COOKIE);
  if (!session) throw new Error(`login for ${email} produced no ${SESSION_COOKIE} cookie`);
  await page.context().storageState({ path: file });
}

export async function sessionCookies(page: Page) {
  const cookies = await page.context().cookies();
  return {
    session: cookies.find((c) => c.name === SESSION_COOKIE),
    csrf: cookies.find((c) => c.name === CSRF_COOKIE),
  };
}

// awaitTerminal polls the chat thread until every live runline is gone —
// the terminal render carries Stopped/failed vocabulary, never a timeout.
export async function awaitTerminal(page: Page, timeout = 60_000) {
  await expect
    .poll(() => page.locator('.thread-body .runline:not(.muted)').count(), {
      timeout,
      intervals: [500],
    })
    .toBe(0);
}

export async function fieldString(res: { json(): Promise<unknown> }, field: string): Promise<string> {
  const value: unknown = await res.json();
  if (typeof value === 'object' && value !== null && field in value) {
    const v: unknown = (value as Record<string, unknown>)[field];
    if (typeof v === 'string') return v;
  }
  throw new Error(`response is missing string field "${field}": ${JSON.stringify(value)?.slice(0, 200)}`);
}

// uploadDoc uploads a text file through the multipart API and waits for the
// document to reach ready (the 08-08 ingest pipeline under the worker).
export async function uploadDoc(collectionID: string, name: string, content: string) {
  const form = new FormData();
  form.append('file', new Blob([Buffer.from(content)], { type: 'text/plain' }), name);
  await api('POST', `/api/v1/rag/collections/${collectionID}/documents`, form);
  await expect
    .poll(
      async () => {
        const list = await api<{ documents?: Array<{ file_name: string; status: string }> }>(
          'GET', `/api/v1/rag/collections/${collectionID}/documents?status=ready`);
        return (list.documents || []).some((d) => d.file_name === name);
      },
      { timeout: 60_000, message: `document ${name} never reached ready — the ingest pipeline failed` },
    )
    .toBe(true);
}
