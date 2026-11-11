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

test('NFS editor validates inline and stages add, edit and removal in its datacenter draft', async () => {
 const data = editorData();
 topology.actions['topo.nfsAdd'](ctx, null, null, data);
 await topology.actions['topo.nfsSave'](ctx, null, null, data);
 let markup = String(topology.render(ctx, data));
 assert.match(markup, /Use lower-case letters/); assert.match(markup, /Enter a server/);
 assert.match(markup, /absolute export path/); assert.match(markup, /positive size/);
 for (const [f, value] of Object.entries({name:'shared-nfs', remoteHost:'nfs.sim.invalid', remotePath:'/exports/sim', capacity:'1000', freeSpace:'1001'})) topology.actions['topo.nfsInput'](ctx,{dataset:{f},value});
 await topology.actions['topo.nfsSave'](ctx, null, null, data);
 markup = String(topology.render(ctx, data)); assert.match(markup,/Free space cannot exceed capacity/);
 topology.actions['topo.nfsInput'](ctx,{dataset:{f:'freeSpace'},value:'800'});
 await topology.actions['topo.nfsSave'](ctx, null, null, data);
 assert.deepEqual(saved.datacenters[0].nfs, [{name:'shared-nfs', remoteHost:'nfs.sim.invalid', remotePath:'/exports/sim', capacity:1000, freeSpace:800}]);
 data.draft.scenario = clone(saved);
 topology.actions['topo.nfsAdd'](ctx,null,null,data);
 for (const [f, value] of Object.entries({name:'shared-nfs', remoteHost:'nfs.sim.invalid', remotePath:'/exports/sim', capacity:'1000', freeSpace:'800'})) topology.actions['topo.nfsInput'](ctx,{dataset:{f},value});
 await topology.actions['topo.nfsSave'](ctx,null,null,data);
 assert.match(String(topology.render(ctx,data)),/already exists/);
 topology.actions['topo.nfsCancel'](ctx);
 topology.actions['topo.nfsEdit'](ctx,{dataset:{arg:'0'}},null,data);
 topology.actions['topo.nfsInput'](ctx,{dataset:{f:'remotePath'},value:'/exports/new'});
 await topology.actions['topo.nfsSave'](ctx,null,null,data);
 assert.equal(saved.datacenters[0].nfs[0].remotePath,'/exports/new');
 data.draft.scenario = clone(saved);
 await topology.actions['topo.nfsRemove'](ctx,{dataset:{arg:'0'}},null,data);
 assert.deepEqual(saved.datacenters[0].nfs, []);
});

test('storage edits stage host faults and preserve them on later edits', async () => {
 const data=editorData();
 data.cluster.hosts=[{name:'esx01.cluster-01.dc-01.sim.invalid',identity:'unchanged'},{name:'esx02.cluster-01.dc-01.sim.invalid',identity:'unchanged'}];
 topology.actions['topo.iscsiEdit'](ctx,null,null,data);
 for (const [group,index,f,value] of [['targets',0,'address','array.example.test'],['targets',0,'iqn','iqn.2026-01.test.example:array'],['luns',0,'name','shared-vmfs'],['luns',0,'capacity','1073741824'],['luns',0,'freeSpace','1024']]) topology.actions['topo.iscsiInput'](ctx,{dataset:{group,index:String(index),f},value});
 topology.actions['topo.iscsiPath'](ctx,{dataset:{index:'0',path:'0',f:'state'},value:'dead'});
 await topology.actions['topo.iscsiSave'](ctx,null,null,data);
 assert.equal(saved.datacenters[0].iscsi[0].luns[0].paths[0].state,'dead');
 data.draft.scenario=clone(saved);
 topology.actions['topo.iscsiEdit'](ctx,null,null,data);
 topology.actions['topo.iscsiInput'](ctx,{dataset:{group:'luns',index:'0',f:'freeSpace'},value:'2048'});
 await topology.actions['topo.iscsiSave'](ctx,null,null,data);
 assert.equal(saved.datacenters[0].iscsi[0].luns[0].freeSpace,2048);
 assert.equal(saved.datacenters[0].iscsi[0].luns[0].paths[0].state,'dead');
 data.draft.scenario=clone(saved);
 topology.actions['topo.nfsAdd'](ctx,null,null,data);
 for (const [f,value] of Object.entries({name:'shared-nfs',remoteHost:'nfs.example.test',remotePath:'/shared',capacity:'1000',freeSpace:'100'})) topology.actions['topo.nfsInput'](ctx,{dataset:{f},value});
 topology.actions['topo.nfsScope'](ctx,{value:'current'},null,data);
 topology.actions['topo.nfsHost'](ctx,{dataset:{arg:'dc-01/cluster-01/esx02'},value:'inaccessible'});
 await topology.actions['topo.nfsSave'](ctx,null,null,data);
 assert.deepEqual(saved.datacenters[0].nfs[0].clusters,['dc-01/cluster-01']);
 assert.deepEqual(saved.datacenters[0].nfs[0].inaccessibleHosts,['dc-01/cluster-01/esx02']);
 assert.equal(saved.datacenters[0].iscsi[0].luns[0].name,'shared-vmfs');
});

