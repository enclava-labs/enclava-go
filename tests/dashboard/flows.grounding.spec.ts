import { test, expect } from '@playwright/test';
import { api, seedData, uploadDoc } from './fixtures';

// flows.grounding.spec.ts — the whole knowledge pipeline over the public
// API, no UI: upload a book to a collection, confirm the ingest worker
// actually embedded it (qdrant points), ground a fresh assistant on that
// collection, ask it about the book, and prove the answer could only come
// from the retrieved passages (the stub echoes a GROUNDING-MARKER sentence
// it can only have seen in the prompt) with the run citing evidence.

const MARKER = 'GROUNDING-MARKER-7f3k9';

interface Collection { id: string }
interface Chatbot { id: string }
interface DocList {
  documents: Array<{ file_name: string; status: string; qdrant_point_count: number }>;
}
interface Run {
  id: string; conversation_id?: string; status: string; output?: string;
  evidence_ids?: string[]; error?: string;
}

test('book → embeddings → grounded assistant answer, end to end over the API', async () => {
  const seed = seedData();
  const suffix = Date.now();

  // 1. A knowledge collection with a "book" whose marker sentence only
  //    exists in the uploaded text.
  const col = await api<Collection>('POST', '/api/v1/rag/collections', {
    name: `grounding handbook ${suffix}`,
  });
  const book = [
    '# Operations handbook',
    '',
    '## Renewals',
  '',
    `The renewal window opens on the first Monday of each quarter. The secret marker for this handbook is ${MARKER}.`,
    '',
    '## Escalation',
    '',
    'Escalations route to the on-call enclave operator after fifteen minutes.',
  ].join('\n');
  await uploadDoc(col.id, `handbook-${suffix}.md`, book);

  // 2. Embeddings were really generated, not just parsed.
  const docs = await api<DocList>('GET', `/api/v1/rag/collections/${col.id}/documents`);
  const doc = (docs.documents || []).find((d) => d.file_name.includes('handbook'));
  expect(doc?.status).toBe('ready');
  expect((doc?.qdrant_point_count ?? 0)).toBeGreaterThan(0);

  // 3. An assistant grounded on exactly that collection.
  const bot = await api<Chatbot>('POST', '/api/v1/chatbots', {
    name: `grounded ${suffix}`,
    model: seed.chatModel,
    rag_collection_ids: [col.id],
    audience: ['*'],
  });

  // 4. Ask it; the marker is never in the question, only in the book.
  const run = await api<Run>('POST', '/api/v1/agent-runs', {
    assistant_id: bot.id,
    input: 'What is the secret marker in the handbook?',
  });
  await expect
    .poll(
      async () => {
        const r = await api<Run>('GET', `/api/v1/agent-runs/${run.id}`);
        return r.status;
      },
      { timeout: 60_000, message: 'grounded run never reached a terminal status' },
    )
    .toBe('succeeded');

  const finished = await api<Run>('GET', `/api/v1/agent-runs/${run.id}`);
  // The answer quotes the marker: the retrieved passage reached the model.
  expect(finished.output || '').toContain(MARKER);

  // The run cites the passage it answered from.
  expect((finished.evidence_ids || []).length).toBeGreaterThan(0);

  // The suite tenant is shared within a run; later specs assert exact
  // chatbot/grant lists, so remove what this test created. Chatbot delete
  // cascades the run's conversation (migration 031).
  await api('DELETE', `/api/v1/chatbots/${bot.id}`);
  await api('DELETE', `/api/v1/rag/collections/${col.id}`);
});
