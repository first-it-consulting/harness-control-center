// The dashboard: what needs a person, what runs now, and how jobs and
// model usage went over a time range. Everything is computed here from
// what the runners keep (jobs, schedules, approvals), so the numbers
// cover at most their retention (30 days by default).
//
// Like the rest of the UI it builds the DOM from text and attributes
// only; job and harness names come from runners and agents.

const SVG = "http://www.w3.org/2000/svg";

const RANGES = [
  { id: "24h", label: "Last 24 hours", hours: 24, bucket: "hour" },
  { id: "7d", label: "Last 7 days", hours: 24 * 7, bucket: "day" },
  { id: "30d", label: "Last 30 days", hours: 24 * 30, bucket: "day" },
];

// Outcomes, in stacking order (bottom up). The order keeps the red and
// green of the status palette apart, for color-blind readers.
const OUTCOMES = [
  { id: "succeeded", label: "Succeeded", states: ["succeeded"] },
  { id: "active", label: "Running or queued", states: ["queued", "provisioning", "running", "awaiting_approval"] },
  { id: "stopped", label: "Stopped (cancelled, timed out, over budget)", states: ["cancelled", "timed_out", "over_budget"] },
  { id: "failed", label: "Failed", states: ["failed"] },
];
const OUTCOME_OF = Object.fromEntries(OUTCOMES.flatMap((o) => o.states.map((s) => [s, o.id])));

const PREFS_KEY = "hcc-dashboard";

function loadPrefs() {
  try {
    return JSON.parse(localStorage.getItem(PREFS_KEY)) || {};
  } catch {
    return {};
  }
}

function savePrefs(p) {
  try {
    localStorage.setItem(PREFS_KEY, JSON.stringify(p));
  } catch {
    // a convenience only
  }
}

