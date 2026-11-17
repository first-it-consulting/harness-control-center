// Network policies: named sets of hosts an agent may reach, defined once
// and attached to harnesses. Attaching copies a policy's hosts into the
// harness, so every runner enforces exactly what the harness says.

// Templates for what agents commonly need.
const TEMPLATES = [
  { name: "github", description: "GitHub: the site, its API and downloads", hosts: ["github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"] },
  { name: "gitlab", description: "GitLab.com", hosts: ["gitlab.com"] },
  { name: "python-packages", description: "Python packages from PyPI (pip)", hosts: ["pypi.org", "files.pythonhosted.org"] },
  { name: "npm-packages", description: "JavaScript packages from npm", hosts: ["registry.npmjs.org"] },
  { name: "go-modules", description: "Go modules", hosts: ["proxy.golang.org", "sum.golang.org"] },
  { name: "rust-crates", description: "Rust crates from crates.io", hosts: ["crates.io", "index.crates.io", "static.crates.io"] },
];

const hostText = (r) => r.host + (r.ports && r.ports.length > 0 && !(r.ports.length === 1 && r.ports[0] === 443) ? ":" + r.ports.join(",") : "");

export async function networkView(ui) {
  const { h, api, set, show, fail } = ui;
  const list = h("div", { class: "grid" }, h("p", { class: "muted" }, "Loading…"));
  const page = { ui, policies: [], render: null };
  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Network policies"),
      h("p", { class: "muted" }, "An agent can reach nothing unless a harness allows it. A policy is a named set of hosts (\"GitHub\", \"Python packages\"); attach it to a harness instead of listing its hosts there each time.")),
      h("div", { class: "actions" }, h("button", { class: "primary", onclick: () => editPolicy(page, -1) }, "Add a policy"))),
    list);
  page.render = () => set(list, ...(page.policies.length === 0
    ? [h("div", { class: "empty" }, "No policies yet. Add one, or start from a template.")]
    : page.policies.map((p, i) => policyCard(page, p, i))));
  try {
    page.policies = (await api("GET", "/api/policies")).policies;
  } catch (err) {
    fail(err);
    return;
  }
  page.render();
}

function policyCard(page, p, index) {
  const { h, badge } = page.ui;
  return h("div", { class: "card" },
    h("h3", {}, p.name, " ", p.private && badge("own network", "queued")),
    p.description && h("p", { class: "muted" }, p.description),
    h("ul", { class: "chips" }, p.hosts.map((r) => h("li", {}, hostText(r)))),
    h("p", { class: "muted" }, p.usedBy.length === 0 ? "Not attached to a harness." : ["Attached to ", ...p.usedBy.flatMap((n, j) =>
      [j > 0 ? ", " : null, h("a", { href: "#/harness/" + encodeURIComponent(n) }, n)]), "."]),
    h("div", { class: "actions" },
      h("button", { onclick: () => editPolicy(page, index) }, "Change"),
      h("button", { onclick: () => removePolicy(page, index) }, "Remove")));
}

const plain = (p) => {
  const out = { name: p.name, hosts: p.hosts.map((r) => (r.ports && r.ports.length > 0 ? { host: r.host, ports: r.ports } : { host: r.host })) };
  if (p.description) out.description = p.description;
  if (p.private) out.private = true;
  return out;
};

async function save(page, next, problem) {
  const { h, api, set } = page.ui;
  try {
    page.policies = (await api("PUT", "/api/policies", { policies: next })).policies;
    page.render();
    return true;
  } catch (err) {
    const errs = (err.data && err.data.errors) || [err.message];
    set(problem, h("div", { class: "error-box" }, h("ul", { class: "explain errors" }, errs.map((e) => h("li", {}, e)))));
    return false;
  }
}

// parseHosts reads "host" or "host:port,port" per line.
function parseHosts(text) {
  return text.split(/[\n,;\s]+/).filter(Boolean).reduce((out, item) => {
    const [host, ports] = item.split(":");
    const p = (ports || "").split("/").filter(Boolean).map(Number);
    out.push(p.length > 0 ? { host: host.trim().toLowerCase(), ports: p } : { host: host.trim().toLowerCase() });
    return out;
  }, []);
}

function editPolicy(page, index) {
  const { h, set, say } = page.ui;
  const dialog = document.getElementById("dialog");
  const existing = index >= 0 ? page.policies[index] : null;
  const help = (t) => h("span", { class: "muted note" }, t);
  const template = h("select", {}, [h("option", { value: "" }, "Start empty"), ...TEMPLATES.map((t, i) => h("option", { value: String(i) }, t.description))]);
  const name = h("input", { required: true, pattern: "[a-z0-9]([a-z0-9\\-]{0,61}[a-z0-9])?", autocomplete: "off", placeholder: "github" });
  const description = h("input", { maxlength: "200", autocomplete: "off", placeholder: "What it is for (optional)" });
  const hosts = h("textarea", { rows: "6", required: true, spellcheck: "false", placeholder: "github.com\napi.github.com\nnas.lan:8443" });
  const priv = h("input", { type: "checkbox" });
  const problem = h("div", {});
  template.addEventListener("change", () => {
    const t = TEMPLATES[Number(template.value)];
    if (!t) return;
    name.value = t.name;
    description.value = t.description;
    hosts.value = t.hosts.join("\n");
    priv.checked = false;
  });
  if (existing) {
    name.value = existing.name;
    description.value = existing.description || "";
    hosts.value = existing.hosts.map((r) => r.host + (r.ports && r.ports.length > 0 ? ":" + r.ports.join("/") : "")).join("\n");
    priv.checked = !!existing.private;
  }
  const form = h("form", { method: "dialog", onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const p = { name: name.value.trim(), description: description.value.trim(), hosts: parseHosts(hosts.value), private: priv.checked };
    const next = page.policies.map(plain);
    if (existing) next[index] = plain(p);
    else next.push(plain(p));
    if (await save(page, next, problem)) {
      dialog.close();
      say("Saved the policy " + p.name + "." + (existing && existing.usedBy.length > 0 ? " Harnesses that have it keep their hosts until you change them there." : ""));
    }
  } },
  h("h1", {}, existing ? "Change " + existing.name : "Add a network policy"),
  h("div", { class: "form-stack" },
    !existing && h("label", {}, "Template", template, help("Common sets of hosts, to start from.")),
    h("label", {}, "Name", name),
    h("label", {}, "Description", description),
    h("label", {}, "Hosts", hosts, help("One per line. Port 443 (https) unless you add others: nas.lan:8443, or several as git.lan:22/443. *.example.com allows all of example.com's subdomains.")),
    h("label", { class: "check" }, priv, "These hosts are in my own network (at home or at work)"),
    help("Needed for names like nas.lan, and for any host whose address is in your own network.")),
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Save"), h("button", { type: "button", onclick: () => dialog.close() }, "Cancel")));
  set(dialog, form);
  dialog.showModal();
}

function removePolicy(page, index) {
  const { h, say, fail } = page.ui;
  const p = page.policies[index];
  if (!confirm("Remove the policy " + p.name + "?" + (p.usedBy.length > 0 ? " The harnesses that have it (" + p.usedBy.join(", ") + ") keep its hosts." : ""))) return;
  const problem = h("div", {});
  save(page, page.policies.filter((_, i) => i !== index).map(plain), problem)
    .then((ok) => (ok ? say("Removed " + p.name + ".") : fail(new Error(problem.textContent))));
}
