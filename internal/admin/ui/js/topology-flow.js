// Topology flow pages: the Quick generator (#/topology/generator) and the
// Review-before-apply page (#/topology/review). Imported by topology.js, which
// routes to `generator` and `review`. Actions are prefixed gen. and rev.
import { api, ApiError, html, raw, join, badge, banner, tile, int, signed, bytes, hms, hm, clone } from './lib.js';

const num = (v) => { const n = parseInt(String(v ?? '').replace(/[^0-9-]/g, ''), 10); return Number.isFinite(n) ? n : 0; };
const optNum = (v) => (String(v ?? '').trim() === '' ? null : num(v));

// ---------------------------------------------------------------------------
// generator
// ---------------------------------------------------------------------------
let form = null;          // the form's values, strings, kept across redraws
let formVersion = -1;     // the applied topology version the form was seeded from
let pv = null;            // last good preview
let pvErr = '';           // last preview error (422 message)
let pvBusy = false;
let pvSeq = 0;
let pvTimer = 0;
let genBusy = false;
let draftInfo = null;     // GET /api/draft at load time

function seedForm(scn, projection) {
  const d = scn.defaults || {};
  const h = d.host || {};
  const v = d.vms || {};
  const primaryStates = [];
  for (const dc of scn.datacenters || []) {
    for (let i = 1; i <= dc.clusters; i++) {
      const key = `${dc.name}/cluster-${String(i).padStart(2, '0')}`;
      if ((scn.removed || []).includes(key)) continue;
      const override = (scn.clusters || []).find((c) => c.key === key);
      primaryStates.push((override?.statsPrimary ?? d.statsPrimary) !== false);
    }
  }
  form = {
    seed: scn.seed || '',
    dcs: (scn.datacenters || []).map((x) => ({ name: x.name, clusters: String(x.clusters ?? 1), nfs: x.nfs || [], iscsi: x.iscsi || [] })),
    hosts: String(d.hosts ?? 4),
    architecture: projection?.clusters > 0 && projection.osaClusters === projection.clusters ? 'osa' : projection?.clusters > 0 && projection.osaClusters === 0 ? 'esa' : (d.architecture || 'esa'),
    statsPrimary: primaryStates.length && primaryStates.every((x) => x === primaryStates[0]) ? primaryStates[0] : d.statsPrimary !== false,
    vmsPerHost: String(v.perHost ?? 2),
    disksPerVM: String(v.disksPerVM ?? 1),
    claimedDevices: String(h.claimedDevices ?? 1),
    localDevices: String(h.localDevices ?? 1),
    threads: String(h.threads ?? 32),
    domWorlds: String(h.domWorlds ?? 66),
    vsanPnics: String(h.vsanPnics ?? 2),
    dedup: d.dedup !== false,
    compression: d.compression !== false,
  };
  if (!form.dcs.length) form.dcs = [{ name: 'dc-01', clusters: '1' }];
}

function params() {
  return {
    seed: form.seed.trim(),
    datacenters: form.dcs.map((d) => ({ name: d.name.trim(), clusters: num(d.clusters), ...(d.nfs?.length ? { nfs: d.nfs } : {}), ...(d.iscsi?.length ? { iscsi: d.iscsi } : {}) })),
    hosts: num(form.hosts),
    architecture: form.architecture,
    statsPrimary: form.statsPrimary,
    vmsPerHost: optNum(form.vmsPerHost),
    disksPerVM: num(form.disksPerVM),
    claimedDevices: num(form.claimedDevices),
    localDevices: optNum(form.localDevices),
    threads: num(form.threads),
    domWorlds: optNum(form.domWorlds),
    vsanPnics: num(form.vsanPnics),
    dedup: form.dedup,
    compression: form.compression,
  };
}

function schedulePreview(ctx, delay = 300) {
  clearTimeout(pvTimer);
  pvBusy = true;
  pvTimer = setTimeout(() => runPreview(ctx), delay);
}
async function runPreview(ctx) {
  const mine = ++pvSeq;
  pvBusy = true;
  try {
    const r = await api('/preview/generate', { body: params() });
    if (mine !== pvSeq) return;
    pv = r; pvErr = '';
  } catch (e) {
    if (mine !== pvSeq) return;
    pvErr = e.message || String(e);
  }
  pvBusy = false;
  ctx.rerender();
}

