// Harness Control Center UI.

import { dashboardView } from "./dashboard.js";
import { localPanel } from "./local.js";
import { credentialsView } from "./credentials.js";
//
// Everything shown here may come from an agent (job errors, console
// output, file names), so the DOM is only ever built with textContent and
// attributes, never by parsing HTML. A test enforces that.

const view = document.getElementById("view");
const dialog = document.getElementById("dialog");
const notice = document.getElementById("notice");
const approvalsBox = document.getElementById("approvals");

const TEMPLATE = `# A new harness. Everything not written here is denied:
# no network, no credentials, no subagents, 30 minutes, $1.
apiVersion: hcc/v1alpha1
kind: Harness
metadata:
  name: my-harness
  description: What this agent should do.
spec:
  runtime: claude-code
  prompt: Write a haiku about sandboxes to haiku.txt.
  sandbox:
    image: claude-code
`;

const TERMINAL = new Set(["succeeded", "failed", "timed_out", "cancelled", "over_budget"]);

// --- API ---

// The session comes in the URL fragment of the login redirect (never sent
// to a server) and stays in sessionStorage, which only this origin (this
// port) can read. It is sent in a header; there is no cookie.
const SESSION_KEY = "hcc-session";
let session = "";

function loadSession() {
  if (location.hash.startsWith("#session=")) {
    session = location.hash.slice("#session=".length);
    history.replaceState(null, "", "/#/dashboard");
    try {
      sessionStorage.setItem(SESSION_KEY, session);
    } catch {
      // Private mode: the session lasts as long as this page.
    }
    return;
  }
  try {
    session = sessionStorage.getItem(SESSION_KEY) || "";
  } catch {
    session = "";
  }
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-HCC-Session": session }, credentials: "omit" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  let data = null;
  try {
    data = await resp.json();
  } catch {
    data = null;
  }
  if (!resp.ok) {
    const err = new Error((data && data.error) || resp.statusText);
    err.status = resp.status;
    err.data = data;
    throw err;
  }
  return data;
}

// --- DOM helpers (no HTML parsing anywhere) ---

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "onclick" || k === "onsubmit" || k === "oninput" || k === "onchange") {
      el.addEventListener(k.slice(2), v);
    } else if (k === "class") {
      el.className = v;
    } else if (v === true) {
      el.setAttribute(k, "");
    } else {
      el.setAttribute(k, String(v));
    }
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

// set replaces el's children; false, null and undefined are skipped (for
// optional parts written as "cond && node").
function set(el, ...children) {
  el.replaceChildren();
  append(el, children);
}

function show(...children) {
  set(view, ...children);
}

function badge(text, kind) {
  return h("span", { class: "badge " + (kind || text) }, text.replace("_", " "));
}

function say(message, isError) {
  notice.textContent = message;
  notice.className = isError ? "error" : "";
  notice.hidden = !message;
  if (message && !isError) {
    setTimeout(() => {
      if (notice.textContent === message) notice.hidden = true;
    }, 5000);
  }
}

function fail(err) {
  if (err && err.status === 401) {
    show(h("div", { class: "empty" },
      h("h1", {}, "Not signed in"),
      h("p", {}, "Open the link that \"hcc control serve\" printed in your terminal.")));
    return;
  }
  say(err && err.message ? err.message : String(err), true);
}

function fmtTime(s) {
  if (!s) return "–";
  const d = new Date(s);
  return isNaN(d) ? "–" : d.toLocaleString();
}

function fmtDuration(from, to) {
  if (!from) return "–";
  const ms = (to ? new Date(to) : new Date()) - new Date(from);
  if (!(ms >= 0)) return "–";
  const s = Math.round(ms / 1000);
  if (s < 60) return s + " s";
  const m = Math.floor(s / 60);
  if (m < 60) return m + " min " + (s % 60) + " s";
  return Math.floor(m / 60) + " h " + (m % 60) + " min";
}

function fmtUsage(u) {
  if (!u) return "–";
  let out = (u.inputTokens + u.outputTokens).toLocaleString() + " tokens";
  if (u.costKnown) out += " ($" + u.costUSD.toFixed(4) + ")";
  return out;
}

function plural(n, word) {
  return n.toLocaleString() + " " + word + (n === 1 ? "" : "s");
}

function fmtBytes(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KiB";
  return (n / 1024 / 1024).toFixed(1) + " MiB";
}

// --- routing ---

let refresh = null; // the current view's timer

