// Topology: the cluster tree and the cluster editor, plus the three
// identity-changing confirmations (Regenerate, Reset cluster seed, Delete).
// The generator and review pages live in topology-flow.js and are mounted
// under #/topology/generator and #/topology/review.
import { api, html, raw, join, badge, banner, int, bytes, hm, clone, plural } from './lib.js';
import flow from './topology-flow.js';

// ---- view state that survives re-renders ------------------------------------
const ui = { filter: '', pill: 'all', closed: {}, expanded: {}, allHosts: false, nfsForm: null, nfsDC: null, iscsiForm: null };

const mode = (ctx) => (['generator', 'review'].includes(ctx.route.parts[0]) ? ctx.route.parts[0] : 'editor');
const pad = (n) => String(n).padStart(2, '0');

// editable fields: where each lives in an override entry and in an effective spec
const FIELDS = {
	architecture: { label: 'vSAN architecture', path: ['architecture'], select: true, src: 'architecture' },
	statsPrimary: { label: 'Stats primary online', path: ['statsPrimary'], bool: true, defaultOn: true, src: 'statsPrimary', hint: 'When off, performance queries return a missing-primary fault.' },
  hosts: { label: 'Hosts', path: ['hosts'], min: 3, src: 'hosts' },
  vmsPerHost: { label: 'VMs per host', path: ['vms', 'perHost'], min: 0, src: 'vmsPerHost' },
  disksPerVM: { label: 'Virtual disks / VM', path: ['vms', 'disksPerVM'], min: 1, src: 'disksPerVM' },
  threads: { label: 'Logical CPUs', path: ['host', 'threads'], min: 1, src: 'threads', hint: 'host-cpu objects' },
  domWorlds: { label: 'DOM worlds', path: ['host', 'domWorlds'], min: 0, src: 'domWorlds', hint: 'dom-world-cpu objects' },
  claimedDevices: { label: 'NVMe devices', path: ['host', 'claimedDevices'], min: 1, src: 'claimedDevices', hint: 'claimed by vSAN' },
  localDevices: { label: 'Local devices', path: ['host', 'localDevices'], min: 0, src: 'localDevices', hint: 'local VMFS, inventory only' },
  vsanPnics: { label: 'pNICs (vSAN)', path: ['host', 'vsanPnics'], min: 1, src: 'vsanPnics', hint: 'vsan-pnic-net objects' },
  dedup: { label: 'Dedup', path: ['dedup'], bool: true, src: 'dedup' },
  compression: { label: 'Compression', path: ['compression'], bool: true, src: 'compression' },
};

function getPath(obj, path) {
  let v = obj;
  for (const p of path) { if (v == null) return undefined; v = v[p]; }
  return v;
}
function setPath(obj, path, val) {
  let o = obj;
  for (const p of path.slice(0, -1)) { o[p] = o[p] || {}; o = o[p]; }
  o[path[path.length - 1]] = val;
}
function unsetPath(obj, path) {
  const chain = [obj];
  for (const p of path.slice(0, -1)) { if (!chain[chain.length - 1]?.[p]) return; chain.push(chain[chain.length - 1][p]); }
  delete chain[chain.length - 1][path[path.length - 1]];
  for (let i = chain.length - 1; i > 0; i--) if (!Object.keys(chain[i]).length) delete chain[i - 1][path[i - 1]];
}
const overrideOf = (scn, key) => (scn.clusters || []).find((c) => c.key === key);
function mutateOverride(scn, key, fn) {
  scn.clusters = scn.clusters || [];
  let o = scn.clusters.find((c) => c.key === key);
  if (!o) { o = { key }; scn.clusters.push(o); }
  fn(o);
  const empty = (x) => Object.keys(x).every((k) => k === 'key' || (typeof x[k] === 'object' && x[k] !== null && !Array.isArray(x[k]) && !Object.keys(x[k]).length));
  if (empty(o)) scn.clusters = scn.clusters.filter((c) => c !== o);
}

// ---- data ---------------------------------------------------------------------
async function loadEditor(ctx) {
  const [tree, draft] = await Promise.all([api('/tree'), api('/draft')]);
  const all = tree.datacenters.flatMap((d) => d.clusters);
  let key = ctx.route.parts.length >= 2 ? `${ctx.route.parts[0]}/${ctx.route.parts[1]}` : '';
  if (!all.some((c) => c.key === key)) key = '';
  if (!key) {
    const pick = all.find((c) => c.state) || all.find((c) => c.override) || all[0];
    key = pick ? pick.key : '';
  }
  const cluster = key ? await api('/clusters/' + key) : null;
  let review = null;
  if (draft.pending.exists && !draft.pending.scheduled) { try { review = await api('/review'); } catch (e) { review = null; } }
  else if (draft.pending.scheduled) { try { review = await api('/review'); } catch (e) { review = null; } }
  return { tree, draft, key, cluster, review, all };
}

// ---- left: the tree -----------------------------------------------------------
function matches(c) {
  if (ui.filter && !c.name.toLowerCase().includes(ui.filter.toLowerCase()) && !`${c.datacenter}/${c.name}`.toLowerCase().includes(ui.filter.toLowerCase())) return false;
  if (ui.pill === 'overridden') return c.override;
  if (ui.pill === 'pending') return !!c.state;
  if (ui.pill === 'warnings') return c.warnings > 0;
  return true;
}
const isPlain = (c, key) => !c.state && !c.override && !c.regenerate && !c.seedReset && !c.warnings && c.key !== key;

function treeRow(c, key) {
  const sel = c.key === key;
  const hosts = c.state === 'removed' ? `${c.hostsBefore}→0` : c.hostsBefore >= 0 && c.hostsBefore !== c.hosts ? `${c.hostsBefore}→${c.hosts}` : String(c.hosts);
  let b = '';
  if (c.state === 'removed') b = badge('DELETED', 'bad');
  else if (c.state === 'new') b = badge('NEW', 'warn');
  else if (c.state === 'pending') b = badge('PENDING', 'warn');
  else if (c.regenerate) b = badge('REGEN', 'warn');
  else if (c.override) b = badge('OVERRIDE');
  else if (c.warnings) b = badge('WARN', 'warn');
  return html`<li><a class="lat-row tree-row${sel ? ' is-sel' : ''}${c.state ? ' is-pending' : ''}" href="#/topology/${c.key}" data-k="row:${c.key}" data-key="${c.key}" ${raw(sel ? 'aria-current="true"' : '')}>
    <span class="ink mono" style="font-size:12px${c.state === 'removed' ? ';text-decoration:line-through' : ''}">${c.name}</span>
    <span class="num r" style="font-size:12px">${hosts}h</span>
    <span class="num r subtle" style="font-size:11.5px">${(c.architecture || 'esa').toUpperCase()} · ${c.devices}×N</span>
    <span style="text-align:right">${b}</span></a></li>`;
}

function dcGroup(dc, key) {
  const open = !ui.closed[dc.name];
  const filtering = ui.filter || ui.pill !== 'all';
  const rows = [];
  let run = [];
  const flush = (idx) => {
    if (!run.length) return;
    if (run.length >= 5 && !ui.expanded[dc.name + ':' + run[0].key]) {
      rows.push(html`<li><button type="button" class="tree-run" data-act="topo.expand" data-arg="${dc.name}:${run[0].key}" data-k="run:${run[0].key}">${run[0].name} … ${run[run.length - 1].name} · ${run.length} uniform ${(run[0].architecture || 'esa').toUpperCase()} clusters</button></li>`);
    } else run.forEach((c) => rows.push(treeRow(c, key)));
    run = [];
  };
  for (const c of dc.clusters) {
    if (!matches(c)) continue;
    if (!filtering && isPlain(c, key)) {
      const last = run[run.length - 1];
      if (last && ((last.architecture || 'esa') !== (c.architecture || 'esa') || last.hosts !== c.hosts || last.devices !== c.devices || last.vmsPerHost !== c.vmsPerHost)) flush();
      run.push(c);
    }
    else { flush(); rows.push(treeRow(c, key)); }
  }
  flush();
  return html`<div class="lat-group${open ? ' is-open' : ''}"><button type="button" class="lat-group-summary tree-dc" data-act="topo.toggleDC" data-arg="${dc.name}" aria-expanded="${open}" data-k="dc:${dc.name}">${dc.name} <span class="lat-n">${plural(dc.clusters.length, 'cluster')} · ${int(dc.hosts)} hosts</span></button>
    ${open ? html`<ul class="lat-tree">${rows.length ? rows : html`<li class="more">No cluster matches.</li>`}</ul>` : ''}</div>`;
}

