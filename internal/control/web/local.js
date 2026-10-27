// "Run agents on this Mac": the local runner (ADR-0016), set up from the
// Control Center with one button. The server does the work (GET and POST
// /api/local); this panel shows its state and follows the setup step by
// step. On a computer that cannot run local agents it shows nothing.

const STATES = {
  running: ["working…", "running"],
  done: ["done", "succeeded"],
  skipped: ["already done", "queued"],
  failed: ["failed", "failed"],
};

// localPanel returns an element that fills itself in; ui has the app's
// helpers. compact: only when something is to be done (the dashboard).
export function localPanel(ui, compact) {
  const { h, api, set, fail, say } = ui;
  const box = h("div", {});
  let timer = null;

  const step = (s) => {
    const [label, kind] = STATES[s.state] || [s.state, "queued"];
    let detail = s.state === "skipped" ? "" : s.detail || "";
    if (s.state === "running" && s.bytes) detail = (detail ? detail + ": " : "") + Math.round(s.bytes / (1 << 20)) + " MiB";
    return h("li", {}, h("span", { class: "badge " + kind }, label), " ", s.title, detail && h("span", { class: "muted" }, " – " + detail));
  };

  const setup = async () => {
    try {
      await api("POST", "/api/local/setup");
    } catch (err) {
      fail(err);
    }
    load();
  };

  const stop = async () => {
    if (!confirm("Stop the runner on this Mac? Its jobs stop, and it no longer starts at login. Setting it up again starts it.")) return;
    try {
      await api("POST", "/api/local/stop");
      say("The runner on this Mac is stopped.");
    } catch (err) {
      fail(err);
    }
    load();
  };

  const render = (v) => {
    if (v.supported) {
      set(box);
      return;
    }
    const st = v.setup;
    const ready = v.ready && v.registered && !st.running;
    if (compact && ready) {
      set(box);
      return;
    }
    const steps = st.steps.length > 0 && h("ol", { class: "steps" }, st.steps.map(step));
    let body;
    if (st.running) {
      body = [h("p", {}, "Setting up… This takes about a minute; the first time, it downloads about 210 MB."), steps];
    } else if (ready) {
      body = [
        h("p", {}, "Agents can run on this Mac: harnesses with ", h("code", {}, "target: local"), " run here, in VMs of Apple's own Virtualization framework."),
        st.finished && steps,
        h("div", { class: "actions" },
          h("button", { onclick: setup }, "Check for updates"),
          h("button", { class: "danger", onclick: stop }, "Stop the runner")),
      ];
    } else if (v.answering && !v.registered) {
      body = [
        h("p", {}, "The runner on this Mac is running, but this Control Center does not list it yet."),
        h("div", { class: "actions" }, h("button", { class: "primary", onclick: setup }, "Add it here")),
      ];
    } else {
      const again = v.images && v.config;
      body = [
        h("p", {}, again
          ? "The runner on this Mac is set up but not running."
          : "Run agents right here, without a server. Each agent gets its own VM with no network of its own; it reaches only what its harness allows."),
        !again && h("p", { class: "muted" }, "This downloads the VM images (about 210 MB) and Claude Code, and starts the runner in the background, as you. Nothing needs administrator rights."),
        st.error && h("div", { class: "error-box" }, st.error),
        steps,
        h("div", { class: "actions" }, h("button", { class: "primary", onclick: setup }, again ? "Start the runner" : st.error ? "Try again" : "Set up this Mac")),
      ];
    }
    set(box, h("div", { class: "card local" }, h("h2", {}, "This Mac"), ...body));
    // Follow a running setup; stop when the panel is gone.
    clearTimeout(timer);
    if (st.running) timer = setTimeout(() => box.isConnected && load(), 1000);
  };

  const load = async () => {
    try {
      render(await api("GET", "/api/local"));
    } catch (err) {
      fail(err);
    }
  };
  load();
  return box;
}
