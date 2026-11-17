// Secrets and variables (ADR-0019, ADR-0020): reusable configuration kept
// here and given to harnesses, laid out like GitHub's page of the same
// name. Secrets are write-only and copied to a runner only when a harness
// that uses them starts; the agent reads them, or (for API keys) never
// sees them because the runner adds them to its requests. Variables are
// not secret and shown as plain text.

// Presets for secrets the runner adds to web requests.
const PRESETS = [
  { id: "anthropic", label: "Anthropic API key", hosts: "api.anthropic.com", scheme: "https", port: 443, header: "x-api-key", prefix: "" },
  { id: "github", label: "GitHub token (GitHub API)", hosts: "api.github.com", scheme: "https", port: 443, header: "Authorization", prefix: "Bearer " },
  { id: "gitlab", label: "GitLab token (GitLab API)", hosts: "gitlab.com", scheme: "https", port: 443, header: "PRIVATE-TOKEN", prefix: "" },
  { id: "bearer", label: "API token sent as \"Authorization: Bearer …\" (OpenAI, OpenRouter, LiteLLM…)", hosts: "", scheme: "https", port: 443, header: "Authorization", prefix: "Bearer " },
  { id: "local", label: "Model server in your network (oMLX, Ollama, LM Studio…)", hosts: "", scheme: "http", port: 4000, header: "x-api-key", prefix: "" },
  { id: "custom", label: "Something else (my own header)", hosts: "", scheme: "https", port: 443, header: "", prefix: "" },
];

// ago says how long ago a time was, in words.
function ago(t) {
  if (!t) return "";
  const s = Math.max(0, Math.round((Date.now() - new Date(t).getTime()) / 1000));
  const units = [[31536000, "year"], [2592000, "month"], [86400, "day"], [3600, "hour"], [60, "minute"]];
  for (const [n, name] of units) {
    if (s >= n) {
      const k = Math.floor(s / n);
      return k + " " + name + (k > 1 ? "s" : "") + " ago";
    }
  }
  return "just now";
}

export async function credentialsView(ui, tab) {
  const { h, set, show } = ui;
  const page = { ui, tab: tab === "variables" ? "variables" : "secrets", secrets: [], variables: [] };
  const body = h("div", {}, h("p", { class: "muted" }, "Loading…"));
  const tabs = h("div", { class: "tabs", role: "tablist" });
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Secrets and variables"),
      h("p", { class: "muted" }, "Secrets and variables keep reusable configuration in one place for your harnesses. Secrets are for sensitive data: once saved they cannot be seen again here, they are kept readable only by you on this computer, and a runner gets one only when a harness that uses it starts there. Variables are shown as plain text and are for data that is not sensitive."))),
    tabs, body);
  page.render = () => {
    set(tabs, ...["secrets", "variables"].map((t) => h("button", { type: "button", class: "tab" + (page.tab === t ? " active" : ""),
      onclick: () => { page.tab = t; history.replaceState(null, "", "#/credentials/" + t); page.render(); } }, t === "secrets" ? "Secrets" : "Variables")));
    set(body, page.tab === "secrets" ? secretsTab(page) : variablesTab(page));
  };
  page.reload = async () => {
    try {
      const [c, v] = await Promise.all([ui.api("GET", "/api/credentials"), ui.api("GET", "/api/variables")]);
      page.secrets = c.credentials;
      page.variables = v.variables;
    } catch (err) {
      set(body, h("div", { class: "error-box" }, err.message));
      return;
    }
    page.render();
  };
  await page.reload();
}

// listBox is GitHub's box: a heading with its "New" button, rows, or an
// empty state with the button.
function listBox(ui, title, newLabel, onNew, rows, empty) {
  const { h } = ui;
  return h("section", { class: "stack" },
    h("div", { class: "card-head" }, h("h2", {}, title), rows.length > 0 && h("button", { class: "primary", onclick: onNew }, newLabel)),
    rows.length === 0
      ? h("div", { class: "card empty-box" }, h("p", { class: "muted" }, empty), h("button", { class: "primary", onclick: onNew }, newLabel))
      : h("div", { class: "card list" }, rows));
}

