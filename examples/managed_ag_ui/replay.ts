/** Native AG-UI client reconstruction. Callers replace saved UI state only after
 * this returns; it never appends a full replay onto an existing partial message.
 */
import {
  HttpAgent,
  EventType,
  type HttpAgentConfig,
  type Message,
} from "@ag-ui/client";
import { EventSchema } from "@ag-ui/core/schemas";

export async function reconstructManagedAGUI(options: {
  attachUrl: string;
  threadId: string;
  runId: string;
  assistantMessageId: string;
  messages: Message[];
  fetch?: HttpAgentConfig["fetch"];
  headers?: Record<string, string>;
}): Promise<Message[]> {
  const fetcher = options.fetch ?? globalThis.fetch;
  const agent = new HttpAgent({
    threadId: options.threadId,
    url: options.attachUrl,
    headers: options.headers,
    initialMessages: options.messages.filter(
      (message) => message.id !== options.assistantMessageId,
    ),
    fetch: (url, init) =>
      fetcher(url, { ...init, method: "GET", body: undefined }),
  });
  let completed = false;
  let invalid = false;
  let starts = 0;
  let textStarts = 0;
  await agent.runAgent(
    { runId: options.runId },
    {
      onEvent({ event: raw }) {
        const parsed = EventSchema.safeParse(raw);
        if (!parsed.success) {
          invalid = true;
          agent.abortRun();
          return;
        }
        const event = parsed.data;
        switch (event.type) {
          case EventType.RUN_STARTED:
            invalid ||=
              ++starts !== 1 ||
              event.threadId !== options.threadId ||
              event.runId !== options.runId;
            break;
          case EventType.TEXT_MESSAGE_START:
            invalid ||=
              ++textStarts !== 1 ||
              event.messageId !== options.assistantMessageId;
            break;
          case EventType.TEXT_MESSAGE_CONTENT:
          case EventType.TEXT_MESSAGE_END:
            invalid ||= event.messageId !== options.assistantMessageId;
            break;
          case EventType.RUN_FINISHED:
            invalid ||=
              event.threadId !== options.threadId ||
              event.runId !== options.runId;
            break;
          default:
            invalid = true;
        }
        if (invalid) agent.abortRun();
      },
      onRunFinishedEvent({ outcome }) {
        completed = outcome === "success";
      },
    },
  );
  if (invalid || !completed || starts !== 1 || textStarts !== 1)
    throw new Error(
      "AG-UI replay did not confirm the owned response and durable success",
    );
  return agent.messages;
}
