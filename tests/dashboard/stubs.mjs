#!/usr/bin/env node
// stubs.mjs — run-wide deterministic fixtures for `make dashboard-e2e`.
// Dependency-free (node: builtins only); the wrapper scripts/dashboard_e2e.sh
// starts one process per kind BEFORE the app binary, waits for each origin
// file, exports the origins as E2E_* env vars and kills the PIDs at teardown.
//
//   node stubs.mjs inference <originfile>
//   node stubs.mjs mcp <originfile>
//   node stubs.mjs connector <originfile> <cafile>
//
// These are honest in-repo fixtures: they prove the dashboard works against
// deterministic protocol peers, never a live provider/attestation claim.
//
// inference: OpenAI-compatible peer. GET /models at the canonical root feeds
// ensureEnvEndpoint discovery (the runtime transport appends /v1 itself, so
// completions/embeddings live under /v1). Chat is a scripted two-turn
// contract: while tools are offered and no tool message exists, it returns
// one e2e_echo tool call; afterwards it answers with a [1] citation.
// mcp: Streamable-HTTP JSON-RPC server exposing echo-tool (approved by
// global.setup) and pending-tool (left for the approve flow).
// connector: HTTPS GitHub repo-API fixture the real NewGitHubAdapter
// consumes — real /repos/... endpoints with a run-generated test CA written
// to cafile (the wrapper trusts it via SSL_CERT_FILE/NODE_EXTRA_CA_CERTS).

import http from 'node:http';
import https from 'node:https';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

const [kind, originFile, caFile] = process.argv.slice(2);
if (!kind || !originFile) {
  console.error('usage: stubs.mjs <inference|mcp|connector> <originfile> [<cafile>]');
  process.exit(2);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function send(res, status, obj, headers = {}) {
  const body = JSON.stringify(obj);
  res.writeHead(status, { 'Content-Type': 'application/json', ...headers });
  res.end(body);
}

// ── inference ────────────────────────────────────────────────────────────

const STUB_CHAT_MODEL = 'e2e-chat';
const STUB_ALT_MODEL = 'e2e-chat-alt';
const STUB_EMBED_MODEL = 'e2e-embed';
const EMBED_DIM = 64;

function deterministicVector(input) {
  const v = new Array(EMBED_DIM);
  for (let i = 0; i < EMBED_DIM; i++) {
    const h = crypto.createHash('sha256').update(String(input) + ':' + i).digest();
    v[i] = h[0] / 255;
  }
  return v;
}

function chatReply(model, content, extra = {}) {
  return {
    id: 'chatcmpl-e2e-' + crypto.randomBytes(6).toString('hex'),
    object: 'chat.completion',
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, message: { role: 'assistant', content, ...extra }, finish_reason: 'stop' }],
    usage: { prompt_tokens: 12, completion_tokens: 8, total_tokens: 20 },
  };
}

