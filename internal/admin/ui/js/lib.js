// Shared helpers for the console: safe HTML templating, formatting, the
// admin API client. No framework; screens return HTML strings built with
// the `html` tag, which escapes every interpolation unless it is `raw()`.

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

// ---- formatting ---------------------------------------------------------
const nf = new Intl.NumberFormat('en-US');
export const int = (n) => nf.format(Math.round(n ?? 0));
export const signed = (n) => (n > 0 ? '+' : n < 0 ? '−' : '') + int(Math.abs(n));
export function pct(n, d = 1) { return (n ?? 0).toFixed(d) + ' %'; }
export function bytes(n) {
  n = n ?? 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? int(n) : n >= 100 ? n.toFixed(0) : n.toFixed(1)) + ' ' + u[i];
}
export function ms(n) {
  n = n ?? 0;
  if (n >= 10000) return (n / 1000).toFixed(1) + ' s';
  if (n >= 100) return int(n) + ' ms';
  if (n >= 10) return n.toFixed(0) + ' ms';
  return n.toFixed(1) + ' ms';
}
const p2 = (n) => String(n).padStart(2, '0');
export function localTime(iso) { if (!iso) return '—'; return new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }).format(new Date(iso)); }
export function scheduledMessage(p) {
  const what = p.topologyChanges ? 'A topology change' : 'A change';
  return html`<b>${what} is scheduled for <span title="${p.at}">${localTime(p.at)}</span>.</b> Changes are locked until then.`;
}
export function hms(iso) { if (!iso) return '—'; const d = new Date(iso); return `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}:${p2(d.getUTCSeconds())}Z`; }
export function hm(iso) { if (!iso) return '—'; const d = new Date(iso); return `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}`; }
export function hmsms(iso) { const d = new Date(iso); return `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}:${p2(d.getUTCSeconds())}.${String(d.getUTCMilliseconds()).padStart(3, '0')}`; }
export function dateTime(iso) { if (!iso) return '—'; const d = new Date(iso); return `${d.getUTCFullYear()}-${p2(d.getUTCMonth() + 1)}-${p2(d.getUTCDate())} ${hm(iso)}Z`; }
export function ago(iso, now = Date.now()) {
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (s < 90) return s + ' s ago';
  if (s < 5400) return Math.round(s / 60) + ' min ago';
  return Math.round(s / 3600) + ' h ago';
}
export const plural = (n, one, many) => `${int(n)} ${n === 1 ? one : many || one + 's'}`;

// ---- small components ----------------------------------------------------
export const badge = (text, kind = '') => html`<span class="lat-badge${kind ? ' lat-badge--' + kind : ''}">${text}</span>`;
export function tile(k, v, d, extra = '') {
  return html`<div class="tile ${extra}"><div class="k">${k}</div><div class="v">${v}</div>${d ? html`<div class="d">${d}</div>` : ''}</div>`;
}
export function banner(kind, body, action = '') {
  return html`<div class="lat-banner lat-banner--${kind}" role="${kind === 'bad' ? 'alert' : 'status'}">${action}${body}</div>`;
}
export function btn(label, act, arg = '', cls = '', extra = '') {
  return html`<button type="button" class="lat-btn ${cls}" data-act="${act}" data-arg="${arg}" ${raw(extra)}>${label}</button>`;
}
export const familyBehaviourLabel = { data: 'data', empty: 'empty', 'invalid-argument': 'invalid arg', 'not-supported': 'not supported' };

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
  const res = await fetch('/api' + path, init);
  let data = null;
  try { data = await res.json(); } catch (e) { /* empty body */ }
  if (res.status === 401) { location.assign('/login?expired=1'); throw new ApiError(401, 'Please sign in again.'); }
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || res.statusText);
  return data;
}

// deep clone of plain JSON
export const clone = (v) => JSON.parse(JSON.stringify(v));
