// Clusters: the list, one cluster's page, and the tasks that change them.
import { api, html, raw, badge, btn, link, lbtn, card, table, fit, plural, num, list, pager, pageOf } from './lib.js';
import { taskScreen } from './task.js';

const listScreen = {
  title: 'Clusters', tab: 'clusters', poll: true,
  load: () => api('/clusters'),
  render(ctx, d) {
    const F = ctx.F, q = (F.q || '').trim().toLowerCase(), dc = F.dc || 'all';
    const rows = d.rows.filter((c) => (!q || c.name.includes(q)) && (dc === 'all' || c.dc === dc));
    const per = fit(372);
    const shown = pageOf(ctx, 'cl', rows, per).map((c) => {
      const tag = ctx.mark('cluster', c.key);
      return { _cls: tag ? 'is-new' : '', name: html`<a class="rowlink" href="#/cluster/${c.key}">${c.name}</a> ${tag ? badge('ok', tag) : ''}`, dc: c.dc, hosts: c.hosts, vms: num(c.vms), vsan: c.vsan, ver: c.version, act: lbtn('Open', '/cluster/' + c.key) };
    });
    const dcs = d.datacenters;
    const listCard = card('', html`<div class="m-row" style="margin-bottom:10px"><input class="lat-input" style="max-width:260px" id="f-q" data-f="q" value="${F.q || ''}" placeholder="Find a cluster by name" aria-label="Find a cluster by name">
        ${dcs.length > 1 ? html`<label for="f-dc" class="m-small">Datacenter</label><select class="lat-select" style="max-width:160px" id="f-dc" data-f="dc">${[['all', 'All of them'], ...dcs.map((x) => [x, x])].map(([v, t]) => html`<option value="${v}" ${dc === v ? raw('selected') : ''}>${t}</option>`)}</select>` : ''}
        <span class="m-small">${rows.length === d.rows.length ? plural(rows.length, 'cluster') : `${rows.length} of ${plural(d.rows.length, 'cluster')}`}</span></div>
      ${rows.length ? html`${table([{ h: 'Cluster', k: 'name' }, { h: 'Datacenter', k: 'dc' }, { h: 'Hosts', k: 'hosts', cls: 'num' }, { h: 'VMs', k: 'vms', cls: 'num' }, { h: 'vSAN type', k: 'vsan' }, { h: 'Version', k: 'ver' }, { h: '', k: 'act', cls: 'act dup' }], shown)}${pager(ctx, 'cl', rows.length, per)}`
        : html`<div class="m-empty"><h3>${q ? `No cluster is called "${F.q}"` : 'No cluster is in that datacenter'}</h3><p>Check the spelling, or clear the box to see them all.</p></div>`}`);
    return html`<div class="m-page"><div class="m-head"><div><h1>Clusters and hosts</h1><p class="sub">${plural(d.rows.length, 'cluster')}, ${plural(d.hosts, 'host')} and ${plural(d.vms, 'VM')} in ${plural(dcs.length, 'datacenter')}. Open a cluster to see its hosts, or to add and remove hosts.</p></div>
      <div class="acts">${lbtn('Add a cluster', '/clusters/add', 'lat-btn--primary')}</div></div>
      ${listCard}<p class="m-small">Starting a new test at a different size? ${link('Replace everything with a new environment', '/clusters/build')}. That removes every cluster listed above and builds new ones.</p></div>`;
  },
  input(ctx, el) { if (el.dataset.f) { ctx.F[el.dataset.f] = el.value; ctx.page.cl = 0; ctx.rerender(); } },
};

