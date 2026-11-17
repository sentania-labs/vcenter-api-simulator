// Activity: the environment changes by itself. One dial for each thing that
// happens, for the whole vCenter or for one cluster; what has happened; and
// one button that stops or starts all of it.
import { api, html, raw, badge, btn, link, lbtn, card, plural, num, rich, timeText, dateText } from './lib.js';

const enc = encodeURIComponent;
const UNITS = { minutes: 'minutes', hours: 'hours', days: 'days', perDay: 'a day', percent: '%' };

// ask does something that may first need a "this is what will happen" box:
// the simulator answers either with the box or with what it did.
async function ask(ctx, path, body) {
  try {
    let r = await api(path, { body });
    if (r.confirm) {
      const yes = await ctx.confirm({ title: r.confirm.title, ok: r.confirm.ok, body: html`${r.confirm.body.map((p) => html`<p>${rich(p)}</p>`)}` });
      if (!yes) { await ctx.reload(); return; }
      r = await api(path, { body: { ...body, confirmed: true } });
    }
    if (r.done) ctx.say(r.tone || 'ok', r.done);
  } catch (e) { ctx.say('bad', `**That did not work.** ${e.message}`); }
  await ctx.reload();
}

function dialRow(d, r) {
  const i = Math.max(0, r.levels.findIndex((l) => l.v === r.level));
  const follows = r.follows;
  const label = r.exact ? (follows ? "The vCenter's number" : 'Your own number') : r.levels[i].label;
  const state = follows ? badge('', 'Follows the vCenter') : d.scope ? badge('accent', 'Set for this cluster') : '';
  return html`<li class="a-dial ${r.held ? 'is-held' : ''}">
    <div class="a-dial-h"><h3>${r.name}</h3><span class="a-level" id="l-${r.key}">${label}</span>${state}</div>
    <p class="a-text" id="t-${r.key}">${r.text}</p>
    <p class="m-small">${r.what}</p>
    ${r.note ? html`<p class="a-note">${rich(r.note)} ${r.held ? btn('Switch it on again', 'actResume', '', { b: r.key }) : ''}</p>` : ''}
    <div class="a-dial-c">
      <input type="range" class="a-range" id="r-${r.key}" min="0" max="${r.levels.length - 1}" step="1" value="${i}" data-dial="${r.key}" aria-label="${r.name}: slower to busier" aria-valuetext="${label}" list="ticks-${r.key}">
      <datalist id="ticks-${r.key}">${r.levels.map((l, n) => html`<option value="${n}" label="${l.label}"></option>`)}</datalist>
      <span class="a-ends"><span>${r.levels[0].label}</span><span>${r.levels[r.levels.length - 1].label}</span></span>
    </div>
    <div class="a-dial-b">${r.units && r.units.length ? btn('Type an exact number…', 'actExact', '', { b: r.key }) : ''}${d.scope && r.own ? btn('Follow the vCenter again', 'actFollow', '', { b: r.key }) : ''}${r.doNow ? btn('Do it now…', 'actNow', '', { b: r.key }) : ''}</div></li>`;
}

function verdict(d) {
  if (!d.setUp) return html`<section class="lat-card m-verdict"><div><div class="big">Nothing happens by itself.</div><div class="m-muted">The environment is still: ${plural(d.hosts, 'host')} and ${plural(d.vms, 'VM')}, and nothing changes unless you change it. A monitoring tool sees no events. The dials below say what would happen; set them before or after you start.</div></div><div class="m-row">${btn('Start activity…', 'actStart', 'lat-btn--primary')}</div></section>`;
  if (d.paused) return html`<section class="lat-card m-verdict"><div><div class="big">Activity is waiting.</div><div class="m-muted">The whole simulator is paused, so nothing happens by itself either. It carries on when you resume, and nothing is caught up.</div></div></section>`;
  if (!d.running) return html`<section class="lat-card m-verdict"><div><div class="big"><span class="m-dot warn" style="display:inline-block;margin-right:8px"></span>Activity is stopped.</div><div class="m-muted">Nothing happens by itself until you start it again. The environment is still, with ${plural(d.vms, 'VM')}. The dials below are kept as you set them.</div></div><div class="m-row">${btn('Start activity again…', 'actStart', 'lat-btn--primary')}</div></section>`;
  return html`<section class="lat-card m-verdict"><div><div class="big"><span class="m-dot ok" style="display:inline-block;margin-right:8px"></span>Activity is running.</div>
    <div class="m-muted">${d.last ? html`Last: ${rich(d.last)} ` : 'Nothing has happened yet. '}${d.now}</div></div><div class="m-row">${btn('Stop all activity…', 'actStop', 'lat-btn--primary')}</div></section>`;
}

