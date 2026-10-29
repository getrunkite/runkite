import { createServer } from "node:http";
import { timingSafeEqual } from "node:crypto";
import { generateText } from "ai";
import { createOpenAI } from "@ai-sdk/openai";
import { createGovernanceClient } from "runkite-governance";
import { NoteStore, type NoteJob } from "./state.ts";
import { createNoteTool } from "./tools.ts";

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}
const appToken = Buffer.from(required("APP_TOKEN"));
const provider = createOpenAI({ apiKey: required("OPENAI_API_KEY") });
const model = provider(required("MODEL"));
const governance = createGovernanceClient({
  baseUrl: required("RUNKITE_HTTP_URL"),
  apiKey: required("RUNKITE_API_KEY"),
  timeoutMs: 10_000,
});
const store = new NoteStore(process.env.APP_STATE_FILE ?? "data/state.json");

createServer(async (request, response) => {
  const send = (status: number, value: unknown) => {
    response.writeHead(status, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    });
    response.end(JSON.stringify(value));
  };
  const authorization = request.headers.authorization ?? "";
  const supplied = Buffer.from(
    authorization.startsWith("Bearer ") ? authorization.slice(7) : "",
  );
  if (
    supplied.length !== appToken.length ||
    !timingSafeEqual(supplied, appToken)
  ) {
    send(401, { error: "Unauthorized application request" });
    return;
  }
  try {
    const url = new URL(request.url ?? "/", "http://localhost");
    if (request.method === "POST" && url.pathname === "/turns") {
      const turnId = request.headers["idempotency-key"];
      if (
        typeof turnId !== "string" ||
        !/^[A-Za-z0-9_-]{1,100}$/.test(turnId)
      ) {
        send(400, {
          error:
            "Supply an Idempotency-Key using letters, numbers, underscores or hyphens",
        });
        return;
      }
      let bytes = 0;
      const chunks: Buffer[] = [];
      for await (const chunk of request) {
        bytes += chunk.length;
        if (bytes > 32_768) {
          send(413, { error: "Request too large" });
          return;
        }
        chunks.push(chunk);
      }
      const { prompt } = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      if (typeof prompt !== "string" || !prompt || prompt.length > 10_000) {
        send(400, {
          error: "prompt must be a nonempty string of at most 10000 characters",
        });
        return;
      }
      if (!(await store.claimTurn(turnId, prompt))) {
        const state = await store.read();
        send(state.turns[turnId].status === "done" ? 200 : 202, {
          turnId,
          ...state.turns[turnId],
          jobs: Object.values(state.jobs).filter(
            (job) => job.turnId === turnId,
          ),
        });
        return;
      }
      const jobs: NoteJob[] = [];
      const result = await generateText({
        model,
        prompt,
        tools: {
          createNote: createNoteTool({
            client: governance,
            store,
            turnId,
            onJob: (job) => jobs.push(job),
          }),
        },
      });
      const reply = { turnId, text: result.text, jobs };
      await store.finishTurn(turnId, reply);
      send(jobs.some((job) => job.status === "pending") ? 202 : 200, reply);
      return;
    }
    const resume = /^\/actions\/([a-f0-9]{64})\/continue$/.exec(url.pathname);
    if (request.method === "POST" && resume) {
      const job = (await store.read()).jobs[resume[1]];
      if (!job) {
        send(404, { error: "Unknown application job" });
        return;
      }
      if (job.status !== "pending") {
        send(200, job);
        return;
      }
      if (!job.actionId)
        throw new Error("Pending application job has no plane action ID");
      const action = await governance.action(job.actionId);
      if (action.status === "pending") {
        send(202, job);
        return;
      }
      // Execute only the stored arguments. Repeated approved continuations return the stored result.
      send(200, await store.settle(job.id, action.status));
      return;
    }
    if (request.method === "GET" && url.pathname === "/notes") {
      send(200, (await store.read()).notes);
      return;
    }
    send(404, { error: "Not found" });
  } catch (error) {
    send(502, {
      error:
        error instanceof Error ? error.message : "Application request failed",
    });
  }
}).listen(Number(process.env.PORT ?? 8088), "127.0.0.1", () => {
  console.log(
    "Governance AI SDK example listening on loopback port " +
      (process.env.PORT ?? 8088),
  );
});
