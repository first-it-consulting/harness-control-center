// Model providers: where your agents' models run (Anthropic, Ollama,
// oMLX, LiteLLM, LM Studio, OpenRouter …). A harness picks a provider and
// one of its models; the Control Center asks the provider for its models
// with the provider's credential from the Credentials page.

const TYPES = [
  { id: "anthropic", label: "Anthropic", url: "https://api.anthropic.com", placeholder: "", credential: "anthropic", local: false,
    note: "Claude models, with an Anthropic API key." },
  { id: "ollama", label: "Ollama", url: "", placeholder: "http://my-ollama.lan:11434", credential: "", local: true,
    note: "Ollama 0.14 or newer speaks Anthropic's API. Its address usually ends in :11434." },
  { id: "omlx", label: "oMLX", url: "", placeholder: "http://mac-studio.lan:4000", credential: "", local: true,
    note: "An oMLX server on a Mac in your network." },
  { id: "litellm", label: "LiteLLM", url: "", placeholder: "http://litellm.lan:4000", credential: "", local: true,
    note: "A LiteLLM proxy: one address for many models (its /v1/messages)." },
  { id: "lmstudio", label: "LM Studio", url: "", placeholder: "http://my-mac.lan:1234", credential: "", local: true,
    note: "LM Studio's server, with its Anthropic-compatible API turned on." },
  { id: "openrouter", label: "OpenRouter", url: "https://openrouter.ai/api", placeholder: "", credential: "openrouter", local: false,
    note: "Many models through one OpenRouter key (sent as Authorization: Bearer)." },
  { id: "other", label: "Something else", url: "", placeholder: "https://llm.example.com", credential: "", local: false,
    note: "Any server that speaks Anthropic's Messages API." },
];
const typeOf = (id) => TYPES.find((t) => t.id === id) || TYPES[TYPES.length - 1];

export async function modelsView(ui) {
  const { h, api, set, show, fail } = ui;
  const list = h("div", { class: "grid" }, h("p", { class: "muted" }, "Loading…"));
  const page = { ui, providers: [], credentials: [], render: null };
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Model providers"),
      h("p", { class: "muted" }, "Where your agents' models run. Add a provider once; a harness then picks it and one of its models. The agent (Claude Code) talks Anthropic's Messages API, so the provider must offer it: Anthropic, Ollama, oMLX, LiteLLM, LM Studio and OpenRouter do.")),
      h("div", { class: "actions" }, h("button", { class: "primary", onclick: () => editProvider(page, -1) }, "Add a provider"))),
    list);
  page.render = () => set(list, ...(page.providers.length === 0
    ? [h("div", { class: "empty" }, "No providers yet. Add the service your models run at.")]
    : page.providers.map((p, i) => providerCard(page, p, i))));
  try {
    const [pv, creds] = await Promise.all([api("GET", "/api/providers"), api("GET", "/api/credentials").catch(() => ({ credentials: [] }))]);
    page.providers = pv.providers;
    page.credentials = creds.credentials;
  } catch (err) {
    fail(err);
    return;
  }
  page.render();
}

function providerCard(page, p, index) {
  const { h, api, set } = page.ui;
  const models = h("div", {});
  const show = async () => {
    set(models, h("p", { class: "muted" }, "Asking " + p.name + "…"));
    try {
      const r = await api("POST", "/api/providers/" + encodeURIComponent(p.name) + "/models");
      set(models, r.models.length === 0 ? h("p", { class: "muted" }, "It offers no models.")
        : h("ul", { class: "chips" }, r.models.map((m) => h("li", { title: m.label || "" }, m.id))));
    } catch (err) {
      set(models, h("div", { class: "error-box" }, err.message));
    }
  };
  return h("div", { class: "card" },
    h("h3", {}, p.name, " ", h("span", { class: "badge" }, typeOf(p.type).label)),
    h("p", { class: "muted" }, p.url, p.url.startsWith("http://") && " ", p.url.startsWith("http://") && page.ui.badge("unencrypted", "failed")),
    h("dl", { class: "facts" },
      h("dt", {}, "Where"), h("dd", {}, p.private ? "In your own network" : "On the internet"),
      h("dt", {}, "Credential"), h("dd", {}, p.credential || "none"),
      h("dt", {}, "Used by"), h("dd", {}, p.usedBy.length === 0 ? "no harness" : p.usedBy.flatMap((n, j) =>
        [j > 0 ? ", " : null, h("a", { href: "#/harness/" + encodeURIComponent(n) }, n)]))),
    models,
    h("div", { class: "actions" },
      h("button", { onclick: show }, "Show its models"),
      h("button", { onclick: () => editProvider(page, index) }, "Change"),
      h("button", { onclick: () => removeProvider(page, index) }, "Remove")));
}

