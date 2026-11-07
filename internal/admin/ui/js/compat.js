import { api, html, join, badge, banner, int, ms, hms, plural } from './lib.js';

const statusKind = { verified: 'ok', partial: 'warn', unverified: 'bad' };
const kindOrder = [['data', 'Data', 'entities with samples'], ['empty', 'Empty', 'answered with an echo and no samples'], ['invalid-argument', 'Invalid argument', 'SOAP fault InvalidArgument'], ['not-supported', 'Not supported', 'SOAP fault NotSupported (PMem)']];
const sevKind = { bad: 'bad', warn: 'warn', ok: 'ok', info: '' };

function matrix(c) {
  const ms_ = c.methods;
  const counts = { verified: 0, partial: 0, unverified: 0 };
  ms_.forEach((m) => { counts[m.status] = (counts[m.status] || 0) + 1; });
  return html`<section class="lat-card" aria-labelledby="cp-matrix"><h2 class="lat-card-title" id="cp-matrix">Method matrix <span class="aside">${counts.verified} verified · ${counts.partial} partial · ${counts.unverified} unverified · calls seen since start</span></h2>
    <table class="lat-table"><thead><tr><th>Method</th><th>Basis</th><th>Status</th><th class="r">Calls</th><th class="r">Faults</th></tr></thead><tbody>
    ${ms_.map((m) => html`<tr><td class="wrap"><span class="ink mono">${m.method}</span><div class="anno mono">/${m.surface} · ${m.object}</div></td><td>${m.basis}</td><td>${badge(m.status, statusKind[m.status] || '')}</td><td class="num r">${int(m.callsSeen)}</td><td class="num r">${m.faultsSeen ? badge(int(m.faultsSeen), 'warn') : html`<span class="subtle">0</span>`}</td></tr>
      <tr><td colspan="5" class="wrap subtle" style="padding-top:0;border-top:0;font-size:11.5px">${m.note}</td></tr>`)}
    </tbody></table></section>`;
}

function unlisted(c) {
  return html`<section class="lat-card" aria-labelledby="cp-unl"><h2 class="lat-card-title" id="cp-unl">Seen in traffic, not in the matrix <span class="aside">${c.unlisted.length ? plural(c.unlisted.length, 'operation') : 'none'}</span></h2>
    ${c.unlisted.length ? html`<table class="lat-table"><thead><tr><th>Method</th><th>Surface</th><th class="r">Calls</th><th class="r">Faults</th><th>Status</th></tr></thead><tbody>
      ${c.unlisted.map((m) => html`<tr><td class="ink mono">${m.method}</td><td class="mono">/${m.surface}</td><td class="num r">${int(m.callsSeen)}</td><td class="num r">${int(m.faultsSeen)}</td><td>${badge('unverified', 'bad')}</td></tr>`)}</tbody></table>
      <div class="anno">The collector called operations the simulator serves from stock vcsim and nothing here has verified.</div>`
      : html`<div class="empty">Every operation the collector has called is in the matrix above.</div>`}
  </section>`;
}

function storage() {
  const row = (t, sdk, kinds, reads) => html`<tr><td class="ink">${t}</td><td>${sdk}</td><td class="mono">${kinds}</td><td class="wrap">${reads}</td></tr>`;
  return html`<section class="lat-card" aria-labelledby="cp-stor"><h2 class="lat-card-title" id="cp-stor">Storage coverage <span class="aside">what the reference MP reads · static, from the recon report</span></h2>
    <table class="lat-table"><thead><tr><th>Type</th><th>/sdk inventory</th><th>Perf kinds</th><th>MP reads</th></tr></thead><tbody>
    ${row('vSAN ESA', badge('impl', 'ok'), 'served', 'yes')}
    ${row('vSAN OSA', badge('planned'), 'disk-group, cache/capacity disk', 'yes (empty on ESA)')}
    ${row('vSAN iSCSI target', badge('planned'), 'vsan-iscsi-*', 'yes, no view')}
    ${row('NFS · FC · iSCSI · local', badge('planned'), 'none', html`<span class="muted">no · native adapter</span>`)}
    </tbody></table></section>`;
}