function field(label, key, hint = '', cls = '') {
  return html`<div><label class="lat-label" for="gen-${key}">${label}</label><input class="lat-input num ${cls}" id="gen-${key}" inputmode="numeric" autocomplete="off" data-k="gen-${key}" data-inp="gen.set" data-f="${key}" value="${form[key]}">${hint ? html`<div class="lat-hint">${hint}</div>` : ''}</div>`;
}

function famRows(p, scope) {
  // largest families that carry objects; the rest collapse into one line
  const inScope = (f) => scope !== 'dashboard' || f.dashboard;
  const rows = p.families.filter((f) => f.objects > 0 && inScope(f));
  const top = rows.slice(0, 8);
  const rest = rows.slice(8);
  const hosts = p.hosts || 1;
  const per = (f) => {
    if (f.objects === p.clusters) return 'cluster';
    if (f.objects % hosts === 0) return `${int(f.objects / hosts)} / host`;
    if (f.objects === p.vms) return 'VM';
    return '—';
  };
  return { top, rest, per };
}

function genRender(ctx, data) {
  const st = ctx.state;
  const next = (st?.topology.version ?? 0) + 1;
  const p = pv?.projection;
  const sel = pv?.selected;
  const pending = draftInfo?.pending;
  const clusters = form.dcs.reduce((s, d) => s + num(d.clusters), 0);
  const sameSeed = st && form.seed.trim() === st.topology.seed;
  const topoPresets = (data.presets || []).filter((x) => x.kind === 'topology');
  const fam = p ? famRows(p, pv.scope) : null;
  const emptyCalls = p ? p.emptyCalls : 0;
  const canCreate = !pvErr && !pvBusy && !!pv && !genBusy;
  return html`
  <div class="page-head"><span class="page-title">Quick generator</span>${badge('new topology draft')}
    <span class="anno">Creates a topology draft. The API keeps serving v${st?.topology.version} until you review and apply it.</span><span class="grow"></span>
    <button type="button" class="lat-btn" data-act="gen.cancel">Cancel</button>
    <button type="button" class="lat-btn lat-btn--primary" data-act="gen.create" ${canCreate ? '' : 'disabled'}>Create draft v${next} &amp; review…</button></div>

  ${pending?.exists ? banner('warn', html`<b>A pending draft already targets v${pending.target}</b> (${join([pending.topologyChanges ? `${int(pending.topologyChanges)} topology change${pending.topologyChanges === 1 ? '' : 's'}` : '', pending.telemetryChanges ? `${int(pending.telemetryChanges)} telemetry change${pending.telemetryChanges === 1 ? '' : 's'}` : ''].filter(Boolean), ', ')}). Creating a generated draft discards it, and the review step will list it as discarded. Apply or discard it first to keep it.`,
    html`<button type="button" class="lat-btn lat-btn--sm act" data-act="gen.openDraft">Open pending draft</button>`) : ''}

  <section class="lat-card" aria-labelledby="gen-presets"><h2 class="lat-card-title" id="gen-presets">Start from a preset <span class="aside">creates a draft straight away and opens the review · replaces the pending draft</span></h2>
    <div class="preset-row">${topoPresets.map((x) => html`<button type="button" class="preset" data-act="gen.preset" data-arg="${x.name}"><span class="t">${x.title}</span><span class="s">${x.description}</span><span class="e">${x.effect}</span></button>`)}</div>
  </section>

  <div class="g" style="grid-template-columns: 1fr 1.15fr;">
    <section class="lat-card" aria-labelledby="gen-shape">
      <h2 class="lat-card-title" id="gen-shape">Shape <span class="aside">uniform start; override clusters afterwards in the editor</span></h2>
      <div class="stack">
        ${form.dcs.map((d, i) => html`<div class="row dc-row"><div class="grow"><label class="lat-label" for="gen-dc-${i}">Datacenter ${i + 1}</label><input class="lat-input mono" id="gen-dc-${i}" data-k="gen-dc-${i}" autocomplete="off" spellcheck="false" data-inp="gen.dc" data-i="${i}" data-f="name" value="${d.name}"></div>
          <div style="width:110px"><label class="lat-label" for="gen-dcn-${i}">Clusters</label><input class="lat-input num" id="gen-dcn-${i}" data-k="gen-dcn-${i}" inputmode="numeric" autocomplete="off" data-inp="gen.dc" data-i="${i}" data-f="clusters" value="${d.clusters}"></div>
          <button type="button" class="lat-btn lat-btn--sm" data-act="gen.dcRemove" data-arg="${i}" ${form.dcs.length < 2 ? 'disabled' : ''} aria-label="Remove datacenter ${i + 1}">Remove</button></div>`)}
        <div class="row"><button type="button" class="lat-btn lat-btn--sm" data-act="gen.dcAdd">Add datacenter</button><span class="anno">${int(clusters)} cluster${clusters === 1 ? '' : 's'} · named cluster-01, cluster-02 … in each datacenter</span></div>
      </div>
      <div class="fields c3">${field('Hosts per cluster', 'hosts', 'at least 3: vSAN needs three hosts')}</div>
      <div class="fields c3"><div><label class="lat-label" for="gen-architecture">vSAN architecture</label><select class="lat-input" id="gen-architecture" data-chg="gen.architecture" data-k="gen-architecture"><option value="esa" ${form.architecture === 'esa' ? 'selected' : ''}>vSAN ESA</option><option value="osa" ${form.architecture === 'osa' ? 'selected' : ''}>vSAN OSA</option></select><div class="lat-hint">Applies to generated clusters; override individual clusters afterwards.</div></div>
        <div><span class="lat-label">Stats primary online</span><label class="chk"><input type="checkbox" data-chg="gen.flag" data-f="statsPrimary" data-k="gen-statsPrimary" ${form.statsPrimary ? 'checked' : ''}> ${form.statsPrimary ? 'on' : 'off'}</label><div class="lat-hint">When off, performance queries return a missing-primary fault.</div></div></div>
      <h2 class="lat-card-title">Per host</h2>
      <div class="fields c3">
        ${field('NVMe devices', 'claimedDevices', form.architecture === 'osa' ? 'capacity devices in each OSA disk group' : 'claimed by vSAN ESA')}
        ${field('Logical CPUs', 'threads', 'host-cpu entities')}
        ${field('DOM worlds', 'domWorlds', 'dom-world-cpu entities')}
        ${field('vSAN pNICs', 'vsanPnics', 'uplinks on the vDS')}
        ${field('VMs', 'vmsPerHost', 'per host')}
        ${field('Virtual disks per VM', 'disksPerVM')}
        ${field('Local NVMe (VMFS)', 'localDevices', 'a local datastore each')}
      </div>
      ${form.architecture === 'osa' ? html`<div class="anno">OSA deduplication and compression are not modeled from the capture.</div>` : html`<div class="row wrap" style="gap:16px"><label class="chk"><input type="checkbox" data-chg="gen.flag" data-f="dedup" data-k="gen-dedup" ${form.dedup ? 'checked' : ''}> ESA dedup</label>
        <label class="chk"><input type="checkbox" data-chg="gen.flag" data-f="compression" data-k="gen-compression" ${form.compression ? 'checked' : ''}> Compression</label>
        <span class="anno">dedup off: the *-esa-dedup-* kinds answer empty</span></div>`}
      <h2 class="lat-card-title">Identity</h2>
      <div class="fields c2"><div><label class="lat-label" for="gen-seed">Global seed</label><input class="lat-input mono" id="gen-seed" data-k="gen-seed" autocomplete="off" spellcheck="false" data-inp="gen.seed" value="${form.seed}">
        <div class="lat-hint">Changing the seed replaces identities.</div></div></div>
      ${sameSeed
        ? banner('info', html`<b>Same seed: existing objects keep their IDs when their shape changes.</b> A removed object gets its previous IDs back if it returns with the same name and seed, and its registry entry has not been retired. Regenerate retires a cluster’s IDs. The review separates additions, restorations and removals.`)
        : banner('info', html`<b>Applying a generated draft with a new seed replaces every identity.</b> VCF Operations will see every cluster as new and stop receiving data for the v${st?.topology.version} objects. To grow an existing topology without churn, edit clusters instead.`)}
    </section>

    <section class="lat-card" aria-labelledby="gen-proj">
      <h2 class="lat-card-title" id="gen-proj">Projected cardinality <span class="aside">${p ? html`${int(p.clusters)} cluster${p.clusters === 1 ? '' : 's'} · ${int(p.hosts)} hosts · computed, not measured` : ''}${pvBusy ? ' · calculating…' : ''}</span></h2>
      ${pvErr ? banner('bad', html`<b>This shape cannot be generated.</b> ${pvErr}`) : ''}
      ${p && !pvErr ? html`
        <div class="tiles g4">
          ${tile('Perf entities', int(sel.entities), pv.scope === 'dashboard' ? 'dashboard kinds' : 'all modeled')}
          ${tile('Series / cycle', int(sel.series), pv.scope === 'dashboard' ? 'dashboard kinds' : 'all modeled')}
          ${tile('PerfQuery calls', int(sel.calls), `${sel.kinds} kinds × ${p.clusters}`)}
          ${tile('Bytes / cycle', html`~${bytes(sel.bytes).split(' ')[0]} <small>${bytes(sel.bytes).split(' ')[1]}</small>`, 'estimate, 3 samples per series')}
        </div>
        <table class="lat-table"><thead><tr><th>Entity family</th><th>Per</th><th class="lat-num">Objects</th><th class="lat-num">Series</th><th>Scope</th></tr></thead>
          <tbody>${fam.top.map((f) => html`<tr><td class="ink mono">${f.family}</td><td class="muted">${fam.per(f)}</td><td class="lat-num num">${int(f.objects)}</td><td class="lat-num num">${int(f.series)}</td><td>${f.dashboard ? badge('dashboard') : html`<span class="subtle">all only</span>`}</td></tr>`)}
            ${fam.rest.length ? html`<tr><td class="subtle" colspan="2">… ${fam.rest.length} more kinds</td><td class="lat-num num">${int(fam.rest.reduce((s, f) => s + f.objects, 0))}</td><td class="lat-num num">${int(fam.rest.reduce((s, f) => s + f.series, 0))}</td><td></td></tr>` : ''}</tbody>
          <tfoot><tr><td>${sel.kinds} kinds queried</td><td></td><td class="lat-num num">${int(sel.entities)}</td><td class="lat-num num">${int(sel.series)}</td><td></td></tr></tfoot></table>
        <div class="anno">${int(emptyCalls)} of ${int(p.all.calls)} queries / cycle across all modeled kinds return no samples or a fault (kinds a cluster does not model, or a stats-primary fault) but are still issued.</div>
        ${(pv.warnings || []).map((w) => banner(w.severity === 'warn' ? 'warn' : 'info', html`<b>${w.title}.</b> ${w.text}`))}
        <div class="row wrap anno">${pv.scope === 'dashboard' ? 'All modeled kinds' : 'Dashboard-only scope'} would project <b class="num">${int(pv.other.entities)}</b> entities · <b class="num">${int(pv.other.calls)}</b> calls · <a href="#/telemetry">Compare scopes in Telemetry →</a></div>`
        : (pvErr ? '' : html`<div class="empty">Calculating…</div>`)}
    </section>
  </div>`;
}

