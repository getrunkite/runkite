import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { randomUUID } from "node:crypto";

export type NoteArgs = { title: string; body: string };
export type NoteJob = {
  id: string;
  turnId: string;
  args: NoteArgs;
  actionId?: string;
  status: "pending" | "denied" | "done";
  result?: { noteId: string };
};
type State = {
  version: 1;
  turns: Record<
    string,
    { prompt: string; status: "processing" | "done"; reply?: unknown }
  >;
  jobs: Record<string, NoteJob>;
  notes: Record<string, NoteArgs>;
};

/** Single-process example store. All note + execution-record changes share one atomic rename. */
export class NoteStore {
  private writes: Promise<unknown> = Promise.resolve();
  private path: string;
  constructor(path: string) {
    this.path = path;
  }

  async read(): Promise<State> {
    try {
      const value: State = JSON.parse(await readFile(this.path, "utf8"));
      if (value.version !== 1)
        throw new Error("Unsupported application state version");
      return value;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      return { version: 1, turns: {}, jobs: {}, notes: {} };
    }
  }

  private change<T>(mutate: (state: State) => T): Promise<T> {
    const next = this.writes.then(async () => {
      const state = await this.read();
      const result = mutate(state);
      await mkdir(dirname(this.path), { recursive: true });
      const temporary = this.path + "." + randomUUID() + ".tmp";
      await writeFile(temporary, JSON.stringify(state), { mode: 0o600 });
      await rename(temporary, this.path);
      return result;
    });
    this.writes = next.catch(() => {});
    return next;
  }

  claimTurn(id: string, prompt: string): Promise<boolean> {
    return this.change((state) => {
      const existing = Object.hasOwn(state.turns, id)
        ? state.turns[id]
        : undefined;
      if (existing) {
        if (existing.prompt !== prompt)
          throw new Error("Idempotency-Key already used for another prompt");
        return false;
      }
      Object.defineProperty(state.turns, id, {
        value: { prompt, status: "processing" },
        enumerable: true,
        writable: true,
        configurable: true,
      });
      return true;
    });
  }

  finishTurn(id: string, reply: unknown): Promise<void> {
    return this.change((state) => {
      state.turns[id].status = "done";
      state.turns[id].reply = reply;
    });
  }

  addDecision(job: NoteJob, allow: boolean): Promise<NoteJob> {
    return this.change((state) => {
      if (state.jobs[job.id]) return state.jobs[job.id];
      state.jobs[job.id] = job;
      return allow ? this.apply(state, job) : job;
    });
  }

  settle(
    id: string,
    status: "approved" | "rejected" | "consumed",
  ): Promise<NoteJob> {
    return this.change((state) => {
      const job = state.jobs[id];
      if (!job) throw new Error("Unknown job");
      if (job.status !== "pending") return job;
      if (status === "approved") return this.apply(state, job);
      job.status = "denied";
      return job;
    });
  }

  private apply(state: State, job: NoteJob): NoteJob {
    const noteId = job.id;
    state.notes[noteId] = job.args;
    job.status = "done";
    job.result = { noteId };
    return job;
  }
}
