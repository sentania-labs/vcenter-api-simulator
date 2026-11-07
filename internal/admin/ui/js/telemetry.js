import { api, html, raw, join, badge, banner, int, bytes, hm, hms, dateTime, clone, plural } from './lib.js';

// Telemetry: metric scope, latency, fault rules, scenario presets, timeline.
// Every edit goes into the single pending draft (PUT /api/draft); nothing
// reaches the API until Apply, and a telemetry-only apply never rebuilds the
// vCenter, so identities and history are untouched.

const METHODS = ['VsanPerfQueryNodeInformation', 'VsanClusterGetConfig', 'QueryVsanManagedDisks', 'VsanQueryVcClusterHealthSummary'];
const FAULTS = [['invalid-argument', 'InvalidArgument'], ['not-supported', 'NotSupported'], ['system-error', 'SystemError'], ['timeout', 'timeout (no answer)']];
const READS = {
  'system-error': ['SystemError', 'reported as a collection problem'],
  'invalid-argument': ['InvalidArgument', 'treated as “kind not available”, silent'],
  'not-supported': ['NotSupported', 'treated as “kind not available”, silent'],
  timeout: ['timeout', 'no answer for 125 s; the pack’s 120 s request timeout fires and the cluster is reported failed'],
};

// the add-rule form is local state: it is not part of the draft until Add rule
let form = { kind: 'family', family: '', method: METHODS[0], fault: 'system-error', clusters: [], from: '', to: '' };
let selectedRule = -1;
let famAdd = '';

const delay = (sc, which) => (which === 'sdk' ? sc.latency?.sdk : sc.latency?.perfQuery) || {};
const famDelay = (sc, f) => sc.latency?.families?.[f] || {};
const dtext = (d) => (d && (d.fixedMs || d.jitterMs) ? `${d.fixedMs || 0} ms +0–${d.jitterMs || 0}` : 'none');
const num = (v) => Math.max(0, parseInt(v, 10) || 0);

function setDelay(sc, which, field, v) {
  sc.latency = sc.latency || {};
  const key = which === 'sdk' ? 'sdk' : 'perfQuery';
  const d = { ...(sc.latency[key] || {}), [field]: num(v) };
  if (!d.fixedMs && !d.jitterMs) delete sc.latency[key]; else sc.latency[key] = d;
}
function ruleText(f) {
  const m = f.family ? `${f.family}:*` : f.method;
  const where = f.clusters?.length ? plural(f.clusters.length, 'cluster') : 'all clusters';
  return `${m} → ${(READS[f.fault] || [f.fault])[0]} on ${where}`;
}
const chgText = (c) => (c.after === 'added' || c.after === 'removed' ? `${c.object} ${c.field} ${c.after}` : `${c.object} ${c.field} ${c.before} → ${c.after}`);
// 24 h UTC text: "YYYY-MM-DD HH:MM" (a T works too); the Z is implied
const utc = (v) => { const m = /^(\d{4}-\d{2}-\d{2})[ T](\d{1,2}):(\d{2})$/.exec((v || '').trim()); return m ? `${m[1]}T${m[2].padStart(2, '0')}:${m[3]}:00Z` : undefined; };

async function save(ctx, data, mutate, scope) {
  const sc = clone(data.draft.scenario);
  mutate(sc);
  const r = await ctx.run(() => api('/draft', { method: 'PUT', body: { scenario: sc, scope: scope ?? data.draft.scope } }));
  await ctx.refresh();
  return r;
}

