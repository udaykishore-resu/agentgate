package fleet

import (
	"net/http"
)

// handleDashboard serves the fleet view. It is a single self-contained page
// that reads the same JSON APIs an operator would call, so there is no second
// data path to keep in step and nothing to build.
func (s *Service) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AgentGate Fleet</title>
<style>
  :root {
    --bg: #f7f7f8; --panel: #ffffff; --ink: #16181d; --muted: #616776;
    --line: #e3e5ea; --accent: #2f5bd7;
    --ok: #1a7f4b; --warn: #9a6400; --bad: #b3261e;
    --ok-bg: #e6f4ec; --warn-bg: #fdf3e0; --bad-bg: #fbe9e7;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #14161a; --panel: #1c1f25; --ink: #eceef2; --muted: #a2a9b8;
      --line: #2c3038; --accent: #7ea0f5;
      --ok: #6ddc9d; --warn: #f0c36d; --bad: #f2857c;
      --ok-bg: #1a2a22; --warn-bg: #2b2418; --bad-bg: #2c1c1b;
    }
  }
  * { box-sizing: border-box; }
  body { margin: 0; background: var(--bg); color: var(--ink);
    font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
  header { padding: 20px 24px; border-bottom: 1px solid var(--line); background: var(--panel);
    display: flex; align-items: baseline; gap: 16px; flex-wrap: wrap; }
  h1 { font-size: 17px; margin: 0; letter-spacing: -0.01em; }
  .sub { color: var(--muted); font-size: 12px; }
  main { padding: 20px 24px 48px; max-width: 1500px; margin: 0 auto; }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); gap: 12px; margin-bottom: 22px; }
  .card { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; padding: 14px 16px; }
  .card .k { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; }
  .card .v { font-size: 24px; font-weight: 600; margin-top: 4px; font-variant-numeric: tabular-nums; }
  .card .n { color: var(--muted); font-size: 12px; margin-top: 2px; }
  section { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; margin-bottom: 20px; overflow: hidden; }
  section > h2 { font-size: 13px; margin: 0; padding: 12px 16px; border-bottom: 1px solid var(--line);
    text-transform: uppercase; letter-spacing: 0.06em; color: var(--muted); }
  .scroll { overflow-x: auto; }
  table { border-collapse: collapse; width: 100%; min-width: 900px; }
  th, td { text-align: left; padding: 9px 12px; border-bottom: 1px solid var(--line); white-space: nowrap; }
  th { font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; color: var(--muted); font-weight: 600; }
  td.num { text-align: right; font-variant-numeric: tabular-nums; }
  tr:last-child td { border-bottom: none; }
  .pill { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 11px; font-weight: 600; }
  .healthy { background: var(--ok-bg); color: var(--ok); }
  .degraded { background: var(--warn-bg); color: var(--warn); }
  .unhealthy { background: var(--bad-bg); color: var(--bad); }
  .bar { height: 6px; border-radius: 3px; background: var(--line); width: 120px; overflow: hidden; }
  .bar > i { display: block; height: 100%; background: var(--ok); }
  .bar.low > i { background: var(--bad); }
  .bar.mid > i { background: var(--warn); }
  .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
  .issues { color: var(--muted); font-size: 12px; white-space: normal; }
  .controls { display: flex; gap: 8px; margin-left: auto; }
  select, button { font: inherit; padding: 5px 9px; border-radius: 7px; border: 1px solid var(--line);
    background: var(--panel); color: var(--ink); }
  button { cursor: pointer; }
  .empty { padding: 28px 16px; color: var(--muted); text-align: center; }
</style>
</head>
<body>
<header>
  <h1>AgentGate Fleet</h1>
  <span class="sub" id="meta">loading…</span>
  <span class="controls">
    <select id="env"><option value="">all environments</option><option>prod</option><option>staging</option><option>dev</option></select>
    <select id="health"><option value="">all health</option><option>healthy</option><option>degraded</option><option>unhealthy</option></select>
    <button id="refresh">Refresh</button>
  </span>