function happened(d) {
  const line = (x) => html`<li><span class="when">${timeText(x.at)}</span><span class="what ${x.bad ? 'bad' : ''}">${x.text}</span></li>`;
  const body = !d.lines.length ? html`<p class="m-muted">Nothing yet. What happens is listed here as it happens, newest first, so you can see it working without opening another tool.</p>`
    : html`<p style="margin-bottom:6px"><b>In the last hour:</b> ${d.hour ? html`${d.hour}.${d.hourYou ? ` ${plural(d.hourYou, 'line')} came from a button you pressed.` : ''}` : 'nothing happened.'}</p>
      ${d.today ? html`<p class="a-today"><b>Since midnight:</b> ${d.today}.</p>` : ''}
      <ul class="a-log">${d.lines.map(line)}</ul>
      <div class="m-row" style="margin-top:8px">${lbtn('See everything from the last hour', '/activity/log')}${link('or from today', '/activity/log?range=today')}</div>`;
  return card('What has happened', body, d.lines.length ? 'Newest first · your computer\'s local time' : '');
}

const main = (q) => ({
  title: 'Activity', tab: 'activity', poll: true,
  load: () => api('/activity' + (q.cluster ? '?cluster=' + enc(q.cluster) : '')),
  render(ctx, d) {
    const scope = d.clusters.find((c) => c.key === d.scope);
    const picker = html`<label class="a-scope"><span>These dials are for</span> <select class="lat-select" id="a-scope" data-scope aria-label="The whole vCenter or one cluster">
      <option value="" ${d.scope ? '' : raw('selected')}>the whole vCenter (${plural(d.hosts, 'host')}, ${plural(d.vms, 'VM')})</option>
      ${d.clusters.map((c) => html`<option value="${c.key}" ${c.key === d.scope ? raw('selected') : ''}>${c.label} (${plural(c.hosts, 'host')}, ${plural(c.vms, 'VM')})${c.own ? ` · ${c.own} of its own` : ''}</option>`)}</select></label>`;
    const intro = scope ? html`${scope.label} follows the vCenter's dials until you move one here. A dial you move applies to this cluster only, and "Follow the vCenter again" puts it back.`
      : html`Each dial is one thing that happens. The middle is the baseline for an environment of this size; left is slower, right is busier. A cluster follows these until you give it its own.`;
    return html`<div class="m-page"><div class="m-head"><div><h1>Activity</h1><p class="sub">Make the environment behave like one that is in use, so a monitoring tool has something to see: VMs move, come and go and are powered off and on, hosts go into maintenance mode, datastores fill, and the load rises and falls. How much happens follows the size of the environment. Stop it and the environment holds still; VMs that were added or removed stay that way.</p></div></div>
      ${verdict(d)}
      <div class="m-grid m-g32 a-main"><div class="m-stack">
        ${card('How much happens', html`<div class="a-scope-row">${picker}</div><p class="m-small" style="margin:6px 0 10px">${intro}</p>
          <ul class="a-dials">${d.rows.map((r) => dialRow(d, r))}</ul>`)}
      </div><div class="m-stack">${happened(d)}
        ${card('What it adds up to', html`<p style="font-size:13.5px">${d.day}</p>
          <p style="font-size:13.5px;margin-top:8px">The number of VMs stays between <b>${num(d.minVMs)}</b> and <b>${num(d.maxVMs)}</b>. ${d.vms === d.built ? html`It is ${num(d.vms)} now.` : html`It is ${num(d.vms)} now.`} At most a quarter of each cluster's hosts are in maintenance mode at once, so every VM always has somewhere to run.</p>
          <div class="m-row" style="margin-top:8px">${btn('Change the limits…', 'actLimits')}</div>
          <p class="m-small" style="margin-top:8px">If the simulator restarts, everything carries on as it was: the dials, where each VM is, which are off, which hosts are in maintenance mode and how full the datastores are. Nothing happens while it is off, and nothing is caught up afterwards.</p>`)}
      </div></div></div>`;
  },
  // dragging a dial says what that position would mean, before it is let go
  input(ctx, el) {
    if (el.dataset.dial) {
      const r = ctx.data.rows.find((x) => x.key === el.dataset.dial), l = r.levels[Number(el.value)];
      const t = document.getElementById('t-' + r.key), lab = document.getElementById('l-' + r.key);
      if (t) t.textContent = l.text;
      if (lab) lab.textContent = l.label;
      el.setAttribute('aria-valuetext', l.label);
      return;
    }
    if (el.dataset.x) ctx.F[el.dataset.x] = el.value;
  },
  change(ctx, el) {
    if (el.dataset.scope !== undefined) { ctx.go('/activity' + (el.value ? '?cluster=' + enc(el.value) : '')); return; }
    if (el.dataset.x) { ctx.F[el.dataset.x] = el.value; return; }
    if (!el.dataset.dial) return;
    const r = ctx.data.rows.find((x) => x.key === el.dataset.dial), l = r.levels[Number(el.value)];
    if (!r.exact && !r.follows && l.v === r.level) return;
    ask(ctx, '/activity/dial', { cluster: ctx.data.scope, behaviour: r.key, level: l.v });
  },
  actions: {
    actStart(ctx) { ask(ctx, '/activity/start', {}); },
    actStop(ctx) { ask(ctx, '/activity/stop', {}); },
    actFollow(ctx, el) { ask(ctx, '/activity/dial', { cluster: ctx.data.scope, behaviour: el.dataset.b, follow: true }); },
    actNow(ctx, el) { ask(ctx, '/activity/do-now', { cluster: ctx.data.scope, behaviour: el.dataset.b }); },
    async actResume(ctx, el) { await ctx.act(() => api('/activity/resume', { body: { behaviour: el.dataset.b } })); },
    async actExact(ctx, el) {
      const d = ctx.data, r = d.rows.find((x) => x.key === el.dataset.b), u = r.units;
      ctx.F.n = r.exact ? String(r.exact.n) : '';
      ctx.F.unit = r.exact ? r.exact.unit : u[0];
      const where = d.scope ? d.clusters.find((c) => c.key === d.scope).label : 'the whole vCenter';
      const lead = { perDay: 'About', percent: '', minutes: 'One every', hours: 'One every', days: 'One every' }[u[0]];
      const yes = await ctx.confirm({ title: `${r.name}: an exact number`, ok: 'Use this number', body: html`<p>${r.what}</p>
        <div class="m-field"><label for="f-exact">For ${where}</label><div class="m-inline">${lead} <input class="lat-input n" id="f-exact" data-x="n" inputmode="decimal" value="${ctx.F.n}" style="width:96px">
          ${u.length > 1 ? html`<select class="lat-select" data-x="unit" aria-label="Minutes, hours or days">${u.map((k) => html`<option value="${k}" ${k === ctx.F.unit ? raw('selected') : ''}>${UNITS[k]}</option>`)}</select>` : html`<span>${u[0] === 'percent' ? (r.key === 'maintenance' ? '% of the hosts' : '% of capacity a day') : 'vMotions a day'}</span>`}</div>
          <span class="lat-hint">An exact number stays as you typed it, whatever the size of the environment. Moving the dial afterwards goes back to a level.</span></div>` });
      if (!yes) return;
      const n = Number(String(ctx.F.n).trim());
      if (!String(ctx.F.n).trim() || Number.isNaN(n)) { ctx.say('bad', '**Not changed.** Type a number.'); ctx.rerender(); return; }
      ask(ctx, '/activity/dial', { cluster: d.scope, behaviour: r.key, n, unit: ctx.F.unit });
    },
    async actLimits(ctx) {
      const d = ctx.data;
      ctx.F.lo = String(d.minVMs); ctx.F.hi = String(d.maxVMs);
      const yes = await ctx.confirm({ title: 'Change the limits', ok: 'Save limits', body: html`<p>So the environment never grows or shrinks without end. There are ${plural(d.vms, 'VM')} now. At a limit, adding or removing VMs waits; nothing else stops.</p>
        <div class="m-field"><label for="f-lo">Fewest VMs</label><div class="m-inline"><input class="lat-input n" id="f-lo" data-x="lo" inputmode="numeric" value="${ctx.F.lo}" style="width:110px"> VMs</div></div>
        <div class="m-field"><label for="f-hi">Most VMs</label><div class="m-inline"><input class="lat-input n" id="f-hi" data-x="hi" inputmode="numeric" value="${ctx.F.hi}" style="width:110px"> VMs</div></div>` });
      if (yes) ask(ctx, '/activity/limits', { min: ctx.F.lo, max: ctx.F.hi });
    },
  },
});

