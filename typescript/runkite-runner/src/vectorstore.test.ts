import { test } from "node:test";
import assert from "node:assert/strict";
import { RunkiteVectorStore } from "./vectorstore.js";
import type { EmbeddingsInterface } from "@langchain/core/embeddings";
import { Document } from "@langchain/core/documents";

/** Minimal fake embeddings -- one fixed-length vector per text, distinct
 * enough per input to sanity-check ordering without needing a real
 * embedding model. Matches EmbeddingsInterface's shape directly (no
 * need to extend the abstract Embeddings class). */
function fakeEmbeddings(): EmbeddingsInterface {
  return {
    async embedDocuments(texts: string[]): Promise<number[][]> {
      return texts.map((t) => [t.length, 0, 0]);
    },
    async embedQuery(text: string): Promise<number[]> {
      return [text.length, 0, 0];
    },
  };
}

/** Mocks the global fetch for the duration of one test -- same approach
 * as a2a.test.ts, reassigning the plain global rather than adding a
 * mocking dependency. */
function mockFetch(handler: (url: string, init: RequestInit) => { status: number; body: unknown }): {
  calls: Array<{ url: string; init: RequestInit }>;
  restore: () => void;
} {
  const original = globalThis.fetch;
  const calls: Array<{ url: string; init: RequestInit }> = [];
  globalThis.fetch = (async (url: any, init: any) => {
    calls.push({ url: String(url), init });
    const { status, body } = handler(String(url), init);
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return {
    calls,
    restore: () => {
      globalThis.fetch = original;
    },
  };
}

test("RunkiteVectorStore requires httpBaseUrl", () => {
  assert.throws(() => new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "" }), /httpBaseUrl/);
});

