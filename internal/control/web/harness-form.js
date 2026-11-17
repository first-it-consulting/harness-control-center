// The harness editor: a form for everyone, and the same harness as YAML
// for those who want it. The form edits the harness as plain data; the
// server turns it into YAML and checks it (POST /api/harness/encode), so
// the panel on the right always explains exactly what would be saved.
//
// Picking a connection and a model copies them in, and grants what
// reaching it needs (its credential, its host and port, the own network
// for a model server there), saying so next to the choice.

const SCHEDULES = [
  ["0 * * * *", "Every hour"],
  ["0 3 * * *", "Every night at 03:00"],
  ["0 9 * * 1-5", "Weekdays at 09:00"],
  ["0 9 * * 1", "Mondays at 09:00"],
];
const TIMEOUTS = ["5m", "15m", "30m", "1h", "2h", "4h", "8h"];
const MEMORY = [512, 1024, 2048, 4096, 8192, 16384];
const APPROVAL_LABELS = {
  "http.get": "Reading from a website or API (GET)",
  "http.head": "Checking a web address (HEAD)",
  "http.post": "Sending data (POST)",
  "http.put": "Replacing data (PUT)",
  "http.patch": "Changing data (PATCH)",
  "http.delete": "Deleting data (DELETE)",
  "http.other": "Other web requests",
  "git.push": "Pushing to a Git repository",
};

// --- the document ---

const get = (doc, path) => path.split(".").reduce((o, k) => (o == null ? undefined : o[k]), doc);

function put(doc, path, value) {
  const keys = path.split(".");
  let o = doc;
  for (const k of keys.slice(0, -1)) {
    if (o[k] == null || typeof o[k] !== "object" || Array.isArray(o[k])) o[k] = {};
    o = o[k];
  }
  const last = keys[keys.length - 1];
  if (value === undefined || value === "" || value === null || (Array.isArray(value) && value.length === 0)) delete o[last];
  else o[last] = value;
}

// endpointOf splits a model endpoint URL into what its egress rule needs.
function endpointOf(url) {
  try {
    const u = new URL(url);
    return { host: u.hostname.toLowerCase(), port: Number(u.port) || (u.protocol === "http:" ? 80 : 443) };
  } catch {
    return null;
  }
}

// --- the editor ---

export async function harnessEditor(ui, name, template) {
  const { h, api, set, show, fail } = ui;
  let yamlText = template;
  if (name) {
    try {
      yamlText = (await api("GET", "/api/harnesses/" + encodeURIComponent(name))).yaml;
    } catch (err) {
      fail(err);
      return;
    }
  }
  show(h("p", { class: "muted" }, "Loading…"));
  let ctx;
  let doc;
  try {
    const [runners, providers, policies, limits, decoded] = await Promise.all([
      api("GET", "/api/runners"),
      api("GET", "/api/providers").then((r) => r.providers).catch(() => []),
      api("GET", "/api/policies").then((r) => r.policies).catch(() => []),
      api("GET", "/api/harness/limits"),
      api("POST", "/api/harness/decode", { yaml: yamlText }),
    ]);
    const store = await api("GET", "/api/credentials").then((r) => r.credentials).catch(() => []);
    const variables = await api("GET", "/api/variables").then((r) => r.variables).catch(() => []);
    ctx = { runners, providers, policies, limits, store, variables, credentials: new Map(), models: new Map() };
    doc = decoded.doc;
  } catch (err) {
    fail(err);
    return;
  }

  const ed = { ui, name, ctx, doc, yaml: yamlText, mode: "form", check: null, seq: 0, timer: null };
  ed.panel = h("div", { class: "card" });
  ed.formBox = h("div", { class: "harness-form" });
  ed.text = h("textarea", { class: "yaml", spellcheck: "false", "aria-label": "Harness (YAML)", hidden: true });
  ed.runBtn = h("button", { class: "primary", onclick: () => ui.runDialog(ed.yaml) }, "Run…");
  ed.saveBtn = h("button", { onclick: () => save(ed) }, "Save");
  const tabForm = h("button", { type: "button", class: "tab active", onclick: () => switchMode(ed, "form") }, "Form");
  const tabYAML = h("button", { type: "button", class: "tab", onclick: () => switchMode(ed, "yaml") }, "YAML");
  ed.tabs = { form: tabForm, yaml: tabYAML };
  ed.text.addEventListener("input", () => {
    ed.yaml = ed.text.value;
    schedule(ed, () => checkYAML(ed));
  });

  show(
    h("div", { class: "head" },
      h("div", {}, h("h1", {}, name || "New harness"),
        h("p", { class: "muted" }, "Say what the agent should do and what it may use. Everything you do not allow is denied; the right side says in plain words what it may do.")),
      h("div", { class: "actions" }, ed.saveBtn, ed.runBtn,
        name && h("button", { onclick: () => remove(ed) }, "Delete"),
        h("a", { class: "button", href: "#/harnesses" }, "Back"))),
    h("div", { class: "tabs", role: "tablist" }, tabForm, tabYAML),
    h("div", { class: "editor" }, h("div", {}, ed.formBox, ed.text), ed.panel));

  await loadCredentials(ed);
  render(ed);
  await encode(ed);
}