// ---- cards ---------------------------------------------------------------------
function scopeCard(ctx, d) {
  const st = ctx.state;
  const p = st.projection;
  const scope = d.draft.scope;
  const a = p.all; const b = p.dashboard;
  const chg = (x, y) => (x ? `${y < x ? '−' : '+'}${Math.abs(Math.round(((y - x) / x) * 100))}%` : '—');
  const rows = [['PerfQuery calls', a.calls, b.calls, int], ['Perf entities', a.entities, b.entities, int], ['Series returned', a.series, b.series, int], ['Response bytes', a.bytes, b.bytes, (n) => '~' + bytes(n)]];
  const skipped = p.families.filter((f) => !f.dashboard && f.behaviour === 'data').sort((x, y) => y.objects - x.objects).slice(0, 2);
  const observed = st.activity.families.length;
  const sel = scope === 'dashboard' ? b.kinds : a.kinds;
  let warn = '';
  if (!observed) warn = banner('info', html`<b>No collector traffic yet.</b> Once the pack queries this endpoint, the kinds it asks for are compared with the selected scope here.`);
  else if (observed !== sel) warn = banner('warn', html`<b>The MP decides what it queries.</b> In the last hour the collector asked for ${observed} kinds; the selected scope projects ${sel}. Set the MP adapter instance to ${scope === 'dashboard' ? 'dashboard' : 'all'} metrics too, or the projection will not match traffic.`);
  else warn = banner('ok', html`<b>Traffic matches the scope.</b> The collector asked for ${observed} kinds in the last hour.`);
  const opt = (id, title, kinds, text) => html`<label class="opt${scope === id ? ' is-on' : ''}" style="cursor:pointer"><input type="radio" name="scope" class="sr-only" value="${id}" ${scope === id ? 'checked' : ''} data-chg="scope" data-k="scope-${id}"><span class="dot"></span><span class="t">${title}</span><span class="mono subtle">${kinds} kinds</span><span class="s">${text}</span></label>`;
  return html`<section class="lat-card" aria-labelledby="tl-scope"><h2 class="lat-card-title" id="tl-scope">Collector metric scope <span class="aside">projections only</span></h2>
    <div class="opts" role="radiogroup" aria-label="Collector metric scope">
      ${opt('all', 'All modeled kinds', a.kinds, 'What the MP queries when its instance is set to all metrics. The simulator serves every kind either way.')}
      ${opt('dashboard', 'Dashboard-only', b.kinds, 'Kinds referenced by shipped MP views, plus the ESA dedup kinds. The list is carried from the recon report, not read from the pack at run time.')}
    </div>
    <table class="lat-table"><thead><tr><th>Per cycle (applied topology)</th><th class="r">All</th><th class="r">Dashboard</th><th class="r">Change</th></tr></thead><tbody>
      ${rows.map(([k, x, y, f]) => html`<tr><td class="ink">${k}</td><td class="num r">${f(x)}</td><td class="num r">${f(y)}</td><td class="num r">${chg(x, y)}</td></tr>`)}</tbody></table>
    <div class="anno">Scope saves round trips, not metrics within a kind: the perf API returns every metric of a kind per call.${skipped.length ? ` Largest skipped kinds: ${skipped.map((f) => `${f.family} (${int(f.objects)})`).join(', ')}.` : ''} Bytes are an estimate.</div>
    ${warn}</section>`;
}

function presetsCard(ctx, d) {
  const list = d.presets.filter((x) => x.kind === 'telemetry');
  const has = ctx.state.pending.exists;
  return html`<section class="lat-card" aria-labelledby="tl-pre"><h2 class="lat-card-title" id="tl-pre">Scenario presets <span class="aside">latency and fault rules only; topology stays</span></h2>
    ${has ? html`<div class="anno">A draft is pending. Loading a preset replaces the faults and latency in it; its topology edits stay.</div>` : ''}
    <div class="stack">${list.map((x) => html`<div class="well row" style="align-items:flex-start"><div class="grow"><b>${x.title}</b><div class="anno">${x.description}</div></div><button type="button" class="lat-btn lat-btn--sm" data-act="preset" data-arg="${x.name}">Load into draft</button></div>`)}</div></section>`;
}