const RANGES = [['hour', 'Last hour'], ['today', 'Today'], ['day', 'Last 24 hours'], ['week', 'Last 7 days']];
const SHOWS = [['all', 'Everything'], ['moves', 'Load balancing'], ['power', 'Power'], ['vms', 'VMs added and removed'], ['hosts', 'Hosts'], ['disks', 'Datastores'], ['other', 'Waiting or failed']];
const LEADS = { hour: 'In the last hour', today: 'Today', day: 'In the last 24 hours', week: 'In the last 7 days' };

const log = (q) => ({
  title: 'What has happened', tab: 'activity', poll: true, back: ['Back to Activity', '/activity'],
  load: () => api(`/activity/log?range=${enc(q.range || 'hour')}&show=${enc(q.show || 'all')}&page=${enc(q.page || '1')}`),
  render(ctx, d) {
    const u = (r, s, p) => `/activity/log?range=${r}${s !== 'all' ? '&show=' + s : ''}${p > 1 ? '&page=' + p : ''}`;
    const pill = (href, label, on) => html`<a class="lat-pill ${on ? 'is-on' : ''}" href="#${href}" ${on ? raw('aria-current="true"') : ''}>${label}</a>`;
    const when = (at) => (d.range === 'hour' ? timeText(at) : `${dateText(at)}, ${timeText(at)}`);
    const what = (x) => html`<span class="${x.bad ? 'bad' : ''}">${x.text}</span>${x.details && x.details.length ? html`<details class="a-det"><summary>Show the ${plural(x.details.length, 'line')} of detail</summary><ul>${x.details.map((t) => html`<li>${t}</li>`)}</ul></details>` : ''}`;
    const lead = d.full ? 'In the time the simulator remembers' : LEADS[d.range];
    const kind = d.show !== 'all' ? `, ${SHOWS.find((s) => s[0] === d.show)[1].toLowerCase().replace('vms', 'VMs')}` : '';
    return html`<div class="m-page"><div class="m-crumb">${link('Activity', '/activity')} › What has happened</div>
      <div class="m-head"><div><h1>What has happened</h1><p class="sub">Everything that happened to hosts, VMs and datastores through Activity, newest first, whether it happened by itself or you started it. Each line is one cause together with what it set off: a host going into maintenance mode is one line, however many VMs had to move. It remembers the last 1,000 lines. Times are your computer's local time.</p></div></div>
      <section class="lat-card m-verdict"><div><div class="big">${lead}${kind}: ${d.count ? plural(d.count, 'line') + '.' : 'nothing happened.'}</div>
        <div class="m-muted">${d.count ? html`${d.sum ? `They add up to: ${d.sum}.` : 'They only say what you changed.'}${d.you ? ` ${plural(d.you, 'line')} came from a button you pressed, and ${d.you === 1 ? 'says' : 'say'} so under Because of.` : ''}` : 'Choose a longer time, or show everything.'}${d.full ? html` Only the newest 1,000 lines are remembered; they go back to ${dateText(d.first)}.` : ''}</div></div></section>
      <section class="lat-card"><div class="m-row" style="margin-bottom:8px"><span class="m-small">When</span><div class="m-pills">${RANGES.map(([k, l]) => pill(u(k, d.show, 1), l, d.range === k))}</div>
        <span class="m-small a-show">Show</span><div class="m-pills">${SHOWS.map(([k, l]) => pill(u(d.range, k, 1), l, d.show === k))}</div></div>
        ${d.lines.length ? html`<div class="lat-table-scroll"><table class="lat-table resp"><thead><tr><th class="nw">When</th><th class="w-main">What happened</th><th>Because of</th></tr></thead><tbody>
          ${d.lines.map((x) => html`<tr><td class="nw" data-l="When">${when(x.at)}</td><td class="main" style="font-weight:400">${what(x)}</td><td data-l="Because of">${x.why}${x.you && x.why !== 'You' ? ', after a button you pressed' : ''}</td></tr>`)}</tbody></table></div>
          <div class="m-pager"><span>Page ${num(d.page)} of ${num(d.pages)} · ${plural(d.count, 'line')}</span>${d.page > 1 ? lbtn('Newer', u(d.range, d.show, d.page - 1)) : ''}${d.page < d.pages ? lbtn('Older', u(d.range, d.show, d.page + 1)) : ''}</div>`
    : html`<div class="m-empty"><h3>Nothing of that kind</h3><p>Choose a longer time, or show everything.</p></div>`}</section></div>`;
  },
});

export default { screen: (p, q) => (p[1] === 'log' ? log(q) : main(q)) };