function route() {
  if (refresh) {
    clearInterval(refresh);
    refresh = null;
  }
  say("");
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  for (const a of document.querySelectorAll("nav a")) {
    a.classList.toggle("active", a.dataset.nav === (parts[0] || "dashboard"));
  }
  const routes = {
    dashboard: () => dashboardView({ h, api, set, show, every, fail, say, badge, fmtTime, fmtDuration, localPanel }),
    jobs: () => jobsView(),
    job: () => jobView(parts[1], parts[2]),
    harnesses: () => harnessesView(),
    harness: () => harnessView(parts[1]),
    new: () => harnessView(""),
    runners: () => runnersView(),
    credentials: () => credentialsView({ h, api, set, show, fail, say }),
    schedules: () => schedulesView(),
  };
  (routes[parts[0]] || routes.dashboard)();
  view.focus();
}

function every(ms, fn) {
  refresh = setInterval(() => {
    if (!document.hidden) fn();
  }, ms);
}

// --- jobs ---

async function jobsView() {
  const body = h("tbody");
  const errors = h("div");
  show(
    h("div", { class: "head" },
      h("div", {}, h("h1", {}, "Jobs"), h("p", { class: "muted" }, "Every run on every runner, newest first.")),
      h("div", { class: "actions" }, h("a", { class: "button primary", href: "#/harnesses" }, "Start a harness"))),
    errors,
    h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, ["State", "Harness", "Runner", "Started", "Duration", "Usage", "Results", "Detail"].map((t) => h("th", {}, t)))),
      body)));
  const load = async () => {
    try {
      const data = await api("GET", "/api/jobs");
      set(errors, ...Object.entries(data.errors).map(([r, e]) =>
        h("div", { class: "error-box" }, "Runner " + r + " is unreachable: " + e)));
      if (data.jobs.length === 0) {
        set(body, h("tr", {}, h("td", { colspan: 8, class: "muted" }, "No jobs yet.")));
        return;
      }
      set(body, ...data.jobs.map((j) => {
        const href = "#/job/" + encodeURIComponent(j.runner) + "/" + encodeURIComponent(j.id);
        return h("tr", { class: "link", onclick: () => { location.hash = href; } },
          h("td", {}, badge(j.state)),
          h("td", {}, h("a", { href }, j.harness), j.parent && h("div", { class: "muted" }, "helper of " + j.parent)),
          h("td", {}, j.runner),
          h("td", {}, fmtTime(j.started || j.created)),
          h("td", {}, fmtDuration(j.started, j.finished)),
          h("td", {}, fmtUsage(j.usage)),
          h("td", {}, j.results && j.results.files > 0 ? plural(j.results.files, "file") : "–"),
          h("td", { class: "detail" }, j.error || ""));
      }));
    } catch (err) {
      fail(err);
    }
  };
  await load();
  every(3000, load);
}

async function jobView(runner, id) {
  const head = h("div", { class: "head" });
  const facts = h("dl", { class: "facts" });
  const results = h("div");
  const helpers = h("div");
  const consoleText = h("pre", { class: "console" }, "Loading…");
  show(head, h("div", { class: "card" }, facts), helpers, results, h("h2", {}, "Console"),
    h("p", { class: "muted" }, "What the agent printed. Control characters are shown visibly."), consoleText);
  const jobPath = "/api/runners/" + encodeURIComponent(runner) + "/jobs/" + encodeURIComponent(id);

  const load = async () => {
    let j;
    try {
      j = await api("GET", jobPath);
    } catch (err) {
      fail(err);
      return;
    }
    const done = TERMINAL.has(j.state);
    set(head,
      h("div", {}, h("h1", {}, j.harness, " ", badge(j.state)), h("p", { class: "muted" }, "Job " + j.id + " on " + runner)),
      h("div", { class: "actions" },
        !done && h("button", { class: "danger", onclick: () => cancelJob(jobPath) }, "Stop this job"),
        h("a", { class: "button", href: "#/jobs" }, "All jobs")));
    set(facts,
      h("dt", {}, "Created"), h("dd", {}, fmtTime(j.created)),
      h("dt", {}, "Started"), h("dd", {}, fmtTime(j.started)),
      h("dt", {}, "Finished"), h("dd", {}, fmtTime(j.finished)),
      h("dt", {}, "Duration"), h("dd", {}, fmtDuration(j.started, j.finished)),
      h("dt", {}, "Model usage"), h("dd", {}, j.usage
        ? j.usage.inputTokens.toLocaleString() + " in / " + j.usage.outputTokens.toLocaleString() + " out" +
          (j.usage.costKnown ? " ($" + j.usage.costUSD.toFixed(4) + ")" : "") +
          (j.parent ? " (the whole group of agents: they share one budget)" : "")
        : "–"),
      j.schedule && h("dt", {}, "Started by"), j.schedule && h("dd", {}, h("a", { href: "#/schedules" }, "schedule " + j.schedule)),
      j.parent && h("dt", {}, "Started by"), j.parent && h("dd", {}, "the agent of ",
        h("a", { href: "#/job/" + encodeURIComponent(runner) + "/" + encodeURIComponent(j.parent) }, "job " + j.parent),
        " (a helper: at most its permissions, and it stops when that job ends)"),
      j.loopDone && h("dt", {}, "Loop"), j.loopDone && h("dd", {}, "This run reported the task complete."),
      j.error && h("dt", {}, "Why it ended"), j.error && h("dd", {}, j.error));
    set(results, ...resultsBlock(j, jobPath));
    try {
      const all = await api("GET", "/api/jobs");
      const kids = all.jobs.filter((k) => k.runner === runner && k.parent === j.id);
      set(helpers, ...(kids.length === 0 ? [] : [
        h("h2", {}, "Helper agents it started"),
        h("ul", {}, kids.map((k) => h("li", {},
          h("a", { href: "#/job/" + encodeURIComponent(runner) + "/" + encodeURIComponent(k.id) }, k.harness + " (" + k.id + ")"),
          " ", badge(k.state))))]));
    } catch (err) {
      set(helpers);
    }
    try {
      const c = await api("GET", jobPath + "/console");
      const atBottom = consoleText.scrollTop + consoleText.clientHeight >= consoleText.scrollHeight - 8;
      consoleText.textContent = c.text || "(nothing yet)";
      if (atBottom) consoleText.scrollTop = consoleText.scrollHeight;
    } catch (err) {
      consoleText.textContent = "Console unavailable: " + err.message;
    }
    if (done && refresh) {
      clearInterval(refresh);
      refresh = null;
    }
  };
  await load();
  every(2000, load);
}

