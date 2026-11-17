// Models: the connections to the services models run at (Anthropic,
// Ollama, oMLX, LiteLLM …), laid out like Open WebUI's Connections, and
// the models they offer. A connection's key is typed right in its dialog
// and kept as a secret that goes only to that connection's address; the
// agent never sees it. Claude Code speaks Anthropic's Messages API, so a
// connection must offer it.

const TYPES = [
  { id: "anthropic", label: "Anthropic", url: "https://api.anthropic.com", placeholder: "https://api.anthropic.com", auth: "apikey", local: false },
  { id: "ollama", label: "Ollama", url: "", placeholder: "http://my-ollama.lan:11434", auth: "none", local: true },
  { id: "omlx", label: "oMLX", url: "", placeholder: "http://mac-studio.lan:4000", auth: "apikey", local: true },
  { id: "litellm", label: "LiteLLM", url: "", placeholder: "http://litellm.lan:4000", auth: "bearer", local: true },
  { id: "lmstudio", label: "LM Studio", url: "", placeholder: "http://my-mac.lan:1234", auth: "none", local: true },
  { id: "openrouter", label: "OpenRouter", url: "https://openrouter.ai/api", placeholder: "https://openrouter.ai/api", auth: "bearer", local: false },
  { id: "other", label: "Other (Anthropic-compatible)", url: "", placeholder: "https://llm.example.com", auth: "bearer", local: false },
];
const typeOf = (id) => TYPES.find((t) => t.id === id) || TYPES[TYPES.length - 1];

// AUTH: how the key travels.
const AUTH = {
  none: { label: "None", header: "", prefix: "" },
  bearer: { label: "Bearer", header: "Authorization", prefix: "Bearer " },
  apikey: { label: "API key (x-api-key)", header: "x-api-key", prefix: "" },
  custom: { label: "Custom header", header: "", prefix: "" },
};
const authOf = (c) => {
  if (!c) return "none";
  if (c.header.toLowerCase() === "authorization" && (c.prefix || "").trim().toLowerCase() === "bearer") return "bearer";
  if (c.header.toLowerCase() === "x-api-key" && !c.prefix) return "apikey";
  return "custom";
};

export async function modelsView(ui) {
  const { h, api, set, show, fail } = ui;
  const page = { ui, providers: [], credentials: [] };
  const connections = h("section", { class: "card" });
  const models = h("section", { class: "card" });
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Models"),
      h("p", { class: "muted" }, "The connections your agents' models are reached through, and the models they offer. The agent (Claude Code) talks Anthropic's Messages API: Anthropic, Ollama, oMLX, LiteLLM, LM Studio and OpenRouter offer it."))),
    h("div", { class: "stack" }, connections, models));
  page.render = () => {
    set(connections,
      h("div", { class: "card-head" }, h("h2", {}, "Connections"),
        h("button", { class: "icon-button", title: "Add a connection", "aria-label": "Add a connection", onclick: () => editConnection(page, -1) }, "+")),
      page.providers.length === 0
        ? h("p", { class: "muted" }, "No connections yet. Add the service your models run at with +.")
        : h("div", { class: "list" }, page.providers.map((p, i) => connectionRow(page, p, i))));
    loadModels(page, models);
  };
  page.reload = async () => {
    try {
      const [pv, creds] = await Promise.all([api("GET", "/api/providers"), api("GET", "/api/credentials").catch(() => ({ credentials: [] }))]);
      page.providers = pv.providers;
      page.credentials = creds.credentials;
    } catch (err) {
      fail(err);
      return;
    }
    page.render();
  };
  await page.reload();
}

function toggle(ui, on, onChange, label) {
  const { h } = ui;
  const box = h("input", { type: "checkbox", class: "switch", checked: on, "aria-label": label });
  box.addEventListener("change", () => onChange(box.checked));
  return box;
}

function connectionRow(page, p, index) {
  const { h, badge } = page.ui;
  const t = typeOf(p.type);
  return h("div", { class: "list-row" + (p.disabled ? " off" : "") },
    h("div", { class: "list-main" },
      h("div", { class: "mono" }, p.url),
      h("div", { class: "muted small" }, [p.name, t.label, p.private ? "in your network" : null, p.credential ? "key: " + p.credential : "no key",
        p.usedBy.length > 0 ? "used by " + p.usedBy.length + " harness" + (p.usedBy.length > 1 ? "es" : "") : null].filter(Boolean).join(" · "),
      p.url.startsWith("http://") && [" ", badge("unencrypted", "failed")])),
    h("button", { class: "icon-button", title: "Edit", "aria-label": "Edit " + p.name, onclick: () => editConnection(page, index) }, "⚙"),
    toggle(page.ui, !p.disabled, (on) => {
      const next = page.providers.map(plain);
      next[index].disabled = !on || undefined;
      save(page, next).then((err) => err && page.ui.fail(err));
    }, (p.disabled ? "Enable " : "Disable ") + p.name));
}

