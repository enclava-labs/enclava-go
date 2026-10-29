import { test, expect } from '@playwright/test';
import * as fs from 'node:fs';
import {
  BASE, ADMIN_EMAIL, ADMIN_PASSWORD, MANAGER_EMAIL, MEMBER_EMAIL,
  GRANTLESS_EMAIL, MEMBER_PASSWORD, AUTH_DIR, SEED_FILE, Seed,
  api, login, stubOrigins,
} from './fixtures';

// global.setup.ts — the deterministic seed for the whole dashboard-e2e run.
// The stub fixtures were started by scripts/dashboard_e2e.sh BEFORE the app
// (this project only consumes them); every object below is created through
// the real /api/v1 surface with the bootstrap admin key, and each role's
// storage state comes from a REAL /dashboard/login form submit — never an
// injected cookie.
//
// D-25: the seeded Member / Knowledge-manager roles arrive with explicit
// empty grant documents — the setup asserts the deny-by-default empty state
// FIRST, then patches the roles with explicit mode:list allowlists naming
// only the seeded items (never mode:all, never platform.admin).

const CHAT_MODEL = 'e2e-chat';
const ALT_MODEL = 'e2e-chat-alt'; // intentionally unpriced — the Set-price flow owns it
const EMBED_MODEL = 'e2e-embed';
const ENDPOINT_ID = 'openai_compatible';
const MCP_TOOL_MODEL = 'e2e_echo';
async function mcpInitialize(origin: string) {
  const res = await fetch(`${origin}/mcp`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} }),
  });
  if (!res.ok) throw new Error(`MCP stub initialize → ${res.status}`);
  const body = (await res.json()) as { result?: { protocolVersion?: string } };
  if (!body.result?.protocolVersion) {
    throw new Error('MCP stub initialize returned no protocolVersion');
  }
}

async function createCollection(name: string, acl: Record<string, unknown>): Promise<{ id: string; qdrant: string }> {
  const col = await api<{ id: string; qdrant_collection: string }>('POST', '/api/v1/rag/collections', {
    name,
    embedding_model: EMBED_MODEL,
    chunk_size: 200,
    chunk_overlap: 20,
    acl,
  });
  return { id: col.id, qdrant: col.qdrant_collection };
}

async function uploadAndReady(collectionID: string, fileName: string, content: string) {
  const form = new FormData();
  form.append('file', new Blob([Buffer.from(content)], { type: 'text/plain' }), fileName);
  await api('POST', `/api/v1/rag/collections/${collectionID}/documents`, form);
	await expect
		.poll(async () => {
			const list = await api<{ documents?: Array<{ file_name: string; status: string; error_message?: string }> }>(
				'GET', `/api/v1/rag/collections/${collectionID}/documents`);
			const doc = (list.documents || []).find((d) => d.file_name === fileName);
			if (doc && doc.status === 'failed') {
				throw new Error(`${fileName} ingest FAILED: ${doc.error_message || 'no error recorded'}`);
			}
			return doc?.status === 'ready' || false;
		}, { timeout: 90_000, message: `${fileName} never appeared in the collection — the upload or ingest pipeline failed` })
		.toBe(true);
}

