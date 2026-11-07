// The console shell: header, tabs, pending-change banner, router, polling,
// confirm modal, toasts. Screens are modules with this shape:
//
//   export default {
//     id, title,
//     async load(ctx)            -> data       (fetch what the screen needs)
//     render(ctx, data)          -> html       (a string built with lib.html)
//     actions: { 'topo.name'(ctx, el, ev, data) }  keyed by the FULL attribute value
//                                               of data-act / data-chg / data-inp
//     keys?(ctx, ev, data)                      (keydown on the page)
//     pollMs?                                   (reload data on this interval)
//   }
import { api, ApiError, html, raw, join, esc, badge, banner, int, hms, hm, bytes } from './lib.js';
import overview from './overview.js';
import topology from './topology.js';
import telemetry from './telemetry.js';
import activity from './activity.js';
import compat from './compat.js';

const screens = { overview, topology, telemetry, activity, compat };
const TABS = [['overview', 'Overview'], ['topology', 'Topology'], ['telemetry', 'Telemetry'], ['activity', 'API Activity'], ['compat', 'Compatibility']];

export const ctx = {
  state: null, route: { name: 'overview', parts: [], query: new URLSearchParams() }, data: null, loadError: null,
  api,
  go(hash) { if (location.hash === hash) navigate(); else location.hash = hash; },
  async reload() { await loadScreen(); renderPage(); },
  async refreshState() { await loadState(); renderShell(); },
  async refresh() { await Promise.all([loadState(), loadScreen()]); renderShell(); renderPage(); },
  rerender() { renderPage(); },
  toast, confirm: confirmModal, modal: openModal, closeModal,
  async run(fn, okMsg) {
    try { const r = await fn(); if (okMsg) toast(okMsg, 'ok'); return r; }
    catch (e) { toast(e.message || String(e), 'bad'); return undefined; }
  },
};

let screen = overview;
let clockOffset = 0;
let lastStateJSON = '';
let lastDataJSON = '';
let renderedOnce = false;

// ---- data -------------------------------------------------------------------
async function loadState() {
  try {
    const st = await api('/state');
    clockOffset = Date.parse(st.clock.now) - Date.now();
    ctx.state = st;
    ctx.stateError = null;
  } catch (e) {
    ctx.stateError = e;
  }
}
async function loadScreen() {
  try { ctx.data = await screen.load(ctx); ctx.loadError = null; }
  catch (e) { ctx.loadError = e; }
}