async function inference(req, res) {
  const u = new URL(req.url, 'http://127.0.0.1');
  if (req.method === 'GET' && (u.pathname === '/models' || u.pathname === '/v1/models')) {
    return send(res, 200, {
      object: 'list',
      data: [
        { id: STUB_CHAT_MODEL, object: 'model', created: 0, owned_by: 'e2e' },
        { id: STUB_ALT_MODEL, object: 'model', created: 0, owned_by: 'e2e' },
        { id: STUB_EMBED_MODEL, object: 'model', created: 0, owned_by: 'e2e' },
      ],
    });
  }
  if (req.method === 'POST' && u.pathname === '/v1/chat/completions') {
    const body = JSON.parse(await readBody(req) || '{}');
    const model = body.model || STUB_CHAT_MODEL;
    const messages = Array.isArray(body.messages) ? body.messages : [];
    // "Long-running" user inputs hold the reply open so the suite can
    // exercise mid-run reload/stop paths (the resumed-poll contract).
    const slowUser = messages.some((m) => m && m.role === 'user' && typeof m.content === 'string' && m.content.includes('Long-running'));
    if (slowUser) await new Promise((r) => setTimeout(r, 20_000));
    // Grounding oracle: if the prompt carries a GROUNDING-MARKER sentence
    // (only reachable through retrieved passages), quote it back — the
    // grounding spec uses this to prove context reached the model.
    const text = (m) => typeof m?.content === 'string' ? m.content
      : Array.isArray(m?.content) ? m.content.map((p) => p?.text || '').join(' ') : '';
    const marker = messages.map(text).join('\n').match(/GROUNDING-MARKER-[a-z0-9]+/);
    if (marker) {
      return send(res, 200, chatReply(model, `The handbook's secret marker is ${marker[0]} [1].`));
    }
    const hasToolResult = messages.some((m) => m && m.role === 'tool');
    const tools = Array.isArray(body.tools) ? body.tools : [];
    const echo = tools.find((t) => t && t.function && t.function.name === 'e2e_echo');
    if (echo && !hasToolResult) {
      return send(res, 200, chatReply(model, '', {
        tool_calls: [{
          id: 'call_e2e_echo_1',
          type: 'function',
          function: { name: 'e2e_echo', arguments: JSON.stringify({ text: 'e2e echo call' }) },
        }],
      }));
    }
    return send(res, 200, chatReply(model,
      'The enclave renewal window is the first Monday of each quarter [1].'));
  }
  if (req.method === 'POST' && u.pathname === '/v1/embeddings') {
    const body = JSON.parse(await readBody(req) || '{}');
    const inputs = Array.isArray(body.input) ? body.input : [String(body.input ?? '')];
    return send(res, 200, {
      object: 'list',
      data: inputs.map((input, i) => ({
        object: 'embedding', index: i, embedding: deterministicVector(input),
      })),
      model: body.model || STUB_EMBED_MODEL,
      usage: { prompt_tokens: 4, total_tokens: 4 },
    });
  }
  send(res, 404, { error: { message: 'stub has no ' + u.pathname } });
}

// ── mcp ──────────────────────────────────────────────────────────────────

const MCP_TOOLS = [
  {
    name: 'echo-tool',
    description: 'Echoes its text argument back verbatim.',
    inputSchema: {
      type: 'object',
      properties: { text: { type: 'string' } },
      required: ['text'],
      additionalProperties: false,
    },
    annotations: { title: 'Echo tool', readOnlyHint: true },
  },
  {
    name: 'pending-tool',
    description: 'Left unapproved for the Tools queue flow.',
    inputSchema: {
      type: 'object',
      properties: { note: { type: 'string' } },
      required: ['note'],
      additionalProperties: false,
    },
    annotations: { title: 'Pending tool' },
  },
];

async function mcp(req, res) {
  if (req.method === 'DELETE') return send(res, 200, {});
  if (req.method !== 'POST') {
    res.writeHead(405).end();
    return;
  }
  const body = JSON.parse(await readBody(req) || '{}');
  const reply = (result) => send(res, 200, {
    jsonrpc: '2.0', id: body.id, result,
  }, body.id !== undefined ? { 'Mcp-Session-Id': 'e2e-mcp-session' } : {});
  switch (body.method) {
    case 'initialize':
      return reply({
        protocolVersion: '2025-06-18',
        capabilities: { tools: {} },
        serverInfo: { name: 'e2e-mcp-stub', version: '1.0.0' },
      });
    case 'notifications/initialized':
      res.writeHead(202);
      return res.end();
    case 'tools/list':
      return reply({ tools: MCP_TOOLS });
    case 'tools/call': {
      const name = body.params && body.params.name;
      const args = (body.params && body.params.arguments) || {};
      if (name === 'echo-tool') {
        return reply({ content: [{ type: 'text', text: String(args.text ?? '') }] });
      }
      return send(res, 200, {
        jsonrpc: '2.0', id: body.id,
        result: { content: [{ type: 'text', text: 'unavailable' }], isError: true },
      });
    }
    default:
      return send(res, 200, {
        jsonrpc: '2.0', id: body.id ?? 0,
        error: { code: -32601, message: 'method not found' },
      });
  }
}

// ── connector (HTTPS GitHub repo API) ────────────────────────────────────

const COMMIT_SHA = 'e2e0e2e0e2e0e2e0e2e0e2e0e2e0e2e0e2e0e2e0';
const REPO_FILES = [
  { path: 'docs/handbook.md', content: 'The connector handbook chapter one.' },
  { path: 'docs/runbook.md', content: 'Operational runbook for the e2e source.' },
];

