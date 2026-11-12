// Requests: what connected tools ask for and whether they are answered;
// request capture; and, one level down for engineers, every single request
// and the list of what the simulator can answer.
import { api, html, raw, badge, btn, link, lbtn, card, table, fit, plural, num, rich, ago, timeSeconds, today, pager, pageOf } from './lib.js';

const SHOW = [['all', 'Everything'], ['unknown', 'Not answered'], ['fault', 'Failing on purpose'], ['denied', 'Sign-in refused'], ['ok', 'Answered']];
const inShow = (x, show) => show === 'all' || x.st === show;

function save(href, name) {
  const a = document.createElement('a');
  a.href = href; a.download = name;
  document.body.appendChild(a); a.click(); a.remove();
}
function download(ctx, what, n) {
  const name = `vcsim-${what}-requests-${today()}.txt`;
  save(`/api/capture/${what}`, name);
  ctx.closeDialog();
  ctx.say('ok', `**Downloaded.** ${num(n)} ${what === 'unanswered' ? 'unanswered requests' : 'requests'} saved by your browser as ${name}, usually in your Downloads folder.`);
  ctx.rerender();
}

function main(q) {
  const show = SHOW.some(([k]) => k === q.show) ? q.show : 'all';
  return {
    title: 'Requests', tab: 'requests', poll: true,
    load: () => api('/requests'),
    render(ctx, d) {
      const v = d.verdict, cap = d.capture, cnt = d.counts, u = cnt.unknown, address = ctx.home ? ctx.home.address : '';
      const tone = v.tone === 'none' ? 'info' : v.tone;
      const verdict = html`<div class="lat-banner lat-banner--${tone} m-verdict"><div><div class="big">${v.big}</div>${v.tone === 'none' ? html`Point a tool at <code>${address}</code> and what it asks for will appear here within a few seconds.` : v.line}</div>
        ${v.tone === 'bad' && show !== 'unknown' ? html`<div class="m-row">${lbtn('Show the ones not answered', '/requests?show=unknown')}</div>` : ''}</div>`;
      const per = fit(540, 43), sorted = d.asks.filter((x) => inShow(x, show));
      const rows = pageOf(ctx, 'req', sorted, per).map((x) => ({ t: x.t, who: x.who, n: num(x.n),
        res: html`${badge(x.tone, x.label)}${x.st === 'fault' && x.cond ? html` ${btn('Turn the fault off', 'condOff', '', { id: x.cond })}` : ''}${x.short ? html` <span class="m-small">${rich(x.short)}</span>` : ''}`,
        act: btn('Details', 'askDetail', 'lat-btn--ghost', { id: x.id }) }));
      const pills = html`<div class="m-pills" role="group" aria-label="Show">${SHOW.filter(([k]) => k === 'all' || k === 'unknown' || cnt[k]).map(([k, l]) => html`<a class="lat-pill ${show === k ? 'is-on' : ''}" href="#/requests${k === 'all' ? '' : '?show=' + k}">${l} <b>${cnt[k]}</b></a>`)}<span class="m-small" style="align-self:center">kinds of request</span></div>`;
      const sw = (k, on, label) => html`<button type="button" class="m-switch ${on ? 'on' : ''}" data-act="capture" data-k="${k}" role="switch" aria-checked="${on}"><i></i><span>${label}: <b>${on ? 'On' : 'Off'}</b></span></button>`;
      const keptLine = cap.kept ? html`<b>${plural(cap.kept, 'unanswered request')} of ${plural(cap.kinds, 'kind')} ${cap.kept === 1 ? 'has' : 'have'} been captured</b> and can be downloaded.` : 'No unanswered requests have been captured.';
      const rec = card('Request capture', cap.available ? html`<div class="m-stack"><p class="m-small">Capturing means keeping a copy of what tools ask for. There are three kinds.</p>
          <div>${sw('unanswered', cap.unanswered, 'Capture requests that got no answer')}<div class="m-small">${cap.unanswered ? 'This one is on unless you turn it off, so there is nothing to do.' : 'Off: new unanswered requests are not being captured. What was captured earlier is kept until you delete it.'} ${keptLine}</div></div>
          <div>${sw('full', cap.full, 'Capture every request in full')}<div class="m-small">${cap.full ? html`<b>Capturing since ${cap.fullSince ? timeSeconds(cap.fullSince) : 'the simulator started'}: ${plural(cap.fullKept, 'request')} so far.</b> ${cap.fullKept ? 'Download them below, then turn this off.' : 'Repeat the action in your tool. A download button appears here when the first request arrives.'}` : 'For reproducing a problem: turn it on, repeat the action in your tool, download, turn it off.'} It can include names of clusters, hosts and VMs, never passwords.</div></div>
          <div>${sw('debug', cap.debug, 'Record what tools ask about licensing')}<div class="m-small">${cap.debug ? 'Keeps a record of what tools ask about licenses, so a tool that will not connect can be diagnosed. Passwords and sign-in tokens are left out. The record is in the simulator\'s own log, which whoever installed it can open, not in the downloads here.' : 'Off: what tools ask about licensing is not recorded.'}</div></div>
          ${cap.error ? html`<div class="lat-banner lat-banner--bad" role="alert">${cap.error}</div>` : ''}
          <div class="m-row">${cap.kept ? btn('Download unanswered requests', 'download', u ? 'lat-btn--primary' : '', { what: 'unanswered', n: cap.kept }) : ''}${cap.full && cap.fullKept ? btn('Download every captured request', 'download', '', { what: 'all', n: cap.fullKept }) : ''}${cap.kept || cap.fullKept ? btn('Delete what was captured…', 'clearKept') : ''}</div>
          <div class="m-small">The unanswered download is what the simulator's maintainers need to add the missing answers. <a href="#" data-act="report">How to send it to them</a></div></div>`
        : html`<p class="m-muted">Request capture is not available in this installation.</p>`);
      const eng = html`<p class="m-small" style="margin-top:10px">More detail, for engineers: ${link('every request, one at a time', '/requests/log')} · ${link('what the simulator can answer', '/requests/supported')}</p>`;
      const cols = [{ h: 'What was asked for', k: 't', cls: 'w-main' }, ...(d.multiTool ? [{ h: 'Asked by', k: 'who' }] : []), { h: 'Times', k: 'n', cls: 'num' }, { h: 'What happened', k: 'res' }, { h: '', k: 'act', cls: 'act' }];
      const list = card('What was asked for in the last hour', d.asks.length ? html`${pills}<div style="margin-top:10px">${rows.length ? html`${table(cols, rows)}${pager(ctx, 'req', sorted.length, per)}`
          : html`<div class="m-empty"><h3>Nothing in this group</h3><p>${show === 'unknown' ? 'Good news: every request got an answer.' : 'No requests match.'}</p>${lbtn('Show everything', '/requests')}</div>`}${eng}</div>`
        : v.tone === 'none' ? html`<div class="m-empty"><h3>No requests yet</h3><p>This fills in as soon as a tool connects. The address and username to give it are on ${link('Home', '/home')}.</p></div>${eng}`
          : html`<div class="m-empty"><h3>Nothing was asked for in the last hour</h3><p>This fills in again when a tool next asks for something.</p></div>${eng}`);
      return html`<div class="m-page"><div class="m-head"><div><h1>Requests</h1><p class="sub">What the tools connected to the simulator (management packs, scripts, anything that signs in) are asking for, and whether they are getting answers.</p></div></div>
        ${verdict}<div class="m-req"><div id="reqlist">${list}</div>${rec}</div></div>`;
    },
    actions: {
      askDetail(ctx, el) {
        const x = ctx.data.asks.find((a) => String(a.id) === el.dataset.id), cap = ctx.data.capture;
        if (!x) return;
        ctx.dialog({ title: x.t, body: html`<dl class="m-kv"><dt>What happened</dt><dd>${badge(x.tone, x.label)} ${rich(x.long)}</dd><dt>Asked by</dt><dd>${x.who}${x.user && x.who.startsWith('the tool') ? `, signed in as ${x.user}` : ''}</dd><dt>Times in the last hour</dt><dd>${num(x.n)}</dd><dt>Last asked</dt><dd>${ago(x.last)}</dd><dt>Name tools use for it</dt><dd><code>${x.tech}</code></dd></dl>
          ${x.st === 'unknown' ? html`<div class="lat-banner lat-banner--info" style="margin-top:12px"><b>What to do:</b> download the unanswered requests and send the file to the simulator's maintainers, so the answer can be added.${cap.kept ? html`<div class="m-row" style="margin-top:8px">${btn('Download unanswered requests', 'download', 'lat-btn--primary', { what: 'unanswered', n: cap.kept })}</div>` : ''}</div>` : ''}` });
      },
      report(ctx) {
        ctx.dialog({ title: 'Sending unanswered requests to the maintainers', body: html`<p>1. Press "Download unanswered requests". It saves one small text file.</p><p>2. Open a new issue on the simulator's project page and attach the file. Say which tool you were testing.</p>
          <div class="m-row"><a class="lat-btn lat-btn--primary" href="${ctx.data.reportUrl}" target="_blank" rel="noopener noreferrer">Open the project's "new issue" page</a></div><p class="m-small" style="margin-top:10px">It opens in a new tab. The file lists the requests and the names of clusters, hosts and VMs in them. It never holds passwords.</p>` });
      },
      download(ctx, el) { download(ctx, el.dataset.what, Number(el.dataset.n)); },
      async capture(ctx, el) {
        const k = el.dataset.k, on = !ctx.data.capture[k], n = ctx.data.capture.fullKept;
        if (k === 'full' && !on && n) {
          const yes = await ctx.confirm({ title: 'Turn off full capture?', danger: true, ok: 'Turn off and delete', cancel: 'Keep capturing', body: html`<p>Turning this off deletes the ${plural(n, 'request')} captured so far. They cannot be brought back.</p>
            <p><a href="/api/capture/all" download="vcsim-all-requests-${today()}.txt">Download them first</a>, then come back and turn this off.</p><p>The simulator and your tools are not affected.</p>` });
          if (!yes) return;
        }
        await ctx.act(() => api('/capture', { method: 'PUT', body: { kind: k, on } }));
      },
      async clearKept(ctx) {
        const yes = await ctx.confirm({ title: 'Delete what was captured?', danger: true, ok: 'Delete it', body: html`<p>The captured unanswered and full requests are deleted. Download them first if you still need them.</p><p>The simulator and your tools are not affected, and capture carries on from empty.</p>` });
        if (yes) await ctx.act(() => api('/capture', { method: 'DELETE' }));
      },
    },
  };
}

function log(q) {
  const show = SHOW.some(([k]) => k === q.show) ? q.show : 'all';
  return {
    title: 'Every request', tab: 'requests', poll: true,
    load: () => api('/requests/log'),
    render(ctx, all) {
      const rows = all.filter((x) => inShow(x, show)), per = fit(372, 43);
      const cnt = (k) => all.filter((x) => inShow(x, k)).length;
      const shown = pageOf(ctx, 'log', rows, per).map((x) => ({ time: timeSeconds(x.time), who: x.who, t: x.t, techc: html`<code>${x.tech}</code>`, cl: x.cluster || '', res: badge(x.tone, x.label), msc: x.ms < 0.05 ? 'under 0.1 ms' : `${x.ms < 10 ? x.ms.toFixed(1) : Math.round(x.ms)} ms`, act: btn('See what was sent', 'reqBody', 'lat-btn--ghost', { seq: x.seq }) }));
      return html`<div class="m-page"><div class="m-crumb">${link('Requests', '/requests')} ${raw('&rsaquo;')} Every request</div>
        <div class="m-head"><div><h1>Every request, one at a time</h1><p class="sub">For engineers. Each line is one single request, newest first. ${all.length ? `These are the latest ${num(all.length)}; the counts on the Requests screen cover the whole last hour, so they can be larger.` : ''}</p></div>
          <div class="acts">${all.length ? btn('Download this list as a spreadsheet', 'sheet') : ''}</div></div>
        ${card('', all.length ? html`<div class="m-pills" style="margin-bottom:10px">${SHOW.filter(([k]) => k === 'all' || cnt(k)).map(([k, l]) => html`<a class="lat-pill ${show === k ? 'is-on' : ''}" href="#/requests/log${k === 'all' ? '' : '?show=' + k}">${k === 'all' ? 'All requests' : l} <b>${cnt(k)}</b></a>`)}<span class="m-small" style="align-self:center">of the latest ${num(all.length)} requests</span></div>
            ${table([{ h: 'Time', k: 'time', cls: 'nw' }, { h: 'Asked by', k: 'who', cls: 'nw' }, { h: 'What was asked for', k: 't' }, { h: 'Name tools use', k: 'techc' }, { h: 'Cluster', k: 'cl', cls: 'nw' }, { h: 'What happened', k: 'res', cls: 'nw' }, { h: 'Took', k: 'msc', cls: 'num nw' }, { h: '', k: 'act', cls: 'act' }], shown)}${pager(ctx, 'log', rows.length, per)}`
          : html`<div class="m-empty"><h3>No requests yet</h3><p>This fills in as soon as a tool connects.</p></div>`)}</div>`;
    },
    actions: {
      sheet(ctx) {
        const name = `vcsim-requests-${today()}.csv`;
        save('/api/requests/log.csv', name);
        ctx.say('ok', `**Downloaded.** The list was saved by your browser as ${name}, usually in your Downloads folder. It opens in any spreadsheet program.`);
        ctx.rerender();
      },
      async reqBody(ctx, el) {
        let sent;
        try { sent = await api('/requests/call/' + el.dataset.seq); }
        catch (e) { ctx.dialog({ title: 'What the tool sent', body: html`<p>${e.message}</p>` }); return; }
        ctx.dialog({ title: 'What the tool sent', wide: true, body: html`<p>The exact text of this request. Passwords and sign-in tokens are never kept.</p><pre class="lat-code">${sent.line}${sent.body ? '\n\n' + sent.body : ''}</pre>${sent.note ? html`<p class="m-small" style="margin-top:8px">${sent.note}</p>` : ''}` });
      },
    },
  };
}

const supported = {
  title: 'What the simulator can answer', tab: 'requests',
  load: () => api('/requests/supported'),
  render(ctx, d) {
    const st = { full: badge('ok', 'Complete'), part: badge('warn', 'Partly'), no: badge('bad', 'Not yet') };
    return html`<div class="m-page"><div class="m-crumb">${link('Requests', '/requests')} ${raw('&rsaquo;')} What the simulator can answer</div>
      <div class="m-head"><div><h1>What the simulator can answer</h1><p class="sub">The simulator imitates the parts of vCenter that monitoring tools use. "Checked" means its answers were compared with a real vCenter 9.1.1${d.vcenter !== '9.1.1' ? `, whatever version it is set to report (now ${d.vcenter})` : ''}.</p></div></div>
      ${card('', table([{ h: 'Part of vCenter', k: 'a' }, { h: 'How complete', k: 'b' }, { h: 'Notes', k: 'c' }], d.rows.map((r) => ({ a: r.part, b: st[r.state], c: r.notes }))))}</div>`;
  },
};

export default { screen: (p, q) => (p[1] === 'log' ? log(q) : p[1] === 'supported' ? supported : main(q)) };
