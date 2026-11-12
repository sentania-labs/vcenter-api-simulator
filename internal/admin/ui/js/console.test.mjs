// Tests of the console's screens, run by node (go test runs them when node
// is installed). Screens are functions of data, so they are drawn here with
// made-up data and read back as text.
import test from 'node:test';
import assert from 'node:assert/strict';

globalThis.window = { innerWidth: 1440, innerHeight: 900 };
globalThis.document = { title: '' };

const { html, rich, plain, laterMarks, ago, timeText, newPassword, pager, pageOf } = await import('./lib.js');
const { taskScreen } = await import('./task.js');
const home = (await import('./home.js')).default;
const clusters = (await import('./clusters.js')).default;
const storage = (await import('./storage.js')).default;
const versions = (await import('./versions.js')).default;
const users = (await import('./users.js')).default;
const security = (await import('./security.js')).default;
const faults = (await import('./faults.js')).default;
const requests = (await import('./requests.js')).default;

const text = (markup) => String(markup).replace(/<[^>]+>/g, ' ').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const clean = (markup, name) => { for (const bad of ['undefined', 'NaN', '[object Object]', 'null']) assert.ok(!text(markup).includes(bad), `${name} shows "${bad}": ${text(markup).slice(0, 400)}`); };
const now = new Date().toISOString();
const ctxOf = (homeView, F = {}) => ({ home: homeView, F, page: {}, marks: [], mark: () => '', route: { parts: ['home'], query: {} } });

const homeView = {
  now, running: true, address: 'https://vcsim.lab.example:8443', vcenter: '9.1.1',
  counts: { clusters: 11, hosts: 85, vms: 410, datastores: 98, datacenters: 1 },
  estimateAll: { requests: 759, size: '23.7 MB' }, estimateDash: { requests: 407, size: '18.0 MB' },
  tools: [{ addr: '10.20.0.31', agent: 'vSAN Host Metrics pack 2.0', user: 'ops-collector@vsphere.local', last: now, state: 'connected', asks: 'lists of clusters, hosts and VMs and vSAN performance data', unknown: 4, fault: 1, carried: true }],
  attention: [
    { k: 'bad', t: '4 kinds of request are not being answered', d: 'The tool that asked carried on collecting without them.', label: 'See them', href: '/requests?show=unknown' },
    { k: 'warn', t: 'A fault is switched on', d: 'Requests for statistics about host network cards come back with an error, on every cluster.', label: 'Turn it off', act: 'condOff', id: 7 },
    { k: 'warn', t: 'Someone else tried a wrong password 2 times', d: `From 10.20.0.44 (not the connected tool), as administrator@vsphere.local, last at {{time:${now}}}.`, label: 'See it', href: '/requests?show=denied' },
    { k: 'info', t: 'Every request is being captured in full', d: 'Turn it off when you have what you need.', label: 'Go to Requests', href: '/requests' },
  ],
  scheduled: [{ id: 3, at: now, text: 'Add 2 hosts to cluster-01' }],
  recent: [{ at: now, text: 'cluster-11 added to dc-01 with 4 hosts and 32 VMs.' }], historyCount: 5,
  connect: { username: 'administrator@vsphere.local', anyLogin: false, password: 'setup', cert: 'made', issuer: 'vcenter-api-simulator' },
  unanswered: 4, faults: 1, capturing: false, accountNeeded: false,
};

test('sentences from the simulator are escaped, then marked up', () => {
  const out = String(rich('**Done.** <script>alert(1)</script> at {{time:2026-11-12T20:35:00Z}}'));
  assert.match(out, /^<b>Done\.<\/b> &lt;script&gt;/);
  assert.ok(!out.includes('<script>'));
  assert.ok(!out.includes('{{time'));
  assert.equal(plain('**Cancelled.** done'), 'Cancelled. done');
  assert.equal(String(html`<p>${'<img onerror=x>'}</p>`), '<p>&lt;img onerror=x&gt;</p>');
});

test('"later today" offers the next five-minute mark first and nothing overnight', () => {
  const at = (h, m) => new Date(2026, 10, 12, h, m, 20);
  const marks = laterMarks(at(14, 31)).map((d) => `${d.getHours()}:${String(d.getMinutes()).padStart(2, '0')}`);
  assert.deepEqual(marks, ['14:35', '14:40', '14:45', '15:00', '15:30', '16:00', '17:00']);
  assert.deepEqual(laterMarks(at(23, 20)).map((d) => d.getMinutes()), [25, 30, 35]);
  assert.equal(laterMarks(at(23, 58)).length, 0);
});

