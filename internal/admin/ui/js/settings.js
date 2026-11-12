// Simulator settings. Passwords and private keys are submitted only when a
// person asks to change them; neither is loaded back from the server.
import { api, html, raw, clone, bytes, int } from './lib.js';

const UI = { modes: {}, custom: {}, open: {}, force: null, scenarioKey: '', editingUser: '', editingRole: 'administrator' };
const idFor = (kind, key = '') => `${kind}-${encodeURIComponent(key)}`;
const selected = (yes) => raw(yes ? 'selected' : '');
const disabled = (yes) => raw(yes ? 'disabled' : '');
const currentMode = (kind, key, spec, fallback = 'inherit') => UI.modes[idFor(kind, key)] ?? (spec?.preset || (spec?.version ? 'custom' : fallback));
const versionLabel = { '8.0u3': '8.0 Update 3', '9.0': '9.0', '9.1.1': '9.1.1' };

function versionControl(kind, key, label, spec, fallback = 'inherit', locked = false) {
  const id = idFor(kind, key), mode = currentMode(kind, key, spec, fallback);
  const inheritLabel = kind === 'esx' ? 'Match vCenter' : kind === 'cluster' ? 'Use instance default' : 'Use cluster setting';
  return html`<div class="version-control"><label class="lat-label" for="${id}">${label}</label><select class="lat-select" id="${id}" data-chg="settings.versionMode" data-kind="${kind}" data-key="${key}" data-k="${id}" ${disabled(locked)}>
    ${fallback === 'inherit' ? html`<option value="inherit" ${selected(mode === 'inherit')}>${inheritLabel}</option>` : ''}
    ${['8.0u3', '9.0', '9.1.1'].map((v) => html`<option value="${v}" ${selected(mode === v)}>${versionLabel[v]}</option>`)}
    <option value="custom" ${selected(mode === 'custom')}>Other version…</option></select>
    ${mode === 'custom' ? html`<div class="fields c2 version-custom">
      ${[['version', 'Version'], ['build', 'Build'], ['apiVersion', 'API version'], ['fullName', 'Full product name']].map(([field, name]) => html`<div><label class="lat-label" for="${id}-${field}">${name}</label><input class="lat-input" id="${id}-${field}" value="${UI.custom[id]?.[field] ?? spec?.[field] ?? ''}" autocomplete="off" data-inp="settings.custom" data-id="${id}" data-field="${field}" ${disabled(locked)}></div>`)}</div>` : ''}</div>`;
}
function readVersion(kind, key, spec, fallback) {
  const id = idFor(kind, key), mode = document.getElementById(id)?.value || currentMode(kind, key, spec, fallback);
  if (mode === 'inherit') return null;
  if (mode !== 'custom') return { preset: mode };
  const out = {}; for (const f of ['version', 'build', 'apiVersion', 'fullName']) out[f] = document.getElementById(`${id}-${f}`)?.value.trim() || '';
  if (Object.values(out).some((x) => !x)) throw new Error('Enter the version, build, API version and full product name.');
  return out;
}
function presetHelp() {
  return html`<p class="anno">These releases change the reported product version. Older vSAN service versions and some release-specific behavior are not yet modelled.</p>`;
}
function versionPanel(d, locked) {
  const sc = d.draft.scenario, clusters = d.tree.datacenters.flatMap((dc) => dc.clusters);
  return html`<section class="lat-card" aria-labelledby="version-title"><h2 class="lat-card-title" id="version-title">vCenter and ESX versions</h2>
    <p>Choose the vCenter release and the ESX releases applications see. Changes are reviewed before they take effect; existing sessions end when applied.</p>
    <div class="fields c2">${versionControl('vc', '', 'vCenter version', sc.vcenterVersion, '9.1.1', locked)}${versionControl('esx', '', 'Default ESX version', sc.esxVersion, 'inherit', locked)}</div>
    ${presetHelp()}
    ${clusters.map((cluster) => {
      const override = (sc.clusters || []).find((c) => c.key === cluster.key);
      return html`<details class="well" data-version-cluster="${cluster.key}" ${raw(UI.open[cluster.key] ? 'open' : '')}><summary>${cluster.datacenter} / ${cluster.name} · ${cluster.hosts} hosts</summary>
        ${versionControl('cluster', cluster.key, 'ESX version for this cluster', override?.esxVersion, 'inherit', locked)}
        <p class="anno">A host can use a different version while it is being upgraded.</p>
        <div class="fields c2">${Array.from({ length: cluster.hosts }, (_, i) => {
          const key = `${cluster.key}/esx${String(i + 1).padStart(2, '0')}`;
          return versionControl('host', key, `Host esx${String(i + 1).padStart(2, '0')}`, sc.hostVersions?.[key], 'inherit', locked);
        })}</div></details>`;
    })}
    <label class="chk"><input type="checkbox" id="force-newer" data-chg="settings.force" ${disabled(locked)} ${raw((UI.force ?? sc.forceNewerHosts) ? 'checked' : '')}> Allow hosts newer than vCenter</label>
    <div class="anno">The review will identify any hosts newer than vCenter.</div>
    <div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="settings.saveVersions" ${disabled(locked)}>Save version changes to draft</button></div></section>`;
}
function usersPanel(d, locked) {
  return html`<section class="lat-card" aria-labelledby="users-title"><h2 class="lat-card-title" id="users-title">vCenter users</h2>
    <p>These accounts are for applications connecting to the simulated vCenter. They are separate from the console password.</p>
    ${d.users.users.length ? html`<table class="lat-table"><thead><tr><th>User</th><th>Access</th><th>Actions</th></tr></thead><tbody>${d.users.users.map((u) => html`<tr><td>${u.name}</td><td>${u.role === 'read-only' ? 'Read-only' : 'Administrator'}</td><td><button type="button" class="lat-btn lat-btn--sm" data-act="settings.editUser" data-name="${u.name}" data-role="${u.role}" ${disabled(locked)}>Change password</button> <button type="button" class="lat-btn lat-btn--sm" data-act="settings.removeUser" data-name="${u.name}" ${disabled(locked)}>Remove</button></td></tr>`)}</tbody></table>` : html`<div class="empty">No users are configured.</div>`}
    <label class="chk"><input type="checkbox" data-chg="settings.anyLogin" ${raw(d.users.anyLogin ? 'checked' : '')} ${disabled(locked)}> Allow any username and password</label>
    <p class="anno">Use this only when a test needs to accept any login.</p>
    <h3>${UI.editingUser ? 'Change password' : 'Add a user'}</h3><div class="fields c2"><div><label class="lat-label" for="user-name">Username</label><input class="lat-input" id="user-name" value="${UI.editingUser}" autocomplete="off" ${disabled(locked || !!UI.editingUser)}></div>
    <div><label class="lat-label" for="user-password">${UI.editingUser ? 'New password' : 'Password'}</label><input class="lat-input" id="user-password" type="password" autocomplete="new-password" ${disabled(locked)}></div>
    <div><label class="lat-label" for="user-role">Access</label><select class="lat-select" id="user-role" ${disabled(locked)}><option value="administrator" ${selected(UI.editingRole === 'administrator')}>Administrator</option><option value="read-only" ${selected(UI.editingRole === 'read-only')}>Read-only</option></select></div></div>
    <div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="settings.saveUser" ${disabled(locked)}>${UI.editingUser ? 'Save password to draft' : 'Add user to draft'}</button>${UI.editingUser ? html`<button type="button" class="lat-btn" data-act="settings.cancelUser">Cancel</button>` : ''}</div>
    <p class="anno">Changed and removed users are signed out when you apply. Stored passwords are never shown.</p></section>`;
}
function capturePanel(d, locked) {
  const s = d.capture.settings;
  return html`<section class="lat-card" aria-labelledby="capture-title"><h2 class="lat-card-title" id="capture-title">Request capture</h2>
    <label class="chk"><input type="checkbox" data-chg="settings.capture" data-field="full" ${disabled(locked)} ${raw(s.full ? 'checked' : '')}> Full request log</label>
    <p class="anno">The full log records request contents and can hold inventory identifiers. Passwords are never kept.</p>
    <label class="chk"><input type="checkbox" data-chg="settings.capture" data-field="unanswered" ${disabled(locked)} ${raw(s.unanswered ? 'checked' : '')}> Unanswered requests</label>
    <label class="chk"><input type="checkbox" data-chg="settings.capture" data-field="stdout" ${disabled(locked)} ${raw(s.stdout ? 'checked' : '')}> Send unanswered requests to cluster logs</label>
    <label class="chk"><input type="checkbox" data-chg="settings.capture" data-field="debug" ${disabled(locked)} ${raw(s.debug ? 'checked' : '')}> Debug capture of license-service requests</label>
    <p class="anno">Writes each license-service request to the logs with its content type, user agent and a scrubbed body. Credentials, cookies and tokens are replaced by a length marker.</p>
    <p>${int(d.capture.fullHeld)} recent full requests held · ${int(d.capture.unansweredHeld)} unanswered requests held · ${bytes(d.capture.fileBytes)} written to ${d.capture.path}</p>
    ${d.capture.error ? html`<div class="lat-banner lat-banner--bad" role="alert">${d.capture.error}</div>` : ''}
    <div class="row"><a class="lat-btn" href="/api/capture/full.jsonl" download>Download full log</a><a class="lat-btn" href="/api/capture/unanswered.jsonl" download>Download unanswered requests</a><button type="button" class="lat-btn" data-act="settings.clearCapture" ${disabled(locked)}>Clear captured requests…</button></div></section>`;
}
function certificatePanel(d, locked) {
  const c = d.certificate;
  return html`<section class="lat-card" aria-labelledby="cert-title"><h2 class="lat-card-title" id="cert-title">Certificate</h2>
    <dl class="kv"><dt>Names</dt><dd>${c.names.join(', ') || 'None'}</dd><dt>Fingerprint (SHA-256)</dt><dd class="mono">${c.fingerprint}</dd><dt>Expires</dt><dd>${new Date(c.expires).toLocaleString()}</dd></dl>
    ${c.managed ? html`<p>This certificate is managed by the deployment. Change it in the deployment settings.</p>` : html`
      <p>Changing the certificate ends current vCenter sessions. Clients must trust the new certificate.</p>
      <label class="lat-label" for="cert-names">Names for a new self-signed certificate</label><input class="lat-input" id="cert-names" ${disabled(locked)} value="${c.names.join(', ')}" placeholder="Names separated by commas">
      <div class="row"><button type="button" class="lat-btn" data-act="settings.regenerateCert" ${disabled(locked)}>Make a new certificate…</button></div>
      <h3>Use your own certificate</h3><div class="fields c2"><div><label class="lat-label" for="cert-file">Certificate file</label><input id="cert-file" ${disabled(locked)} type="file" accept=".pem,.crt,.cer"></div><div><label class="lat-label" for="key-file">Private key file</label><input id="key-file" ${disabled(locked)} type="file" accept=".pem,.key"></div></div>
      <div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="settings.uploadCert" ${disabled(locked)}>Use certificate…</button></div>`}</section>`;
}
function timeoutPanel(d, locked) {
  return html`<section class="lat-card" aria-labelledby="timeout-title"><h2 class="lat-card-title" id="timeout-title">Session timeout</h2><p>Sign out vCenter sessions after this much idle time. Enter zero to keep them until they sign out.</p><label class="lat-label" for="timeout-seconds">Idle time (seconds)</label><input class="lat-input" id="timeout-seconds" ${disabled(locked)} type="number" min="0" max="31536000" value="${d.settings.sessionTimeoutSeconds}"><div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="settings.saveTimeout" ${disabled(locked)}>Save timeout</button></div></section>`;
}
async function perform(ctx, fn, message) { await ctx.run(fn, message); await ctx.refresh(); }

