// The console shell: header, the eight tabs, the bars shown on every screen
// (paused, a change failed, scheduled), the green "Done." line, dialogs,
// routing and polling. Screens are plain objects:
//
//   { title, tab, load(ctx) -> data, render(ctx, data) -> html, actions: { name(ctx, el) }, poll }
//
// One request, /api/home, tells the shell everything it shows on every
// screen, so the header, the tabs and Home can never disagree.
import { api, polling, html, raw, join, esc, badge, btn, lbtn, rich, plain, plural, timeText, newPassword } from './lib.js';
import home from './home.js';
import clusters from './clusters.js';
import storage from './storage.js';
import versions from './versions.js';
import users from './users.js';
import security from './security.js';
import faults from './faults.js';
import requests from './requests.js';

const TABS = [['home', 'Home'], ['clusters', 'Clusters'], ['storage', 'Storage'], ['versions', 'Versions'], null, ['users', 'Users'], ['security', 'Security'], null, ['faults', 'Slowness and faults'], ['requests', 'Requests']];
const MODULES = { home, clusters, cluster: clusters, storage, versions, users, security, faults, requests };
// addresses of the console before the redesign still land somewhere sensible
const OLD = { overview: 'home', topology: 'clusters', telemetry: 'faults', activity: 'requests', 'api-activity': 'requests', api: 'requests', compat: 'requests/supported', compatibility: 'requests/supported', settings: 'security' };

export const ctx = {
  home: null, homeError: null, route: { path: '/home', parts: ['home'], query: {} }, screen: null, data: null, loadError: null,
  F: {}, page: {}, flash: null, marks: [], retry: null, menu: false, setup: { pw: '', show: false },
  api,
  go(path) { const h = '#' + path; if (location.hash === h) navigate(); else location.hash = h; },
  rerender() { render(); },
  async reload() { await Promise.all([loadHome(), loadScreen()]); render(); },
  // say shows a green (or warning) line at the top of the current screen
  say(tone, text) { ctx.flash = { tone, text, route: ctx.route.path }; scrollTop = true; },
  mark(type, id) { const m = ctx.marks.find((x) => x.type === type && x.id === id); return m ? m.tag : ''; },
  dialog, confirm, closeDialog,
  async copy(text, what) {
    try {
      await navigator.clipboard.writeText(text);
      ctx.say('ok', `**Copied.** ${what} is on your clipboard.`);
    } catch (e) {
      ctx.say('warn', `**Not copied.** This browser would not let the console copy for you. Select ${what.toLowerCase()} and copy it yourself.`);
    }
    render();
  },
  // act runs something that happens at once and shows what it did
  async act(fn, after) {
    try {
      const r = await fn();
      if (r && r.done) ctx.say(r.tone || 'ok', r.done);
      if (after) after(r);
    } catch (e) { ctx.say('bad', `**That did not work.** ${e.message}`); }
    await ctx.reload();
  },
};

let scrollTop = false;
let lastHTML = '';

// ---- data ---------------------------------------------------------------------
async function loadHome() {
  try { ctx.home = await api('/home'); ctx.homeError = null; } catch (e) { ctx.homeError = e; }
}
async function loadScreen() {
  const s = ctx.screen;
  if (!s || !s.load || (ctx.home && ctx.home.accountNeeded)) return;
  try { const d = await s.load(ctx); if (s === ctx.screen) { ctx.data = d; ctx.loadError = null; } }
  catch (e) { if (s === ctx.screen) ctx.loadError = e; }
}

