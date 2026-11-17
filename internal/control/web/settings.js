// A runner's settings: how much it may run at once, how long it keeps
// finished jobs, how long it waits for people, and what models cost.
// The runner reports the bounds its host allows; it checks them again
// when saving. Saving restarts the runner, so it is refused while jobs
// run there. On a server the change is made with sudo over ssh, which may
// ask for the password.

// short drops a Go duration's zero tail: "720h0m0s" reads as "720h".
function short(d) {
  return d === "off" ? d : d.replace(/(\D)0s$/, "$1").replace(/(h)0m$/, "$1");
}

// settingsPanel shows a runner's settings as a form in the page (the
// runner's detail page); done is called after a save.
export function settingsPanel(ui, r, done) {
  const { h, api, set } = ui;
  const box = h("div", {}, h("p", { class: "muted" }, "Asking " + r.name + "…"));
  api("GET", "/api/runners/" + encodeURIComponent(r.name) + "/settings").then(
    (v) => set(box, settingsForm(ui, r, v, done)),
    (err) => set(box, err.data && err.data.needUpdate
      ? h("div", { class: "notice-box" }, r.name + " runs an older version that cannot change its settings from here yet. Update it first (“Update runner” above).")
      : h("div", { class: "error-box" }, "Could not ask " + r.name + ": " + err.message)));
  return box;
}

function settingsForm(ui, r, v, done) {
  const { h, api, set, say } = ui;
  const s = v.settings;
  const b = v.bounds;
  // A value above its bound may stay as it is, but not grow.
  const num = (value, min, max) => h("input", { type: "number", required: true, min: String(min), max: String(Math.max(max, value)), value: String(value) });
  const dur = (value, placeholder) => h("input", { required: true, value: short(value), placeholder, autocomplete: "off", pattern: "off|([0-9]+(\\.[0-9]+)?(h|m|s))+" });
  const f = {
    maxConcurrent: num(s.maxConcurrent, 1, b.maxConcurrent),
    maxPluginVMs: num(s.maxPluginVMs, 0, b.maxPluginVMs),
    maxDiskMiB: num(s.maxDiskMiB, 1, b.maxDiskMiB),
    retentionJobs: num(s.retentionJobs, 1, 1000000),
    retentionAge: dur(s.retentionAge, "720h"),
    approvalTimeout: dur(s.approvalTimeout, "30m"),
    minScheduleInterval: dur(s.minScheduleInterval, "15m"),
    contactTimeout: dur(s.contactTimeout, "72h"),
  };
  const prices = h("tbody", {});
  const priceRow = (model, p) => {
    const row = h("tr", {},
      h("td", {}, h("input", { class: "model", required: true, value: model, placeholder: "claude-opus-5-5", autocomplete: "off", pattern: "[A-Za-z0-9][A-Za-z0-9._:/@+\\-]{0,127}" })),
      h("td", {}, h("input", { class: "in", type: "number", required: true, min: "0", max: "10000", step: "any", value: String(p.input) })),
      h("td", {}, h("input", { class: "out", type: "number", required: true, min: "0", max: "10000", step: "any", value: String(p.output) })),
      h("td", {}, h("button", { type: "button", onclick: () => row.remove() }, "Remove")));
    prices.append(row);
  };
  Object.entries(s.pricing || {}).sort(([a], [b]) => a.localeCompare(b)).forEach(([m, p]) => priceRow(m, p));

  const pw = h("input", { type: "password", autocomplete: "off" });
  const pwBox = h("div", { class: "notice-box", hidden: true },
    h("p", {}, ""), h("label", {}, "Your password on " + (r.ssh || ""), pw));
  const problem = h("div", {});
  const help = (text) => h("span", { class: "muted note" }, text);

  const read = () => {
    const out = {};
    for (const [k, input] of Object.entries(f)) {
      out[k] = input.type === "number" ? Number(input.value) : input.value.trim();
    }
    out.pricing = {};
    for (const row of prices.children) {
      const model = row.querySelector(".model").value.trim();
      out.pricing[model] = { input: Number(row.querySelector(".in").value), output: Number(row.querySelector(".out").value) };
    }
    return out;
  };

  return h("form", { onsubmit: async (ev) => {
    ev.preventDefault();
    set(problem);
    const req = { settings: read() };
    if (!pwBox.hidden && pw.value !== "") req.sudoPassword = pw.value;
    pw.value = "";
    try {
      await api("PUT", "/api/runners/" + encodeURIComponent(r.name) + "/settings", req);
    } catch (err) {
      if (err.data && err.data.needPassword) {
        pwBox.hidden = false;
        pwBox.querySelector("p").textContent = err.message + ". It is used for this change only and is not stored.";
        pw.focus();
        return;
      }
      set(problem, h("div", { class: "error-box" }, err.message));
      return;
    }
    say("Saved the settings of " + r.name + "; it restarted with them.");
    done();
  } },
  h("p", { class: "muted" }, "Saving restarts the runner, so it is only possible while no job runs or waits there. Its schedules, credentials and finished jobs stay."),
  h("div", { class: "form-stack" },
    h("h2", {}, "Capacity"),
    h("div", { class: "row" },
      h("label", {}, "Jobs at once", f.maxConcurrent, help("Each job is a VM. At most " + b.maxConcurrent + " on this runner (its CPUs).")),
      h("label", {}, "Plugin VMs at once", f.maxPluginVMs, help("Plugins run in VMs of their own, besides the jobs."))),
    h("label", {}, "Largest disk a harness may ask for (MiB)", f.maxDiskMiB, help("Reserved on the runner for each job, at most " + b.maxDiskMiB + " MiB.")),
    h("h2", {}, "Finished jobs"),
    h("div", { class: "row" },
      h("label", {}, "Keep at most", f.retentionJobs, help("jobs, with their logs and results")),
      h("label", {}, "Keep for", f.retentionAge, help("e.g. 720h (30 days), at least 1h"))),
    h("h2", {}, "Waiting for people"),
    h("label", {}, "An approval waits for", f.approvalTimeout, help("A gated request is denied after this (1m to 24h).")),
    h("label", {}, "Schedules pause after no contact for", f.contactTimeout,
      help(s.contactTimeout === "off"
        ? "Turned off in the runner's configuration file: schedules never pause. Turning it on here is possible, off again only in the file."
        : "So agents do not keep running when nobody looks after them (1h to 8760h). This Control Center is contact while it runs.")),
    h("label", {}, "Schedules run at most every", f.minScheduleInterval, help("1m to 24h")),
    h("h2", {}, "Model prices"),
    h("p", { class: "muted" }, "In USD per million tokens. A harness's cost budget (maxCostUSD) is enforced only for models with a price here; for the others only its token budget applies."),
    h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "Model"), h("th", {}, "Input"), h("th", {}, "Output"), h("th", {}, ""))),
      prices)),
    h("div", { class: "actions" }, h("button", { type: "button", onclick: () => priceRow("", { input: 0, output: 0 }) }, "Add a price"))),
  pwBox,
  problem,
  h("div", { class: "actions" }, h("button", { class: "primary" }, "Save and restart")));
}