test('times are said the way a person says them', () => {
  const base = new Date(2026, 10, 12, 14, 31);
  const back = (ms) => new Date(base.getTime() - ms).toISOString();
  assert.equal(ago(back(5000), base), 'a moment ago');
  assert.equal(ago(back(60000), base), '1 minute ago');
  assert.equal(ago(back(5 * 60000), base), '5 minutes ago');
  assert.equal(ago(back(3 * 3600000), base), '3 hours ago');
  assert.equal(ago(back(26 * 3600000), base), 'yesterday');
  assert.equal(ago(back(3 * 86400000), base), '3 days ago');
  assert.ok(timeText(new Date(2026, 10, 12, 14, 35).toISOString(), base).startsWith('2:35'));
  assert.match(newPassword(), /^Sim-[A-Za-z0-9]{4}-[A-Za-z0-9]{4}-[A-Za-z0-9]{4}$/);
});

test('lists page at a fixed size', () => {
  const ctx = { page: { x: 99 } };
  const rows = Array.from({ length: 30 }, (_, i) => i);
  assert.deepEqual(pageOf(ctx, 'x', rows, 12), [24, 25, 26, 27, 28, 29]);
  assert.match(text(pager(ctx, 'x', 30, 12)), /25 to 30 of 30/);
  assert.equal(String(pager(ctx, 'x', 5, 12)), '');
});

test('Home says what is running, who is connected and what needs attention', () => {
  const ctx = ctxOf(homeView);
  const out = home.screen(['home']).render(ctx);
  const t = text(out);
  clean(out, 'Home');
  assert.match(t, /The simulator is running/);
  assert.match(t, /pretending to be vCenter 9\.1\.1 with 11 clusters , 85 hosts and 410 VMs \. 1 tool is connected\. 4 things need your attention\./);
  assert.match(t, /It calls itself "vSAN Host Metrics pack 2\.0"\. Signed in as ops-collector@vsphere\.local/);
  assert.match(t, /4 kinds of request the simulator cannot answer \. It carried on collecting without them/);
  assert.match(t, /Show the other 1/);
  assert.match(t, /The password you chose for this account in step 2 of setup\. It is not the console password\./);
  assert.ok(!t.includes('Every request is being captured'), 'only the three most serious are shown at first');

  const fresh = home.screen(['home']).render(ctxOf({ ...homeView, tools: [], attention: [], scheduled: [] }));
  clean(fresh, 'Home, fresh');
  assert.match(text(fresh), /No tool has connected to it yet\./);
  assert.match(text(fresh), /Point your tool at it/);
  assert.match(text(fresh), /Nothing has connected yet/);

  const gone = home.screen(['home']).render(ctxOf({ ...homeView, running: false, pausedAt: now, tools: [{ ...homeView.tools[0], state: 'paused', since: now, unknown: 0, fault: 0 }] }));
  clean(gone, 'Home, paused');
  assert.match(text(gone), /The simulator is paused/);
  assert.match(text(gone), /Not connected now: the tool at 10\.20\.0\.31/);
  assert.match(text(gone), /until you paused the simulator at/);

  // a tool that signed itself out: not connected, said plainly, and not a problem
  const left = home.screen(['home']).render(ctxOf({ ...homeView, attention: [], tools: [{ ...homeView.tools[0], state: 'left', since: now, unknown: 0, fault: 0 }] }));
  clean(left, 'Home, signed out');
  assert.match(text(left), /No tool is connected right now\. One was until /);
  assert.match(text(left), /Not connected now: the tool at 10\.20\.0\.31/);
  assert.match(text(left), /Was signed in as ops-collector@vsphere\.local\. It signed out at .+\. A tool that collects on a schedule signs in again by itself for its next collection\./);
});

