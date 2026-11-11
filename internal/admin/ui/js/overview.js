import { api, html, raw, join, badge, tile, int, bytes, ms, hms, hm, dateTime, plural } from './lib.js';

// Shared by the Overview and the API Activity screens.
export function requestsChart(minutes, w = 560, h = 120) {
  const n = minutes.length || 60;
  const bw = w / n;
  const max = Math.max(10, ...minutes.map((m) => m.requests));
  const bars = minutes.map((m, i) => {
    const bh = (h * m.requests) / max;
    const fh = m.injected + m.unexpected ? Math.max(2, (h * (m.injected + m.unexpected)) / max) : 0;
    const x = i * bw + 1;
    return `<rect x="${x.toFixed(1)}" y="${(h - bh).toFixed(1)}" width="${(bw - 2).toFixed(1)}" height="${bh.toFixed(1)}" style="fill:var(--ink-muted)"><title>${hm(m.t)}Z · ${m.requests} requests · ${m.injected} injected · ${m.unexpected} unexpected</title></rect>` +
      (fh ? `<rect x="${x.toFixed(1)}" y="${(h - fh).toFixed(1)}" width="${(bw - 2).toFixed(1)}" height="${fh.toFixed(1)}" style="fill:var(--bad)"></rect>` : '');
  }).join('');
  const grid = [0.25, 0.5, 0.75].map((g) => `<line x1="0" x2="${w}" y1="${(h - h * g).toFixed(1)}" y2="${(h - h * g).toFixed(1)}" style="stroke:var(--line-soft)"/>`).join('');
  const idx = [0, 15, 30, 45, n - 1].filter((i) => minutes[i]);
  const labels = idx.map((i, k) => `<text x="${k === idx.length - 1 ? w - 30 : i * bw}" y="${h + 12}" style="fill:var(--ink-muted);font:10.5px var(--font-mono)">${hm(minutes[i].t)}</text>`).join('');
  return raw(`<svg class="spark" viewBox="0 0 ${w} ${h + 14}" height="${h + 14}" role="img" aria-label="Requests per minute, last 60 minutes, peak ${max}">${grid}${bars}${labels}</svg>`);
}

const kind = (s) => ({ bad: 'bad', warn: 'warn', ok: 'ok', info: '' }[s] ?? '');