test('seed: stubs reachable, data seeded, three sessions minted', async ({ page }) => {
  test.setTimeout(600_000);
  fs.mkdirSync(AUTH_DIR, { recursive: true });

  // ── Stub reachability through the exact endpoints the app hits ──
  const { llm, mcp, connector } = stubOrigins();
  for (const [name, origin] of Object.entries({ E2E_LLM_URL: llm, E2E_MCP_URL: mcp, E2E_CONNECTOR_URL: connector })) {
    if (!origin) throw new Error(`${name} is unset — the wrapper publishes stub origins before Playwright`);
  }
  const models = await fetch(`${llm}/models`);
  if (!models.ok) throw new Error(`inference stub GET /models → ${models.status}`);
  // Idempotent across Playwright's per-invocation reruns: E2E_ADMIN_API_KEY
  // is unique per bootstrap, so a seed.json stamped with the CURRENT key
  // proves the whole object set below already exists on this schema.
  if (fs.existsSync(SEED_FILE)) {
    try {
      const prev = JSON.parse(fs.readFileSync(SEED_FILE, 'utf8')) as Seed & { apiKey?: string };
      if (prev.apiKey === (process.env.E2E_ADMIN_API_KEY || '')) return;
    } catch { /* corrupt seed.json — reseed */ }
  }

  const memberUser = await api<{ id: string }>('POST', '/api/v1/identities/users', {
    email: MEMBER_EMAIL, display_name: 'E2E Member',
    password: MEMBER_PASSWORD, role_ids: ['role_member'],
  });
  await api('POST', '/api/v1/identities/users', {
    email: MANAGER_EMAIL, display_name: 'E2E Manager',
    password: MEMBER_PASSWORD, role_ids: ['role_knowledge_manager'],
  });
  // A fourth user on a role whose grants stay EMPTY for the whole run —
  // the grantless truth cases need one account that is never repaired.
  const grantlessUser = await api<{ id: string }>('POST', '/api/v1/identities/users', {
    email: GRANTLESS_EMAIL, display_name: 'E2E Grantless',
    password: MEMBER_PASSWORD, role_ids: [],
  });
  const grantlessRole = await api<{ id: string }>('POST', '/api/v1/identities/roles', {
    name: 'e2e-grantless',
    scopes: ['models.list', 'chat.completions', 'embeddings.create', 'responses', 'chatbot', 'rag', 'extract'],
    grants: {
      models: { mode: 'list', items: [] },
      endpoints: { mode: 'list', items: [] },
      assistants: { mode: 'list', items: [] },
      extract_templates: { mode: 'list', items: [] },
    },
  });
  await api('PUT', `/api/v1/identities/users/${grantlessUser.id}/roles/${grantlessRole.id}`);

  // Deny-by-default proof (D-25): before ANY grant exists, the member's
  // Chat/Extract surfaces render the truthful grant-needed empty state.
  await login(page, MEMBER_EMAIL, MEMBER_PASSWORD);
  await page.goto(`${BASE}/dashboard/chat`);
  await expect(page.locator('#assistantSelect')).toBeDisabled();
  await expect(page.locator('.thread-head')).toContainText('ask an admin to grant access');
  await page.goto(`${BASE}/dashboard/extract`);
  await expect(page.locator('#content')).toContainText('administrator');
  await page.context().clearCookies();

  // ── Pricing: the env endpoint's chat model priced on every required unit
  //    (model-subject rows so the alt model stays unpriced for the
  //    Set-price flow), embeddings priced BEFORE any upload — the embed
  //    worker reserves on dispatch, and an unpriced unit fails the ingest
  //    job for real ──
  const price = async (subjectID: string, capability: string, unit: string, rate: number) =>
    api('POST', '/api/v1/admin/pricing', {
      subject_type: 'model', subject_id: subjectID, capability,
      unit, rate_micros: rate, unit_quantity: 1_000_000, currency: 'USD',
    });
  await price(CHAT_MODEL, 'chat/completions', 'input', 2_500_000);
  await price(CHAT_MODEL, 'chat/completions', 'output', 5_000_000);
  await price(CHAT_MODEL, 'chat/completions', 'cached_input', 1_250_000);
  await price(EMBED_MODEL, 'embeddings', 'input', 500_000);
  await price(EMBED_MODEL, 'embeddings', 'cached_input', 250_000);

  // ── Collections + documents (Qdrant required; names recorded for the
  //    wrapper's teardown) ──
  const colA = await createCollection('E2E Contracts A', { public: true });
  const colB = await createCollection('E2E Contracts B', { public: true });
  const colSecret = await createCollection('E2E Restricted Annex', { audience: ['actor_outsider'] });
  const dataDir = process.env.ENCLAVA_DATA_DIR || '';
  if (!dataDir) throw new Error('ENCLAVA_DATA_DIR is unset — the wrapper owns teardown bookkeeping');
  fs.writeFileSync(
    `${dataDir}/qdrant-collections.txt`,
    [colA.qdrant, colB.qdrant, colSecret.qdrant].filter(Boolean).join('\n') + '\n',
  );

  await uploadAndReady(colA.id, 'alpha-contract.txt',
    'The contract renewal clause requires notice ninety days before expiry. ' +
    'The enclave renewal window is the first Monday of each quarter.');
  await uploadAndReady(colA.id, 'e2e-renewals.txt',
    'The enclave renewal window is the first Monday of each quarter. ' +
    'Renewals are reviewed by the platform committee.');
  await uploadAndReady(colB.id, 'beta-contract.txt',
    'Contract pricing terms are reviewed each quarter by the committee.');
  await uploadAndReady(colSecret.id, 'secret-contract.txt',
    'Secret contract annex the member must never see.');

  const search = await api<{ results?: Array<{ evidence_id?: string }> }>(
    'POST', '/api/v1/rag/search',
    { query: 'enclave renewal window', mode: 'lexical', collection_ids: [colA.id] });
  const evidenceID = (search.results || []).map((r) => r.evidence_id).find((v): v is string => typeof v === 'string');
  if (!evidenceID) {
    throw new Error('seeded search returned no evidence_id — the evidence contract is unmet');
  }

  // ── Assistants ──
  const bot = await api<{ id: string }>('POST', '/api/v1/chatbots', {
    name: 'E2E Reviewer',
    provider_id: ENDPOINT_ID,
    model: CHAT_MODEL,
    rag_collection_ids: [colA.id],
    settings: { tools: [MCP_TOOL_MODEL] },
    audience: ['*'],
  });
  const bot2 = await api<{ id: string }>('POST', '/api/v1/chatbots', {
    name: 'E2E Notes',
    provider_id: ENDPOINT_ID,
    model: CHAT_MODEL,
    rag_collection_ids: [colB.id],
    audience: ['*'],
  });
  const failingBot = await api<{ id: string }>('POST', '/api/v1/chatbots', {
    name: 'E2E Unpriced',
    provider_id: ENDPOINT_ID,
    model: ALT_MODEL,
    rag_collection_ids: [],
    audience: ['*'],
  });

  // ── Extraction template bound to the deliberately unpriced model — the
  //    202→worker-failure cases rely on the price reserve, never a mock ──
  const tpl = await api<{ id: string }>('POST', '/api/v1/extract/templates', {
    id: 'tpl-e2e-fields',
    name: 'E2E Fields',
    system_prompt: 'Extract fields.',
    user_prompt: 'Return JSON.',
    model: ALT_MODEL,
    is_active: true,
    output_schema: { type: 'object' },
  });

  // ── MCP server against the run-wide stub: discover, approve echo-tool,
  //    leave pending-tool pending ──
  const srv = await api<{ id: string }>('POST', '/api/v1/mcp/servers', {
    id: 'e2e-mcp',
    origin: mcp,
    endpoint_path: '/mcp',
    authorized_private: true,
    enabled: true,
    audience_scopes: ['chatbot'],
  });
  const discovered = await api<{ tools: Array<{ tool_id: string; model_name: string; remote_name: string; state: string; schema_version: number }> }>(
    'POST', `/api/v1/mcp/servers/${srv.id}/discovery`);
  const echoRow = discovered.tools.find((t) => t.remote_name === 'echo-tool');
  const pendingRow = discovered.tools.find((t) => t.remote_name === 'pending-tool');
  if (!echoRow || !pendingRow) {
    throw new Error(`MCP discovery missed a stub tool: ${JSON.stringify(discovered.tools)}`);
  }
  await api('PUT', `/api/v1/mcp/servers/${srv.id}/tools/${echoRow.tool_id}`, {
    model_name: MCP_TOOL_MODEL,
    caller_scopes: ['chatbot'],
    schema_version: echoRow.schema_version,
  });


  // ── D-25 grants: explicit mode:list allowlists naming only the seeded
  //    items — never mode:all, never platform.admin ──
  const grantDoc = (items: string[]) => ({ mode: 'list', items });
  const allItems = {
    models: grantDoc([CHAT_MODEL, ALT_MODEL, EMBED_MODEL]),
    endpoints: grantDoc([ENDPOINT_ID]),
    assistants: grantDoc([bot.id, bot2.id, failingBot.id]),
    extract_templates: grantDoc([tpl.id]),
  };
  for (const roleID of ['role_member', 'role_knowledge_manager']) {
    await api('PATCH', `/api/v1/identities/roles/${roleID}`, { grants: allItems });
  }

  // ── Storage states: one REAL login per role ──
  for (const [file, email] of [
    ['admin.json', ADMIN_EMAIL],
    ['manager.json', MANAGER_EMAIL],
    ['member.json', MEMBER_EMAIL],
  ] as const) {
    const password = email === ADMIN_EMAIL ? ADMIN_PASSWORD : MEMBER_PASSWORD;
    await login(page, email, password);
    await page.context().storageState({ path: `${AUTH_DIR}/${file}` });
    await page.context().clearCookies();
  }

  const seed: Seed = {
    endpointID: ENDPOINT_ID,
    chatModel: CHAT_MODEL,
    altModel: ALT_MODEL,
    embedModel: EMBED_MODEL,
    collectionID: colA.id,
    collectionBID: colB.id,
    secretCollectionID: colSecret.id,
    evidenceID,
    assistantID: bot.id,
    assistant2ID: bot2.id,
    failingAssistantID: failingBot.id,
    extractTemplateID: tpl.id,
    mcpServerID: srv.id,
    memberRoleID: 'role_member',
    managerRoleID: 'role_knowledge_manager',
    memberUserID: memberUser.id,
  };
  fs.writeFileSync(SEED_FILE, JSON.stringify({ ...seed, apiKey: process.env.E2E_ADMIN_API_KEY || '' }, null, 2));
});
