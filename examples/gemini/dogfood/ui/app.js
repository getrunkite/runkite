/* Runkite Gemini dogfood playground — talks Agent Protocol to the control plane. */

const AGENTS = [
  { id: "gemini_langgraph", label: "LangGraph", port: 3101, note: "tools + Gemini" },
  { id: "gemini_langchain", label: "LangChain", port: 3102, note: "chain" },
  { id: "gemini_crewai", label: "CrewAI", port: 3103, note: "crew" },
  { id: "gemini_llamaindex", label: "LlamaIndex", port: 3104, note: "chat engine" },
  { id: "approval_agent", label: "HITL", port: 3105, note: "email approve demo (not Gemini chat)" },
  { id: "gemini_autogen", label: "AutoGen", port: 3106, note: "assistant" },
  { id: "gemini_langgraphjs", label: "LangGraph.js", port: 3107, note: "TS runner" },
];

const cfg = window.DOGFOOD || {};
const CP = (cfg.controlPlaneUrl || "http://127.0.0.1:2026").replace(/\/$/, "");
const lockedAgent = cfg.agentId || new URLSearchParams(location.search).get("agent") || "";

const els = {
  nav: document.getElementById("agent-nav"),
  subtitle: document.getElementById("subtitle"),
  chat: document.getElementById("chat"),
  log: document.getElementById("log"),
  form: document.getElementById("form"),
  prompt: document.getElementById("prompt"),
  send: document.getElementById("send"),
  cpUrl: document.getElementById("cp-url"),
  agentId: document.getElementById("agent-id"),
  threadId: document.getElementById("thread-id"),
  runId: document.getElementById("run-id"),
  usage: document.getElementById("usage"),
  hitl: document.getElementById("hitl"),
  hitlText: document.getElementById("hitl-text"),
  approve: document.getElementById("approve"),
  deny: document.getElementById("deny"),
  healthDot: document.getElementById("health-dot"),
  healthText: document.getElementById("health-text"),
};

els.cpUrl.textContent = CP;

let currentAgent = lockedAgent || AGENTS[0].id;
let threadId = null;
let busy = false;
let pendingInterrupt = null;
/** @type {{ text: string; at: string; agent: string }[]} */
let questionLog = [];

function renderQuestionLog() {
  const host = document.getElementById("question-log");
  if (!host) return;
  if (questionLog.length === 0) {
    host.textContent = "—";
    return;
  }
  host.textContent = questionLog.map((q, i) => `${i + 1}. ${q.text}`).join("\n");
}

function copyQuestionLog() {
  const text = questionLog.map((q, i) => `${i + 1}. ${q.text}`).join("\n");
  navigator.clipboard.writeText(text);
  log("question log copied");
}

function runHistoryRecallTest() {
  if (busy || questionLog.length < 2) {
    addMsg("system", "Ask at least two questions on this thread first.");
    return;
  }
  sendPrompt("List every question I asked you in this conversation, in order, quoting my exact wording.");
}

function log(line) {
  const ts = new Date().toISOString().slice(11, 19);
  els.log.textContent += `[${ts}] ${line}\n`;
  els.log.scrollTop = els.log.scrollHeight;
}

function addMsg(role, text) {
  const div = document.createElement("div");
  div.className = `msg ${role}`;
  div.innerHTML = `<span class="role">${role}</span>`;
  div.append(document.createTextNode(text));
  els.chat.appendChild(div);
  els.chat.scrollTop = els.chat.scrollHeight;
}

function setBusy(v) {
  busy = v;
  els.send.disabled = v;
  els.prompt.disabled = v;
}