function resultsBlock(j, jobPath) {
  const r = j.results;
  if (!r) {
    return TERMINAL.has(j.state) ? [h("h2", {}, "Results"), h("p", { class: "muted" }, "This job handed nothing back.")] : [];
  }
  const out = [h("h2", {}, "Results")];
  if (r.files > 0) {
    out.push(h("p", {}, plural(r.files, "file") + ", " + fmtBytes(r.bytes) + ". ",
      h("button", { onclick: () => download(jobPath) }, "Download (.tar)")),
    h("p", { class: "muted" }, "Written by the agent: review it like any download before running anything in it."));
  } else {
    out.push(h("p", { class: "muted" }, "No files."));
  }
  if (r.error) out.push(h("div", { class: "error-box" }, "Problem: " + r.error));
  if (r.skipped && r.skipped.length) {
    out.push(h("p", { class: "muted" }, "Left out (only plain files and folders with safe names are kept):"),
      h("ul", {}, r.skipped.map((s) => h("li", {}, h("code", {}, s)))));
  }
  return out;
}

// download fetches a single-use ticket (a link cannot carry the session
// header) and lets the browser download from it.
async function download(jobPath) {
  try {
    const r = await api("POST", jobPath + "/download");
    location.assign(r.url);
  } catch (err) {
    fail(err);
  }
}

async function cancelJob(jobPath) {
  try {
    await api("POST", jobPath + "/cancel");
    say("Stopping the job…");
  } catch (err) {
    fail(err);
  }
}

// --- harnesses ---

async function harnessesView() {
  const list = h("div", { class: "grid" });
  show(
    h("div", { class: "head" },
      h("div", {}, h("h1", {}, "Harnesses"),
        h("p", { class: "muted" }, "A harness is a task plus exactly what the agent may do. Saved on this computer.")),
      h("div", { class: "actions" }, h("a", { class: "button primary", href: "#/new" }, "New harness"))),
    list);
  try {
    const entries = await api("GET", "/api/harnesses");
    if (entries.length === 0) {
      list.replaceWith(h("div", { class: "empty" }, "No harnesses yet. ", h("a", { href: "#/new" }, "Create your first one.")));
      return;
    }
    set(list, ...entries.map((e) =>
      h("a", { class: "card", href: "#/harness/" + encodeURIComponent(e.name) },
        h("h3", {}, e.name, " ", e.risk ? badge(e.risk + " risk", e.risk) : badge("invalid", "failed")),
        h("p", { class: "muted" }, e.error || e.description || ""),
        h("p", { class: "muted" }, [e.runtime, e.target].filter(Boolean).join(" · ")))));
  } catch (err) {
    fail(err);
  }
}

