// Shared helpers for the console: safe HTML templating, the admin API
// client, and the words the console uses for times and counts. No
// framework; screens return HTML built with the `html` tag, which escapes
// every interpolation unless it is `raw()`.

class Safe { constructor(s) { this.s = s; } toString() { return this.s; } }
export const raw = (s) => new Safe(String(s));

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

function part(v) {
  if (v instanceof Safe) return v.s;
  if (v === null || v === undefined || v === false) return '';
  if (Array.isArray(v)) return v.map(part).join('');
  return esc(v);
}
export function html(strings, ...vals) {
  let out = strings[0];
  for (let i = 0; i < vals.length; i++) out += part(vals[i]) + strings[i + 1];
  return new Safe(out);
}
export const join = (list, sep = '') => raw(list.map(part).join(sep));

// ---- words ----------------------------------------------------------------
const nf = new Intl.NumberFormat('en-US');
export const num = (n) => nf.format(Math.round(n ?? 0));
export const plural = (n, one, many) => `${num(n)} ${n === 1 ? one : many || one + 's'}`;
export const list = (a) => (a.length <= 1 ? (a[0] || '') : a.slice(0, -1).join(', ') + ' and ' + a[a.length - 1]);
export const lower1 = (t) => t.charAt(0).toLowerCase() + t.slice(1);

const clock = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
const clockSec = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit', second: '2-digit' });
const day = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' });
const longDay = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'long', year: 'numeric' });
const sameDay = (a, b) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();

// timeText is a moment in the viewer's local time: "2:35 PM" today,
// "12 Nov, 2:35 PM" on any other day.
export function timeText(iso, now = new Date()) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return sameDay(d, now) ? clock.format(d) : `${day.format(d)}, ${clock.format(d)}`;
}
export const timeSeconds = (iso) => clockSec.format(new Date(iso));
export const dateText = (iso) => longDay.format(new Date(iso));

// ago says how long ago something happened, the way a person would.
export function ago(iso, now = new Date()) {
  const d = new Date(iso);
  const s = Math.max(0, Math.round((now.getTime() - d.getTime()) / 1000));
  if (s < 45) return 'a moment ago';
  if (s < 90) return '1 minute ago';
  if (s < 3600) return `${Math.round(s / 60)} minutes ago`;
  if (s < 5400) return '1 hour ago';
  if (s < 86400) return `${Math.round(s / 3600)} hours ago`;
  const days = Math.round((new Date(now.getFullYear(), now.getMonth(), now.getDate()) - new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 86400000);
  if (days <= 1) return 'yesterday';
  return `${days} days ago`;
}

// rich turns a sentence from the simulator into HTML. The simulator marks
// bold with **two stars** and a moment with {{time:...}}, which is shown in
// the viewer's local time. Everything else is escaped.
export function rich(text) {
  return raw(esc(text)
    .replace(/\{\{time:([^}]+)\}\}/g, (_, iso) => esc(timeText(iso)))
    .replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>'));
}
// plain is the same sentence without markup, for dialogs' titles and labels.
export const plain = (text) => String(text ?? '').replace(/\{\{time:([^}]+)\}\}/g, (_, iso) => timeText(iso)).replace(/\*\*([^*]+)\*\*/g, '$1');