export default {
  id: 'settings', title: 'Simulator settings',
  async load() {
    const [users, draft, tree, capture, certificate, settings, auth] = await Promise.all([api('/credentials'), api('/draft'), api('/tree'), api('/capture'), api('/certificate'), api('/settings'), api('/auth/status')]);
    const key = JSON.stringify(draft.scenario);
    if (UI.scenarioKey && UI.scenarioKey !== key) { UI.modes = {}; UI.custom = {}; UI.force = null; }
    UI.scenarioKey = key;
    return { users, draft, tree, capture, certificate, settings, auth };
  },
  render(ctx, d) {
    const pending = !!ctx.state?.pending?.exists, locked = !!ctx.state?.pending?.scheduled;
    return html`<div class="page-head"><span class="page-title">Simulator settings</span><span class="grow"></span><button type="button" class="lat-btn lat-btn--primary" data-act="settings.review" ${disabled(!pending || locked)}>Review &amp; Apply…</button>${pending && !locked ? '' : html`<span class="anno">${locked ? 'Changes are scheduled' : 'No pending changes'}</span>`}</div>
      ${d.auth?.managed ? html`<div class="lat-banner lat-banner--info">The console password is managed by the deployment. Change it in the deployment settings.</div>` : ''}
      <div class="settings-grid">${usersPanel(d, locked)}${versionPanel(d, locked)}${capturePanel(d, false)}${timeoutPanel(d, false)}${certificatePanel(d, false)}</div>`;
  },
  actions: {
    'settings.review'(ctx) { ctx.go('#/topology/review'); },
    'settings.versionMode'(ctx, el) { document.querySelectorAll('details[data-version-cluster]').forEach((d) => { UI.open[d.dataset.versionCluster] = d.open; }); UI.modes[idFor(el.dataset.kind, el.dataset.key)] = el.value; ctx.rerender(); },
    'settings.custom'(ctx, el) { (UI.custom[el.dataset.id] ||= {})[el.dataset.field] = el.value; },
    'settings.force'(ctx, el) { UI.force = el.checked; },
    async 'settings.saveVersions'(ctx, el, ev, d) {
      await perform(ctx, async () => {
        const sc = clone(d.draft.scenario);
        const vc = readVersion('vc', '', sc.vcenterVersion, '9.1.1');
        if (!sc.vcenterVersion?.preset && !sc.vcenterVersion?.version && vc?.preset === '9.1.1') delete sc.vcenterVersion; else sc.vcenterVersion = vc;
        const esx = readVersion('esx', '', sc.esxVersion, 'inherit'); if (esx) sc.esxVersion = esx; else delete sc.esxVersion;
        sc.hostVersions = {};
        for (const cluster of d.tree.datacenters.flatMap((dc) => dc.clusters)) {
          const existing = (sc.clusters || []).find((c) => c.key === cluster.key);
          const version = readVersion('cluster', cluster.key, existing?.esxVersion, 'inherit');
          if (version) { sc.clusters ||= []; let override = sc.clusters.find((c) => c.key === cluster.key); if (!override) { override = { key: cluster.key }; sc.clusters.push(override); } override.esxVersion = version; }
          else if (existing) { delete existing.esxVersion; if (Object.keys(existing).length === 1) sc.clusters = sc.clusters.filter((c) => c !== existing); }
          for (let i = 0; i < cluster.hosts; i++) { const key = `${cluster.key}/esx${String(i + 1).padStart(2, '0')}`; const v = readVersion('host', key, d.draft.scenario.hostVersions?.[key], 'inherit'); if (v) sc.hostVersions[key] = v; }
        }
        if (!Object.keys(sc.hostVersions).length) delete sc.hostVersions;
        sc.forceNewerHosts = document.getElementById('force-newer').checked;
        await api('/draft', { method: 'PUT', body: { scenario: sc } }); UI.modes = {}; UI.custom = {}; UI.force = null;
      }, 'Version changes added to the draft.');
    },
    'settings.editUser'(ctx, el) { UI.editingUser = el.dataset.name; UI.editingRole = el.dataset.role; ctx.rerender(); document.getElementById('user-password')?.focus(); },
    'settings.cancelUser'(ctx) { UI.editingUser = ''; UI.editingRole = 'administrator'; ctx.rerender(); },
    async 'settings.saveUser'(ctx) {
      const name = UI.editingUser || document.getElementById('user-name').value.trim(), password = document.getElementById('user-password').value, role = document.getElementById('user-role').value;
      if (!name || !password) { ctx.toast('Enter a username and password.', 'bad'); return; }
      await perform(ctx, async () => { await api('/credentials/user', { method: 'PUT', body: { name, password, role } }); UI.editingUser = ''; UI.editingRole = 'administrator'; }, 'User change added to the draft.');
    },
    async 'settings.removeUser'(ctx, el) {
      if (!await ctx.confirm({ title: `Remove ${el.dataset.name}?`, body: html`The user is removed when you apply, and their sessions will end.`, confirmLabel: 'Remove user', danger: true })) return;
      await perform(ctx, () => api('/credentials/user', { method: 'DELETE', body: { name: el.dataset.name } }), 'Removal added to the draft.');
    },
    async 'settings.anyLogin'(ctx, el) { await perform(ctx, () => api('/credentials/mode', { method: 'PUT', body: { anyLogin: el.checked } }), 'Login setting added to the draft.'); },
    async 'settings.capture'(ctx, el, ev, d) { await perform(ctx, () => api('/capture', { method: 'PUT', body: { ...d.capture.settings, [el.dataset.field]: el.checked } }), 'Capture setting saved.'); },
    async 'settings.clearCapture'(ctx) {
      if (!await ctx.confirm({ title: 'Clear captured requests?', body: html`The saved full log and the requests held in memory will be removed. Download them first if you need them.`, confirmLabel: 'Clear requests', danger: true })) return;
      await perform(ctx, () => api('/capture', { method: 'DELETE' }), 'Captured requests cleared.');
    },
    async 'settings.saveTimeout'(ctx) {
      const n = Number(document.getElementById('timeout-seconds').value); if (!Number.isInteger(n) || n < 0 || n > 31536000) { ctx.toast('Enter a whole number between 0 and 31,536,000 seconds.', 'bad'); return; }
      await perform(ctx, () => api('/settings', { method: 'PUT', body: { sessionTimeoutSeconds: n } }), 'Session timeout saved.');
    },
    async 'settings.regenerateCert'(ctx) {
      const names = document.getElementById('cert-names').value.split(',').map((x) => x.trim()).filter(Boolean);
      if (!names.length) { ctx.toast('Enter at least one name.', 'bad'); return; }
      if (!await ctx.confirm({ title: 'Make a new certificate?', body: html`Clients must trust the new certificate and current vCenter sessions will end.`, confirmLabel: 'Make certificate' })) return;
      await perform(ctx, () => api('/certificate/regenerate', { body: { names } }), 'Certificate changed. Clients must trust it again.');
    },
    async 'settings.uploadCert'(ctx) {
      const cert = document.getElementById('cert-file').files[0], key = document.getElementById('key-file').files[0];
      if (!cert || !key) { ctx.toast('Choose a certificate and a private key.', 'bad'); return; }
      if (!await ctx.confirm({ title: 'Use this certificate?', body: html`Clients must trust the new certificate and current vCenter sessions will end.`, confirmLabel: 'Use certificate' })) return;
      await perform(ctx, async () => api('/certificate', { method: 'PUT', body: { certificate: await cert.text(), privateKey: await key.text() } }), 'Certificate changed. Clients must trust it again.');
    },
  },
};
