# Scenario file reference

The scenario file is the operator's contract with the simulator: one YAML
file that says what the simulated vCenter contains, who may log in, which
certificate it serves, and how it misbehaves. Change the file and restart, or
edit a running simulator in the control console (below). Examples live in
[`examples/scenarios/`](../examples/scenarios/).

```sh
vcsimulator -scenario examples/scenarios/ten-clusters.yaml -data ./data -listen 0.0.0.0:8443
```

NFS and iSCSI entries describe simulated inventory and do not connect to the named
server. Capacity and free space are bytes. The configured name must be unique within
its datacenter, and the remote path must be absolute. In the console, the
Storage screen adds, resizes, disconnects and removes shared datastores; it
asks for a name, a size and how full, and invents the connection details.

See [`storage.yaml`](../examples/scenarios/storage.yaml) for an NFS mount
failure and a near-full shared VMFS datastore whose iSCSI paths are all down
on one host. NFS `clusters` is optional; when omitted the mount is available
to every cluster in the datacenter. `inaccessibleHosts` contains host keys
such as `dc-01/cluster-01/esx02`. For iSCSI, configure one entry per cluster:
named software or hardware adapters, targets, and LUNs. Each LUN creates a
shared VMFS datastore with the same name. A path names an adapter and state
(`active`, `standby`, `dead`, or `disabled`); an optional host key overrides
that adapter's state for one host. A host with no active or standby path has
an inaccessible datastore mount and a LUN in `lostCommunication`. Capacity
and free space are bytes; LUN capacity must be a multiple of 512. Fault host
keys must name hosts that exist in the resolved scenario. Adapter, target and
datastore names are stable keys; changing a datastore name creates a new
datastore identity. Round robin is
the default policy; `VMW_PSP_FIXED` and `VMW_PSP_MRU` are also accepted.

A storage change in the console rebuilds the simulated vCenter and drops
existing sessions; clients sign in again. MoRefs and other identities of
unchanged datastores are retained. "Disconnect some hosts" writes
`inaccessibleHosts` for NFS and a dead per-host path for iSCSI.
Removing a cluster removes its iSCSI configuration and its NFS scope and
host-fault references. An NFS datastore limited only to removed clusters is
removed rather than made available everywhere. Removing hosts removes
fault and path overrides for them; a LUN with no paths left is removed.
Every change validates the resulting scenario. On startup, a previously
saved console state with dangling storage references is repaired and the
removal is logged; unrelated invalid settings still prevent startup.

The NFS v3 summary, NAS information and accessible mount shape were observed
on vCenter 9.1.1 build 25784172. The sanitized structural fixture is in
[`fixtures/storage`](../fixtures/storage/vcenter-9.1.1-structure.json).
The iSCSI topology, multipath policies, fault behavior, and hardware adapter
shape are plausible simulations, not verified against the lab's iSCSI hosts.
NFS 4.1, storage I/O, discovery or rescan operations, CHAP, live path
transitions and non-vSAN storage health are not modeled. The eight storage
counters observed on the lab's NFS datastore are exposed for simulated NFS
and iSCSI datastores. Capacity and used samples track configured space;
provisioned and unshared are approximated as used, and I/O rates are zero.
Historical space samples use the current scenario values rather than a history
of earlier edits. REST datastore inventory reflects the same type, capacity,
free space and accessibility as SOAP. This inventory model does not contact a storage
server or create real LUNs and filesystems.

## Identity: seed, registry, regenerate

Every identity a client keys on (the vCenter instance uuid, vSAN cluster
uuids, host node and hardware uuids, disk vsanUuids and ScsiLun uuids, VM
uuids and instanceUuids, VM namespace paths) is derived, never stored: a
UUID v5 of (namespace, kind, serial). The namespace comes from `seed`; the
serial comes from the **identity registry**, `<data>/registry.json`, which
maps every entity's logical key (`host/dc-01/cluster-03/esx02`) to the
serial it was first given. Serials are never reused.