test('Clusters lists, finds and marks what is new', () => {
  const rows = Array.from({ length: 30 }, (_, i) => ({ key: `dc-01/cluster-${String(i + 1).padStart(2, '0')}`, name: `cluster-${String(i + 1).padStart(2, '0')}`, dc: 'dc-01', hosts: 4, vms: 32, vsan: 'vSAN ESA', version: 'ESX 9.1.1' }));
  const d = { rows, datacenters: ['dc-01'], hosts: 120, vms: 960 };
  const ctx = { ...ctxOf(homeView), mark: (type, id) => (id === 'dc-01/cluster-30' ? 'New' : '') };
  ctx.page.cl = 1e6;
  const out = clusters.screen(['clusters'], {}).render(ctx, d);
  clean(out, 'Clusters');
  assert.match(text(out), /cluster-30 New/);
  assert.match(text(out), /30 clusters, 120 hosts and 960 VMs in 1 datacenter/);
  ctx.F.q = 'cluster-07';
  assert.match(text(clusters.screen(['clusters'], {}).render(ctx, d)), /1 of 30 clusters/);
  ctx.F.q = 'nope';
  assert.match(text(clusters.screen(['clusters'], {}).render(ctx, d)), /No cluster is called "nope"/);
});

test('Requests leads with the verdict and lists problems with what happened', () => {
  const d = {
    verdict: { tone: 'bad', big: '4 kinds of request are not being answered.', line: 'The tool at 10.20.0.31 ("pack") is connected. It carried on collecting without them.' },
    counts: { all: 3, unknown: 1, fault: 1, denied: 0, ok: 1 },
    asks: [
      { id: 1, t: 'Which license this vCenter has', tech: 'POST /ls/sdk', n: 12, st: 'unknown', tone: 'bad', label: 'Not answered', short: "The tool carried on. VCF Operations' own adapter would stop", long: 'x', who: 'the tool at 10.20.0.31', last: now },
      { id: 2, t: 'vSAN performance data for host network cards', tech: 'VsanPerfQueryPerf: vsan-pnic-net', n: 11, st: 'fault', tone: 'warn', label: 'Failing on purpose', long: 'x', who: 'the tool at 10.20.0.31', cond: 7, last: now },
      { id: 3, t: 'Signing in and out', tech: 'Login', n: 14, st: 'ok', tone: 'ok', label: 'Answered', long: 'x', who: 'the tool at 10.20.0.31', last: now },
    ],
    multiTool: false, capture: { available: true, unanswered: true, full: false, debug: true, kept: 37, kinds: 4, fullKept: 0 }, reportUrl: 'https://example.test/issues/new',
  };
  const out = requests.screen(['requests'], {}).render(ctxOf(homeView), d);
  clean(out, 'Requests');
  const t = text(out);
  assert.match(t, /4 kinds of request are not being answered\./);
  assert.match(t, /Turn the fault off/);
  assert.match(t, /37 unanswered requests of 4 kinds have been captured/);
  assert.match(t, /Download unanswered requests/);
  assert.match(t, /Capturing means keeping a copy of what tools ask for\. There are three kinds\./);
  assert.match(t, /Record what tools ask about licensing: On/);
  assert.match(t, /Keeps a record of what tools ask about licenses, so a tool that will not connect can be diagnosed\. Passwords and sign-in tokens are left out\./);
  assert.match(t, /The record is in the simulator's own log, which whoever installed it can open, not in the downloads here\./);
  const off = text(requests.screen(['requests'], {}).render(ctxOf(homeView), { ...d, capture: { ...d.capture, debug: false } }));
  assert.match(off, /Record what tools ask about licensing: Off/);
  assert.match(off, /Off: what tools ask about licensing is not recorded\./);
  // the capture box is for someone who has never run the simulator from a terminal
  for (const said of [t, off]) {
    const box = said.slice(said.indexOf('Request capture'));
    for (const word of ['standard output', 'stdout', 'container log', 'license service', 'license-service', 'JSON']) assert.ok(!box.includes(word), `the capture box says "${word}"`);
  }
  assert.ok(t.indexOf('Which license') < t.indexOf('Signing in and out'), 'problems come first');
  const only = text(requests.screen(['requests'], { show: 'unknown' }).render(ctxOf(homeView), d));
  assert.ok(!only.includes('Signing in and out'));
  const none = requests.screen(['requests'], {}).render(ctxOf(homeView), { ...d, verdict: { tone: 'none', big: 'Nothing has connected yet.', line: '' }, asks: [], counts: { all: 0, unknown: 0, fault: 0, denied: 0, ok: 0 } });
  clean(none, 'Requests, empty');
  assert.match(text(none), /Point a tool at https:\/\/vcsim\.lab\.example:8443/);
});

test('the other screens draw from their data without gaps', () => {
  const ctx = ctxOf(homeView);
  const st = storage.screen(['storage'], {}).render(ctx, { rows: [{ id: 'dc-01/nfs-ds-1', name: 'nfs-ds-1', kind: 'NFS', dc: 'dc-01', usedBy: 'Every cluster in dc-01', size: '4 TB', free: '3 TB', full: 'A quarter full', cut: ['esx02 in cluster-01'] }], clusters: 11, hosts: 85, multi: false });
  clean(st, 'Storage'); assert.match(text(st), /1 host cannot reach it/);
  const empty = storage.screen(['storage'], {}).render(ctx, { rows: [], clusters: 1, hosts: 4, multi: false });
  assert.match(text(empty), /No shared storage yet/);
  const ve = versions.screen(['versions'], {}).render(ctx, { vcenter: '9.0', hosts: 85, byEsx: [{ label: '9.0', hosts: 83 }, { label: '8.0 Update 3', hosts: 2 }], rows: [{ key: 'dc-01/cluster-01', name: 'cluster-01', dc: 'dc-01', hosts: 6, label: '4 hosts on ESX 9.0, 2 on ESX 8.0 Update 3', mixed: true }], multi: false });
  clean(ve, 'Versions'); assert.match(text(ve), /Mixed 4 hosts on ESX 9\.0, 2 on ESX 8\.0 Update 3/);
  const us = users.screen(['users'], {}).render(ctx, { users: [{ name: 'administrator@vsphere.local', role: 'full' }, { name: 'ops@vsphere.local', role: 'read', last: now, inUse: ['10.20.0.31'] }], anyLogin: false });
  clean(us, 'Users'); assert.match(text(us), /In use now by the tool at 10\.20\.0\.31/); assert.match(text(us), /Never/);
  const se = security.screen(['security'], {}).render(ctx, { idle: '1 hour', consoleManaged: false, certificate: { kind: 'managed', names: ['vcsim.lab.example'], expires: now, days: 12, fingerprint: '93:A1:CC:59:6B:F1:65:2D:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:0B:6C:8E:E7' } });
  clean(se, 'Security'); assert.match(text(se), /signed out after 1 hour of no activity/); assert.match(text(se), /cannot be changed here/); assert.match(text(se), /in 12 days/);
  assert.ok(!String(se).includes('new-certificate'), 'a certificate from the deployment offers no way to replace it');
  const fa = faults.screen(['faults'], {}).render(ctx, { on: [], ready: [{ key: 'slow', name: 'Slow answers', what: 'w', tool: 't' }] });
  clean(fa, 'Faults'); assert.match(text(fa), /Nothing\. Every request is answered normally and at full speed\./);
});

test('every task draws its questions and its summary', async () => {
  const forms = {
    'add-cluster': { values: { dc: 'dc-01', hosts: 4, vms: 8, vsan: 'ESA', count: 1 }, options: { dcs: ['dc-01'], newDc: 'dc-02', minHosts: 3, maxHosts: 64 } },
    'replace-everything': { values: { start: 'own', dcs: 1, per: 10, hmin: 4, hmax: 16, vmin: 2, vmax: 8, mix: 'ESA', key: 'lab-a' }, options: { minHosts: 3, maxHosts: 64 } },
    'add-hosts': { values: { cluster: 'dc-01/cluster-01', n: 2 }, options: { max: 60 } },
    'remove-hosts': { values: { cluster: 'dc-01/cluster-01', pick: [] }, options: { hosts: ['esx01', 'esx02', 'esx03', 'esx04'], minHosts: 3 } },
    hardware: { values: { cluster: 'dc-01/cluster-01', cpu: 32, nics: 2, disks: 1, local: 1, vms: 8, vdisks: 1, dedup: true, comp: true }, options: { esa: true } },
    'add-iscsi': { values: { cluster: 'dc-01/cluster-01', name: 'iscsi-ds-1', size: 2, unit: 'TB', full: 'Half full', edit: true, server: '10.0.50.21', port: 3260, target: 'iqn.x' }, options: { clusters: [['dc-01/cluster-01', 'cluster-01 (4 hosts, dc-01)']], targets: {}, full: ['Empty', 'Half full'] } },
    'add-nfs': { values: { dc: 'dc-01', who: 'some', pick: [], name: 'nfs-ds-1', size: 4, unit: 'TB', full: 'Empty', edit: false, server: 's', folder: '/exports/nfs-ds-1' }, options: { dcs: ['dc-01', 'dc-02'], clusters: { 'dc-01': [['dc-01/cluster-01', 'cluster-01']] }, full: ['Empty'] } },
    'storage-size': { values: { id: 'dc-01/nfs-ds-1', size: 4, unit: 'TB', full: 'Empty' }, options: { full: ['Empty'] } },
    'storage-disconnect': { values: { id: 'dc-01/nfs-ds-1', cluster: 'dc-01/cluster-01', pick: [] }, options: { clusters: [['dc-01/cluster-01', 'cluster-01']], hosts: { 'dc-01/cluster-01': ['esx01'] } } },
    'vcenter-version': { values: { vc: 'other', other: '', hosts: 'all' }, options: { versions: [['9.1.1', 'vCenter 9.1.1']], current: '9.1.1' } },
    'cluster-versions': { values: { cluster: 'dc-01/cluster-01', mode: 'mixed', ver: '9.1.1', low: '9.0', pick: ['esx01'], allow: false }, options: { versions: [['9.0', 'ESX 9.0'], ['9.1.1', 'ESX 9.1.1']], hosts: ['esx01', 'esx02'], vcenter: '9.0' } },
    'add-user': { values: { name: 'tool-1@vsphere.local', pw: '', role: 'read' } },
    'change-user': { values: { name: 'a@b', role: 'full', pw: '' }, options: { role: 'full' } },
    idle: { values: { idle: 'Never' }, options: { choices: ['Never', '15 minutes'], current: 'Never' } },
    'new-certificate': { values: { names: 'vcsim.lab.example' } },
    'own-certificate': { values: { crt: 'x', key: '', crtName: 'a.crt', keyName: '' } },
    'console-password': { values: { cur: '', pw: '', pw2: '' }, noWhen: true },
    fault: { values: { kind: 'slow', which: 'one', one: 'vsan-pnic-net', ms: 200, jitter: 50, where: 'some', pick: [], until: 'off' }, options: { clusters: [['dc-01/cluster-01', 'cluster-01']], main: [['vsan-pnic-net', 'Host network cards']], more: [['cache-disk', 'Cache disks']], kinds: 69 } },
  };
  const plan = { sum: [{ t: 'Adds **cluster-12** to dc-01.' }, { t: 'Nothing else is touched.', note: true }], go: 'Go ahead', extra: { tooNew: '9.1.1', hostOpts: [['all', 'Put all 4 hosts on ESX 9.2.0 as well', 'The usual choice.']], found: { state: 'bad', text: 'valid for other.test, signed by itself, runs out 3 March 2027.' } } };
  const calls = [], realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => { calls.push(url); throw new Error('drawing a form must not call the simulator'); };
  for (const [kind, f] of Object.entries(forms)) {
    const form = { kind, title: 'A task', crumbs: [['Back', '/home']], options: {}, ...f };
    const screen = taskScreen(kind, {}, 'home', 'A task');
    const ctx = { ...ctxOf(homeView), F: { ...f.values, _err: { _: 'Something general.' } } };
    const out = screen.render(ctx, { form, plan });
    clean(out, kind);
    const t = text(out);
    assert.match(t, /What will happen/, kind);
    assert.match(t, /Adds cluster-12 to dc-01\./, kind);
    assert.match(t, /Go ahead/, kind);
    assert.match(t, /One thing needs fixing before this can go ahead\. Something general\./, kind);
    assert.equal(t.includes('When should this happen?'), !form.noWhen, kind);
    assert.deepEqual(calls, [], kind);
  }
  globalThis.fetch = realFetch;
});

// "Try again" opens the failed change's screen with ?retry=1. Every task route has to carry
// that through, including the ones that take their subject from the path.
test('"Try again" restores the failed change on every task route', async () => {
  const realFetch = globalThis.fetch;
  const reply = (data) => ({ ok: true, status: 200, headers: { get: () => 'application/json' }, json: async () => data, text: async () => JSON.stringify(data) });
  const routes = [
    [storage, ['storage', 'dc-01', 'nfs-ds-1', 'change'], 'storage-size', { id: 'dc-01/nfs-ds-1', size: 4, unit: 'TB', full: 'Empty' }, { size: 9 }],
    [storage, ['storage', 'dc-01', 'nfs-ds-1', 'problem'], 'storage-disconnect', { id: 'dc-01/nfs-ds-1', cluster: 'dc-01/cluster-01', pick: [] }, { pick: ['esx02'] }],
    [versions, ['versions', 'cluster', 'dc-01', 'cluster-01'], 'cluster-versions', { cluster: 'dc-01/cluster-01', mode: 'same', ver: '9.1.1', low: '9.0', pick: [], allow: false }, { mode: 'mixed', pick: ['esx01'] }],
    [users, ['users', 'edit', 'tool-1@vsphere.local'], 'change-user', { name: 'tool-1@vsphere.local', role: 'read', pw: '' }, { role: 'full' }],
    [clusters, ['cluster', 'dc-01', 'cluster-01', 'add-hosts'], 'add-hosts', { cluster: 'dc-01/cluster-01', n: 2 }, { n: 7 }],
    [clusters, ['clusters', 'add'], 'add-cluster', { dc: 'dc-01', hosts: 4, vms: 8, vsan: 'ESA', count: 1 }, { hosts: 12 }],
  ];
  for (const [mod, parts, kind, values, failed] of routes) {
    const asked = [];
    globalThis.fetch = async (url) => { asked.push(url); return reply(url.includes('/preview') ? { sum: [] } : { kind, title: 'A task', values, options: { role: 'read' } }); };
    const load = async (query, retry) => {
      const ctx = { ...ctxOf(homeView), retry };
      const d = await mod.screen(parts, query).load(ctx);
      return { ctx, d };
    };
    const saved = { kind, params: failed };
    const again = await load({ retry: '1' }, saved);
    for (const [k, v] of Object.entries(failed)) assert.deepEqual(again.ctx.F[k], v, `${kind}: ${k} was not restored`);
    assert.match(again.d.form.intro || '', kind === 'change-user' ? /./ : /These are the values of the change that failed/, kind);
    assert.equal(again.ctx.retry, null, `${kind}: the saved answers are used once`);
    assert.ok(asked[0].startsWith(`/api/task/${kind}?`), `${kind} asked ${asked[0]}`);
    // opened without "Try again", or for another kind of change: the form's own values
    const fresh = await load({}, saved);
    for (const k of Object.keys(failed)) assert.deepEqual(fresh.ctx.F[k], values[k], `${kind}: ${k} without retry`);
    const other = await load({ retry: '1' }, { kind: 'some-other-change', params: failed });
    for (const k of Object.keys(failed)) assert.deepEqual(other.ctx.F[k], values[k], `${kind}: ${k} with another change's answers`);
  }
  globalThis.fetch = realFetch;
});

test('turning full capture off asks first, and deletes nothing until confirmed', async () => {
  const sent = [];
  globalThis.fetch = async (url, o = {}) => { sent.push({ url, method: o.method }); return { ok: true, status: 200, headers: { get: () => 'application/json' }, json: async () => ({}), text: async () => '{}' }; };
  const asked = [];
  const run = async (answer, capture) => {
    sent.length = 0; asked.length = 0;
    const ctx = { data: { capture }, confirm: async (o) => { asked.push(o); return answer; }, act: async (f) => { await f(); } };
    await requests.screen(['requests'], {}).actions.capture(ctx, { dataset: { k: 'full' } });
  };
  const on = { available: true, full: true, fullKept: 12 };
  await run(false, on);
  assert.equal(asked.length, 1);
  assert.match(text(asked[0].body), /deletes the 12 requests captured so far/);
  assert.match(String(asked[0].body), /href="\/api\/capture\/all"/);
  assert.equal(sent.length, 0, 'declining sends nothing');
  await run(true, on);
  assert.deepEqual(sent.map((s) => [s.url, s.method]), [['/api/capture', 'PUT']]);
  await run(true, { ...on, fullKept: 0 });
  assert.equal(asked.length, 0, 'nothing captured, nothing to confirm');
  assert.equal(sent.length, 1);
  await run(true, { ...on, full: false });
  assert.equal(asked.length, 0, 'turning it on needs no confirmation');
});
