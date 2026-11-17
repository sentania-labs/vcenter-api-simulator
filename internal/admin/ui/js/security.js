// Security: idle sign-out, the certificate, the console password, and how
// licensing looks to tools.
import { api, html, badge, btn, lbtn, card, dateText } from './lib.js';
import { taskScreen } from './task.js';

// the fingerprint is long: show both ends, copy all of it
const short = (f) => { const p = f.split(':'); return p.length > 14 ? `${p.slice(0, 8).join(':')} … ${p.slice(-4).join(':')}` : f; };

const main = {
  title: 'Security', tab: 'security', poll: true,
  load: () => api('/security'),
  render(ctx, d) {
    const c = d.certificate, l = d.licensing || {};
    const kind = !c ? '' : c.kind === 'managed' ? 'Supplied by the deployment' : c.kind === 'own' ? (c.selfSigned ? 'Your own certificate, signed by itself' : `Your own certificate, issued by ${c.issuer}`) : 'Made by the simulator (self-signed)';
    return html`<div class="m-page"><div class="m-head"><div><h1>Security</h1><p class="sub">How long tools stay signed in, the certificate the simulator shows them, the password for this console, and how licensing looks to tools.</p></div></div>
      <div class="m-grid m-g3">
      ${card('Sign idle tools out', html`<p class="m-muted">Also called the session timeout. A tool that has signed in and then stays quiet for too long is signed out, and has to sign in again.</p>
        <p class="m-lead" style="margin:12px 0">Now: <b>${d.idle ? `signed out after ${d.idle} of no activity` : 'never signed out'}</b></p>
        <div class="m-row">${lbtn('Change the time limit', '/security/idle', 'lat-btn--primary')}</div>`)}
      ${card('Certificate', c ? html`<p class="m-muted">What the simulator shows a tool to prove which server it is.</p><dl class="m-kv" style="margin-top:8px"><dt>Kind</dt><dd>${kind}</dd><dt>Valid for</dt><dd>${c.names.join(', ')}</dd><dt>Runs out</dt><dd>${dateText(c.expires)}${c.days < 0 ? html` ${badge('bad', 'run out')}` : c.days < 30 ? html` ${badge('warn', `in ${c.days} ${c.days === 1 ? 'day' : 'days'}`)}` : ''}</dd>
          <dt>Fingerprint</dt><dd><code>${short(c.fingerprint)}</code> ${btn('Copy', 'copy', 'lat-btn--ghost', { text: c.fingerprint, what: 'The certificate fingerprint' })}</dd></dl>
        <p class="m-small" style="margin-top:6px">The fingerprint is a short code that identifies this exact certificate. Some tools ask for it when you add a vCenter.</p>
        ${c.kind === 'managed' ? html`<div class="lat-banner" style="margin-top:10px">This certificate comes from the settings the simulator was installed with, so it cannot be changed here. To change it, update the certificate in those install settings; the simulator picks it up by itself.</div>`
          : html`<div class="m-row" style="margin-top:10px">${lbtn('Make a new certificate', '/security/new-certificate')}${lbtn('Use my own certificate', '/security/own-certificate')}</div>`}`
        : html`<p class="m-muted">This installation does not let the console show or change the certificate.</p>`)}
      ${card('Console password', html`<p class="m-muted">The password you typed to open this console. Tools never use it.</p>
        ${d.consoleManaged ? html`<div class="lat-banner" style="margin-top:10px">The console password comes from the settings the simulator was installed with, so it cannot be changed here. To change it, update it in those install settings.</div>`
          : html`<div class="m-row" style="margin-top:12px">${lbtn('Change console password', '/security/password')}</div>`}`)}
      ${card('How licensing looks to tools', html`<p class="m-muted">What this vCenter tells a tool about its own license. It can say it is in evaluation. It cannot say it is licensed.</p>
        <dl class="m-kv" style="margin-top:8px"><dt>License</dt><dd>${l.expired ? html`Evaluation, run out on ${dateText(l.ends)} ${badge('warn', 'run out')}` : 'In evaluation, 90 days left'}</dd>
          <dt>Accounts</dt><dd>${l.admins ? 'Licensing administrators' : html`Not licensing administrators ${badge('warn', 'tools will warn')}`}</dd>
          <dt>Managed by</dt><dd>${l.managed ? html`The VCF Operations at ${l.managerUrl || 'an address it did not give'}<br><span class="m-small">Its id: <code>${l.managerId || 'none given'}</code></span>` : 'No VCF Operations'}</dd></dl>
        <div class="m-row" style="margin-top:10px">${lbtn('Change how licensing looks', '/security/licensing')}</div>`)}</div></div>`;
  },
};

const TASKS = { idle: 'idle', licensing: 'licensing', 'new-certificate': 'new-certificate', 'own-certificate': 'own-certificate', password: 'console-password' };
export default { screen: (p, q) => (TASKS[p[1]] ? taskScreen(TASKS[p[1]], q, 'security', 'Security') : main) };
