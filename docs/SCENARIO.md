# Scenario file reference

The scenario file is the operator's contract with the simulator: one YAML
file that says what the simulated vCenter contains, who may log in, which
certificate it serves, and how it misbehaves. Change the file and restart, or
edit a running simulator in the control console (below). Examples live in
[`examples/scenarios/`](../examples/scenarios/).

```sh
vcsimulator -scenario examples/scenarios/ten-clusters.yaml -data ./data -listen 0.0.0.0:8443
```

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
| add devices to an existing host | existing ones unchanged | the host's new local datastore is created inside the host's step, so objects created after that host renumber |
| remove entities | the rest unchanged; the removed serials stay retired | objects created after the removed one renumber |
| change `seed` | refused at start unless `-regenerate` is given; then every identity changes | all change |
| change `telemetrySeed` | unchanged (every metric value changes) | unchanged |

uuids are the registry's business and hold through every edit. MoRefs come
from vcsim's creation counter: the inventory is created in registry serial
order, so a restart assigns the same MoRefs and additions are created after
everything that existed, but a removal (or a device added to an existing
host) renumbers what was created after it. A client that keys on MoRefs
(the VCF Operations vCenter adapter does) sees those objects as new; a
client that keys on uuids (the vSAN pack) does not. `-regenerate` discards
the registry whatever the seed; with the same seed the same identities come
back, so change the seed to mint new ones.

## Reference

```yaml
seed: ten-clusters          # required; names the identity namespace
telemetrySeed: ""           # optional; metric values only (default: seed)

datacenters:                # each gets one vDS (vds-01) with four portgroups
  - name: dc-01             # lower-case letters, digits, hyphens
    clusters: 10            # cluster-01 .. cluster-10

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
                            #   cluster's identities stay retired

auth:                       # no users: any non-empty name and password log in
  users:
    - name: administrator@vsphere.local
      password: VMware1!
    - name: readonly@vsphere.local
      passwordEnv: VCSIM_READONLY_PASSWORD   # read from the environment at start

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

The walk did not contain live samples for VM-level families, stretched
clusters, multiple disk groups per host, dedup enabled, or populated iSCSI.
Those cases are schema-only and are not validated as live OSA behavior.
OSA models one disk-group identity per host; disk mappings were empty in
both walks. Capacity and telemetry values are synthetic, not recordings of
the source cluster.

## Editing a running simulator: the control console

The console (`-admin`, default `http://127.0.0.1:9090/`) edits the same
model without touching the file. Edits collect in one pending draft and
apply at the next five-minute bucket boundary (or at once). The applied
scenario is saved in `<data>/console.json` without the `auth` and `tls`
sections (those always come from the file, so no password is ever written
there). On the next start the saved state resumes unless the scenario file
changed since, in which case the file wins.

| Console action | Scenario effect | Identities |
|---|---|---|
| edit a cluster | an entry under `clusters:` | kept; new hosts and VMs get new serials |
| Add cluster | `datacenters[].clusters` + 1 | existing unchanged |
| Delete cluster | the key under `removed:` | the cluster's serials are retired |
| Regenerate | none (an apply-time action) | the cluster's serials are retired and new ones minted, same seed |
| Reset cluster seed | `seedEpoch` + 1 on the cluster | unchanged; every sample of the cluster changes |
| generator / new global seed | replaces `datacenters`, `defaults`, `vary`, `clusters`, `removed` and `seed` | a new seed replaces every identity |
| latency, fault rules | `latency:`, `faults:` | none; the running vCenter is not rebuilt |