function treeCard(ctx, d) {
  const t = d.tree;
  const scheduled = !!ctx.state?.pending?.scheduled;
  const pill = (id, label, n) => html`<button type="button" class="lat-pill${ui.pill === id ? ' is-on' : ''}" data-act="topo.pill" data-arg="${id}" aria-pressed="${ui.pill === id}" data-k="pill:${id}">${label} <b>${int(n)}</b></button>`;
  return html`<section class="lat-card tree-card" aria-labelledby="tp-clusters"><h2 class="lat-card-title" id="tp-clusters">Clusters <span class="aside">${int(t.clusters)} · ${int(t.hosts)} hosts</span></h2>
    <div class="row">${scheduled ? html`<button type="button" class="lat-btn lat-btn--sm" disabled>Quick generator…</button>` : html`<a class="lat-btn lat-btn--sm" href="#/topology/generator" style="text-decoration:none">Quick generator…</a>`}<button type="button" class="lat-btn lat-btn--sm" data-act="topo.add" data-k="add-cluster" ${raw(scheduled ? 'disabled' : '')}>Add cluster</button></div>
    <div class="row"><input class="lat-input grow" value="${ui.filter}" placeholder="Filter, e.g. cluster-1   ( / )" aria-label="Filter clusters" data-k="tree-filter" data-inp="topo.filter" autocomplete="off" spellcheck="false"></div>
    <div class="pill-row">${pill('all', 'All', t.clusters)}${pill('overridden', 'Overridden', t.overridden)}${pill('pending', 'Pending', t.pendingClusters)}${pill('warnings', 'Warnings', t.warningClusters)}</div>
    <div class="tree-scroll" data-scroll="tree">${t.datacenters.map((dc) => dcGroup(dc, d.key))}</div></section>`;
}

// ---- centre: the editor ---------------------------------------------------------
function fieldValue(v, f) { return f.defaultOn ? v !== false : f.select ? (v || 'esa') : f.bool ? !!v : v; }

function input(view, id, locked) {
  const f = FIELDS[id];
  const cur = fieldValue(getPath(view.spec, f.path.length === 1 ? f.path : f.path), f);
  const was = view.applied ? fieldValue(getPath(view.applied, f.path), f) : undefined;
  const changed = view.applied && cur !== was;
  const src = (view.sources.find((s) => s.field === f.src) || {}).source || 'default';
  const marker = src === 'override' ? html` <span title="This cluster overrides the scenario default" role="img" aria-label="override">◆</span>` : '';
  let hint;
  if (changed) hint = html`was ${f.bool ? (was ? 'on' : 'off') : was} · <button type="button" class="link" data-act="topo.revert" data-arg="${id}" data-k="revert:${id}">↺ revert</button>`;
  else if (src === 'override') hint = html`override · <button type="button" class="link" data-act="topo.inherit" data-arg="${id}" data-k="inherit:${id}">↺ inherit</button>`;
  else hint = src === 'varied' ? 'varied per cluster' : f.hint ? f.hint : 'scenario default';
  const hid = `h-${id}`;
  if (f.select) {
    return html`<div><label class="lat-label" for="f-${id}">${f.label}${marker}</label><select class="lat-input${changed ? ' is-changed' : ''}" id="f-${id}" ${raw(locked ? 'disabled' : '')} data-chg="topo.field" data-f="${id}" data-k="f:${id}" aria-describedby="${hid}"><option value="esa" ${raw(cur === 'esa' ? 'selected' : '')}>vSAN ESA</option><option value="osa" ${raw(cur === 'osa' ? 'selected' : '')}>vSAN OSA</option></select><div class="lat-hint" id="${hid}">${hint}</div></div>`;
  }
  if (f.bool) {
    return html`<div><span class="lat-label">${f.label}${marker}</span><div style="padding:4px 0"><label class="chk"><input type="checkbox" ${raw(cur ? 'checked' : '')} ${raw(locked ? 'disabled' : '')} data-chg="topo.field" data-f="${id}" data-k="f:${id}" aria-describedby="${hid}"> ${cur ? 'on' : 'off'}</label></div><div class="lat-hint" id="${hid}">${hint}</div></div>`;
  }
  return html`<div><label class="lat-label" for="f-${id}">${f.label}${marker}</label><input class="lat-input num${changed ? ' is-changed' : ''}" id="f-${id}" inputmode="numeric" autocomplete="off" value="${cur}" ${raw(locked ? 'disabled' : '')} data-chg="topo.field" data-f="${id}" data-k="f:${id}" aria-describedby="${hid}"><div class="lat-hint" id="${hid}">${hint}</div></div>`;
}

function hostsTable(view) {
  const rows = view.hosts;
  const pending = rows.some((h) => h.identity !== 'unchanged');
  const badgeFor = (h) => (h.identity === 'unchanged' ? badge('unchanged') : h.identity === 'new' ? badge('new', 'warn') : badge('removed', 'bad'));
  const one = (h) => html`<tr class="${h.identity === 'unchanged' ? '' : 'is-pending'}"><td class="ink mono">${h.name.split('.')[0]}</td><td class="mono${h.moref ? '' : ' subtle'}">${h.moref || '(derived)'}</td><td class="mono${h.moref ? '' : ' subtle'}">${h.nodeUuid}</td><td class="lat-num">${h.nvme}</td><td class="lat-num">${h.lcpu}</td><td class="lat-num">${h.vms}</td><td>${badgeFor(h)}</td></tr>`;
  const out = [];
  if (ui.allHosts || rows.length <= 8) rows.forEach((h) => out.push(one(h)));
  else {
    let i = 0;
    while (i < rows.length) {
      let j = i;
      while (j < rows.length && rows[j].identity === rows[i].identity) j++;
      const grp = rows.slice(i, j);
      const keep = grp[0].identity === 'unchanged' ? 3 : 1;
      grp.slice(0, keep).forEach((h) => out.push(one(h)));
      if (grp.length > keep) {
        const rest = grp.slice(keep);
        const nm = (h) => h.name.split('.')[0];
        out.push(html`<tr class="${grp[0].identity === 'unchanged' ? '' : 'is-pending'}"><td colspan="7" class="more">${nm(rest[0])} … ${nm(rest[rest.length - 1])} · ${rest.length} more${grp[0].identity === 'unchanged' ? ', unchanged' : ' ' + grp[0].identity + ' hosts'}</td></tr>`);
      }
      i = j;
    }
  }
  return html`<div class="lat-table-scroll"><table class="lat-table"><thead><tr><th>Host</th><th>MoRef</th><th>Host / vSAN node UUID</th><th class="lat-num">NVMe</th><th class="lat-num">lCPU</th><th class="lat-num">VMs</th><th>Identity</th></tr></thead><tbody>${out}</tbody></table></div>
    ${rows.length > 8 ? html`<div><button type="button" class="link" data-act="topo.allHosts" data-k="all-hosts">${ui.allHosts ? 'Collapse the host list' : `Show all ${rows.length} hosts`}</button></div>` : ''}`;
}

function changeCount(view, regen) {
  if (!view.applied) return 0;
  let n = 0;
  for (const f of Object.values(FIELDS)) if (fieldValue(getPath(view.spec, f.path), f) !== fieldValue(getPath(view.applied, f.path), f)) n++;
  if (regen) n++;
  return n;
}

