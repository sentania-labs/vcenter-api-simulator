import test from 'node:test';
import assert from 'node:assert/strict';
import topology from './topology.js';
import flow from './topology-flow.js';
import compat from './compat.js';
import overview from './overview.js';

const clone = (x) => JSON.parse(JSON.stringify(x));
const scn = { seed: 'osa', datacenters: [{ name: 'dc-01', clusters: 1 }], defaults: { architecture: 'osa', statsPrimary: false, hosts: 3, host: { threads: 5, domWorlds: 14, vsanPnics: 1, claimedDevices: 1, localDevices: 1 }, vms: { perHost: 1, disksPerVM: 1 } }, clusters: [] };
const load = { kinds: 74, entities: 0, series: 0, calls: 74, bytes: 0 };
const projection = { clusters: 1, osaClusters: 1, hosts: 3, vms: 3, vdisks: 3, nvme: 3, localDevices: 3, logicalCpus: 15, domWorlds: 42, datastores: 4, families: [], all: load, dashboard: load, emptyCalls: 74 };
let saved, previewParams;
globalThis.fetch = async (url, init = {}) => {
  const method = init.method || 'GET';
  let data = {};
  if (url === '/api/draft' && method === 'GET') data = { pending: { exists: false }, applied: clone(scn), scenario: clone(scn), regenerate: [] };
  else if (url === '/api/draft' && method === 'PUT') { saved = JSON.parse(init.body).scenario; data = {}; }
  else if (url === '/api/presets') data = [{ name: 'osa', title: 'Three-host OSA', description: 'OSA cluster', kind: 'topology', effect: 'replaces identities' }];
  else if (url === '/api/preview/generate') { previewParams = JSON.parse(init.body); data = { projection, selected: load, other: load, scope: 'all', warnings: [] }; }
  else if (url === '/api/draft/generate') { saved = JSON.parse(init.body); data = { ok: true }; }
  return { ok: true, status: 200, json: async () => data };
};
const ctx = { route: { parts: ['dc-01', 'cluster-01'] }, state: { topology: { version: 1, seed: 'osa' }, pending: { exists: false }, projection, selected: load, scope: 'all', clock: {} }, rerender() {}, refresh: async () => {}, refreshState: async () => {}, run: async (fn) => fn(), go() {}, toast() {} };
function editorData() {
 const row = { key: 'dc-01/cluster-01', name: 'cluster-01', datacenter: 'dc-01', architecture: 'osa', hosts: 3, hostsBefore: 3, devices: 1, warnings: 0 };
 const view = { ...row, spec: clone(scn.defaults), applied: clone(scn.defaults), sources: [], datastores: [{ name: 'vsan', type: 'vSAN OSA', backing: 'vSAN', mounted: 3, perfService: 'queried' }], hosts: [], checks: [], seedEpoch: 0, vms: 3 };
 return { tree: { datacenters: [{ name: 'dc-01', hosts: 3, clusters: [row] }], clusters: 1, hosts: 3, overridden: 0, pendingClusters: 0, warningClusters: 0 }, draft: { scenario: clone(scn), applied: clone(scn), regenerate: [] }, key: row.key, cluster: view, review: null };
}

test('OSA editor exposes architecture and primary without ESA dedup controls, and saves edits', async () => {
 const data = editorData();
 const rendered = String(topology.render(ctx, data));
 assert.match(rendered, /vSAN OSA/); assert.match(rendered, /OSA · 1×N/);
 assert.match(rendered, /vSAN architecture/); assert.match(rendered, /Stats primary online/);
 assert.doesNotMatch(rendered, /data-f="dedup"/); assert.doesNotMatch(rendered, /not editable in the console/);
 await topology.actions['topo.field'](ctx, { dataset: { f: 'architecture' }, value: 'esa' }, null, data);
 assert.equal(saved.clusters[0].architecture, 'esa');
 await topology.actions['topo.field'](ctx, { dataset: { f: 'statsPrimary' }, checked: true }, null, data);
 assert.equal(saved.clusters[0].statsPrimary, true);
 data.cluster.spec.architecture = 'esa';
 const esa = String(topology.render(ctx, data));
 assert.match(esa, /data-f="dedup"/); assert.match(esa, /data-f="compression"/);
});

