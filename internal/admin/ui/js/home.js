// Home: what is running, what needs attention, who is connected and how to
// connect a tool. Everything on it comes from /api/home, which the shell
// already holds, so Home and the header cannot disagree.
import { api, html, raw, badge, btn, link, lbtn, card, dot, rich, plural, num, timeText, ago, pager, pageOf } from './lib.js';

const toolState = (t) => {
  const at = t.since ? timeText(t.since) : '';
  switch (t.state) {
    case 'removed': return `Was signed in as ${t.user} until you removed that user at ${at}. It is being refused now.`;
    case 'cert': return `Was signed in as ${t.user} until the certificate was replaced at ${at}. It has not reconnected: it has to be told to trust the new certificate first.`;
    case 'paused': return `Was signed in as ${t.user} until you paused the simulator at ${at}. It is being refused while the simulator is paused.`;
    case 'denied': return `Was signed in as ${t.user}. Its last sign-in, at ${at}, was refused. Check the username and password it uses.`;
    case 'signedout': return `Was signed in as ${t.user}. Since ${at} its requests have been refused, because its sign-in ended and it has not signed in again.`;
    case 'quiet': return `Was signed in as ${t.user}. It last asked for something at ${at} and has been quiet since.`;
    default: return `Signed in as ${t.user}. Last asked for something ${ago(t.last)}.`;
  }
};

function toolItem(t) {
  const off = t.state !== 'connected';
  const asked = t.asks
    ? html`${off ? 'Before that it asked' : 'It asks'} for ${t.asks}${t.unknown ? html`, and for <b>${plural(t.unknown, 'kind')} of request the simulator cannot answer</b>. ${t.carried ? `It carried on collecting without ${t.unknown === 1 ? 'it' : 'them'}` : 'It asked for nothing more after one of them'}` : ''}.${t.fault ? ` ${plural(t.fault, 'kind is', 'kinds are')} failing on purpose.` : ''}${!t.unknown && !t.fault ? ' Everything was answered.' : ''}`
    : html`It has asked for nothing in the last hour.`;
  return html`<li><span class="m-dot ${off || t.unknown ? 'bad' : t.fault ? 'warn' : 'ok'}"></span><div class="grow"><div class="t">${off ? 'Not connected now: the' : 'The'} tool at ${t.addr}</div>
    <div class="d">${t.agent ? `It calls itself "${t.agent}".` : 'It does not say what it is.'} ${toolState(t)}</div>
    <div class="d">${asked}</div>
    <div class="m-row" style="margin-top:8px">${lbtn('See what it asks for', '/requests')}</div></div></li>`;
}