// Accept familiar binary storage units without rounding persisted values.
const sizeUnits = {B:1, KB:1024, MB:1024**2, GB:1024**3, TB:1024**4};
function parseStorageSize(value) {
  const match=String(value ?? '').trim().match(/^(\d+(?:\.\d+)?)\s*(B|KB|MB|GB|TB)?$/i);
  if (!match) return NaN;
  const n=Number(match[1])*sizeUnits[(match[2]||'B').toUpperCase()];
  return Number.isSafeInteger(n) ? n : NaN;
}
function storageSizeText(value) {
  if (value === '' || value == null) return '';
  const n=Number(value); if (!Number.isSafeInteger(n)) return String(value);
  for (const unit of ['TB','GB','MB','KB']) if (n>=sizeUnits[unit] && n%sizeUnits[unit]===0) return `${n/sizeUnits[unit]} ${unit}`;
  return `${n} B`;
}

// NFS is a datacenter setting shared by all of its clusters. Changes are
// written to the ordinary draft only after the complete form validates.
function activeNfsDC(d) { return d.draft.scenario.datacenters.find((x) => x.name === ui.nfsDC) || d.draft.scenario.datacenters.find((x) => x.name === d.cluster.datacenter); }
function nfsEditor(d, locked) {
  const dc = activeNfsDC(d);
  if (!dc) return '';
  const entries = dc.nfs || [], form = ui.nfsForm?.dc === dc.name ? ui.nfsForm : null;
  const fields = [['name', 'Name'], ['remoteHost', 'Server'], ['remotePath', 'Export path'], ['capacity', 'Capacity'], ['freeSpace', 'Free space']];
  const cluster = d.cluster, selected = form?.values.clusters || [], hostFaults = form?.values.inaccessibleHosts || [];
  const controls = form ? html`<div class="anno">Version: NFS 3</div><div class="fields c6">${fields.map(([key, label]) => html`<div><label class="lat-label" for="nfs-${key}">${label}</label><input class="lat-input${form.errors[key] ? ' is-bad' : ''}" id="nfs-${key}" data-inp="topo.nfsInput" data-f="${key}" value="${form.values[key]}" ${raw(key === 'capacity' || key === 'freeSpace' ? 'placeholder="For example, 2 TB"' : '')} aria-describedby="nfs-error-${key}" ${raw(form.errors[key] ? 'aria-invalid="true"' : '')}><div class="lat-hint" id="nfs-error-${key}" role="status">${form.errors[key] || ((key === 'capacity' || key === 'freeSpace') ? 'Use units such as GB or TB.' : '')}</div></div>`)}</div>
    ${cluster && cluster.datacenter === dc.name ? html`<label class="lat-label" for="nfs-scope">Available to</label><select id="nfs-scope" class="lat-input" data-chg="topo.nfsScope"><option value="all" ${raw(!selected.length ? 'selected' : '')}>All clusters in ${dc.name}</option><option value="current" ${raw(selected.length === 1 && selected[0] === cluster.key ? 'selected' : '')}>${cluster.name}</option>${selected.length && !(selected.length === 1 && selected[0] === cluster.key) ? html`<option value="existing" selected>Existing selection</option>` : ''}</select>
    ${!selected.length || selected.includes(cluster.key) ? html`<h3>Host mounts in ${cluster.name}</h3>${cluster.hosts.filter(h => h.identity !== 'removed').map(h => { const key = cluster.key + '/' + h.name.split('.')[0]; return html`<div class="row"><span>${h.name.split('.')[0]}</span><select class="lat-input" aria-label="Mount state for ${h.name.split('.')[0]}" data-chg="topo.nfsHost" data-arg="${key}"><option value="accessible" ${raw(!hostFaults.includes(key) ? 'selected' : '')}>Accessible</option><option value="inaccessible" ${raw(hostFaults.includes(key) ? 'selected' : '')}>Inaccessible</option></select></div>`; })}` : ''}` : ''}
    <div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="topo.nfsSave">Save to draft</button><button type="button" class="lat-btn" data-act="topo.nfsCancel">Cancel</button></div>` : '';
  return html`<h2 class="lat-card-title">NFS datastores <span class="aside">${dc.name} · available to all clusters in this datacenter unless limited to specific clusters; individual hosts can be marked unable to reach it</span></h2>
    <div class="anno">Changes join the draft and take effect only after review and apply. NFS is simulated; no server connection is made.</div>
    <label class="lat-label" for="nfs-datacenter">Datacenter</label><select id="nfs-datacenter" class="lat-input" data-chg="topo.nfsSelect" ${raw(locked ? 'disabled' : '')}>${d.draft.scenario.datacenters.map((item) => html`<option value="${item.name}" ${raw(item.name === dc.name ? 'selected' : '')}>${item.name}</option>`)}</select>
    ${entries.length ? html`<div class="lat-table-scroll"><table class="lat-table"><thead><tr><th>Name</th><th>Server and export</th><th>Capacity / free</th><th>Actions</th></tr></thead><tbody>${entries.map((n,i) => html`<tr><td>${n.name}</td><td>${n.remoteHost}:${n.remotePath}</td><td>${bytes(n.capacity)} / ${bytes(n.freeSpace)}</td><td><button type="button" class="lat-btn lat-btn--sm" data-act="topo.nfsEdit" data-arg="${i}" ${raw(locked ? 'disabled' : '')} aria-label="Edit ${n.name}">Edit</button> <button type="button" class="lat-btn lat-btn--sm" data-act="topo.nfsRemove" data-arg="${i}" ${raw(locked ? 'disabled' : '')} aria-label="Remove ${n.name}">Remove</button></td></tr>`)}</tbody></table></div>` : html`<div class="anno">No NFS datastores configured for this datacenter.</div>`}
    ${controls}${form ? '' : html`<div class="row"><button type="button" class="lat-btn lat-btn--primary lat-btn--sm" data-act="topo.nfsAdd" ${raw(locked ? 'disabled' : '')}>Add NFS datastore</button></div>`}`;
}

function validateNFS(values, entries, index) {
  const errors = {}, name = values.name.trim(), host = values.remoteHost.trim(), path = values.remotePath.trim();
  if (!/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(name)) errors.name = 'Use lower-case letters, digits and hyphens.';
  else if (entries.some((n, i) => i !== index && n.name === name)) errors.name = 'A datastore with this name already exists in the datacenter.';
  if (!host) errors.remoteHost = 'Enter a server.';
  if (!path.startsWith('/')) errors.remotePath = 'Enter an absolute export path.';
  for (const key of ['capacity', 'freeSpace']) {
    const size=parseStorageSize(values[key]);
    if (!Number.isSafeInteger(size) || (key === 'capacity' && size === 0)) errors[key] = key === 'capacity' ? 'Enter a positive size, for example 2 TB.' : 'Enter a size of zero or more, for example 200 GB.';
  }
  if (!errors.capacity && !errors.freeSpace && parseStorageSize(values.freeSpace) > parseStorageSize(values.capacity)) errors.freeSpace = 'Free space cannot exceed capacity.';
  return errors;
}

// Undoing a draft deletion brings back that cluster's applied storage while
// retaining unrelated changes in the other clusters.
function restoreClusterStorage(scn, applied, key) {
  const dcName=key.split('/')[0], dc=scn.datacenters.find(x=>x.name===dcName), original=(applied.datacenters||[]).find(x=>x.name===dcName);
  if (!dc || !original) return;
  const st=(original.iscsi||[]).find(x=>x.cluster===key);
  if (st && !(dc.iscsi||[]).some(x=>x.cluster===key)) {dc.iscsi=dc.iscsi||[]; dc.iscsi.push(clone(st));}
  for (const source of original.nfs||[]) {
    if (source.clusters?.length && !source.clusters.includes(key)) continue;
    dc.nfs=dc.nfs||[];
    let n=dc.nfs.find(x=>x.name===source.name);
    if (!n && source.clusters?.length===1 && source.clusters[0]===key) {n=clone(source); dc.nfs.push(n);}
    if (!n) continue;
    if (source.clusters?.length && n.clusters?.length && !n.clusters.includes(key)) n.clusters.push(key);
    const faults=(source.inaccessibleHosts||[]).filter(h=>h.startsWith(key+'/'));
    if (faults.length) n.inaccessibleHosts=[...new Set([...(n.inaccessibleHosts||[]),...faults])];
  }
  const override=(applied.clusters||[]).find(x=>x.key===key);
  if (override) {scn.clusters=(scn.clusters||[]).filter(x=>x.key!==key); scn.clusters.push(clone(override));}
}