async function harnessView(name) {
  let doc = TEMPLATE;
  if (name) {
    try {
      doc = (await api("GET", "/api/harnesses/" + encodeURIComponent(name))).yaml;
    } catch (err) {
      fail(err);
      return;
    }
  }
  const text = h("textarea", { class: "yaml", spellcheck: "false", "aria-label": "Harness (YAML)" });
  text.value = doc;
  const panel = h("div", { class: "card" });
  const runBtn = h("button", { class: "primary", onclick: () => runDialog(text.value) }, "Run…");
  const saveBtn = h("button", { onclick: () => save() }, "Save");
  const del = name && h("button", { onclick: () => remove() }, "Delete");
  show(
    h("div", { class: "head" },
      h("div", {}, h("h1", {}, name || "New harness"),
        h("p", { class: "muted" }, "Edit the harness on the left; the right side says in plain words what it may do.")),
      h("div", { class: "actions" }, saveBtn, runBtn, del, h("a", { class: "button", href: "#/harnesses" }, "Back"))),
    h("div", { class: "editor" }, text, panel));

  let timer = null;
  let seq = 0;
  const check = async () => {
    const mine = ++seq;
    try {
      const c = await api("POST", "/api/check", { yaml: text.value });
      if (mine !== seq) return;
      set(panel, ...checkBlock(c));
      runBtn.disabled = !c.valid;
      saveBtn.disabled = !c.valid;
    } catch (err) {
      fail(err);
    }
  };
  text.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(check, 300);
  });
  await check();

  async function save() {
    try {
      const r = await api("PUT", "/api/harnesses", { yaml: text.value });
      say("Saved " + r.name + ".");
      if (r.name !== name) location.hash = "#/harness/" + encodeURIComponent(r.name);
    } catch (err) {
      fail(err);
    }
  }
  async function remove() {
    if (!confirm("Delete the harness " + name + "? Jobs that already ran are kept.")) return;
    try {
      await api("DELETE", "/api/harnesses/" + encodeURIComponent(name));
      location.hash = "#/harnesses";
    } catch (err) {
      fail(err);
    }
  }
}

function checkBlock(c) {
  if (!c.valid) {
    return [h("h2", {}, "Not valid yet"), h("ul", { class: "explain errors" }, (c.errors || []).map((e) => h("li", {}, e)))];
  }
  return [
    h("h2", {}, "What it may do"),
    h("p", {}, "Risk: ", badge(c.risk, c.risk)),
    (c.reasons || []).length > 0 && h("ul", { class: "explain" }, c.reasons.map((r) => h("li", {}, "Because it " + r))),
    h("ul", { class: "explain" }, c.explain.map((l) => h("li", {}, l))),
  ];
}

// runDialog shows exactly what will start and needs an explicit
// confirmation; the server checks the same document again.
async function runDialog(doc) {
  let c;
  let runners;
  try {
    [c, runners] = await Promise.all([api("POST", "/api/check", { yaml: doc }), api("GET", "/api/runners")]);
  } catch (err) {
    fail(err);
    return;
  }
  if (!c.valid) {
    say("Fix the harness first.", true);
    return;
  }
  const usable = runners.filter((r) => r.status);
  const select = h("select", { "aria-label": "Runner" }, usable.map((r) =>
    h("option", { value: r.name, selected: r.status.target === c.target },
      r.name + " (" + r.status.target + (r.status.killed ? ", stopped" : "") + ")")));
  const ok = h("input", { type: "checkbox" });
  const start = h("button", { class: "primary", disabled: true }, c.trigger ? "Add schedule" : "Start");
  const mismatch = h("div");
  const updateMismatch = () => {
    const r = usable.find((x) => x.name === select.value);
    set(mismatch, r && r.status.target !== c.target
      ? h("div", { class: "warn-box" }, "This harness is meant for \"" + c.target + "\", but this runner is \"" + r.status.target + "\". It will refuse it.")
      : "");
  };
  select.addEventListener("change", updateMismatch);
  ok.addEventListener("change", () => { start.disabled = !ok.checked || usable.length === 0; });
  start.addEventListener("click", async () => {
    start.disabled = true;
    try {
      const r = await api("POST", "/api/runs", { runner: select.value, yaml: doc, confirmed: true });
      dialog.close();
      if (r.schedule) {
        say("Added schedule " + r.schedule + " on " + r.runner + ".");
        location.hash = "#/schedules";
      } else {
        location.hash = "#/job/" + encodeURIComponent(r.runner) + "/" + encodeURIComponent(r.id);
      }
    } catch (err) {
      start.disabled = false;
      set(mismatch, h("div", { class: "error-box" }, err.message));
    }
  });
  set(dialog,
    h("h1", {}, (c.trigger ? "Schedule " : "Start ") + c.name + "?"),
    c.trigger && h("div", { class: "warn-box" }, c.trigger === "schedule"
      ? "This adds a schedule: the runner starts it again and again on its own, without asking you, until you pause or delete it."
      : "This adds a loop: the runner starts it several times in a row on its own, without asking you."),
    ...checkBlock(c),
    usable.length === 0
      ? h("div", { class: "error-box" }, "No runner is reachable. ", h("a", { href: "#/runners", onclick: () => dialog.close() }, "Check your runners."))
      : h("label", {}, "Runner", select),
    mismatch,
    h("label", { class: "check" }, ok, c.trigger
      ? "I have read what this harness may do, and I want it to run unattended."
      : "I have read what this harness may do, and I want to start it."),
    h("div", { class: "actions" }, start, h("button", { onclick: () => dialog.close() }, "Cancel")));
  updateMismatch();
  dialog.showModal();
}