</header>
<main>
  <div class="cards" id="cards"></div>

  <section>
    <h2>Service level objectives</h2>
    <div class="scroll"><table id="slo"><thead><tr>
      <th>Objective</th><th class="num">SLI</th><th class="num">Target</th>
      <th>Error budget</th><th class="num">Remaining</th><th>Status</th><th>Runbook</th>
    </tr></thead><tbody></tbody></table></div>
  </section>

  <section>
    <h2>Agents</h2>
    <div class="scroll"><table id="agents"><thead><tr>
      <th>Agent</th><th>Team</th><th>Owner</th><th>Runtime</th><th>Prod version</th>
      <th class="num">Req/h</th><th class="num">Tokens/h</th><th class="num">Err %</th>
      <th>Telemetry</th><th class="num">Spend 24h</th><th>Cost centre</th><th>Health</th>
    </tr></thead><tbody></tbody></table></div>
    <div class="empty" id="agents-empty" hidden>No agents match this filter.</div>
  </section>

  <section>
    <h2>Chargeback by cost centre, last 24 hours</h2>
    <div class="scroll"><table id="charge"><thead><tr>
      <th>Cost centre</th><th class="num">Requests</th><th class="num">Input tokens</th>
      <th class="num">Output tokens</th><th class="num">Spend</th><th class="num">Cache savings</th>
    </tr></thead><tbody></tbody></table></div>
  </section>
</main>
<script>
const fmt = new Intl.NumberFormat();
const usd = n => "$" + (n || 0).toFixed(n >= 1 ? 2 : 4);
const pct = n => (100 * (n || 0)).toFixed(2) + "%";
const el = id => document.getElementById(id);

async function get(path) {
  const r = await fetch(path, { headers: { accept: "application/json" } });
  if (!r.ok) throw new Error(path + " returned " + r.status);
  return r.json();
}

function bar(v) {
  const cls = v >= 0.98 ? "" : v >= 0.9 ? " mid" : " low";
  const w = Math.max(0, Math.min(1, v || 0)) * 100;
  return '<span class="bar' + cls + '"><i style="width:' + w + '%"></i></span>';
}

function cards(agents, slo, charge) {
  const prod = agents.filter(a => a.prod_version).length;
  const bad = agents.filter(a => a.health !== "healthy").length;
  const spend = agents.reduce((s, a) => s + (a.spend_usd_last_24h || 0), 0);
  const worst = slo.reduce((m, o) => Math.min(m, o.budget_remaining_fraction ?? 1), 1);
  const untrusted = agents.filter(a => a.telemetry && a.telemetry.completeness < 0.98).length;
  el("cards").innerHTML = [
    ["Agents registered", fmt.format(agents.length), prod + " running in production"],
    ["Needing attention", fmt.format(bad), bad ? "see the health column" : "all green"],
    ["Spend, last 24h", usd(spend), "attributed to " + Object.keys(charge.by_cost_center || {}).length + " cost centres"],
    ["Tightest error budget", pct(worst), "across " + slo.length + " objectives"],
    ["Telemetry below bar", fmt.format(untrusted), "blocked from promotion"],
  ].map(([k, v, n]) => '<div class="card"><div class="k">' + k + '</div><div class="v">' + v + '</div><div class="n">' + n + '</div></div>').join("");
}

function renderSLO(rows) {
  el("slo").tBodies[0].innerHTML = rows.map(o => {
    const cls = o.error ? "degraded" : o.met ? "healthy" : "unhealthy";
    const label = o.error ? "no data" : o.met ? "met" : "breached";
    return "<tr><td>" + o.name + "</td>" +
      '<td class="num">' + (o.error ? "—" : pct(o.sli)) + "</td>" +
      '<td class="num">' + pct(o.target) + "</td>" +
      "<td>" + bar(o.budget_remaining_fraction) + "</td>" +
      '<td class="num">' + (o.error ? "—" : pct(o.budget_remaining_fraction)) + "</td>" +
      '<td><span class="pill ' + cls + '">' + label + "</span></td>" +
      '<td class="mono">' + (o.runbook || "") + "</td></tr>";
  }).join("");
}

