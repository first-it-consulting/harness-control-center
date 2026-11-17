// Credentials (ADR-0019): API keys and tokens kept here, in the Control
// Center, and attached to harnesses. When a harness starts, the ones it
// is granted are copied to its runner, whose broker adds them to the
// agent's requests: the agent never sees them. A secret is write-only:
// typed here once, never shown again.

// Presets fill in what most people would have to look up.
const KINDS = [
  { id: "anthropic", label: "Anthropic API key", name: "anthropic", hosts: "api.anthropic.com", scheme: "https", port: 443, header: "x-api-key", prefix: "" },
  { id: "github", label: "GitHub token (GitHub API)", name: "github", hosts: "api.github.com", scheme: "https", port: 443, header: "Authorization", prefix: "Bearer " },
  { id: "gitlab", label: "GitLab token (GitLab API)", name: "gitlab", hosts: "gitlab.com", scheme: "https", port: 443, header: "PRIVATE-TOKEN", prefix: "" },
  { id: "bearer", label: "API token sent as \"Authorization: Bearer …\" (OpenAI, OpenRouter, LiteLLM…)", name: "", hosts: "", scheme: "https", port: 443, header: "Authorization", prefix: "Bearer " },
  { id: "local", label: "Model server on your network (oMLX, Ollama, LM Studio…)", name: "", hosts: "", scheme: "http", port: 4000, header: "Authorization", prefix: "Bearer " },
  { id: "custom", label: "Something else (my own header)", name: "", hosts: "", scheme: "https", port: 443, header: "", prefix: "" },
];

export async function credentialsView(ui) {
  const { h, set, show } = ui;
  const body = h("div", { class: "stack" }, h("p", { class: "muted" }, "Loading…"));
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Credentials"),
        h("p", { class: "muted" }, "API keys and tokens your agents may use. You keep them here and attach them to a harness; when it starts, they are copied to its runner, which adds them to the agent's requests to the hosts you name. The agent itself never sees them.")),
      h("div", { class: "actions" }, h("button", { class: "primary", onclick: () => editCredential(ui, null, reload) }, "Add a credential"))),
    body);
  const reload = async () => {
    let list;
    try {
      list = (await ui.api("GET", "/api/credentials")).credentials;
    } catch (err) {
      set(body, h("div", { class: "error-box" }, err.message));
      return;
    }
    set(body, storeCard(ui, list, reload), await runnerOnlyCard(ui, list));
  };
  await reload();
}

function storeCard(ui, list, reload) {
  const { h } = ui;
  const rows = list.map((c) => h("tr", {},
    h("td", {}, h("strong", {}, c.name)),
    c.kind === "secret"
      ? h("td", { colspan: 2 }, ui.badge("for the agent", "queued"), " ", h("span", { class: "muted" }, "the agent can read it"))
      : [h("td", {}, c.hosts.join(", "), c.scheme === "http" && h("span", { class: "badge failed" }, "unencrypted")),
        h("td", {}, c.header + (c.prefix ? " (" + c.prefix.trim() + ")" : ""))],
    h("td", {}, Object.keys(c.runners).length === 0 ? h("span", { class: "muted" }, "not yet")
      : Object.entries(c.runners).sort().map(([r, state]) => h("div", {}, r + " ",
        state === "current" ? ui.badge("current", "succeeded") : [ui.badge("older version", "queued"), " ",
          h("button", { onclick: () => copyNow(ui, c, r, reload) }, "Copy now")]))),
    h("td", { class: "actions" },
      h("button", { onclick: () => editCredential(ui, c, reload) }, "Change"),
      h("button", { onclick: () => removeCredential(ui, c, reload) }, "Remove"))));
  return h("section", { class: "card" },
    h("h2", {}, "Your credentials"),
    rows.length === 0
      ? h("p", { class: "muted" }, "None yet. Add one, then attach it to a harness under Credentials.")
      : h("div", { class: "table-wrap" }, h("table", {},
        h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Sent only to"), h("th", {}, "Header"), h("th", {}, "On runners"), h("th", {}, ""))),
        h("tbody", {}, rows))),
    h("p", { class: "muted" }, "Two kinds: for a web service (APIs, model servers), the runner adds the key to the agent's requests and the agent never sees it. A secret for the agent (a database password, a kubeconfig) is handed to the agent itself when its harness starts, as a variable or a file; the agent can read it, so such a harness is rated high risk."));
}