function schedule(ed, fn) {
  clearTimeout(ed.timer);
  ed.timer = setTimeout(fn, 300);
}

// changed is called by every field: the form's data changed.
function changed(ed, rerender) {
  if (rerender) render(ed);
  schedule(ed, () => encode(ed));
}

async function encode(ed) {
  const mine = ++ed.seq;
  try {
    const r = await ed.ui.api("POST", "/api/harness/encode", { doc: ed.doc });
    if (mine !== ed.seq) return;
    ed.yaml = r.yaml;
    showCheck(ed, r.check);
  } catch (err) {
    ed.ui.fail(err);
  }
}

async function checkYAML(ed) {
  const mine = ++ed.seq;
  try {
    const c = await ed.ui.api("POST", "/api/check", { yaml: ed.yaml });
    if (mine !== ed.seq) return;
    showCheck(ed, c);
  } catch (err) {
    ed.ui.fail(err);
  }
}

function showCheck(ed, c) {
  ed.check = c;
  ed.ui.set(ed.panel, ...ed.ui.checkBlock(c));
  ed.runBtn.disabled = !c.valid;
  ed.saveBtn.disabled = !c.valid;
}

async function switchMode(ed, mode) {
  if (mode === ed.mode) return;
  if (mode === "yaml") {
    clearTimeout(ed.timer);
    await encode(ed);
    ed.text.value = ed.yaml;
  } else {
    // The YAML may have been edited: read it back into the form.
    try {
      ed.doc = (await ed.ui.api("POST", "/api/harness/decode", { yaml: ed.text.value })).doc;
    } catch (err) {
      ed.ui.say("The YAML cannot be shown as a form: " + err.message, true);
      return;
    }
    render(ed);
    await encode(ed);
  }
  ed.mode = mode;
  ed.formBox.hidden = mode !== "form";
  ed.text.hidden = mode !== "yaml";
  ed.tabs.form.classList.toggle("active", mode === "form");
  ed.tabs.yaml.classList.toggle("active", mode === "yaml");
}

async function save(ed) {
  const { api, say, fail } = ed.ui;
  if (ed.mode === "form") {
    clearTimeout(ed.timer);
    await encode(ed);
  }
  try {
    const r = await api("PUT", "/api/harnesses", { yaml: ed.yaml });
    say("Saved " + r.name + ".");
    if (r.name !== ed.name) location.hash = "#/harness/" + encodeURIComponent(r.name);
  } catch (err) {
    fail(err);
  }
}

async function remove(ed) {
  if (!confirm("Delete the harness " + ed.name + "? Jobs that already ran are kept.")) return;
  try {
    await ed.ui.api("DELETE", "/api/harnesses/" + encodeURIComponent(ed.name));
    location.hash = "#/harnesses";
  } catch (err) {
    ed.ui.fail(err);
  }
}

// --- what the runners offer ---

const targetOf = (doc) => get(doc, "spec.target") || "local";

// runnersFor are the reachable runners a harness with target runs on.
const runnersFor = (ed, target) => ed.ctx.runners.filter((r) => r.status && r.status.target === target);

async function loadCredentials(ed) {
  const target = targetOf(ed.doc);
  if (ed.ctx.credentials.has(target)) return;
  const names = new Set();
  await Promise.all(runnersFor(ed, target).map((r) =>
    ed.ui.api("GET", "/api/runners/" + encodeURIComponent(r.name) + "/credentials")
      .then((l) => l.credentials.forEach((c) => names.add(c.name)))
      .catch(() => {})));
  ed.ctx.credentials.set(target, [...names].sort());
}

// --- the form ---

function render(ed) {
  const { h, set } = ed.ui;
  set(ed.formBox,
    section(ed, "Basics", basics(ed)),
    section(ed, "Task", task(ed)),
    section(ed, "Model", model(ed)),
    section(ed, "When it runs", trigger(ed)),
    section(ed, "Safety limits", limits(ed)),
    section(ed, "Internet and network", network(ed)),
    section(ed, "Secrets and variables", [
      h("h3", {}, "Secrets the agent reads"), ...secrets(ed),
      h("h3", {}, "Secrets added to web requests"), ...credentials(ed),
      h("h3", {}, "Variables"), ...variablesPart(ed)]),
    section(ed, "Tools", tools(ed)),
    section(ed, "Ask me first", approvals(ed)),
    h("details", { class: "form-section card" }, h("summary", {}, "Advanced: helper agents, MCP servers, other agents"), ...advanced(ed)));
}

function section(ed, title, children) {
  const { h } = ed.ui;
  return h("section", { class: "card form-section" }, h("h2", {}, title), h("div", { class: "form-stack" }, ...children));
}

const help = (h, t) => h("span", { class: "muted note" }, t);

// text binds an input to a path.
function text(ed, path, attrs, transform = (v) => v) {
  const el = ed.ui.h("input", { autocomplete: "off", ...attrs, value: get(ed.doc, path) ?? "" });
  el.addEventListener("input", () => {
    put(ed.doc, path, transform(el.value.trim() === "" ? undefined : el.value));
    changed(ed);
  });
  return el;
}