// "Later today": the next five-minute mark, the two after it, then the next
// half hour and 30, 60 and 120 minutes after that. Today only.
export function laterMarks(now = new Date()) {
  const t = now.getTime(), five = 300000, half = 1800000;
  const first = Math.ceil((t + 60000) / five) * five;
  const h = Math.ceil((first + 11 * 60000) / half) * half;
  const marks = [first, first + five, first + 2 * five, h, h + half, h + 2 * half, h + 4 * half];
  return marks.map((m) => new Date(m)).filter((d) => sameDay(d, now));
}
// endOfToday is the last moment of the viewer's day.
export function endOfToday(now = new Date()) {
  return new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
}
export function today(now = new Date()) {
  const p = (n) => String(n).padStart(2, '0');
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`;
}

// newPassword makes a password nobody has to think up: three groups of
// letters and digits that are hard to mistake for one another.
export function newPassword() {
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789';
  const pick = new Uint32Array(12);
  crypto.getRandomValues(pick);
  const chars = [...pick].map((n) => alphabet[n % alphabet.length]);
  return `Sim-${chars.slice(0, 4).join('')}-${chars.slice(4, 8).join('')}-${chars.slice(8).join('')}`;
}

// ---- small building blocks ---------------------------------------------------
export const badge = (kind, text) => html`<span class="lat-badge${kind ? ' lat-badge--' + kind : ''}">${text}</span>`;
export const btn = (label, act, cls = '', data = {}) => html`<button type="button" class="lat-btn ${cls}" data-act="${act}" ${attrs(data)}>${label}</button>`;
export const link = (label, href, cls = '') => html`<a class="${cls}" href="#${href}">${label}</a>`;
export const lbtn = (label, href, cls = '') => html`<a class="lat-btn ${cls}" href="#${href}">${label}</a>`;
export const card = (title, body, aside = '') => html`<section class="lat-card">${title ? html`<div class="m-card-h"><h2>${title}</h2>${aside ? html`<span class="aside">${aside}</span>` : ''}</div>` : ''}${body}</section>`;
export const dot = (kind) => html`<span class="m-dot ${kind}" style="display:inline-block"></span>`;
function attrs(data) {
  return raw(Object.entries(data).filter(([, v]) => v !== false && v !== undefined && v !== null).map(([k, v]) => (v === true ? k : `${k === 'disabled' || k.startsWith('aria-') ? k : 'data-' + k}="${esc(v)}"`)).join(' '));
}

// table draws a list that turns into stacked cards at phone width. cols:
// [{h, k, cls}], rows: objects keyed by k, with _cls for the row.
export function table(cols, rows) {
  return html`<table class="lat-table resp"><thead><tr>${cols.map((c) => html`<th class="${c.cls || ''}">${c.h}</th>`)}</tr></thead>
    <tbody>${rows.map((r) => html`<tr class="${r._cls || ''}">${cols.map((c, i) => html`<td class="${c.cls || ''} ${i === 0 ? 'main' : ''}" data-l="${c.h}">${r[c.k] ?? ''}</td>`)}</tr>`)}</tbody></table>`;
}

// fit is how many rows of a list fit the window: the same number every
// time at one window size, five on a phone.
export function fit(offset, rowH = 44) {
  if (window.innerWidth < 720) return 5;
  return Math.max(5, Math.min(16, Math.floor((window.innerHeight - offset) / rowH)));
}

// pager: "1 to 12 of 100", Previous, Next. pageOf returns the rows of the
// page a list is on. Both keep the page in ctx.page[key].
export function pager(ctx, key, total, per) {
  const pages = Math.ceil(total / per);
  if (pages <= 1) return '';
  const pg = Math.min(ctx.page[key] || 0, pages - 1);
  return html`<div class="m-pager"><span>${pg * per + 1} to ${Math.min(total, (pg + 1) * per)} of ${num(total)}</span>${btn('Previous', 'page', '', { k: key, d: -1, disabled: pg === 0 })}${btn('Next', 'page', '', { k: key, d: 1, disabled: pg >= pages - 1 })}</div>`;
}
export function pageOf(ctx, key, rows, per) {
  const pages = Math.max(1, Math.ceil(rows.length / per));
  ctx.page[key] = Math.min(Math.max(0, ctx.page[key] || 0), pages - 1);
  return rows.slice(ctx.page[key] * per, ctx.page[key] * per + per);
}

// ---- admin API ------------------------------------------------------------
export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}
export const polling = { active: false };
export async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', headers: { 'X-Console': '1' }, cache: 'no-store' };
  if (polling.active && init.method === 'GET') init.headers['X-Console-Poll'] = '1';
  if (opts.body !== undefined) {
    init.method = opts.method || 'POST';
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  } else if (init.method !== 'GET') {
    init.headers['Content-Type'] = 'application/json';
    init.body = '{}';
  }
  let res;
  try { res = await fetch('/api' + path, init); } catch (e) { throw new ApiError(0, 'The console cannot reach the simulator. Check that it is still running, then try again.'); }
  let data = null;
  try { data = await res.json(); } catch (e) { /* empty body */ }
  if (res.status === 401) { location.assign('/login?expired=1'); throw new ApiError(401, 'Please sign in again.'); }
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || 'The simulator could not do that. Try again.');
  return data;
}