function svg(tag, attrs, ...children) {
  const el = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v !== undefined && v !== null && v !== false) el.setAttribute(k, String(v));
  }
  for (const c of children.flat()) {
    if (c !== undefined && c !== null && c !== false) el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function compact(n) {
  if (n < 1000) return String(Math.round(n));
  if (n < 1e6) return (n / 1e3).toFixed(n < 1e4 ? 1 : 0).replace(/\.0$/, "") + "K";
  if (n < 1e9) return (n / 1e6).toFixed(n < 1e7 ? 1 : 0).replace(/\.0$/, "") + "M";
  return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
}

function tokens(j) {
  // A helper shares its parent's meter: count a tree's usage once, at
  // its root.
  if (!j.usage || j.parent) return { in: 0, out: 0, cost: 0, priced: true };
  return { in: j.usage.inputTokens, out: j.usage.outputTokens, cost: j.usage.costUSD, priced: j.usage.costKnown };
}

function when(j) {
  return new Date(j.started || j.created);
}

// buckets returns the time buckets of a range, oldest first, in local time.
function buckets(range, now) {
  const out = [];
  if (range.bucket === "hour") {
    const end = new Date(now);
    end.setMinutes(0, 0, 0);
    for (let i = range.hours - 1; i >= 0; i--) {
      const from = new Date(end.getTime() - i * 3600e3);
      out.push({ from, to: new Date(from.getTime() + 3600e3) });
    }
  } else {
    const days = range.hours / 24;
    const today = new Date(now);
    today.setHours(0, 0, 0, 0);
    for (let i = days - 1; i >= 0; i--) {
      const from = new Date(today);
      from.setDate(today.getDate() - i);
      const to = new Date(from);
      to.setDate(from.getDate() + 1);
      out.push({ from, to });
    }
  }
  return out;
}

function bucketLabel(b, range) {
  return range.bucket === "hour"
    ? b.from.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : b.from.toLocaleDateString([], { month: "short", day: "numeric" });
}

export async function dashboardView(ui) {
  const { h, api, set, show, every, fail, badge, fmtTime, fmtDuration, localPanel } = ui;
  const prefs = loadPrefs();
  let range = RANGES.find((r) => r.id === prefs.range) || RANGES[0];
  let runnerFilter = prefs.runner || "";
  let data = null;

  const body = h("div", { class: "dash" });
  const rangeButtons = RANGES.map((r) => h("button", { type: "button", "aria-pressed": String(r === range), onclick: () => {
    range = r;
    for (const [i, b] of rangeButtons.entries()) b.setAttribute("aria-pressed", String(RANGES[i] === range));
    savePrefs({ range: range.id, runner: runnerFilter });
    render();
  } }, r.label));
  const runnerSelect = h("select", { "aria-label": "Runner", onchange: () => {
    runnerFilter = runnerSelect.value;
    savePrefs({ range: range.id, runner: runnerFilter });
    render();
  } });

  show(
    h("div", { class: "head" }, h("div", {}, h("h1", {}, "Dashboard"),
      h("p", { class: "muted" }, "What needs you, what runs now, and how jobs went. Updated every 15 seconds, from what the runners keep (up to 30 days)."))),
    // Until this Mac runs agents, offer to set it up.
    localPanel(ui, true),
    h("div", { class: "filters" }, h("div", { class: "segmented", role: "group", "aria-label": "Time range" }, ...rangeButtons), runnerSelect),
    body);

  const load = async () => {
    try {
      const [jobs, runners, schedules, approvals] = await Promise.all([
        api("GET", "/api/jobs"), api("GET", "/api/runners"), api("GET", "/api/schedules"), api("GET", "/api/approvals"),
      ]);
      data = { jobs: jobs.jobs, jobErrors: jobs.errors || {}, runners, schedules: schedules.schedules || [], approvals: approvals.approvals || [] };
    } catch (err) {
      fail(err);
      return;
    }
    const names = data.runners.map((r) => r.name);
    if (runnerFilter && !names.includes(runnerFilter)) runnerFilter = "";
    set(runnerSelect, h("option", { value: "" }, "All runners"), ...names.map((n) => h("option", { value: n }, n)));
    runnerSelect.value = runnerFilter;
    render();
  };

  function render() {
    if (!data) return;
    const now = new Date();
    const since = new Date(now.getTime() - range.hours * 3600e3);
    const mine = (x) => !runnerFilter || x.runner === runnerFilter;
    const all = data.jobs.filter(mine);
    const jobs = all.filter((j) => when(j) >= since);
    const runners = data.runners.filter((r) => !runnerFilter || r.name === runnerFilter);
    set(body,
      attention(now, jobs, runners),
      tiles(all, jobs, runners),
      h("div", { class: "dash-row" },
        h("section", { class: "card" }, h("h2", {}, "Jobs over time"), jobsChart(jobs, now)),
        h("section", { class: "card" }, h("h2", {}, range.hours > 24 * 7 ? "Tokens by weekday and hour" : "Tokens by day and hour"), heatmap(jobs, now))),
      h("section", { class: "card" }, h("h2", {}, "By harness"), byHarness(jobs)),
      h("div", { class: "dash-row three" },
        h("section", { class: "card" }, h("h2", {}, "By model"), byModel(jobs)),
        h("section", { class: "card" }, h("h2", {}, "Runners"), runnerTable(runners)),
        h("section", { class: "card" }, h("h2", {}, "Next scheduled runs"), nextRuns())));
  }

  // --- what needs a person ---

  function attention(now, jobs, runners) {
    const items = [];
    const approvals = data.approvals.filter(mine2);
    if (approvals.length > 0) {
      items.push(["warning", approvals.length + (approvals.length === 1 ? " request waits" : " requests wait") + " for your decision (shown at the top of every page)."]);
    }
    for (const r of runners) {
      if (!r.status) items.push(["critical", "Runner " + r.name + " is unreachable.", "#/runners"]);
      else if (r.status.killed) items.push(["critical", "Runner " + r.name + " is stopped (kill switch): it refuses new jobs.", "#/runners"]);
    }
    const failed = jobs.filter((j) => j.state === "failed").length;
    if (failed > 0) items.push(["critical", failed + (failed === 1 ? " job" : " jobs") + " failed in this range.", "#/jobs"]);
    const paused = data.schedules.filter((s) => mine2(s) && s.state === "paused").length;
    if (paused > 0) items.push(["warning", paused + (paused === 1 ? " schedule is" : " schedules are") + " paused.", "#/schedules"]);
    if (items.length === 0) {
      return h("section", { class: "attention ok" }, h("span", { class: "status-dot good", "aria-hidden": "true" }, "✓"), "All clear: nothing needs you.");
    }
    return h("section", { class: "attention", "aria-label": "Needs your attention" },
      h("h2", {}, "Needs your attention"),
      h("ul", {}, items.map(([level, text, href]) => h("li", {},
        h("span", { class: "status-dot " + level, "aria-hidden": "true" }, level === "critical" ? "✕" : "!"),
        h("span", { class: "sr-only" }, level === "critical" ? "Problem: " : "Note: "),
        href ? h("a", { href }, text) : text))));
  }

  function mine2(x) {
    return !runnerFilter || x.runner === runnerFilter;
  }

  // --- headline numbers ---

  function tiles(all, jobs, runners) {
    const active = all.filter((j) => ["provisioning", "running", "awaiting_approval"].includes(j.state)).length;
    const queued = all.filter((j) => j.state === "queued").length;
    const slots = runners.reduce((n, r) => n + (r.status ? r.status.maxConcurrent : 0), 0);
    const finished = jobs.filter((j) => OUTCOME_OF[j.state] && OUTCOME_OF[j.state] !== "active");
    const ok = finished.filter((j) => j.state === "succeeded").length;
    const failed = finished.filter((j) => j.state === "failed").length;
    const t = jobs.map(tokens).reduce((a, b) => ({ in: a.in + b.in, out: a.out + b.out, cost: a.cost + (b.priced ? b.cost : 0), unpriced: a.unpriced + (b.priced ? 0 : 1) }), { in: 0, out: 0, cost: 0, unpriced: 0 });
    const scheduled = jobs.filter((j) => j.schedule).length;
    const tile = (label, value, sub) => h("div", { class: "tile" }, h("div", { class: "tile-label" }, label), h("div", { class: "tile-value" }, value), h("div", { class: "tile-sub muted" }, sub));
    return h("div", { class: "tiles" },
      tile("Running now", String(active), queued + " queued · " + slots + " slots"),
      tile("Jobs", jobs.length.toLocaleString(), scheduled + " started by schedules"),
      tile("Success rate", finished.length ? Math.round((ok / finished.length) * 100) + "%" : "–",
        finished.length ? ok + " of " + finished.length + " finished · " + failed + " failed" : "no finished jobs"),
      tile("Tokens", compact(t.in + t.out), compact(t.in) + " in · " + compact(t.out) + " out"),
      tile("Cost", "$" + t.cost.toFixed(2), t.unpriced ? t.unpriced + (t.unpriced === 1 ? " job" : " jobs") + " on models without a price" : "all models priced"));
  }

  // --- jobs over time: stacked columns by outcome ---

  function jobsChart(jobs, now) {
    const bs = buckets(range, now).map((b) => ({ ...b, n: Object.fromEntries(OUTCOMES.map((o) => [o.id, 0])) }));
    for (const j of jobs) {
      const t = when(j);
      const b = bs.find((x) => t >= x.from && t < x.to);
      if (b && OUTCOME_OF[j.state]) b.n[OUTCOME_OF[j.state]]++;
    }
    const totals = bs.map((b) => OUTCOMES.reduce((s, o) => s + b.n[o.id], 0));
    const max = Math.max(1, ...totals);
    const step = max <= 4 ? 1 : Math.ceil(max / 4);
    const top = step * Math.ceil(max / step);
    const W = 640, H = 200, L = 32, B = 22, T = 8;
    const plotW = W - L - 4, plotH = H - B - T;
    const slot = plotW / bs.length;
    const bar = Math.min(24, slot * 0.7);
    const y = (v) => T + plotH - (v / top) * plotH;
    const chart = svg("svg", { viewBox: `0 0 ${W} ${H}`, class: "chart", role: "img", "aria-label": "Jobs per " + range.bucket + " by outcome" });
    for (let v = 0; v <= top; v += step) {
      chart.append(svg("line", { x1: L, x2: W - 4, y1: y(v), y2: y(v), class: "grid" }));
      chart.append(svg("text", { x: L - 6, y: y(v) + 4, class: "axis", "text-anchor": "end" }, String(v)));
    }
    const tip = h("div", { class: "tooltip", hidden: true });
    const labelEvery = Math.ceil(bs.length / 8);
    bs.forEach((b, i) => {
      const x = L + i * slot + (slot - bar) / 2;
      if ((bs.length - 1 - i) % labelEvery === 0) {
        chart.append(svg("text", { x: x + bar / 2, y: H - 6, class: "axis", "text-anchor": "middle" }, bucketLabel(b, range)));
      }
      let base = 0;
      const segs = OUTCOMES.filter((o) => b.n[o.id] > 0);
      segs.forEach((o, k) => {
        const v = b.n[o.id];
        const y0 = y(base), y1 = y(base + v);
        // A 2px surface gap between stacked segments.
        const gap = k > 0 ? 2 : 0;
        const hgt = Math.max(1, y0 - y1 - gap);
        const last = k === segs.length - 1;
        chart.append(last
          ? svg("path", { d: roundTop(x, y1, bar, hgt, Math.min(4, hgt)), class: "o-" + o.id })
          : svg("rect", { x, y: y1, width: bar, height: hgt, class: "o-" + o.id }));
        base += v;
      });
      // The hit target is the whole column slot.
      const hit = svg("rect", { x: L + i * slot, y: T, width: slot, height: plotH, class: "hit", tabindex: totals[i] ? 0 : -1 });
      const showTip = () => {
        set(tip, h("div", { class: "tip-title" }, bucketLabel(b, range) + (range.bucket === "hour" ? "" : ", " + b.from.getFullYear())),
          h("div", { class: "tip-row" }, h("strong", {}, String(totals[i])), " jobs"),
          ...OUTCOMES.filter((o) => b.n[o.id]).map((o) => h("div", { class: "tip-row" }, h("span", { class: "key o-" + o.id }), h("strong", {}, String(b.n[o.id])), " " + o.label.toLowerCase())));
        tip.hidden = false;
        const box = chart.getBoundingClientRect();
        const px = ((L + (i + 0.5) * slot) / W) * box.width;
        tip.classList.toggle("left", px > box.width / 2);
        tip.style.left = px + "px"; // CSSOM, which the CSP allows
      };
      hit.addEventListener("pointerenter", showTip);
      hit.addEventListener("focus", showTip);
      hit.addEventListener("pointerleave", () => { tip.hidden = true; });
      hit.addEventListener("blur", () => { tip.hidden = true; });
      chart.append(hit);
    });
    const legend = h("div", { class: "legend" }, OUTCOMES.map((o) => h("span", {}, h("span", { class: "swatch o-" + o.id }), o.label)));
    const table = h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, range.bucket === "hour" ? "Hour" : "Day"), h("th", {}, "Jobs"), OUTCOMES.map((o) => h("th", {}, o.label)))),
      h("tbody", {}, bs.filter((_, i) => totals[i]).map((b) => h("tr", {}, h("td", {}, bucketLabel(b, range)), h("td", {}, String(OUTCOMES.reduce((s, o) => s + b.n[o.id], 0))),
        OUTCOMES.map((o) => h("td", {}, String(b.n[o.id])))))));
    if (jobs.length === 0) return h("p", { class: "muted" }, "No jobs in this range.");
    return h("div", {}, legend, h("div", { class: "chart-wrap" }, chart, tip), h("details", {}, h("summary", {}, "Show as table"), h("div", { class: "table-wrap" }, table)));
  }

  // --- tokens by day and hour: a heatmap ---

  function heatmap(jobs, now) {
    // Up to a week, one row per day; beyond, one row per weekday (summed),
    // so the chart stays seven rows high.
    // The last 24 hours reach into yesterday: two rows.
    const days = range.bucket === "hour" ? 2 : Math.round(range.hours / 24);
    const byWeekday = days > 7;
    const today = new Date(now);
    today.setHours(0, 0, 0, 0);
    const rows = [];
    if (byWeekday) {
      const monday = new Date(2024, 0, 1); // a Monday, for weekday names
      for (let i = 0; i < 7; i++) {
        const d = new Date(monday);
        d.setDate(monday.getDate() + i);
        rows.push({ weekday: d.getDay(), label: d.toLocaleDateString([], { weekday: "short" }), long: d.toLocaleDateString([], { weekday: "long" }) + "s", cells: new Array(24).fill(0) });
      }
    } else {
      for (let i = days - 1; i >= 0; i--) {
        const d = new Date(today);
        d.setDate(today.getDate() - i);
        rows.push({ day: d, label: d.toLocaleDateString([], { month: "short", day: "numeric" }), long: d.toLocaleDateString([], { weekday: "short", month: "short", day: "numeric" }), cells: new Array(24).fill(0) });
      }
    }
    for (const j of jobs) {
      const t = when(j), u = tokens(j);
      let row;
      if (byWeekday) {
        row = rows.find((r) => r.weekday === t.getDay());
      } else {
        const d0 = new Date(t);
        d0.setHours(0, 0, 0, 0);
        row = rows.find((r) => r.day.getTime() === d0.getTime());
      }
      if (row) row.cells[t.getHours()] += u.in + u.out;
    }
    const max = Math.max(0, ...rows.flatMap((r) => r.cells));
    if (max === 0) return h("p", { class: "muted" }, "No model usage in this range.");
    // Six steps of one hue, light to dark; zero is the empty cell.
    const level = (v) => (v === 0 ? 0 : 1 + Math.min(5, Math.floor((v / max) * 6)));
    const cell = 18, gap = 3, L = 52, T = 16;
    const W = L + 24 * (cell + gap), H = T + rows.length * (cell + gap);
    const chart = svg("svg", { viewBox: `0 0 ${W} ${H}`, class: "chart heat", role: "img", "aria-label": "Model tokens per hour" });
    for (const hr of [0, 6, 12, 18]) chart.append(svg("text", { x: L + hr * (cell + gap), y: 11, class: "axis" }, String(hr).padStart(2, "0")));
    const tip = h("div", { class: "tooltip", hidden: true });
    rows.forEach((r, ri) => {
      const y = T + ri * (cell + gap);
      chart.append(svg("text", { x: L - 6, y: y + cell - 5, class: "axis", "text-anchor": "end" }, r.label));
      r.cells.forEach((v, hr) => {
        const x = L + hr * (cell + gap);
        const c = svg("rect", { x, y, width: cell, height: cell, rx: 3, class: "q" + level(v), tabindex: v ? 0 : -1 });
        const showTip = () => {
          set(tip, h("div", { class: "tip-title" }, r.long + ", " + String(hr).padStart(2, "0") + ":00"),
            h("div", { class: "tip-row" }, h("strong", {}, v.toLocaleString()), " tokens"));
          tip.hidden = false;
          const box = chart.getBoundingClientRect();
          const px = ((x + cell / 2) / W) * box.width;
          tip.classList.toggle("left", px > box.width / 2);
          tip.style.left = px + "px";
          tip.style.top = ((y / H) * box.height + 24) + "px";
        };
        c.addEventListener("pointerenter", showTip);
        c.addEventListener("focus", showTip);
        c.addEventListener("pointerleave", () => { tip.hidden = true; });
        c.addEventListener("blur", () => { tip.hidden = true; });
        chart.append(c);
      });
    });
    const scale = h("div", { class: "legend" }, "Fewer", ...[1, 2, 3, 4, 5, 6].map((q) => h("span", { class: "swatch q" + q, "aria-hidden": "true" })), "more tokens (most in one cell: " + max.toLocaleString() + ")");
    return h("div", {}, h("div", { class: "chart-wrap" }, chart, tip), scale,
      h("p", { class: "muted small" }, "A job's tokens count in the hour it started; a helper agent's count with its parent."));
  }

  // --- tables ---

  function summarize(jobs, key) {
    const by = new Map();
    for (const j of jobs) {
      const k = key(j);
      const s = by.get(k) || { name: k, runs: 0, ok: 0, failed: 0, stopped: 0, in: 0, out: 0, cost: 0, priced: true, ms: 0, timed: 0 };
      const u = tokens(j);
      s.runs++;
      if (j.state === "succeeded") s.ok++;
      if (j.state === "failed") s.failed++;
      if (OUTCOME_OF[j.state] === "stopped") s.stopped++;
      s.in += u.in;
      s.out += u.out;
      s.cost += u.cost;
      s.priced = s.priced && u.priced;
      if (j.started && j.finished) {
        s.ms += new Date(j.finished) - new Date(j.started);
        s.timed++;
      }
      by.set(k, s);
    }
    return [...by.values()].sort((a, b) => b.in + b.out - (a.in + a.out) || b.runs - a.runs);
  }

  function avg(s) {
    if (!s.timed) return "–";
    const ms = s.ms / s.timed;
    return fmtDuration(new Date(0).toISOString(), new Date(ms).toISOString());
  }

  function byHarness(jobs) {
    const rows = summarize(jobs, (j) => j.harness);
    if (rows.length === 0) return h("p", { class: "muted" }, "No jobs in this range.");
    return h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Harness", "Runs", "Succeeded", "Failed", "Stopped", "Tokens", "Avg. time", "Cost"].map((c, i) => h("th", { class: i ? "num" : "" }, c)))),
      h("tbody", {}, rows.map((s) => h("tr", {},
        h("td", {}, s.name), h("td", { class: "num" }, String(s.runs)), h("td", { class: "num" }, String(s.ok)),
        h("td", { class: "num" }, String(s.failed)), h("td", { class: "num" }, String(s.stopped)),
        h("td", { class: "num" }, compact(s.in + s.out)), h("td", { class: "num" }, avg(s)),
        h("td", { class: "num" }, s.priced ? "$" + s.cost.toFixed(2) : "no price"))))));
  }

  function byModel(jobs) {
    const rows = summarize(jobs.filter((j) => !j.parent), (j) => j.model || "(not recorded)");
    if (rows.length === 0) return h("p", { class: "muted" }, "No jobs in this range.");
    return h("div", {}, h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Model", "Runs", "Input tokens", "Output tokens", "Per run", "Cost"].map((c, i) => h("th", { class: i ? "num" : "" }, c)))),
      h("tbody", {}, rows.map((s) => h("tr", {},
        h("td", {}, s.name), h("td", { class: "num" }, String(s.runs)),
        h("td", { class: "num" }, compact(s.in)), h("td", { class: "num" }, compact(s.out)),
        h("td", { class: "num" }, compact((s.in + s.out) / s.runs)),
        h("td", { class: "num" }, s.priced ? "$" + s.cost.toFixed(2) : "no price")))))),
    rows.some((s) => s.name === "(not recorded)") && h("p", { class: "muted small" }, "Jobs from before this version have no model recorded."));
  }

  function runnerTable(runners) {
    if (runners.length === 0) return h("p", { class: "muted" }, h("a", { href: "#/runners" }, "Add a runner"), " to start.");
    return h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "Runner"), h("th", {}, "State"), h("th", { class: "num" }, "Busy"), h("th", {}, "Last contact"))),
      h("tbody", {}, runners.map((r) => {
        const st = r.status;
        return h("tr", {},
          h("td", {}, h("a", { href: "#/runners" }, r.name)),
          h("td", {}, st ? (st.killed ? badge("stopped", "killed") : badge("ready", "succeeded")) : badge("unreachable", "failed")),
          h("td", { class: "num" }, st ? st.running + " of " + st.maxConcurrent + (st.queued ? " (+" + st.queued + " queued)" : "") : "–"),
          h("td", {}, st ? fmtTime(st.lastContact) : "–"));
      }))));
  }

  function nextRuns() {
    const next = data.schedules.filter((s) => mine2(s) && s.state === "active" && s.nextRun)
      .sort((a, b) => new Date(a.nextRun) - new Date(b.nextRun)).slice(0, 5);
    if (next.length === 0) return h("p", { class: "muted" }, "Nothing scheduled.");
    return h("div", { class: "table-wrap" }, h("table", {},
      h("thead", {}, h("tr", {}, h("th", {}, "When"), h("th", {}, "Harness"), h("th", {}, "Runner"))),
      h("tbody", {}, next.map((s) => h("tr", {}, h("td", {}, fmtTime(s.nextRun)), h("td", {}, s.harness), h("td", {}, s.runner))))));
  }

  await load();
  every(15000, load);
}

// roundTop is a column of width w and height hgt with its top at y and
// rounded top corners (radius r); the baseline stays square.
function roundTop(x, y, w, hgt, r) {
  return `M${x},${y + hgt}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + hgt}Z`;
}