function latencyCard(ctx, d) {
  const sc = d.draft.scenario; const ap = d.draft.applied;
  const was = (a, b, field) => (a?.[field] || 0) !== (b?.[field] || 0);
  const field = (label, which, f, hint) => {
    const cur = delay(sc, which); const old = delay(ap, which);
    const ch = was(cur, old, f);
    return html`<div><label class="lat-label" for="lat-${which}-${f}">${label}</label><input class="lat-input num${ch ? ' is-changed' : ''}" id="lat-${which}-${f}" type="number" min="0" step="1" value="${cur[f] || 0}" data-chg="lat" data-which="${which}" data-field="${f}" data-k="lat-${which}-${f}">${ch ? html`<div class="lat-hint">was ${old[f] || 0}</div>` : hint ? html`<div class="lat-hint">${hint}</div>` : ''}</div>`;
  };
  const fams = Object.keys(sc.latency?.families || {}).sort();
  const avail = d.families.filter((f) => !fams.includes(f.family));
  const act = ap.latency || {};
  const aside = dtext(act.perfQuery) === 'none' && dtext(act.sdk) === 'none' && !Object.keys(act.families || {}).length ? 'none active' : `${dtext(act.perfQuery)} active`;
  return html`<section class="lat-card" aria-labelledby="tl-lat"><h2 class="lat-card-title" id="tl-lat">Latency <span class="aside">${aside} · edits wait for Apply</span></h2>
    <div class="anno"><b>VsanPerfQueryPerf</b> · every performance query</div>
    <div class="fields c2">${field('Fixed delay (ms)', 'perf', 'fixedMs')}${field('Jitter, adds 0 to N ms', 'perf', 'jitterMs')}</div>
    <div class="anno"><b>/sdk</b> · every inventory call</div>
    <div class="fields c2">${field('Fixed delay (ms)', 'sdk', 'fixedMs')}${field('Jitter, adds 0 to N ms', 'sdk', 'jitterMs')}</div>
    <div class="divider"></div>
    <div class="row"><b class="grow">Per-family overrides</b><span class="anno">replace the perf-query delay for one kind</span></div>
    ${fams.length ? html`<table class="lat-table"><thead><tr><th>Family</th><th class="r">Fixed ms</th><th class="r">Jitter ms</th><th></th></tr></thead><tbody>
      ${fams.map((f) => { const cur = famDelay(sc, f); const old = ap.latency?.families?.[f]; const ch = !old || old.fixedMs !== cur.fixedMs || old.jitterMs !== cur.jitterMs;
        return html`<tr><td class="ink mono">${f}</td>
          <td class="r"><input class="lat-input num${ch ? ' is-changed' : ''}" style="width:78px" type="number" min="0" value="${cur.fixedMs || 0}" aria-label="${f} fixed ms" data-chg="fam" data-fam="${f}" data-field="fixedMs" data-k="fam-${f}-f"></td>
          <td class="r"><input class="lat-input num${ch ? ' is-changed' : ''}" style="width:78px" type="number" min="0" value="${cur.jitterMs || 0}" aria-label="${f} jitter ms" data-chg="fam" data-fam="${f}" data-field="jitterMs" data-k="fam-${f}-j"></td>
          <td class="r"><button type="button" class="lat-btn lat-btn--ghost lat-btn--sm" data-act="famDel" data-arg="${f}" aria-label="Remove override for ${f}">Remove</button></td></tr>`; })}</tbody></table>` : html`<div class="anno">None. Every kind uses the perf-query delay.</div>`}
    <div class="row"><div class="sel-wrap grow"><select class="lat-select" style="width:100%" aria-label="Family to override" data-chg="famPick" data-k="fam-pick"><option value="">Add a family override…</option>${avail.map((f) => html`<option value="${f.family}" ${famAdd === f.family ? 'selected' : ''}>${f.family}</option>`)}</select></div><button type="button" class="lat-btn lat-btn--sm" data-act="famAdd" ${famAdd ? '' : 'disabled'}>Add</button></div>
    <div class="anno">Delays run before the request reaches the service, so one slow kind never stalls another cluster. Jitter is uniform from 0 to N ms. Not modeled: max concurrency and per-cluster latency.</div></section>`;
}

