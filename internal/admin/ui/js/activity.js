// API Activity: the redacted request log, aggregates, the read-only request
// inspector, and the "why did this collection return nothing" diagnosis.
// Data: GET /api/activity (calls, newest first), GET /api/activity/<seq>
// (one call with its /vsanHealth request body), ctx.state.activity (summary).
import { api, html, raw, join, badge, tile, banner, int, bytes, ms, hms, hmsms, hm, plural } from './lib.js';

const LIMIT = 5000;
const RESULTS = ['all', 'unanswered', 'ok', 'injected', 'unexpected', 'denied', 'empty'];

// Module state: it survives navigation, as filters should.
const S = {
  range: '1h', surface: '', method: '', cluster: '', family: '', result: 'all', q: '',
  sel: 0, frozen: false, cache: null, detail: {},
};

// ---- helpers ------------------------------------------------------------------
const nowMs = (ctx) => (ctx.state ? Date.parse(ctx.state.clock.now) : Date.now());
const pathOf = (c) => (c.surface === 'sdk' ? '/sdk' : '/vsanHealth');
const target = (c) => (c.family ? `${c.family}:*` : '');
let shortDC = '';
const short = (k) => (k ? (shortDC && k.startsWith(shortDC) ? k.slice(shortDC.length) : k) : '—');
const trace = (c) => c.seq.toString(16).padStart(5, '0');
const series = (c) => (c.method === 'VsanPerfQueryPerf' ? Math.max(0, (c.entities || 0) - (c.empty || 0)) : 0);
const isEmpty = (c) => c.result === 'ok' && c.method === 'VsanPerfQueryPerf' && c.entities > 0 && c.empty === c.entities;
const resultOf = (c) => (c.result === 'fault' ? 'unexpected' : isEmpty(c) ? 'empty' : c.result);

function resultBadge(c) {
  switch (resultOf(c)) {
    case 'ok': return badge('ok', 'ok');
    case 'injected': return badge('injected', 'warn');
    case 'unexpected': return badge('fault', 'bad');
    case 'refused': return badge('refused', 'warn');
    case 'denied': return badge('denied', 'warn');
    case 'empty': return badge('empty');
    default: return badge(c.result);
  }
}

function pctile(sorted, p) {
  if (!sorted.length) return 0;
  return sorted[Math.min(sorted.length - 1, Math.floor((sorted.length - 1) * p))];
}

function inRange(ctx, calls) {
  const cyc = ctx.state && ctx.state.activity.lastCycle;
  const now = nowMs(ctx);
  if (S.range === '5m') return calls.filter((c) => now - Date.parse(c.time) <= 5 * 60000);
  if (S.range === 'cycle' && cyc && cyc.calls) {
    const a = Date.parse(cyc.start) - 1, b = Date.parse(cyc.end) + 1;
    return calls.filter((c) => { const t = Date.parse(c.time); return t >= a && t <= b; });
  }
  return calls.filter((c) => now - Date.parse(c.time) <= 3600000);
}

function filtered(ctx, data) {
  const q = S.q.trim().toLowerCase();
  return inRange(ctx, data.calls).filter((c) => {
    if (S.surface && c.surface !== S.surface) return false;
    if (S.method && c.method !== S.method) return false;
    if (S.cluster && c.cluster !== S.cluster) return false;
    if (S.family && c.family !== S.family) return false;
    if (S.result !== 'all' && resultOf(c) !== S.result) return false;
    if (q && !(`${target(c)} ${trace(c)} ${c.seq} ${c.method}`.toLowerCase().includes(q))) return false;
    return true;
  });
}