// --- schedules ---

async function schedulesView() {
  const body = h("tbody");
  const errors = h("div");
  show(
    h("div", { class: "head" },
      h("div", {}, h("h1", {}, "Schedules"),
        h("p", { class: "muted" }, "Harnesses with a trigger run on their own. Times are UTC on the runner; shown here in your time.")),
      h("div", { class: "actions" }, h("a", { class: "button primary", href: "#/harnesses" }, "Add one from a harness"))),
    errors,
    h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, ["State", "Harness", "Runner", "When", "Runs", "Next run", "Last run", "Detail", ""].map((t) => h("th", {}, t)))),
      body)));
  const load = async () => {
    try {
      const data = await api("GET", "/api/schedules");
      set(errors, ...Object.entries(data.errors).map(([r, e]) => h("div", { class: "error-box" }, "Runner " + r + " is unreachable: " + e)));
      if (data.schedules.length === 0) {
        set(body, h("tr", {}, h("td", { colspan: 9, class: "muted" }, "No schedules. A harness with spec.trigger becomes one when you start it.")));
        return;
      }
      set(body, ...data.schedules.map(scheduleRow));
    } catch (err) {
      fail(err);
    }
  };
  await load();
  every(5000, load);
}

function scheduleRow(s) {
  const path = "/api/runners/" + encodeURIComponent(s.runner) + "/schedules/" + encodeURIComponent(s.id);
  const act = (method, what, question) => async () => {
    if (question && !confirm(question)) return;
    try {
      await api(method, what ? path + "/" + what : path);
      schedulesView();
    } catch (err) {
      fail(err);
    }
  };
  const when = s.cron
    ? h("span", {}, h("code", {}, s.cron), " UTC")
    : "loop, at most " + s.maxRuns + " runs" + (s.interval ? ", " + Math.round(s.interval / 6e10) + " min apart" : "");
  const kind = { active: "running", paused: "cancelled", finished: "queued" }[s.state] || s.state;
  return h("tr", {},
    h("td", {}, badge(s.state, kind)),
    h("td", {}, s.harness),
    h("td", {}, s.runner),
    h("td", {}, when),
    h("td", {}, String(s.runs)),
    h("td", {}, s.nextRun ? fmtTime(s.nextRun) : "–"),
    h("td", {}, s.lastJob ? h("a", { href: "#/job/" + encodeURIComponent(s.runner) + "/" + encodeURIComponent(s.lastJob) }, "open") : "–"),
    h("td", { class: "detail" }, s.message || ""),
    h("td", {}, h("div", { class: "actions" },
      s.state === "active" && h("button", { onclick: act("POST", "pause") }, "Pause"),
      s.state === "paused" && h("button", { onclick: act("POST", "resume") }, "Resume"),
      h("button", { onclick: act("DELETE", "", "Delete this schedule? Its past jobs are kept; a running one goes on.") }, "Delete"))));
}

// --- runners ---

// runnerFields returns the inputs for a runner entry, filled from ref, and
// a function that reads them back.
function runnerFields(ref) {
  const name = h("input", { required: true, placeholder: "hcc-dev", pattern: "[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?" });
  const kind = h("select", {}, h("option", { value: "ssh" }, "over ssh"), h("option", { value: "socket" }, "on this computer"));
  // Over ssh, a host from your ssh configuration, or any other you type.
  const other = ""; // no ssh alias is empty
  const host = h("select", {}, ...sshHosts.map((v) => h("option", { value: v }, v)), h("option", { value: other }, "Other host…"));
  const where = h("input", { autocomplete: "off" });
  const sync = () => {
    const ssh = kind.value === "ssh";
    host.hidden = !ssh || sshHosts.length === 0;
    where.hidden = ssh && !host.hidden && host.value !== other;
    where.required = !where.hidden;
    where.placeholder = ssh ? "ssh host, e.g. user@10.0.0.5" : "/run/hcc/runner.sock";
  };
  kind.addEventListener("change", () => {
    where.value = kind.value === "socket" ? "/run/hcc/runner.sock" : "";
    sync();
  });
  host.addEventListener("change", () => {
    sync();
    if (!where.hidden) where.focus();
  });
  if (ref) {
    name.value = ref.name;
    kind.value = ref.socket ? "socket" : "ssh";
    where.value = ref.socket || ref.ssh;
    host.value = sshHosts.includes(ref.ssh) ? ref.ssh : other;
  } else if (sshHosts.length > 0) {
    host.value = sshHosts[0];
  }
  sync();
  const read = () => {
    const out = { name: name.value };
    out[kind.value] = kind.value === "ssh" && !host.hidden && host.value !== other ? host.value : where.value;
    // Kept as it was: the UI does not show it.
    if (kind.value === "ssh" && ref && ref.remoteSocket) out.remoteSocket = ref.remoteSocket;
    return out;
  };
  return { labels: [h("label", {}, "Name", name), h("label", {}, "Reached", kind), h("label", {}, "Where", host, where)], read };
}

