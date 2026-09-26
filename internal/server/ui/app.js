"use strict";

const ui = {
  state: null,          // /api/state
  report: null,         // selected session report
  selectedId: null,     // selected session id
  selectedRun: null,    // selected run index
  liveRun: null,        // run currently executing (from events)
  liveCalls: new Map(), // call id -> call, for the executing run or proxy mode
  expanded: new Set(),  // expanded call ids
  connected: false,
};

const $ = (sel) => document.querySelector(sel);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

async function api(path, opts) {
  const res = await fetch(path, opts);
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.headers.get("content-type")?.includes("json") ? res.json() : res.text();
}

function ago(iso) {
  if (!iso) return "";
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return new Date(iso).toLocaleDateString();
}

function duration(ms) {
  if (ms == null) return "";
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`;
}

function compactArgs(args) {
  if (!args || typeof args !== "object") return "";
  return Object.entries(args)
    .map(([k, v]) => `${k}=${typeof v === "string" && !/\s/.test(v) ? v : JSON.stringify(v)}`)
    .join(" ");
}

function toast(msg) {
  const el = document.createElement("div");
  el.className = "toast";
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 6000);
}

// ---------- data ----------

async function loadState() {
  ui.state = await api("/api/state");
  renderSidebar();
  renderMeta();
}

async function loadSession(id, { keepRun = false } = {}) {
  ui.selectedId = id;
  try {
    ui.report = await api(`/api/sessions/${encodeURIComponent(id)}`);
  } catch {
    ui.report = null;
  }
  if (!keepRun || !ui.selectedRun) {
    const firstBad = ui.report?.runs?.find((r) => r.status !== "passed");
    ui.selectedRun = firstBad?.index ?? ui.report?.runs?.[0]?.index ?? null;
  }
  location.hash = id;
  renderSidebar();
  renderMain();
}

// ---------- sidebar ----------

function renderMeta() {
  const s = ui.state;
  if (!s) return;
  $("#meta").innerHTML = `
    <span><span class="live-dot ${ui.connected ? "" : "off"}"></span>${ui.connected ? "connected" : "reconnecting"}</span>
    <span>mode: ${esc(s.mode)}</span>
    <span class="mono">${esc(s.control)}</span>
    <span>v${esc(s.version)}</span>`;
}

function renderSidebar() {
  const s = ui.state;
  if (!s) return;
  const busy = Boolean(s.active);
  const scen = s.scenarios || [];
  $("#scenarios").innerHTML = scen.length
    ? scen
        .map(
          (sc, i) => `
      <div class="item" data-scenario="${i}">
        <div class="title">${esc(sc.name)}</div>
        <div class="sub mono">${esc(sc.file)}</div>
        ${sc.error ? `<div class="err">${esc(sc.error)}</div>` : `
        <div class="row">
          <label class="runs-label">runs <input class="runs-input" type="number" min="1" max="200" value="${sc.runs}" data-runs="${i}"></label>
          ${busy && s.active.file === sc.file
            ? `<button class="btn" data-cancel>Cancel</button>`
            : `<button class="btn primary" data-run="${i}" ${busy || !sc.agent ? "disabled" : ""} title="${sc.agent ? "" : "no agent configured"}">Run</button>`}
        </div>`}
      </div>`,
        )
        .join("")
    : `<div class="empty-note">No <code>*.fault.yaml</code> files found under ${esc(s.cwd)}</div>`;

  const sessions = s.sessions || [];
  $("#sessions").innerHTML = sessions.length
    ? sessions
        .map((x) => {
          const status = s.active?.id === x.id ? "running" : x.status;
          return `
      <div class="item ${x.id === ui.selectedId ? "selected" : ""}" data-session="${esc(x.id)}">
        <div class="title"><span class="dot ${esc(status)}"></span>${esc(x.scenario)}</div>
        <div class="sub">${esc(ago(x.started))} · ${x.runs}/${x.planned_runs} runs${x.violations ? ` · <span style="color:var(--bad)">${x.violations} violated</span>` : ""}</div>
      </div>`;
        })
        .join("")
    : `<div class="empty-note">No sessions yet.</div>`;
}

document.addEventListener("click", async (e) => {
  const t = e.target;
  if (t.closest("#home")) {
    e.preventDefault();
    ui.selectedId = null;
    ui.report = null;
    location.hash = "";
    renderSidebar();
    renderMain();
    return;
  }
  const runBtn = t.closest("[data-run]");
  if (runBtn) {
    const i = Number(runBtn.dataset.run);
    const sc = ui.state.scenarios[i];
    const runs = Number(document.querySelector(`[data-runs="${i}"]`).value) || sc.runs;
    try {
      await api("/api/runs", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ file: sc.file, runs }) });
      await loadState();
    } catch (err) {
      toast(err.message);
    }
    return;
  }
  if (t.closest("[data-cancel]")) {
    await api("/api/runs/cancel", { method: "POST" });
    return;
  }
  const sess = t.closest("[data-session]");
  if (sess) return loadSession(sess.dataset.session);
  const sq = t.closest("[data-runidx]");
  if (sq) {
    ui.selectedRun = Number(sq.dataset.runidx);
    renderMain();
    return;
  }
  const row = t.closest("tr.call");
  if (row) {
    const id = Number(row.dataset.call);
    ui.expanded.has(id) ? ui.expanded.delete(id) : ui.expanded.add(id);
    renderMain();
  }
});

// ---------- main ----------

function renderMain() {
  const main = $("#main");
  if (ui.state?.mode === "proxy" && !ui.selectedId) {
    main.innerHTML = renderLive();
    return;
  }
  if (!ui.report) {
    main.innerHTML = `
      <div class="empty">
        <h1>Test how your agent recovers when tools fail</h1>
        <p>Pick a scenario on the left and press Run, or run from a terminal. Results show up here live.</p>
        <pre>mcpfault run path/to/scenario.fault.yaml</pre>
      </div>`;
    return;
  }
  const r = ui.report;
  const running = r.status === "running" || ui.state?.active?.id === r.id;
  const status = running ? "running" : r.status;
  const dur = r.finished ? duration(new Date(r.finished) - new Date(r.started)) : "";
  const disconnected = (r.servers || []).filter((s) => !s.connected && !running);

  main.innerHTML = `
    <div class="head">
      <h1>${esc(r.scenario)} <span class="pill ${esc(status)}">${esc(status)}</span></h1>
      ${r.description ? `<p class="desc">${esc(r.description)}</p>` : ""}
      <div class="kv">
        <span>Agent <code>${esc(r.agent)}</code></span>
        <span>Started <b>${esc(new Date(r.started).toLocaleString())}</b></span>
        ${dur ? `<span>Took <b>${dur}</b></span>` : ""}
        <span>Results <code>${esc(r.dir)}</code></span>
      </div>
    </div>
    ${disconnected.map((s) => `<div class="banner">Server <b>${esc(s.name)}</b> never connected through mcpfault. Point your agent at <code>${esc(s.endpoint)}</code></div>`).join("")}
    ${(r.warnings || []).map((w) => `<div class="banner warn">${esc(w)}</div>`).join("")}
    <div class="grid">
      <section class="card">
        <h3>Invariants <small>violations across runs</small></h3>
        ${(r.invariants || []).map(renderInvariant).join("") || `<div class="hint">No invariants defined.</div>`}
      </section>
      <section class="card">
        <h3>Runs <small>${r.runs.length}/${r.planned_runs}</small></h3>
        <div class="runs">${renderRunSquares(r, running)}</div>
        ${r.faults?.length ? `<h3 style="margin-top:18px">Faults</h3><ul class="faults">${r.faults.map((f) => `<li>${esc(f)}</li>`).join("")}</ul>` : ""}
      </section>
    </div>
    ${(r.recovery || []).map(renderRecovery).join("")}
    ${renderContract(r.contract)}
    ${renderRun(r, running)}`;
}

function renderInvariant(inv) {
  const bad = inv.failed > 0;
  const warn = inv.severity === "warn";
  const pct = inv.evaluated ? (100 * inv.failed) / inv.evaluated : 0;
  const cls = bad ? (warn ? "warn" : "bad") : inv.evaluated ? "ok" : "";
  return `
    <div class="inv">
      <span class="mark ${cls}">${bad ? (warn ? "!" : "✗") : inv.evaluated ? "✓" : "–"}</span>
      <span class="name">${esc(inv.name)}${warn ? `<span class="sev">warn</span>` : ""}</span>
      <div class="bar ${warn ? "warn" : ""}"><span style="width:${pct}%"></span></div>
      <span class="rate ${bad ? cls : ""}">${inv.failed}/${inv.evaluated}</span>
    </div>`;
}

function renderRunSquares(r, running) {
  const squares = [];
  for (let i = 1; i <= r.planned_runs; i++) {
    const run = r.runs.find((x) => x.index === i);
    let cls = run ? run.status : "pending";
    if (!run && running && ui.liveRun === i) cls = "running";
    const sel = ui.selectedRun === i ? "selected" : "";
    const clickable = run || cls === "running";
    squares.push(`<div class="run-sq ${cls} ${sel}" ${clickable ? `data-runidx="${i}" style="cursor:pointer"` : ""} title="run ${i}: ${cls}">${i}</div>`);
  }
  return squares.join("");
}

function renderRecovery(rec) {
  const rows = Object.entries(rec.outcomes).sort((a, b) => b[1] - a[1]);
  const blind = rec.outcomes["retried without checking state or reusing a key"] || 0;
  let hint = "";
  if (rec.contract_level === "gap") hint = "The tool has no idempotency key and no lookup tool, so no agent can retry it safely. This is a tool-contract gap: escalating is the best an agent can do until the API changes.";
  else if (blind > 0 && rec.contract_level) hint = "The tool offers a safe path (see Tool contract) and the agent didn't use it. This is an agent bug, not a tool-contract gap.";
  return `
    <section class="card" style="margin-top:16px">
      <h3>After <code>${esc(rec.tool)}</code> ran but the agent wasn't told <small>${rec.total}×</small></h3>
      ${rows.map(([k, n]) => `<div class="recovery-row ${k.startsWith("retried without") || k.startsWith("crashed") ? "risky" : ""}"><span>${esc(k)}</span><b>${n}</b></div>`).join("")}
      ${hint ? `<div class="hint">${esc(hint)}</div>` : ""}
    </section>`;
}

function renderContract(findings) {
  const shown = (findings || []).filter((f) => f.level !== "ok");
  if (!shown.length) return "";
  return `
    <section class="card" style="margin-top:16px">
      <h3>Tool contract <small>heuristic, from tools/list</small></h3>
      ${shown.map((f) => `<div class="finding"><span class="tag ${esc(f.level)}">${esc(f.level)}</span><span><code>${esc(f.server)}/${esc(f.tool)}</code> ${esc(f.message)}</span></div>`).join("")}
    </section>`;
}

function renderRun(r, running) {
  const idx = ui.selectedRun;
  if (!idx) return "";
  const run = r.runs.find((x) => x.index === idx);
  if (!run) {
    if (running && ui.liveRun === idx) {
      const calls = [...ui.liveCalls.values()].filter((c) => c.run === idx).sort((a, b) => a.id - b.id);
      return `<section class="card" style="margin-top:16px"><h3>Run ${idx} <span class="pill running">running</span></h3>${renderCalls(calls)}</section>`;
    }
    return "";
  }
  const verdicts = run.invariants
    .map((v) => `<div class="verdict"><span class="mark ${v.pass ? "ok" : v.severity === "warn" ? "warn" : "bad"}">${v.pass ? "✓" : v.severity === "warn" ? "!" : "✗"}</span> ${esc(v.name)} ${v.detail ? `<span class="d">— ${esc(v.detail)}</span>` : ""}</div>`)
    .join("");
  return `
    <section class="card" style="margin-top:16px">
      <h3>Run ${run.index} <span class="pill ${esc(run.status)}">${esc(run.status)}</span> <small>${duration(run.duration_ms)} · ${run.calls?.length || 0} tool calls</small></h3>
      ${run.error ? `<div class="run-error">${esc(run.error)}</div>` : ""}
      <div class="verdicts">${verdicts}</div>
      ${renderCalls(run.calls || [])}
      <details class="output" ${run.status === "error" ? "open" : ""}>
        <summary>Agent output</summary>
        <pre>${esc(run.output || "(no output)")}</pre>
        ${run.stderr ? `<pre style="margin-top:8px">${esc(run.stderr)}</pre>` : ""}
      </details>
    </section>`;
}

function serverBadge(c) {
  if (!c.forwarded) return `<span class="badge b-notsent">not sent</span>`;
  if (c.committed === true) return `<span class="badge b-executed">executed</span>`;
  if (c.committed === false) return `<span class="badge b-rejected">rejected</span>`;
  if (c.server_outcome?.kind === "transport_error") return `<span class="badge b-transport_error">error</span>`;
  return `<span class="badge b-pending">pending</span>`;
}

function renderCalls(calls) {
  if (!calls.length) return `<div class="hint">No tool calls recorded.</div>`;
  const t0 = new Date(calls[0].start).getTime();
  const rows = calls
    .map((c) => {
      const saw = c.delivered ? `<span class="badge b-${esc(c.delivered.kind)}">${esc(c.delivered.kind)}</span>` : `<span class="badge b-pending">waiting</span>`;
      const fault = c.fault ? `<span class="badge b-fault" title="${esc(c.fault.label)}">${esc(c.fault.type)}</span>` : "";
      const main = `
        <tr class="call ${c.fault ? "faulted" : ""}" data-call="${c.id}">
          <td>${c.seq || c.id}</td>
          <td class="mono">+${duration(new Date(c.start).getTime() - t0)}</td>
          <td><b>${esc(c.server)}</b>/${esc(c.tool)}</td>
          <td class="args" title="${esc(compactArgs(c.args))}">${esc(compactArgs(c.args))}</td>
          <td>${serverBadge(c)}</td>
          <td>${saw}</td>
          <td>${fault}</td>
        </tr>`;
      if (!ui.expanded.has(c.id)) return main;
      return (
        main +
        `<tr class="detail"><td colspan="7"><div class="detail-grid">
          <div><h4>Arguments</h4><pre>${esc(JSON.stringify(c.args, null, 2))}</pre></div>
          <div><h4>What the server returned</h4><pre>${esc(c.result !== undefined ? JSON.stringify(c.result, null, 2) : c.server_outcome?.text || (c.forwarded ? "(pending)" : "(never sent)"))}</pre></div>
          <div><h4>What the agent saw</h4><pre>${esc(c.delivered ? `${c.delivered.kind}${c.delivered.code ? ` (${c.delivered.code})` : ""}\n${c.delivered.text || ""}` : "(nothing yet)")}</pre></div>
          <div><h4>Fault</h4><pre>${esc(c.fault?.label || "none")}</pre></div>
        </div></td></tr>`
      );
    })
    .join("");
  return `
    <table class="calls">
      <colgroup><col style="width:42px"><col style="width:70px"><col style="width:22%"><col><col style="width:92px"><col style="width:104px"><col style="width:112px"></colgroup>
      <thead><tr><th>#</th><th>time</th><th>tool</th><th>arguments</th><th>server</th><th>agent saw</th><th>fault</th></tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
}

