// Users: the accounts tools sign in to the simulated vCenter with.
import { api, html, raw, badge, btn, lbtn, card, table, dot, plural, list, ago } from './lib.js';
import { taskScreen } from './task.js';

const ROLE = { full: 'Full access', read: 'Read-only' };
const upper1 = (s) => s.charAt(0).toUpperCase() + s.slice(1);

const main = {
  title: 'Users', tab: 'users', poll: true,
  load: () => api('/users'),
  render(ctx, d) {
    const rows = d.users.map((u) => {
      const tag = ctx.mark('user', u.name);
      return { _cls: tag ? 'is-new' : '', name: html`${u.name} ${tag ? badge('ok', tag) : ''}`, role: ROLE[u.role],
        last: u.inUse ? html`<span><span style="white-space:nowrap">${dot('ok')} In use now</span> by the tool at ${list(u.inUse)}</span>` : u.last ? upper1(ago(u.last)) : 'Never',
        act: html`${lbtn('Change', '/users/edit/' + encodeURIComponent(u.name))} ${btn('Remove…', 'removeUser', '', { n: u.name })}` };
    });
    const cols = [{ h: 'Username', k: 'name' }, { h: 'Access', k: 'role' }, { h: 'Last signed in', k: 'last' }, { h: '', k: 'act', cls: 'act' }];
    return html`<div class="m-page"><div class="m-head"><div><h1>Users</h1><p class="sub">The usernames and passwords that tools use to sign in to the simulated vCenter. These are separate from the password for this console.</p></div>
      <div class="acts">${lbtn('Add a user', '/users/add', 'lat-btn--primary')}</div></div>
      <div class="m-grid m-g21">${card('Who can sign in', html`${d.anyLogin ? html`<div class="lat-banner lat-banner--warn" style="margin-bottom:10px"><b>Any username and password is accepted right now</b>, so this list is not being checked. Turn that off under "Accept anyone" to use the list again.</div>` : ''}
          ${rows.length ? table(cols, rows) : html`<div class="m-empty"><h3>No users yet</h3><p>Any username and password is accepted, so none are needed. Add one before turning "Accept anyone" off.</p></div>`}`, plural(d.users.length, 'user'))}
        ${card('Accept anyone', html`<p class="m-muted">For tests where the username and password do not matter.</p><div style="margin:10px 0"><button type="button" class="m-switch ${d.anyLogin ? 'on' : ''}" data-act="anyLogin" role="switch" aria-checked="${d.anyLogin}"><i></i><span>Accept any username and password: <b>${d.anyLogin ? 'On' : 'Off'}</b></span></button></div>
          <p class="m-small">${d.anyLogin ? 'Anyone who can reach the simulator can sign in with full access.' : 'Off is the safe setting: only the users in the list can sign in.'}</p>`)}</div></div>`;
  },
  actions: {
    async anyLogin(ctx) {
      const on = !ctx.data.anyLogin;
      if (on) {
        const yes = await ctx.confirm({ title: 'Accept any username and password?', ok: 'Yes, accept anyone', body: html`<p>Anyone who can reach the simulator will be able to sign in, with any name and any password, and gets full access.</p><p>Use it only for a test that needs it, and turn it off afterwards.</p>` });
        if (!yes) return;
      }
      await ctx.act(() => api('/users/any-login', { method: 'PUT', body: { on } }));
    },
    async removeUser(ctx, el) {
      const u = ctx.data.users.find((x) => x.name === el.dataset.n);
      if (!u) return;
      if (ctx.data.users.length === 1) {
        ctx.dialog({ title: 'This is the only user', body: html`<p>If ${u.name} were removed, nothing could sign in to the simulated vCenter.</p><p>Add another user first, then remove this one.</p>` });
        return;
      }
      const live = u.inUse ? list(u.inUse) : '';
      const yes = await ctx.confirm({ title: `Remove ${u.name}?`, danger: true, ok: live ? 'Remove user and sign the tool out' : 'Remove user',
        body: html`${live ? html`<div class="lat-banner lat-banner--warn" style="margin-bottom:10px"><b>The tool at ${live} is signed in as this user right now.</b> It will be signed out at once and refused from then on, so it stops collecting until you give it another account.</div>` : ''}
          <p>${u.name} will no longer be able to sign in.${live ? '' : ' Nothing is signed in as this user at the moment.'}</p><p>Other users are not affected.${live ? ' What the tool asked for so far stays on the Requests screen.' : ''}</p>` });
      if (!yes) return;
      await ctx.act(async () => { const r = await api('/users', { method: 'DELETE', body: { name: u.name } }); return { done: `**Done.** ${r.done}`, tone: r.tone }; });
    },
  },
};

export default {
  screen(p, q) {
    if (p[1] === 'add') return taskScreen('add-user', q, 'users', 'Users');
    if (p[1] === 'edit' && p[2]) return taskScreen('change-user', { ...q, name: p[2] }, 'users', 'Users');
    return main;
  },
};
export { raw };
