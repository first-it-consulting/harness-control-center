// Models: the AI models harnesses can use, and the endpoints they are
// reached at. Picking a model in a harness copies it in, so a harness
// runs the same on every runner; this page only saves typing. No API
// keys here: an endpoint names a credential, which lives on the runners
// (Credentials page).

// Kinds of endpoint, with what a newcomer would otherwise look up.
const KINDS = [
  { id: "anthropic", label: "Anthropic API", url: "https://api.anthropic.com", credential: "anthropic", name: "anthropic" },
  { id: "network", label: "Model server on your network (oMLX, LM Studio, LiteLLM…)", url: "", placeholder: "http://mac-studio.lan:4000", credential: "", name: "" },
  { id: "other", label: "Another service that speaks Anthropic's API", url: "", placeholder: "https://llm.example.com", credential: "", name: "" },
];
const kindLabel = (id) => (KINDS.find((k) => k.id === id) || { label: id }).label;

// Model IDs offered for Anthropic's API (any other ID can be typed).
const ANTHROPIC_MODELS = ["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-2"];

const fmtTokens = (n) => (n ? Number(n).toLocaleString("en-US") : "");

export async function modelsView(ui) {
  const { h, api, set, show, fail } = ui;
  const body = h("div", { class: "stack" }, h("p", { class: "muted" }, "Loading…"));
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Models"),
      h("p", { class: "muted" }, "The AI models your harnesses can use, and where they are reached. When you pick one in a harness, its settings are filled in for you. API keys are not kept here: an endpoint only names a credential, and the key stays on the runners (Credentials)."))),
    body);
  let view;
  try {
    view = await api("GET", "/api/models");
  } catch (err) {
    fail(err);
    return;
  }
  const credentialNames = await knownCredentials(api);
  const page = { ui, view, credentialNames, render: null };
  page.render = () => set(body, endpointsCard(page), modelsCard(page));
  page.render();
}

// knownCredentials are the credential names on any runner, for
// suggestions (a runner that does not answer is left out).
async function knownCredentials(api) {
  const names = new Set();
  try {
    const runners = await api("GET", "/api/runners?status=false");
    await Promise.all(runners.map((r) => api("GET", "/api/runners/" + encodeURIComponent(r.name) + "/credentials")
      .then((l) => l.credentials.forEach((c) => names.add(c.name)))
      .catch(() => {})));
  } catch {
    // Suggestions only.
  }
  return [...names].sort();
}

// save sends the whole catalog; on success the page shows what was saved.
async function save(page, next, problem) {
  const { h, api, set } = page.ui;
  try {
    page.view = await api("PUT", "/api/models", next);
    page.render();
    return true;
  } catch (err) {
    const errs = (err.data && err.data.errors) || [err.message];
    set(problem, h("div", { class: "error-box" }, h("ul", { class: "explain errors" }, errs.map((e) => h("li", {}, e)))));
    return false;
  }
}

const copy = (v) => ({ endpoints: v.endpoints.map((e) => ({ ...e })), models: v.models.map((m) => ({ ...m })) });

function endpointsCard(page) {
  const { h } = page.ui;
  const v = page.view;
  const rows = v.endpoints.map((e, i) => h("tr", {},
    h("td", {}, h("strong", {}, e.name)),
    h("td", {}, kindLabel(e.kind)),
    h("td", {}, e.url, e.url.startsWith("http://") && h("span", { class: "badge failed" }, "unencrypted")),
    h("td", {}, e.credential || h("span", { class: "muted" }, "none")),
    h("td", { class: "actions" },
      h("button", { onclick: () => editEndpoint(page, i) }, "Change"),
      h("button", { onclick: () => removeEndpoint(page, i) }, "Remove"))));
  return h("section", { class: "card" },
    h("div", { class: "card-head" }, h("h2", {}, "Endpoints"),
      h("button", { class: "primary", onclick: () => editEndpoint(page, -1) }, "Add an endpoint")),
    h("p", { class: "muted" }, "Where models are reached: Anthropic's API, or a model server that speaks it."),
    rows.length === 0
      ? h("p", { class: "muted" }, "No endpoints yet. Add one first, then its models.")
      : h("div", { class: "table-wrap" }, h("table", {},
        h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Kind"), h("th", {}, "Address"), h("th", {}, "Credential"), h("th", {}, ""))),
        h("tbody", {}, rows))));
}

