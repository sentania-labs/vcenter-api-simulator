// Storage: shared iSCSI and NFS datastores.
import { api, html, raw, badge, btn, link, lbtn, card, table, dot, plural, list, lower1 } from './lib.js';
import { taskScreen } from './task.js';

const cond = (s) => (s.cut.length ? html`${dot('warn')} ${plural(s.cut.length, 'host')} cannot reach it` : html`${dot('ok')} Every host can reach it`);

const listScreen = {
  title: 'Storage', tab: 'storage', poll: true,
  load: () => api('/storage'),
  render(ctx, d) {
    const rows = d.rows.map((s) => {
      const tag = ctx.mark('storage', s.id);
      return { _cls: tag ? 'is-new' : '', name: html`<a class="rowlink" href="#/storage/${s.id}">${s.name}</a> ${tag ? badge('ok', tag) : ''}`, kind: s.kind, dc: s.dc, by: s.usedBy, size: s.size, free: s.free, cond: cond(s), act: lbtn('Open', '/storage/' + s.id) };
    });
    const cols = [{ h: 'Name', k: 'name' }, { h: 'Kind', k: 'kind' }, ...(d.multi ? [{ h: 'Datacenter', k: 'dc' }] : []), { h: 'Used by', k: 'by' }, { h: 'Size', k: 'size', cls: 'num' }, { h: 'Free', k: 'free', cls: 'num' }, { h: 'Condition', k: 'cond' }, { h: '', k: 'act', cls: 'act dup' }];
    return html`<div class="m-page"><div class="m-head"><div><h1>Storage</h1><p class="sub">Shared datastores that hosts connect to. An iSCSI datastore belongs to one cluster; an NFS datastore can be shared by several. It is all pretend: the simulator never contacts a real storage server.</p></div>
      <div class="acts">${lbtn('Add an iSCSI datastore', '/storage/add-iscsi', 'lat-btn--primary')}${lbtn('Add an NFS datastore', '/storage/add-nfs', 'lat-btn--primary')}</div></div>
      ${card('Shared storage', rows.length ? table(cols, rows)
        : html`<div class="m-empty"><h3>No shared storage yet</h3><p>Use the buttons above to add an iSCSI datastore for one cluster, or an NFS datastore that several clusters share.</p></div>`, rows.length ? plural(rows.length, 'item') : '')}
      ${card('Made automatically', html`<p class="m-muted">Every cluster also has its own vSAN datastore, and hosts have local disks. There ${d.clusters === 1 ? 'is' : 'are'} ${plural(d.clusters, 'vSAN datastore')} for ${plural(d.hosts, 'host')} right now. They come and go with their cluster, so there is nothing to set up here. ${link('See clusters', '/clusters')}</p>`)}</div>`;
  },
};

function itemScreen(id) {
  return {
    title: 'Storage', tab: 'storage', poll: true, back: ['Back to storage', '/storage'],
    load: () => api('/storage/' + id),
    render(ctx, s) {
      const tag = ctx.mark('storage', s.id);
      return html`<div class="m-page"><div class="m-crumb">${link('Storage', '/storage')} ${raw('&rsaquo;')} ${s.name}</div>
        <div class="m-head"><div><h1>${s.name} ${tag ? badge('ok', tag) : ''}</h1><p class="sub">${s.kind} datastore. ${s.size}, ${s.full.toLowerCase()} (${s.free} free). Used by ${lower1(s.usedBy)}.</p></div>
          <div class="acts">${lbtn('Change size', `/storage/${s.id}/change`)}${btn('Remove…', 'removeStore')}</div></div>
        <div class="m-grid m-g2">${card('Can hosts reach it?', html`<p>${cond(s)}${s.cut.length ? ': ' + list(s.cut) + '.' : '.'}</p>
            <p class="m-small" style="margin:8px 0">To test how a tool reports a storage problem, make some hosts lose their connection to it.</p>
            <div class="m-row">${lbtn('Disconnect some hosts', `/storage/${s.id}/problem`)}${s.cut.length ? btn('Reconnect every host', 'reconnect', 'lat-btn--primary') : ''}</div>`)}
          ${card('Connection details tools will see', html`<dl class="m-kv">${s.details.map(([k, v], i) => html`<dt>${k}</dt><dd>${i < 2 ? html`<code>${v}</code>` : v}</dd>`)}</dl><p class="m-small" style="margin-top:8px">These are pretend. Nothing real is contacted.</p>`)}</div></div>`;
    },
    actions: {
      async reconnect(ctx) { await ctx.act(async () => { const r = await api(`/storage/${id}/reconnect`, { method: 'POST' }); return { done: `**Done.** ${r.done}` }; }); },
      async removeStore(ctx) {
        const s = ctx.data;
        const yes = await ctx.confirm({ title: `Remove ${s.name}?`, danger: true, ok: 'Remove storage', body: html`<p>The datastore disappears from ${lower1(s.usedBy)}. Your tool will see it as deleted.</p><p>Clusters, hosts and VMs are not touched.</p>` });
        if (!yes) return;
        try {
          const r = await api('/storage/' + id, { method: 'DELETE' });
          ctx.flash = { tone: 'ok', text: `**Done.** ${r.done}`, route: '/storage' };
          ctx.go('/storage');
        } catch (e) { ctx.say('bad', `**${s.name} was not removed.** ${e.message}`); await ctx.reload(); }
      },
    },
  };
}

export default {
  screen(p, q) {
    if (p[1] === 'add-iscsi' || p[1] === 'add-nfs') return taskScreen(p[1], q, 'storage', 'Storage');
    if (!p[1]) return listScreen;
    const id = `${p[1]}/${p[2]}`;
    if (p[3] === 'problem') return taskScreen('storage-disconnect', { ...q, id }, 'storage', 'Storage');
    if (p[3] === 'change') return taskScreen('storage-size', { ...q, id }, 'storage', 'Storage');
    return itemScreen(id);
  },
};