test('storage table keeps the performance column beside state and shows each row\'s own text', () => {
 const data=editorData();
 const q='queried: capacity and used space, I/O counters read zero';
 data.cluster.datastores=[
  {name:'vsan',type:'vSAN OSA',backing:'1 NVMe / host',mounted:3,perfService:'queried'},
  {name:'local VMFS',type:'VMFS · local',backing:'1 / host',mounted:3,perfService:'inventory only'},
  {name:'shared-nfs',type:'NFS',backing:'nfs.example.test:/shared',mounted:3,perfService:q,state:'1 host mount inaccessible'},
  {name:'shared-vmfs',type:'VMFS · iSCSI',backing:'array',mounted:3,perfService:q,state:'1 host has no working path'}];
 const rendered=String(topology.render(ctx,data));
 assert.match(rendered,/<th>State<\/th><th>MP Perf Service<\/th>/);
 for (const [name,state,perf] of [['vsan','Accessible','queried'],['local VMFS','Accessible','inventory only'],['shared-nfs','1 host mount inaccessible',q],['shared-vmfs','1 host has no working path',q]]) {
  const row=rendered.split('<tr>').find((r)=>r.includes(`>${name}</td>`));
  assert.ok(row,name); assert.ok(row.includes(`<td>${state}</td>`),name+' state'); assert.ok(row.includes(`>${perf}</td>`),name+' perf');
 }
 assert.doesNotMatch(rendered,/other rows are inventory only/);
});


test('renaming adapters and targets preserves selected paths and offers field-specific errors', async () => {
 const data=editorData(); topology.actions['topo.iscsiEdit'](ctx,null,null,data);
 topology.actions['topo.iscsiInput'](ctx,{dataset:{group:'adapters',index:'0',f:'name'},value:'iscsi-hba-1'});
 topology.actions['topo.iscsiInput'](ctx,{dataset:{group:'targets',index:'0',f:'name'},value:'array-1'});
 topology.actions['topo.iscsiInput'](ctx,{dataset:{group:'targets',index:'0',f:'iqn'},value:'bad'});
 await topology.actions['topo.iscsiSave'](ctx,null,null,data);
 let markup=String(topology.render(ctx,data));
 assert.match(markup,/Start the target name with iqn/); assert.match(markup,/aria-invalid="true"/);
 for (const [group,f,value] of [['targets','address','array.example.test'],['targets','iqn','iqn.2026-01.test.example:array'],['luns','name','shared-vmfs'],['luns','capacity','1 TB'],['luns','freeSpace','200 GB']]) topology.actions['topo.iscsiInput'](ctx,{dataset:{group,index:'0',f},value});
 await topology.actions['topo.iscsiSave'](ctx,null,null,data);
 const st=saved.datacenters[0].iscsi[0]; assert.equal(st.luns[0].target,'array-1'); assert.equal(st.luns[0].paths[0].adapter,'iscsi-hba-1'); assert.equal(st.luns[0].capacity,1024**4); assert.equal(st.luns[0].freeSpace,200*1024**3);
});

