// One job per screen. Every change the console makes uses this layout:
// questions on the left, already answered; on the right "What will happen"
// in sentences, "When should this happen?", and the one button.
//
// The simulator writes the sentences (POST /api/task/<kind>/preview) and
// makes the change (POST /api/task/<kind>), from the same plan, so the box
// cannot promise one thing while the button does another. This file only
// draws the questions.
import { api, html, raw, join, esc, btn, link, lbtn, rich, plural, timeText, ago, laterMarks, endOfToday, newPassword } from './lib.js';

const LOCAL = new Set(['when', 'at', 'show', 'copied']);
const params = (F) => Object.fromEntries(Object.entries(F).filter(([k]) => !k.startsWith('_') && !LOCAL.has(k) && !k.endsWith('_find')));

// ---- form controls --------------------------------------------------------------
function errOf(F, k) { return F._err && F._err[k] ? html`<span class="lat-hint lat-hint--bad" role="alert">${F._err[k]}</span>` : ''; }
function field(F, label, control, hint, key) {
  const bad = key && F._err && F._err[key];
  const forId = key ? 'f-' + key : (String(control).match(/ id="(f-[^"]+)"/) || [])[1];
  return html`<div class="m-field ${bad ? 'has-err' : ''}"><label ${forId ? raw(`for="${esc(forId)}"`) : ''}>${label}</label>${control}${bad ? errOf(F, key) : (hint ? html`<span class="lat-hint">${hint}</span>` : '')}</div>`;
}
const text = (F, k, ph = '', extra = '') => html`<input class="lat-input" id="f-${k}" data-f="${k}" value="${F[k] ?? ''}" placeholder="${ph}" autocomplete="off" spellcheck="false" ${raw(extra)}>`;
const numb = (F, k, min, max) => html`<input class="lat-input n" type="number" id="f-${k}" data-f="${k}" value="${F[k] ?? ''}" min="${min}" max="${max}" inputmode="numeric">`;
const sel = (F, k, options, extra = '') => html`<select class="lat-select" id="f-${k}" data-f="${k}" ${raw(extra)}>${options.map((o) => { const [v, t] = Array.isArray(o) ? o : [o, o]; return html`<option value="${v}" ${String(F[k]) === String(v) ? raw('selected') : ''}>${t}</option>`; })}</select>`;
function opts(F, k, items, cols) {
  return html`<div class="m-opts ${cols ? 'cols2' : ''}" role="radiogroup">${items.map(([v, t, d, off]) => html`<label class="m-opt ${F[k] === v ? 'is-on' : ''}"><input type="radio" name="${k}" data-f="${k}" value="${v}" ${F[k] === v ? raw('checked') : ''} ${off ? raw('disabled') : ''}><span><span class="t">${t}</span>${d ? html`<span class="d">${rich(d)}</span>` : ''}</span></label>`)}</div>`;
}
const check = (F, k, label) => html`<label class="m-check"><input type="checkbox" data-f="${k}" ${F[k] ? raw('checked') : ''}> <span>${label}</span></label>`;
// a box of tick boxes; with more than a dozen it gains a find box
function checks(F, k, values, labels) {
  const on = new Set(F[k] || []), big = values.length > 12, fk = k + '_find', q = (F[fk] || '').trim().toLowerCase();
  const shown = values.map((v, i) => [v, labels ? labels[i] : v]).filter(([v, l]) => !q || l.toLowerCase().includes(q) || on.has(v));
  return html`${big ? html`<div class="m-row" style="margin-bottom:6px"><input class="lat-input" style="max-width:220px" id="f-${fk}" data-f="${fk}" data-rr value="${F[fk] || ''}" placeholder="Find by name" aria-label="Find by name"><span class="m-small">${on.size} ticked, ${values.length} to choose from</span></div>` : ''}
    <div class="m-checks" data-scroll="${k}">${shown.length ? shown.map(([v, l]) => html`<label class="m-check"><input type="checkbox" data-fm="${k}" value="${v}" ${on.has(v) ? raw('checked') : ''}> <span>${l}</span></label>`) : html`<span class="m-small">Nothing matches.</span>`}</div>`;
}
function pwField(F, label, hint) {
  const bad = F._err && F._err.pw;
  return html`<div class="m-field ${bad ? 'has-err' : ''}"><label for="f-pw">${label}</label>
    <div class="m-inline"><input class="lat-input" style="width:240px" type="${F.show ? 'text' : 'password'}" id="f-pw" data-f="pw" value="${F.pw ?? ''}" autocomplete="new-password">${btn('Make one for me', 'genPw')}${F.pw ? btn('Copy', 'copyPw') : ''}</div>
    ${check(F, 'show', 'Show the password')}${bad ? errOf(F, 'pw') : html`<span class="lat-hint">${F.copied ? html`<b>Copied to your clipboard.</b> ` : ''}${hint}</span>`}</div>`;
}
const grid2 = (...cells) => html`<div class="m-grid m-g2">${cells}</div>`;