function renderLive() {
  const live = ui.state.live || { servers: [], faults: [] };
  const calls = [...ui.liveCalls.values()].sort((a, b) => a.id - b.id);
  return `
    <div class="head">
      <h1>Live proxy <span class="pill running">listening</span></h1>
      <p class="desc">Faults apply to every matching call. Point your agent or MCP client at the endpoints below and watch calls arrive.</p>
    </div>
    <div class="grid">
      <section class="card">
        <h3>Endpoints</h3>
        ${live.servers.map((s) => `<div class="finding"><span class="tag info">${esc(s.transport)}</span><span><b>${esc(s.name)}</b><br><code>${esc(s.endpoint)}</code>${s.upstream ? `<div class="hint">→ ${esc(s.upstream)}</div>` : ""}</span></div>`).join("") || `<div class="hint">No servers.</div>`}
      </section>
      <section class="card">
        <h3>Active faults</h3>
        ${live.faults?.length ? `<ul class="faults">${live.faults.map((f) => `<li>${esc(f)}</li>`).join("")}</ul>` : `<div class="hint">None. Traffic passes through unchanged.</div>`}
      </section>
    </div>
    <section class="card" style="margin-top:16px"><h3>Calls <small>${calls.length}</small></h3>${renderCalls(calls)}</section>`;
}