const main = {
  title: 'Home', tab: 'home',
  render(ctx) {
    const h = ctx.home;
    if (!h) return html`<div class="m-page"><div class="m-loading" role="status">${ctx.homeError ? ctx.homeError.message : 'Loading…'}</div></div>`;
    const F = ctx.F, c = h.counts, a = h.attention, fresh = !h.tools.length, paused = !h.running;
    const live = h.tools.filter((t) => t.state === 'connected'), c0 = h.tools[0];
    // a refused tool's "since" is its latest refusal, which keeps moving: it was connected until it was last served
    const until = c0 && (c0.state === 'denied' ? c0.last : c0.since);
    const toolLine = paused ? `Tools cannot connect until you resume.${c0 && c0.state === 'paused' ? ` One was connected until you paused at ${timeText(h.pausedAt)}.` : ''}`
      : fresh ? 'No tool has connected to it yet.'
        : live.length ? (live.length === 1 ? '1 tool is connected.' : `${live.length} tools are connected.`)
          : `No tool is connected right now.${until ? ` One was until ${timeText(until)}.` : ''}`;
    const con = h.connect;
    const hero = html`<section class="lat-card"><div class="m-head"><div><h1>${paused ? 'The simulator is paused' : 'The simulator is running'}</h1>
      <p class="sub">It is pretending to be <b>vCenter ${h.vcenter}</b> with <b>${plural(c.clusters, 'cluster')}</b>, <b>${plural(c.hosts, 'host')}</b> and <b>${plural(c.vms, 'VM')}</b>. ${toolLine}${a.length ? html` <b style="color:var(--bad-ink)">${plural(a.length, 'thing needs', 'things need')} your attention.</b>` : ''}</p></div></div>
      ${fresh ? html`<div class="m-steps" style="margin-top:12px">
        <div class="m-step done"><span class="k">1</span><div><b>The simulator is running</b><div class="m-small">It started by itself. This step is done.</div></div></div>
        <div class="m-step now"><span class="k">2</span><div><b>Point your tool at it</b><div class="m-small">Add it to your tool the way you would add a real vCenter, using the address and username under "Connect a tool" below and ${con.password === 'setup' ? 'the password you chose for that account in step 2 of setup (not the console password)' : con.password === 'any' ? 'any password' : 'the password for that account'}.</div></div></div>
        <div class="m-step"><span class="k">3</span><div><b>See what it asks for</b><div class="m-small">Waiting for the first request. When it arrives, this page and ${link('Requests', '/requests')} show what your tool asked for and whether it got an answer.</div></div></div></div>` : ''}</section>`;

    const edit = '/users/edit/' + encodeURIComponent(con.username || '');
    const pwLine = con.password === 'any' ? 'Any password is accepted'
      : con.password === 'inuse' ? html`This account is in use by the tool at ${con.inUseBy}, so do not change its password. For another tool, ${link('add a user', '/users/add')}.`
        : con.password === 'console' ? html`The one you set for this user on the Users screen. ${link('Forgotten it? Set a new one', edit)}`
          : con.password === 'setup' ? html`The password you chose for this account in step 2 of setup. It is not the console password. ${link('Forgotten it? Set a new one', edit)}`
            : html`Set by whoever installed the simulator. ${link('Do not know it? Set a new one', edit)}`;
    const certLine = con.cert === 'own' && !con.issuer ? 'Your own, signed by itself, so your tool will ask whether to trust it. Say yes.'
      : con.cert === 'own' ? `Your own, issued by ${con.issuer}. A tool that trusts that authority connects without asking. Any other tool will ask whether to trust it.`
      : con.cert === 'managed' ? 'Supplied by whoever installed the simulator. A tool that does not know it will ask whether to trust it.'
        : 'Made by the simulator, so your tool will ask whether to trust it. Say yes.';
    const connect = card('Connect a tool', html`<dl class="m-kv">
        <dt>Address</dt><dd><code>${h.address}</code> ${btn('Copy', 'copy', 'lat-btn--ghost', { text: h.address, what: 'The address' })}</dd>
        <dt>Username</dt><dd>${con.anyLogin ? 'Any username is accepted' : html`<code>${con.username}</code> ${btn('Copy', 'copy', 'lat-btn--ghost', { text: con.username, what: 'The username' })}`}</dd>
        <dt>Password</dt><dd>${pwLine}</dd>
        <dt>Certificate</dt><dd>${certLine} ${link('Details', '/security')}</dd></dl>
      <div class="m-row" style="margin-top:10px">${btn(F.checking ? 'Checking…' : 'Check the address works', 'checkAddr', '', { disabled: !!F.checking })}${F.checked ? html`<span class="m-small" role="status">${dot(F.checked.ok ? 'ok' : 'warn')} ${F.checked.text}</span>` : html`<span class="m-small">Any other user on the ${link('Users', '/users')} screen works too.</span>`}</div>`);

    const who = card(live.length || fresh ? 'Who is connected' : 'Who was connected', h.tools.length ? html`<ul class="m-list">${h.tools.map(toolItem)}</ul>`
      : html`<div class="m-empty"><h3>Nothing has connected yet</h3><p>When a tool signs in (a management pack, a script, anything that talks to vCenter), it appears here with what it asked for.</p></div>`);

    const shown = F.allAtt ? a : a.slice(0, 3);
    const attBtn = (x) => (x.href ? lbtn(x.label, x.href) : btn(x.label, x.act, '', { id: x.id }));
    const att = card('Needs your attention', a.length ? html`<ul class="m-list">${shown.map((x) => html`<li><span class="m-dot ${x.k}"></span><div class="grow"><div class="t">${rich(x.t)}</div><div class="d">${rich(x.d)}</div></div>${attBtn(x)}</li>`)}</ul>
        ${a.length > 3 && !F.allAtt ? html`<div class="m-row" style="margin-top:6px">${btn(`Show the other ${a.length - 3}`, 'allAtt')}<span class="m-small">The most serious are listed first.</span></div>` : ''}`
      : html`<div class="m-empty"><span class="m-dot ok"></span><h3>Nothing needs attention</h3><p>${fresh ? 'The simulator is healthy.' : 'Everything asked for in the last hour was answered.'}</p></div>`, a.length ? plural(a.length, 'thing') : '');

    const contains = card('What it contains', html`<div class="m-stats">
        <a class="m-stat" href="#/clusters"><div class="n">${num(c.clusters)}</div><div class="l">${c.clusters === 1 ? 'Cluster' : 'Clusters'}</div></a>
        <div class="m-stat"><div class="n">${num(c.hosts)}</div><div class="l">Hosts</div></div>
        <div class="m-stat"><div class="n">${num(c.vms)}</div><div class="l">VMs</div></div>
        <a class="m-stat" href="#/storage"><div class="n">${num(c.datastores)}</div><div class="l">Datastores</div></a></div>
      <p class="m-small" style="margin-top:10px">vCenter ${h.vcenter} in ${plural(c.datacenters, 'datacenter')}. Hosts and VMs are listed inside each cluster. One full collection by a tool is about ${num(h.estimateAll.requests)} requests. <a href="#" data-act="estimate">How that is worked out</a></p>`);

    const s = h.scheduled;
    const sched = s.length ? html`<ul class="m-list">${s.map((x) => html`<li><span class="m-dot info"></span><div class="grow"><div class="t">${x.text}</div><div class="d">Scheduled for ${timeText(x.at)} today. Nothing has changed yet.</div></div>${btn('Do it now', 'schedNow', '', { id: x.id })}${btn('Cancel it', 'schedCancel', '', { id: x.id })}</li>`)}</ul>`
      : html`<p class="m-muted">Nothing is scheduled. To schedule a change, start one that has a "When should this happen?" box (adding or changing clusters, storage, versions, users, the certificate, slowness and faults) and choose "Later today". Removing a cluster, a datastore or a user, pausing and "Accept anyone" always happen when you press the button.</p>`;
    const recent = html`<ul class="m-list">${h.recent.map((x) => html`<li><span class="m-small" style="width:64px;flex:none">${timeText(x.at)}</span><div class="grow t">${rich(x.text)}</div></li>`)}</ul>`;
    const top = fresh ? html`${connect}${who}${att}` : html`${att}${who}${connect}`;
    return html`<div class="m-page">${hero}<div class="m-grid m-g3">${top}</div>
      <div class="m-grid m-g3">${contains}${card('Scheduled changes', sched, s.length ? plural(s.length, 'change') : '')}${card('Recent changes', recent, html`Local time · ${link(`See all ${h.historyCount}`, '/home/history')}`)}</div></div>`;
  },
  actions: {
    allAtt(ctx) { ctx.F.allAtt = true; ctx.rerender(); },
    async checkAddr(ctx) {
      ctx.F.checking = true; ctx.rerender();
      try { ctx.F.checked = await api('/check-address', { method: 'POST' }); }
      catch (e) { ctx.F.checked = { ok: false, text: e.message }; }
      ctx.F.checking = false; ctx.rerender();
    },
    estimate(ctx) {
      const h = ctx.home;
      ctx.dialog({ title: 'How big is one collection?', body: html`<p>Each time a tool collects, it asks the simulator for everything it wants to know. This is an estimate worked out from the number of clusters and hosts. It is not a measurement.</p>
        <dl class="m-kv"><dt>If the tool collects everything vSAN can report</dt><dd>about <b>${num(h.estimateAll.requests)} requests</b> and ${h.estimateAll.size}</dd><dt>If it collects only what its dashboards show</dt><dd>about <b>${num(h.estimateDash.requests)} requests</b> and ${h.estimateDash.size}</dd></dl>
        <p style="margin-top:10px">The number of requests grows with the number of clusters. The amount of data grows with the number of hosts.</p>` });
    },
  },
};

const history = {
  title: 'All changes', tab: 'home',
  load: () => api('/history'),
  render(ctx, rows) {
    const shown = pageOf(ctx, 'hist', rows, 12);
    return html`<div class="m-page"><div class="m-crumb">${link('Home', '/home')} ${raw('&rsaquo;')} All changes</div><div class="m-head"><div><h1>All changes</h1><p class="sub">Everything that has been changed in the simulator, newest first. Times are your computer's local time.</p></div></div>
      ${card('', html`<ul class="m-list">${shown.map((x) => html`<li><span class="m-small" style="width:110px;flex:none">${timeText(x.at)}</span><div class="grow t">${rich(x.text)}</div></li>`)}</ul>${pager(ctx, 'hist', rows.length, 12)}`)}</div>`;
  },
};

export default { screen: (p) => (p[1] === 'history' ? history : main) };
export { badge };