function secretsTab(page) {
  const { ui } = page;
  const { h, badge } = ui;
  const rows = page.secrets.map((c) => h("div", { class: "list-row" },
    h("div", { class: "list-main" },
      h("strong", {}, c.name),
      h("div", { class: "muted small" }, c.kind === "secret"
        ? "The agent reads it (as a variable or a file, chosen in the harness)"
        : ["Added to requests to ", c.hosts.join(", "), " — the agent never sees it", c.scheme === "http" ? " (unencrypted)" : ""].join("")),
      Object.keys(c.runners).length > 0 && h("div", { class: "muted small" }, "On runners: ", Object.entries(c.runners).sort().flatMap(([r, state], i) =>
        [i > 0 ? ", " : null, r, " ", state === "current" ? badge("current", "succeeded") : [badge("older version", "queued"), " ",
          h("button", { class: "link-button", onclick: () => copyNow(page, c, r) }, "copy now")]]))),
    h("div", { class: "muted small" }, c.updated ? "Updated " + ago(c.updated) : ""),
    h("div", { class: "actions" },
      h("button", { title: "Change", onclick: () => editSecret(page, c) }, "Edit"),
      h("button", { title: "Remove", onclick: () => removeSecret(page, c) }, "Remove"))));
  const box = listBox(ui, "Secrets", "New secret", () => editSecret(page, null), rows, "There are no secrets yet.");
  const others = h("div", {});
  runnerOnly(page).then((card) => card && others.append(card));
  return h("div", { class: "stack" }, box, others);
}

function variablesTab(page) {
  const { ui } = page;
  const { h } = ui;
  const rows = page.variables.map((v) => h("div", { class: "list-row" },
    h("div", { class: "list-main" }, h("strong", {}, v.name), h("div", { class: "muted small mono" }, v.value)),
    h("div", { class: "muted small" }, "Updated " + ago(v.updated)),
    h("div", { class: "actions" },
      h("button", { onclick: () => editVariable(page, v) }, "Edit"),
      h("button", { onclick: () => removeVariable(page, v) }, "Remove"))));
  return listBox(ui, "Variables", "New variable", () => editVariable(page, null), rows, "There are no variables yet.");
}

function dialogForm(ui, title, fields, submit, submitLabel = "Save") {
  const { h, set } = ui;
  const dialog = document.getElementById("dialog");
  const problem = h("div", {});
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    try {
      if (await submit(problem) !== false) dialog.close();
    } catch (err) {
      set(problem, h("div", { class: "error-box" }, err.message));
    }
  } },
  h("h1", {}, title), h("div", { class: "form-stack" }, ...fields), problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, submitLabel), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

const help = (h, t) => h("span", { class: "muted note" }, t);

