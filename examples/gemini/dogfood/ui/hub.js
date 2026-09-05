/* Manual QA hub — loads qa-matrix.json, persists checklist in localStorage. */

const cfg = window.DOGFOOD || {};
const CP = (cfg.controlPlaneUrl || "http://127.0.0.1:2026").replace(/\/$/, "");
const STORAGE_KEY = "runkite-dogfood-qa-v1";

const els = {
  checklist: document.getElementById("checklist"),
  backends: document.getElementById("backends"),
  scenarios: document.getElementById("scenarios"),
  progressText: document.getElementById("progress-text"),
  progressFill: document.getElementById("progress-fill"),
  cpUrl: document.getElementById("cp-url"),
  healthDot: document.getElementById("health-dot"),
  healthText: document.getElementById("health-text"),
  quickNav: document.getElementById("quick-nav"),
};

els.cpUrl.textContent = CP;

let matrix = null;
let checked = loadChecked();

function loadChecked() {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY) || "{}");
  } catch {
    return {};
  }
}

function saveChecked() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(checked));
  updateProgress();
}

function updateProgress() {
  if (!matrix) return;
  const total = matrix.features.length;
  const done = matrix.features.filter((f) => checked[f.id]).length;
  els.progressText.textContent = `${done} / ${total} done`;
  els.progressFill.style.width = total ? `${Math.round((done / total) * 100)}%` : "0%";
}

function renderChecklist() {
  els.checklist.innerHTML = "";
  const bySection = new Map();
  for (const f of matrix.features) {
    const sec = f.section || "Other";
    if (!bySection.has(sec)) bySection.set(sec, []);
    bySection.get(sec).push(f);
  }

  for (const [section, items] of bySection) {
    const secEl = document.createElement("section");
    secEl.className = "qa-section";
    secEl.dataset.section = section;

    const head = document.createElement("button");
    head.type = "button";
    head.className = "qa-section-head";
    head.innerHTML = `<span>${section}</span><span class="qa-chevron">▾</span>`;
    head.addEventListener("click", () => secEl.classList.toggle("collapsed"));
    secEl.appendChild(head);

    const body = document.createElement("div");
    body.className = "qa-section-body";

    for (const f of items) {
      const item = document.createElement("article");
      item.className = "qa-item";
      item.dataset.id = f.id;

      const label = document.createElement("label");
      label.className = "qa-check";
      const box = document.createElement("input");
      box.type = "checkbox";
      box.checked = !!checked[f.id];
      box.addEventListener("change", () => {
        checked[f.id] = box.checked;
        saveChecked();
      });
      label.append(box, document.createTextNode(f.title));
      item.appendChild(label);

      const ol = document.createElement("ol");
      ol.className = "qa-steps";
      for (const step of f.steps) {
        const li = document.createElement("li");
        li.textContent = step;
        ol.appendChild(li);
      }
      item.appendChild(ol);
      body.appendChild(item);
    }

    secEl.appendChild(body);
    els.checklist.appendChild(secEl);
  }
  updateProgress();
}

function badge(text, kind) {
  const b = document.createElement("span");
  b.className = `badge badge-${kind}`;
  b.textContent = text;
  return b;
}

function renderBackends() {
  els.backends.innerHTML = "";
  for (const p of matrix.backendProfiles) {
    const card = document.createElement("article");
    card.className = "backend-card";
    if (p.recommended) card.classList.add("recommended");

    const title = document.createElement("h3");
    title.textContent = p.label;
    card.appendChild(title);

    const tags = document.createElement("div");
    tags.className = "tag-row";
    tags.append(
      badge(p.state, "state"),
      badge(p.transport, "transport"),
      badge(`gov: ${p.governance}`, p.governance === "501" ? "warn" : "ok"),
      badge(`finops: ${p.finops}`, "muted"),
    );
    card.appendChild(tags);

    const note = document.createElement("p");
    note.className = "backend-note";
    note.textContent = p.notes;
    card.appendChild(note);

    const cmd = document.createElement("pre");
    cmd.className = "cmd-block";
    cmd.textContent = p.start;
    card.appendChild(cmd);

    const actions = document.createElement("div");
    actions.className = "card-actions";
    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.textContent = "Copy start";
    copyBtn.addEventListener("click", () => navigator.clipboard.writeText(p.start));
    actions.appendChild(copyBtn);

    if (p.cpUrl) {
      const open = document.createElement("a");
      open.href = p.cpUrl + "/admin/";
      open.target = "_blank";
      open.rel = "noreferrer";
      open.textContent = "Admin";
      actions.appendChild(open);
    }
    card.appendChild(actions);
    els.backends.appendChild(card);
  }
}