// runnerOnlyCard lists credentials that a runner has but this Control
// Center does not keep (added on the runner before).
async function runnerOnlyCard(ui, list) {
  const { h, api } = ui;
  const kept = new Set(list.map((c) => c.name));
  let runners = [];
  try {
    runners = await api("GET", "/api/runners?status=false");
  } catch {
    return null;
  }
  const found = [];
  await Promise.all(runners.map((r) => api("GET", "/api/runners/" + encodeURIComponent(r.name) + "/credentials")
    .then((l) => l.credentials.filter((c) => !kept.has(c.name)).forEach((c) => found.push({ runner: r, c })))
    .catch(() => {})));
  if (found.length === 0) return null;
  found.sort((a, b) => (a.runner.name + a.c.name).localeCompare(b.runner.name + b.c.name));
  return h("section", { class: "card" },
    h("h2", {}, "Only on a runner"),
    h("p", { class: "muted" }, "Added on a runner before. Harnesses there can still use them. To keep one here instead, add it above with the same name and its key; it then replaces the runner's copy."),
    h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "Runner"), h("th", {}, "Name"), h("th", {}, "Sent only to"), h("th", {}, ""))),
      h("tbody", {}, found.map(({ runner, c }) => h("tr", {},
        h("td", {}, runner.name), h("td", {}, h("strong", {}, c.name)), h("td", {}, c.hosts.join(", ")),
        h("td", { class: "actions" }, h("button", { onclick: () => removeFromRunner(ui, runner, c) }, "Remove from " + runner.name))))))));
}

function passwordBox(ui, host) {
  const { h } = ui;
  const input = h("input", { type: "password", autocomplete: "off" });
  const box = h("div", { class: "notice-box", hidden: true }, h("p", {}, ""), h("label", {}, "Your password on " + (host || "the runner"), input));
  return {
    box,
    ask(msg) {
      box.hidden = false;
      box.querySelector("p").textContent = msg + ". It is used for this change only and is not stored.";
      input.focus();
    },
    take() {
      const v = box.hidden || input.value === "" ? undefined : input.value;
      input.value = "";
      return v;
    },
  };
}

function editCredential(ui, existing, done) {
  const { h, set, api, say } = ui;
  const dialog = document.getElementById("dialog");
  const purpose = h("select", {},
    h("option", { value: "web" }, "For a web service: the runner adds it to requests, the agent never sees it"),
    h("option", { value: "secret" }, "For the agent itself: handed to it as a variable or file (it can read it)"));
  const value = h("textarea", { rows: "6", spellcheck: "false", autocomplete: "off",
    placeholder: existing ? "leave empty to keep the current value" : "a password, a token, or a whole file (e.g. a kubeconfig)" });
  const kind = h("select", {}, KINDS.map((k) => h("option", { value: k.id }, k.label)));
  const name = h("input", { required: true, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", placeholder: "anthropic", autocomplete: "off" });
  const hosts = h("input", { required: true, placeholder: "api.anthropic.com", autocomplete: "off" });
  const scheme = h("select", {}, h("option", { value: "https" }, "https (encrypted)"), h("option", { value: "http" }, "http (unencrypted)"));
  const port = h("input", { type: "number", min: "1", max: "65535", required: true });
  const header = h("input", { required: true, placeholder: "x-api-key", autocomplete: "off" });
  const prefix = h("input", { placeholder: "e.g. Bearer (with the space)", autocomplete: "off" });
  const secret = h("input", { type: "password", autocomplete: "new-password", required: !existing,
    placeholder: existing ? "leave empty to keep the current key" : "paste the key" });
  const warn = h("div", { class: "error-box", hidden: true }, "Unencrypted: the key crosses your network in clear text. Use this only for a server in your own network.");
  const problem = h("div", {});
  const webPart = h("div", { class: "form-stack" });
  const secretPart = h("div", { class: "form-stack" },
    h("label", {}, "Value", value, h("span", { class: "muted note" }, "Write-only: nobody can see it again here. Up to 64 KiB.")),
    h("div", { class: "warn-box" }, "The agent can read this secret, and a misled agent could send it to any host its harness may reach. Give it only what it needs, and prefer a token that can do little."));
  const syncPurpose = () => {
    const s = purpose.value === "secret";
    webPart.hidden = s;
    secretPart.hidden = !s;
    for (const el of [hosts, port, header]) el.required = !s;
    secret.required = !s && !existing;
    value.required = s && (!existing || existing.kind !== "secret");
  };
  purpose.addEventListener("change", syncPurpose);
  const sync = () => { warn.hidden = scheme.value !== "http"; };
  const fill = (k) => {
    hosts.value = k.hosts;
    scheme.value = k.scheme;
    port.value = String(k.port);
    header.value = k.header;
    prefix.value = k.prefix;
    if (!existing) name.value = k.name;
    sync();
  };
  kind.addEventListener("change", () => fill(KINDS.find((k) => k.id === kind.value)));
  scheme.addEventListener("change", () => {
    if (scheme.value === "http" && port.value === "443") port.value = "80";
    if (scheme.value === "https" && port.value === "80") port.value = "443";
    sync();
  });
  if (existing) {
    purpose.value = existing.kind === "secret" ? "secret" : "web";
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
    let body;
    if (purpose.value === "secret") {
      body = { kind: "secret", hosts: [], scheme: "", port: 0, header: "" };
      if (value.value !== "") body.secret = value.value;
    } else {
      body = {
        hosts: hosts.value.split(/[\s,]+/).filter(Boolean),
        scheme: scheme.value,
        port: Number(port.value),
        header: header.value.trim(),
        prefix: prefix.value,
      };
      if (secret.value !== "") body.secret = secret.value;
    }
    try {
      await api("PUT", "/api/credentials/" + encodeURIComponent(name.value), body);
    } catch (err) {
      set(problem, h("div", { class: "error-box" }, err.message));
      return;
    } finally {
      secret.value = "";
      value.value = "";
    }
    dialog.close();
    say("Saved " + name.value + ". Harnesses that use it get it on their runner when they start.");
    done();
  } },
  h("h1", {}, existing ? "Change " + existing.name : "Add a credential"),
  h("label", {}, "What is it for?", purpose),
  h("label", {}, "Name", name, h("span", { class: "muted note" }, "You pick it from this name in a harness.")),
  secretPart,
  webPart,
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Save"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  webPart.append(...[
    !existing && h("label", {}, "What is it?", kind),
    h("label", {}, "Sent only to", hosts, h("span", { class: "muted note" }, "Exact host names, separated by commas. Never to any other host, even if the agent asks.")),
    h("div", { class: "row" }, h("label", {}, "Connection", scheme), h("label", {}, "Port", port)),
    warn,
    h("div", { class: "row" }, h("label", {}, "Header", header), h("label", {}, "Before the key", prefix)),
    h("label", {}, "Key", secret, h("span", { class: "muted note" }, "Write-only: nobody can see it again here, not even you."))].filter(Boolean));
  syncPurpose();
  set(dialog, form);
  dialog.showModal();
}