const VSAN_OPTS = [['ESA', 'vSAN ESA', 'The newer kind: every disk is NVMe, no disk groups.'], ['OSA', 'vSAN OSA', 'The original kind: cache and capacity disks in disk groups.']];
const ROLE_OPTS = [['full', 'Full access', 'Can read everything and is allowed to make changes, like an administrator.'], ['read', 'Read-only', 'Can read everything but is refused if it tries to change anything.']];
const ROLE = { full: 'Full access', read: 'Read-only' };
const SIZES = [['tiny', 'One small cluster', '1 cluster of 4 hosts. Quick to collect.'], ['ten', 'Ten varied clusters', '10 clusters of 4 to 16 hosts each.'], ['fifty', 'Fifty clusters', '2 datacenters, 50 clusters of 8 hosts.'], ['hundred', 'One hundred clusters', '2 datacenters, 100 clusters of 4 to 16 hosts.'], ['own', 'My own numbers', 'Choose the counts yourself.']];

const sizeFields = (F, O) => html`${field(F, 'How big is it?', html`<div class="m-inline">${numb(F, 'size', 1, 9999)}${sel(F, 'unit', ['GB', 'TB'], 'aria-label="Unit"')}</div>`, '', 'size')}${field(F, 'How full is it?', sel(F, 'full', O.full), 'Tools read this as used and free space.')}`;
const conn = (F, fieldsHtml) => html`<div class="m-field"><span class="lbl">Server address and other connection details</span>${F.edit ? '' : html`<span class="lat-hint" style="margin:0 0 6px">The simulator makes these up for you, and nothing real is contacted. You can see them on the datastore's page afterwards.</span>`}${check(F, 'edit', 'I want to choose them myself')}${F.edit ? fieldsHtml : ''}</div>`;
const filePick = (F, k, label, hint) => html`<div class="m-field ${F._err && F._err[k] ? 'has-err' : ''}"><span class="lbl">${label}</span>
  <div class="m-inline">${btn(F[k] ? 'Choose a different file…' : 'Choose file…', 'pickFile', '', { k })}<span class="m-small">${F[k] ? html`<b>${F[k + 'Name'] || 'A file'}</b>` : 'No file chosen yet'}</span></div>
  <input type="file" class="m-file" data-file="${k}" id="file-${k}" tabindex="-1" aria-hidden="true">${F._err && F._err[k] ? errOf(F, k) : html`<span class="lat-hint">${hint}</span>`}</div>`;