test('undo cluster deletion restores applied storage without erasing another cluster', async () => {
 const data=editorData(), key='dc-01/cluster-01';
 const applied=clone(scn); applied.datacenters[0].clusters=2;
 applied.datacenters[0].iscsi=[{cluster:key,adapters:[{name:'a',kind:'software'}],targets:[],luns:[]}];
 applied.datacenters[0].nfs=[{name:'scoped',remoteHost:'nfs.example.test',remotePath:'/share',capacity:1024,freeSpace:512,clusters:[key],inaccessibleHosts:[key+'/esx01']},{name:'shared',remoteHost:'nfs.example.test',remotePath:'/shared',capacity:1024,freeSpace:512,clusters:[key,'dc-01/cluster-02'],inaccessibleHosts:[key+'/esx02','dc-01/cluster-02/esx01']}];
 data.draft.applied=clone(applied); data.draft.scenario=clone(applied); data.draft.scenario.removed=[key]; data.draft.scenario.datacenters[0].iscsi=[]; data.draft.scenario.datacenters[0].nfs=[{...clone(applied.datacenters[0].nfs[1]),clusters:['dc-01/cluster-02'],inaccessibleHosts:['dc-01/cluster-02/esx01']}];
 await topology.actions['topo.undelete'](ctx,null,null,data);
 assert.deepEqual(saved.datacenters[0].iscsi,applied.datacenters[0].iscsi); assert.deepEqual(saved.datacenters[0].nfs.find(n=>n.name==='scoped'),applied.datacenters[0].nfs[0]); const shared=saved.datacenters[0].nfs.find(n=>n.name==='shared'); assert.deepEqual(new Set(shared.clusters),new Set(applied.datacenters[0].nfs[1].clusters)); assert.deepEqual(new Set(shared.inaccessibleHosts),new Set(applied.datacenters[0].nfs[1].inaccessibleHosts)); assert.deepEqual(saved.removed,[]);
});

test('reverting host count restores storage faults for the returned hosts', async () => {
 const data=editorData(), key='dc-01/cluster-01', host=key+'/esx04';
 data.cluster.spec.hosts=3; data.cluster.hostsApplied=4;
 const applied=clone(scn);
 applied.datacenters[0].nfs=[{name:'nfs',clusters:[key],inaccessibleHosts:[host]}];
 applied.datacenters[0].iscsi=[{cluster:key,adapters:[{name:'a'}],luns:[{name:'lun',paths:[{adapter:'a',state:'active'},{adapter:'a',host,state:'dead'}]}]}];
 data.draft.applied=clone(applied); data.draft.scenario=clone(applied);
 data.draft.scenario.datacenters[0].nfs[0].inaccessibleHosts=[];
 data.draft.scenario.datacenters[0].iscsi[0].luns[0].paths.pop();
 await topology.actions['topo.revert'](ctx,{dataset:{arg:'hosts'}},null,data);
 assert.deepEqual(saved.datacenters[0].nfs[0].inaccessibleHosts,[host]);
 assert.deepEqual(saved.datacenters[0].iscsi[0].luns[0].paths[1],{adapter:'a',host,state:'dead'});
});

test('review is clearly disabled without changes and opens review when enabled', () => {
 const data = editorData(); const empty = String(topology.render(ctx, data));
 assert.match(empty, /data-act="topo.review" disabled/); assert.match(empty, /No pending changes/);
 const withChanges = { ...ctx, state: { ...ctx.state, pending: { exists: true, topologyChanges: 1 } } };
 const active = String(topology.render(withChanges, data)); assert.match(active, /class="lat-btn lat-btn--primary" data-act="topo.review"/); assert.doesNotMatch(active, /No pending changes/);
 let route = ''; topology.actions['topo.review']({ go(value) { route = value; } }); assert.equal(route, '#/topology/review');
});