// ---- routing ----------------------------------------------------------------
function parseHash() {
  const h = location.hash.replace(/^#\/?/, '');
  const [path, q] = h.split('?');
  const parts = path.split('/').filter(Boolean).map(decodeURIComponent);
  const alias = { compatibility: 'compat', 'api-activity': 'activity', api: 'activity' };
  if (alias[parts[0]]) parts[0] = alias[parts[0]];
  const name = screens[parts[0]] ? parts.shift() : 'overview';
  return { name, parts, query: new URLSearchParams(q || '') };
}
async function navigate() {
  ctx.route = parseHash();
  screen = screens[ctx.route.name];
  document.title = `${screen.title} · vCenter API Simulator`;
  lastDataJSON = '';
  await Promise.all([ctx.state ? null : loadState(), loadScreen()]);
  renderShell();
  renderPage(true);
  window.scrollTo(0, 0);
}
window.addEventListener('hashchange', navigate);

// ---- shell ------------------------------------------------------------------
function pausedBanner() {
  const st = ctx.state;
  if (!st || st.running) return '';
  return banner('warn', html`<b>The API is paused.</b> Connections to /sdk and /vsanHealth are refused, so a collection now fails. Topology, history and seeds are untouched.`,
    html`<button type="button" class="lat-btn act" data-act="app.pause">Resume serving…</button>`);
}
function failedBanner() {
  const st = ctx.state;
  if (!st || !st.lastError) return '';
  return banner('bad', html`<b>An apply failed.</b> ${st.lastError}`, html`<button type="button" class="lat-btn act" data-act="app.dismissError">Dismiss</button>`);
}
function pendingBanner() {
  return html`${failedBanner()}${pausedBanner()}${pendingOnly()}`;
}
function pendingOnly() {
  const st = ctx.state;
  if (!st) return '';
  const p = st.pending || {};
  // the review, generator and telemetry screens carry their own apply controls
  const onReview = (ctx.route.name === 'topology' && ['review', 'generator'].includes(ctx.route.parts[0])) || ctx.route.name === 'telemetry';
  if (p.scheduled) {
    const what = p.topologyChanges ? html`Topology v${p.target} is scheduled` : html`A telemetry change is scheduled`;
    return banner('info', html`<b>${what}</b> for ${hms(p.at)}. The API keeps serving ${p.topologyChanges ? html`v${st.topology.version}` : 'the current settings'} until then.`,
      html`<button type="button" class="lat-btn act" data-act="app.cancelScheduled">Cancel schedule</button>`);
  }
  if (!p.exists || onReview) return '';
  const bits = [];
  if (p.topologyChanges) bits.push(`${int(p.topologyChanges)} unapplied topology change${p.topologyChanges === 1 ? '' : 's'}`);
  if (p.telemetryChanges) bits.push(`${int(p.telemetryChanges)} unapplied telemetry change${p.telemetryChanges === 1 ? '' : 's'}`);
  const text = bits.join(' and ') || 'An unapplied change';
  return banner('warn', html`<b>${text}</b> · The API keeps serving topology v${st.topology.version} until you apply. Edits wait for Apply and land at a bucket boundary; history is never reset by a latency or fault edit.`,
    html`<button type="button" class="lat-btn act" data-act="app.review">Review change</button>`);
}

// the header's clock ticks on its own timer; it must not make the header
// look changed every poll, or the header would be rebuilt under the pointer
const CLOCK = '·clock·';
function hdr() {
  const st = ctx.state;
  if (!st) return html`<header class="lat-header"><span class="app-name">vCenter API Simulator</span><span class="subtle">${ctx.stateError ? 'cannot reach the admin API: ' + ctx.stateError.message : 'loading…'}</span></header>`;
  const port = (st.API && st.API[0] ? st.API[0].split(':').pop() : '443');
  const host = st.endpoint || 'localhost';
  const loop = /^(127\.|localhost|\[::1\])/.test(st.admin || '');
  const paused = !st.running;
  return html`<header class="lat-header">
    <span class="app-name">vCenter API Simulator</span><span class="lat-ver" style="white-space:nowrap">${st.version}</span>
    <span class="hdr-sep"></span>
    <span class="hdr-kv"><span class="k">API endpoint</span><span class="v">${host}:${port}</span></span>
    <span class="hdr-kv"><span class="k">Version profile</span><span class="v">vCenter ${st.profile.version} · build ${st.profile.build} (captured)</span></span>
    <span class="hdr-kv"><span class="k">Admin UI</span><span class="v">${loop ? 'private' : 'network'} · ${st.admin}</span></span>
    <span class="hdr-kv"><span class="k">Sim clock (UTC)</span><span class="v" id="hdr-clock">${CLOCK}</span></span>
    <span class="hdr-spacer"></span>
    <span class="hdr-kv" style="align-items:flex-end"><span class="k">API service</span><span>${badge(paused ? 'paused' : 'running', paused ? 'warn' : 'ok')} <span class="subtle" style="font-size:11.5px">since ${hms(st.since)}</span></span></span>
    <span class="hdr-actions"><button type="button" class="lat-btn" data-act="app.pause">${paused ? 'Resume serving…' : 'Pause serving…'}</button></span>
  </header>`;
}

function tabs() {
  const st = ctx.state;
  const badges = {};
  if (st) {
    const p = st.pending || {};
    const word = p.scheduled ? 'scheduled' : 'pending';
    if (p.exists && p.topologyChanges) badges.topology = badge(`${p.topologyChanges} ${word}`, 'warn');
    if (p.exists && p.telemetryChanges) badges.telemetry = badge(`${p.telemetryChanges} ${word}`, 'warn');
    const f = st.activity.faults;
    if (f.unexpected) badges.activity = badge(`${int(f.unexpected)} unexpected`, 'bad');
    else if (f.denied) badges.activity = badge(`${int(f.denied)} denied`, 'bad');
    else if (f.injected) badges.activity = badge(`${int(f.injected)} injected`, 'warn');
    else if (st.activity.lastCycle.calls && st.activity.lastCycle.entitiesWithData === 0 && st.activity.lastCycle.steps.some((s) => s.name === 'VsanPerfQueryPerf')) badges.activity = badge('0 series', 'bad');
  }
  const t = st ? html`topology <span class="mono">v${st.topology.version}</span> · seed <span class="mono">${st.topology.seed}</span> · cfg <span class="mono">${st.topology.config}</span> · <kbd>?</kbd> shortcuts` : '';
  const dark = document.documentElement.getAttribute('data-theme') === 'dark';
  return html`<div class="lat-tabs" role="tablist">${TABS.map(([id, label]) => html`<button type="button" role="tab" aria-selected="${ctx.route.name === id}" class="lat-tab${ctx.route.name === id ? ' is-on' : ''}" data-act="app.tab" data-arg="${id}">${label}${badges[id] || ''}</button>`)}</div>
    <span class="tab-meta">${t}<button type="button" class="lat-btn lat-btn--ghost lat-btn--sm theme-btn" data-act="app.theme" aria-label="Switch to ${dark ? 'light' : 'dark'} theme">${dark ? 'Light' : 'Dark'} theme</button></span>`;
}

function renderShell() {
  const root = document.getElementById('shell');
  if (!root.firstChild) {
    root.innerHTML = '<div id="hdr"></div><nav class="tabbar" id="tabs" aria-label="Sections"></nav><div id="banner" class="page" style="padding-bottom:0"></div><main class="page" id="page"></main>';
  }
  const parts = { hdr: hdr(), tabs: tabs(), banner: pendingBanner() };
  const key = JSON.stringify(Object.values(parts).map(String));
  const shown = ctx.state ? key : 'x';
  if (pointerDown) return; // never rebuild under a click in progress
  if (shown !== lastStateJSON) {
    lastStateJSON = shown;
    for (const [id, v] of Object.entries(parts)) setHTML(document.getElementById(id), String(v).replace(CLOCK, clockText()));
    const bn = document.getElementById('banner');
    bn.style.display = String(parts.banner) ? '' : 'none';
  }
}

function setHTML(el, s) {
  const focus = captureFocus();
  el.innerHTML = s;
  restoreFocus(focus);
}

function domPath(el) {
  const path = [];
  for (let n = el; n && n.id !== 'page' && n.id !== 'shell' && n.parentElement; n = n.parentElement) {
    path.unshift(`${n.tagName}:${[...n.parentElement.children].indexOf(n)}`);
  }
  return `${el.closest('#page') ? 'page' : 'shell'}>` + path.join('>');
}
function captureFocus() {
  const a = document.activeElement;
  if (!a || a === document.body || !a.dataset) return null;
  const k = a.dataset.k || a.dataset.act && `${a.dataset.act}|${a.dataset.arg}`;
  if (!k) {
    if (!a.closest('#page, #shell')) return null;
    return { k: domPath(a), sel: null, end: null, attr: 'path' };
  }
  return { k, sel: a.selectionStart, end: a.selectionEnd, attr: a.dataset.k ? 'data-k' : 'act' };
}
function restoreFocus(f) {
  if (!f) return;
  let el;
  if (f.attr === 'path') {
    const [root, ...rest] = f.k.split('>');
    let n = document.getElementById(root === 'page' ? 'page' : 'shell');
    for (const step of rest) {
      if (!n) break;
      const [tag, idx] = step.split(':');
      const c = n.children[Number(idx)];
      n = c && c.tagName === tag ? c : null;
    }
    el = n && n.focus ? n : null;
  } else if (f.attr === 'data-k') el = document.querySelector(`[data-k="${CSS.escape(f.k)}"]`);
  else { const [act, arg] = f.k.split('|'); el = [...document.querySelectorAll('[data-act]')].find((e) => e.dataset.act === act && (e.dataset.arg || '') === (arg || '')); }
  if (el) { el.focus({ preventScroll: true }); try { if (f.sel != null) el.setSelectionRange(f.sel, f.end); } catch (e) { /* not a text field */ } }
}

// a press that has not been released yet: a re-render now would swap the
// element under the pointer and the click would be lost
let pointerDown = false;
document.addEventListener('pointerdown', () => { pointerDown = true; }, true);
for (const t of ['pointerup', 'pointercancel', 'dragend']) document.addEventListener(t, () => { pointerDown = false; }, true);

function renderPage(force) {
  const page = document.getElementById('page');
  if (!page) return;
  if (pointerDown && !force) return;
  let out;
  if (ctx.loadError && !ctx.data) {
    out = html`<div class="lat-banner lat-banner--bad" role="alert"><b>${screen.title} could not load.</b> ${ctx.loadError.message}</div>`;
  } else if (!ctx.data) {
    out = html`<div class="empty">Loading…</div>`;
  } else {
    try { out = screen.render(ctx, ctx.data); }
    catch (e) { console.error(e); out = html`<div class="lat-banner lat-banner--bad" role="alert"><b>${screen.title} failed to draw.</b> ${e.message}</div>`; }
  }
  const s = String(out);
  if (!force && s === lastDataJSON) return;
  lastDataJSON = s;
  const y = window.scrollY;
  const scrolls = [...page.querySelectorAll('[data-scroll]')].map((e) => [e.dataset.scroll, e.scrollTop]);
  setHTML(page, s);
  for (const [k, top] of scrolls) { const e = page.querySelector(`[data-scroll="${k}"]`); if (e) e.scrollTop = top; }
  if (!force) window.scrollTo(0, y);
  for (const f of page.querySelectorAll('input:not([name]):not([id]), select:not([name]):not([id]), textarea:not([name]):not([id])')) f.name = f.dataset.k || f.dataset.field || 'field';
  if (screen.mounted) screen.mounted(ctx, ctx.data, page);
}

// ---- events -----------------------------------------------------------------
const appActions = {
  tab(c, el) { c.go('#/' + el.dataset.arg); },
  review() { ctx.go('#/topology/review'); },
  async cancelScheduled() { await ctx.run(() => api('/apply/cancel', { method: 'POST' }), 'Schedule cancelled; the change is a draft again.'); await ctx.refresh(); },
  async dismissError() { await ctx.run(() => api('/error/dismiss', { method: 'POST' })); await ctx.refreshState(); },
  theme() {
    const next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try { localStorage.setItem('vcs-theme', next); } catch (e) { /* private mode */ }
    lastStateJSON = '';
    renderShell(); renderPage(true);
  },
  async pause() {
    const st = ctx.state;
    if (!st) return;
    if (st.running) {
      const ok = await confirmModal({
        title: 'Pause serving the API?',
        body: html`<ul>
          <li>Connections to <span class="mono">/sdk</span> and <span class="mono">/vsanHealth</span> are refused. A collection that starts now <b>fails</b>; it does not read empty data.</li>
          <li>Topology, history and seeds are untouched. Resume brings every identity back as it was.</li>
          <li>Requests already running finish; the request log keeps counting refused attempts.</li></ul>`,
        confirmLabel: 'Pause serving', danger: true,
      });
      if (!ok) return;
      await ctx.run(() => api('/pause', { body: { paused: true } }), 'Serving paused.');
    } else {
      // resuming is harmless: no confirm
      await ctx.run(() => api('/pause', { body: { paused: false } }), 'Serving resumed.');
    }
    await ctx.refresh();
  },
};

document.addEventListener('click', (ev) => {
  const el = ev.target.closest('[data-act]');
  if (!el || el.disabled) return;
  const name = el.dataset.act;
  const [scope, fn] = name.includes('.') ? name.split('.') : [null, name];
  const handler = scope === 'app' ? appActions[fn] : screen.actions && screen.actions[name];
  if (handler) { ev.preventDefault(); handler(ctx, el, ev, ctx.data); }
});
for (const type of ['change', 'input']) {
  document.addEventListener(type, (ev) => {
    const attr = type === 'change' ? 'chg' : 'inp';
    const el = ev.target.closest(`[data-${attr}]`);
    if (!el) return;
    const h = screen.actions && screen.actions[el.dataset[attr]];
    if (h) h(ctx, el, ev, ctx.data);
  });
}
document.addEventListener('keydown', (ev) => {
  const t = ev.target;
  const typing = t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT');
  if (document.getElementById('modal-root').firstChild) return;
  if (ev.key === '?' && !typing) { ev.preventDefault(); showShortcuts(); return; }
  if (!typing && !ev.ctrlKey && !ev.metaKey && !ev.altKey) {
    if (pendingG && ev.key.length === 1) {
      const dest = { o: 'overview', t: 'topology', e: 'telemetry', a: 'activity', c: 'compat' }[ev.key];
      pendingG = false;
      if (dest) { ev.preventDefault(); ctx.go('#/' + dest); return; }
    }
    if (ev.key === 'g') { pendingG = true; setTimeout(() => { pendingG = false; }, 1200); return; }
  }
  if (screen.keys) screen.keys(ctx, ev, ctx.data, typing);
});
let pendingG = false;

function showShortcuts() {
  const rows = [['?', 'This list'], ['g o / g t / g e / g a / g c', 'Go to Overview, Topology, Telemetry, API Activity, Compatibility'], ['/', 'Focus the filter on the current screen'],
    ['j / k, Enter', 'Move and open in the request log'], ['Esc', 'Close a dialog; every confirm starts on Cancel']];
  openModal(html`<div class="modal-back" data-modal-back><div class="modal" role="dialog" aria-modal="true" aria-label="Keyboard shortcuts"><h2>Keyboard shortcuts</h2>
    <table class="lat-table"><tbody>${rows.map(([k, d]) => html`<tr><td class="mono ink">${k}</td><td class="wrap">${d}</td></tr>`)}</tbody></table>
    <div class="row end"><button type="button" class="lat-btn" data-modal-close autofocus>Close</button></div></div></div>`);
}

// ---- modal and toast ----------------------------------------------------------
let modalReturn = null;
let modalFocus = null;
function openModal(content) {
  const root = document.getElementById('modal-root');
  modalReturn = document.activeElement;
  modalFocus = captureFocus();
  root.innerHTML = String(content);
  const first = root.querySelector('[data-modal-initial]') || root.querySelector('[autofocus]') || root.querySelector('button, input, select');
  if (first) first.focus();
}
function closeModal() {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';
  if (modalReturn && document.contains(modalReturn)) modalReturn.focus();
  else {
    restoreFocus(modalFocus);
    if (document.activeElement === document.body || !document.activeElement) {
      // the trigger is gone (its action rebuilt the screen): land on the page, not on <body>
      const page = document.getElementById('page');
      if (page) { page.setAttribute('tabindex', '-1'); page.focus({ preventScroll: true }); }
    }
  }
  modalReturn = null;
  modalFocus = null;
}
document.getElementById('modal-root').addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape') { ev.stopPropagation(); document.getElementById('modal-root').querySelector('[data-modal-close]')?.click(); }
  if (ev.key === 'Tab') {
    const f = [...document.getElementById('modal-root').querySelectorAll('button:not([disabled]), input, select, textarea, a[href]')];
    if (!f.length) return;
    const i = f.indexOf(document.activeElement);
    if (ev.shiftKey && i <= 0) { ev.preventDefault(); f[f.length - 1].focus(); }
    else if (!ev.shiftKey && i === f.length - 1) { ev.preventDefault(); f[0].focus(); }
  }
});
document.getElementById('modal-root').addEventListener('click', (ev) => {
  if (ev.target.closest('[data-modal-close]') || ev.target.matches('[data-modal-back]')) closeModal();
});