// ---- the questions of each task --------------------------------------------------
const FIELDS = {
  'add-cluster': (F, O) => html`${field(F, 'Which datacenter?', sel(F, 'dc', [...O.dcs, ['new', 'A new datacenter']]), F.dc === 'new' ? `It will be called ${O.newDc}.` : '', 'dc')}
    ${field(F, 'How many hosts in the cluster?', numb(F, 'hosts', O.minHosts, O.maxHosts), `From ${O.minHosts} to ${O.maxHosts}. vSAN needs at least ${O.minHosts}.`, 'hosts')}
    ${field(F, 'How many VMs on each host?', numb(F, 'vms', 0, 200), 'From 0 to 200.', 'vms')}
    <div class="m-field"><span class="lbl">Which kind of vSAN?</span>${opts(F, 'vsan', VSAN_OPTS, true)}<span class="lat-hint">Not sure? Leave it on vSAN ESA, the usual kind on vCenter 9.</span></div>
    ${field(F, 'How many clusters like this?', numb(F, 'count', 1, 50), 'Leave at 1 to add a single cluster.', 'count')}`,

  'replace-everything': (F, O) => html`<div class="m-field ${F._err && F._err.start ? 'has-err' : ''}"><span class="lbl">How big?</span>${opts(F, 'start', SIZES, true)}${errOf(F, 'start')}</div>
    ${F.start === 'own' ? html`${field(F, 'Datacenters, and clusters in each', html`<div class="m-inline">${numb(F, 'dcs', 1, 8)}<span>datacenters with</span>${numb(F, 'per', 1, 100)}<span>clusters in each</span></div>`, '', 'dcs')}
      ${field(F, 'Hosts in each cluster', html`<div class="m-inline"><span>from</span>${numb(F, 'hmin', O.minHosts, O.maxHosts)}<span>to</span>${numb(F, 'hmax', O.minHosts, O.maxHosts)}<span>hosts</span></div>`, `Use the same number twice to make every cluster the same size. vSAN needs at least ${O.minHosts}.`, 'hmin')}
      ${field(F, 'VMs on each host', html`<div class="m-inline"><span>from</span>${numb(F, 'vmin', 0, 200)}<span>to</span>${numb(F, 'vmax', 0, 200)}<span>VMs</span></div>`, '', 'vmin')}` : ''}
    ${field(F, 'Which kind of vSAN?', sel(F, 'mix', [['ESA', 'Every cluster vSAN ESA (the newer kind)'], ['OSA', 'Every cluster vSAN OSA (the original kind)'], ['mix', 'Mostly ESA, with a few OSA clusters']]))}
    ${field(F, 'Layout name (optional)', text(F, 'key'), 'The same name and sizes always build exactly the same environment, so a test can be repeated. Change the name to get a different arrangement.', 'key')}`,

  'add-hosts': (F, O) => field(F, 'How many hosts to add?', numb(F, 'n', 1, O.max), O.max > 0 ? `From 1 to ${O.max}. New hosts are the same as the ones already in the cluster.` : 'This cluster already has as many hosts as the console adds.', 'n'),

  'remove-hosts': (F, O) => html`<div class="m-field ${F._err && F._err.pick ? 'has-err' : ''}"><span class="lbl">Which hosts should go?</span>${checks(F, 'pick', O.hosts)}<span class="lat-hint">Tick one or more. At least ${O.minHosts} hosts have to stay, because vSAN needs that many.</span>${errOf(F, 'pick')}</div>`,

  hardware: (F, O) => html`${grid2(field(F, 'Processor threads per host', numb(F, 'cpu', 1, 512), '', 'cpu'), field(F, 'Network cards used by vSAN', numb(F, 'nics', 1, 8), '', 'nics'),
    field(F, 'vSAN disks per host', numb(F, 'disks', 1, 24), '', 'disks'), field(F, 'Local disks per host', numb(F, 'local', 0, 8), 'Each becomes a small local datastore.', 'local'),
    field(F, 'VMs on each host', numb(F, 'vms', 0, 200), '', 'vms'), field(F, 'Virtual disks per VM', numb(F, 'vdisks', 1, 16), '', 'vdisks'))}
    ${O.esa ? html`<div class="m-field"><span class="lbl">Space saving</span>${check(F, 'dedup', 'Use deduplication')}${check(F, 'comp', 'Use compression')}</div>` : ''}`,

  'add-iscsi': (F, O) => html`${field(F, 'Which cluster will use it?', sel(F, 'cluster', O.clusters), 'One cluster only. To share a datastore between clusters, add an NFS datastore instead.', 'cluster')}
    ${field(F, 'Name of the datastore', text(F, 'name'), 'Small letters, numbers and hyphens.', 'name')}${sizeFields(F, O)}
    ${conn(F, html`<div class="m-grid m-g2" style="margin-top:8px">${field(F, 'Storage server address', text(F, 'server'), '', 'server')}${field(F, 'Port', numb(F, 'port', 1, 65535), 'Usually 3260.', 'port')}</div>${field(F, 'Target name', text(F, 'target'), 'The name the storage server gives this storage. It starts with iqn.', 'target')}`)}`,

  'add-nfs': (F, O) => {
    const inDc = O.clusters[F.dc] || [];
    return html`${O.dcs.length > 1 ? field(F, 'Which datacenter?', sel(F, 'dc', O.dcs), '', 'dc') : ''}
      <div class="m-field ${F._err && F._err.pick ? 'has-err' : ''}"><span class="lbl">Which clusters can use it?</span>${opts(F, 'who', [['all', `Every cluster in ${F.dc}`, `${plural(inDc.length, 'cluster')} now, and any added later.`], ['some', 'Only the clusters I tick', '']], true)}
        ${F.who === 'some' ? checks(F, 'pick', inDc.map((c) => c[0]), inDc.map((c) => c[1])) : ''}${errOf(F, 'pick')}</div>
      ${field(F, 'Name of the datastore', text(F, 'name'), 'Small letters, numbers and hyphens.', 'name')}${sizeFields(F, O)}
      ${conn(F, html`<div class="m-grid m-g2" style="margin-top:8px">${field(F, 'NFS server address', text(F, 'server'), '', 'server')}${field(F, 'Shared folder on the server', text(F, 'folder'), 'Starts with a slash.', 'folder')}</div>`)}`;
  },

  'storage-size': (F, O) => sizeFields(F, O),

  'storage-disconnect': (F, O) => html`${O.clusters.length > 1 ? field(F, 'Hosts in which cluster?', sel(F, 'cluster', O.clusters)) : ''}
    <div class="m-field ${F._err && F._err.pick ? 'has-err' : ''}"><span class="lbl">Which hosts lose their connection?</span>${checks(F, 'pick', O.hosts[F.cluster] || [])}${errOf(F, 'pick')}</div>`,

  'vcenter-version': (F, O, plan) => {
    const X = (plan && plan.extra) || {};
    return html`<div class="m-field ${F._err && F._err.vc ? 'has-err' : ''}"><span class="lbl">Which version should vCenter be?</span>${opts(F, 'vc', [...O.versions.map(([v, l]) => [v, l, v === O.current ? 'What it is now.' : '']), ['other', 'A version that is not in this list', '']], true)}${errOf(F, 'vc')}</div>
      ${F.vc === 'other' ? field(F, 'Version number', text(F, 'other', 'for example 9.2.0'), 'Just the number. The simulator works out the build number and full product name to match. They are plausible, not ones a real release carries. It only changes the version tools see; behaviour that is new in that release is not imitated.', 'other') : ''}
      ${F.vc !== O.current && X.hostOpts && !X.same ? html`<div class="m-field"><span class="lbl">And the hosts?</span>${opts(F, 'hosts', X.hostOpts)}</div>` : ''}`;
  },

  'cluster-versions': (F, O, plan) => {
    const tooNew = plan && plan.extra && plan.extra.tooNew;
    const lowLabel = (O.versions.find(([v]) => v === F.low) || [0, F.low])[1];
    return html`<div class="m-field"><span class="lbl">What should the hosts run?</span>${opts(F, 'mode', [['same', 'Every host on the same version', ''], ['mixed', 'A mix of two versions', 'As if an upgrade is part-way through.']], true)}</div>
      ${F.mode === 'same' ? field(F, 'Which version?', sel(F, 'ver', O.versions), '', 'ver')
        : html`${field(F, 'Most hosts run', sel(F, 'ver', O.versions), '', 'ver')}${field(F, 'The hosts I tick below run', sel(F, 'low', O.versions), '', 'low')}
          <div class="m-field ${F._err && F._err.pick ? 'has-err' : ''}"><span class="lbl">Which hosts run ${lowLabel}?</span>${checks(F, 'pick', O.hosts)}${errOf(F, 'pick')}</div>`}
      ${tooNew ? html`<div class="m-field ${F._err && F._err.allow ? 'has-err' : ''}"><div class="lat-banner lat-banner--warn" style="margin-bottom:8px"><b>ESX ${tooNew} is newer than vCenter ${O.vcenter}.</b> A real vCenter cannot manage hosts newer than itself. Pick ESX ${O.vcenter} or older, or ${link('raise the vCenter version first', '/versions/vcenter')}.</div>${check(F, 'allow', 'Do it anyway, to test how a tool copes with an impossible setup')}${errOf(F, 'allow')}</div>` : ''}`;
  },

  'add-user': (F) => html`${field(F, 'Username', text(F, 'name'), 'A name has been suggested. Most vCenter usernames end in @vsphere.local.', 'name')}
    ${pwField(F, 'Password', 'At least 8 characters. Copy it before you press Add user: once saved it is never shown again.')}
    <div class="m-field"><span class="lbl">What may this user do?</span>${opts(F, 'role', ROLE_OPTS)}</div>`,

  'change-user': (F) => html`<div class="m-field"><span class="lbl">What may this user do?</span>${opts(F, 'role', ROLE_OPTS)}</div>${pwField(F, 'New password (optional)', 'Leave empty to keep the password it has now.')}`,

  idle: (F, O) => html`<div class="m-field ${F._err && F._err.idle ? 'has-err' : ''}"><span class="lbl">Sign a tool out after it has been quiet for</span>${opts(F, 'idle', O.choices.map((v) => [v, v, v === O.current ? 'What it is now.' : v === 'Never' ? 'Tools stay signed in until they sign out themselves.' : '']), true)}${errOf(F, 'idle')}</div>`,

  'new-certificate': (F) => field(F, 'Names and addresses tools use to reach the simulator', text(F, 'names'), 'Filled in for you from the name of this machine, the address you opened this console with and the names the certificate covers now. Add more separated by commas if tools use another name.', 'names'),

  'own-certificate': (F, O, plan) => {
    const found = plan && plan.extra && plan.extra.found;
    return html`${filePick(F, 'crt', 'Certificate file', 'The public part. The file usually ends in .crt or .pem.')}${filePick(F, 'key', 'Private key file that belongs to it', 'The secret half that was made together with the certificate. The file usually ends in .key.')}
      ${F.crt && found ? html`<div class="lat-banner ${found.state === 'bad' ? 'lat-banner--bad' : found.state === 'ok' ? 'lat-banner--ok' : ''}"><b>What the simulator found in ${F.crtName || 'the certificate file'}:</b> ${found.text}
        ${found.state === 'bad' && F.key ? html` <b>${F.keyName || 'The key file'} does not belong to this certificate.</b> Choose the key that was created together with it.` : found.state === 'ok' ? ` ${F.keyName || 'The key file'} belongs to it.` : ''}
        ${found.dropped ? html` <b>${found.dropped}</b>` : ''}${found.expired ? html` <b>${found.expired}</b>` : ''}</div>` : ''}`;
  },

  'console-password': (F) => {
    const p = (k, label, hint, ac) => field(F, label, html`<input class="lat-input" type="password" id="f-${k}" data-f="${k}" value="${F[k] ?? ''}" autocomplete="${ac}">`, hint, k);
    return html`${p('cur', 'Password you use now', '', 'current-password')}${p('pw', 'New password', 'At least 8 characters.', 'new-password')}${p('pw2', 'New password again', '', 'new-password')}`;
  },

  fault: (F, O) => {
    const which = [['perf', 'All vSAN performance data'], ['one', 'One kind of vSAN performance data'], ...(F.kind === 'slow' ? [['inv', 'Lists of clusters, hosts and VMs']] : [])];
    const showWhere = !(F.kind === 'slow' && F.which === 'inv');
    const group = (label, rows) => html`<optgroup label="${label}">${rows.map(([v, t]) => html`<option value="${v}" ${F.one === v ? raw('selected') : ''}>${t}</option>`)}</optgroup>`;
    return html`<div class="m-field"><span class="lbl">What should go wrong?</span>${opts(F, 'kind', [['slow', 'Answer slowly', 'Requests still work, but take longer.'], ['error', 'Answer with an error', 'The tool is told that something went wrong.'], ['missing', 'Say the data does not exist', 'The tool is told this kind of data is not available.'], ['timeout', 'Never answer', 'The tool waits until it gives up.'], ['perfdown', 'Performance service switched off', 'As if the vSAN performance service was never turned on.']], true)}</div>
      ${F.kind === 'slow' ? grid2(field(F, 'How much slower?', html`<div class="m-inline">${numb(F, 'ms', 1, 60000)}<span>milliseconds</span></div>`, '1,000 milliseconds is one second.', 'ms'), field(F, 'Vary it by up to', html`<div class="m-inline">${numb(F, 'jitter', 0, 60000)}<span>milliseconds</span></div>`, 'So that answers are not all slow by exactly the same amount. Use 0 for no variation.', 'jitter')) : ''}
      ${F.kind !== 'perfdown' ? field(F, 'For which requests?', sel(F, 'which', which)) : ''}
      ${F.kind !== 'perfdown' && F.which === 'one' ? field(F, 'Which kind?', html`<select class="lat-select" id="f-one" data-f="one">${group('Asked for most often', O.main)}${group('Others', O.more)}</select>`, `The kinds tools ask for most are at the top. The simulator knows ${O.kinds} kinds in all.`, 'one') : ''}
      ${showWhere ? html`<div class="m-field ${F._err && F._err.pick ? 'has-err' : ''}"><span class="lbl">On which clusters?</span>${opts(F, 'where', [['all', 'Every cluster', ''], ['some', 'Only the clusters I tick', '']], true)}${F.where === 'some' ? checks(F, 'pick', O.clusters.map((c) => c[0]), O.clusters.map((c) => c[1])) : ''}${errOf(F, 'pick')}</div>` : ''}
      ${field(F, 'For how long?', sel(F, 'until', [['off', 'Until I turn it off'], ['15m', 'For 15 minutes'], ['1h', 'For 1 hour'], ['today', 'Until the end of today']]))}`;
  },
};