function faultsCard(ctx, d) {
  const sc = d.draft.scenario; const ap = d.draft.applied;
  const faults = sc.faults || [];
  const enc = (f) => JSON.stringify(f);
  const appliedSet = new Set((ap.faults || []).map(enc));
  const clusters = d.tree.datacenters.flatMap((dc) => dc.clusters.filter((c) => c.state !== 'removed').map((c) => c.key));
  const shown = selectedRule >= 0 && faults[selectedRule] ? faults[selectedRule].fault : form.fault;
  const seg = form.kind;
  return html`<section class="lat-card" aria-labelledby="tl-flt"><h2 class="lat-card-title" id="tl-flt">Fault rules <span class="aside">${faults.length} in the draft · ${(ap.faults || []).length} active</span></h2>
    ${faults.length ? html`<div class="scroll"><table class="lat-table"><thead><tr><th>Rule</th><th>Match → fault</th><th>Where · window (UTC)</th><th></th></tr></thead><tbody>
      ${faults.map((f, i) => html`<tr class="is-click${i === selectedRule ? ' is-sel' : ''}${appliedSet.has(enc(f)) ? '' : ' is-pending'}" data-act="pick" data-arg="${i}"><td class="mono ink">F${i + 1}${appliedSet.has(enc(f)) ? '' : html` ${badge('new', 'warn')}`}</td><td class="wrap"><span class="mono ink">${f.family ? f.family + ':*' : f.method}</span><div class="anno mono">${(READS[f.fault] || [f.fault])[0]}</div></td><td>${f.clusters?.length ? (f.clusters.length === 1 ? f.clusters[0] : plural(f.clusters.length, 'cluster')) : 'all clusters'}${f.from || f.to ? html`<div class="anno mono">${f.from ? dateTime(f.from) : '…'} → ${f.to ? dateTime(f.to) : '…'}</div>` : ''}</td>
        <td class="r"><button type="button" class="lat-btn lat-btn--ghost lat-btn--sm" data-act="ruleDel" data-arg="${i}" aria-label="Delete rule F${i + 1}">Delete…</button></td></tr>`)}</tbody></table></div>`
      : html`<div class="empty">No fault rules. Every query is answered.</div>`}
    <h2 class="lat-card-title" style="margin-top:4px">Add rule</h2>
    <div class="fields c3">
      <div><label class="lat-label" for="fr-kind">Match by</label><div class="sel-wrap"><select class="lat-select" id="fr-kind" data-chg="form" data-field="kind" data-k="fr-kind"><option value="family" ${seg === 'family' ? 'selected' : ''}>Entity family</option><option value="method" ${seg === 'method' ? 'selected' : ''}>Method</option></select></div></div>
      ${seg === 'family'
        ? html`<div><label class="lat-label" for="fr-fam">Entity family</label><div class="sel-wrap"><select class="lat-select" id="fr-fam" data-chg="form" data-field="family" data-k="fr-fam"><option value="">Choose…</option>${d.families.map((f) => html`<option value="${f.family}" ${form.family === f.family ? 'selected' : ''}>${f.family}</option>`)}</select></div><div class="lat-hint">a VsanPerfQueryPerf wildcard of this kind</div></div>`
        : html`<div><label class="lat-label" for="fr-meth">Method</label><div class="sel-wrap"><select class="lat-select" id="fr-meth" data-chg="form" data-field="method" data-k="fr-meth">${METHODS.map((m) => html`<option ${form.method === m ? 'selected' : ''}>${m}</option>`)}</select></div></div>`}
      <div><label class="lat-label" for="fr-fault">Fault</label><div class="sel-wrap"><select class="lat-select" id="fr-fault" data-chg="form" data-field="fault" data-k="fr-fault">${FAULTS.map(([v, l]) => html`<option value="${v}" ${form.fault === v ? 'selected' : ''}>${l}</option>`)}</select></div></div>
      <div style="grid-column: span 3"><label class="lat-label" for="fr-cl">Clusters</label><select multiple size="8" class="lat-select" style="width:100%;padding-right:8px" id="fr-cl" data-chg="form" data-field="clusters" data-k="fr-cl">${clusters.map((c) => html`<option ${form.clusters.includes(c) ? 'selected' : ''}>${c}</option>`)}</select><div class="lat-hint">${form.clusters.length ? html`<b>${form.clusters.length} of ${clusters.length} clusters selected</b>` : `none selected = the rule applies to all ${clusters.length} clusters`}</div></div>
      <div><label class="lat-label" for="fr-from">From (UTC 24 h, optional)</label><input class="lat-input" type="text" placeholder="YYYY-MM-DD HH:MM" inputmode="numeric" autocomplete="off" id="fr-from" value="${form.from}" data-chg="form" data-field="from" data-k="fr-from"></div>
      <div><label class="lat-label" for="fr-to">To (UTC 24 h, optional)</label><input class="lat-input" type="text" placeholder="YYYY-MM-DD HH:MM" inputmode="numeric" autocomplete="off" id="fr-to" value="${form.to}" data-chg="form" data-field="to" data-k="fr-to"></div>
      <div style="align-self:end"><button type="button" class="lat-btn lat-btn--primary" data-act="ruleAdd" ${seg === 'family' && !form.family ? 'disabled' : ''}>Add rule to draft</button></div>
    </div>
    <div class="anno">A window snaps outward to 5-minute buckets: From down, To up, so the fault starts and stops on a bucket boundary. Rules are present or removed; there is no per-rule on/off switch or rate.</div>
    <div class="well" style="font-size:12px"><div class="row"><b>How the MP reads this fault</b><span class="anno">reference pack perfsvc.py, as recon recorded it</span></div>
      <table class="lat-table" style="margin-top:4px"><tbody>${Object.entries(READS).map(([k, [n, t]]) => html`<tr><td class="mono ink">${n}</td><td class="wrap">${t} ${shown === k ? badge('selected', 'accent') : ''}</td></tr>`)}</tbody></table></div></section>`;
}

