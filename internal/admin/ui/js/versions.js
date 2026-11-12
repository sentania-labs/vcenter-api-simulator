// Versions: which release vCenter and its hosts say they are.
import { api, html, raw, badge, lbtn, card, table, fit, plural, num, join, pager, pageOf } from './lib.js';
import { taskScreen } from './task.js';

const main = {
  title: 'Versions', tab: 'versions', poll: true,
  load: () => api('/versions'),
  render(ctx, d) {
    const F = ctx.F, q = (F.q || '').trim().toLowerCase(), per = fit(540);
    const rows = d.rows.filter((c) => !q || c.name.includes(q));
    const shown = pageOf(ctx, 'ver', rows, per).map((c) => {
      const tag = ctx.mark('cluster', c.key);
      return { _cls: tag ? 'is-new' : '', name: html`${c.name} ${tag ? badge('ok', tag) : ''}`, dc: c.dc, hosts: c.hosts, ver: c.mixed ? html`${badge('accent', 'Mixed')} ${c.label}` : c.label, act: lbtn('Change', '/versions/cluster/' + c.key) };
    });
    const cols = [{ h: 'Cluster', k: 'name' }, ...(d.multi ? [{ h: 'Datacenter', k: 'dc' }] : []), { h: 'Hosts', k: 'hosts', cls: 'num' }, { h: 'ESX version', k: 'ver' }, { h: '', k: 'act', cls: 'act' }];
    return html`<div class="m-page"><div class="m-head"><div><h1>Versions</h1><p class="sub">Which release the simulated vCenter and its hosts say they are. Tools read this to decide how to talk to them.</p></div></div>
      <div class="m-grid m-g2">${card('vCenter', html`<div class="m-row"><span class="m-lead">vCenter <b>${d.vcenter}</b></span><span class="m-sp"></span>${lbtn('Change vCenter version', '/versions/vcenter', 'lat-btn--primary')}</div>`)}
        ${card('Hosts', html`<p class="m-lead">${d.byEsx.length === 1 ? html`All ${plural(d.hosts, 'host')} run <b>ESX ${d.byEsx[0].label}</b>.` : join(d.byEsx.map((v) => html`<b>${num(v.hosts)}</b> on ESX ${v.label}`), ' · ')}</p><p class="m-small">To change hosts, press Change next to their cluster below. One cluster can hold a mix.</p>`)}</div>
      ${card('ESX version in each cluster', html`<div class="m-row" style="margin-bottom:10px"><input class="lat-input" style="max-width:260px" id="f-q" data-f="q" value="${F.q || ''}" placeholder="Find a cluster by name" aria-label="Find a cluster by name"></div>
        ${rows.length ? html`${table(cols, shown)}${pager(ctx, 'ver', rows.length, per)}` : html`<div class="m-empty"><h3>No cluster is called "${F.q}"</h3><p>Check the spelling, or clear the box to see them all.</p></div>`}`)}</div>`;
  },
  input(ctx, el) { if (el.dataset.f) { ctx.F[el.dataset.f] = el.value; ctx.page.ver = 0; ctx.rerender(); } },
};

export default {
  screen(p, q) {
    if (p[1] === 'vcenter') return taskScreen('vcenter-version', q, 'versions', 'Versions');
    if (p[1] === 'cluster' && p[2] && p[3]) return taskScreen('cluster-versions', { ...q, cluster: `${p[2]}/${p[3]}` }, 'versions', 'Versions');
    return main;
  },
};
export { raw };