// Host aliases from ~/.ssh/config, loaded with the runners page.
let sshHosts = [];

async function runnersView() {
  try {
    sshHosts = (await api("GET", "/api/ssh-hosts")).hosts;
  } catch {
    sshHosts = []; // only suggestions
  }
  const list = h("div", { class: "grid wide" });
  const fields = runnerFields();
  const form = h("form", { class: "inline", onsubmit: async (ev) => {
    ev.preventDefault();
    const ref = fields.read();
    try {
      await api("POST", "/api/runners", ref);
      say("Added runner " + ref.name + ".");
      runnersView();
    } catch (err) {
      fail(err);
    }
  } }, ...fields.labels, h("button", { class: "primary" }, "Add runner"));
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Runners"),
      h("p", { class: "muted" }, "Machines that run harnesses. Runners enforce every limit themselves; this list only says how to reach them."))),
    localPanel({ h, api, set, fail, say }),
    list,
    h("h2", {}, "Add a runner"),
    h("p", { class: "muted" }, "Over ssh, your own ssh configuration and keys are used. If a server is unreachable, its card can install the runner for you, and when a newer version is available, update it."),
    form);
  // Every card shows at once and fills in when its runner answers, so a
  // slow or unreachable runner holds up only its own card.
  const cards = new Map(); // runner name → its card, while shown
  const busy = new Set(); // runners being asked
  const refresh = (ref) => {
    if (busy.has(ref.name)) return;
    busy.add(ref.name);
    const key = JSON.stringify(ref);
    api("GET", "/api/runners/" + encodeURIComponent(ref.name))
      .then((v) => {
        const old = cards.get(ref.name);
        if (!old || old.dataset.ref !== key) return; // removed or changed meanwhile
        const card = runnerCard(v);
        card.dataset.ref = old.dataset.ref;
        old.replaceWith(card);
        cards.set(ref.name, card);
      })
      .catch(() => {}) // the next round tries again
      .finally(() => busy.delete(ref.name));
  };
  const load = async () => {
    let runners;
    try {
      installable = await api("GET", "/api/runner-version").catch(() => null);
      runners = await api("GET", "/api/runners?status=false");
    } catch (err) {
      fail(err);
      return;
    }
    if (runners.length === 0) {
      cards.clear();
      set(list, h("div", { class: "empty" }, "No runners yet. Add one below."));
      return;
    }
    // Keep shown cards; add new runners as pending, drop removed ones.
    const next = runners.map((ref) => {
      const key = JSON.stringify(ref);
      const card = cards.get(ref.name);
      return card && card.dataset.ref === key ? card : runnerCard(ref, true);
    });
    cards.clear();
    runners.forEach((ref, i) => {
      next[i].dataset.ref = JSON.stringify(ref);
      cards.set(ref.name, next[i]);
    });
    set(list, ...next);
    runners.forEach(refresh);
  };
  await load();
  every(5000, load);
}

// installable is the version an install or update would put on a server
// ({version} or {error}), as the runners page last asked.
let installable = null;

// updateTo is the version r's runner can be updated to, if it runs another.
function updateTo(r) {
  const v = installable && installable.version;
  return r.ssh && r.status && v && v !== "unknown" && r.status.version !== v ? v : "";
}