async function save(page, next, problem) {
  const { h, api, set } = page.ui;
  try {
    page.providers = (await api("PUT", "/api/providers", { providers: next })).providers;
    page.render();
    return true;
  } catch (err) {
    const errs = (err.data && err.data.errors) || [err.message];
    set(problem, h("div", { class: "error-box" }, h("ul", { class: "explain errors" }, errs.map((e) => h("li", {}, e)))));
    return false;
  }
}

const plain = (p) => {
  const out = { name: p.name, type: p.type, url: p.url };
  if (p.credential) out.credential = p.credential;
  if (p.local) out.local = true;
  return out;
};

function editProvider(page, index) {
  const { h, set, say } = page.ui;
  const dialog = document.getElementById("dialog");
  const existing = index >= 0 ? page.providers[index] : null;
  const help = (t) => h("span", { class: "muted note" }, t);
  const type = h("select", {}, TYPES.map((t) => h("option", { value: t.id }, t.label)));
  const note = help("");
  const name = h("input", { required: true, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", autocomplete: "off" });
  const url = h("input", { required: true, type: "url", autocomplete: "off" });
  const local = h("input", { type: "checkbox" });
  const credNames = page.credentials.map((c) => c.name);
  const credential = h("select", {}, [h("option", { value: "" }, "none"), ...credNames.map((n) => h("option", { value: n }, n))]);
  const warn = h("div", { class: "error-box", hidden: true }, "Unencrypted: prompts, code and the key cross the network in clear text. Use http:// only for a server in your own network.");
  const credHint = h("p", { class: "muted" });
  const problem = h("div", {});
  const sync = () => {
    warn.hidden = !url.value.startsWith("http://");
    const t = typeOf(type.value);
    note.textContent = t.note;
    credHint.textContent = t.credential && !credNames.includes(credential.value) && !credNames.includes(t.credential)
      ? "It needs a key: add it on the Credentials page (e.g. named " + t.credential + "), then choose it here." : "";
  };
  const fill = (t) => {
    url.placeholder = t.placeholder || t.url;
    if (!existing) {
      url.value = t.url;
      local.checked = t.local;
      if (!name.value || TYPES.some((x) => x.id === name.value)) name.value = t.id;
      credential.value = credNames.includes(t.credential) ? t.credential : "";
    }
    sync();
  };
  type.addEventListener("change", () => fill(typeOf(type.value)));
  url.addEventListener("input", sync);
  credential.addEventListener("change", sync);
  if (existing) {
    type.value = existing.type;
    name.value = existing.name;
    url.value = existing.url;
    local.checked = !!existing.local;
    if (existing.credential && !credNames.includes(existing.credential)) {
      credential.append(h("option", { value: existing.credential }, existing.credential + " (not on the Credentials page)"));
    }
    credential.value = existing.credential || "";
  }
  fill(typeOf(type.value));
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const p = { name: name.value.trim(), type: type.value, url: url.value.trim().replace(/\/+$/, ""), credential: credential.value, local: local.checked };
    const next = page.providers.map(plain);
    if (existing) next[index] = plain(p);
    else next.push(plain(p));
    if (await save(page, next, problem)) {
      dialog.close();
      say("Saved the provider " + p.name + ". \"Show its models\" checks that it answers.");
    }
  } },
  h("h1", {}, existing ? "Change " + existing.name : "Add a provider"),
  h("div", { class: "form-stack" },
    h("label", {}, "What is it?", type, note),
    h("label", {}, "Name", name, help("How harnesses refer to it.")),
    h("label", {}, "Address", url, help("Its base address, without /v1/…")),
    warn,
    h("label", { class: "check" }, local, "It is in my own network (at home or at work)"),
    help("Then a harness using it may reach your own network for this one address. Names like *.lan count as your own network anyway."),
    h("label", {}, "Credential", credential, help("The API key it needs, from the Credentials page. Model servers at home often need none.")),
    credHint),
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Save"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

function removeProvider(page, index) {
  const { h, say, fail } = page.ui;
  const p = page.providers[index];
  if (!confirm("Remove the provider " + p.name + "?" + (p.usedBy.length > 0 ? " The harnesses that use it (" + p.usedBy.join(", ") + ") keep working: they have their own copy of the model." : ""))) return;
  const problem = h("div", {});
  save(page, page.providers.filter((_, i) => i !== index).map(plain), problem)
    .then((ok) => (ok ? say("Removed " + p.name + ".") : fail(new Error(problem.textContent))));
}