const generator = {
  async load(ctx) {
    const [scn, presets] = await Promise.all([api('/draft'), api('/presets')]);
    draftInfo = scn;
    const v = ctx.state?.topology.version ?? 0;
    if (!form || formVersion !== v) {
      seedForm(scn.applied, ctx.state?.projection);
      formVersion = v;
      pv = null; pvErr = '';
    }
    runPreview(ctx);
    return { presets };
  },
  render: genRender,
  actions: {
    'gen.architecture'(ctx, el) { form.architecture = el.value; ctx.rerender(); schedulePreview(ctx, 0); },
    'gen.set'(ctx, el) { form[el.dataset.f] = el.value; schedulePreview(ctx); },
    'gen.seed'(ctx, el) { form.seed = el.value; schedulePreview(ctx); },
    'gen.flag'(ctx, el) { form[el.dataset.f] = el.checked; schedulePreview(ctx, 0); },
    'gen.dc'(ctx, el) { form.dcs[+el.dataset.i][el.dataset.f] = el.value; schedulePreview(ctx); },
    'gen.dcAdd'(ctx) {
      const used = new Set(form.dcs.map((d) => d.name));
      let n = form.dcs.length + 1;
      while (used.has('dc-' + String(n).padStart(2, '0'))) n++;
      form.dcs.push({ name: 'dc-' + String(n).padStart(2, '0'), clusters: '1' });
      ctx.rerender(); schedulePreview(ctx, 0);
    },
    'gen.dcRemove'(ctx, el) { form.dcs.splice(+el.dataset.arg, 1); ctx.rerender(); schedulePreview(ctx, 0); },
    'gen.cancel'(ctx) { ctx.go('#/topology'); },
    'gen.openDraft'(ctx) { ctx.go('#/topology'); },
    async 'gen.create'(ctx) {
      if (genBusy) return;
      genBusy = true; ctx.rerender();
      const r = await ctx.run(() => api('/draft/generate', { body: params() }));
      genBusy = false;
      if (r) { await ctx.refreshState(); ctx.go('#/topology/review'); } else ctx.rerender();
    },
    async 'gen.preset'(ctx, el) {
      const name = el.dataset.arg;
      const r = await ctx.run(() => api('/draft/preset', { body: { name } }));
      if (r) { await ctx.refreshState(); ctx.go('#/topology/review'); }
    },
  },
};

