import test from 'node:test';
import assert from 'node:assert/strict';
import { scheduledMessage } from './lib.js';

test('scheduled topology uses local time, exposes UTC on hover, and explains the lock', () => {
  process.env.TZ = 'America/Chicago';
  const message = String(scheduledMessage({ topologyChanges: 1, at: '2026-11-11T19:55:00Z', target: 4 }));
  assert.match(message, /A topology change is scheduled for/);
  assert.match(message, /Changes are locked until then/);
  assert.match(message, /title="2026-11-11T19:55:00Z"/);
  assert.doesNotMatch(message, /topology v|\bv4\b/);
  assert.match(message, /(?:CST|GMT-6)/);
});