test("addDocuments embeds texts and PUTs one upsert per document to /internal/vectors/items", async () => {
  const mock = mockFetch(() => ({ status: 200, body: {} }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    const ids = await store.addDocuments([
      new Document({ pageContent: "hello", metadata: { source: "a" }, id: "doc-1" }),
      new Document({ pageContent: "world!", metadata: { source: "b" }, id: "doc-2" }),
    ]);

    assert.deepEqual(ids, ["doc-1", "doc-2"]);
    assert.equal(mock.calls.length, 2);
    for (const call of mock.calls) assert.equal(call.url, "http://cp:2026/internal/vectors/items");
    const bodies = mock.calls.map((c) => JSON.parse(c.init.body as string));
    assert.equal(bodies[0].namespace, "docs");
    assert.equal(bodies[0].id, "doc-1");
    assert.equal(bodies[0].content, "hello");
    assert.deepEqual(bodies[0].metadata, { source: "a" });
    assert.deepEqual(bodies[0].embedding, [5, 0, 0]); // "hello".length === 5
  } finally {
    mock.restore();
  }
});

test("addVectors upserts each vector/document pair directly, without re-embedding", async () => {
  const mock = mockFetch(() => ({ status: 200, body: {} }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    const ids = await store.addVectors(
      [
        [1, 2, 3],
        [4, 5, 6],
      ],
      [new Document({ pageContent: "hello", id: "doc-1" }), new Document({ pageContent: "world", id: "doc-2" })],
    );
    assert.deepEqual(ids, ["doc-1", "doc-2"]);
    const bodies = mock.calls.map((c) => JSON.parse(c.init.body as string));
    assert.deepEqual(bodies[0].embedding, [1, 2, 3]);
    assert.deepEqual(bodies[1].embedding, [4, 5, 6]);
  } finally {
    mock.restore();
  }
});

test("addVectors throws when vectors.length and documents.length disagree", async () => {
  const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
  await assert.rejects(
    () => store.addVectors([[1, 2, 3]], [new Document({ pageContent: "a" }), new Document({ pageContent: "b" })]),
    /vectors\.length/,
  );
});

test("addVectors throws when options.ids is shorter than documents (fails loudly instead of upserting an undefined id)", async () => {
  const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
  await assert.rejects(
    () =>
      store.addVectors(
        [
          [1, 2, 3],
          [4, 5, 6],
        ],
        [new Document({ pageContent: "a" }), new Document({ pageContent: "b" })],
        { ids: ["only-one-id"] },
      ),
    /options\.ids\.length/,
  );
});

test("addDocuments generates a random id for a document with none", async () => {
  const mock = mockFetch(() => ({ status: 200, body: {} }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    const ids = await store.addDocuments([new Document({ pageContent: "no id here" })]);
    assert.equal(ids.length, 1);
    assert.match(ids[0], /^[0-9a-f-]{36}$/i, "expected a generated UUID");
  } finally {
    mock.restore();
  }
});

test("similaritySearch embeds the query, POSTs to /internal/vectors/search, and maps results back to Documents", async () => {
  const mock = mockFetch((url) => {
    assert.equal(url, "http://cp:2026/internal/vectors/search");
    return {
      status: 200,
      body: {
        results: [
          { item: { id: "doc-1", content: "hello", metadata: { source: "a" } }, score: 0.9 },
          { item: { id: "doc-2", content: "world", metadata: { source: "b" } }, score: 0.5 },
        ],
      },
    };
  });
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    const docs = await store.similaritySearch("a query", 2);

    assert.equal(docs.length, 2);
    assert.equal(docs[0].pageContent, "hello");
    assert.deepEqual(docs[0].metadata, { source: "a" });
    assert.equal(docs[0].id, "doc-1");

    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.equal(body.namespace, "docs");
    assert.equal(body.top_k, 2);
    assert.deepEqual(body.embedding, [7, 0, 0]); // "a query".length === 7
  } finally {
    mock.restore();
  }
});

test("similaritySearchWithScore returns documents paired with their scores, in the order the server returned them", async () => {
  const mock = mockFetch(() => ({
    status: 200,
    body: {
      results: [
        { item: { id: "doc-1", content: "hello" }, score: 0.9 },
        { item: { id: "doc-2", content: "world" }, score: 0.5 },
      ],
    },
  }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    const results = await store.similaritySearchWithScore("query", 2);
    assert.deepEqual(
      results.map(([doc, score]) => [doc.id, score]),
      [
        ["doc-1", 0.9],
        ["doc-2", 0.5],
      ],
    );
  } finally {
    mock.restore();
  }
});

test("similaritySearch forwards a filter to the search request", async () => {
  const mock = mockFetch(() => ({ status: 200, body: { results: [] } }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    await store.similaritySearch("query", 4, { category: "faq" });
    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.deepEqual(body.filter, { category: "faq" });
  } finally {
    mock.restore();
  }
});

test("delete sends one DELETE per id to /internal/vectors/items", async () => {
  const mock = mockFetch(() => ({ status: 200, body: {} }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    await store.delete({ ids: ["doc-1", "doc-2"] });

    assert.equal(mock.calls.length, 2);
    for (const call of mock.calls) {
      assert.equal(call.init.method, "DELETE");
      assert.equal(call.url, "http://cp:2026/internal/vectors/items");
    }
    const bodies = mock.calls.map((c) => JSON.parse(c.init.body as string));
    assert.deepEqual(bodies.map((b) => b.id).sort(), ["doc-1", "doc-2"]);
  } finally {
    mock.restore();
  }
});

test("delete with no ids is a no-op (no fetch call at all)", async () => {
  const mock = mockFetch(() => ({ status: 200, body: {} }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    await store.delete();
    await store.delete({ ids: [] });
    assert.equal(mock.calls.length, 0);
  } finally {
    mock.restore();
  }
});

test("runnerToken sets X-Runner-Kind/X-Runner-Token headers on every request", async () => {
  const mock = mockFetch(() => ({ status: 200, body: { results: [] } }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), {
      namespace: "docs",
      httpBaseUrl: "http://cp:2026",
      runnerToken: "test-token",
    });
    await store.similaritySearch("query", 4);
    const headers = mock.calls[0].init.headers as Record<string, string>;
    assert.equal(headers["X-Runner-Kind"], "typescript-langgraphjs");
    assert.equal(headers["X-Runner-Token"], "test-token");
  } finally {
    mock.restore();
  }
});

test("no runnerToken means no runner auth headers are sent", async () => {
  const mock = mockFetch(() => ({ status: 200, body: { results: [] } }));
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    await store.similaritySearch("query", 4);
    const headers = mock.calls[0].init.headers as Record<string, string>;
    assert.equal("X-Runner-Kind" in headers, false);
    assert.equal("X-Runner-Token" in headers, false);
  } finally {
    mock.restore();
  }
});

test("a non-ok HTTP response throws with the status and body text", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = (async () => new Response("boom", { status: 500 })) as typeof fetch;
  try {
    const store = new RunkiteVectorStore(fakeEmbeddings(), { namespace: "docs", httpBaseUrl: "http://cp:2026" });
    await assert.rejects(() => store.similaritySearch("query", 4), /500/);
  } finally {
    globalThis.fetch = original;
  }
});

test("fromDocuments embeds and adds the given documents directly, then returns a usable store", async () => {
  const mock = mockFetch(() => ({ status: 200, body: {} }));
  try {
    const store = await RunkiteVectorStore.fromDocuments(
      [new Document({ pageContent: "alpha", metadata: { tag: "a" }, id: "doc-1" })],
      fakeEmbeddings(),
      { namespace: "docs", httpBaseUrl: "http://cp:2026" },
    );
    assert.ok(store instanceof RunkiteVectorStore);
    assert.equal(mock.calls.length, 1);
    const body = JSON.parse(mock.calls[0].init.body as string);
    assert.equal(body.id, "doc-1");
    assert.deepEqual(body.metadata, { tag: "a" });
  } finally {
    mock.restore();
  }
});

test("fromTexts embeds and adds each text with its corresponding metadata, then returns a usable store", async () => {
  const mock = mockFetch(() => ({ status: 200, body: {} }));
  try {
    const store = await RunkiteVectorStore.fromTexts(
      ["alpha", "beta"],
      [{ tag: "a" }, { tag: "b" }],
      fakeEmbeddings(),
      { namespace: "docs", httpBaseUrl: "http://cp:2026" },
    );
    assert.ok(store instanceof RunkiteVectorStore);
    assert.equal(mock.calls.length, 2);
    const bodies = mock.calls.map((c) => JSON.parse(c.init.body as string));
    assert.deepEqual(
      bodies.map((b) => b.metadata),
      [{ tag: "a" }, { tag: "b" }],
    );
  } finally {
    mock.restore();
  }
});
