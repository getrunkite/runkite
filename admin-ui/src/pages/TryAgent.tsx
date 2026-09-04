import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { FlaskConical } from "lucide-react";
import { api, ApiError } from "../api/client";
import { streamSSEPost, type SseEvent } from "../api/sse";
import type { AdminAgent } from "../api/types";
import { EmptyState, ErrorState, PageHeader, supportPage } from "../components/common";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import {
  extractAssistantText,
  extractUsage,
  parseSseData,
  parseSseMethod,
} from "../lib/agentMessages";

type ChatMsg = { role: "user" | "assistant" | "system"; text: string };
type RawLine = { ts: string; event: string; preview: string };

export function TryAgent() {
  const [searchParams] = useSearchParams();
  const [agents, setAgents] = useState<AdminAgent[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [tenantId, setTenantId] = useState("default");
  const [agentId, setAgentId] = useState("");
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);
  const [threadId, setThreadId] = useState<string | null>(null);
  const [runId, setRunId] = useState<string | null>(null);
  const [chat, setChat] = useState<ChatMsg[]>([]);
  const [raw, setRaw] = useState<RawLine[]>([]);
  const [usage, setUsage] = useState<Record<string, unknown> | null>(null);
  const [hitl, setHitl] = useState(false);
  const abortRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const rows = await api.get<AdminAgent[]>("/agents?limit=200");
        if (cancelled) return;
        setAgents(rows ?? []);
        const urlTenant = searchParams.get("tenant_id");
        const urlAgent = searchParams.get("agent_id");
        if (urlTenant) setTenantId(urlTenant);
        if (urlAgent) {
          setAgentId(urlAgent);
        } else if (!agentId && rows?.length) {
          setAgentId(rows[0].agent_id);
          setTenantId(rows[0].tenant_id || "default");
        } else if (urlTenant && !urlAgent && rows?.length) {
          const match = rows.find((a) => a.tenant_id === urlTenant);
          if (match) setAgentId(match.agent_id);
        }
      } catch (e) {
        if (!cancelled) setLoadError(e instanceof ApiError ? e.message : String(e));
      }
    })();
    return () => {
      cancelled = true;
      abortRef.current?.();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const agentOptions = useMemo(() => {
    return agents.map((a) => ({
      value: `${a.tenant_id}\t${a.agent_id}`,
      label: `${a.agent_id} (${a.tenant_id})`,
    }));
  }, [agents]);

  const appendRaw = (event: string, data: unknown) => {
    const preview = typeof data === "string" ? data : JSON.stringify(data).slice(0, 240);
    setRaw((prev) => [
      ...prev.slice(-200),
      { ts: new Date().toISOString().slice(11, 19), event, preview },
    ]);
  };

  const ensureThread = useCallback(async () => {
    if (threadId) return threadId;
    const t = await api.post<{ thread_id: string }>(`/threads?tenant_id=${encodeURIComponent(tenantId)}`, {});
    const id = t.thread_id;
    setThreadId(id);
    return id;
  }, [threadId, tenantId]);

  const runStream = useCallback(
    async (body: Record<string, unknown>) => {
      const tid = await ensureThread();
      setBusy(true);
      setHitl(false);
      let lastAi = "";
      return new Promise<void>((resolve) => {
        abortRef.current?.();
        abortRef.current = streamSSEPost(
          `/threads/${encodeURIComponent(tid)}/runs/stream?tenant_id=${encodeURIComponent(tenantId)}`,
          body,
          (ev: SseEvent) => {
            let payload: Record<string, unknown> = {};
            try {
              payload = JSON.parse(ev.data) as Record<string, unknown>;
            } catch {
              appendRaw(ev.event, ev.data);
              return;
            }
            const method = parseSseMethod(ev.event, payload);
            const data = parseSseData(payload);
            if (payload.run_id) setRunId(String(payload.run_id));
            if (data && typeof data === "object" && "run_id" in data && (data as { run_id?: string }).run_id) {
              setRunId(String((data as { run_id: string }).run_id));
            }
            appendRaw(method || ev.event, data);

            if (method === "values" || method === "messages/partial" || method === "messages/complete") {
              const text = extractAssistantText(data);
              if (text) lastAi = text;
              const u = extractUsage(data);
              if (u) setUsage(u);
            }
            if (method === "end") {
              if (lastAi) {
                // Snapshot into a const: the setChat updater is a closure
                // over `lastAi` and only runs on React's next flush, by
                // which point the `lastAi = ""` reset below would already
                // have landed, pushing an empty bubble instead of the reply.
                const text = lastAi;
                setChat((c) => [...c, { role: "assistant", text }]);
                lastAi = "";
              }
              const status = (data as { status?: string } | null)?.status;
              if (status === "interrupted") setHitl(true);
            }
            if (method === "error") {
              const msg =
                (data as { message?: string } | null)?.message || JSON.stringify(data);
              setChat((c) => [...c, { role: "system", text: `Error: ${msg}` }]);
            }
            if (method === "input.requested" || method === "lifecycle") {
              const d = data as { event?: string } | null;
              if (method === "input.requested" || d?.event === "interrupted") setHitl(true);
            }
          },
          (err) => {
            if (err) setChat((c) => [...c, { role: "system", text: err }]);
            if (lastAi) {
              const text = lastAi;
              setChat((c) => [...c, { role: "assistant", text }]);
            }
            setBusy(false);
            resolve();
          },
        );
      });
    },
    [ensureThread, tenantId],
  );

  const onSend = async () => {
    const text = prompt.trim();
    if (!text || !agentId || busy) return;
    setPrompt("");
    setChat((c) => [...c, { role: "user", text }]);
    try {
      await runStream({
        assistant_id: agentId,
        agent_id: agentId,
        input: { messages: [{ role: "user", content: text }] },
        stream_mode: ["values", "updates"],
      });
    } catch (e) {
      setChat((c) => [...c, { role: "system", text: e instanceof Error ? e.message : String(e) }]);
      setBusy(false);
    }
  };

  const onResume = async (ok: boolean) => {
    if (busy || !agentId) return;
    setChat((c) => [...c, { role: "system", text: ok ? "Resuming (approve)…" : "Resuming (deny)…" }]);
    await runStream({
      assistant_id: agentId,
      agent_id: agentId,
      command: { resume: ok },
      stream_mode: ["values", "updates"],
    });
  };

  const onNewThread = () => {
    abortRef.current?.();
    setThreadId(null);
    setRunId(null);
    setChat([]);
    setRaw([]);
    setUsage(null);
    setHitl(false);
  };

  if (loadError) return <ErrorState message={loadError} />;
  if (agents.length === 0 && !loadError) {
    return (
      <EmptyState
        icon={FlaskConical}
        title="No agents registered"
        message="Start a runner so agents appear here, then try a prompt."
      />
    );
  }

  return (
    <div>
      <PageHeader
        title="Try agent"
        subtitle="Run any registered agent through the real control plane — live protocol and per-turn usage, same path clients use."
        actions={
          <a
            href={supportPage("admin-guide.html#3-try-agent")}
            target="_blank"
            rel="noreferrer"
            className="text-sm font-medium text-primary hover:underline"
          >
            Docs: try agent →
          </a>
        }
      />

      <div className="mb-4 rounded-sm border border-border bg-muted/40 px-4 py-3 text-sm text-muted-foreground">
        <p>
          <strong className="font-medium text-foreground">LLM keys live on the runner, not here.</strong>{" "}
          Try agent is a client — it dispatches to whatever graph your runner loaded. Demo agents
          (echo, react, approval) need no key; real models need{" "}
          <code className="text-xs text-foreground">GOOGLE_API_KEY</code> /{" "}
          <code className="text-xs text-foreground">OPENAI_API_KEY</code> (etc.) in the runner
          process before you start it. The dropdown lists every agent the control plane knows about —
          if you see <code className="text-xs">Graph not found</code>, pick one your runner actually
          loaded or align CP + runner config.{" "}
          <a
            href={supportPage("credentials.html")}
            target="_blank"
            rel="noreferrer"
            className="font-medium text-primary hover:underline"
          >
            Credentials map →
          </a>
        </p>
      </div>

      <div className="mb-4 flex flex-wrap items-end gap-3">
        <label className="text-sm">
          <span className="mb-1 block text-xs text-muted-foreground">Agent</span>
          <select
            className="h-9 min-w-[16rem] rounded-sm border border-border bg-background px-2 font-mono text-xs"
            value={`${tenantId}\t${agentId}`}
            onChange={(e) => {
              const [t, a] = e.target.value.split("\t");
              setTenantId(t || "default");
              setAgentId(a || "");
              onNewThread();
            }}
            disabled={busy}
          >
            {agentOptions.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
        <Button type="button" variant="outline" size="sm" onClick={onNewThread} disabled={busy}>
          New thread
        </Button>
        <div className="text-xs text-muted-foreground">
          thread {threadId ? <code className="text-foreground">{threadId.slice(0, 8)}…</code> : "—"}
          {" · "}
          run{" "}
          {runId ? (
            <Link className="font-mono text-primary hover:underline" to={`/admin/runs/${runId}`}>
              {runId.slice(0, 8)}…
            </Link>
          ) : (
            "—"
          )}
        </div>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <div className="flex min-h-[28rem] flex-col rounded-sm border border-border bg-card">
          <div className="flex-1 space-y-3 overflow-y-auto p-3">
            {chat.length === 0 && (
              <p className="text-sm text-muted-foreground">Send a prompt to exercise this agent end-to-end.</p>
            )}
            {chat.map((m, i) => (
              <div key={i} className="text-sm">
                <p className="mb-0.5 font-mono text-[11px] uppercase tracking-wide text-muted-foreground">
                  {m.role}
                </p>
                <p className="whitespace-pre-wrap text-foreground">{m.text}</p>
              </div>
            ))}
          </div>
          {hitl && (
            <div className="flex items-center gap-2 border-t border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm">
              <span className="text-muted-foreground">Interrupted (HITL)</span>
              <Button type="button" size="sm" onClick={() => void onResume(true)} disabled={busy}>
                Approve
              </Button>
              <Button type="button" size="sm" variant="outline" onClick={() => void onResume(false)} disabled={busy}>
                Deny
              </Button>
            </div>
          )}
          <form
            className="flex gap-2 border-t border-border p-3"
            onSubmit={(e) => {
              e.preventDefault();
              void onSend();
            }}
          >
            <Input
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              placeholder="Message…"
              disabled={busy || !agentId}
            />
            <Button type="submit" disabled={busy || !agentId || !prompt.trim()}>
              {busy ? "Running…" : "Send"}
            </Button>
          </form>
        </div>

        <div className="flex min-h-[28rem] flex-col gap-3">
          <div className="rounded-sm border border-border bg-card px-3 py-2">
            <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Usage (this turn)</p>
            <pre className="mt-1 max-h-24 overflow-auto font-mono text-xs text-foreground">
              {usage ? JSON.stringify(usage, null, 2) : "—"}
            </pre>
            <p className="mt-2 text-xs text-muted-foreground">
              Aggregates: <Link className="text-primary hover:underline" to="/admin/spend">Spend</Link>
            </p>
          </div>
          <div className="flex min-h-0 flex-1 flex-col rounded-sm border border-border bg-card">
            <p className="border-b border-border px-3 py-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
              Live protocol
            </p>
            <pre className="flex-1 overflow-auto p-3 font-mono text-[11px] leading-relaxed text-muted-foreground">
              {raw.length === 0 && "SSE events appear here as the run streams."}
              {raw.map((l, i) => (
                <div key={i}>
                  [{l.ts}] {l.event} {l.preview}
                </div>
              ))}
            </pre>
          </div>
        </div>
      </div>
    </div>
  );
}