// Restoring removed hosts also restores their applied storage faults, without
// replacing changes made to the surviving hosts or datastores.
function restoreHostStorage(scn, applied, key, from, to) {
  const dcName=key.split('/')[0], dc=scn.datacenters.find(x=>x.name===dcName), original=(applied.datacenters||[]).find(x=>x.name===dcName);
  if (!dc || !original || to<=from) return;
  const restored=new Set(Array.from({length:to-from},(_,i)=>`${key}/esx${pad(from+i+1)}`));
  for (const source of original.nfs||[]) {
    const n=(dc.nfs||[]).find(x=>x.name===source.name);
    if (!n || (n.clusters?.length && !n.clusters.includes(key))) continue;
    const faults=(source.inaccessibleHosts||[]).filter(h=>restored.has(h));
    if (faults.length) n.inaccessibleHosts=[...new Set([...(n.inaccessibleHosts||[]),...faults])];
  }
  const source=(original.iscsi||[]).find(x=>x.cluster===key), current=(dc.iscsi||[]).find(x=>x.cluster===key);
  if (!source || !current) return;
  for (const lun of current.luns||[]) {
    const old=(source.luns||[]).find(x=>x.name===lun.name);
    for (const path of old?.paths||[]) {
      if (restored.has(path.host) && current.adapters.some(a=>a.name===path.adapter) && !(lun.paths||[]).some(p=>p.host===path.host && p.adapter===path.adapter)) {
        lun.paths=lun.paths||[]; lun.paths.push(clone(path));
      }
    }
  }
}

function validateISCSI(v) {
  const errors={}, name=/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;
  const put=(g,i,k,msg)=>{errors[`${g}.${i}.${k}`]=msg;};
  for (const [group,items] of [['adapters',v.adapters],['targets',v.targets],['luns',v.luns]]) {
    items.forEach((x,i)=>{
      if (!name.test(x.name)) put(group,i,'name','Use lowercase letters, numbers and hyphens.');
      else if (items.some((other,j)=>j!==i && other.name===x.name)) put(group,i,'name','Choose a unique name.');
    });
  }
  v.targets.forEach((t,i)=>{
    if (!t.address?.trim()) put('targets',i,'address','Enter a server.');
    if (!t.iqn?.startsWith('iqn.')) put('targets',i,'iqn','Start the target name with iqn.');
    if (!Number.isInteger(t.port)||t.port<1||t.port>65535) put('targets',i,'port','Enter a port from 1 to 65535.');
  });
  v.luns.forEach((l,i)=>{
    if (!v.targets.some(t=>t.name===l.target)) put('luns',i,'target','Choose a target from the list.');
    if (!Number.isSafeInteger(l.number)||l.number<0) put('luns',i,'number','Enter a whole number of zero or more.');
    if (!Number.isSafeInteger(l.capacity)||l.capacity<=0||l.capacity%512!==0) put('luns',i,'capacity','Enter a positive size in multiples of 512 bytes, for example 2 TB.');
    if (!Number.isSafeInteger(l.freeSpace)||l.freeSpace<0||l.freeSpace>l.capacity) put('luns',i,'freeSpace','Enter a size from zero up to the capacity.');
    const keys=l.paths.map(p=>p.adapter+'/'+(p.host||''));
    if (!l.paths.length) put('luns',i,'paths','Add at least one path.');
    else if (l.paths.some(p=>!v.adapters.some(a=>a.name===p.adapter))) put('luns',i,'paths','Choose an existing adapter for each path.');
    else if (new Set(keys).size!==keys.length) put('luns',i,'paths','Each adapter and host can appear only once.');
  });
  return errors;
}

function iscsiEditor(d, locked) {
  const cluster = d.cluster, dc = d.draft.scenario.datacenters.find(x => x.name === cluster.datacenter);
  if (!dc) return '';
  const existing = (dc.iscsi || []).find(x => x.cluster === cluster.key);
  const form = ui.iscsiForm?.cluster === cluster.key ? ui.iscsiForm : null;
  const field = (group, index, key, label, value) => {
    const id=`iscsi-${group}-${index}-${key}`, error=form?.errors?.[`${group}.${index}.${key}`];
    const hint=key==='name' ? 'Use lowercase letters, numbers and hyphens.' : key==='iqn' ? 'Start the target name with iqn.' : (key==='capacity' || key==='freeSpace') ? 'For example, 2 TB or 200 GB.' : '';
    return html`<label for="${id}">${label}<input id="${id}" class="lat-input${error ? ' is-bad' : ''}" data-inp="topo.iscsiInput" data-group="${group}" data-index="${index}" data-f="${key}" value="${value ?? ''}" aria-describedby="${id}-hint" ${raw(error ? 'aria-invalid="true"' : '')}><span id="${id}-hint" class="lat-hint" role="status">${error || hint}</span></label>`;
  };
  return html`<h2 class="lat-card-title">iSCSI storage</h2>
    ${existing && !form ? html`<p>${existing.luns.length} shared datastore${existing.luns.length === 1 ? '' : 's'} on ${existing.adapters.length} adapter${existing.adapters.length === 1 ? '' : 's'} per host.</p>` : ''}
    ${!form ? html`${existing ? '' : html`<div class="anno">No iSCSI storage configured for this cluster.</div>`}<div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="topo.iscsiEdit" ${raw(locked ? 'disabled' : '')}>${existing ? 'Edit iSCSI storage' : 'Add iSCSI storage'}</button>${existing ? html` <button type="button" class="lat-btn" data-act="topo.iscsiDelete" ${raw(locked ? 'disabled' : '')}>Remove iSCSI storage</button>` : ''}</div>` : html`
      <div class="anno">These settings apply to every host in ${cluster.name}. Changes wait in the draft until you review and apply them.</div>
      <h3>Adapters per host</h3>${form.value.adapters.map((a,i) => html`<div class="row">${field('adapters',i,'name','Name',a.name)}<label>Type<select class="lat-input" data-chg="topo.iscsiInput" data-group="adapters" data-index="${i}" data-f="kind"><option value="software" ${raw(a.kind === 'software' ? 'selected' : '')}>Software</option><option value="hardware" ${raw(a.kind === 'hardware' ? 'selected' : '')}>Hardware</option></select></label><button type="button" class="lat-btn" data-act="topo.iscsiRemove" data-group="adapters" data-index="${i}">Remove</button></div>`)}
      <div class="row"><button type="button" class="lat-btn" data-act="topo.iscsiAdd" data-group="adapters">Add adapter</button></div>
      <h3>Targets</h3>${form.value.targets.map((t,i) => html`<div class="row">${field('targets',i,'name','Name',t.name)}${field('targets',i,'address','Server',t.address)}${field('targets',i,'iqn','Target name',t.iqn)}${field('targets',i,'port','Port',t.port || 3260)}<button type="button" class="lat-btn" data-act="topo.iscsiRemove" data-group="targets" data-index="${i}">Remove</button></div>`)}
      <div class="row"><button type="button" class="lat-btn" data-act="topo.iscsiAdd" data-group="targets">Add target</button></div>
      <h3>Datastores and paths</h3>${form.value.luns.map((l,i) => html`<div class="lat-card"><div class="row">${field('luns',i,'name','Datastore name',l.name)}<label>Target<select class="lat-input${form.errors?.[`luns.${i}.target`] ? ' is-bad' : ''}" data-chg="topo.iscsiInput" data-group="luns" data-index="${i}" data-f="target" ${raw(form.errors?.[`luns.${i}.target`] ? 'aria-invalid="true"' : '')}><option value="">Choose a target</option>${form.value.targets.map(t=>html`<option value="${t.name}" ${raw(l.target===t.name?'selected':'')}>${t.name}</option>`)}</select><span class="lat-hint">${form.errors?.[`luns.${i}.target`]||''}</span></label>${field('luns',i,'number','LUN number',l.number)}${field('luns',i,'capacity','Capacity',l.capacity)}${field('luns',i,'freeSpace','Free space',l.freeSpace)}<label>Path policy<select class="lat-input" data-chg="topo.iscsiInput" data-group="luns" data-index="${i}" data-f="policy"><option value="VMW_PSP_RR" ${raw(!l.policy || l.policy === 'VMW_PSP_RR' ? 'selected' : '')}>Round robin</option><option value="VMW_PSP_FIXED" ${raw(l.policy === 'VMW_PSP_FIXED' ? 'selected' : '')}>Fixed</option><option value="VMW_PSP_MRU" ${raw(l.policy === 'VMW_PSP_MRU' ? 'selected' : '')}>Most recently used</option></select></label></div>
        ${l.paths.map((p,j) => html`<div class="row"><label>Adapter<select class="lat-input${form.errors?.[`luns.${i}.paths`] ? ' is-bad' : ''}" ${raw(form.errors?.[`luns.${i}.paths`] ? 'aria-invalid="true"' : '')} data-chg="topo.iscsiPath" data-index="${i}" data-path="${j}" data-f="adapter">${form.value.adapters.map(a => html`<option value="${a.name}" ${raw(p.adapter === a.name ? 'selected' : '')}>${a.name}</option>`)}</select></label><label>Host<select class="lat-input" data-chg="topo.iscsiPath" data-index="${i}" data-path="${j}" data-f="host"><option value="" ${raw(!p.host ? 'selected' : '')}>All hosts</option>${cluster.hosts.filter(h => h.identity !== 'removed').map(h => { const key = cluster.key + '/' + h.name.split('.')[0]; return html`<option value="${key}" ${raw(p.host === key ? 'selected' : '')}>${h.name.split('.')[0]}</option>`; })}</select></label><label>State<select class="lat-input" data-chg="topo.iscsiPath" data-index="${i}" data-path="${j}" data-f="state">${['active','standby','dead','disabled'].map(state => html`<option value="${state}" ${raw(p.state === state ? 'selected' : '')}>${state}</option>`)}</select></label><button type="button" class="lat-btn" data-act="topo.iscsiRemovePath" data-index="${i}" data-path="${j}">Remove path</button></div>`)}
        ${form.errors?.[`luns.${i}.paths`] ? html`<div class="lat-hint is-bad" role="alert">${form.errors[`luns.${i}.paths`]}</div>` : ''}<div class="row"><button type="button" class="lat-btn" data-act="topo.iscsiAddPath" data-index="${i}">Add path</button><button type="button" class="lat-btn" data-act="topo.iscsiRemove" data-group="luns" data-index="${i}">Remove datastore</button></div></div>`)}
      <div class="row"><button type="button" class="lat-btn" data-act="topo.iscsiAdd" data-group="luns">Add datastore</button></div>
      ${form.error ? html`<div role="alert" class="lat-hint is-bad">${form.error}</div>` : ''}<div class="row"><button type="button" class="lat-btn lat-btn--primary" data-act="topo.iscsiSave">Save to draft</button><button type="button" class="lat-btn" data-act="topo.iscsiCancel">Cancel</button></div>`}`;
}