// values a choice drags along with it
function follow(F, k, form) {
  const O = form.options || {};
  if (form.kind === 'add-iscsi' && k === 'cluster' && !F.edit && O.targets) F.target = O.targets[F.cluster] || F.target;
  if (form.kind === 'add-nfs' && k === 'name' && !F.edit) F.folder = '/exports/' + F.name;
  if (form.kind === 'add-nfs' && k === 'dc') F.pick = [];
  if (form.kind === 'storage-disconnect' && k === 'cluster') F.pick = [];
  if (form.kind === 'fault') {
    if (F.kind !== 'slow' && F.which === 'inv') F.which = 'perf';
    if (F.kind === 'perfdown' && k === 'kind' && !(F.pick || []).length && O.clusters.length) { F.where = 'some'; F.pick = [O.clusters[0][0]]; }
  }
}

// ---- the screen --------------------------------------------------------------------
function whenBox(ctx, F, form) {
  const marks = laterMarks();
  if (!F.when) F.when = 'now';
  if (!F.at || !marks.some((m) => m.toISOString() === F.at)) F.at = marks.length ? marks[0].toISOString() : '';
  const tool = ctx.home && ctx.home.tools.some((t) => t.state === 'connected');
  const nowNote = form.nowNote || (form.mix && tool ? 'A tool is connected. If it is in the middle of collecting, it may see a mix of old and new for one round.' : 'It takes effect as soon as you press the button.');
  const later = marks.length ? 'Nothing changes until the time you pick, and you can cancel it before then.' : 'There is no time left today to pick. Choose Right away.';
  return html`<div class="m-when ${F._err && F._err.at ? 'm-field has-err' : ''}"><span class="lbl">When should this happen?</span>
    ${opts(F, 'when', [['now', 'Right away', nowNote], ['later', 'Later today', later, !marks.length]])}
    ${F.when === 'later' && marks.length ? html`<div class="m-inline"><label for="f-at">At</label>${sel(F, 'at', marks.map((m) => [m.toISOString(), timeText(m)]))}<span class="m-small">today. It is ${timeText(new Date())} now.</span></div>${errOf(F, 'at')}` : ''}</div>`;
}