function timelineCard(ctx, d) {
  const rev = d.review; const st = ctx.state;
  const pend = rev ? rev.changes.map(chgText).join(' · ') : '';
  return html`<section class="lat-card" aria-labelledby="tl-time"><h2 class="lat-card-title" id="tl-time">Scenario timeline <span class="aside">every change lands on a 5-minute bucket boundary</span></h2>
    <table class="lat-table"><thead><tr><th>Bucket (UTC)</th><th>Change</th><th>Area</th><th>History</th><th>Identities</th><th class="r">Version</th></tr></thead><tbody>
    ${rev && st.pending.exists ? html`<tr class="is-pending"><td class="mono ink">${hm(rev.boundary)}</td><td class="ink wrap">${pend || 'pending change'} ${badge('pending', 'warn')}</td><td>${rev.rebuildsVcenter ? 'Topology' : 'Telemetry'}</td><td>preserved</td><td>${rev.identityChurn ? 'changes' : 'unchanged'}</td><td class="num r">v${rev.to}</td></tr>` : ''}
    ${d.timeline.slice(0, 12).map((t) => html`<tr><td class="mono ink">${hm(t.bucket)}</td><td class="ink wrap">${t.text}</td><td>${t.area}</td><td>${t.history}</td><td>${t.identities}</td><td class="num r">v${t.version}</td></tr>`)}</tbody></table></section>`;
}

function actionBar(ctx, d) {
  const st = ctx.state; const rev = d.review;
  if (!st.pending.exists || !rev) return '';
  if (st.pending.scheduled) return '';
  const tele = rev.changes.filter((c) => c.area === 'Telemetry');
  const topo = rev.topologyChanges;
  const text = tele.map(chgText).join(' · ');
  const n = rev.topologyChanges + rev.telemetryChanges;
  return html`<div class="well row" role="group" aria-label="Pending changes on this tab" style="gap:12px">
    <span class="grow"><b>${plural(n, 'unapplied change')}:</b> ${text || 'see the review'}${topo ? html` · ${plural(topo, 'topology change')} in the same draft, so review it first` : ''}. Edits wait here, land at a bucket boundary, and never reset history.</span>
    <button type="button" class="lat-btn lat-btn--sm" data-act="discard">Discard…</button>
    ${topo ? html`<button type="button" class="lat-btn lat-btn--primary lat-btn--sm" data-act="reviewAll">Review ${n} changes…</button>` : html`<button type="button" class="lat-btn lat-btn--primary lat-btn--sm" data-act="apply">Apply ${plural(n, 'change')} at ${hms(rev.boundary)}</button>`}</div>`;
}

