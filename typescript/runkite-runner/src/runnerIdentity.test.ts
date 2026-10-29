import { test } from "node:test";
import assert from "node:assert/strict";
import { Document } from "@langchain/core/documents";
import { getConnectorSession } from "./connectors.js";
import { callAgent } from "./a2a.js";
import { RunkiteStore } from "./store.js";
import { RunkiteVectorStore } from "./vectorstore.js";
import { runWithBinding } from "./tenantCtx.js";

test("all HTTP helpers preserve concurrent custom-kind worker identities", async () => {
  const original = globalThis.fetch;
  const kinds: string[] = [];
  globalThis.fetch = async (url, init) => {
    const headers = new Headers(init?.headers);
    const kind = headers.get("X-Runner-Kind")!;
    kinds.push(kind);
    assert.equal(headers.get("X-Runner-Token"), kind + "-token");
    return new Response("{}", { status: String(url).includes("/store/items?") ? 404 : 200 });
  };
  try {
    await Promise.all(
      ["custom-a", "custom-b"].map((runnerKind) =>
        runWithBinding(
          {
            tenantId: "tenant",
            runId: "run",
            generation: 3,
            runnerKind,
            runnerToken: runnerKind + "-token",
          },
          async () => {
            const config = { configurable: { run_id: "run", generation: 3 } };
            await getConnectorSession(config, "workspace", { controlPlaneUrl: "http://plane" });
            await callAgent(config, "worker", {}, { wait: false, controlPlaneUrl: "http://plane" });
            await new RunkiteStore({ httpBaseUrl: "http://plane" }).batch([{ namespace: ["n"], key: "k" }]);
            const embeddings = { embedDocuments: async () => [[1]], embedQuery: async () => [1] };
            await new RunkiteVectorStore(embeddings, { namespace: "n", httpBaseUrl: "http://plane" }).addVectors(
              [[1]],
              [new Document({ id: "id", pageContent: "content" })],
            );
          },
        ),
      ),
    );
    assert.equal(kinds.length, 8);
    assert.deepEqual([...new Set(kinds)].sort(), ["custom-a", "custom-b"]);
  } finally {
    globalThis.fetch = original;
  }
});