function editorCard(ctx, d) {
  const v = d.cluster;
  if (!v) return html`<section class="lat-card"><div class="empty">No clusters. Use Quick generator… to create a topology.</div></section>`;
  const sched = ctx.state.pending && ctx.state.pending.scheduled;
  const removed = v.state === 'removed';
  const locked = sched || removed;
  const regen = d.draft.regenerate.includes(v.key);
  const n = Math.max(v.state === 'pending' ? 1 : 0, changeCount(v, regen) + (d.draft.scenario.clusters || []).filter((c) => c.key === v.key && c.seedEpoch > ((overrideOf(d.draft.applied, v.key) || {}).seedEpoch || 0)).length);
  const stateBadge = removed ? badge('deleted in draft', 'bad') : v.state === 'new' ? badge('new cluster', 'warn') : n ? badge(`${n} unapplied change${n === 1 ? '' : 's'}`, 'warn') : '';
  const pending = ctx.state.pending && ctx.state.pending.exists;
  const canReview = pending && !sched;
  const vsan = v.datastores.find((x) => x.type.startsWith('vSAN'));
  return html`<section class="lat-card" aria-labelledby="ed-title">
    <fieldset ${raw(sched ? 'disabled' : '')} style="border:0;padding:0;margin:0;min-width:0">
    <div class="row">
      <h2 class="page-title mono" id="ed-title" style="font-size:16px;white-space:nowrap;margin:0">${v.name} <span class="muted" style="font-size:12px;font-weight:400">${v.datacenter}</span></h2>
      ${badge(v.spec.architecture === 'osa' ? 'vSAN OSA' : 'vSAN ESA')}${stateBadge}${regen ? badge('new identity generation', 'warn') : ''}${v.seedEpoch > 0 ? badge(`seed epoch ${v.seedEpoch}`, 'warn') : ''}
      <span class="grow"></span><span class="anno" style="white-space:nowrap">${v.moref ? html`MoRef <span class="mono">${v.moref}</span> · ` : ''}${v.vsanUuid ? html`UUID <span class="mono">${v.vsanUuid}</span>` : html`<span class="subtle">identities are derived at apply</span>`}</span>
    </div>
    ${sched ? banner('info', html`A change is scheduled. Editing is locked until it lands or you cancel it from the banner above.`) : ''}
    ${removed ? banner('bad', html`<b>${v.name} is deleted in the draft.</b> Nothing changes until you review and apply. <button type="button" class="link" data-act="topo.undelete" data-k="undelete">Undo delete</button> restores it with its identities.`) : ''}
    <h2 class="lat-card-title">Cluster <span class="aside">◆ marks a value this cluster overrides; the rest come from the scenario defaults or its per-cluster draws</span></h2>
    <div class="fields c6">
      ${input(v, 'hosts', locked)}${input(v, 'vmsPerHost', locked)}${input(v, 'disksPerVM', locked)}
      ${input(v, 'architecture', locked)}${input(v, 'statsPrimary', locked)}
      ${v.spec.architecture === 'osa' ? '' : html`${input(v, 'dedup', locked)}${input(v, 'compression', locked)}`}
      <div><span class="lat-label">vDS</span><div class="mono ink" style="padding:5px 0">${v.vds}</div><div class="lat-hint">${v.portgroups} port groups · ${v.datacenter}</div></div>
    </div>
    <h2 class="lat-card-title">Storage <span class="aside">vSAN is what the MP collects; NFS and iSCSI datastores answer performance queries with capacity and used space (I/O counters read zero); local VMFS is inventory only</span></h2>
    <div class="lat-table-scroll"><table class="lat-table"><thead><tr><th>Datastore</th><th>Type</th><th>Backing</th><th class="lat-num">Mounted</th><th>State</th><th>MP Perf Service</th></tr></thead><tbody>
      ${v.datastores.map((s) => html`<tr><td class="ink mono">${s.name}</td><td>${badge(s.type)}</td><td class="mono">${s.backing}</td><td class="lat-num">${s === vsan && v.applied && v.hostsApplied !== v.spec.hosts && !removed ? `${v.hostsApplied} → ${v.spec.hosts}` : s.mounted}</td><td>${s.state || 'Accessible'}</td><td class="wrap ${(s.perfService || '').startsWith('queried') ? 'ink' : 'muted'}">${s.perfService}</td></tr>`)}</tbody></table></div>
    <div class="anno">${v.spec.architecture === 'osa' ? 'OSA deduplication and compression are not modeled from the capture. ' : ''}Fibre Channel is not simulated.</div>
    ${iscsiEditor(d, locked)}
    ${nfsEditor(d, locked)}
    <h2 class="lat-card-title">Per-host defaults <span class="aside">applies to every host in ${v.name}</span></h2>
    <div class="fields c6">${['threads', 'domWorlds', 'claimedDevices', 'localDevices', 'vsanPnics'].map((id) => input(v, id, locked))}</div>
    <h2 class="lat-card-title">Hosts <span class="aside">${v.state === 'removed' ? 0 : v.spec.hosts} after apply · identities derive from seed ${ctx.state.topology.seed} + cluster + host ordinal</span></h2>
    ${hostsTable(v)}
    <div class="row" style="margin-top:auto;border-top:1px solid var(--line-soft);padding-top:10px">
      <button type="button" class="lat-btn" data-act="topo.discard" ${raw(pending && !sched ? '' : 'disabled')} data-k="discard">Discard…</button>
      <button type="button" class="lat-btn lat-btn--primary" data-act="topo.review" ${raw(canReview ? '' : 'disabled')} data-k="review">Review &amp; Apply…</button>${canReview ? '' : html`<span class="anno">${sched ? 'Changes are scheduled' : 'No pending changes'}</span>`}
      <span class="grow"></span><span class="anno" style="white-space:nowrap">Changes identities:</span>
      <button type="button" class="lat-btn" data-act="topo.regenerate" ${raw(locked ? 'disabled' : '')} data-k="regen">Regenerate…</button>
      <button type="button" class="lat-btn" data-act="topo.resetSeed" ${raw(locked ? 'disabled' : '')} data-k="reset-seed">Reset cluster seed…</button>
      <button type="button" class="lat-btn lat-btn--danger" style="margin-left:6px" data-act="topo.delete" ${raw(locked ? 'disabled' : '')} data-k="delete">Delete cluster…</button>
    </div></fieldset></section>`;
}