function kinds(c) {
  const total = Object.values(c.kinds).reduce((a, b) => a + b, 0);
  return html`<section class="lat-card" aria-labelledby="cp-kinds"><h2 class="lat-card-title" id="cp-kinds">Entity kind coverage <span class="aside">${total} kinds advertised · from the captured catalog</span></h2>
    <table class="lat-table"><thead><tr><th>Behaviour on ESA</th><th class="r">Kinds</th><th>Meaning</th></tr></thead><tbody>
    ${kindOrder.map(([k, label, d]) => html`<tr><td class="ink">${label}</td><td class="num r">${int(c.kinds[k] || 0)}</td><td class="wrap muted">${d}</td></tr>`)}</tbody></table>
    <div class="anno">Behaviour is what the live service did for a wildcard query in the capture. Values are synthetic inside each label's observed range.</div></section>`;
}

function health(c) {
  const cy = c.lastCycle;
  return html`<section class="lat-card" aria-labelledby="cp-health"><h2 class="lat-card-title" id="cp-health">MP collection health <span class="aside">inferred from requests${cy.calls ? ' · cycle ' + hms(cy.start) : ''}</span></h2>
    ${cy.calls ? html`<dl class="kv"><dt>Client</dt><dd>${cy.client}</dd><dt>Calls</dt><dd>${int(cy.calls)}</dd><dt>Entities with data</dt><dd>${int(cy.entitiesWithData)}</dd><dt>Empty entities</dt><dd>${int(cy.emptyEntities)}</dd></dl>
      <table class="lat-table"><thead><tr><th>Step</th><th class="r">Calls</th><th class="r">P95</th><th>Result</th></tr></thead><tbody>
      ${cy.steps.map((s) => html`<tr><td class="ink">${s.name}</td><td class="num r">${int(s.calls)}</td><td class="num r">${ms(s.p95Ms)}</td><td>${s.unexpected ? badge(`${s.unexpected} faults`, 'bad') : s.denied ? badge(`${s.denied} denied`, 'bad') : s.name === 'VsanPerfQueryPerf' && s.calls > 0 && s.series === 0 && !s.injected ? badge('0 series', 'bad') : s.injected ? badge(`${s.injected} injected`, 'warn') : badge('ok', 'ok')}</td></tr>`)}</tbody></table>
      <div class="stack">${cy.findings.map((f) => html`<div class="row" style="align-items:flex-start">${badge(f.severity, sevKind[f.severity])}<span><b>${f.title}</b> <span class="muted">${f.detail}</span></span></div>`)}</div>`
    : html`<div class="empty">No collector traffic yet. Run the pack against this endpoint (mp-test or an adapter instance) and its cycle appears here.</div>`}
    <dl class="kv"><dt>VCF Operations</dt><dd class="sans">not attached · end-to-end unverified (Phase 3)</dd></dl></section>`;
}

function pending(c) {
  return html`<section class="lat-card" aria-labelledby="cp-pend"><h2 class="lat-card-title" id="cp-pend">Still unverified</h2>
    <ul style="margin:0;padding-left:18px;display:grid;gap:4px">${c.unverified.map((u) => html`<li>${u}</li>`)}</ul>
    <div class="anno">Fixture files are not served by the console; browse <span class="mono">fixtures/live/9.1.1</span> in the repository.</div></section>`;
}

export default {
  id: 'compat',
  title: 'Compatibility',
  pollMs: 8000,
  async load() { return api('/compat'); },
  render(ctx, c) {
    return html`${banner('info', html`<b>Verified against the captured fixtures only.</b> ${c.profile}, ${c.verifiedBy}. Nothing here claims behaviour of a live vCenter beyond them; “verified” means the simulator's answer matched a captured one, not that a vCenter was asked today.`)}
    <div class="g" style="grid-template-columns:1.75fr 1fr">
      <div class="g" style="align-content:start">${matrix(c)}${unlisted(c)}${storage()}</div>
      <div class="g" style="align-content:start">${health(c)}${kinds(c)}${pending(c)}</div>
    </div>`;
  },
  actions: {},
};