function number(ed, path, attrs) {
  return text(ed, path, { type: "number", step: "any", ...attrs }, (v) => (v === undefined ? undefined : Number(v)));
}

function select(ed, path, options, attrs = {}, rerender = false) {
  const { h } = ed.ui;
  const current = get(ed.doc, path) ?? "";
  const opts = options.slice();
  if (current !== "" && !opts.some(([v]) => String(v) === String(current))) opts.push([current, String(current)]);
  const el = h("select", attrs, opts.map(([v, label]) => h("option", { value: String(v), selected: String(v) === String(current) }, label)));
  el.addEventListener("change", () => {
    put(ed.doc, path, el.value === "" ? undefined : el.value);
    changed(ed, rerender);
  });
  return el;
}

function checkbox(ed, label, checked, onChange) {
  const { h } = ed.ui;
  const box = h("input", { type: "checkbox", checked });
  box.addEventListener("change", () => onChange(box.checked));
  return h("label", { class: "check" }, box, label);
}

function basics(ed) {
  const { h } = ed.ui;
  const targets = [...new Set(ed.ctx.runners.map((r) => r.status && r.status.target).filter(Boolean))].sort();
  if (!targets.includes("local")) targets.unshift("local");
  const target = select(ed, "spec.target", targets.map((t) => [t, t === "local" ? "This Mac (local)" : t + runnerNames(ed, t)]), {}, true);
  target.addEventListener("change", async () => {
    await loadCredentials(ed);
    render(ed);
  });
  const images = [...new Set(runnersFor(ed, targetOf(ed.doc)).flatMap((r) => r.status.images))];
  if (images.length === 0) images.push("base", "claude-code");
  if (!get(ed.doc, "spec.runtime")) put(ed.doc, "spec.runtime", "claude-code");
  return [
    h("label", {}, "Name", text(ed, "metadata.name", { required: true, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", placeholder: "my-harness" }),
      help(h, "Lowercase letters, digits and '-'. Saving under a new name makes a copy.")),
    h("label", {}, "Description", text(ed, "metadata.description", { placeholder: "What this agent is for (optional)" })),
    h("div", { class: "row" },
      h("label", {}, "Runs on", target),
      h("label", {}, "Sandbox image", select(ed, "spec.sandbox.image", [["", "Default (" + ed.ctx.limits.defaults.image + ")"], ...images.map((i) => [i, i])], {}, true),
        help(h, "claude-code has the agent; base is an empty Linux."))),
  ];
}

function runnerNames(ed, target) {
  const names = runnersFor(ed, target).map((r) => r.name);
  return names.length > 0 ? " (" + names.join(", ") + ")" : "";
}

function task(ed) {
  const { h } = ed.ui;
  const area = h("textarea", { class: "prompt", rows: "8", placeholder: "Describe the task as you would to a colleague." });
  area.value = get(ed.doc, "spec.prompt") || "";
  area.addEventListener("input", () => {
    put(ed.doc, "spec.prompt", area.value.trim() === "" ? undefined : area.value);
    changed(ed);
  });
  return [h("label", {}, "What should the agent do?", area)];
}

// --- model ---

const sameURL = (a, b) => (a || "").replace(/\/+$/, "") === (b || "").replace(/\/+$/, "");

// providerOf is the provider the harness's model is at.
const providerOf = (ed) => {
  const m = get(ed.doc, "spec.model");
  return m ? ed.ctx.providers.find((p) => sameURL(p.url, m.endpoint)) || null : null;
};

// loadModels asks a provider for its models, once per page.
function loadModels(ed, p) {
  if (!ed.ctx.models.has(p.name)) {
    ed.ctx.models.set(p.name, ed.ui.api("POST", "/api/providers/" + encodeURIComponent(p.name) + "/models")
      .then((r) => ({ models: r.models }), (err) => ({ error: err.message })));
  }
  return ed.ctx.models.get(p.name);
}

function model(ed) {
  const { h } = ed.ui;
  const providers = ed.ctx.providers;
  const m = get(ed.doc, "spec.model");
  const current = providerOf(ed);
  // Connections switched off on the Connections page are not offered (unless
  // this harness uses one already).
  const opts = [["", "The runtime's default"], ...providers.filter((p) => !p.disabled || p === current).map((p) => [p.name, p.name + " (" + p.url + ")"])];
  let value = current ? current.name : "";
  if (m && !current) {
    opts.push(["custom", m.endpoint + " (not on the Connections page)"]);
    value = "custom";
  }
  const providerSel = h("select", {}, opts.map(([v, l]) => h("option", { value: v, selected: v === value }, l)));
  providerSel.addEventListener("change", () => {
    if (providerSel.value === "custom") return;
    applyProvider(ed, providers.find((p) => p.name === providerSel.value) || null);
    changed(ed, true);
  });
  const out = [h("label", {}, "Connection", providerSel,
    providers.length === 0 ? help(h, "Add the services your models run at on the Connections page to choose them here.") : help(h, "Where the model runs."))];
  if (!m) return out;

  // The model: from the provider's list, or typed.
  const listID = "provider-models";
  const list = h("datalist", { id: listID });
  const name = text(ed, "spec.model.name", { required: true, list: listID, placeholder: "Pick or type the model's name" });
  const status = help(h, "");
  if (current) {
    status.textContent = "Asking " + current.name + " for its models…";
    loadModels(ed, current).then((r) => {
      if (r.error) {
        status.textContent = "Could not list its models (" + r.error + "). Type the model's name.";
        return;
      }
      list.replaceChildren(...r.models.map((x) => h("option", { value: x.id }, x.label || x.id)));
      status.textContent = r.models.length + " models offered; click the field to choose.";
    });
  }
  out.push(h("label", {}, "Model", name, list, status));

  const e = endpointOf(m.endpoint);
  const granted = [e && "network access to " + e.host + ":" + e.port, m.credential && "the credential " + m.credential,
    get(ed.doc, "spec.permissions.network.allowPrivateNetworks") && "your own network"].filter(Boolean);
  out.push(h("div", { class: "notice-box" }, h("p", {}, "To reach it, this harness may use " + granted.join(", ") + ".")));
  if (m.endpoint.startsWith("http://")) {
    out.push(h("div", { class: "error-box" }, "Unencrypted: prompts and code" + (m.credential ? ", and the credential," : "") + " cross the network in clear text."));
  }
  out.push(h("details", {}, h("summary", {}, "More model settings (optional)"),
    h("div", { class: "form-stack" },
      h("label", {}, "Context window (tokens)", number(ed, "spec.model.contextWindow", { min: "8192", max: "1000000", step: "1", placeholder: "the runtime's guess" }),
        help(h, "Only for a model Claude Code does not know; it then assumes 200,000.")),
      h("label", {}, "Model for quick side tasks", text(ed, "spec.model.smallModel", { list: listID, placeholder: "the same model" }),
        help(h, "A faster model at the same provider for summaries and titles.")),
      h("div", { class: "row" },
        h("label", {}, "Longest answer (tokens)", number(ed, "spec.model.maxOutputTokens", { min: "1024", max: "262144", step: "1", placeholder: "runtime default" })),
        h("label", {}, "Thinking per answer (tokens)", number(ed, "spec.model.thinkingTokens", { min: "1024", max: "262144", step: "1", placeholder: "no extended thinking" }))))));
  return out;
}

// applyProvider points the model at a provider (null: the runtime's
// default), taking back what the previous one was granted and granting
// what this one needs.
function applyProvider(ed, p) {
  const doc = ed.doc;
  const prev = get(doc, "spec.model");
  if (prev) {
    const e = endpointOf(prev.endpoint);
    if (e) {
      put(doc, "spec.permissions.network.egress", (get(doc, "spec.permissions.network.egress") || []).filter((r) => !(r.host === e.host && samePorts(r.ports, e.port))));
    }
    if (prev.credential) {
      put(doc, "spec.permissions.credentials", (get(doc, "spec.permissions.credentials") || []).filter((n) => n !== prev.credential));
    }
    const was = ed.ctx.providers.find((x) => sameURL(x.url, prev.endpoint));
    if (was && was.private) put(doc, "spec.permissions.network.allowPrivateNetworks", undefined);
  }
  if (!p) {
    put(doc, "spec.model", undefined);
    return;
  }
  const m = { name: (prev && prev.name) || "", endpoint: p.url };
  if (p.credential) {
    m.credential = p.credential;
    const creds = get(doc, "spec.permissions.credentials") || [];
    if (!creds.includes(p.credential)) put(doc, "spec.permissions.credentials", [...creds, p.credential]);
  }
  put(doc, "spec.model", m);
  const e = endpointOf(p.url);
  const rules = get(doc, "spec.permissions.network.egress") || [];
  if (e && !rules.some((r) => r.host === e.host && samePorts(r.ports, e.port))) {
    put(doc, "spec.permissions.network.egress", [...rules, e.port === 443 ? { host: e.host } : { host: e.host, ports: [e.port] }]);
  }
  if (p.private) put(doc, "spec.permissions.network.allowPrivateNetworks", true);
}

// samePorts: a rule's ports are just port (no ports means 443).
const samePorts = (ports, port) => (ports && ports.length > 0 ? ports.length === 1 && Number(ports[0]) === port : port === 443);

// --- when it runs ---

function trigger(ed) {
  const { h } = ed.ui;
  const t = get(ed.doc, "spec.trigger") || {};
  const kind = t.schedule ? "schedule" : t.loop ? "loop" : "manual";
  const radio = (value, label) => {
    const r = h("input", { type: "radio", name: "trigger", value, checked: kind === value });
    r.addEventListener("change", () => {
      if (value === "manual") put(ed.doc, "spec.trigger", undefined);
      if (value === "schedule") put(ed.doc, "spec.trigger", { schedule: SCHEDULES[1][0] });
      if (value === "loop") put(ed.doc, "spec.trigger", { loop: { maxIterations: 5 } });
      changed(ed, true);
    });
    return h("label", { class: "check" }, r, label);
  };
  const out = [h("div", { class: "row" }, radio("manual", "When I start it"), radio("schedule", "On a schedule"), radio("loop", "Again and again until done"))];
  if (kind === "schedule") {
    const preset = h("select", {}, [...SCHEDULES.map(([v, l]) => h("option", { value: v, selected: v === t.schedule }, l)),
      h("option", { value: "custom", selected: !SCHEDULES.some(([v]) => v === t.schedule) }, "Something else…")]);
    const cron = text(ed, "spec.trigger.schedule", { placeholder: "minute hour day month weekday", required: true });
    cron.hidden = preset.value !== "custom";
    preset.addEventListener("change", () => {
      cron.hidden = preset.value !== "custom";
      if (preset.value !== "custom") {
        put(ed.doc, "spec.trigger.schedule", preset.value);
        cron.value = preset.value;
        changed(ed);
      }
    });
    out.push(h("label", {}, "How often", preset, cron, help(h, "Times are UTC. A schedule pauses itself when nobody looks after the runner for a while.")));
  }
  if (kind === "loop") {
    out.push(h("div", { class: "row" },
      h("label", {}, "At most this many runs", number(ed, "spec.trigger.loop.maxIterations", { min: "1", max: String(ed.ctx.limits.maxIterations), required: true })),
      h("label", {}, "Pause between runs", text(ed, "spec.trigger.loop.interval", { placeholder: "none, e.g. 10m" }))),
    help(h, "Each run starts fresh and stops when the agent says the task is done."));
  }
  return out;
}

// --- limits ---

function limits(ed) {
  const { h } = ed.ui;
  const l = ed.ctx.limits;
  return [
    h("p", { class: "muted" }, "An agent works without anyone watching. These limits stop a run as soon as one of them is reached, so a stuck or runaway agent cannot go on forever or spend too much. Empty fields use the default shown."),
    h("div", { class: "row" },
      h("label", {}, "Longest run time", select(ed, "spec.sandbox.timeout", [["", "Default (" + short(l.defaults.timeout) + ")"], ...TIMEOUTS.map((t) => [t, t])]),
        help(h, "The sandbox is switched off after this.")),
      h("label", {}, "Most money (USD)", number(ed, "spec.budget.maxCostUSD", { min: "0.01", max: String(l.maxCostUSD), placeholder: String(l.defaults.maxCostUSD) }),
        help(h, "What the model's use may cost. Counted for models the runner has a price for (runner settings).")),
      h("label", {}, "Most tokens", number(ed, "spec.budget.maxTokens", { min: "1", max: String(l.maxTokens), step: "1", placeholder: l.defaults.maxTokens.toLocaleString("en-US") }),
        help(h, "Text the model reads and writes, in tokens (about ¾ of a word each). Counted for every model."))),
    h("details", {}, h("summary", {}, "Size of the sandbox"),
      h("div", { class: "row" },
        h("label", {}, "CPUs", number(ed, "spec.sandbox.cpus", { min: "1", max: String(l.maxCPUs), step: "1", placeholder: String(l.defaults.cpus) })),
        h("label", {}, "Memory", select(ed, "spec.sandbox.memoryMiB", [["", "Default (" + l.defaults.memoryMiB + " MiB)"], ...MEMORY.map((m) => [m, m >= 1024 ? m / 1024 + " GiB" : m + " MiB"])])),
        h("label", {}, "Results it can hand back (MiB)", number(ed, "spec.sandbox.diskMiB", { min: "1", max: String(l.maxDiskMiB), step: "1", placeholder: String(l.defaults.diskMiB) }),
          help(h, "Files the agent leaves in its work folder are returned when it finishes, up to this size.")))),
  ];
}

const short = (d) => d.replace(/(\D)0s$/, "$1").replace(/(h)0m$/, "$1");

// --- network ---

// covers: egress allows all of a policy's hosts (on their ports).
const covers = (rules, p) => p.hosts.every((x) => rules.some((r) => r.host === x.host && sameList(r.ports, x.ports)));
const sameList = (a, b) => {
  const n = (p) => [...new Set(p && p.length > 0 ? p.map(Number) : [443])].sort((x, y) => x - y).join(",");
  return n(a) === n(b);
};

// neededElsewhere: a rule something else in the harness still needs
// (its model, a credential, another attached policy).
function neededElsewhere(ed, r, except) {
  const doc = ed.doc;
  const m = endpointOf(get(doc, "spec.model.endpoint") || "");
  if (m && m.host === r.host && sameList(r.ports, [m.port])) return true;
  const granted = get(doc, "spec.permissions.credentials") || [];
  for (const c of ed.ctx.store) {
    if (granted.includes(c.name) && c.hosts.includes(r.host) && sameList(r.ports, [c.port])) return true;
  }
  const rules = get(doc, "spec.permissions.network.egress") || [];
  return ed.ctx.policies.some((p) => p.name !== except.name && covers(rules, p) && p.hosts.some((x) => x.host === r.host && sameList(r.ports, x.ports)));
}

function policies(ed) {
  const { h } = ed.ui;
  const rules = get(ed.doc, "spec.permissions.network.egress") || [];
  if (ed.ctx.policies.length === 0) {
    return [help(h, "Network policies (named sets of hosts such as GitHub or Python packages) can be defined on the Network page and attached here.")];
  }
  return [
    h("p", {}, "Network policies"),
    h("div", { class: "checks" }, ed.ctx.policies.map((p) => checkbox(ed, p.name + (p.description ? " — " + p.description : ""), covers(rules, p), (on) => {
      let next = (get(ed.doc, "spec.permissions.network.egress") || []).slice();
      if (on) {
        for (const x of p.hosts) {
          if (!next.some((r) => r.host === x.host && sameList(r.ports, x.ports))) next.push(x.ports && x.ports.length > 0 ? { host: x.host, ports: x.ports } : { host: x.host });
        }
        if (p.private) put(ed.doc, "spec.permissions.network.allowPrivateNetworks", true);
      } else {
        next = next.filter((r) => !(p.hosts.some((x) => x.host === r.host && sameList(r.ports, x.ports)) && !neededElsewhere(ed, r, p)));
      }
      put(ed.doc, "spec.permissions.network.egress", next);
      changed(ed, true);
    }))),
  ];
}

function network(ed) {
  const { h } = ed.ui;
  const rules = get(ed.doc, "spec.permissions.network.egress") || [];
  const list = h("div", { class: "form-stack" });
  rules.forEach((r, i) => {
    const host = h("input", { value: r.host, placeholder: "github.com", autocomplete: "off", required: true });
    const ports = h("input", { value: (r.ports || []).join(", "), placeholder: "443", autocomplete: "off", class: "ports" });
    const update = () => {
      const next = (get(ed.doc, "spec.permissions.network.egress") || []).slice();
      const p = ports.value.split(/[\s,]+/).filter(Boolean).map(Number);
      next[i] = p.length > 0 ? { host: host.value.trim().toLowerCase(), ports: p } : { host: host.value.trim().toLowerCase() };
      put(ed.doc, "spec.permissions.network.egress", next);
      changed(ed);
    };
    host.addEventListener("input", update);
    ports.addEventListener("input", update);
    list.append(h("div", { class: "row rule" }, h("label", {}, "Host", host), h("label", {}, "Ports", ports),
      h("button", { type: "button", onclick: () => {
        const next = (get(ed.doc, "spec.permissions.network.egress") || []).filter((_, j) => j !== i);
        put(ed.doc, "spec.permissions.network.egress", next);
        changed(ed, true);
      } }, "Remove")));
  });
  const private_ = get(ed.doc, "spec.permissions.network.allowPrivateNetworks") === true;
  return [
    ...policies(ed),
    h("p", {}, "All hosts it may reach"),
    rules.length === 0 ? h("p", { class: "muted" }, "None: the agent cannot reach anything.") : help(h, "From the policies, the model, the credentials, and any you add. Only these hosts, on these ports (443, the usual one for https, if none). *.example.com means all of example.com's subdomains (for web tools only). Database clients and ssh reach the exact hosts listed, on their ports."),
    list,
    h("div", { class: "actions" }, h("button", { type: "button", onclick: () => {
      put(ed.doc, "spec.permissions.network.egress", [...rules, { host: "" }]);
      changed(ed, true);
    } }, "Allow a host")),
    checkbox(ed, "Some of these hosts are in my own network (e.g. nas.lan, a model server at home)", private_, (on) => {
      put(ed.doc, "spec.permissions.network.allowPrivateNetworks", on || undefined);
      changed(ed, true);
    }),
    help(h, "Needed for names that only exist at home. Without it, the agent can only reach public internet hosts."),
    private_ && h("div", { class: "error-box" }, "Hosts in your own network (a NAS, a router, other computers) can be reached if they are listed above. List only what the agent needs."),
  ];
}

// --- credentials ---

function credentials(ed) {
  const { h } = ed.ui;
  const granted = get(ed.doc, "spec.permissions.credentials") || [];
  const stored = new Map(ed.ctx.store.filter((c) => c.kind !== "secret").map((c) => [c.name, c]));
  const secretNames = new Set(ed.ctx.store.filter((c) => c.kind === "secret").map((c) => c.name));
  const onRunners = (ed.ctx.credentials.get(targetOf(ed.doc)) || []).filter((n) => !secretNames.has(n));
  const all = [...new Set([...stored.keys(), ...onRunners, ...granted])].sort();
  if (all.length === 0) {
    return [h("p", { class: "muted" }, "No credentials yet. Add API keys and tokens on the Credentials page, then attach them here.")];
  }
  const modelCred = get(ed.doc, "spec.model.credential");
  const where = (n) => {
    const c = stored.get(n);
    if (c) return " — sent to " + c.hosts.join(", ");
    if (onRunners.includes(n)) return " — only on the runner";
    return " — not found: add it on the Credentials page";
  };
  return [
    help(h, "Attached credentials are copied to the runner when the harness starts. The runner adds each one only to requests to its hosts; the agent never sees them. Their hosts must also be allowed under Internet and network."),
    ...all.map((n) => checkbox(ed, n + (n === modelCred ? " (for the model)" : "") + where(n), granted.includes(n), (on) => {
      const next = on ? [...granted, n] : granted.filter((x) => x !== n);
      put(ed.doc, "spec.permissions.credentials", next);
      // Its hosts must be reachable for it to be any use: allowed with
      // it, and taken back with it (unless something else needs them).
      const c = stored.get(n);
      if (c) {
        const rules = get(ed.doc, "spec.permissions.network.egress") || [];
        if (on) {
          const add = c.hosts.filter((host) => !rules.some((r) => r.host === host && samePorts(r.ports, c.port)))
            .map((host) => (c.port === 443 ? { host } : { host, ports: [c.port] }));
          put(ed.doc, "spec.permissions.network.egress", [...rules, ...add]);
        } else {
          const model = endpointOf(get(ed.doc, "spec.model.endpoint") || "");
          put(ed.doc, "spec.permissions.network.egress", rules.filter((r) => !(c.hosts.includes(r.host) && samePorts(r.ports, c.port) &&
            !(model && model.host === r.host && model.port === c.port))));
        }
      }
      changed(ed, true);
    })),
  ];
}

// --- secrets for the agent (ADR-0020) ---

// defaultEnv suggests a variable name for a secret: DB_PASSWORD for
// db-password.
const defaultEnv = (name) => name.toUpperCase().replace(/[^A-Z0-9]+/g, "_").replace(/^([0-9])/, "_$1");

function secrets(ed) {
  const { h } = ed.ui;
  const granted = get(ed.doc, "spec.permissions.secrets") || [];
  const available = ed.ctx.store.filter((c) => c.kind === "secret").map((c) => c.name);
  const all = [...new Set([...available, ...granted.map((s) => s.name)])].sort();
  if (all.length === 0) {
    return [help(h, "Passwords, tokens or files the agent itself needs (a database password, a kubeconfig). Add them on the Credentials page as \"for the agent itself\", then choose them here.")];
  }
  const set_ = (next) => {
    put(ed.doc, "spec.permissions.secrets", next);
    changed(ed, true);
  };
  const rows = all.map((name) => {
    const g = granted.find((s) => s.name === name);
    const box = h("input", { type: "checkbox", checked: !!g });
    box.addEventListener("change", () => set_(box.checked
      ? [...granted, { name, env: defaultEnv(name) }]
      : granted.filter((s) => s.name !== name)));
    const out = [h("label", { class: "check" }, box, name, !available.includes(name) && " (not on the Credentials page)")];
    if (g) {
      const as = h("select", {}, h("option", { value: "env", selected: !g.file }, "as the environment variable"), h("option", { value: "file", selected: !!g.file }, "as the file ~/"));
      const where = h("input", { autocomplete: "off", value: g.file || g.env || "", placeholder: g.file ? ".kube/config" : defaultEnv(name) });
      const update = () => {
        const v = where.value.trim();
        set_(granted.map((s) => (s.name === name ? (as.value === "file" ? { name, file: v } : { name, env: v }) : s)));
      };
      as.addEventListener("change", () => {
        where.value = as.value === "file" ? (name.includes("kube") ? ".kube/config" : name) : defaultEnv(name);
        update();
      });
      where.addEventListener("change", update);
      out.push(h("div", { class: "row rule" }, h("label", {}, "Given", as), h("label", {}, as.value === "file" ? "File" : "Variable", where)));
    }
    return h("div", { class: "form-stack" }, ...out);
  });
  return [
    help(h, "Handed to the agent when it starts. The agent can read them, unlike credentials; the console shows them as ***."),
    ...rows,
    granted.length > 0 && h("div", { class: "error-box" }, "The agent can read " + (granted.length === 1 ? "this secret" : "these secrets") +
      ". A misled agent could send " + (granted.length === 1 ? "it" : "them") + " to any host it may reach, so allow only the hosts it needs."),
  ];
}

// --- variables (spec.env): copies of the Secrets and variables page's ---

function variablesPart(ed) {
  const { h } = ed.ui;
  const env = get(ed.doc, "spec.env") || {};
  const kept = new Map(ed.ctx.variables.map((v) => [v.name, v.value]));
  const names = [...new Set([...kept.keys(), ...Object.keys(env)])].sort();
  if (names.length === 0) {
    return [help(h, "Values that are not secret (a region, a host name) for the agent's environment. Add them under Secrets & variables, then choose them here.")];
  }
  return [
    help(h, "Set as environment variables for the agent. Not secret: their values become part of this harness."),
    h("div", { class: "checks" }, names.map((n) => {
      const has = Object.prototype.hasOwnProperty.call(env, n);
      const value = has ? env[n] : kept.get(n);
      const differs = has && kept.has(n) && kept.get(n) !== env[n];
      return checkbox(ed, n + " = " + value + (differs ? " (the page now says " + kept.get(n) + ")" : "") + (!kept.has(n) ? " (only in this harness)" : ""), has, (on) => {
        const next = { ...(get(ed.doc, "spec.env") || {}) };
        if (on) next[n] = kept.has(n) ? kept.get(n) : value;
        else delete next[n];
        put(ed.doc, "spec.env", Object.keys(next).length > 0 ? next : undefined);
        changed(ed, true);
      });
    })),
  ];
}

// --- tools ---

function tools(ed) {
  const { h } = ed.ui;
  const image = get(ed.doc, "spec.sandbox.image") || ed.ctx.limits.defaults.image;
  const offered = new Map();
  for (const r of runnersFor(ed, targetOf(ed.doc))) {
    for (const t of (r.status.tools || {})[image] || []) offered.set(t.split("@")[0], t);
  }
  const chosen = get(ed.doc, "spec.permissions.tools.install") || [];
  const names = [...new Set([...offered.keys(), ...chosen.map((t) => t.split("@")[0])])].sort();
  const more = help(h, "Need another tool? Add it on the runner's page (Runners → the runner → Tools).");
  if (names.length === 0) return [h("p", { class: "muted" }, "The image " + image + " offers no tools to install here."), more];
  return [
    help(h, "Installed into the sandbox before the agent starts, from the runner's checked package list. The agent cannot install anything else."),
    h("div", { class: "checks" }, names.map((n) => checkbox(ed, toolLabel(offered.get(n) || n), chosen.some((t) => t.split("@")[0] === n), (on) => {
      const next = on ? [...chosen, n] : chosen.filter((t) => t.split("@")[0] !== n);
      put(ed.doc, "spec.permissions.tools.install", next);
      changed(ed);
    }))),
    more,
  ];
}

// toolLabel shows "curl@8.23.0-r0" as "curl (8.23.0)".
function toolLabel(t) {
  const [name, version] = t.split("@");
  return version ? name + " (" + version.replace(/-r\d+$/, "") + ")" : name;
}

// --- helpers and approvals ---

function approvals(ed) {
  const { h } = ed.ui;
  const required = get(ed.doc, "spec.approvals.requireFor") || [];
  const actions = [...new Set([...ed.ctx.limits.approvalActions, ...required])];
  return [
    h("p", { class: "muted" }, "The agent pauses before these and waits for you: you approve or deny on the dashboard. Without an answer it is denied after a while (30 minutes, unless the runner is set otherwise). For a scheduled run, only choose what you will be around to answer."),
    h("div", { class: "checks" }, actions.map((a) => checkbox(ed, APPROVAL_LABELS[a] || a, required.includes(a), (on) => {
      const next = on ? [...required, a] : required.filter((x) => x !== a);
      put(ed.doc, "spec.approvals.requireFor", next);
      changed(ed);
    }))),
  ];
}

// --- MCP servers and A2A agents ---

function advanced(ed) {
  const { h } = ed.ui;
  const l = ed.ctx.limits;
  return [
    [h("h3", {}, "Helper agents"),
      help(h, "The agent can split a big task and start helper agents for parts of it by itself; you do not talk to them. Each helper runs in its own sandbox with at most this harness's permissions and shares its limits. Leave it at 0 unless the task is large."),
      h("label", {}, "Helper agents it may start", number(ed, "spec.permissions.subagents.max", { min: "0", max: String(l.maxSubagents), step: "1", placeholder: "0 (it works alone)" }))],
    rows(ed, "MCP servers", "spec.mcp.servers", [["name", "Name", "notes"], ["url", "Address (https://…)", "https://mcp.example.com/mcp"]],
      "Tools the agent may call. Their host must be allowed under Internet and network."),
    rows(ed, "Other agents (A2A)", "spec.a2a.agents", [["name", "Name", "reviewer"], ["url", "Address (https://…)", "https://agent.example.com/a2a"]],
      "Agents it may send messages to. Their host must be allowed under Internet and network."),
  ].map((x) => h("div", { class: "form-stack" }, ...x));
}

function rows(ed, title, path, fields, note) {
  const { h } = ed.ui;
  const items = get(ed.doc, path) || [];
  const out = [h("h3", {}, title), help(h, note)];
  items.forEach((item, i) => {
    const inputs = fields.map(([key, label, placeholder]) => {
      const el = h("input", { value: item[key] || "", placeholder, autocomplete: "off" });
      el.addEventListener("input", () => {
        const next = (get(ed.doc, path) || []).map((x) => ({ ...x }));
        next[i][key] = el.value.trim();
        put(ed.doc, path, next);
        changed(ed);
      });
      return h("label", {}, label, el);
    });
    out.push(h("div", { class: "row rule" }, ...inputs, h("button", { type: "button", onclick: () => {
      put(ed.doc, path, (get(ed.doc, path) || []).filter((_, j) => j !== i));
      changed(ed, true);
    } }, "Remove")));
  });
  out.push(h("div", { class: "actions" }, h("button", { type: "button", onclick: () => {
    put(ed.doc, path, [...items, {}]);
    changed(ed, true);
  } }, "Add")));
  return out;
}