// runnerCard shows a runner; pending while it has not answered yet.
function runnerCard(r, pending) {
  const st = r.status;
  const update = updateTo(r);
  const path = "/api/runners/" + encodeURIComponent(r.name);
  const act = (what, msg) => async () => {
    try {
      await api("POST", path + "/" + what);
      say(msg);
      runnersView();
    } catch (err) {
      fail(err);
    }
  };
  return h("div", { class: "card" },
    h("h3", {}, r.name, " ", pending ? badge("checking…", "queued") : st ? (st.killed ? badge("stopped", "killed") : badge("ready", "succeeded")) : badge("unreachable", "failed")),
    h("p", { class: "muted" }, r.ssh ? "ssh " + r.ssh : r.socket),
    st && h("dl", { class: "facts" },
      h("dt", {}, "Target"), h("dd", {}, st.target),
      h("dt", {}, "Jobs"), h("dd", {}, st.running + " running, " + st.queued + " queued (at most " + st.maxConcurrent + " at once)"),
      h("dt", {}, "Schedules"), h("dd", {}, String(st.schedules), st.contactTimeout && h("span", { class: "muted note" },
        "Paused if nobody contacts this runner for " + st.contactTimeout + "; this Control Center does every 5 minutes while it runs.")),
      h("dt", {}, "Last contact"), h("dd", {}, fmtTime(st.lastContact)),
      h("dt", {}, "Plugins"), h("dd", {}, (r.plugins || []).length === 0 ? "none" : h("ul", { class: "explain" },
        r.plugins.map((p) => h("li", {}, p.name + " " + p.version + " (" + p.type + ", risk " + p.risk + ")",
          p.status && p.status.lastResult ? " – last run " + fmtTime(p.status.lastRun) + ": " + p.status.lastResult : "")))),
      h("dt", {}, "Images"), h("dd", {}, st.images.join(", ")),
      h("dt", {}, "Tools"), h("dd", {}, Object.keys(st.tools || {}).length === 0 ? "none" : Object.entries(st.tools).map(([img, list]) =>
        h("div", {}, h("span", { class: "muted" }, img), h("ul", { class: "chips" }, list.map((t) => h("li", {}, t)))))),
      h("dt", {}, "Version"), h("dd", {}, st.version || "–", update && h("span", { class: "muted note" }, "Version " + update + " is available."))),
    r.error && h("div", { class: "error-box" }, r.error),
    h("div", { class: "actions" },
      st && !st.killed && h("button", { class: "danger", onclick: act("kill", "Stopping every job on " + r.name + ".") }, "Stop all jobs"),
      st && st.killed && h("button", { onclick: act("resume", r.name + " accepts jobs again. Its schedules stay paused until you resume them.") }, "Accept jobs again"),
      r.ssh && !pending && !st && h("button", { class: "primary", onclick: () => installRunner(r) }, "Install Runner"),
      update && h("button", { class: "primary", onclick: () => installRunner(r, update) }, "Update runner"),
      h("button", { onclick: () => editRunner(r) }, "Edit"),
      h("button", { onclick: async () => {
        if (!confirm("Remove " + r.name + " from this list? The runner itself keeps running.")) return;
        try {
          await api("DELETE", path);
          runnersView();
        } catch (err) {
          fail(err);
        }
      } }, "Remove")));
}

// installRunner asks before making system changes on an ssh host, then
// follows the installer's progress without blocking the page. With
// version, it updates the runner that answers there, keeping its target.
function installRunner(r, version) {
  const st = version ? r.status : null;
  const target = h("input", { required: true, pattern: "server:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?", value: st ? st.target : "server:" + r.name, autocomplete: "off", readonly: !!st });
  const password = h("input", { type: "password", autocomplete: "off", placeholder: "only if sudo asks for one" });
  const confirmBox = h("input", { type: "checkbox" });
  const verb = st ? "Update" : "Install";
  const start = h("button", { class: "primary", disabled: true }, st ? "Update runner" : "Install Runner");
  const problem = h("div");
  const log = h("pre", { class: "console", hidden: true });
  let timer = null;
  let succeeded = false;

  const clearTimer = () => {
    if (timer) clearTimeout(timer);
    timer = null;
  };
  dialog.addEventListener("close", () => {
    clearTimer();
    if (succeeded) runnersView();
  }, { once: true });
  confirmBox.addEventListener("change", () => { start.disabled = !confirmBox.checked; });

  const poll = async () => {
    clearTimer();
    let st;
    try {
      st = await api("GET", pathForInstall(r));
    } catch (err) {
      set(problem, h("div", { class: "error-box" }, err.message));
      return;
    }
    if ((st.log || []).length > 0) {
      log.hidden = false;
      log.textContent = st.log.join("\n");
      log.scrollTop = log.scrollHeight;
    }
    if (st.running) {
      timer = setTimeout(poll, 2000);
      return;
    }
    if (st.error) {
      set(problem, h("div", { class: "error-box" }, st.error));
      start.disabled = false;
      return;
    }
    succeeded = true;
    say(verb + "d the runner on " + r.ssh + ".");
    set(problem, h("div", { class: "warn-box" }, verb + "d. Close this window to refresh the runner card."));
  };

  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    start.disabled = true;
    set(problem);
    const body = { target: target.value.trim(), confirmed: true };
    if (password.value !== "") body.sudoPassword = password.value;
    password.value = "";
    try {
      await api("POST", pathForInstall(r), body);
      log.hidden = false;
      log.textContent = "Starting…";
      poll();
    } catch (err) {
      start.disabled = false;
      set(problem, h("div", { class: "error-box" }, err.message));
    }
  } },
  h("h1", {}, verb + " runner on " + r.name),
  st
    ? h("p", {}, "This updates the runner on ", h("code", {}, r.ssh), " from " + (st.version || "an unknown version") + " to " + version +
      ": it replaces its programs, rebuilds the VM images if they changed, and restarts its service. Its credentials stay; its runner settings are written anew.")
    : h("p", {}, "This installs the Linux x86_64 runner on ", h("code", {}, r.ssh), ": it creates the unprivileged hcc-runner user, grants it one sudo rule for hcc-helper, builds the VM images, and starts its systemd service."),
  st && st.running + st.queued > 0 && h("div", { class: "warn-box" },
    "Jobs are running or queued there. Restarting would end them, so the update waits until they are done (or stopped)."),
  h("p", { class: "muted" }, "Your ssh keys are used. The sudo password, if needed, is sent once over that ssh connection and is not stored. This first installer needs the Control Center to be the bin/hcc of an hcc source checkout (or HCC_SOURCE_DIR set to one)."),
  h("div", { class: "form-stack" },
    h("label", {}, "Target name used by harnesses", target),
    h("label", {}, "Sudo password on " + r.ssh, password),
    h("label", { class: "check" }, confirmBox, "I want to " + verb.toLowerCase() + " the runner and make these system changes on " + r.ssh + ".")),
  problem,
  log,
  h("div", { class: "actions" }, start, h("button", { type: "button", onclick: () => dialog.close() }, "Close")));
  set(dialog, form);
  dialog.showModal();
}