function xmlPretty(s) {
  if (!s) return '';
  let depth = 0;
  return s.replace(/>\s*</g, '>\n<').split('\n').map((line) => {
    if (/^<\//.test(line)) depth = Math.max(0, depth - 1);
    const out = '  '.repeat(depth) + line;
    if (/^<[^/?!][^>]*[^/]>$/.test(line) && !/<\/[^>]+>$/.test(line)) depth++;
    return out;
  }).join('\n');
}

const READING = {
  SystemError: ['SystemError', 'reported as a collection problem for that kind on that cluster; the other kinds in the cycle still collect.'],
  InvalidArgument: ['InvalidArgument', 'treated by the pack as “kind not available” and skipped silently.'],
  NotSupported: ['NotSupported', 'treated by the pack as “kind not available” and skipped silently.'],
  timeout: ['Timeout', 'the answer is held past the pack’s 120 s request timeout; the pack reports a failed cluster.'],
};

async function ensureDetail(ctx, seq) {
  if (!seq || S.detail[seq]) return;
  S.detail[seq] = { loading: true };
  try { S.detail[seq] = await api('/activity/' + seq); }
  catch (e) { S.detail[seq] = { gone: true, message: e.message }; }
  ctx.rerender();
}

function csv(rows) {
  const cols = ['time', 'trace', 'surface', 'method', 'cluster', 'family', 'result', 'fault', 'durationMs', 'bytes', 'status', 'entities', 'empty', 'client'];
  const cell = (v) => { v = v === undefined || v === null ? '' : String(v); return /[",\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v; };
  return [cols.join(','), ...rows.map((c) => cols.map((k) => cell(k === 'trace' ? trace(c) : c[k])).join(','))].join('\n');
}

// ---- diagnosis ----------------------------------------------------------------
function diagnosis(ctx) {
  const a = ctx.state && ctx.state.activity;
  if (!a) return null;
  const cyc = a.lastCycle;
  if (!cyc || !cyc.calls) return null;
  const perf = cyc.steps.find((s) => s.name === 'VsanPerfQueryPerf');
  const bad = cyc.findings.find((f) => f.severity === 'bad');
  const zero = !!(perf && perf.calls > 0 && cyc.entitiesWithData === 0);
  if (!zero && !bad) return null;
  const cause = bad || cyc.findings.find((f) => f.severity === 'warn') || cyc.findings.find((f) => f.severity === 'info');
  return { cyc, perf, zero, cause: bad || (zero ? cause : null) };
}

function stepEvidence(s, cyc) {
  if (s.name === 'VsanPerfQueryPerf') {
    const parts = [`${int(cyc.entitiesWithData)} entities with samples`, `${int(cyc.emptyEntities)} echoed empty`];
    if (s.injected) parts.push(`${int(s.injected)} injected faults`);
    if (s.unexpected) parts.push(`${int(s.unexpected)} unexpected faults`);
    if (s.denied) parts.push(`${int(s.denied)} refused: not authenticated`);
    return parts.join(' · ');
  }
  if (s.unexpected) return `${int(s.unexpected)} unexpected faults`;
  if (s.denied) return `${int(s.denied)} refused: not authenticated`;
  if (s.injected) return `${int(s.injected)} injected faults`;
  return 'no faults';
}

function stepBadge(s, cyc) {
  if (s.unexpected) return badge(`${s.unexpected} faults`, 'bad');
  if (s.denied) return badge(`${s.denied} denied`, 'bad');
  if (s.name === 'VsanPerfQueryPerf' && cyc.entitiesWithData === 0 && s.calls) return badge('0 series', 'bad');
  if (s.injected) return badge(`${s.injected} faults`, 'warn');
  return badge('ok', 'ok');
}

const sevKind = (s) => ({ bad: 'bad', warn: 'warn', ok: 'ok' }[s] || '');

// ---- screen -------------------------------------------------------------------
export default {
  id: 'activity',
  title: 'API Activity',
  pollMs: 3000,

  async load(ctx) {
    if (S.frozen && S.cache) return S.cache;
    const [res, unanswered] = await Promise.all([api(`/activity?limit=${LIMIT}`), api('/capture/unanswered'), ctx.refreshState()]);
    S.cache = { calls: res.calls, totals: res.totals, unanswered };
    const list = filtered(ctx, S.cache);
    if (!S.sel && list.length) S.sel = (list.find((c) => c.result === 'injected' || c.result === 'fault') || list[0]).seq;
    if (S.sel) ensureDetail(ctx, S.sel);
    return S.cache;
  },

  render(ctx, data) {
    const st = ctx.state;
    if (!st) return html`<div class="empty">Loading…</div>`;
    const a = st.activity;
    const dcs = new Set(a.clusters.map((k) => k.split('/')[0]));
    shortDC = dcs.size === 1 ? [...dcs][0] + '/' : '';
    const cyc = a.lastCycle;
    const diag = diagnosis(ctx);
    const all = data.calls;
    const rows = filtered(ctx, data);
    const truncated = data.totals.held > all.length;

    if (S.result === 'unanswered') {
      const records = [...(data.unanswered || [])].reverse();
      return html`<section class="lat-card"><h2 class="lat-card-title">Unanswered requests <span class="aside">${int(records.length)} held · newest first</span></h2>
        <p>These requests could not be answered. Records can include inventory identifiers.</p>
        <div class="row"><button type="button" class="lat-btn" data-act="act.result" data-arg="all">All requests</button>
          <a class="lat-btn" href="/api/capture/unanswered.jsonl" download>Download unanswered requests</a></div>
        ${records.length ? records.map((r) => html`<details class="well"><summary>${hmsms(r.time)} · ${r.method} ${r.path} · ${r.operation || 'request'} · HTTP ${r.status}${r.fault ? ` · ${r.fault}` : ''}</summary>
          <p>Object: ${r.objectType || '—'} · Properties: ${(r.properties || []).join(', ') || '—'}</p>
          ${r.request ? html`<pre class="code-well">${r.request}</pre>${r.requestTruncated ? html`<p class="anno">This in-memory record is shortened. The full log, if enabled, has the complete redacted request.</p>` : ''}` : html`<p>No request body was kept.</p>`}</details>`)
          : html`<div class="empty">No unanswered requests have been captured.</div>`}</section>`;
    }

    if (!data.totals.requests) {
      return html`<section class="lat-card" style="max-width:720px"><h2 class="lat-card-title">No requests yet</h2>
        <p style="margin:0">Nothing has called this simulator since it started at ${hms(data.totals.since)}. Point the collector at the API endpoint
        <span class="mono ink">${st.endpoint}:${(st.API && st.API[0] || '').split(':').pop()}</span> (paths <span class="mono">/sdk</span> and <span class="mono">/vsanHealth</span>),
        log in with ${st.auth.users.length ? html`one of <span class="mono ink">${st.auth.users.join(', ')}</span>` : 'any user name and password'}, and run a collection.
        Each request appears here within a few seconds.</p>
        <p class="anno" style="margin:0">Cookies and credentials are never kept.</p><button type="button" class="lat-btn" data-act="act.result" data-arg="unanswered">Unanswered requests</button></section>`;
    }

    const counts = { injected: 0, unexpected: 0, denied: 0 };
    for (const c of inRange(ctx, all)) { const r = resultOf(c); if (counts[r] !== undefined) counts[r]++; }
    const durs = rows.map((c) => c.durationMs).sort((x, y) => x - y);
    const largest = rows.reduce((m, c) => (c.bytes > (m ? m.bytes : -1) ? c : m), null);
    const seriesSum = rows.reduce((s, c) => s + series(c), 0);
    const bytesSum = rows.reduce((s, c) => s + c.bytes, 0);
    const emptySum = rows.reduce((s, c) => s + (c.method === 'VsanPerfQueryPerf' ? c.empty || 0 : 0), 0);
    const perfCyc = (cyc.steps.find((s) => s.name === 'VsanPerfQueryPerf') || { calls: 0 }).calls;
    const peak = Math.max(0, ...a.minutes.map((m) => m.requests)) / 60;
    const methods = [...new Set(all.map((c) => c.method))].sort();
    const fams = [...new Set(all.map((c) => c.family).filter(Boolean))].sort();
    const sel = S.sel && (S.detail[S.sel] && !S.detail[S.sel].loading && !S.detail[S.sel].gone ? S.detail[S.sel] : all.find((c) => c.seq === S.sel));
    const shown = rows.slice(0, 300);
    const rangeLabel = { '5m': '5 min', cycle: cyc.calls ? `last cycle ${hm(cyc.start)}` : 'last cycle', '1h': '1 h' };
    const seg = (act, cur, opts) => html`<span class="seg" role="group">${opts.map(([v, l]) => html`<button type="button" class="${cur === v ? 'is-on' : ''}" aria-pressed="${cur === v}" data-act="${act}" data-arg="${v}" data-k="${act}-${v}">${l}</button>`)}</span>`;
    const select = (name, label, cur, list, w) => html`<div class="sel-wrap" style="width:${w}px"><select class="lat-select${cur ? ' is-changed' : ''}" name="${name}" aria-label="${label}" data-chg="act.filter" data-f="${name}" data-k="act-f-${name}">
      <option value="">${label}: all</option>${list.map((v) => html`<option value="${v}"${v === cur ? raw(' selected') : ''}>${v}</option>`)}</select></div>`;

    return html`
    ${diag ? banner(diag.zero ? 'bad' : 'warn', diag.zero
      ? html`<b>Collection cycle ${hms(cyc.start)} returned 0 metric series.</b> ${diag.cause && diag.cause.severity === 'bad' ? diag.cause.detail : 'Every VsanPerfQueryPerf call returned an empty result. The MP will report that the Performance Service returned no data.'}`
      : html`<b>${diag.cause.title}.</b> ${diag.cause.detail}`) : ''}
    ${diag ? diagnosisCard(diag, all, ctx) : ''}

    <div class="row wrap" style="gap:6px">
      ${seg('act.range', S.range, [['5m', '5 min'], ['cycle', rangeLabel.cycle], ['1h', '1 h']])}
      ${seg('act.surface', S.surface, [['', 'all paths'], ['sdk', '/sdk'], ['vsanHealth', '/vsanHealth']])}
      ${select('method', 'Op', S.method, methods, 120)}
      ${select('cluster', 'Cluster', S.cluster, a.clusters, 135)}
      ${select('family', 'Family', S.family, fams, 130)}
      <span class="seg" role="group" aria-label="Result">${RESULTS.map((r) => html`<button type="button" class="${S.result === r ? 'is-on' : ''}" aria-pressed="${S.result === r}" data-act="act.result" data-arg="${r}" data-k="act-r-${r}">${r}${r === 'injected' || r === 'unexpected' || r === 'denied' ? ' ' + counts[r] : ''}</button>`)}</span>
      <input class="lat-input" style="width:175px" name="q" placeholder="family, trace ( / )" aria-label="Filter requests" value="${S.q}" data-inp="act.q" data-k="act-q" autocomplete="off">
      <span class="grow"></span>
    </div>

    <div class="tiles" style="grid-template-columns: repeat(7, 1fr)">
      ${tile('Requests · cycle', cyc.calls ? int(cyc.calls) : '—', cyc.calls ? `${int(perfCyc)} perf · ${int(cyc.calls - perfCyc)} inventory` : 'no cycle yet')}
      ${tile('Rate', html`${a.ratePerSec.toFixed(1)}<small> /s</small>`, `peak ${peak.toFixed(1)} /s · last 60 min`)}
      ${tile('p50 · p95 · p99', html`${durs.length ? int(pctile(durs, 0.5)) : '—'} <small>·</small> ${durs.length ? int(pctile(durs, 0.95)) : '—'} <small>·</small> ${durs.length ? int(pctile(durs, 0.99)) : '—'}`, `ms · ${int(durs.length)} filtered requests`)}
      ${tile('Faults', int(a.faults.injected + a.faults.unexpected + (a.faults.denied || 0)), `${int(a.faults.injected)} injected · ${int(a.faults.unexpected)} unexpected · ${int(a.faults.denied || 0)} denied (last hour, all calls)`)}
      ${tile('Empty results', int(emptySum), 'entities echoed empty (measured, filtered)')}
      ${tile('Response bytes', html`${bytes(bytesSum).split(' ')[0]}<small> ${bytes(bytesSum).split(' ')[1]}</small>`, largest ? `largest call ${bytes(largest.bytes)} (${largest.family || largest.method})` : 'no calls')}
      ${tile('Entities with samples', int(seriesSum), 'measured in filtered calls; Overview projects series')}
    </div>

    <div class="g" style="grid-template-columns: 1fr 380px">
      <div class="g" style="grid-template-rows: auto auto; align-content:start">
        <section class="lat-card" aria-labelledby="act-log"><h2 class="lat-card-title" id="act-log">Request log <span class="aside">${rangeLabel[S.range]} · newest first · <kbd>j</kbd>/<kbd>k</kbd> move · <kbd>enter</kbd> inspect · <button type="button" class="link" data-act="act.csv">export CSV</button></span>
          ${S.frozen ? badge('frozen', 'warn') : badge('live', 'ok')}<button type="button" class="lat-btn lat-btn--sm" style="font-weight:400;text-transform:none;letter-spacing:0" data-act="act.freeze" data-k="act-freeze">${S.frozen ? 'Resume log' : 'Freeze log'}</button></h2>
          <div class="anno">Showing ${int(shown.length)}${rows.length > shown.length ? ` of ${int(rows.length)} matching` : ''} · ${truncated ? `the newest ${int(all.length)} of ${int(data.totals.held)} calls the log holds (ring of ${int(Math.max(data.totals.held, all.length))}); ` : `${int(data.totals.held)} calls held; `}${int(data.totals.requests)} since ${hms(data.totals.since)}${S.frozen ? ' · frozen: new requests are not shown' : ''}</div>
          ${shown.length ? html`<div class="lat-table-scroll log-scroll" data-scroll="log"><table class="lat-table" aria-label="Request log"><thead><tr><th>Time (UTC)</th><th>Trace</th><th>Path</th><th>Operation</th><th>Cluster</th><th>entityRefId / target</th><th>Result</th><th class="lat-num">ms</th><th class="lat-num">Bytes</th><th class="lat-num">Series</th></tr></thead>
            <tbody>${shown.map((c) => html`<tr class="is-click${c.seq === S.sel ? ' is-sel' : ''}" tabindex="0" data-seq="${c.seq}" data-act="act.sel" data-arg="${c.seq}" aria-selected="${c.seq === S.sel}"><td class="mono">${hmsms(c.time)}</td><td class="mono">${trace(c)}</td><td class="mono">${pathOf(c)}</td><td class="ink">${c.method}</td><td class="mono" title="${c.cluster}">${short(c.cluster)}</td><td class="mono ink">${target(c)}</td><td>${resultBadge(c)}</td><td class="lat-num">${int(c.durationMs)}</td><td class="lat-num">${bytes(c.bytes)}</td><td class="lat-num">${c.method === 'VsanPerfQueryPerf' ? int(series(c)) : ''}</td></tr>`)}</tbody></table></div>`
            : html`<div class="empty">No requests match these filters in ${rangeLabel[S.range]}. ${S.range !== '1h' ? html`<button type="button" class="link" data-act="act.range" data-arg="1h">Widen to 1 h</button>` : ''}</div>`}
        </section>

        <div class="g" style="grid-template-columns: 1.25fr 1fr">
          <section class="lat-card" aria-labelledby="act-fam"><h2 class="lat-card-title" id="act-fam">By entity family <span class="aside">last hour · ${a.families.length} kinds queried · by entities</span></h2>
            ${a.families.length ? html`<div class="scroll" style="max-height:230px" data-scroll="fam"><table class="lat-table"><thead><tr><th>Family</th><th class="lat-num">Calls</th><th class="lat-num">Entities</th><th class="lat-num">Empty</th><th class="lat-num">Bytes</th><th class="lat-num">p95</th><th class="lat-num">Faults</th></tr></thead>
              <tbody>${a.families.slice(0, 40).map((f) => html`<tr><td class="mono ink"><button type="button" class="link" style="text-decoration:none;color:inherit;font-family:inherit" data-act="act.family" data-arg="${f.family}">${f.family}</button></td><td class="num r">${int(f.calls)}</td><td class="num r">${int(f.entities)}</td><td class="num r">${int(f.empty)}</td><td class="num r">${bytes(f.bytes)}</td><td class="num r">${int(f.p95Ms)}</td><td class="num r">${f.injected + f.unexpected ? badge(int(f.injected + f.unexpected), f.unexpected ? 'bad' : 'warn') : '0'}</td></tr>`)}</tbody></table></div>`
              : html`<div class="empty">No performance queries in the last hour.</div>`}
          </section>
          <section class="lat-card" aria-labelledby="act-dur"><h2 class="lat-card-title" id="act-dur">Duration distribution <span class="aside">last hour · ${int(a.totals.requests)} requests total</span></h2>
            ${durationChart(a)}
          </section>
        </div>
      </div>

      <aside class="lat-card" aria-labelledby="act-insp" tabindex="-1" id="act-inspector"><h2 class="lat-card-title" id="act-insp">Request ${sel ? trace(sel) : ''} <span class="aside">read-only inspection</span></h2>
        ${sel ? inspector(sel, S.detail[sel.seq]) : html`<div class="empty">Select a request to inspect it (<kbd>j</kbd>/<kbd>k</kbd>, <kbd>enter</kbd>).</div>`}
      </aside>
    </div>`;
  },

  keys(ctx, ev, data, typing) {
    if (!data) return;
    if (ev.key === '/' && !typing) { ev.preventDefault(); document.querySelector('[data-k="act-q"]')?.focus(); return; }
    if (typing) { if (ev.key === 'Escape') ev.target.blur(); return; }
    if (ev.key !== 'j' && ev.key !== 'k' && ev.key !== 'Enter') return;
    const rows = filtered(ctx, data).slice(0, 300);
    if (!rows.length) return;
    const i = rows.findIndex((c) => c.seq === S.sel);
    if (ev.key === 'Enter') {
      ev.preventDefault();
      const focused = Number(ev.target && ev.target.dataset && ev.target.dataset.seq);
      if (focused) S.sel = focused;
      ensureDetail(ctx, S.sel || rows[0].seq);
      document.getElementById('act-inspector')?.focus();
      return;
    }
    ev.preventDefault();
    const next = rows[Math.max(0, Math.min(rows.length - 1, i < 0 ? 0 : i + (ev.key === 'j' ? 1 : -1)))];
    select(ctx, next.seq);
  },

  actions: {
    'act.range'(ctx, el) { S.range = el.dataset.arg; ctx.rerender(); },
    'act.surface'(ctx, el) { S.surface = el.dataset.arg; ctx.rerender(); },
    'act.result'(ctx, el) { S.result = el.dataset.arg; ctx.rerender(); },
    'act.filter'(ctx, el) { S[el.dataset.f] = el.value; ctx.rerender(); },
    'act.q'(ctx, el) { S.q = el.value; ctx.rerender(); },
    'act.family'(ctx, el) { S.family = el.dataset.arg; S.range = '1h'; ctx.rerender(); },
    'act.sel'(ctx, el) { select(ctx, Number(el.dataset.arg), false); },
    'act.freeze'(ctx) {
      S.frozen = !S.frozen;
      if (!S.frozen) { S.cache = null; ctx.reload(); } else ctx.rerender();
    },
    'act.csv'(ctx, el, ev, data) {
      const rows = filtered(ctx, data);
      const url = URL.createObjectURL(new Blob([csv(rows)], { type: 'text/csv' }));
      const a = document.createElement('a');
      a.href = url; a.download = `request-log-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '')}.csv`;
      document.body.appendChild(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      ctx.toast(`Exported ${int(rows.length)} rows (redacted: no bodies, cookies or credentials).`, 'ok');
    },
    'act.cycle'(ctx, el) { S.range = 'cycle'; S.result = el.dataset.arg; S.surface = ''; S.method = ''; S.family = ''; S.cluster = ''; S.q = ''; ctx.rerender(); },
  },
};

function select(ctx, seq, scroll = true) {
  S.sel = seq;
  ensureDetail(ctx, seq);
  ctx.rerender();
  if (scroll) requestAnimationFrame(() => document.querySelector(`tr[data-seq="${seq}"]`)?.scrollIntoView({ block: 'nearest' }));
}

function durationChart(a) {
  const d = a.duration;
  const max = Math.max(1, ...d.map((b) => b.count));
  const w = 460, h = 130, top = 14, bw = w / d.length;
  const total = d.reduce((s, b) => s + b.count, 0);
  // the bucket holding p95
  let acc = 0, p95i = d.length - 1;
  for (let i = 0; i < d.length; i++) { acc += d[i].count; if (total && acc / total >= 0.95) { p95i = i; break; } }
  const bars = d.map((b, i) => {
    const bh = ((h - top) * b.count) / max;
    const x = i * bw + 6;
    return `<rect x="${x.toFixed(1)}" y="${(h - bh).toFixed(1)}" width="${(bw - 12).toFixed(1)}" height="${bh.toFixed(1)}" style="fill:var(--ink-muted)"><title>${b.label}: ${b.count}</title></rect>
      <text x="${(x + (bw - 12) / 2).toFixed(1)}" y="${(h - bh - 3).toFixed(1)}" text-anchor="middle" style="fill:var(--ink);font:10.5px var(--font-mono)">${b.count || ''}</text>
      <text x="${(x + (bw - 12) / 2).toFixed(1)}" y="${h + 12}" text-anchor="middle" style="fill:var(--ink-muted);font:10px var(--font-mono)">${b.label.replace(' ms', '').replace(' s', 's')}</text>`;
  }).join('');
  const mark = total ? `<line x1="${(p95i * bw + bw / 2).toFixed(1)}" x2="${(p95i * bw + bw / 2).toFixed(1)}" y1="0" y2="${h}" style="stroke:var(--ink);stroke-dasharray:3 3"/><text x="${Math.min(w - 40, p95i * bw + bw / 2 + 4).toFixed(1)}" y="10" style="fill:var(--ink);font:10.5px var(--font-mono)">p95 ${ms(a.p95Ms)}</text>` : '';
  return raw(total
    ? `<svg class="spark" viewBox="0 0 ${w} ${h + 18}" height="${h + 18}" role="img" aria-label="Duration distribution of ${total} requests in the last hour; p95 ${ms(a.p95Ms)}">${bars}${mark}</svg>`
    : '<div class="empty">No requests in the last hour.</div>');
}

function inspector(c, detail) {
  const inj = c.result === 'injected';
  const flt = c.result === 'fault';
  const den = c.result === 'denied';
  const reading = c.fault && (READING[c.fault] || (c.fault.toLowerCase() === 'timeout' ? READING.timeout : null));
  const body = detail && !detail.loading && !detail.gone ? detail.body : '';
  return html`
    <div class="row wrap">${resultBadge(c)}${c.fault ? html`<span class="mono ink">${c.fault}</span>` : ''}${inj ? badge('scenario fault rule', '') : ''}</div>
    <dl class="kv">
      <dt>Operation</dt><dd class="sans">${c.method} · <span class="mono">${pathOf(c)}</span></dd>
      <dt>Cluster</dt><dd>${c.cluster || '—'}</dd>
      ${c.family ? html`<dt>Target</dt><dd>${target(c)}</dd>` : ''}
      <dt>Client</dt><dd>${c.client}</dd>
      <dt>Time</dt><dd>${hmsms(c.time)}Z</dd>
      <dt>Duration</dt><dd>${ms(c.durationMs)}</dd>
      <dt>Response</dt><dd>${bytes(c.bytes)} · HTTP ${c.status}${c.method === 'VsanPerfQueryPerf' ? ` · ${int(c.entities)} entities, ${int(c.empty)} empty` : ''}</dd>
      <dt>Session</dt><dd class="sans">cookie never stored</dd>
    </dl>
    ${den ? html`<div class="lat-banner lat-banner--info"><b>How the MP reads this.</b> The request was refused before any rule or family was consulted: it carried no valid session, or the login was wrong. The collector cannot log in, so it collects nothing. Fix the adapter's credentials against the accepted logins on Overview; fault rules are not involved.</div>` : ''}
    ${(inj || flt || den) && c.message ? html`<div class="well anno" style="font-size:11.5px"><b>Fault message</b> · <span class="mono">${c.message}</span></div>` : ''}
    ${inj && reading ? html`<div class="lat-banner lat-banner--info"><b>How the MP reads this.</b> <span class="mono">${reading[0]}</span>: ${reading[1]}</div>`
      : flt ? html`<div class="lat-banner lat-banner--bad">This fault did not come from a scenario rule. Open Telemetry to check the rules, then the request body below.</div>` : ''}
    <h2 class="lat-card-title" style="margin-top:2px">Request body <span class="aside">credentials and cookies never stored</span></h2>
    ${c.surface === 'sdk'
      ? html`<div class="well anno">Bodies of /sdk calls are not kept (a login carries a password).</div>`
      : detail && detail.loading ? html`<div class="well anno">Loading…</div>`
      : detail && detail.gone ? html`<div class="well anno">${detail.message || 'This request has left the log.'}</div>`
      : body ? html`<pre class="well code-well mono" tabindex="0" aria-label="Request body">${xmlPretty(body)}</pre>${body.length >= 2048 ? html`<div class="anno">Cut at 2 KB.</div>` : ''}`
      : html`<div class="well anno">No body was kept for this request.</div>`}`;
}

function diagnosisCard(d, all, ctx) {
  const { cyc, cause } = d;
  const a = Date.parse(cyc.start) - 1, b = Date.parse(cyc.end) + 1;
  const inCycle = all.filter((c) => { const t = Date.parse(c.time); return t >= a && t <= b; });
  const bad = inCycle.filter((c) => resultOf(c) !== 'ok');
  const rows = bad.slice(0, 8);
  const secondary = cyc.findings.filter((f) => f !== cause);
  return html`<div class="g" style="grid-template-columns: 1fr 1fr">
    <section class="lat-card" aria-labelledby="dg-t"><h2 class="lat-card-title" id="dg-t">Diagnosis · cycle ${hms(cyc.start)} <span class="aside">client ${cyc.client || '—'} · ${int(cyc.calls)} requests</span></h2>
      <table class="lat-table"><thead><tr><th>Check</th><th class="lat-num">Calls</th><th>Result</th><th>Evidence</th></tr></thead><tbody>
        ${cyc.steps.map((s) => html`<tr><td class="ink">${s.name}</td><td class="num r">${int(s.calls)}</td><td>${stepBadge(s, cyc)}</td><td class="wrap">${stepEvidence(s, cyc)}</td></tr>`)}
        ${cyc.findings.map((f) => html`<tr><td class="ink wrap" colspan="1">${f.title}</td><td></td><td>${badge(f.severity === 'info' ? 'note' : f.severity === 'ok' ? 'ok' : f.severity === 'bad' ? 'problem' : 'warn', sevKind(f.severity))}</td><td class="wrap">${f.detail}</td></tr>`)}
      </tbody></table>
      ${cause ? html`<h2 class="lat-card-title" style="margin-top:4px">Most likely cause</h2><div class="well"><b>${cause.title}.</b> ${cause.detail}</div>` : ''}
      <div class="row"><a class="lat-btn" href="#/telemetry">Open Telemetry</a><span class="anno">Fault rules and latency live there.</span></div>
    </section>
    <section class="lat-card" aria-labelledby="rc-t"><h2 class="lat-card-title" id="rc-t">Requests in this cycle <span class="aside">not ok · ${int(bad.length)} of ${int(inCycle.length)}</span></h2>
      ${rows.length ? html`<table class="lat-table"><thead><tr><th>Time</th><th>Cluster</th><th>Target</th><th>Result</th><th class="lat-num">ms</th></tr></thead><tbody>
        ${rows.map((c) => html`<tr class="is-click" data-act="act.sel" data-arg="${c.seq}"><td class="mono">${hmsms(c.time)}</td><td class="mono">${c.cluster || '—'}</td><td class="mono ink">${target(c) || c.method}</td><td>${resultBadge(c)}</td><td class="num r">${int(c.durationMs)}</td></tr>`)}</tbody></table>
        <div class="row"><button type="button" class="lat-btn lat-btn--sm" data-act="act.cycle" data-arg="${bad.some((c) => resultOf(c) === 'empty') ? 'empty' : 'all'}">Filter the log to this cycle</button><span class="anno">${bad.length > rows.length ? `${int(rows.length)} of ${int(bad.length)} shown` : ''}</span></div>`
        : html`<div class="empty">Nothing in this cycle is empty or faulted; the cause is upstream of the simulator (login, cluster discovery or the adapter’s scope).</div>`}
      <div class="well anno"><b>Other causes this screen checks for</b> · refused or failed logins · no performance queries after a login · every kind answered empty · injected and unexpected faults. Clock offset between collector and simulator is not checked.</div>
    </section></div>`;
}