// ---------------------------------------------------------------------------
// review
// ---------------------------------------------------------------------------
let when = 'boundary';
let showAll = false;
const CAP = 10;
let applying = false;

const effectKind = (e) => (/new identit|replaces|removes|changes samples|changes performance identities/.test(e) ? 'warn' : '');

function delta(a, b) {
  const d = b - a;
  if (d === 0) return 'unchanged';
  return `${signed(d)}${a ? ` (${d > 0 ? '+' : '−'}${Math.abs((d / a) * 100).toFixed(1)}%)` : ''}`;
}

function reviewRender(ctx, data) {
  const st = ctx.state;
  if (data.empty) {
    return html`<section class="lat-card"><h2 class="lat-card-title">Review before apply</h2>
      <div class="empty"><b>Nothing to review.</b> There is no pending draft. Edit a cluster, generate a topology or start from a preset first.</div>
      <div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="rev.back">Back to Topology</button></div></section>`;
  }
  const rv = data.rv;
  const topoOnly = rv.rebuildsVcenter;
  const boundary = hms(rv.boundary);
  const ids = rv.identities || [];
  const hasChurnCard = (rv.warnings || []).some((w) => w.title === 'No identity churn');
  const newDraft = rv.origin && rv.origin !== 'edit';
  const sumRow = (lab) => ids.find((r) => r.label.startsWith(lab));
  const cl = sumRow('Cluster');
  const nfsChanges = rv.changes.filter((c) => c.field === 'NFS datastore');
  const otherChanges = rv.changes.filter((c) => c.field !== 'NFS datastore');
  const shownChanges = showAll ? rv.changes : [...nfsChanges, ...otherChanges.slice(0, CAP)];
  const hiddenChanges = rv.changes.length - shownChanges.length;
  const originText = rv.origin === 'generator' ? 'generated draft' : String(rv.origin).startsWith('preset:') ? 'preset ' + rv.origin.slice(7) : '';
  return html`
  <section class="lat-card" aria-labelledby="rev-title">
    <div class="row"><span class="page-title" id="rev-title">Review before apply</span>${badge(rv.to === rv.from ? `telemetry only · topology v${rv.from} stays` : `topology v${rv.from} → v${rv.to}`, 'warn')}${originText ? badge(originText) : ''}<span class="grow"></span><span class="anno">Step 2 of 2 · nothing has changed yet</span></div>
    ${rv.discarded ? banner('warn', html`<b>Replaces a pending draft.</b> ${rv.discarded}`) : ''}
    ${newDraft && cl && (cl.removed > 0 || cl.added > 0 || cl.restored > 0) ? banner('info', html`<b>Cluster changes.</b> ${int(cl.removed)} removed, ${int(cl.added)} added and ${int(cl.restored)} restored. Restored objects regain their previous IDs; removed objects are no longer present.`) : ''}
    <div class="g" style="grid-template-columns: 1.25fr 1fr;">
      <div class="stack" style="gap:10px">
        <h2 class="lat-card-title">Changes <span class="aside">${int(rv.topologyChanges)} topology · ${int(rv.telemetryChanges)} telemetry</span></h2>
        <div class="lat-table-scroll"><table class="lat-table review-changes"><thead><tr><th>Object</th><th>Field</th><th>Before</th><th>After</th><th>Effect</th></tr></thead>
          <tbody>${shownChanges.map((c) => html`<tr><td class="wrap ink">${c.object}</td><td class="wrap">${c.field}</td><td class="wrap">${c.before}</td><td class="wrap">${c.after}</td><td class="wrap">${badge(c.effect, effectKind(c.effect))}</td></tr>`)}
            ${!showAll && hiddenChanges > 0 ? html`<tr><td colspan="5"><button type="button" class="link" data-act="rev.showAll">Show all ${int(rv.changes.length + rv.moreChanges)} changes</button> <span class="subtle">· ${int(hiddenChanges + rv.moreChanges)} more</span></td></tr>` : ''}
            ${(showAll || hiddenChanges === 0) && rv.moreChanges ? html`<tr><td class="subtle" colspan="5">… ${int(rv.moreChanges)} more changes not listed</td></tr>` : ''}
            ${showAll && rv.changes.length > CAP ? html`<tr><td colspan="5"><button type="button" class="link" data-act="rev.showAll">Show fewer</button></td></tr>` : ''}</tbody></table></div>
        ${topoOnly && ids.length ? html`
          <h2 class="lat-card-title">Identity consequences</h2>
          <table class="lat-table"><thead><tr><th>Identity</th><th class="r">Kept</th><th class="r">Added</th><th class="r">Restored</th><th class="r">Removed</th></tr></thead>
            <tbody>${ids.map((r, i) => html`<tr class="${i === ids.length - 1 ? '' : ''}">${i === ids.length - 1 ? html`<td class="ink"><b>${r.label === 'Performance entities' ? 'All perf entities' : r.label}</b></td>` : html`<td>${r.label}</td>`}<td class="num r">${int(r.kept)}</td><td class="num r">${int(r.added)}</td><td class="num r">${int(r.restored)}</td><td class="num r">${int(r.removed)}</td></tr>`)}</tbody></table><p class="anno">Restored counts cover registry-tracked clusters, hosts, VMs and NVMe disks. Added virtual disk and performance identities may include previously seen identities.</p>` : ''}
        ${(rv.warnings || []).filter((w) => w.title !== 'Replaces a pending draft').map((w) => banner(w.severity === 'ok' ? 'ok' : w.severity === 'warn' ? 'warn' : 'info', html`<b>${w.title}.</b> ${w.text}`))}
        ${topoOnly && !rv.identityChurn && !hasChurnCard && ids.length ? banner('ok', html`<b>No identity churn.</b> Nothing is removed or renamed.`) : ''}
        ${!topoOnly && !(rv.warnings || []).some((w) => w.title === 'Telemetry only') ? banner('ok', html`<b>Telemetry only.</b> The running vCenter is not rebuilt. Sessions, history and identities are untouched.`) : ''}
        <h2 class="lat-card-title">Not affected</h2>
        <div class="pill-row">${(rv.notAffected || []).map((x) => html`<span class="lat-pill">${x}</span>`)}</div>
      </div>

      <div class="stack" style="gap:10px">
        <h2 class="lat-card-title" id="rev-when">When it takes effect</h2>
        <div class="opts" role="radiogroup" aria-labelledby="rev-when">
          <label class="opt${when === 'boundary' ? ' is-on' : ''}"><input type="radio" class="sr-only" name="rev-when" value="boundary" data-chg="rev.when" data-k="rev-when-boundary" ${when === 'boundary' ? 'checked' : ''}><span class="dot"></span><span class="t">At next bucket boundary</span><span class="num">${boundary}</span>
            <span class="s">${topoOnly ? html`Responses in flight finish on v${rv.from}; the ${hm(rv.boundary)} bucket is the first served from v${rv.to}.` : html`The change is read from the ${hm(rv.boundary)} bucket on.`}</span></label>
          <label class="opt${when === 'now' ? ' is-on' : ''}"><input type="radio" class="sr-only" name="rev-when" value="now" data-chg="rev.when" data-k="rev-when-now" ${when === 'now' ? 'checked' : ''}><span class="dot"></span><span class="t">Immediately</span><span></span>
            <span class="s">A collection cycle in progress may see both ${topoOnly ? 'topologies' : 'settings'}.</span></label>
        </div>
        <h2 class="lat-card-title">Projected load after apply <span class="aside">${rv.scope === 'dashboard' ? 'dashboard kinds' : 'all modeled kinds'} · projected</span></h2>
        <div class="tiles" style="grid-template-columns:repeat(3,1fr)">
          ${tile('Entities', int(rv.after.entities), delta(rv.before.entities, rv.after.entities))}
          ${tile('Series', int(rv.after.series), delta(rv.before.series, rv.after.series))}
          ${tile('Calls', int(rv.after.calls), delta(rv.before.calls, rv.after.calls))}
        </div>
        <div class="anno">${topoOnly ? html`Hosts ${int(rv.hostsBefore)} → ${int(rv.hostsAfter)} · ` : ''}calls scale with clusters × kinds, not hosts; the extra load lands in response size.</div>
        <div class="row" style="margin-top:6px"><button type="button" class="lat-btn" data-act="rev.discard">Discard draft…</button><span class="grow"></span>
          <button type="button" class="lat-btn" data-act="rev.back">← Back to edit</button>
          <button type="button" class="lat-btn lat-btn--primary" data-act="rev.apply" ${applying ? 'disabled' : ''}>${when === 'now' ? 'Apply now' : html`Apply at ${boundary}`}</button></div>
      </div>
    </div>
  </section>`;
}