// confirmModal opens a confirm card that starts on Cancel. With `typed`, the
// confirm button stays disabled until the operator types that text.
function confirmModal({ title, body, confirmLabel = 'Confirm', cancelLabel = 'Cancel', danger = false, typed = '', wide = false }) {
  return new Promise((resolve) => {
    const id = 'cf' + Math.random().toString(36).slice(2, 7);
    openModal(html`<div class="modal-back" data-modal-back><div class="modal${wide ? ' modal-wide' : ''}" role="alertdialog" aria-modal="true" aria-labelledby="${id}-t">
      <h2 id="${id}-t">${title}</h2>${body}
      ${typed ? html`<label class="lat-label" for="${id}-in">Type ${typed} to enable ${confirmLabel.split(' ')[0]}</label><input class="lat-input" id="${id}-in" autocomplete="off" spellcheck="false" data-typed="${typed}">` : ''}
      <div class="row end"><button type="button" class="lat-btn" data-modal-close data-modal-initial>${cancelLabel}</button>
      <button type="button" class="lat-btn ${danger ? 'lat-btn--danger' : 'lat-btn--primary'}" data-confirm ${typed ? 'disabled' : ''}>${confirmLabel}</button></div></div></div>`);
    const root = document.getElementById('modal-root');
    const ok = root.querySelector('[data-confirm]');
    const input = root.querySelector('[data-typed]');
    if (input) input.addEventListener('input', () => { ok.disabled = input.value.trim() !== typed; });
    const done = (v) => { root.removeEventListener('click', onClick, true); closeModal(); resolve(v); };
    const onClick = (ev) => {
      if (ev.target.closest('[data-confirm]') && !ok.disabled) { ev.stopPropagation(); done(true); }
      else if (ev.target.closest('[data-modal-close]') || ev.target.matches('[data-modal-back]')) { ev.stopPropagation(); done(false); }
    };
    root.addEventListener('click', onClick, true);
  });
}