export default {
  id: 'telemetry',
  title: 'Telemetry',
  pollMs: 10000,
  async load(ctx) {
    const [draft, presets, families, tree, timeline] = await Promise.all([api('/draft'), api('/presets'), api('/families'), api('/tree'), api('/timeline')]);
    let review = null;
    if (draft.pending.exists && !draft.pending.scheduled) { try { review = await api('/review'); } catch (e) { review = null; } }
    return { draft, presets, families, tree, timeline, review };
  },
  render(ctx, d) {
    if (!ctx.state) return html`<div class="empty">Loading…</div>`;
    return html`${actionBar(ctx, d)}
    <div class="g" style="grid-template-columns:1fr 1fr 1.15fr">
      <div class="g" style="align-content:start">${scopeCard(ctx, d)}${presetsCard(ctx, d)}</div>
      <div class="g" style="align-content:start">${latencyCard(ctx, d)}</div>
      <div class="g" style="align-content:start">${faultsCard(ctx, d)}</div>
    </div>
    ${timelineCard(ctx, d)}`;
  },
  // a scheduled change owns the draft: lock the page the way Topology is locked
  mounted(ctx, d, page) {
    if (ctx.state && ctx.state.pending && ctx.state.pending.scheduled) page.querySelectorAll('input, select, textarea, button').forEach((e) => { e.disabled = true; });
  },
  actions: {
    async scope(ctx, el, ev, d) { await save(ctx, d, () => {}, el.value); },
    async lat(ctx, el, ev, d) { await save(ctx, d, (sc) => setDelay(sc, el.dataset.which, el.dataset.field, el.value)); },
    async fam(ctx, el, ev, d) {
      await save(ctx, d, (sc) => { const cur = sc.latency.families[el.dataset.fam] || {}; sc.latency.families[el.dataset.fam] = { ...cur, [el.dataset.field]: num(el.value) }; });
    },
    famPick(ctx, el) { famAdd = el.value; ctx.rerender(); },
    async famAdd(ctx, el, ev, d) {
      const f = famAdd; famAdd = '';
      await save(ctx, d, (sc) => { sc.latency = sc.latency || {}; sc.latency.families = sc.latency.families || {}; sc.latency.families[f] = { fixedMs: 100, jitterMs: 0 }; });
    },
    async famDel(ctx, el, ev, d) { await save(ctx, d, (sc) => { delete sc.latency.families[el.dataset.arg]; }); },
    form(ctx, el) {
      const f = el.dataset.field;
      form[f] = f === 'clusters' ? [...el.selectedOptions].map((o) => o.value) : el.value;
      ctx.rerender();
    },
    pick(ctx, el, ev) { if (ev.target.closest('button')) return; selectedRule = selectedRule === +el.dataset.arg ? -1 : +el.dataset.arg; ctx.rerender(); },
    async ruleAdd(ctx, el, ev, d) {
      const rule = { fault: form.fault };
      if (form.kind === 'family') rule.family = form.family; else rule.method = form.method;
      if (form.clusters.length) rule.clusters = form.clusters;
      for (const k of ['from', 'to']) {
        if (form[k] && !utc(form[k])) { ctx.toast(`${k === 'from' ? 'From' : 'To'} must be a UTC time like 2026-11-07 14:00 (24 h).`, 'bad'); return; }
      }
      if (form.from) rule.from = utc(form.from);
      if (form.to) rule.to = utc(form.to);
      const r = await save(ctx, d, (sc) => { sc.faults = [...(sc.faults || []), rule]; });
      if (r) { selectedRule = -1; form = { ...form, family: '', clusters: [], from: '', to: '' }; ctx.toast('Rule added to the draft. It lands at the next bucket after Apply.', 'ok'); ctx.rerender(); }
    },
    async ruleDel(ctx, el, ev, d) {
      ev.stopPropagation();
      const i = +el.dataset.arg;
      const f = d.draft.scenario.faults[i];
      const active = (d.draft.applied.faults || []).some((x) => JSON.stringify(x) === JSON.stringify(f));
      const ok = await ctx.confirm({
        title: `Delete fault rule F${i + 1}?`,
        body: html`<ul><li><span class="mono">${ruleText(f)}</span>${f.from || f.to ? ' (time-boxed)' : ''}. ${active ? 'The rule is active now.' : 'The rule is only in the draft; it never reached the API.'}</li>
          <li>Joins the pending draft and the Telemetry banner; it stops at the next 5-minute bucket after you Apply. History and identities are untouched.</li>
          <li>To keep the rule and stop it later, set a To time instead of deleting it.</li></ul>`,
        confirmLabel: `Delete rule F${i + 1}`, danger: true,
      });
      if (!ok) return;
      selectedRule = -1;
      await save(ctx, d, (sc) => { sc.faults.splice(i, 1); });
    },
    async preset(ctx, el, ev, d) {
      await ctx.run(() => api('/draft/preset', { body: { name: el.dataset.arg } }), 'Preset loaded into the draft.');
      await ctx.refresh();
    },
    async apply(ctx) {
      const r = await ctx.run(() => api('/apply', { body: { when: 'boundary' } }));
      if (r) ctx.toast(r.message + '. The API keeps the old settings until then.', 'ok');
      await ctx.refresh();
    },
    reviewAll(ctx) { ctx.go('#/topology/review'); },
    async discard(ctx) {
      const ok = await ctx.confirm({ title: 'Discard the pending draft?', body: html`<ul><li>Every unapplied edit in the draft goes, topology edits included. The API is unaffected.</li></ul>`, confirmLabel: 'Discard draft', danger: true });
      if (!ok) return;
      await ctx.run(() => api('/draft', { method: 'DELETE' }), 'Draft discarded.');
      selectedRule = -1;
      await ctx.refresh();
    },
  },
};