const review = {
  pollMs: 10000,
  async load(ctx) {
    try {
      return { rv: await api('/review') };
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) return { empty: true };
      throw e;
    }
  },
  render: reviewRender,
  actions: {
    'rev.when'(ctx, el) { when = el.value; ctx.rerender(); },
    'rev.back'(ctx) { ctx.go('#/topology'); },
    'rev.showAll'(ctx) { showAll = !showAll; ctx.rerender(); },
    async 'rev.discard'(ctx) {
      const ok = await ctx.confirm({ title: 'Discard the pending draft?', body: html`<ul><li>Every unapplied change on every tab is dropped. The API is unaffected: it keeps serving the applied topology.</li></ul>`, confirmLabel: 'Discard draft', danger: true });
      if (!ok) return;
      await ctx.run(() => api('/draft', { method: 'DELETE' }), 'Draft discarded.');
      await ctx.refreshState();
      ctx.go('#/topology');
    },
    async 'rev.apply'(ctx) {
      if (applying) return;
      applying = true; ctx.rerender();
      const r = await ctx.run(() => api('/apply', { body: { when } }));
      applying = false;
      if (!r) { ctx.rerender(); return; }
      ctx.toast(r.message || 'Applied.', 'ok');
      when = 'boundary'; showAll = false;
      await ctx.refreshState();
      ctx.go('#/topology');
    },
  },
};

export default { generator, review };
