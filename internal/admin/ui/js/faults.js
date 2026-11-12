// Slowness and faults: make the simulator misbehave on purpose, and stop it.
import { api, html, badge, btn, lbtn, card, plural, timeText } from './lib.js';
import { taskScreen } from './task.js';

const main = {
  title: 'Slowness and faults', tab: 'faults', poll: true,
  load: () => api('/faults'),
  render(ctx, d) {
    const when = (c) => (c.starts ? `Not on yet. It starts at ${timeText(c.starts)}.` : `${c.since ? `On since ${timeText(c.since)}. ` : ''}${c.until ? `Until ${timeText(c.until)}, then it turns itself off.` : 'Until you turn it off.'}`);
    const on = d.on.length ? html`<ul class="m-list">${d.on.map((c) => html`<li><span class="m-dot warn"></span><div class="grow"><div class="t">${c.text}</div><div class="d">${when(c)}</div></div>${btn('Turn it off', 'condOff', 'lat-btn--primary', { id: c.id })}</li>`)}</ul>${d.on.length > 1 ? html`<div class="m-row" style="margin-top:8px">${btn('Turn everything off', 'condOff', '', { id: 0 })}</div>` : ''}`
      : html`<div class="m-row"><span class="m-dot ok"></span><span class="m-lead">Nothing. Every request is answered normally and at full speed.</span></div>`;
    const tile = (r) => html`<div class="m-tile"><h3>${r.name} ${r.on ? badge('warn', 'On now') : ''}</h3><p>${r.what}</p><p>${r.tool}</p>${r.on ? btn('Turn it off', 'condOff', '', { id: r.on }) : lbtn('Set up…', '/faults/add?preset=' + r.key)}</div>`;
    return html`<div class="m-page"><div class="m-head"><div><h1>Slowness and faults</h1><p class="sub">Make the simulator slow, or make it fail, to see how your tool copes. Nothing here damages anything: turn it off and everything is normal again.</p></div></div>
      ${card('Switched on now', on, d.on.length ? plural(d.on.length, 'thing') : '')}
      ${card('Switch something on', html`<div class="m-grid m-g3">${d.ready.map(tile)}
        <div class="m-tile"><h3>Something else</h3><p>Choose exactly what goes wrong, for which requests, on which clusters and for how long.</p>${lbtn('Set up…', '/faults/add?preset=own')}</div></div>`)}</div>`;
  },
};

export default { screen: (p, q) => (p[1] === 'add' ? taskScreen('fault', q, 'faults', 'Slowness and faults') : main) };
