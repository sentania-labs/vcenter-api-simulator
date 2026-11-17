# Sizing

## What to give it

| Environment | Memory limit | CPUs | Data volume |
|---|---|---|---|
| The default: 1 cluster, 4 hosts, 4 VMs | 512Mi | 1 | 100Mi |
| Up to about 800 VMs (4 clusters, 32 hosts) | 512Mi | 1 | 1Gi |
| **215 hosts and 5,009 VMs** (7 clusters) | **2Gi** | **2** | **1Gi** |

For the large size, request 1Gi and one CPU if you reserve capacity separately
from limits. Memory is what runs out first: 512Mi is not enough for the large
size while tools are collecting, and the container is killed. The data volume
holds the state files and the request log (at most 24 MiB by default), so 1Gi is
plenty. Where the volume has less room than the log's cap, the log limits itself
to the space there, less 8 MiB kept for the state files. Expanding an existing
Kubernetes volume needs a storage class that allows it
([chart's README](../charts/vcenter-api-simulator/README.md#upgrading)).

The console helps you plan. Before a change that adds clusters or hosts, it shows
a memory estimate (128Mi plus 2Mi per host, 256Ki per VM and 16Ki per physical
device, rounded up to 256Mi). Where it can read the container's memory limit, it
warns before a change that would not fit. It is a planning number, not a
guarantee. Different tools, several collecting at once, more disks per VM, a
rebuild while tools collect and full request capture can all need more.

## Measurements

Measured on Linux on 2026-11-17 with vCenter and ESX 9.1.1 and vSAN ESA, one
simulator at a time, under a 6Gi safety cap and a one-CPU quota. Two clients each
sign in, create container views and read every property of datacenters,
clusters, hosts, datastores, resource pools, VMs, switches and portgroups, 250
objects a page. Full request capture was on. These are observations, not limits.

| Environment | Memory after build | Peak memory, two clients | Build time (CPU seconds) | Two-client collection |
|---|---:|---:|---:|---:|
| Default: 1 cluster, 4 hosts, 4 VMs | 39Mi | 51Mi | 0.20s (0.05s) | 0.15s |
| 4 clusters, 32 hosts, 800 VMs | 75Mi | 173Mi | 0.90s (0.87s) | 3.79s |
| 7 clusters, 215 hosts, 5,009 VMs | 268Mi | 815Mi | 6.37s (6.14s) | 27.21s |

The medium and large cases have 12 claimed and one local device per host (2,795
devices in the large case). The large case is four clusters of 8 hosts and 200
VMs plus three of 61 hosts and 1,403 VMs. With no CPU cap, the large case built in
4.82s and two clients collected in 11.97s, so give it two CPUs.

This is broader than what VCF Operations reads and is not a replay of it: it has
no long-lived property filters, performance polling or vSAN metric collection.

**With Activity running.** Activity changes the running vCenter in place, so it
adds no second copy of the inventory. It keeps its settings, the last 1,000 lines
of "What has happened" and the tasks of the last ten minutes. At the baseline a
large environment sees about one vMotion a minute. In a week of activity at every
dial's baseline, played as fast as it goes, each simulated minute took 3.5 to 3.7
ms and memory in use settled at 192 to 194Mi. With every dial at its busiest and
two clients collecting, peak memory was 745Mi. The 2Gi and two-CPU advice stands
with Activity on.

**Standard performance collection.** A client built from the Operations 9.1.1
adapter's own classes discovered counters for all 5,454 objects of the large
environment, then read one sample of every supported counter in batches of 100
objects (20-second intervals for VMs and hosts, 300-second for the rest). It decoded
879,322 series in 35.6s, and the simulator peaked at 465Mi. That is one
collection cycle, not an Operations ingestion run, and it does not ask for every
per-device wildcard series.

CPU totals add up across levels. For 288 five-minute samples over a simulated day
for 12 VMs, four hosts, their cluster and datacenter, VM totals matched host totals
and hosts matched the cluster, allowing less than 1 MHz of rounding for each
independently averaged series.

## Probes

The simulator listens before it has built the inventory, so a Kubernetes probe
never sees a closed port.

| Path | Answers |
|---|---|
| `/healthz` | Immediately, once running. After startup it checks that the simulator can answer a SOAP request, and returns 503 after two minutes without one. It recovers when requests are answered again |
| `/readyz` | 503 until the inventory is built and serving. Use it for startup and readiness |
| `/sdk/vimServiceVersions.xml` | Answers even while the inventory is being built. The existing inventory stays available while a replacement builds |

The chart uses `/readyz` for startup (every 2 seconds, up to 150 times, so five
minutes) and readiness, and `/healthz` for liveness. A non-empty `probes.path`
overrides all three paths. The large environment answers discovery in 0.20s, and
no answer during collection took longer than 0.94s, under the probe's 3 to 5
second limits. An out-of-memory kill is fatal whatever the probes say.

## When the data volume is full

A failed save of the identities or console state does not stop startup or a
change. The log says the simulator carries on in memory and that changes may be
lost at the next restart, and Home shows a notice until the saves succeed. A
certificate or sign-on key that could not be saved changes at the next restart, and
tools may need to trust it again. A certificate and key pair already on the volume
that is invalid or incomplete is still refused, so a pinned identity is not changed
without you knowing. If request capture cannot start for lack of space, the log
says so and the simulator serves without it until you make room and restart.

## Reproduce the measurements

Build the program, then run each size in turn in your own scratch directory. The
script picks free loopback ports, checks the simulator's start-up message and the
inventory counts, and stops its child by process id.

```sh
go build -o /your/scratch/vcsimulator ./cmd/vcsimulator
systemd-run --user --scope -p MemoryMax=6G -p CPUQuota=100% \
  python3 tools/sizing/measure.py large /your/scratch/vcsimulator large-one-cpu /your/scratch
```

Leave out `CPUQuota` for the run without a CPU cap, and keep the memory cap. When
diagnosing another deployment, get the container's termination reason and events:
a restart with no out-of-memory event and memory below the limit points at the
probes or the volume instead.