// loadModels lists the models of every enabled connection.
async function loadModels(page, box) {
  const { h, api, set } = page.ui;
  const on = page.providers.filter((p) => !p.disabled);
  set(box, h("div", { class: "card-head" }, h("h2", {}, "Models")),
    on.length === 0 ? h("p", { class: "muted" }, "Models appear here once a connection is on.") : h("p", { class: "muted" }, "Asking the connections…"));
  if (on.length === 0) return;
  const results = await Promise.all(on.map((p) => api("POST", "/api/providers/" + encodeURIComponent(p.name) + "/models")
    .then((r) => ({ p, models: r.models }), (err) => ({ p, error: err.message }))));
  const rows = [];
  for (const r of results) {
    if (r.error) {
      rows.push(h("div", { class: "list-row" }, h("div", { class: "list-main" }, h("strong", {}, r.p.name), h("div", { class: "error-text small" }, r.error))));
      continue;
    }
    for (const m of r.models) {
      rows.push(h("div", { class: "list-row" }, h("div", { class: "list-main" }, h("span", { class: "mono" }, m.id), m.label && h("span", { class: "muted small" }, "  " + m.label)),
        h("div", { class: "muted small" }, r.p.name)));
    }
  }
  set(box, h("div", { class: "card-head" }, h("h2", {}, "Models")),
    rows.length === 0 ? h("p", { class: "muted" }, "The connections offer no models.") : h("div", { class: "list" }, rows));
}

const plain = (p) => {
  const out = { name: p.name, type: p.type, url: p.url };
  if (p.credential) out.credential = p.credential;
  if (p.local) out.local = true;
  if (p.disabled) out.disabled = true;
  return out;
};

// save stores the connections; it returns an error, if any.
async function save(page, next) {
  try {
    page.providers = (await page.ui.api("PUT", "/api/providers", { providers: next })).providers;
    page.render();
    return null;
  } catch (err) {
    const errs = (err.data && err.data.errors) || [err.message];
    return new Error(errs.join("; "));
  }
}

function hostPort(url) {
  try {
    const u = new URL(url);
    return { scheme: u.protocol.replace(":", ""), host: u.hostname.toLowerCase(), port: Number(u.port) || (u.protocol === "http:" ? 80 : 443) };
  } catch {
    return null;
  }
}