function renderNav() {
  els.nav.innerHTML = "";
  if (!lockedAgent) {
    const hub = document.createElement("a");
    hub.href = "http://127.0.0.1:3100/";
    hub.textContent = "Hub";
    hub.className = location.port === "3100" || location.port === "" ? "active" : "";
    els.nav.appendChild(hub);
  }
  for (const a of AGENTS) {
    const link = document.createElement("a");
    link.href = lockedAgent ? "#" : `http://127.0.0.1:${a.port}/`;
    link.textContent = a.label;
    if (a.id === currentAgent) link.classList.add("active");
    if (!lockedAgent) {
      link.addEventListener("click", (e) => {
        e.preventDefault();
        currentAgent = a.id;
        threadId = null;
        questionLog = [];
        renderQuestionLog();
        els.threadId.textContent = "—";
        els.agentId.textContent = currentAgent;
        els.subtitle.textContent = `${a.label} · ${a.note}`;
        renderNav();
        addMsg("system", `Switched to ${a.id}. New thread on next send.`);
      });
    } else if (a.id !== lockedAgent) {
      link.href = `http://127.0.0.1:${a.port}/`;
    }
    els.nav.appendChild(link);
  }
  els.agentId.textContent = currentAgent;
  const meta = AGENTS.find((x) => x.id === currentAgent);
  if (meta) els.subtitle.textContent = `${meta.label} · ${meta.note}`;
}

async function ensureThread() {
  if (threadId) return threadId;
  const res = await fetch(`${CP}/threads`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  if (!res.ok) throw new Error(`create thread: ${res.status} ${await res.text()}`);
  const data = await res.json();
  threadId = data.thread_id;
  els.threadId.textContent = threadId;
  log(`thread ${threadId}`);
  return threadId;
}

function messageContent(msg) {
  if (!msg || typeof msg !== "object") return "";
  // Flat Agent Protocol / Python shapes: {role|type, content}
  // LangGraph.js SSE often nests: {type:"ai", data:{content:"..."}}
  const raw = msg.content ?? msg.data?.content;
  if (typeof raw === "string") return raw;
  if (Array.isArray(raw)) {
    return raw
      .map((c) => (typeof c === "string" ? c : c?.text || ""))
      .filter(Boolean)
      .join("\n");
  }
  return raw != null ? String(raw) : "";
}

function messageRole(msg) {
  if (!msg || typeof msg !== "object") return "";
  const r = (msg.role || msg.type || msg.data?.type || "").toString().toLowerCase();
  if (r === "human") return "user";
  if (r === "ai" || r === "aimessage") return "assistant";
  return r;
}

function extractText(data) {
  if (!data || typeof data !== "object") return "";
  if (typeof data.content === "string") return data.content;
  const messages = data.messages;
  if (!Array.isArray(messages) || messages.length === 0) return "";
  // Prefer the last model turn — intermediate "values" snapshots often end
  // on the just-appended user message, which would otherwise render as a
  // fake AI echo in the chat pane (seen with HITL + LangGraph.js).
  for (let i = messages.length - 1; i >= 0; i--) {
    const role = messageRole(messages[i]);
    if (role === "assistant" || role === "ai") {
      const text = messageContent(messages[i]);
      if (text) return text;
    }
  }
  return "";
}

async function streamRun(body) {
  const tid = await ensureThread();
  const res = await fetch(`${CP}/threads/${tid}/runs/stream`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "text/event-stream",
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`run stream: ${res.status} ${await res.text()}`);

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  let lastAi = "";
  let eventName = "message";

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const chunks = buf.split("\n\n");
    buf = chunks.pop() || "";
    for (const chunk of chunks) {
      const lines = chunk.split("\n");
      let dataLine = "";
      for (const line of lines) {
        if (line.startsWith("event:")) eventName = line.slice(6).trim();
        if (line.startsWith("data:")) dataLine += line.slice(5).trim();
      }
      if (!dataLine) continue;
      let payload;
      try {
        payload = JSON.parse(dataLine);
      } catch {
        log(`raw ${eventName}: ${dataLine.slice(0, 200)}`);
        continue;
      }

      // Agent Protocol / LangGraph SSE shapes vary: {event,data} or flat method/data.
      const method = payload.event || payload.method || eventName;
      const data = payload.data !== undefined ? payload.data : payload;
      if (payload.run_id) els.runId.textContent = payload.run_id;
      if (data && data.run_id) els.runId.textContent = data.run_id;

      log(`${method} ${JSON.stringify(data).slice(0, 240)}`);

      if (method === "values" || method === "messages/partial" || method === "messages/complete") {
        const text = extractText(data);
        if (text && text !== lastAi) {
          lastAi = text;
        }
        if (data && data.usage) {
          els.usage.textContent = JSON.stringify(data.usage);
        }
      }
      if (method === "end") {
        if (lastAi) addMsg("ai", lastAi);
        const status = data?.status || "?";
        log(`end status=${status}`);
        if (status === "interrupted") {
          pendingInterrupt = true;
          els.hitl.classList.add("visible");
          els.hitlText.textContent = "Interrupted — approve or deny to resume (checkpointed).";
        }
      }
      if (method === "error") {
        addMsg("system", `Error: ${data?.message || JSON.stringify(data)}`);
      }
      if (method === "input.requested" || method === "lifecycle") {
        if (method === "input.requested" || data?.event === "interrupted") {
          pendingInterrupt = data;
          els.hitl.classList.add("visible");
          els.hitlText.textContent = `HITL: ${JSON.stringify(data?.value || data).slice(0, 160)}`;
        }
      }
    }
  }
  if (lastAi && !els.chat.lastChild?.textContent?.includes(lastAi)) {
    addMsg("ai", lastAi);
  }
}