test('tree labels mixed architectures and collapses only uniform runs', () => {
 const data = editorData();
 const rows = [];
 for (let i = 1; i <= 5; i++) rows.push({ key: `dc-01/esa-${i}`, name: `esa-${i}`, architecture: 'esa', hosts: 3, devices: 1, vmsPerHost: 1 });
 for (let i = 1; i <= 5; i++) rows.push({ key: `dc-01/osa-${i}`, name: `osa-${i}`, architecture: 'osa', hosts: 3, devices: 1, vmsPerHost: 1 });
 data.tree.datacenters[0].clusters = [...rows, data.tree.datacenters[0].clusters[0]];
 const text = String(topology.render(ctx, data));
 assert.match(text, /5 uniform ESA clusters/); assert.match(text, /5 uniform OSA clusters/);
 assert.match(text, /OSA · 1×N/);
});

test('generator starts with the applied OSA settings and submits deliberate choices', async () => {
 const data = await flow.generator.load(ctx);
 await new Promise((resolve) => setTimeout(resolve, 10));
 let rendered = String(flow.generator.render(ctx, data));
 assert.match(rendered, /Three-host OSA/); assert.match(rendered, /value="osa" selected/);
 assert.match(rendered, /Stats primary online/); assert.doesNotMatch(rendered, /ESA dedup/);
 assert.equal(previewParams.architecture, 'osa'); assert.equal(previewParams.statsPrimary, false);
 await flow.generator.actions['gen.create'](ctx);
 assert.equal(saved.architecture, 'osa'); assert.equal(saved.statsPrimary, false);
 flow.generator.actions['gen.architecture'](ctx, { value: 'esa' });
 await flow.generator.actions['gen.create'](ctx);
 assert.equal(saved.architecture, 'esa');
 rendered = String(flow.generator.render(ctx, data)); assert.match(rendered, /ESA dedup/);
});

test('compatibility shows OSA capture outcomes and avoids planned label', () => {
 const c = { methods: [], unlisted: [], kinds: { data: 41, empty: 17, 'invalid-argument': 9, 'not-supported': 2 }, osaKinds: { data: 30, empty: 26, 'invalid-argument': 16, 'not-supported': 2 }, profile: 'capture', verifiedBy: 'tests', lastCycle: { calls: 0 }, unverified: ['Per-VM performance remains unverified'] };
 const text = String(compat.render(ctx, c));
 assert.match(text, /OSA \(74\)/); assert.match(text, /30 of 74 wildcard families/); assert.doesNotMatch(text, /yes \(empty on ESA\)/);
});

test('overview uses the biggest non-dashboard families and OSA-aware empty subtitle', () => {
 const families = [{ family: 'disk-group', objects: 3, dashboard: false }, { family: 'capacity-disk', objects: 3, dashboard: true }, { family: 'host-cpu', objects: 15, dashboard: false }, { family: 'dom-world-cpu', objects: 42, dashboard: false }];
 const p = { ...projection, families, all: { ...load, entities: 63 } };
 const st = { ...ctx.state, projection: p, auth: { users: [] }, scenario: { latency: 'none', faultRules: 0 }, profile: {}, process: { rssBytes: 0, cores: 0 }, activity: { totals: { requests: 0 }, faults: { empty: 0, unexpected: 0, injected: 0, percent: 0 }, lastCycle: { calls: 0 }, ratePerSec: 0, p95Ms: 0 }, Top: families.slice(0, 2), events: [], api: [] };
 const text = String(overview.render({ state: st }));
 assert.match(text, /dom-world-cpu and host-cpu hold 90%/);
 assert.doesNotMatch(text, /capacity-disk and disk-group hold/);
 assert.match(text, /expected: unmodeled kinds on a cluster/);
 assert.match(text, /74 kinds × 1 cluster/);
});