| You do | uuids | MoRefs |
|---|---|---|
| restart on the same data directory | unchanged | unchanged |
| add clusters, hosts or VMs | existing ones unchanged; new ones get new serials | existing ones unchanged; new objects are created last (VMs that rebalance onto a new host keep their MoRef: they are created in their own order and moved afterwards) |
| add devices to an existing host | existing ones unchanged | existing ones unchanged; each new local datastore takes MoRefs from fresh space past every reservation |
| remove entities | the rest unchanged; a removed cluster, host, VM or device keeps its serial for a later re-add | the rest unchanged; a removed object's references stay reserved for it |
| change `seed` | refused at start unless `-regenerate` is given; then every identity changes | all change |
| change `telemetrySeed` | unchanged (every metric value changes) | unchanged |

UUIDs are derived from the seed and registry serials. The registry also
reserves the vcsim counter interval for each inventory creation step (one
per datacenter, cluster, host, VM and local datastore), so a removal leaves
a gap, an addition is created in fresh space, and existing objects keep
their MoRefs across applies and restarts. A cluster, host, VM or device
removed and later added back under the same seed and name gets its previous
UUIDs and MoRefs back, unless its registry entries were retired by an
explicit regeneration. `-regenerate` discards the registry; the same seed
reproduces the same identities, so change the seed to mint new ones.

## Reference

```yaml
seed: ten-clusters          # required; names the identity namespace
telemetrySeed: ""           # optional; metric values only (default: seed)

datacenters:                # each gets one vDS (vds-01) with four portgroups
  - name: dc-01             # lower-case letters, digits, hyphens
    clusters: 10            # cluster-01 .. cluster-10
    nfs:                    # optional shared NFS v3 inventory; see storage.yaml for scope and iSCSI
      - name: shared-nfs
        remoteHost: nfs.sim.invalid
        remotePath: /exports/sim
        capacity: 1000000000000
        freeSpace: 800000000000

defaults:                   # per-cluster defaults (built-in values shown)
  hosts: 4                  # at least 3: vSAN needs three hosts
  architecture: esa         # esa (default) or osa; OSA disables dedup and compression
  statsPrimary: true        # false simulates an unavailable vSAN stats primary
  dedup: true               # ESA dedup: the *-esa-dedup-* families carry data
  compression: true
  host:
    threads: 32             # host-cpu entities: cpu-0..31 plus the aggregate
    domWorlds: 66           # dom-world-cpu entities per host
    vsanPnics: 2            # vSAN uplinks on the vDS: vsan-pnic-net entities
    claimedDevices: 1       # NVMe devices in the ESA storage pool
    localDevices: 1         # NVMe devices with a local VMFS datastore
  vms:
    perHost: 2
    disksPerVM: 1           # virtual-disk and vscsi entities per VM

vary:                       # each generated cluster draws its own values with
  hosts: [4, 6, 8, 8, 12]   #   its own seed; a list is a weighted choice
  vmsPerHost: {min: 2, max: 8}
  claimedDevices: [1, 1, 2]
  domWorlds: {min: 40, max: 90}
  threads: [32, 64]

clusters:                   # explicit overrides win over draws and defaults;
  - key: dc-01/cluster-03   #   the key must name a generated cluster
    hosts: 16
    dedup: false
    host: {domWorlds: 140}
    seedEpoch: 0            # above 0: this cluster gets its own telemetry seed
                            #   (every sample changes, no identity does)

removed:                    # generated clusters left out (the console's Delete);
  - dc-01/cluster-07        #   the others keep their numbers and the removed
                            #   cluster's identities stay reserved

auth:                       # no users: the console's first-run set-up chooses the password
  # anyLogin: true          # explicit opt-out: accept any non-empty pair
  users:
    - name: administrator@vsphere.local
      password: VMware1!
    - name: readonly@vsphere.local
      passwordEnv: VCSIM_READONLY_PASSWORD   # read from the environment at start
      role: read-only                        # administrator (default) or read-only

tls:                        # a provided pair, or a self-signed one kept in dir
  certFile: /etc/vcsimulator/tls/tls.crt
  keyFile: /etc/vcsimulator/tls/tls.key
  # dir: ./data/tls         # default <data>/tls; created once, kept across restarts
  # names: [vcsim.lab.example]   # extra SANs for a new self-signed certificate

faults:                     # SOAP faults on /vsanHealth; families, methods and
  - family: vsan-pnic-net   #   cluster keys are checked at start, a typo is an error
    clusters: [dc-01/cluster-03]   # ... on these clusters (empty: all)
    fault: invalid-argument # invalid-argument | not-supported | system-error | timeout
    from: 2026-11-07T14:00:00Z     # optional; snapped to 5-minute buckets
    to: 2026-11-07T15:00:00Z
  - method: QueryVsanManagedDisks  # or a served method by name
    fault: system-error

latency:                    # fixed delay plus uniform jitter, milliseconds
  perfQuery: {fixedMs: 100, jitterMs: 50}       # every VsanPerfQueryPerf
  families:
    host-domowner: {fixedMs: 300, jitterMs: 100}  # overrides perfQuery
  sdk: {fixedMs: 5, jitterMs: 5}                 # every /sdk call
```