function repoPaths() {
  const entries = REPO_FILES.map((f, i) => ({
    path: f.path,
    mode: '100644',
    type: 'blob',
    sha: crypto.createHash('sha1').update('blob ' + f.path).digest('hex'),
    size: Buffer.byteLength(f.content),
    url: 'unused-' + i,
  }));
  return entries;
}

async function connector(req, res) {
  const u = new URL(req.url, 'https://127.0.0.1');
  const p = decodeURIComponent(u.pathname);
  const prefix = '/repos/e2e/e2e-docs';
  if (req.method !== 'GET' || !p.startsWith(prefix)) {
    return send(res, 404, { message: 'Not Found' });
  }
  const rest = p.slice(prefix.length);
  if (rest === '/commits/HEAD') {
    return send(res, 200, { sha: COMMIT_SHA });
  }
  if (rest.startsWith('/git/ref/heads/')) {
    return send(res, 200, { object: { sha: COMMIT_SHA } });
  }
  if (rest === '/git/trees/' + COMMIT_SHA) {
    return send(res, 200, { sha: COMMIT_SHA, tree: repoPaths(), truncated: false });
  }
  if (rest.startsWith('/contents/')) {
    const filePath = rest.slice('/contents/'.length).replace(/^\/+|\/+$/g, '');
    const file = REPO_FILES.find((f) => f.path === filePath);
    if (!file) return send(res, 404, { message: 'Not Found' });
    return send(res, 200, {
      content: Buffer.from(file.content, 'utf8').toString('base64'),
      encoding: 'base64',
      sha: crypto.createHash('sha1').update('blob ' + file.content).digest('hex'),
      path: file.path,
    });
  }
  if (rest === '/issues' || rest === '/pulls' || rest === '/collaborators' ||
      /^\/(issues|pulls)\/\d+\/comments$/.test(rest)) {
    return send(res, 200, []);
  }
  send(res, 404, { message: 'Not Found' });
}

// ── main ─────────────────────────────────────────────────────────────────

async function main() {
  if (kind === 'inference' || kind === 'mcp') {
    const server = http.createServer((req, res) => {
      (kind === 'inference' ? inference : mcp)(req, res).catch((e) => {
        send(res, 500, { error: String(e) });
      });
    });
    await new Promise((r) => server.listen(0, '127.0.0.1', r));
    fs.writeFileSync(originFile, `http://127.0.0.1:${server.address().port}`);
    return;
  }
  if (kind === 'connector') {
    if (!caFile) {
      console.error('connector requires <cafile>');
      process.exit(2);
    }
    // Throwaway CA + 127.0.0.1 leaf cert, generated per run.
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-connector-ca-'));
    const caKey = path.join(dir, 'ca.key');
    const caCrt = path.join(dir, 'ca.crt');
    const leafKey = path.join(dir, 'leaf.key');
    const leafCsr = path.join(dir, 'leaf.csr');
    const leafCrt = path.join(dir, 'leaf.crt');
    execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes',
      '-keyout', caKey, '-out', caCrt, '-days', '1', '-subj', '/CN=Enclava E2E Test CA']);
    execFileSync('openssl', ['req', '-newkey', 'rsa:2048', '-nodes',
      '-keyout', leafKey, '-out', leafCsr, '-subj', '/CN=127.0.0.1',
      '-addext', 'subjectAltName=IP:127.0.0.1']);
    execFileSync('openssl', ['x509', '-req', '-in', leafCsr, '-CA', caCrt, '-CAkey', caKey,
      '-CAcreateserial', '-out', leafCrt, '-days', '1', '-copy_extensions', 'copy']);
    fs.copyFileSync(caCrt, caFile);
    const server = https.createServer(
      { key: fs.readFileSync(leafKey), cert: fs.readFileSync(leafCrt) },
      (req, res) => connector(req, res).catch((e) => send(res, 500, { error: String(e) })),
    );
    await new Promise((r) => server.listen(0, '127.0.0.1', r));
    fs.writeFileSync(originFile, `https://127.0.0.1:${server.address().port}`);
    return;
  }
  console.error('unknown stub kind ' + kind);
  process.exit(2);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