function pathForInstall(r) {
  return "/api/runners/" + encodeURIComponent(r.name) + "/install";
}

function editRunner(r) {
  const fields = runnerFields(r);
  const problem = h("div", {});
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    const ref = fields.read();
    try {
      await api("PUT", "/api/runners/" + encodeURIComponent(r.name), ref);
      dialog.close();
      say("Saved runner " + ref.name + ".");
      runnersView();
    } catch (err) {
      set(problem, h("div", { class: "error-box" }, err.message));
    }
  } },
  h("h1", {}, "Edit " + r.name),
  h("p", { class: "muted" }, "This only changes how this Control Center reaches the runner. Harnesses address a runner by its own target name (" +
    (r.status ? r.status.target : "set when it was installed") + "), which stays the same."),
  h("div", { class: "form-stack" }, ...fields.labels),
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Save"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

// --- approvals (shown on every page) ---

async function loadApprovals() {
  if (document.hidden) return;
  let data;
  try {
    data = await api("GET", "/api/approvals");
  } catch {
    return; // the views report connection problems
  }
  set(approvalsBox, ...data.approvals.map(approvalCard));
  approvalsBox.hidden = data.approvals.length === 0;
}

function approvalCard(a) {
  const path = "/api/runners/" + encodeURIComponent(a.runner) + "/approvals/" + encodeURIComponent(a.id);
  const decide = (approve) => async () => {
    if (approve && !confirm("Let the agent do this?\n\n" + a.method + " " + a.host + a.path)) return;
    try {
      await api("POST", path, { approve });
      say(approve ? "Approved." : "Denied: the agent gets a refusal.");
    } catch (err) {
      fail(err);
    }
    loadApprovals();
  };
  const jobHref = "#/job/" + encodeURIComponent(a.runner) + "/" + encodeURIComponent(a.jobId);
  return h("div", { class: "approval" },
    h("h2", {}, "The agent asks to: ", a.actions.join(", ")),
    h("p", {}, h("code", {}, a.method + " " + a.host + a.path)),
    a.details && a.details.length > 0 && h("ul", {}, a.details.map((d) => h("li", {}, h("code", {}, d)))),
    h("p", { class: "muted" }, "Job ", h("a", { href: jobHref }, a.harness), " on " + a.runner +
      ". Refused automatically at " + fmtTime(a.expires) + " unless you decide."),
    h("div", { class: "actions" },
      h("button", { class: "primary", onclick: decide(true) }, "Approve"),
      h("button", { class: "danger", onclick: decide(false) }, "Deny")));
}

// --- kill switch ---

document.getElementById("stop-all").addEventListener("click", async () => {
  if (!confirm("Stop every job on every runner, pause every schedule, and refuse new jobs until you resume each runner?")) return;
  try {
    const res = await api("POST", "/api/kill");
    const failed = res.filter((r) => r.error);
    if (failed.length) {
      say("Could NOT reach: " + failed.map((r) => r.name + " (" + r.error + ")").join(", ") + ". Jobs there may still run.", true);
    } else {
      say("Every runner is stopping its jobs and refusing new ones. Schedules are paused; resume them one by one when you are ready.");
    }
  } catch (err) {
    fail(err);
  }
});

// --- start ---

async function start() {
  loadSession();
  try {
    await api("GET", "/api/session");
  } catch (err) {
    fail(err);
    return;
  }
  window.addEventListener("hashchange", route);
  route();
  loadApprovals();
  setInterval(loadApprovals, 3000);
}

start();