// ---- right: impact -----------------------------------------------------------------
function impactCard(ctx, d) {
  const st = ctx.state, v = d.cluster, rv = d.review, pend = st.pending && st.pending.exists;
  const sel = st.selected;
  const row = (label, a, b, fmt = int) => html`<tr><td>${label}</td><td class="lat-num">${fmt(a)}</td><td class="lat-num">${pend && rv ? fmt(b) : '—'}</td></tr>`;
  const approx = (n) => '~' + bytes(n);
  const others = d.tree.clusters - 1;
  const pendingOthers = d.tree.pendingClusters - (v && v.state ? 1 : 0);
  const banners = [];
  if (v && v.state === 'removed') {
    banners.push(banner('bad', html`<b>Removes the whole cluster</b> from the next topology: its hosts, VMs, NVMe devices and ${int(v.entities)} perf entities.`));
  } else if (v && (v.state || d.draft.regenerate.includes(v.key))) {
    const kept = v.hosts.filter((h) => h.identity === 'unchanged').length;
    const added = v.hosts.filter((h) => h.identity === 'new').length;
    const gone = v.hosts.filter((h) => h.identity === 'removed').length;
    const regen = d.draft.regenerate.includes(v.key);
    const architectureChanged = v.applied && (v.applied.architecture || 'esa') !== (v.spec.architecture || 'esa');
    if (architectureChanged) banners.push(banner('warn', html`<b>Performance identities change.</b> The review lists the performance entities added and removed by changing architecture.`));
    if (regen) banners.push(banner('warn', html`<b>Every identity in ${v.name} is replaced.</b> The cluster UUID, ${v.hosts.length} hosts, their NVMe disks and ${v.vms} VMs get new UUIDs; VCF Operations sees new objects and the old ones go stale.`));
    else if (kept) banners.push(banner('ok', html`<b>Host and VM identities kept.</b> ${kept} host${kept === 1 ? '' : 's'} keep MoRefs, host UUIDs, vSAN node UUIDs, disk UUIDs and VM instanceUuids.${others > 0 ? html` The other ${others - pendingOthers} cluster${others - pendingOthers === 1 ? ' is' : 's are'} untouched.` : ''}`));
    if (added && !regen) {
      const one = d.tree.pendingClusters === 1 && rv ? rv.identities : null;
      const vms = one ? (one.find((r) => r.label.startsWith('VM')) || {}).added : null;
      const nv = one ? (one.find((r) => r.label.startsWith('NVMe')) || {}).added : null;
      banners.push(banner('info', html`<b>Adds ${added} host${added === 1 ? '' : 's'}</b> with new derived identities. VCF Operations will discover ${added} host${added === 1 ? '' : 's'}${vms != null ? html`, ${int(vms)} VMs and ${int(nv)} NVMe devices` : ''}.`));
    }
    if (gone && !regen) banners.push(banner('warn', html`<b>Removes ${gone} host${gone === 1 ? '' : 's'}.</b> The remaining hosts and VMs keep their MoRefs and UUIDs; the removed hosts' objects go stale in VCF Operations.`));
    if (v.seedEpoch > 0 && v.state) banners.push(banner('warn', html`<b>Cluster seed reset.</b> Every synthetic sample of ${v.name}, including its history, changes. Identities do not.`));
    banners.push(banner('', html`Telemetry history and seeds elsewhere are preserved. ${added ? html`New hosts start reporting at the next bucket, <span class="mono">${hm(st.clock.nextBoundary)}:00Z</span>.` : html`The change lands at the next bucket, <span class="mono">${hm(st.clock.nextBoundary)}:00Z</span>, if you apply at the boundary.`}`));
  }
  return html`<section class="lat-card" aria-labelledby="im-t"><h2 class="lat-card-title" id="im-t">Impact of pending edits</h2>
    <table class="lat-table"><thead><tr><th></th><th class="r">Now</th><th class="r">After</th></tr></thead><tbody>
      ${row('Hosts', rv ? rv.hostsBefore : st.projection.hosts, rv ? rv.hostsAfter : 0)}
      ${row('Perf entities', rv ? rv.before.entities : sel.entities, rv ? rv.after.entities : 0)}
      ${row('Series / cycle', rv ? rv.before.series : sel.series, rv ? rv.after.series : 0)}
      ${row('PerfQuery calls', rv ? rv.before.calls : sel.calls, rv ? rv.after.calls : 0)}
      ${row('Bytes / cycle', rv ? rv.before.bytes : sel.bytes, rv ? rv.after.bytes : 0, approx)}</tbody></table>
    <div class="anno">Whole topology, ${((d.draft && d.draft.scope) || st.scope) === 'dashboard' ? 'dashboard kinds' : 'all modeled kinds'}, projected not measured. Calls scale with clusters × kinds, not hosts; extra load lands in response size.</div>
    ${pend ? banners : html`<div class="anno">No pending edits. Change a field and the effect on identities appears here.</div>`}
    <h2 class="lat-card-title" style="margin-top:4px">Consistency checks</h2>
    <div class="stack" style="font-size:12px">${(v ? v.checks : []).map((c) => html`<div class="row"><span>${badge(c.severity, c.severity === 'info' ? '' : c.severity)}</span><span class="muted">${c.severity === 'ok' ? c.text : html`<b>${c.title}.</b> ${c.text}`}</span></div>`)}</div>
  </section>`;
}

// ---- confirmations (mockup 12, real numbers) ---------------------------------------
function counts(v) {
  const hosts = v.hosts.filter((h) => h.identity !== 'removed');
  const nvme = hosts.reduce((s, h) => s + h.nvme, 0);
  const vdisks = v.vms * (v.spec.vms.disksPerVM || 1);
  return { hosts: hosts.length, pendingHosts: hosts.filter((h) => h.identity === 'new').length, nvme, vms: v.vms, vdisks, ent: v.entities };
}
const joins = html`<li>Joins the pending draft: nothing changes until you review it and apply it at a bucket boundary.</li>`;

async function post(ctx, path, key, ok) {
  await ctx.run(() => api(path, { body: { cluster: key } }), ok);
  await ctx.refresh();
}