// ---------- live events ----------

function connectEvents() {
  const es = new EventSource("/api/events");
  es.onopen = () => {
    ui.connected = true;
    renderMeta();
  };
  es.onerror = () => {
    ui.connected = false;
    renderMeta();
  };
  let pending = null;
  const schedule = () => {
    if (pending) return;
    pending = setTimeout(() => {
      pending = null;
      renderMain();
    }, 120);
  };
  es.onmessage = async (msg) => {
    const ev = JSON.parse(msg.data);
    switch (ev.type) {
      case "call":
        ui.liveCalls.set(ev.call.id, ev.call);
        if (ui.liveCalls.size > 2000) ui.liveCalls.delete(ui.liveCalls.keys().next().value);
        schedule();
        break;
      case "session_started":
        ui.liveCalls.clear();
        ui.liveRun = null;
        await loadState();
        await loadSession(ev.data.id);
        break;
      case "run_started":
        ui.liveRun = ev.data.run;
        if (ui.report && (ui.report.status === "running" || ui.state?.active)) ui.selectedRun = ev.data.run;
        schedule();
        break;
      case "run_finished":
        await loadState();
        if (ui.selectedId === ev.data.session) await loadSession(ev.data.session, { keepRun: true });
        break;
      case "session_finished":
      case "idle":
        ui.liveRun = null;
        await loadState();
        if (ui.selectedId) await loadSession(ui.selectedId, { keepRun: true });
        break;
      case "error":
        toast(ev.data.message);
        await loadState();
        break;
    }
  };
}

(async function init() {
  await loadState();
  if (ui.state.mode === "proxy") {
    for (const c of await api("/api/calls")) ui.liveCalls.set(c.id, c);
  }
  const fromHash = decodeURIComponent(location.hash.slice(1));
  const target = fromHash || ui.state.active?.id || (ui.state.mode !== "proxy" ? ui.state.sessions?.[0]?.id : null);
  if (target) await loadSession(target);
  else renderMain();
  connectEvents();
  setInterval(() => renderSidebar(), 30000);
})();
