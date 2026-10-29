import { createHash } from "node:crypto";
import { jsonSchema, tool, type ToolExecutionOptions } from "ai";
import {
  createGovernedExecute,
  type GovernanceClient,
  type GovernanceDecision,
} from "runkite-governance";
import { NoteStore, type NoteArgs, type NoteJob } from "./state.ts";

const schema = jsonSchema<NoteArgs>({
  type: "object",
  additionalProperties: false,
  properties: {
    title: { type: "string", minLength: 1, maxLength: 200 },
    body: { type: "string", maxLength: 10_000 },
  },
  required: ["title", "body"],
});

function validateNote(input: NoteArgs): NoteArgs {
  if (
    !input ||
    typeof input.title !== "string" ||
    !input.title ||
    input.title.length > 200 ||
    typeof input.body !== "string" ||
    input.body.length > 10_000
  )
    throw new Error("Invalid note arguments");
  return { title: input.title, body: input.body };
}

/** The app owns its note transaction, pending continuation and idempotency. */
export function createNoteTool({
  client,
  store,
  turnId,
  onJob,
}: {
  client: Pick<GovernanceClient, "decide">;
  store: NoteStore;
  turnId: string;
  onJob?: (job: NoteJob) => void;
}) {
  return tool({
    description:
      "Create a local application note, subject to Runkite operator policy.",
    inputSchema: schema,
    execute: (input, options: ToolExecutionOptions<unknown>) => {
      const id = createHash("sha256")
        .update(turnId + ":" + options.toolCallId)
        .digest("hex");
      const record = async (
        args: Readonly<NoteArgs>,
        decision: GovernanceDecision,
      ): Promise<NoteJob> => {
        const job = await store.addDecision(
          {
            id,
            turnId,
            args: { ...args },
            actionId: decision.actionId,
            status: decision.effect === "deny" ? "denied" : "pending",
          },
          decision.effect === "allow",
        );
        onJob?.(job);
        return job;
      };
      const execute = createGovernedExecute<
        NoteArgs,
        NoteJob,
        ToolExecutionOptions<unknown>
      >({
        client,
        identify: () => ({
          agentId: "notes-app",
          connector: "workspace",
          tool: "createNote",
          runId: id,
        }),
        execute: (args) => record(args, { effect: "allow" }),
        onDenied: (args, _options, decision) => record(args, decision),
        // Persist the exact operation and return immediately. A later app
        // request continues it after the operator approves the stored action.
        onPending: (args, _options, decision) => record(args, decision),
      });
      return execute(validateNote(input), options);
    },
  });
}