const actions = {
  review(ctx) { ctx.go('#/topology/review'); },
  expand(ctx, el) { ui.expanded[el.dataset.arg] = true; ctx.rerender(); },
  toggleDC(ctx, el) { ui.closed[el.dataset.arg] = !ui.closed[el.dataset.arg]; ctx.rerender(); },
  pill(ctx, el) { ui.pill = el.dataset.arg; ctx.rerender(); },
  filter(ctx, el) { ui.filter = el.value; ctx.rerender(); },
  allHosts(ctx) { ui.allHosts = !ui.allHosts; ctx.rerender(); },
  nfsSelect(ctx, el) { ui.nfsDC = el.value; ui.nfsForm = null; ctx.rerender(); },
  nfsAdd(ctx, el, ev, data) { ui.nfsForm = { dc:activeNfsDC(data).name, index:-1, values:{name:'', remoteHost:'', remotePath:'', capacity:'', freeSpace:''}, errors:{} }; ctx.rerender(); },
  nfsEdit(ctx, el, ev, data) { const dc = activeNfsDC(data); const index = Number(el.dataset.arg); ui.nfsForm = { dc:dc.name, index, values:{...dc.nfs[index], capacity:storageSizeText(dc.nfs[index].capacity), freeSpace:storageSizeText(dc.nfs[index].freeSpace)}, errors:{} }; ctx.rerender(); },
  nfsInput(ctx, el) { if (ui.nfsForm) ui.nfsForm.values[el.dataset.f] = el.value; },
  nfsScope(ctx, el, ev, data) { const v = ui.nfsForm?.values; if (!v || el.value === 'existing') return; v.clusters = el.value === 'all' ? [] : [data.cluster.key]; if (v.clusters.length) v.inaccessibleHosts = (v.inaccessibleHosts || []).filter(k => k.startsWith(data.cluster.key + '/')); ctx.rerender(); },
  nfsHost(ctx, el) { const v = ui.nfsForm?.values; if (!v) return; v.inaccessibleHosts = (v.inaccessibleHosts || []).filter(k => k !== el.dataset.arg); if (el.value === 'inaccessible') v.inaccessibleHosts.push(el.dataset.arg); },
  iscsiEdit(ctx, el, ev, data) {
    const c = data.cluster, dc = data.draft.scenario.datacenters.find(x => x.name === c.datacenter);
    const current = (dc.iscsi || []).find(x => x.cluster === c.key);
    ui.iscsiForm = {cluster:c.key, value:clone(current || {cluster:c.key, adapters:[{name:'adapter-1',kind:'software'}], targets:[{name:'target-1',address:'',iqn:'',port:3260}], luns:[{name:'',target:'target-1',number:0,capacity:'',freeSpace:'',policy:'VMW_PSP_RR',paths:[{adapter:'adapter-1',state:'active'}]}]}), error:'', errors:{}};
    for (const l of ui.iscsiForm.value.luns) {l.capacity=storageSizeText(l.capacity); l.freeSpace=storageSizeText(l.freeSpace);}
    ctx.rerender();
  },
  iscsiInput(ctx, el) {
    const f=ui.iscsiForm; if (!f) return;
    const {group,index,f:key}=el.dataset, item=f.value[group][Number(index)], before=item[key]; item[key]=el.value;
    if (key==='name' && before!==el.value && (group==='adapters' || group==='targets')) {
      for (const l of f.value.luns) {
        if (group==='targets' && l.target===before) l.target=el.value;
        if (group==='adapters') for (const p of l.paths) if (p.adapter===before) p.adapter=el.value;
      }
      const selector=group==='adapters' ? '[data-chg="topo.iscsiPath"][data-f="adapter"]' : '[data-chg="topo.iscsiInput"][data-f="target"]';
      for (const select of globalThis.document?.querySelectorAll(selector)||[]) for (const option of select.options) if (option.value===before) {option.value=el.value; option.textContent=el.value;}
    }
  },
  iscsiPath(ctx, el) { const f=ui.iscsiForm; if (f) f.value.luns[Number(el.dataset.index)].paths[Number(el.dataset.path)][el.dataset.f]=el.value; },
  iscsiAdd(ctx, el) { const f=ui.iscsiForm; if (!f) return; const g=el.dataset.group; if (g==='adapters') f.value.adapters.push({name:`adapter-${f.value.adapters.length+1}`,kind:'software'}); if (g==='targets') f.value.targets.push({name:`target-${f.value.targets.length+1}`,address:'',iqn:'',port:3260}); if (g==='luns') f.value.luns.push({name:'',target:f.value.targets[0]?.name || '',number:0,capacity:'',freeSpace:'',policy:'VMW_PSP_RR',paths:[{adapter:f.value.adapters[0]?.name || '',state:'active'}]}); ctx.rerender(); },
  iscsiRemove(ctx, el) { ui.iscsiForm?.value[el.dataset.group].splice(Number(el.dataset.index),1); ctx.rerender(); },
  iscsiAddPath(ctx, el) { const f=ui.iscsiForm; if (f) f.value.luns[Number(el.dataset.index)].paths.push({adapter:f.value.adapters[0]?.name || '',state:'active'}); ctx.rerender(); },
  iscsiRemovePath(ctx, el) { ui.iscsiForm?.value.luns[Number(el.dataset.index)].paths.splice(Number(el.dataset.path),1); ctx.rerender(); },
  iscsiCancel(ctx) { ui.iscsiForm=null; ctx.rerender(); },
  async iscsiDelete(ctx, el, ev, data) { const scn=clone(data.draft.scenario), dc=scn.datacenters.find(x=>x.name===data.cluster.datacenter); dc.iscsi=(dc.iscsi||[]).filter(x=>x.cluster!==data.cluster.key); await save(ctx,scn); },
  async iscsiSave(ctx, el, ev, data) {
    const f=ui.iscsiForm; if (!f || f.cluster!==data.cluster.key) return;
    const v=clone(f.value);
    for (const t of v.targets) t.port=Number(t.port || 3260);
    for (const l of v.luns) { l.number=Number(l.number); l.capacity=parseStorageSize(l.capacity); l.freeSpace=parseStorageSize(l.freeSpace); for (const p of l.paths) if (!p.host) delete p.host; }
    f.errors=validateISCSI(v);
    if (!v.adapters.length || !v.targets.length || !v.luns.length || Object.keys(f.errors).length) { f.error='Add an adapter, target and datastore, then correct the highlighted fields.'; ctx.rerender(); return; }
    const scn=clone(data.draft.scenario), dc=scn.datacenters.find(x=>x.name===data.cluster.datacenter); dc.iscsi=(dc.iscsi||[]).filter(x=>x.cluster!==v.cluster); dc.iscsi.push(v);
    if (await save(ctx,scn)) {ui.iscsiForm=null; ctx.rerender();}
  },
  nfsCancel(ctx) { ui.nfsForm = null; ctx.rerender(); },
  async nfsSave(ctx, el, ev, data) {
    const form = ui.nfsForm; if (!form || form.dc !== activeNfsDC(data).name) return;
    const scn = clone(data.draft.scenario), dc = scn.datacenters.find((x) => x.name === form.dc);
    dc.nfs = dc.nfs || []; form.errors = validateNFS(form.values, dc.nfs, form.index);
    if (Object.keys(form.errors).length) { ctx.rerender(); return; }
    const n = {name:form.values.name.trim(), remoteHost:form.values.remoteHost.trim(), remotePath:form.values.remotePath.trim(), capacity:parseStorageSize(form.values.capacity), freeSpace:parseStorageSize(form.values.freeSpace), ...(form.values.clusters?.length ? {clusters:form.values.clusters} : {}), ...(form.values.inaccessibleHosts?.length ? {inaccessibleHosts:form.values.inaccessibleHosts} : {})};
    if (form.index < 0) dc.nfs.push(n); else dc.nfs[form.index] = n;
    if (await save(ctx, scn)) { ui.nfsForm = null; ctx.rerender(); }
  },
  async nfsRemove(ctx, el, ev, data) { const scn = clone(data.draft.scenario), dc = scn.datacenters.find((x) => x.name === activeNfsDC(data).name); dc.nfs.splice(Number(el.dataset.arg), 1); ui.nfsForm = null; await save(ctx, scn); },

  async field(ctx, el, ev, data) {
    const id = el.dataset.f, f = FIELDS[id], key = data.key;
    let val;
    if (f.bool) val = el.checked;
    else if (f.select) val = el.value;
    else {
      const txt = el.value.trim();
      if (!/^\d+$/.test(txt) || Number(txt) < f.min) {
        el.classList.add('is-bad');
        el.setAttribute('aria-invalid', 'true');
        ctx.toast(`${f.label} must be a whole number of at least ${f.min}.`, 'bad');
        return;
      }
      val = Number(txt);
    }
    const scn = clone(data.draft.scenario);
    mutateOverride(scn, key, (o) => setPath(o, f.path, val));
    await save(ctx, scn);
  },
  async revert(ctx, el, ev, data) {
    const f = FIELDS[el.dataset.arg], key = data.key;
    const was = getPath(overrideOf(data.draft.applied, key) || {}, f.path);
    const scn = clone(data.draft.scenario);
    mutateOverride(scn, key, (o) => (was === undefined ? unsetPath(o, f.path) : setPath(o, f.path, was)));
    if (el.dataset.arg === 'hosts') restoreHostStorage(scn, data.draft.applied, key, data.cluster.spec.hosts, data.cluster.hostsApplied);
    await save(ctx, scn);
  },
  async inherit(ctx, el, ev, data) {
    const f = FIELDS[el.dataset.arg];
    const scn = clone(data.draft.scenario);
    mutateOverride(scn, data.key, (o) => unsetPath(o, f.path));
    await save(ctx, scn);
  },
  async add(ctx, el, ev, data) {
    const scn = clone(data.draft.scenario);
    const cur = data.cluster ? data.cluster.datacenter : (scn.datacenters[0] || {}).name;
    const dc = scn.datacenters.find((x) => x.name === cur) || scn.datacenters[0];
    dc.clusters = (dc.clusters || 0) + 1;
    const key = `${dc.name}/cluster-${pad(dc.clusters)}`;
    if (await save(ctx, scn)) ctx.go('#/topology/' + key);
  },
  async undelete(ctx, el, ev, data) {
    const scn = clone(data.draft.scenario);
    scn.removed = (scn.removed || []).filter((k) => k !== data.key);
    restoreClusterStorage(scn, data.draft.applied, data.key);
    await save(ctx, scn);
  },
  async discard(ctx, el, ev, data) {
    const p = ctx.state.pending || {};
    const n = (p.topologyChanges || 0) + (p.telemetryChanges || 0) + (p.settingsChanges || 0);
    const ok = await ctx.confirm({
      title: 'Discard the pending draft?',
      body: html`<ul><li>Drops all ${n || ''} unapplied change${n === 1 ? '' : 's'} in the draft: every cluster${p.telemetryChanges ? ', latency and fault edits' : ''}${p.settingsChanges ? ', and user settings' : ''}, not only ${data.key.split('/')[1]}.</li>
        <li>The current settings keep serving. A discarded draft cannot be restored.</li></ul>`,
      confirmLabel: 'Discard draft', danger: true,
    });
    if (!ok) return;
    await ctx.run(() => api('/draft', { method: 'DELETE' }), 'Draft discarded.');
    await ctx.refresh();
  },
  async regenerate(ctx, el, ev, data) {
    const v = data.cluster, c = counts(v);
    const ok = await ctx.confirm({
      title: `Regenerate identities for ${v.key}?`, wide: true,
      body: html`<ul>
        <li>New identity generation for this cluster. The seed is unchanged, so sample shapes stay the same.</li>
        <li><b>Replaced:</b> the cluster UUID · ${int(c.hosts)} host MoRefs, host UUIDs and vSAN node UUIDs · ${int(c.nvme)} NVMe UUIDs · ${int(c.vms)} VMs · ${int(c.vdisks)} virtual disks · ${int(c.ent)} perf entities.</li>
        <li><b>Kept:</b> the cluster name, datacenter, vDS, every setting in the editor and every seed. VCF Operations sees ${int(c.hosts)} new hosts, and the old ones go stale.</li>
        ${joins}</ul>`,
      confirmLabel: 'Add regenerate to draft', danger: true,
    });
    if (ok) await post(ctx, '/draft/regenerate', v.key, `Regenerate of ${v.name} added to the draft.`);
  },
  async resetSeed(ctx, el, ev, data) {
    const v = data.cluster;
    const ok = await ctx.confirm({
      title: `Give ${v.key} a new cluster seed?`, wide: true,
      body: html`<ul>
        <li>Moves ${v.name} to a new telemetry seed epoch (${v.seedEpoch} → ${v.seedEpoch + 1}). The global seed ${ctx.state.topology.seed} and the other clusters are not touched.</li>
        <li><b>Replaced:</b> every synthetic sample for ${v.name}, including its 24 h of history. <b>Kept:</b> every identity: UUIDs and MoRefs do not change.</li>
        <li>Earlier runs on ${v.name} can no longer be replayed bit-for-bit.</li>
        ${joins}</ul>`,
      confirmLabel: 'Add seed reset to draft', danger: true,
    });
    if (ok) await post(ctx, '/draft/reset-seed', v.key, `Seed reset of ${v.name} added to the draft.`);
  },
  async delete(ctx, el, ev, data) {
    const v = data.cluster, c = counts(v), st = ctx.state;
    const kinds = st.selected.kinds, nc = data.tree.clusters;
    const ok = await ctx.confirm({
      title: `Delete ${v.key} and everything in it?`, wide: true, typed: v.key,
      body: html`<ul>
        <li><b>Removed:</b> ${int(c.hosts)} hosts${v.applied && v.hostsApplied !== c.hosts ? ` (${v.hostsApplied} applied, ${c.hosts} in the draft)` : ''}, ${int(c.vms)} VMs, ${int(c.nvme)} NVMe devices and ${int(c.ent)} perf entities. ${v.datastores[0].name} and local VMFS datastores are deleted. iSCSI storage and NFS mounts for this cluster are also removed.</li>
        <li>Per cycle: ${nc} → ${nc - 1} clusters, and PerfQuery calls drop from ${int(kinds * nc)} to ${int(kinds * (nc - 1))}.</li>
        <li>Other clusters, hosts and VMs keep their IDs. Deleted objects are removed. Re-adding them with the same seed and names restores their previous IDs, provided the registry has not been regenerated.</li>
        ${joins}<li>Until you apply, <b>Undo delete</b> on this cluster, or Discard, brings it back as it is applied now; its pending edits are dropped.</li></ul>`,
      confirmLabel: `Delete ${v.key}`, danger: true,
    });
    if (ok) await post(ctx, '/draft/delete', v.key, `Delete of ${v.name} added to the draft.`);
  },
};