function modelsCard(page) {
  const { h } = page.ui;
  const v = page.view;
  const rows = v.models.map((m, i) => {
    const extras = [
      m.contextWindow && "window " + fmtTokens(m.contextWindow),
      m.smallModel && "side tasks: " + m.smallModel,
      m.maxOutputTokens && "answers ≤ " + fmtTokens(m.maxOutputTokens),
      m.thinkingTokens && "thinking ≤ " + fmtTokens(m.thinkingTokens),
    ].filter(Boolean);
    const used = v.usedBy[i] || [];
    return h("tr", {},
      h("td", {}, h("strong", {}, m.label || m.id), m.label && h("div", { class: "muted" }, m.id)),
      h("td", {}, m.endpoint),
      h("td", {}, extras.length === 0 ? h("span", { class: "muted" }, "runtime defaults") : extras.join(", ")),
      h("td", {}, used.length === 0 ? h("span", { class: "muted" }, "no harness") : used.flatMap((n, j) =>
        [j > 0 ? ", " : null, h("a", { href: "#/harness/" + encodeURIComponent(n) }, n)])),
      h("td", { class: "actions" },
        h("button", { onclick: () => editModel(page, i) }, "Change"),
        h("button", { onclick: () => removeModel(page, i) }, "Remove")));
  });
  return h("section", { class: "card" },
    h("div", { class: "card-head" }, h("h2", {}, "Models"),
      h("button", { class: "primary", disabled: v.endpoints.length === 0, onclick: () => editModel(page, -1) }, "Add a model")),
    rows.length === 0
      ? h("p", { class: "muted" }, v.endpoints.length === 0 ? "Add an endpoint first." : "No models yet.")
      : h("div", { class: "table-wrap" }, h("table", {},
        h("thead", {}, h("tr", {}, h("th", {}, "Model"), h("th", {}, "Endpoint"), h("th", {}, "Settings"), h("th", {}, "Used by"), h("th", {}, ""))),
        h("tbody", {}, rows))));
}

function dialogForm(page, title, fields, onSave) {
  const { h, set } = page.ui;
  const dialog = document.getElementById("dialog");
  const problem = h("div", {});
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    if (await onSave(problem)) dialog.close();
  } },
  h("h1", {}, title),
  h("div", { class: "form-stack" }, ...fields),
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Save"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