function editSecret(page, existing) {
  const { ui } = page;
  const { h, api, say } = ui;
  const name = h("input", { required: true, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", placeholder: "db-password", autocomplete: "off" });
  const value = h("textarea", { rows: "5", spellcheck: "false", autocomplete: "off",
    placeholder: existing ? "Leave empty to keep the current value" : "The secret: a password, a token, or a whole file (e.g. a kubeconfig)" });
  const reads = h("input", { type: "radio", name: "use", value: "secret" });
  const requests = h("input", { type: "radio", name: "use", value: "web" });
  // How a web secret is added to requests.
  const preset = h("select", {}, PRESETS.map((p) => h("option", { value: p.id }, p.label)));
  const hosts = h("input", { placeholder: "api.anthropic.com", autocomplete: "off" });
  const scheme = h("select", {}, h("option", { value: "https" }, "https (encrypted)"), h("option", { value: "http" }, "http (unencrypted)"));
  const port = h("input", { type: "number", min: "1", max: "65535" });
  const header = h("input", { placeholder: "x-api-key", autocomplete: "off" });
  const prefix = h("input", { placeholder: "e.g. Bearer (with the space)", autocomplete: "off" });
  const web = h("div", { class: "form-stack nested" },
    h("label", {}, "Kind of key", preset),
    h("label", {}, "Only to these hosts", hosts, help(h, "Exact host names, separated by commas. It is never sent anywhere else.")),
    h("div", { class: "row" }, h("label", {}, "Connection", scheme), h("label", {}, "Port", port)),
    h("div", { class: "row" }, h("label", {}, "Header", header), h("label", {}, "Before the value", prefix)));
  const warn = h("div", { class: "warn-box" }, "The agent can read this secret, and a misled agent could send it to any host its harness may reach. Prefer a token that can do little.");
  const fill = (p) => {
    hosts.value = p.hosts;
    scheme.value = p.scheme;
    port.value = String(p.port);
    header.value = p.header;
    prefix.value = p.prefix;
  };
  preset.addEventListener("change", () => fill(PRESETS.find((p) => p.id === preset.value)));
  const sync = () => {
    web.hidden = !requests.checked;
    warn.hidden = !reads.checked;
    for (const el of [hosts, port, header]) el.required = requests.checked;
  };
  reads.addEventListener("change", sync);
  requests.addEventListener("change", sync);
  if (existing) {
    name.value = existing.name;
    name.disabled = true;
    if (existing.kind === "secret") {
      reads.checked = true;
      fill(PRESETS[0]);
    } else {
      requests.checked = true;
      preset.value = "custom";
      fill({ hosts: existing.hosts.join(", "), scheme: existing.scheme, port: existing.port, header: existing.header, prefix: existing.prefix || "" });
    }
  } else {
    value.required = true;
    reads.checked = true;
    fill(PRESETS[0]);
  }
  sync();
  dialogForm(ui, existing ? "Update secret " + existing.name : "New secret", [
    h("label", {}, "Name", name, help(h, "Lowercase letters, digits and '-'. Harnesses pick it by this name.")),
    h("label", {}, "Secret", value),
    h("fieldset", { class: "choice" }, h("legend", {}, "How harnesses use it"),
      h("label", { class: "check" }, reads, "The agent reads it: as an environment variable or a file, chosen in each harness"),
      h("label", { class: "check" }, requests, "Only add it to web requests to certain hosts: the agent never sees it (best for API keys)")),
    web, warn,
  ], async () => {
    const kindChanged = existing && (existing.kind === "secret") !== reads.checked;
    if (kindChanged && value.value === "") throw new Error("Changing how it is used needs the secret again: enter it above.");
    const body = reads.checked
      ? { kind: "secret", hosts: [], scheme: "", port: 0, header: "" }
      : { hosts: hosts.value.split(/[\s,]+/).filter(Boolean), scheme: scheme.value, port: Number(port.value), header: header.value.trim(), prefix: prefix.value };
    if (value.value !== "") body.secret = value.value;
    try {
      await api("PUT", "/api/credentials/" + encodeURIComponent(name.value), body);
    } finally {
      value.value = "";
    }
    say((existing ? "Updated " : "Added ") + name.value + ".");
    page.reload();
  }, existing ? "Update secret" : "Add secret");
}

function removeSecret(page, c) {
  const { ui } = page;
  const { h, api, say } = ui;
  const runners = Object.keys(c.runners);
  dialogForm(ui, "Remove secret " + c.name + "?", [
    h("p", {}, "Harnesses that use " + c.name + " can no longer get it."),
    runners.length > 0 && h("p", { class: "muted" }, "The copies on " + runners.join(", ") + " stay until you remove them there (listed under \"Only on a runner\")."),
  ], async () => {
    await api("DELETE", "/api/credentials/" + encodeURIComponent(c.name));
    say("Removed " + c.name + ".");
    page.reload();
  }, "Remove");
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

// copyNow copies a changed secret to a runner now (e.g. for its
// schedules), asking for sudo's password if its host wants it.
function copyNow(page, c, runner) {
  const { ui } = page;
  const { h, api, say } = ui;
  const pw = passwordBox(ui, runner);
  dialogForm(ui, "Copy " + c.name + " to " + runner + "?", [
    h("p", {}, runner + " has an older version. Harnesses copy it anyway when they start; this does it now, e.g. for its schedules."),
    pw.box,
  ], async () => {
    const body = { runner };
    const p = pw.take();
    if (p !== undefined) body.sudoPassword = p;
    try {
      await api("POST", "/api/credentials/" + encodeURIComponent(c.name) + "/copy", body);
    } catch (err) {
      if (err.data && err.data.needPassword) {
        pw.ask(err.message);
        return false;
      }
      throw err;
    }
    say("Copied " + c.name + " to " + runner + ".");
    page.reload();
  }, "Copy");
}

// runnerOnly lists secrets a runner has but this Control Center does not
// keep (added on the runner before).
async function runnerOnly(page) {
  const { ui } = page;
  const { h, api } = ui;
  const kept = new Set(page.secrets.map((c) => c.name));
  let runners;
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
  return h("section", { class: "stack" },
    h("h2", {}, "Only on a runner"),
    h("p", { class: "muted" }, "Added on a runner before. Harnesses there can still use them. To keep one here instead, add a secret with the same name and its value; it then replaces the runner's copy."),
    h("div", { class: "card list" }, found.map(({ runner, c }) => h("div", { class: "list-row" },
      h("div", { class: "list-main" }, h("strong", {}, c.name), h("div", { class: "muted small" }, "on " + runner.name + (c.hosts.length > 0 ? ", for " + c.hosts.join(", ") : ""))),
      h("div", { class: "actions" }, h("button", { onclick: () => removeFromRunner(page, runner, c) }, "Remove from " + runner.name))))));
}

function removeFromRunner(page, runner, c) {
  const { ui } = page;
  const { h, api, say } = ui;
  const pw = passwordBox(ui, runner.ssh);
  dialogForm(ui, "Remove " + c.name + " from " + runner.name + "?", [
    h("p", {}, "Harnesses on " + runner.name + " that use it will be refused until it is there again."),
    pw.box,
  ], async () => {
    const body = {};
    const p = pw.take();
    if (p !== undefined) body.sudoPassword = p;
    try {
      await api("DELETE", "/api/runners/" + encodeURIComponent(runner.name) + "/credentials/" + encodeURIComponent(c.name), body);
    } catch (err) {
      if (err.data && err.data.needPassword) {
        pw.ask(err.message);
        return false;
      }
      throw err;
    }
    say("Removed " + c.name + " from " + runner.name + ".");
    page.reload();
  }, "Remove");
}

function editVariable(page, existing) {
  const { ui } = page;
  const { h, api, say } = ui;
  const name = h("input", { required: true, pattern: "[A-Za-z_][A-Za-z0-9_]{0,63}", placeholder: "DB_HOST", autocomplete: "off" });
  const value = h("textarea", { rows: "3", spellcheck: "false", placeholder: "db.lan" });
  if (existing) {
    name.value = existing.name;
    name.disabled = true;
    value.value = existing.value;
  }
  dialogForm(ui, existing ? "Update variable " + existing.name : "New variable", [
    h("label", {}, "Name", name, help(h, "As the agent sees it: letters, digits and _, e.g. DB_HOST.")),
    h("label", {}, "Value", value, help(h, "Not secret: shown here and in every harness that uses it. Use a secret for passwords and tokens.")),
  ], async () => {
    await api("PUT", "/api/variables/" + encodeURIComponent(name.value), { value: value.value });
    say((existing ? "Updated " : "Added ") + name.value + ".");
    page.reload();
  }, existing ? "Update variable" : "Add variable");
}

function removeVariable(page, v) {
  const { ui } = page;
  const { h, api, say } = ui;
  dialogForm(ui, "Remove variable " + v.name + "?", [
    h("p", {}, "Harnesses that use it keep their copy until you change them."),
  ], async () => {
    await api("DELETE", "/api/variables/" + encodeURIComponent(v.name));
    say("Removed " + v.name + ".");
    page.reload();
  }, "Remove");
}