function detailScreen(key) {
  return {
    title: 'Clusters', tab: 'clusters', poll: true, back: ['Back to all clusters', '/clusters'],
    load: () => api('/clusters/' + key),
    render(ctx, c) {
      const per = fit(352, 34), n = c.hosts.length;
      const isNew = (h) => ctx.mark('host', `${c.key}/${h.name}`);
      const nNew = c.hosts.filter(isNew).length, tail = nNew > 0 && n > per && !ctx.F.fromTop;
      const from = tail ? Math.max(0, n - Math.max(per, nNew)) : 0;
      const slice = tail ? c.hosts.slice(from) : pageOf(ctx, 'h-' + c.key, c.hosts, per);
      const rows = slice.map((h) => ({ _cls: isNew(h) ? 'is-new' : '', n: html`${h.name} ${isNew(h) ? badge('ok', isNew(h)) : ''}`, v: h.version, vms: h.vms, d: h.disks, st: 'Connected' }));
      const own = c.local === 1 ? 'Made automatically, with one local disk per host.' : c.local > 1 ? `Made automatically, with ${c.local} local disks per host.` : 'Made automatically.';
      return html`<div class="m-page"><div class="m-crumb">${link('Clusters', '/clusters')} ${raw('&rsaquo;')} ${c.name}</div>
        <div class="m-head"><div><h1>${c.name} ${ctx.mark('cluster', c.key) ? badge('ok', ctx.mark('cluster', c.key)) : ''}</h1><p class="sub">In ${c.dc}. ${c.vsan}. ${plural(n, 'host')} and ${plural(c.vms, 'VM')}. ${c.version}.</p></div>
          <div class="acts">${c.canShrink ? lbtn('Remove hosts', `/cluster/${c.key}/remove-hosts`) : ''}${c.canGrow ? lbtn('Add hosts', `/cluster/${c.key}/add-hosts`, 'lat-btn--primary') : ''}</div></div>
        <div class="m-grid m-g21">${card('Hosts', html`${table([{ h: 'Host', k: 'n' }, { h: 'Version', k: 'v' }, { h: 'VMs', k: 'vms', cls: 'num' }, { h: 'Disks', k: 'd' }, { h: 'State', k: 'st' }], rows)}
            ${tail ? html`<div class="m-pager"><span>Showing hosts ${from + 1} to ${n} of ${n}, so that every new one is in view.</span>${btn('Show from the first host', 'hostsTop')}</div>` : pager(ctx, 'h-' + c.key, n, per)}
            ${c.canShrink ? '' : html`<p class="m-small" style="margin-top:8px">This cluster has ${plural(n, 'host')}, the fewest vSAN allows, so none can be removed.</p>`}`, plural(n, 'host'))}
          <div class="m-stack">${card('Storage this cluster uses', html`<ul class="m-list"><li><div class="grow"><div class="t">Its own ${c.vsan} datastore</div><div class="d">${own}</div></div></li>
              ${c.storage.map((s) => html`<li><div class="grow"><div class="t">${link(s.name, '/storage/' + s.id)}</div><div class="d">${s.detail}</div></div></li>`)}</ul>
              <div class="m-row" style="margin-top:8px">${lbtn('Add an iSCSI datastore', '/storage/add-iscsi?cluster=' + encodeURIComponent(c.key))}${lbtn('Add an NFS datastore', '/storage/add-nfs?cluster=' + encodeURIComponent(c.key))}</div>`)}
            ${card('Other things you can do', html`<div class="m-row">${lbtn('Change host hardware', `/cluster/${c.key}/hardware`)}${lbtn('Change versions', '/versions/cluster/' + c.key)}${btn('Remove this cluster…', 'removeCluster')}</div>
              <p class="m-small" style="margin-top:10px"><a href="#" data-act="clusterIds">Show the internal ids</a> that tools use for this cluster. Only needed when matching against a tool's own logs.</p>`)}</div></div></div>`;
    },
    actions: {
      hostsTop(ctx) { ctx.F.fromTop = true; ctx.page['h-' + key] = 0; ctx.rerender(); },
      clusterIds(ctx) {
        const c = ctx.data;
        ctx.dialog({ title: `Internal ids for ${c.name}`, body: html`<p>Tools refer to objects by these ids. They stay the same unless you replace the whole environment.</p>
          <dl class="m-kv"><dt>Cluster id</dt><dd><code>${c.id}</code></dd><dt>vSAN id</dt><dd><code>${c.vsanId}</code></dd>${c.hosts.map((h) => html`<dt>${h.name}</dt><dd><code>${h.id}</code></dd>`)}</dl>` });
      },
      async removeCluster(ctx) {
        const c = ctx.data;
        if (c.only) {
          ctx.dialog({ title: 'This is the only cluster', body: html`<p>The simulator needs at least one cluster, so ${c.name} cannot be removed.</p><p>Add another cluster first, then remove this one. To start again at a different size, use "Replace everything with a new environment" on the Clusters screen.</p>` });
          return;
        }
        const yes = await ctx.confirm({ title: `Remove ${c.name}?`, danger: true, ok: 'Remove cluster', body: html`<p>Its ${plural(c.hosts.length, 'host')} and ${plural(c.vms, 'VM')} disappear from the simulated vCenter, along with storage that only this cluster uses.</p><p>Your tool will see them as deleted. Other clusters are not touched. This cannot be undone.</p>` });
        if (!yes) return;
        try {
          const r = await api('/clusters/' + c.key, { method: 'DELETE' });
          ctx.flash = { tone: 'ok', text: `**Done.** ${r.done}`, route: '/clusters' };
          ctx.go('/clusters');
        } catch (e) { ctx.say('bad', `**${c.name} was not removed.** ${e.message}`); await ctx.reload(); }
      },
    },
  };
}

export default {
  screen(p, q) {
    if (p[0] === 'clusters') {
      if (p[1] === 'add') return taskScreen('add-cluster', q, 'clusters', 'Add a cluster');
      if (p[1] === 'build') return taskScreen('replace-everything', q, 'clusters', 'Replace everything');
      return listScreen;
    }
    const key = `${p[1]}/${p[2]}`;
    if (['add-hosts', 'remove-hosts', 'hardware'].includes(p[3])) return taskScreen(p[3], { ...q, cluster: key }, 'clusters', 'Clusters');
    return detailScreen(key);
  },
};
export { list };