function editEndpoint(page, index) {
  const { h, say } = page.ui;
  const existing = index >= 0 ? page.view.endpoints[index] : null;
  const help = (t) => h("span", { class: "muted note" }, t);
  const kind = h("select", {}, KINDS.map((k) => h("option", { value: k.id }, k.label)));
  const name = h("input", { required: true, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", placeholder: "anthropic", autocomplete: "off" });
  const url = h("input", { required: true, type: "url", autocomplete: "off" });
  const listID = "credential-names";
  const credential = h("input", { list: listID, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", autocomplete: "off", placeholder: "none" });
  const names = h("datalist", { id: listID }, page.credentialNames.map((n) => h("option", { value: n })));
  const warn = h("div", { class: "error-box", hidden: true }, "Unencrypted: prompts, code and the credential cross your network in clear text. Use http:// only for a model server in your own network.");
  const sync = () => { warn.hidden = !url.value.startsWith("http://"); };
  const fill = (k) => {
    if (!existing) {
      url.value = k.url;
      credential.value = k.credential;
      if (k.name && !name.value) name.value = k.name;
    }
    url.placeholder = k.placeholder || k.url;
    sync();
  };
  kind.addEventListener("change", () => fill(KINDS.find((k) => k.id === kind.value)));
  url.addEventListener("input", sync);
  if (existing) {
    kind.value = existing.kind;
    name.value = existing.name;
    url.value = existing.url;
    credential.value = existing.credential || "";
    fill(KINDS.find((k) => k.id === existing.kind) || KINDS[0]);
  } else {
    fill(KINDS[0]);
  }
  dialogForm(page, existing ? "Change " + existing.name : "Add an endpoint", [
    h("label", {}, "What is it?", kind),
    h("label", {}, "Name", name, help("How models and harnesses refer to it.")),
    h("label", {}, "Address", url, help("The API's base address, without a path to a single call.")),
    warn,
    h("label", {}, "Credential", credential, names,
      help("The name of the API key on your runners (Credentials page). Harnesses using this endpoint are granted it.")),
  ], async (problem) => {
    const next = copy(page.view);
    const e = { name: name.value.trim(), kind: kind.value, url: url.value.trim() };
    if (credential.value.trim()) e.credential = credential.value.trim();
    if (existing) {
      next.endpoints[index] = e;
      // Its models follow a rename.
      next.models.forEach((m) => { if (m.endpoint === existing.name) m.endpoint = e.name; });
    } else {
      next.endpoints.push(e);
    }
    const ok = await save(page, next, problem);
    if (ok) say("Saved the endpoint " + e.name + ".");
    return ok;
  });
}

function removeEndpoint(page, index) {
  const { say, fail } = page.ui;
  const e = page.view.endpoints[index];
  const users = page.view.models.filter((m) => m.endpoint === e.name);
  if (users.length > 0) {
    fail(new Error(e.name + " is used by " + users.map((m) => m.label || m.id).join(", ") + ". Remove those models first."));
    return;
  }
  if (!confirm("Remove the endpoint " + e.name + "?")) return;
  const next = copy(page.view);
  next.endpoints.splice(index, 1);
  const problem = page.ui.h("div", {});
  save(page, next, problem).then((ok) => ok ? say("Removed " + e.name + ".") : fail(new Error(problem.textContent)));
}

function editModel(page, index) {
  const { h, say } = page.ui;
  const existing = index >= 0 ? page.view.models[index] : null;
  const help = (t) => h("span", { class: "muted note" }, t);
  const endpoint = h("select", { required: true }, page.view.endpoints.map((e) => h("option", { value: e.name }, e.name + " (" + e.url + ")")));
  const idList = h("datalist", { id: "model-ids" });
  const id = h("input", { required: true, list: "model-ids", autocomplete: "off", pattern: "[A-Za-z0-9][A-Za-z0-9._:\\/\\-]{0,127}", placeholder: "claude-sonnet-5-5" });
  const label = h("input", { autocomplete: "off", maxlength: "80", placeholder: "e.g. Claude Sonnet (optional)" });
  const num = (min, max, placeholder) => h("input", { type: "number", min: String(min), max: String(max), step: "1", placeholder });
  const window_ = num(8192, 1000000, "runtime default");
  const small = h("input", { list: "model-ids", autocomplete: "off", pattern: "[A-Za-z0-9][A-Za-z0-9._:\\/\\-]{0,127}", placeholder: "the same model" });
  const maxOut = num(1024, 262144, "runtime default");
  const think = num(1024, 262144, "no extended thinking");
  const suggest = () => {
    const e = page.view.endpoints.find((x) => x.name === endpoint.value);
    idList.replaceChildren(...(e && e.kind === "anthropic" ? ANTHROPIC_MODELS : []).map((m) => h("option", { value: m })));
  };
  endpoint.addEventListener("change", suggest);
  if (existing) {
    endpoint.value = existing.endpoint;
    id.value = existing.id;
    label.value = existing.label || "";
    window_.value = existing.contextWindow || "";
    small.value = existing.smallModel || "";
    maxOut.value = existing.maxOutputTokens || "";
    think.value = existing.thinkingTokens || "";
  }
  suggest();
  const advanced = h("details", { open: !!(existing && (existing.smallModel || existing.maxOutputTokens || existing.thinkingTokens)) },
    h("summary", {}, "More settings (optional)"),
    h("div", { class: "form-stack" },
      h("label", {}, "Model for quick side tasks", small, help("A faster, cheaper model at the same endpoint for summaries and titles. Empty: the model itself.")),
      h("div", { class: "row" },
        h("label", {}, "Longest answer (tokens)", maxOut, help("1,024 to 262,144")),
        h("label", {}, "Thinking per answer (tokens)", think, help("Must be less than the longest answer.")))));
  const n = (input) => (input.value === "" ? undefined : Number(input.value));
  dialogForm(page, existing ? "Change " + (existing.label || existing.id) : "Add a model", [
    h("label", {}, "Endpoint", endpoint),
    h("label", {}, "Model ID", id, idList, help("Exactly as the endpoint names it.")),
    h("label", {}, "Name shown", label),
    h("label", {}, "Context window (tokens)", window_, help("Only for a model Claude Code does not know (it then assumes 200,000).")),
    advanced,
  ], async (problem) => {
    const next = copy(page.view);
    const m = { id: id.value.trim(), endpoint: endpoint.value };
    if (label.value.trim()) m.label = label.value.trim();
    if (small.value.trim()) m.smallModel = small.value.trim();
    for (const [k, input] of [["contextWindow", window_], ["maxOutputTokens", maxOut], ["thinkingTokens", think]]) {
      if (n(input) !== undefined) m[k] = n(input);
    }
    if (existing) next.models[index] = m;
    else next.models.push(m);
    const ok = await save(page, next, problem);
    if (ok) say("Saved the model " + (m.label || m.id) + "." + (existing && (page.view.usedBy[index] || []).length > 0
      ? " Harnesses that use it keep their copy until you change them." : ""));
    return ok;
  });
}

function removeModel(page, index) {
  const { say, fail } = page.ui;
  const m = page.view.models[index];
  const used = page.view.usedBy[index] || [];
  if (!confirm("Remove " + (m.label || m.id) + "?" + (used.length > 0 ? " The harnesses that use it (" + used.join(", ") + ") keep working: they have their own copy." : ""))) return;
  const next = copy(page.view);
  next.models.splice(index, 1);
  const problem = page.ui.h("div", {});
  save(page, next, problem).then((ok) => ok ? say("Removed " + (m.label || m.id) + ".") : fail(new Error(problem.textContent)));
}