function sumBox(plan) {
  if (!plan) return html`<p class="m-muted">Working it out…</p>`;
  if (plan.idle || !plan.sum || !plan.sum.length) return html`<p class="m-muted">${plan.idle || 'Change something and this box will say what happens.'}</p>`;
  return html`<ul>${plan.sum.map((i) => html`<li class="${i.note ? 'note' : ''}">${rich(i.t)}</li>`)}</ul>`;
}

export function taskScreen(kind, query, tab, title) {
  let seq = 0, timer = null;
  const screen = {
    title: title || 'Change', tab, task: kind,
    async load(ctx) {
      const form = await api(`/task/${kind}?${new URLSearchParams(query)}`);
      const F = ctx.F;
      for (const [k, v] of Object.entries(form.values)) if (!(k in F)) F[k] = Array.isArray(v) ? [...v] : v;
      if (query.retry && ctx.retry && ctx.retry.kind === kind) {
        Object.assign(F, ctx.retry.params);
        form.intro = 'These are the values of the change that failed. Press the button to try it again, or change them first.';
      }
      ctx.retry = null;
      if (kind === 'change-user') {
        const O = form.options;
        form.intro = `${ROLE[O.role]} now. Last signed in: ${O.inUse ? `in use now by the tool at ${O.inUse}` : O.last ? ago(O.last) : 'never'}.`;
      }
      screen.title = form.title;
      document.title = `${form.title} · vCenter Simulator`;
      const plan = await api(`/task/${kind}/preview`, { body: { params: params(F) } });
      if (plan.extra && plan.extra.hosts) F.hosts = plan.extra.hosts;
      return { form, plan };
    },
    render(ctx, d) {
      const F = ctx.F, form = d.form, plan = d.plan, O = form.options || {};
      const errs = F._err || {}, nerr = Object.keys(errs).length;
      const loose = ['_'].map((k) => errs[k]).filter(Boolean).join(' ');
      const crumbs = form.crumbs, back = crumbs[crumbs.length - 1];
      const later = !form.noWhen && F.when === 'later' && F.at;
      const label = (plan && plan.go) || 'Go';
      return html`<div class="m-page"><div class="m-crumb">${join(crumbs.map((c) => link(c[0], c[1])), raw(' &rsaquo; '))} &rsaquo; ${form.title}</div>
        <div class="m-head"><div><h1>${form.title}</h1>${form.intro ? html`<p class="sub">${form.intro}</p>` : ''}</div></div>
        ${nerr ? html`<div class="lat-banner lat-banner--bad" role="alert"><b>${nerr === 1 ? 'One thing needs fixing' : nerr + ' things need fixing'} before this can go ahead.</b> ${loose || (nerr === 1 ? 'It is marked in red below.' : 'They are marked in red below.')}</div>` : ''}
        <form class="m-task" novalidate><section class="lat-card m-form">${FIELDS[kind](F, O, plan, ctx)}</section>
        <section class="lat-card m-sum"><h2>What will happen</h2><div id="sum">${sumBox(plan)}</div>${form.noWhen ? '' : whenBox(ctx, F, form)}
          <div class="m-go"><button type="button" class="lat-btn ${form.danger ? 'lat-btn--danger' : 'lat-btn--primary'}" data-act="taskGo" ${F._busy ? raw('disabled') : ''}>${F._busy ? 'Working…' : label + (later ? ` at ${timeText(F.at)}` : '')}</button>${lbtn('Cancel', back[1])}</div></section></form></div>`;
    },
    input(ctx, el) {
      const F = ctx.F;
      if (el.dataset.fm) {
        const k = el.dataset.fm, cur = new Set(F[k] || []);
        if (el.checked) cur.add(el.value); else cur.delete(el.value);
        F[k] = [...cur];
        if (F._err) delete F._err[k];
        ctx.rerender(); preview(ctx); return;
      }
      const k = el.dataset.f;
      if (!k) return;
      F[k] = el.type === 'checkbox' ? el.checked : el.value;
      if (k === 'pw') F.copied = false;
      if (F._err) { delete F._err[k]; delete F._err._; }
      if (ctx.data) follow(F, k, ctx.data.form);
      if (el.type === 'radio' || el.type === 'checkbox' || el.tagName === 'SELECT' || el.dataset.rr !== undefined) ctx.rerender();
      if (!LOCAL.has(k) && !k.endsWith('_find')) preview(ctx);
    },
    change(ctx, el) {
      const k = el.dataset.file;
      if (!k || !el.files || !el.files[0]) return;
      const file = el.files[0];
      if (file.size > 200000) { ctx.F._err = { ...(ctx.F._err || {}), [k]: 'That file is too large to be a certificate or a key. Choose the right file.' }; ctx.rerender(); return; }
      const reader = new FileReader();
      reader.onload = () => { ctx.F[k] = String(reader.result); ctx.F[k + 'Name'] = file.name; if (ctx.F._err) delete ctx.F._err[k]; ctx.rerender(); preview(ctx); };
      reader.readAsText(file);
    },
    actions: {
      genPw(ctx) { ctx.F.pw = newPassword(); ctx.F.show = true; ctx.F.copied = false; if (ctx.F._err) delete ctx.F._err.pw; ctx.rerender(); preview(ctx); },
      async copyPw(ctx) { try { await navigator.clipboard.writeText(ctx.F.pw); ctx.F.copied = true; } catch (e) { ctx.F.show = true; } ctx.rerender(); },
      pickFile(ctx, el) { const input = document.getElementById('file-' + el.dataset.k); if (input) input.click(); },
      async taskGo(ctx) { await run(ctx, false); },
    },
  };

  function preview(ctx) {
    clearTimeout(timer);
    const mine = ++seq;
    timer = setTimeout(async () => {
      try {
        const plan = await api(`/task/${kind}/preview`, { body: { params: params(ctx.F) } });
        if (mine !== seq || ctx.screen !== screen || !ctx.data) return;
        ctx.data.plan = plan;
        if (plan.extra && plan.extra.hosts && plan.extra.hosts !== ctx.F.hosts) ctx.F.hosts = plan.extra.hosts;
        ctx.rerender();
      } catch (e) { /* the button will say what is wrong */ }
    }, 160);
  }

  async function run(ctx, confirmed) {
    const F = ctx.F, form = ctx.data.form;
    const body = { params: params(F), confirmed };
    if (kind === 'fault') body.params.dayEnd = endOfToday().toISOString();
    if (!form.noWhen && F.when === 'later') {
      if (!F.at) { F._err = { at: 'There is no time left today to pick. Choose Right away.' }; ctx.rerender(); return; }
      body.at = F.at;
    }
    F._busy = true; ctx.rerender();
    let r;
    try { r = await api(`/task/${kind}`, { body }); }
    catch (e) { F._busy = false; F._err = { _: e.message }; ctx.rerender(); window.scrollTo(0, 0); return; }
    F._busy = false;
    if (r.errors) {
      F._err = r.errors; ctx.rerender();
      const first = document.querySelector('.lat-hint--bad');
      if (first) first.scrollIntoView({ block: 'center' }); else window.scrollTo(0, 0);
      return;
    }
    if (r.confirm) {
      ctx.rerender();
      const yes = await ctx.confirm({ title: r.confirm.title, ok: r.confirm.ok, danger: true, body: html`${r.confirm.body.map((p) => html`<p>${rich(p)}</p>`)}` });
      if (yes) await run(ctx, true);
      return;
    }
    const dest = r.dest || form.crumbs[form.crumbs.length - 1][1];
    if (r.outcome) {
      ctx.flash = { tone: r.outcome.tone, text: `**${r.outcome.lead || 'Done.'}** ${r.outcome.done}`, route: dest };
      ctx.marks = r.outcome.marks || [];
      if (r.outcome.marks && r.outcome.marks.some((m) => m.type === 'cluster' && m.tag === 'New')) ctx.page.cl = 1e6;
    } else { ctx.flash = null; }
    ctx.go(dest);
  }
  return screen;
}
