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
