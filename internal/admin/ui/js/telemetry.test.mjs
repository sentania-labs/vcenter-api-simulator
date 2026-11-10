// Behaviour test for the Telemetry screen's draft saves (run by
// internal/admin's Go test, or `node --test`): a fake admin API holds one
// draft; two edits fired back to back must both survive.
import test from 'node:test';
import assert from 'node:assert/strict';

let draft;
let puts;
globalThis.fetch = async (url, init = {}) => {
  const method = init.method || 'GET';
  // the server answers slowly, so an unserialized second save would read the old draft
  await new Promise((r) => setTimeout(r, method === 'PUT' ? 20 : 5));
  if (url === '/api/draft' && method === 'GET') {
    return { ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(draft)) };
  }
  if (url === '/api/draft' && method === 'PUT') {
    puts++;
    const body = JSON.parse(init.body);
    draft = { scenario: body.scenario, scope: body.scope };
    return { ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(draft)) };
  }
  return { ok: false, status: 404, statusText: 'nope', json: async () => ({ error: 'nope' }) };
};

const { save } = await import('./telemetry.js');
const ctx = { run: async (fn) => { try { return await fn(); } catch (e) { return undefined; } }, refresh: async () => {} };

test('concurrent saves each apply to the latest saved draft', async () => {
  draft = { scenario: { seed: 's', latency: {} }, scope: 'all' };
  puts = 0;
  const stale = { draft: JSON.parse(JSON.stringify(draft)) }; // what the screen drew
  await Promise.all([
    save(ctx, stale, (sc) => { sc.latency.perfQuery = { fixedMs: 200 }; }),
    save(ctx, stale, (sc) => { sc.latency.sdk = { fixedMs: 5 }; }),
    save(ctx, stale, (sc) => { sc.faults = [{ family: 'host-cpu', fault: 'system-error' }]; }, 'dashboard'),
  ]);
  assert.equal(puts, 3);
  assert.deepEqual(draft.scenario.latency, { perfQuery: { fixedMs: 200 }, sdk: { fixedMs: 5 } });
  assert.equal(draft.scenario.faults.length, 1);
  assert.equal(draft.scope, 'dashboard');
});

test('a failed save does not block the next one', async () => {
  draft = { scenario: { seed: 's' }, scope: 'all' };
  const failing = save(ctx, {}, () => { throw new Error('bad edit'); });
  const ok = save(ctx, {}, (sc) => { sc.x = 1; });
  await Promise.all([failing, ok]);
  assert.equal(draft.scenario.x, 1);
});
