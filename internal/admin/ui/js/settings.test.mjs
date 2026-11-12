import test from 'node:test';
import assert from 'node:assert/strict';
import settings from './settings.js';

const data = () => ({
  users: { users: [{ name: 'administrator@vsphere.local', role: 'administrator' }, { name: 'reader@vsphere.local', role: 'read-only' }], anyLogin: false },
  draft: { scenario: { datacenters: [], clusters: [] } }, tree: { datacenters: [] },
  capture: { settings: { full: false, unanswered: true, stdout: false }, fullHeld: 0, unansweredHeld: 0, fileBytes: 0, path: '/data/query-log.jsonl' },
  certificate: { managed: true, names: ['console.example.test'], fingerprint: 'abc', expires: '2030-01-01T00:00:00Z' },
  settings: { sessionTimeoutSeconds: 0 },
});

test('deployment managed certificate is read-only and settings are discoverable', () => {
  const markup = String(settings.render({ state: { pending: { exists: false } } }, data()));
  assert.match(markup, /managed by the deployment/);
  assert.doesNotMatch(markup, /data-act="settings\.(?:uploadCert|regenerateCert)"/);
  assert.match(markup, /Read-only/); assert.match(markup, /Full request log/);
  assert.match(markup, /Unanswered requests/); assert.match(markup, /Session timeout/);
  assert.match(markup, /No pending changes/);
});

test('unmanaged certificate can be replaced and pending changes can be reviewed', () => {
  const d = data(); d.certificate.managed = false;
  const markup = String(settings.render({ state: { pending: { exists: true } } }, d));
  assert.match(markup, /data-act="settings\.uploadCert"/); assert.match(markup, /data-act="settings\.regenerateCert"/);
  assert.match(markup, /Clients must trust the new certificate/);
  assert.doesNotMatch(markup, /No pending changes/);
});

test('a deployment-managed console password is announced', () => {
  const d = data(); d.auth = { managed: true };
  assert.match(String(settings.render({ state: { pending: { exists: false } } }, d)), /console password is managed by the deployment/);
  d.auth = { managed: false };
  assert.doesNotMatch(String(settings.render({ state: { pending: { exists: false } } }, d)), /console password is managed by the deployment/);
});

test('only background reads carry the poll marker, and expiry returns to sign-in with a notice', async () => {
  const { api, polling } = await import('./lib.js');
  const seen = [];
  let nextStatus = 200;
  globalThis.location = { assign: (url) => seen.push(['assign', url]) };
  globalThis.fetch = async (url, init) => { seen.push([init.method, init.headers['X-Console-Poll'] || '']); return { status: nextStatus, ok: nextStatus === 200, statusText: '', json: async () => ({}) }; };
  await api('/state');
  polling.active = true;
  await api('/state'); await api('/pause', { method: 'POST' });
  nextStatus = 401;
  await assert.rejects(api('/state'));
  polling.active = false;
  assert.deepEqual(seen, [['GET', ''], ['GET', '1'], ['POST', ''], ['GET', '1'], ['assign', '/login?expired=1']]);
});