async function sendPrompt(text) {
  setBusy(true);
  els.hitl.classList.remove("visible");
  pendingInterrupt = null;
  questionLog.push({ text, at: new Date().toISOString(), agent: currentAgent });
  renderQuestionLog();
  addMsg("user", text);
  try {
    await streamRun({
      assistant_id: currentAgent,
      agent_id: currentAgent,
      input: { messages: [{ role: "user", content: text }] },
      stream_mode: ["values", "updates"],
    });
  } catch (err) {
    addMsg("system", String(err.message || err));
    log(String(err));
  } finally {
    setBusy(false);
  }
}

async function resume(response) {
  setBusy(true);
  els.hitl.classList.remove("visible");
  try {
    await streamRun({
      assistant_id: currentAgent,
      agent_id: currentAgent,
      command: { resume: response },
      stream_mode: ["values", "updates"],
    });
  } catch (err) {
    addMsg("system", String(err.message || err));
  } finally {
    setBusy(false);
    pendingInterrupt = null;
  }
}

els.form.addEventListener("submit", (e) => {
  e.preventDefault();
  if (busy) return;
  const text = els.prompt.value.trim();
  if (!text) return;
  els.prompt.value = "";
  sendPrompt(text);
});

els.approve.addEventListener("click", () => resume(true));
els.deny.addEventListener("click", () => resume(false));

async function ping() {
  try {
    const res = await fetch(`${CP}/health`);
    if (!res.ok) throw new Error(String(res.status));
    els.healthDot.className = "status-dot ok";
    els.healthText.textContent = "control plane healthy";
  } catch {
    els.healthDot.className = "status-dot bad";
    els.healthText.textContent = "control plane unreachable — is dogfood start.sh running?";
  }
}

renderNav();
renderQuestionLog();
ping();
setInterval(ping, 8000);

const copyLogBtn = document.getElementById("copy-question-log");
const recallBtn = document.getElementById("history-recall");
if (copyLogBtn) copyLogBtn.addEventListener("click", copyQuestionLog);
if (recallBtn) recallBtn.addEventListener("click", runHistoryRecallTest);

addMsg(
  "system",
  lockedAgent === "approval_agent"
    ? "Locked to approval_agent — HITL demo: any message proposes sending an email to alice@example.com; Approve/Deny resumes the checkpoint. Not a Gemini chat."
    : lockedAgent
      ? `Locked to ${currentAgent}. Ask anything — traffic goes through Runkite, not Gemini directly.`
      : "Hub mode: pick an agent chip, then ask. Watch usage land in the right panel and Admin Spend.",
);