function removeCredential(ui, c, done) {
  const { h, set, api, say } = ui;
  const dialog = document.getElementById("dialog");
  const runners = Object.keys(c.runners);
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    try {
      await api("DELETE", "/api/credentials/" + encodeURIComponent(c.name));
    } catch (err) {
      ui.fail(err);
      return;
    }
    dialog.close();
    say("Removed " + c.name + ".");
    done();
  } },
  h("h1", {}, "Remove " + c.name + "?"),
  h("p", {}, "Harnesses that use " + c.name + " can no longer get it."),
  runners.length > 0 && h("p", { class: "muted" }, "The copies on " + runners.join(", ") + " stay until you remove them there (they then show under \"Only on a runner\")."),
  h("div", { class: "actions" }, h("button", { class: "danger" }, "Remove"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

// copyNow copies a changed credential to a runner, asking for sudo's
// password if the runner's host wants it.
function copyNow(ui, c, runner, done) {
  const { h, set, api, say } = ui;
  const dialog = document.getElementById("dialog");
  const pw = passwordBox(ui, runner);
  const problem = h("div", {});
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const body = { runner };
    const p = pw.take();
    if (p !== undefined) body.sudoPassword = p;
    try {
      await api("POST", "/api/credentials/" + encodeURIComponent(c.name) + "/copy", body);
    } catch (err) {
      if (err.data && err.data.needPassword) {
        pw.ask(err.message);
        return;
      }
      set(problem, h("div", { class: "error-box" }, err.message));
      return;
    }
    dialog.close();
    say("Copied " + c.name + " to " + runner + ".");
    done();
  } },
  h("h1", {}, "Copy " + c.name + " to " + runner + "?"),
  h("p", {}, runner + " has an older version. Harnesses copy it anyway when they start; this does it now, e.g. for its schedules."),
  pw.box,
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Copy"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

function removeFromRunner(ui, runner, c) {
  const { h, set, api, say } = ui;
  const dialog = document.getElementById("dialog");
  const pw = passwordBox(ui, runner.ssh);
  const problem = h("div", {});
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const body = {};
    const p = pw.take();
    if (p !== undefined) body.sudoPassword = p;
    try {
      await api("DELETE", "/api/runners/" + encodeURIComponent(runner.name) + "/credentials/" + encodeURIComponent(c.name), body);
    } catch (err) {
      if (err.data && err.data.needPassword) {
        pw.ask(err.message);
        return;
      }
      set(problem, h("div", { class: "error-box" }, err.message));
      return;
    }
    dialog.close();
    say("Removed " + c.name + " from " + runner.name + ".");
    location.reload();
  } },
  h("h1", {}, "Remove " + c.name + " from " + runner.name + "?"),
  h("p", {}, "Harnesses on " + runner.name + " that use it will be refused until it is there again."),
  pw.box,
  problem,
  h("div", { class: "actions" }, h("button", { class: "danger" }, "Remove"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}