export default {
  id: 'overview',
  title: 'Overview',
  pollMs: 4000,
  async load(ctx) {
    await ctx.refreshState();
    return {};
  },
  render(ctx) {
    const st = ctx.state;
    if (!st) return html`<div class="empty">Loading…</div>`;
    const p = st.projection;
    const a = st.activity;
    const sel = st.selected;
    const scopeName = st.scope === 'dashboard' ? 'dashboard kinds' : 'all modeled kinds';
    const users = st.auth.users.length ? st.auth.users.join(', ') : 'any user name';
    const cyc = a.lastCycle;
    const emptyEntities = a.faults.empty;
    const faultKind = a.faults.unexpected ? 'bad' : a.faults.injected ? 'warn' : '';
    return html`
    <div class="g" style="grid-template-columns: 330px 1fr 1fr">
      <section class="lat-card" aria-labelledby="ov-service"><h2 class="lat-card-title" id="ov-service">Service</h2>
        <dl class="kv">
          <dt>Hostname</dt><dd>${st.endpoint}</dd>
          <dt>SOAP paths</dt><dd>/sdk · /vsanHealth</dd>
          <dt>TLS</dt><dd class="sans">${st.tls}</dd>
          <dt>Profile</dt><dd class="sans">${st.profile.fullName}</dd>
          <dt>Build string</dt><dd class="sans">${badge('captured')} ${st.profile.build}</dd>
          <dt>Accepted logins</dt><dd class="sans" title="${users}">${users}</dd>
          <dt>Topology</dt><dd>v${st.topology.version} · applied ${dateTime(st.topology.appliedAt)}</dd>
          <dt>Seed</dt><dd>${st.topology.seed} · cfg ${st.topology.config}</dd>
          <dt>Process</dt><dd>RSS ${bytes(st.process.rssBytes)} · ${st.process.cores.toFixed(1)} cores</dd>
        </dl>
        <div class="divider"></div>
        <dl class="kv">
          <dt>Metric scope</dt><dd class="sans">${st.scope === 'dashboard' ? 'Dashboard-only' : 'All modeled'} · ${sel.kinds} kinds</dd>
          <dt>Latency</dt><dd class="sans">${st.scenario.latency}</dd>
          <dt>Faults</dt><dd class="sans">${st.scenario.faultRules ? `${st.scenario.faultRules} rule${st.scenario.faultRules === 1 ? '' : 's'}` : 'none'}</dd>
          <dt>History</dt><dd class="sans">deterministic from the seed · 5-min buckets</dd>
        </dl>
      </section>

      <section class="lat-card" aria-labelledby="ov-inv"><h2 class="lat-card-title" id="ov-inv">Inventory <span class="aside">topology v${st.topology.version} · ${int(st.topology.datacenters)} datacenter${st.topology.datacenters === 1 ? '' : 's'}</span></h2>
        <div class="tiles" style="grid-template-columns:repeat(4,1fr)">
          ${tile('Clusters', int(p.clusters), `${p.osaClusters === 0 ? 'vSAN ESA' : p.osaClusters === p.clusters ? 'vSAN OSA' : `vSAN mixed · ${p.osaClusters} OSA, ${p.clusters - p.osaClusters} ESA`}${st.overriddenClusters ? ' · ' + st.overriddenClusters + ' overridden' : ''}`)}
          ${tile('Hosts', int(p.hosts), p.clusters ? `${(p.hosts / p.clusters).toFixed(1)} per cluster` : '')}
          ${tile('VMs', int(p.vms), p.hosts ? `${(p.vms / p.hosts).toFixed(1)} per host` : '')}
          ${tile('Virtual disks', int(p.vdisks), p.vms ? `${(p.vdisks / p.vms).toFixed(1)} per VM` : '')}
          ${tile('NVMe devices', int(p.nvme), p.hosts ? `${(p.nvme / p.hosts).toFixed(1)} per host claimed` : '')}
          ${tile('Logical CPUs', int(p.logicalCpus), p.hosts ? `${Math.round(p.logicalCpus / p.hosts)} per host` : '')}
          ${tile('DOM worlds', int(p.domWorlds), p.hosts ? `${Math.round(p.domWorlds / p.hosts)} per host` : '')}
          ${tile('Datastores', int(p.datastores), `vSAN ${p.clusters} · local VMFS ${p.localDevices} · NFS ${p.nfsDatastores || 0}`)}
        </div>
        <h2 class="lat-card-title" style="margin-top:4px">Per cycle (5 min) <span class="aside">projected from topology, not measured · ${scopeName}</span></h2>
        <div class="tiles" style="grid-template-columns:repeat(4,1fr)">
          ${tile('Perf entities', int(sel.entities), `${sel.kinds} kinds`)}
          ${tile('Metric series', int(sel.series), 'entities × metrics')}
          ${tile('PerfQuery calls', int(sel.calls), `${sel.kinds} kinds × ${plural(p.clusters, 'cluster')}`)}
          ${tile('Response bytes', html`~${bytes(sel.bytes).split(' ')[0]} <small>${bytes(sel.bytes).split(' ')[1]}</small>`, 'estimate, 3 samples per series')}
        </div>
      </section>

      <section class="lat-card" aria-labelledby="ov-fam"><h2 class="lat-card-title" id="ov-fam">Largest entity families <span class="aside">all modeled kinds · series = objects × metrics</span></h2>
        <table class="lat-table"><thead><tr><th>Family</th><th>Scope</th><th class="r">Objects</th><th class="r">Metrics</th><th class="r">Series</th></tr></thead>
          <tbody>${st.Top.map((f) => html`<tr><td class="ink mono">${f.family}</td><td>${f.dashboard ? badge('dashboard') : html`<span class="subtle">all only</span>`}</td><td class="num r">${int(f.objects)}</td><td class="num r">${int(f.metrics)}</td><td class="num r">${int(f.series)}</td></tr>`)}</tbody>
          <tfoot><tr><td colspan="2">${p.all.kinds} kinds (${st.Top.length} shown)</td><td class="num r">${int(p.all.entities)}</td><td></td><td class="num r">${int(p.all.series)}</td></tr></tfoot></table>
        <div class="anno">${(() => { const big = (p.families || st.Top).filter((f) => !f.dashboard && f.objects > 0).sort((a, b) => b.objects - a.objects).slice(0, 2); const tot = p.all.entities || 1; const share = big.reduce((s, f) => s + f.objects, 0) / tot; return big.length ? html`${join(big.map((f) => f.family), ' and ')} hold ${Math.round(share * 100)}% of objects and are not referenced by any shipped MP view. See Telemetry → Metric scope.` : 'Every large family is referenced by a shipped MP view.'; })()}</div>
      </section>
    </div>

    <div class="g" style="grid-template-columns: 1.1fr 1fr">
      <section class="lat-card" aria-labelledby="ov-act"><h2 class="lat-card-title" id="ov-act">API activity · last 60 min <span class="aside">collector bursts every 5 min · <a href="#/activity">open API Activity →</a></span></h2>
        <div class="g" style="grid-template-columns: 1.5fr 1fr">
          <div>${a.totals.requests ? requestsChart(a.minutes) : html`<div class="empty">No requests yet. Point the collector at this endpoint and a bar appears for each minute it talks.</div>`}
            <div class="legend"><span><i></i>requests / min</span><span><i class="bad"></i>faults</span></div></div>
          <div class="tiles" style="grid-template-columns:1fr 1fr;align-content:start">
            ${tile('Rate now', html`${a.ratePerSec.toFixed(1)} <small>req/s</small>`, 'mean of the last 5 min')}
            ${tile('P95 latency', html`${ms(a.p95Ms).split(' ')[0]} <small>${ms(a.p95Ms).split(' ')[1]}</small>`, 'last 60 min')}
            ${tile('Faults', html`${a.faults.percent.toFixed(1)} <small>%</small>`, `${int(a.faults.injected)} injected · ${int(a.faults.unexpected)} unexpected · ${int(a.faults.denied || 0)} denied`, faultKind === 'bad' ? '' : '')}
            ${tile('Empty results', int(emptyEntities), p.osaClusters === 0 ? 'expected: provisional kinds on ESA' : 'expected: unmodeled kinds on a cluster')}
          </div>
        </div>
      </section>

      <section class="lat-card" aria-labelledby="ov-health"><h2 class="lat-card-title" id="ov-health">Collector health <span class="aside">inferred from requests${cyc.calls ? ' · last cycle ' + hms(cyc.start) : ''}</span></h2>
        ${cyc.calls ? html`<table class="lat-table"><thead><tr><th>Step</th><th class="r">Calls</th><th class="r">P95</th><th>Result</th></tr></thead><tbody>
          ${cyc.steps.map((s) => html`<tr><td class="ink">${s.name}</td><td class="num r">${int(s.calls)}</td><td class="num r">${ms(s.p95Ms)}</td><td>${s.unexpected ? badge(`${s.unexpected} faults`, 'bad') : s.denied ? badge(`${s.denied} denied`, 'bad') : s.name === 'VsanPerfQueryPerf' && s.calls > 0 && s.series === 0 && !s.injected ? badge('0 series', 'bad') : s.injected ? html`${badge(`${s.injected} faults`, 'warn')} <span class="subtle">injected</span>` : badge('ok', 'ok')}</td></tr>`)}</tbody></table>
          <div class="row wrap anno"><span>Cycle ${Math.max(1, Math.round((new Date(cyc.end) - new Date(cyc.start)) / 1000))} s · client ${cyc.client}</span></div>`
          : html`<div class="empty">No collector traffic yet.</div>`}
        <h2 class="lat-card-title" style="margin-top:4px">Recent events</h2>
        <table class="lat-table events-table"><colgroup><col style="width:90px"><col><col></colgroup><tbody>${st.events.length ? st.events.map((e) => html`<tr><td class="mono subtle">${hms(e.at)}</td><td class="wrap ink">${e.text}</td><td class="wrap subtle">${e.detail}</td></tr>`) : html`<tr><td class="subtle">Nothing yet.</td></tr>`}</tbody></table>
        ${st.lastError ? html`<div class="lat-banner lat-banner--bad">${st.lastError}</div>` : ''}
      </section>
    </div>`;
  },
  actions: {},
};