// ---- routing ------------------------------------------------------------------
function parseHash() {
  let h = location.hash.replace(/^#\/?/, '');
  const [path, q] = h.split('?');
  let parts = path.split('/').filter(Boolean).map(decodeURIComponent);
  if (OLD[parts[0]]) parts = OLD[parts[0]].split('/');
  if (!MODULES[parts[0]]) parts = ['home'];
  return { path: '/' + parts.join('/') + (q ? '?' + q : ''), parts, query: Object.fromEntries(new URLSearchParams(q || '')) };
}
async function navigate() {
  const next = parseHash();
  const changed = next.path !== ctx.route.path || !ctx.screen;
  ctx.route = next;
  if (changed) {
    ctx.F = {};
    if (ctx.flash && ctx.flash.route !== next.path.split('?')[0] && ctx.flash.route !== next.path) { ctx.flash = null; }
    if (!ctx.flash) ctx.marks = [];
    ctx.menu = false;
  }
  ctx.screen = MODULES[next.parts[0]].screen(next.parts, next.query, ctx);
  ctx.data = null; ctx.loadError = null;
  document.title = `${ctx.screen.title} · vCenter Simulator`;
  if (changed) scrollTop = true;
  render();
  await Promise.all([loadHome(), loadScreen()]);
  render();
  if (changed && next.query.show) { const el = document.getElementById('reqlist'); if (el) el.scrollIntoView({ block: 'nearest' }); }
}
window.addEventListener('hashchange', navigate);
window.addEventListener('resize', () => render());

// ---- shell --------------------------------------------------------------------
const tabOf = (p0) => (p0 === 'cluster' ? 'clusters' : p0);
function tabBadge(k) {
  const h = ctx.home;
  if (!h) return '';
  if (k === 'requests') return html`${h.unanswered ? badge('bad', `${h.unanswered} not answered`) : ''}${h.capturing ? html` ${badge('accent', 'Capturing all')}` : ''}`;
  if (k === 'faults') return h.faults ? badge('warn', `${h.faults} on`) : '';
  return '';
}
const dark = () => document.documentElement.getAttribute('data-theme') === 'dark';

function header() {
  const h = ctx.home, paused = h && !h.running, n = h ? h.attention.length : 0;
  return html`<header class="lat-header m-hdr">
    <a class="m-brand" href="#/home"><span class="desk">vCenter </span>Simulator</a>
    ${h ? html`<span class="m-status ${paused ? 'paused' : ''}"><i></i>${paused ? 'Paused' : 'Running'}</span>
    <span class="m-addr">Address for your tool: <code>${h.address}</code></span>` : html`<span class="m-addr">${ctx.homeError ? ctx.homeError.message : 'Loading…'}</span>`}
    <span class="m-sp"></span>
    <span class="desk m-row">${h && !paused ? btn('Pause…', 'pause') : ''}${btn(dark() ? 'Light theme' : 'Dark theme', 'theme', 'lat-btn--ghost')}${btn('Sign out', 'signout', 'lat-btn--ghost')}</span>
    <button type="button" class="lat-btn m-menu-btn" data-act="menu" ${n ? raw(`aria-label="Menu, ${n} ${n === 1 ? 'thing needs' : 'things need'} attention"`) : ''}>Menu${n ? html` <span class="lat-badge lat-badge--bad">${n} to check</span>` : ''}</button>
  </header>
  <nav class="lat-tabs m-tabs" aria-label="Sections">${TABS.map((t) => (t ? html`<a class="lat-tab ${tabOf(ctx.route.parts[0]) === t[0] ? 'is-on' : ''}" href="#/${t[0]}" ${tabOf(ctx.route.parts[0]) === t[0] ? raw('aria-current="page"') : ''}>${t[1]} ${tabBadge(t[0])}</a>` : html`<span class="sep"></span>`))}</nav>`;
}

function bars() {
  const h = ctx.home;
  if (!h) return '';
  const out = [];
  const onHome = ctx.route.parts[0] === 'home' && !ctx.route.parts[1];
  if (!h.running) out.push(html`<div class="m-topbar warn" role="status"><span><b>The simulator is paused.</b> It refuses every connection until you resume. Nothing has been lost.</span><span class="acts">${btn('Resume', 'resume', 'lat-btn--primary')}</span></div>`);
  if (h.failure) out.push(html`<div class="m-topbar bad" role="alert"><span><b>${h.failure.what}</b> ${h.failure.why} ${h.failure.fix}</span><span class="acts">${btn('Try again', 'retry', 'lat-btn--primary')}${btn('Hide this', 'dismissFailure')}</span></div>`);
  const s = h.scheduled;
  if (s.length === 1 && !onHome) out.push(html`<div class="m-topbar info" role="status"><span><b>Scheduled for ${timeText(s[0].at)}:</b> ${s[0].text}. Nothing has changed yet.</span><span class="acts">${btn('Do it now', 'schedNow', '', { id: s[0].id })}${btn('Cancel it', 'schedCancel', '', { id: s[0].id })}</span></div>`);
  else if (s.length > 1 && !onHome) out.push(html`<div class="m-topbar info" role="status"><span><b>${s.length} changes are scheduled.</b> The next is at ${timeText(s[0].at)}: ${s[0].text.charAt(0).toLowerCase() + s[0].text.slice(1)}. Nothing has changed yet.</span><span class="acts">${lbtn('See them all on Home', '/home')}</span></div>`);
  return join(out);
}

function flashLine() {
  const f = ctx.flash;
  if (!f) return '';
  return html`<div class="lat-banner lat-banner--${f.tone === 'warn' ? 'warn' : f.tone === 'bad' ? 'bad' : f.tone === 'info' ? 'info' : 'ok'} m-flash" role="status"><span>${rich(f.text)}</span>${btn('Hide', 'dismissFlash', 'lat-btn--ghost x')}</div>`;
}

function body() {
  const s = ctx.screen;
  if (ctx.loadError && !ctx.data) {
    const back = s.back || ['Go to Home', '/home'];
    return html`<div class="m-page">${flashLine()}<div class="m-empty"><h3>${ctx.loadError.message}</h3>${lbtn(back[0], back[1], 'lat-btn--primary')}</div></div>`;
  }
  if (s.load && !ctx.data) return html`<div class="m-page"><div class="m-loading" role="status">Loading…</div></div>`;
  let out;
  try { out = String(s.render(ctx, ctx.data)); }
  catch (e) { console.error(e); return html`<div class="m-page"><div class="m-empty"><h3>This screen could not be drawn</h3><p>Reload the page. If it keeps happening, tell the simulator's maintainers what you were doing.</p></div></div>`; }
  const flash = String(flashLine());
  if (flash) out = out.replace('<div class="m-page">', () => '<div class="m-page">' + flash);
  return raw(out);
}

// first-run step 2: the password tools will sign in with
function setupAccount() {
  const F = ctx.setup, bad = F.err;
  return html`<div class="m-auth"><form class="lat-card" data-submit="setupAccount"><div><p class="m-small">vCenter Simulator · Step 2 of 2</p><h1>Choose the password your tools will sign in with</h1></div>
    <p class="m-muted">Tools sign in to the simulated vCenter with a username and password, as they would to a real one. The first account is ready; it only needs a password.</p>
    <dl class="m-kv"><dt>Username</dt><dd><code>administrator@vsphere.local</code></dd></dl>
    <div class="m-field ${bad ? 'has-err' : ''}"><label for="f-setup-pw">Password for that account</label>
      <div class="m-inline"><input class="lat-input" style="width:220px" type="${F.show ? 'text' : 'password'}" id="f-setup-pw" data-setup="pw" value="${F.pw}" autocomplete="new-password">${btn('Make one for me', 'setupGen')}${F.pw && F.show ? btn('Copy', 'setupCopy') : ''}</div>
      <label class="m-check"><input type="checkbox" data-setup="show" ${F.show ? raw('checked') : ''}> <span>Show the password</span></label>
      ${bad ? html`<span class="lat-hint lat-hint--bad" role="alert">${bad}</span>` : html`<span class="lat-hint">At least 8 characters. ${F.copied ? html`<b>Copied.</b> ` : ''}You will give it to your tool in a moment, so copy it or remember it.</span>`}</div>
    <div class="m-go"><button type="submit" class="lat-btn lat-btn--primary">Save and open the console</button></div>
    <p class="m-small">You can add more users, or change this password, later on the Users screen.</p></form></div>`;
}

function render() {
  const app = document.getElementById('app');
  let out;
  if (ctx.home && ctx.home.accountNeeded) out = String(setupAccount());
  else if (!ctx.screen) return;
  else out = String(html`${header()}${bars()}<main>${body()}</main>`);
  if (out !== lastHTML && !pointerDown) {
    lastHTML = out;
    const act = document.activeElement;
    const keep = act && act.id && app.contains(act) ? { id: act.id, s: act.selectionStart, e: act.selectionEnd } : null;
    const scrolls = [...app.querySelectorAll('[data-scroll]')].map((e) => [e.dataset.scroll, e.scrollTop]);
    const y = window.scrollY;
    app.innerHTML = out;
    for (const [k, top] of scrolls) { const e = app.querySelector(`[data-scroll="${CSS.escape(k)}"]`); if (e) e.scrollTop = top; }
    if (keep) { const el = document.getElementById(keep.id); if (el) { el.focus({ preventScroll: true }); try { el.setSelectionRange(keep.s, keep.e); } catch (e) { /* not a text box */ } } }
    window.scrollTo(0, scrollTop ? 0 : y);
  }
  scrollTop = false;
  const ov = document.getElementById('overlay');
  if (ctx.menu && !current) { ov.hidden = false; ov.innerHTML = String(menu()); } else if (!current) { ov.hidden = true; ov.innerHTML = ''; }
}

function menu() {
  const h = ctx.home, cur = tabOf(ctx.route.parts[0]), n = h ? h.attention.length : 0;
  return html`<div class="m-dlg" role="dialog" aria-label="Menu"><div class="m-menu">${TABS.filter(Boolean).map((t) => html`<a href="#/${t[0]}" class="${cur === t[0] ? 'is-on' : ''}">${t[1]} <span>${t[0] === 'home' && n ? badge('bad', plural(n, 'thing') + ' to check') : tabBadge(t[0])}</span></a>`)}<hr>
    <a href="#" data-act="${h && !h.running ? 'resume' : 'pause'}">${h && !h.running ? 'Resume the simulator' : 'Pause the simulator…'}</a><a href="#" data-act="theme">${dark() ? 'Light theme' : 'Dark theme'}</a><a href="#" data-act="signout">Sign out</a></div>
    <div class="m-row">${btn('Close', 'closeMenu')}</div></div>`;
}

// ---- dialogs --------------------------------------------------------------------
let current = null; // the open dialog's resolver
function dialog({ title, body, ok, cancel, danger, wide }) {
  return new Promise((resolve) => {
    const ov = document.getElementById('overlay');
    const back = document.activeElement;
    ctx.menu = false;
    current = (v) => { current = null; ov.hidden = true; ov.innerHTML = ''; if (back && document.contains(back)) back.focus(); resolve(v); };
    ov.hidden = false;
    ov.innerHTML = String(html`<div class="m-dlg" role="dialog" aria-modal="true" aria-label="${title}" ${wide ? raw('style="max-width:640px"') : ''}><h2>${title}</h2>${body}
      <div class="m-row">${ok ? html`${btn(cancel || 'Cancel', 'dlgCancel')}${btn(ok, 'dlgOk', danger ? 'lat-btn--danger' : 'lat-btn--primary')}` : btn('Close', 'dlgCancel', 'lat-btn--primary')}</div></div>`);
    const first = ov.querySelector('[data-act=dlgCancel]');
    if (first) first.focus();
  });
}
function confirm(o) { return dialog(o); }
function closeDialog(v = false) { if (current) current(v); }

// ---- things every screen can do -----------------------------------------------------
const common = {
  dlgOk() { closeDialog(true); },
  dlgCancel() { closeDialog(false); },
  menu() { ctx.menu = true; render(); },
  closeMenu() { ctx.menu = false; render(); },
  dismissFlash() { ctx.flash = null; render(); },
  theme() {
    const next = dark() ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try { localStorage.setItem('vcs-theme', next); } catch (e) { /* private mode */ }
    ctx.menu = false; render();
  },
  async signout() { try { await api('/auth/logout', { method: 'POST' }); } finally { location.assign('/login?out=1'); } },
  async pause() {
    ctx.menu = false; render();
    const yes = await confirm({ title: 'Pause the simulator?', ok: 'Pause', body: html`<p>While paused it refuses every connection, so a tool that tries to collect will report that it cannot reach vCenter.</p><p>Nothing is lost. When you resume, every cluster, host and VM is exactly as it was.</p>` });
    if (!yes) return;
    await ctx.act(async () => { await api('/pause', { body: { paused: true } }); return { done: '**Paused.** The simulator is refusing connections until you resume.' }; });
  },
  async resume() {
    ctx.menu = false;
    await ctx.act(async () => { await api('/pause', { body: { paused: false } }); return { done: '**Running again.** Tools can connect.' }; });
  },
  async dismissFailure() { try { await api('/failure/dismiss', { method: 'POST' }); } catch (e) { /* the next poll shows the truth */ } await ctx.reload(); },
  async retry() {
    const f = ctx.home && ctx.home.failure;
    if (!f) return;
    ctx.retry = f.kind ? { kind: f.kind, params: f.params } : null;
    try { await api('/failure/dismiss', { method: 'POST' }); } catch (e) { /* the next poll shows the truth */ }
    ctx.go(f.route + (f.kind ? '?retry=1' : ''));
  },
  async schedNow(c, el) {
    await ctx.act(async () => {
      const r = await api(`/scheduled/${el.dataset.id}/now`, { method: 'POST' });
      if (r.failed) return { done: '' };
      ctx.marks = r.outcome.marks || [];
      return { done: `**${r.outcome.lead || 'Done.'}** ${r.outcome.done}`, tone: r.outcome.tone };
    });
  },
  async schedCancel(c, el) {
    const s = ctx.home.scheduled.find((x) => String(x.id) === el.dataset.id);
    if (!s) return;
    const yes = await confirm({ title: 'Cancel this scheduled change?', ok: 'Cancel the change', cancel: 'Keep it', body: html`<p><b>${s.text}</b>, scheduled for ${timeText(s.at)}.</p><p>It will not happen. Nothing else is affected.</p>` });
    if (yes) await ctx.act(() => api(`/scheduled/${s.id}`, { method: 'DELETE' }));
  },
  async condOff(c, el) { await ctx.act(() => api(`/faults/${el.dataset.id}`, { method: 'DELETE' })); },
  page(c, el) { ctx.page[el.dataset.k] = (ctx.page[el.dataset.k] || 0) + Number(el.dataset.d); render(); },
  copy(c, el) { ctx.copy(el.dataset.text, el.dataset.what); },
  setupGen() { ctx.setup.pw = newPassword(); ctx.setup.show = true; ctx.setup.copied = false; ctx.setup.err = ''; render(); },
  async setupCopy() { try { await navigator.clipboard.writeText(ctx.setup.pw); ctx.setup.copied = true; } catch (e) { ctx.setup.err = 'This browser would not let the console copy for you. Select the password and copy it yourself.'; } render(); },
  async setupAccount() {
    const pw = ctx.setup.pw || '';
    if (pw.length < 8) { ctx.setup.err = pw ? `That is ${pw.length} characters. Use at least 8, or press "Make one for me".` : 'Type a password of at least 8 characters, or press "Make one for me".'; render(); return; }
    try {
      await api('/setup/account', { body: { password: pw } });
      ctx.setup = { pw: '', show: false };
      ctx.flash = { tone: 'ok', text: '**All set.** The simulator is running. Follow the three steps below to connect your first tool.', route: '/home' };
      location.hash = '#/home';
      await navigate();
    } catch (e) { ctx.setup.err = e.message; render(); }
  },
};

// ---- events ---------------------------------------------------------------------
// a press that has not been released yet: a re-render now would swap the
// element under the pointer and the click would be lost
let pointerDown = false;
document.addEventListener('pointerdown', () => { pointerDown = true; }, true);
for (const t of ['pointerup', 'pointercancel', 'dragend']) document.addEventListener(t, () => { pointerDown = false; }, true);

function handler(name) { return (ctx.screen && ctx.screen.actions && ctx.screen.actions[name]) || common[name]; }
document.addEventListener('click', (ev) => {
  const el = ev.target.closest('[data-act]');
  if (!el) { if (ev.target.id === 'overlay') { if (current) closeDialog(false); else if (ctx.menu) { ctx.menu = false; render(); } } return; }
  if (el.disabled) return;
  const fn = handler(el.dataset.act);
  if (!fn) return;
  if (el.tagName === 'A' || el.tagName === 'BUTTON') ev.preventDefault();
  pointerDown = false;
  fn(ctx, el, ev);
});
document.addEventListener('input', (ev) => {
  const el = ev.target;
  if (el.dataset.setup) { ctx.setup[el.dataset.setup] = el.type === 'checkbox' ? el.checked : el.value; ctx.setup.err = ''; if (el.type === 'checkbox') render(); return; }
  if (ctx.screen && ctx.screen.input) ctx.screen.input(ctx, el, ev);
});
document.addEventListener('change', (ev) => { if (ctx.screen && ctx.screen.change) ctx.screen.change(ctx, ev.target, ev); });
document.addEventListener('submit', (ev) => {
  ev.preventDefault();
  const name = ev.target.dataset.submit;
  if (name) { const fn = handler(name); if (fn) fn(ctx, ev.target, ev); return; }
  const b = ev.target.querySelector('.m-go [data-act]');
  if (b) b.click();
});
document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape') return;
  if (current) closeDialog(false); else if (ctx.menu) { ctx.menu = false; render(); }
});

// ---- polling --------------------------------------------------------------------
function typing() {
  const a = document.activeElement;
  return a && document.getElementById('app').contains(a) && ['INPUT', 'SELECT', 'TEXTAREA'].includes(a.tagName);
}
setInterval(async () => {
  if (document.hidden || !ctx.screen) return;
  polling.active = true;
  try {
    await loadHome();
    if (ctx.screen.poll && !typing() && !current) await loadScreen();
    render();
  } finally { polling.active = false; }
}, 4000);

navigate();
export { esc, plain };