### Fault kinds and what the pack does with them

The reference management pack treats `InvalidArgument` and `NotSupported`
as "feature absent" and skips the family quietly; any other fault is a
collection problem it reports. `timeout` holds the response past the pack's
120-second request timeout, which it reports as a failed cluster.

| `fault` | SOAP fault | Shape |
|---|---|---|
| `invalid-argument` | `InvalidArgument`, `invalidProperty=entityRefId` | as the live service rejects an unsupported family |
| `not-supported` | `NotSupported` with the PMem message | as the live service rejects the PMem families |
| `system-error` | `SystemError` | a collection problem |
| `timeout` | none | no answer for 125 s |

Methods that accept a `method:` fault: `VsanPerfQueryNodeInformation`,
`VsanClusterGetConfig`, `QueryVsanManagedDisks`,
`VsanQueryVcClusterHealthSummary`.

### Time

Faults with `from`/`to` are evaluated against the Performance Service's
clock and snapped outward to five-minute bucket boundaries (`from` down,
`to` up): the fault is active for every bucket the window touches, and
starts and stops on bucket boundaries, as a scenario edit would. A window
that lies inside one bucket is active for that whole bucket. Everything
else in the file is read once at start.

Latency and timeout holds are applied before the request reaches the
service, so a hold on one cluster or family never delays another cluster's
queries (vcsim answers each managed object under a lock, and the
Performance Manager is one object for every cluster).

## Sizing a scenario

Entities per cluster, by family (the fan-out the live capture showed):

| Family | Entities |
|---|---|
| cluster-* | 1 |
| host-*, cmmds-net, rdt-*, system-mem, vsan-cpu, vsan-memory, vsan-host-net, zdom-vtx, vsan-tcpip-stats, vsan-vnic-net, vnic-rdt-network-latency | hosts |
| vsan-pnic-net | hosts × vsanPnics |
| host-cpu | hosts × (threads + 1) |
| dom-world-cpu | hosts × domWorlds |
| vsan-esa-disk-layer, vsan-esa-disk-scsifw | hosts × claimedDevices |
| virtual-machine | hosts × vms.perHost |
| virtual-disk, vscsi | VMs × disksPerVM |

Ten clusters of the example scenario (82 hosts, 446 VMs) build in under a
second and the process uses tens of megabytes; nothing is stored per metric
sample.

## OSA evidence and limits

[`osa.yaml`](../examples/scenarios/osa.yaml) selects OSA for a three-host
cluster. Set `statsPrimary: false` to reproduce the missing stats primary:
sample queries return `NotFound` with the
`com.vmware.vsan.perfsvc.fault.masternotfound` message. With the primary
online, populated families use five-minute CSV samples, and the observed
empty and faulting families retain their captured outcomes.