function toast(msg, kind = 'ok') {
  const box = document.getElementById('toast');
  const el = document.createElement('div');
  el.innerHTML = String(banner(kind === 'bad' ? 'bad' : kind === 'warn' ? 'warn' : 'ok', html`${msg}`));
  const node = el.firstChild;
  box.appendChild(node);
  setTimeout(() => node.remove(), kind === 'bad' ? 9000 : 4500);
}

// ---- clock + poll ---------------------------------------------------------------
function clockText() {
  const now = new Date(Date.now() + clockOffset).toISOString();
  const bucket = new Date(Math.floor((Date.now() + clockOffset) / 300000) * 300000).toISOString();
  return `${hms(now)} · bucket ${hm(bucket)}`;
}
setInterval(() => {
  const el = document.getElementById('hdr-clock');
  if (el && ctx.state) el.textContent = clockText();
}, 1000);

function editing() {
  const a = document.activeElement;
  const page = document.getElementById('page');
  return a && page && page.contains(a) && a !== page && ['INPUT', 'SELECT', 'TEXTAREA', 'BUTTON', 'A', 'TR'].includes(a.tagName);
}
let tick = 0;
setInterval(async () => {
  tick++;
  if (document.hidden) return;
  await loadState();
  renderShell();
  const every = screen.pollMs ? Math.max(1, Math.round(screen.pollMs / 2000)) : 0;
  if (every && tick % every === 0 && !editing() && !document.getElementById('modal-root').firstChild) {
    await loadScreen();
    renderPage();
  }
}, 2000);

navigate();
export { appActions };