async function save(ctx, scn) {
  try {
    await api('/draft', { method: 'PUT', body: { scenario: scn } });
    await ctx.refresh();
    return true;
  } catch (e) {
    ctx.toast(e.message, 'bad');
    await ctx.reload();
    return false;
  }
}

// ---- screen --------------------------------------------------------------------------
let current = 'editor';
const flowActions = Object.assign({}, flow.generator.actions, flow.review.actions);

export default {
  id: 'topology',
  title: 'Topology',
  get pollMs() { return current === 'editor' ? 6000 : (flow[current] && flow[current].pollMs) || 0; },
  async load(ctx) {
    current = mode(ctx);
    return current === 'editor' ? loadEditor(ctx) : flow[current].load(ctx);
  },
  render(ctx, data) {
    current = mode(ctx);
    if (current !== 'editor') return flow[current].render(ctx, data);
    return html`<div class="topo-grid">${treeCard(ctx, data)}${editorCard(ctx, data)}${impactCard(ctx, data)}</div>`;
  },
  mounted(ctx, data, page) { if (current !== 'editor' && flow[current].mounted) flow[current].mounted(ctx, data, page); },
  actions: Object.assign({}, flowActions, Object.fromEntries(Object.entries(actions).map(([k, f]) => ['topo.' + k, f]))),
  keys(ctx, ev, data, typing) {
    if (current !== 'editor') { if (flow[current].keys) flow[current].keys(ctx, ev, data, typing); return; }
    if (ev.key === '/' && !typing) { ev.preventDefault(); document.querySelector('[data-k="tree-filter"]')?.focus(); return; }
    if (ev.key === 'Escape' && ev.target.matches('[data-k="tree-filter"]')) { ui.filter = ''; ctx.rerender(); return; }
    if ((ev.key === 'ArrowDown' || ev.key === 'ArrowUp') && ev.target.matches('.tree-row, .tree-run, .tree-dc')) {
      const rows = [...document.querySelectorAll('.tree-row, .tree-run, .tree-dc')];
      const i = rows.indexOf(ev.target) + (ev.key === 'ArrowDown' ? 1 : -1);
      if (rows[i]) { ev.preventDefault(); rows[i].focus(); }
    }
  },
};
