// Credentials (ADR-0017): API keys the runners' credential brokers add
// to the agents' requests. The agents never see them. A secret is
// write-only: typed here once, never shown again, by this page or the
// runner. Changes are made on the runner, as the owner of its store; on
// a server that is sudo over ssh, which may ask for the password.

// Presets fill in what most people would have to look up.
const KINDS = [
  { id: "anthropic", label: "Anthropic API key", hosts: "api.anthropic.com", scheme: "https", port: 443, header: "x-api-key", prefix: "" },
  { id: "bearer", label: "OpenAI-compatible API key (Bearer token)", hosts: "api.openai.com", scheme: "https", port: 443, header: "Authorization", prefix: "Bearer " },
  { id: "local", label: "Model server on your network (oMLX, Ollama, LM Studio…)", hosts: "", scheme: "http", port: 4000, header: "Authorization", prefix: "Bearer " },
  { id: "custom", label: "Something else (my own header)", hosts: "", scheme: "https", port: 443, header: "", prefix: "" },
];

export async function credentialsView(ui) {
  const { h, api, set, show, fail } = ui;
  const list = h("div", { class: "stack" });
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Credentials"),
      h("p", { class: "muted" }, "API keys your agents may use, kept on each runner. Agents never see them: the runner adds a key to an agent's requests, and only to the hosts you name here, and only for harnesses that are granted it."))),
    list);
  let runners;
  try {
    runners = await api("GET", "/api/runners?status=false");
  } catch (err) {
    fail(err);
    return;
  }
  if (runners.length === 0) {
    set(list, h("div", { class: "empty" }, "No runners yet. Add one on the Runners page first."));
    return;
  }
  set(list, ...runners.map((r) => runnerSection(ui, r)));
}