function editConnection(page, index) {
  const { h, set, api, say } = page.ui;
  const dialog = document.getElementById("dialog");
  const existing = index >= 0 ? page.providers[index] : null;
  const cred = existing && page.credentials.find((c) => c.name === existing.credential);
  const type = h("select", {}, TYPES.map((t) => h("option", { value: t.id }, t.label)));
  const url = h("input", { required: true, type: "url", autocomplete: "off" });
  const enabled = toggle(page.ui, !existing || !existing.disabled, () => {}, "Enabled");
  const verifyOut = h("div", { class: "small" });
  const auth = h("select", {}, Object.entries(AUTH).map(([k, a]) => h("option", { value: k }, a.label)));
  const key = h("input", { type: "password", autocomplete: "new-password",
    placeholder: cred ? "•••••••• stored — leave empty to keep" : "the key" });
  const eye = h("button", { type: "button", class: "icon-button", title: "Show what you typed", onclick: () => { key.type = key.type === "password" ? "text" : "password"; } }, "👁");
  const customHeader = h("input", { autocomplete: "off", placeholder: "X-My-Key", pattern: "[A-Za-z0-9\\-]{1,64}" });
  const name = h("input", { required: true, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", autocomplete: "off" });
  const local = h("input", { type: "checkbox" });
  const problem = h("div", {});
  const keyRow = h("div", { class: "row" }, auth, key, eye);
  const sync = () => {
    key.hidden = eye.hidden = auth.value === "none";
    customHeader.closest && (customHeader.parentElement && (customHeader.parentElement.hidden = auth.value !== "custom"));
  };
  auth.addEventListener("change", sync);
  type.addEventListener("change", () => {
    const t = typeOf(type.value);
    url.placeholder = t.placeholder;
    if (!existing) {
      url.value = t.url;
      auth.value = t.auth;
      local.checked = t.local;
      if (!name.value || TYPES.some((x) => x.id === name.value)) name.value = t.id;
    }
    sync();
  });
  // The connection as typed, for checking and saving.
  const current = () => {
    const a = AUTH[auth.value];
    return {
      provider: { name: name.value.trim() || "test", type: type.value, url: url.value.trim().replace(/\/+$/, ""), local: local.checked },
      header: auth.value === "custom" ? customHeader.value.trim() : a.header,
      prefix: a.prefix,
    };
  };
  const verify = async () => {
    set(verifyOut, h("span", { class: "muted" }, "Checking…"));
    const c = current();
    const body = { ...c.provider };
    if (auth.value !== "none" && key.value !== "") body.auth = { header: c.header, prefix: c.prefix, key: key.value };
    else if (auth.value !== "none" && existing && existing.credential) body.credential = existing.credential;
    try {
      const r = await api("POST", "/api/providers/test", body);
      set(verifyOut, h("span", { class: "ok-text" }, "✓ Connected: " + r.models.length + " model" + (r.models.length === 1 ? "" : "s") +
        (r.models.length > 0 ? " (" + r.models.slice(0, 4).map((m) => m.id).join(", ") + (r.models.length > 4 ? ", …" : "") + ")" : "")));
    } catch (err) {
      set(verifyOut, h("span", { class: "error-text" }, "✗ " + err.message));
    }
  };
  if (existing) {
    type.value = existing.type;
    url.value = existing.url;
    name.value = existing.name;
    name.disabled = true;
    local.checked = !!existing.local;
    auth.value = existing.credential ? authOf(cred) : "none";
    if (cred && auth.value === "custom") customHeader.value = cred.header;
    url.placeholder = typeOf(existing.type).placeholder;
  } else {
    type.value = "anthropic";
    type.dispatchEvent(new Event("change"));
  }
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const c = current();
    const next = page.providers.map(plain);
    const p = { ...c.provider, credential: undefined, disabled: !enabled.checked || undefined };
    // The key: kept as a secret that goes only to this address.
    if (auth.value !== "none") {
      const hp = hostPort(p.url);
      const credName = (existing && existing.credential) || p.name;
      const needsKey = !cred || key.value !== "";
      if (needsKey && key.value === "") {
        set(problem, h("div", { class: "error-box" }, "Enter the key, or choose None."));
        return;
      }
      if (!hp || !c.header) {
        set(problem, h("div", { class: "error-box" }, "Check the address and the header."));
        return;
      }
      const body = { hosts: [hp.host], scheme: hp.scheme, port: hp.port, header: c.header, prefix: c.prefix };
      if (key.value !== "") body.secret = key.value;
      try {
        await api("PUT", "/api/credentials/" + encodeURIComponent(credName), body);
      } catch (err) {
        set(problem, h("div", { class: "error-box" }, err.message));
        return;
      } finally {
        key.value = "";
      }
      p.credential = credName;
    }
    if (existing) next[index] = plain(p);
    else next.push(plain(p));
    const err = await save(page, next);
    if (err) {
      set(problem, h("div", { class: "error-box" }, err.message));
      return;
    }
    dialog.close();
    say("Saved the connection " + p.name + ".");
    page.reload();
  } },
  h("div", { class: "card-head" }, h("h1", {}, existing ? "Edit connection" : "Add connection"),
    h("button", { type: "button", class: "icon-button", "aria-label": "Close", onclick: () => dialog.close() }, "✕")),
  h("div", { class: "form-stack" },
    h("label", {}, "Connection type", type),
    h("label", {}, "URL", h("div", { class: "row tight" }, url,
      h("button", { type: "button", class: "icon-button", title: "Check the connection", "aria-label": "Check the connection", onclick: verify }, "⟳"), enabled)),
    verifyOut,
    h("label", {}, "Auth", keyRow),
    h("span", { class: "muted note" }, "The key is kept as a secret and sent only to this address; agents never see it."),
    h("details", { open: !existing }, h("summary", {}, "Advanced"),
      h("div", { class: "form-stack" },
        h("label", {}, "Name", name, h("span", { class: "muted note" }, "How harnesses refer to this connection.")),
        h("label", { hidden: true }, "Header", customHeader),
        h("label", { class: "check" }, local, "It is in my own network (at home or at work)")))),
  problem,
  h("div", { class: "actions spread" },
    existing ? h("button", { type: "button", class: "danger", onclick: () => removeConnection(page, index) }, "Delete") : h("span", {}),
    h("button", { class: "primary" }, "Save")));
  set(dialog, form);
  sync();
  dialog.showModal();
}

async function removeConnection(page, index) {
  const { say, fail } = page.ui;
  const p = page.providers[index];
  if (!confirm("Delete the connection " + p.name + "?" + (p.usedBy.length > 0 ? " The harnesses that use it (" + p.usedBy.join(", ") + ") keep working: they have their own copy of the model." : ""))) return;
  const err = await save(page, page.providers.filter((_, i) => i !== index).map(plain));
  if (err) {
    fail(err);
    return;
  }
  document.getElementById("dialog").close();
  say("Deleted " + p.name + ". Its key stays under Secrets & variables.");
}