function renderAgents(rows) {
  el("agents-empty").hidden = rows.length > 0;
  el("agents").tBodies[0].innerHTML = rows.map(a => {
    const t = a.telemetry || {};
    const issues = (a.issues || []).join("; ");
    return "<tr>" +
      '<td><div class="mono">' + a.identity + "</div>" +
      (issues ? '<div class="issues">' + issues + "</div>" : "") + "</td>" +
      "<td>" + a.team + "</td>" +
      "<td>" + (a.owner?.oncall || a.owner?.email || "—") + "</td>" +
      "<td>" + (a.runtime || "—") + "</td>" +
      '<td class="mono">' + (a.prod_version || "—") + "</td>" +
      '<td class="num">' + fmt.format(a.requests_last_hour || 0) + "</td>" +
      '<td class="num">' + fmt.format(a.tokens_last_hour || 0) + "</td>" +
      '<td class="num">' + pct(a.error_rate) + "</td>" +
      "<td>" + bar(t.completeness) + "</td>" +
      '<td class="num">' + usd(a.spend_usd_last_24h) + "</td>" +
      '<td class="mono">' + (a.owner?.cost_center || "—") + "</td>" +
      '<td><span class="pill ' + a.health + '">' + a.health + "</span></td>" +
      "</tr>";
  }).join("");
}

function renderCharge(data) {
  const byCC = {};
  for (const r of data.rollups || []) {
    const cc = r.cost_center || "UNATTRIBUTED";
    byCC[cc] = byCC[cc] || { requests: 0, in: 0, out: 0, cost: 0, saved: 0 };
    byCC[cc].requests += r.requests || 0;
    byCC[cc].in += r.input_tokens || 0;
    byCC[cc].out += r.output_tokens || 0;
    byCC[cc].cost += r.cost_usd || 0;
    byCC[cc].saved += r.savings_usd || 0;
  }
  const rows = Object.entries(byCC).sort((a, b) => b[1].cost - a[1].cost);
  el("charge").tBodies[0].innerHTML = rows.length ? rows.map(([cc, v]) =>
    '<tr><td class="mono">' + cc + '</td><td class="num">' + fmt.format(v.requests) +
    '</td><td class="num">' + fmt.format(v.in) + '</td><td class="num">' + fmt.format(v.out) +
    '</td><td class="num">' + usd(v.cost) + '</td><td class="num">' + usd(v.saved) + "</td></tr>"
  ).join("") : '<tr><td colspan="6" class="empty">No usage recorded yet.</td></tr>';
}

async function load() {
  const env = el("env").value, health = el("health").value;
  const q = new URLSearchParams();
  if (env) q.set("env", env);
  if (health) q.set("health", health);
  try {
    const [agents, slo, charge] = await Promise.all([
      get("/api/v1/agents?" + q.toString()),
      get("/api/v1/slo"),
      get("/api/v1/chargeback?period=day"),
    ]);
    cards(agents.agents || [], slo.objectives || [], charge);
    renderSLO(slo.objectives || []);
    renderAgents(agents.agents || []);
    renderCharge(charge);
    el("meta").textContent = "updated " + new Date().toLocaleTimeString() +
      " · " + (agents.count || 0) + " agents · SLO window " + (slo.window || "");
  } catch (e) {
    el("meta").textContent = "could not load: " + e.message;
  }
}

el("refresh").addEventListener("click", load);
el("env").addEventListener("change", load);
el("health").addEventListener("change", load);
load();
setInterval(load, 30000);
</script>
</body>
</html>
`