function runnerSection(ui, r) {
  const { h, api, set } = ui;
  const body = h("div", {}, h("p", { class: "muted" }, "Asking the runner…"));
  const section = h("section", { class: "card" },
    h("div", { class: "card-head" }, h("h2", {}, r.name),
      h("button", { class: "primary", onclick: () => editCredential(ui, r, null, reload) }, "Add a credential")),
    body);
  const reload = async () => {
    let l;
    try {
      l = await api("GET", "/api/runners/" + encodeURIComponent(r.name) + "/credentials");
    } catch (err) {
      set(body, h("div", { class: "error-box" }, "Could not ask " + r.name + ": " + err.message));
      return;
    }
    const rows = l.credentials.map((c) => h("tr", {},
      h("td", {}, h("strong", {}, c.name)),
      h("td", {}, c.hosts.join(", "), c.scheme === "http" && h("span", { class: "badge failed" }, "unencrypted")),
      h("td", {}, c.header + (c.prefix ? " (" + c.prefix.trim() + ")" : "")),
      h("td", { class: "actions" },
        h("button", { onclick: () => editCredential(ui, r, c, reload) }, "Change"),
        h("button", { onclick: () => removeCredential(ui, r, c, reload) }, "Remove"))));
    set(body,
      l.error && h("div", { class: "error-box" }, "The runner cannot use its store: " + l.error),
      rows.length === 0
        ? h("p", { class: "muted" }, "No credentials on " + r.name + " yet.")
        : h("div", { class: "table-wrap" }, h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Sent to"), h("th", {}, "Header"), h("th", {}, ""))), h("tbody", {}, rows))));
  };
  reload();
  return section;
}

// send makes a change; if the runner's host wants the sudo password, it
// asks for it (in the form) and tries again.
async function send(ui, r, method, name, body, problem, ask) {
  const { api } = ui;
  try {
    await api(method, "/api/runners/" + encodeURIComponent(r.name) + "/credentials/" + encodeURIComponent(name), body);
    return true;
  } catch (err) {
    if (err.data && err.data.needPassword) {
      ask(err.message);
      return false;
    }
    problem(err.message);
    return false;
  }
}

function passwordField(ui, r) {
  const { h } = ui;
  const input = h("input", { type: "password", autocomplete: "off" });
  const box = h("div", { class: "notice-box", hidden: true },
    h("p", {}, "To change credentials on " + r.name + ", the server needs your password (sudo on " + r.ssh + "). It is used for this change only and is not stored."),
    h("label", {}, "Your password on " + r.ssh, input));
  return {
    box,
    show(msg) {
      box.hidden = false;
      box.querySelector("p").textContent = msg + ". It is used for this change only and is not stored.";
      input.focus();
    },
    value() {
      return box.hidden || input.value === "" ? undefined : input.value;
    },
    clear() {
      input.value = "";
    },
  };
}

function editCredential(ui, r, existing, done) {
  const { h, set, say } = ui;
  const dialog = document.getElementById("dialog");
  const kind = h("select", {}, KINDS.map((k) => h("option", { value: k.id }, k.label)));
  const name = h("input", { required: true, pattern: "[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?", placeholder: "anthropic", autocomplete: "off" });
  const hosts = h("input", { required: true, placeholder: "api.anthropic.com", autocomplete: "off" });
  const scheme = h("select", {}, h("option", { value: "https" }, "https (encrypted)"), h("option", { value: "http" }, "http (unencrypted)"));
  const port = h("input", { type: "number", min: "1", max: "65535", required: true });
  const header = h("input", { required: true, placeholder: "x-api-key", autocomplete: "off" });
  const prefix = h("input", { placeholder: "e.g. Bearer (with the space)", autocomplete: "off" });
  const secret = h("input", { type: "password", autocomplete: "new-password", required: !existing,
    placeholder: existing ? "leave empty to keep the current key" : "paste the key" });
  const warn = h("div", { class: "error-box", hidden: true }, "Unencrypted: the key crosses your network in clear text. Use this only for a model server in your own network.");
  const problem = h("div", {});
  const pw = passwordField(ui, r);
  const fill = (k) => {
    hosts.value = k.hosts;
    scheme.value = k.scheme;
    port.value = String(k.port);
    header.value = k.header;
    prefix.value = k.prefix;
    if (!existing && !name.value) name.value = k.id === "custom" || k.id === "local" ? "" : k.id === "bearer" ? "openai" : k.id;
    sync();
  };
  const sync = () => {
    warn.hidden = scheme.value !== "http";
  };
  kind.addEventListener("change", () => fill(KINDS.find((k) => k.id === kind.value)));
  scheme.addEventListener("change", () => {
    if (scheme.value === "http" && port.value === "443") port.value = "80";
    if (scheme.value === "https" && port.value === "80") port.value = "443";
    sync();
  });
  if (existing) {
    name.value = existing.name;
    name.disabled = true;
    hosts.value = existing.hosts.join(", ");
    scheme.value = existing.scheme;
    port.value = String(existing.port);
    header.value = existing.header;
    prefix.value = existing.prefix || "";
    kind.value = "custom";
    sync();
  } else {
    fill(KINDS[0]);
  }
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const body = {
      hosts: hosts.value.split(/[\s,]+/).filter(Boolean),
      scheme: scheme.value,
      port: Number(port.value),
      header: header.value.trim(),
      prefix: prefix.value,
    };
    if (secret.value !== "") body.secret = secret.value;
    const p = pw.value();
    if (p !== undefined) body.sudoPassword = p;
    const ok = await send(ui, r, "PUT", name.value, body, (msg) => set(problem, h("div", { class: "error-box" }, msg)), (msg) => pw.show(msg));
    pw.clear();
    if (!ok) return;
    secret.value = "";
    dialog.close();
    say("Saved " + name.value + " on " + r.name + ". Jobs that start from now on can use it.");
    done();
  } },
  h("h1", {}, existing ? "Change " + existing.name : "Add a credential to " + r.name),
  h("div", { class: "form-stack" },
    !existing && h("label", {}, "What is it?", kind),
    h("label", {}, "Name", name, h("span", { class: "muted note" }, "Harnesses ask for it by this name, in permissions.credentials.")),
    h("label", {}, "Sent only to", hosts, h("span", { class: "muted note" }, "Exact host names, separated by commas. Never to any other host.")),
    h("div", { class: "row" }, h("label", {}, "Connection", scheme), h("label", {}, "Port", port)),
    warn,
    h("div", { class: "row" }, h("label", {}, "Header", header), h("label", {}, "Before the key", prefix)),
    h("label", {}, "Key", secret, h("span", { class: "muted note" }, "Write-only: nobody can see it again here, not even you."))),
  pw.box,
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Save"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

function removeCredential(ui, r, c, done) {
  const { h, set, say } = ui;
  const dialog = document.getElementById("dialog");
  const problem = h("div", {});
  const pw = passwordField(ui, r);
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const body = {};
    const p = pw.value();
    if (p !== undefined) body.sudoPassword = p;
    const ok = await send(ui, r, "DELETE", c.name, body, (msg) => set(problem, h("div", { class: "error-box" }, msg)), (msg) => pw.show(msg));
    pw.clear();
    if (!ok) return;
    dialog.close();
    say("Removed " + c.name + " from " + r.name + ".");
    done();
  } },
  h("h1", {}, "Remove " + c.name + "?"),
  h("p", {}, "Harnesses that use " + c.name + " on " + r.name + " will be refused until you add it again."),
  pw.box,
  problem,
  h("div", { class: "actions" }, h("button", { class: "danger" }, "Remove"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}