The sanitized primary-online walk contains 74 wildcard family calls: **30
returned samples, 26 returned an empty wildcard echo, and 18 returned typed
faults**. Thus 56 calls answered successfully, but 26 of those successes had
no samples. The simulator implements the 30 populated families, with their
captured labels and entity patterns:

`cache-disk`, `capacity-disk`, `cluster-domclient`, `cluster-domcompmgr`,
`cluster-domowner`, `cluster-esa-dedup-domclient-io`,
`cluster-rdt-network-latency`, `cluster-resync`, `cmmds-net`, `disk-group`,
`dom-world-cpu`, `host-cpu`, `host-domclient`, `host-domcompmgr`,
`host-domowner`, `host-esa-dedup-domclient-io`, `host-vsansparse`,
`lsom-world-cpu`, `rdt-net`, `rdt-network-latency`, `system-mem`,
`vnic-rdt-network-latency`, `vsan-cluster-capacity`, `vsan-cpu`,
`vsan-distribution`, `vsan-host-net`, `vsan-memory`, `vsan-pnic-net`,
`vsan-tcpip-stats`, and `vsan-vnic-net`.

The 26 empty outcomes are `cluster-esa-dedup-domcompmgr-io`,
`cluster-esa-dedup-domowner-io`, `cluster-esa-dedup-store-chunksvc`,
`cluster-esa-dedup-store-hashsvc`, `cluster-remotedomclient`,
`cluster-zdom-top-stats`, `computeHost-remotedomclient`,
`host-esa-dedup-domcompmgr-io`, `host-esa-dedup-domowner-io`,
`host-esa-dedup-store-chunksvc`, `host-esa-dedup-store-hashsvc`,
`host-remotedomclient`, `host-zdom-top-stats`, `ioinsight`, `virtual-disk`,
`virtual-machine`, `vsan-direct-cluster`, `vsan-direct-host`,
`vsan-esa-disk-layer`, `vsan-esa-disk-scsifw`, `vsan-file-service`,
`vsan-iscsi-host`, `vsan-iscsi-lun`, `vsan-iscsi-target`, `vscsi`, and
`zdom-vtx`. Those empty outcomes are evidence of no samples in this walk,
not evidence of populated data for these families.

The walk did not contain live samples for VM-level families, stretched
clusters, multiple disk groups per host, dedup enabled, or populated iSCSI.
Those cases are schema-only and are not validated as live OSA behavior.
OSA models one disk-group identity per host; disk mappings were empty in
both walks. Capacity and telemetry values are synthetic, not recordings of
the source cluster.

## Editing a running simulator: the control console

The console (`-admin`, default `http://127.0.0.1:9090/`) edits the same
model without touching the file. Each change is made when its button is
pressed, or at a time later the same day. The served
scenario is saved in `<data>/console.json` without the `auth` and `tls`
sections. Console user changes are stored separately as password hashes in
`<data>/vcenter-users.json`; they take precedence over scenario and environment
credentials until the scenario file changes (accounts made when the deployment
supplies none are kept even then). Changing or removing a user ends
that user's SOAP and REST sessions. Read-only users can query inventory and
telemetry, but mutation calls are refused. On the next start the saved state
resumes unless the scenario file changed since, in which case the file wins.

| Console action | Scenario effect | Identities |
|---|---|---|
| Add a cluster | `datacenters[].clusters` + N, and an entry under `clusters:` with its hosts, VMs per host and vSAN type | existing unchanged |
| Add hosts, Change host hardware | the cluster's entry under `clusters:` | kept; new hosts and VMs get new serials |
| Remove hosts | `removedHosts` on the cluster's entry: the host numbers left out | the other hosts keep their names and serials; a removed number is not reused |
| Remove a cluster | the key under `removed:` | the cluster's serials remain reserved |
| Replace everything with a new environment | replaces `datacenters`, `defaults`, `vary`, `clusters`, `removed` and `seed` | a new registry: every identity is replaced (the same layout name and sizes build the same identities again) |
| Storage | `nfs:` and `iscsi:` under the datacenter | unchanged datastores keep theirs |
| Versions | `vcenterVersion`, `esxVersion`, a cluster's `esxVersion`, `hostVersions`, `forceNewerHosts` | none |
| Slowness and faults | `conditions:` | none; the running vCenter is not rebuilt |