function renderScenarios() {
  els.scenarios.innerHTML = "";
  for (const s of matrix.conversationScenarios) {
    const card = document.createElement("article");
    card.className = "scenario-card";

    const title = document.createElement("h3");
    title.textContent = s.title;
    card.appendChild(title);

    const meta = document.createElement("p");
    meta.className = "scenario-meta";
    meta.textContent = `${s.agent} · :${s.port}`;
    card.appendChild(meta);

    const ol = document.createElement("ol");
    for (const turn of s.turns) {
      const li = document.createElement("li");
      li.textContent = turn;
      ol.appendChild(li);
    }
    card.appendChild(ol);

    const pass = document.createElement("p");
    pass.className = "pass-criteria";
    pass.innerHTML = `<strong>Pass:</strong> ${s.pass}`;
    card.appendChild(pass);

    const actions = document.createElement("div");
    actions.className = "card-actions";

    const open = document.createElement("a");
    open.href = `http://127.0.0.1:${s.port}/`;
    open.target = "_blank";
    open.rel = "noreferrer";
    open.textContent = "Open chat";
    actions.appendChild(open);

    const copy = document.createElement("button");
    copy.type = "button";
    copy.textContent = "Copy turns";
    copy.addEventListener("click", () => navigator.clipboard.writeText(s.turns.join("\n\n")));
    actions.appendChild(copy);

    card.appendChild(actions);
    els.scenarios.appendChild(card);
  }
}

function renderQuickNav() {
  const links = [
    ["Hub", "/hub.html"],
    ["Chat", "/index.html"],
    ["LangGraph", "http://127.0.0.1:3101/"],
    ["HITL", "http://127.0.0.1:3105/"],
  ];
  for (const [label, href] of links) {
    const a = document.createElement("a");
    a.href = href;
    a.textContent = label;
    if (href.startsWith("/hub")) a.classList.add("active");
    els.quickNav.appendChild(a);
  }
}

async function loadMatrix() {
  const res = await fetch("/qa-matrix.json");
  if (!res.ok) throw new Error(`qa-matrix.json: ${res.status}`);
  matrix = await res.json();
  document.title = `Runkite · ${matrix.title || "Manual QA"}`;
  renderChecklist();
  renderBackends();
  renderScenarios();
}

document.getElementById("expand-all").addEventListener("click", () => {
  document.querySelectorAll(".qa-section").forEach((s) => s.classList.remove("collapsed"));
});
document.getElementById("collapse-all").addEventListener("click", () => {
  document.querySelectorAll(".qa-section").forEach((s) => s.classList.add("collapsed"));
});
document.getElementById("reset-checks").addEventListener("click", () => {
  if (!confirm("Clear all checklist ticks in this browser?")) return;
  checked = {};
  saveChecked();
  renderChecklist();
});

async function ping() {
  try {
    const res = await fetch(`${CP}/health`);
    if (!res.ok) throw new Error(String(res.status));
    els.healthDot.className = "status-dot ok";
    els.healthText.textContent = "control plane healthy";
  } catch {
    els.healthDot.className = "status-dot bad";
    els.healthText.textContent = "control plane unreachable — run ./examples/gemini/dogfood/start.sh";
  }
}

renderQuickNav();
loadMatrix().catch((err) => {
  els.checklist.innerHTML = `<p class="hub-error">${err.message}</p>`;
});
ping();
setInterval(ping, 8000);