`conditions:` is the console's one list of what is switched on. Each entry
has a `kind` (`slow`, `error`, `missing`, `timeout`, `perfdown`), `which`
requests it applies to (`perf`: every performance query; `one`: the queries
of one `family`; `inv`: inventory requests on `/sdk`, slowness only; `method`:
one other `/vsanHealth` method), optional `clusters`, `fixedMs` and `jitterMs`
for slowness, and an optional `to` when it turns itself off. When the simulator
starts, the file's own `faults:`, `latency:` and `statsPrimary: false` are
turned into conditions, so the console lists and can turn off everything that
is on. A scenario file may also write `conditions:` directly.

The console no longer offers "Regenerate" or "Reset cluster seed"; `seedEpoch`
on a cluster and `-regenerate` remain available to a scenario file.

## Product versions

`vcenterVersion` and `esxVersion` accept presets `8.0u3`, `9.0`, and
`9.1.1`. ESX defaults to the selected vCenter release; a cluster can set
`esxVersion` and `hostVersions` can override a host by its topology key.
A custom version supplies `version`, `build`, `apiVersion`, and `fullName`.
When ESX matches a custom vCenter, it uses the same version, build, and API
version; its ESXi product name is synthesized from the supplied values.
Hosts newer than vCenter are rejected unless `forceNewerHosts: true` is set;
the console says how many hosts a choice would leave newer than vCenter and
asks for a tick before it goes ahead. A version typed into the console that is
not a preset gets a build number, API version and product name derived from
the number: plausible strings, not ones a real release carries. Version
changes rebuild the simulator, preserve inventory identities, and end sessions.

```yaml
vcenterVersion: {preset: "9.0"}
esxVersion: {preset: "9.0"}
clusters:
  - key: dc-01/cluster-01
    esxVersion: {preset: "8.0u3"}
hostVersions:
  dc-01/cluster-01/esx01: {preset: "9.0"}
```

The release and build pairs come from Broadcom's [vCenter build table](https://knowledge.broadcom.com/external/article/326316/build-numbers-and-versions-of-vmware-vcen.html)
 and [ESX build table](https://knowledge.broadcom.com/external/article/316595/build-numbers-and-versions-of-vmware-esxi.html).
The 9.1.1 names and API versions also come from the repository's live capture.
For 8.0 U3 and 9.0 the full product names are synthesized. Every preset
carries an API version (8.0.3.0, 9.0.0.0 and 9.1.1.0), which hosts report as
their own `about.apiVersion` and `licenseProductVersion` (major.minor of the
host release), so a mixed cluster shows each host's release, not the
vCenter's. The vSAN service-version document is the captured 9.1.1 document cut
back to the newest namespace the selected vCenter can speak: 9.1.1.0, 9.0.0.0,
or, for 8.0 U3, 8.0.0.4, the newest 8.0 entry in the capture. That mapping is
inferred from the capture's ordering and has not been checked against a real
8.0 U3 vCenter. The REST host list keeps the vSphere Automation shape
(`host`, `name`, `connection_state`, `power_state`), which has no product
version; the vCenter version is served at `/api/appliance/system/version` and
host versions through `/sdk` (`config.product`) and the vSAN health surfaces.
Apart from selected version metadata and the advertised API versions,
release-specific namespaces, properties, and vSAN behavior are not modelled.
`tools/smoke/smoke.sh` checks a 9.0 vCenter with an 8.0 U3 cluster and one 9.0
host through govc and REST; `tools/smoke/smoke.ps1` repeats it through PowerCLI.
