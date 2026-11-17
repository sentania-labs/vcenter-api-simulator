# Inventory sizing and health

For **215 hosts and 5,009 VMs**, set the deployment to **2Gi memory limit,
2 CPUs, and a data volume of at least 1Gi**. Request 1Gi memory and 1 CPU
if reserving capacity independently of limits. The lab deployment lives
outside this repository and must apply those values separately. Expanding
an existing PVC also requires storage-class expansion support.

The old 512Mi limit is insufficient for this inventory under collection.
The lab independently confirmed OOM kills. A local two-client run reached
910Mi RSS with full query capture enabled. A full data volume is a separate
failure mode: the old binary exited during first certificate persistence
on an exhausted 4Mi tmpfs. The revised binary served both SDK discovery and
the console on that same exhausted filesystem after clearing only the old
binary's failed first certificate creation.

## Measurements

Measured on Linux, November 17, 2026, with vCenter/ESX 9.1.1, vSAN ESA,
one simulator at a time, a 6Gi safety scope, and a one-CPU quota. Two
independent clients log in, create container views, and collect every property
of datacenters, clusters, hosts, datastores, resource pools, VMs, switches and
portgroups using RetrievePropertiesEx/ContinueRetrievePropertiesEx, 250
objects per page. Full query capture is enabled. RSS is sampled every 20ms;
CPU seconds come from /proc/PID/stat. Numbers are observations, not limits.

| Inventory | RSS after build | Peak RSS, two clients | Build elapsed / CPU seconds | Two-client collection | Suggested memory limit / CPUs |
| --- | ---: | ---: | ---: | ---: | --- |
| Default: 1 cluster, 4 hosts, 4 VMs | 39Mi | 51Mi | 0.20s / 0.05s | 0.15s | 512Mi / 1 |
| 4 clusters, 32 hosts, 800 VMs | 75Mi | 173Mi | 0.90s / 0.87s | 3.79s | 512Mi / 1 |
| 7 clusters, 215 hosts, 5,009 VMs | 268Mi | 815Mi | 6.37s / 6.14s | 27.21s | 2Gi / 2 |

The medium/large cases have 12 claimed and one local device per host
(2,795 devices in the large case). The large case is four clusters of
8 hosts and 200 VMs, plus three of 61 hosts and 1,403 VMs.

Before the change, the large case with capture enabled used 252Mi RSS
after build and peaked at 910Mi; construction took 6.82s, with 6.08 CPU
seconds. Releasing completed construction-task runners removes their
retained VM/host input specifications, while preserving task results and
history. The construction-only registry watcher is detached before serving.
An isolated forced-GC comparison of the runner change retained 108,404,320
bytes / 1,145,279 objects before and 107,992,264 bytes / 1,133,781 objects
after: about 0.39Mi and 11,498 objects freed. This is a modest saving, not a
way to fit the large workload into 512Mi.

RSS after build varies with GC and was higher in the revised run, so these
single runs do not establish a steady-state RSS reduction. The peak dropped
about 95Mi; use the sizing headroom rather than depending on that difference.

Operations 9.1.1 adapter logs show property-event processing plus separate
REST cluster and appliance reads. This harness exercises the full SOAP
inventory/property envelope. It is intentionally broader than selected-property
collection and is **not an exact adapter replay**: it excludes long-lived
property filters, performance polling and vSAN metric collection. Different
collectors, concurrent requests, disks per VM, rebuild overlap and capture
traffic can require more memory and CPU. Repeat the measurement for a new
workload before reducing a reservation.

Without a CPU quota (still capped at 6Gi), the revised large inventory built
in 4.82s using 5.96 CPU seconds; two-client collection took 11.97s, peak RSS
was 783Mi, and maximum successful discovery latency was 153ms. This supports
reserving two CPUs for this size.

## With Activity running

Measured on Linux, November 17, 2026, with the large inventory above inside a
systemd scope with a 2Gi memory limit, no swap and a two-CPU quota. Activity
changes the running vCenter in place, so it adds no second copy of the
inventory; what it keeps is its settings, the last 1,000 lines of "What has
happened", and the tasks of the last ten minutes.

| What ran | Result |
| --- | --- |
| A week of activity at every dial's baseline, played as fast as it goes (`VCSIM_LARGE=7`, see [Testing](TESTING.md)) | 3.5ms for each simulated minute; a day is 1,423 vMotions, 100 VMs powered off and on, 46 added, 45 removed and 11 hosts into maintenance mode, which vCenter records as 6,929 events and 1,816 tasks. Heap in use after each day: 190Mi to 192Mi, level from the fourth day (the test fails if it grows by more than 24Mi). Peak RSS 312Mi. |
| The simulator itself with full query capture, Activity started, then every counted dial and the share of hosts in maintenance mode at their busiest, while two clients each collected every VM, host, cluster and datastore twice | Built and listening in 9s at 255Mi RSS. 269Mi while hosts were being emptied. **Peak RSS 745Mi** during the two-client collection (each round of 5,453 objects took 20s to 23s), 339Mi afterwards. 49.7 CPU seconds in all. The scope's memory limit was never reached (`memory.events`: max 0, oom 0). |

Activity in real time is cheap: at the baseline the large inventory sees about
one vMotion a minute. The 2Gi and two-CPU recommendation above stands with
Activity on; the collecting clients, not Activity, set the peak.

## Standard performance collection

A separate November 17, 2026 run used the Operations 9.1.1 adapter's own
Java VLSI client classes against the simulator, with the large inventory above.
The server used `GOMAXPROCS=2` and `GOMEMLIMIT=1500MiB`; server and client shared
a two-CPU quota and a 4Gi safety scope. Query capture was disabled. The client
used a 512Mi Java heap. The RSS measurement below is the simulator process,
not the Java client or the whole scope.

The client discovered available counters for all 5,454 providers (5,009 VMs,
215 hosts, seven clusters, 222 datastores and one datacenter), then requested
one sample of every supported aggregate counter in batches of 100 entities.
VMs and hosts used 20-second intervals; containers and datastores used
300-second intervals. It decoded 879,322 CSV series, including guest counters,
using the adapter's bindings and decoded a normal XML composite query too.

| Measurement | Observed |
| --- | ---: |
| Discovery and collection elapsed | 35.62s |
| QueryPerf collection elapsed | 18.76s |
| Simulator peak RSS | 464.85Mi |
| Simulator CPU time, including startup | 39.21s |

This fits the documented 2Gi/two-CPU sizing for one collection cycle. It is
not an Operations ingestion run or a concurrent property/vSAN/performance
stress test, and does not request every per-device wildcard series. Keep the
existing sizing headroom for those workloads.

A separate real-client run collected 288 five-minute CPU samples across a
simulated day for 12 VMs, four hosts, their cluster and datacenter. VM totals
agreed with host totals, and hosts with the cluster/datacenter, allowing only
the less-than-one-MHz truncation per independently averaged series. Example
rows (MHz, UTC) from the same collection:

| Time | One VM | Its host | Cluster | Datacenter | Sum of all VMs | Sum of all hosts |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Nov 16 12:10 | 2,621 | 5,483 | 12,767 | 12,767 | 12,761 | 12,766 |
| Nov 16 20:10 | 1,597 | 3,110 | 8,639 | 8,639 | 8,634 | 8,638 |
| Nov 17 04:10 | 1,128 | 2,433 | 6,708 | 6,708 | 6,704 | 6,706 |
| Nov 17 10:10 | 2,097 | 4,523 | 12,705 | 12,705 | 12,701 | 12,704 |

The committed contract tests also check exact 20-second conservation,
rollup averages and summations, state changes, capacity bounds, timestamps,
instances and full property-collection enum validity. No Operations service
was contacted for these measurements.

## Console preview

Add-cluster, add-hosts and replace-environment previews show a planning budget:
128Mi plus 2Mi per host, 256Ki per VM and 16Ki per physical device, rounded up
to 256Mi. This reserves headroom for two collectors and ordinary inventory
rebuild overlap; it is not a heap forecast or a guarantee for arbitrary
request concurrency. When cgroup v2 memory.max is readable, the console uses
the tightest visible parent/leaf limit and warns before Apply if the budget
exceeds it. An unreadable or unlimited cgroup leaves the estimate visible
without claiming a limit. Home is unchanged.

## Probes

The public HTTPS listener now binds before inventory construction.
`/healthz` answers immediately; `/readyz` returns 503 until the inventory and
capture handler have been installed. SDK discovery at
`/sdk/vimServiceVersions.xml` also answers during construction. Existing
inventory remains available while a replacement builds.

On one CPU, initial discovery improved from 6.82s to 0.20s in the large
case. Successful discovery responses during build and collection took at
most 0.94s, below the 3–5s probe budgets. The old run had 67 connection
failures before listening; the revised run had one initial connection
attempt before binding. An OOM kill remains fatal regardless of probes.

The chart uses /readyz for startup/readiness and /healthz for liveness.
Startup allows five minutes (150 attempts every 2s), with a 5s timeout;
readiness allows 3s and liveness 5s. A nonempty probes.path overrides all
three paths for existing custom configurations. The default four-host
scenario keeps a 512Mi memory limit and 1 CPU, with requests of 128Mi and
250m CPU, and a 100Mi data volume. Larger inventories need explicit sizing.

## Full storage

Registry and console-state save errors no longer reject startup or a live
inventory change. The log says the process continues in memory and changes
may be lost on restart, and the existing unsaved-state notice remains until
both saves succeed. Existing credentials/certificates are read normally.
On a fresh full volume, newly generated TLS/sign-on keys and a deployment
console password can serve from memory with explicit unsaved warnings.
An unsaved TLS certificate changes on restart and tools may need to trust it
again. Invalid or incomplete pre-existing certificate pairs are still rejected
to avoid silently changing a pinned identity.

If capture initialization itself fails with ENOSPC/EDQUOT, startup logs that
capture is unavailable and serves without it until storage is repaired and
the process restarted. This change does not modify query-log rotation or
retention. Increase the volume and configure bounded query logging separately;
1Gi is a floor, not a retention guarantee.

## Reproduce

Build the executable, then run each size sequentially. Use your own scratch
directory and remove it afterwards. The script chooses free loopback ports,
checks its own startup banner, verifies inventory counts and stops its child
by PID even when a request fails.

```sh
go build -o /your/scratch/vcsimulator ./cmd/vcsimulator
systemd-run --user --scope -p MemoryMax=6G -p CPUQuota=100% \
  python3 tools/sizing/measure.py large /your/scratch/vcsimulator large-one-cpu /your/scratch
# Omit CPUQuota for the comparison without a CPU cap; keep the 6Gi safety cap.
```

OOM attribution would be falsified by an observed exit with no OOM event and
memory below the enforced limit, or by probe failures preceding restart while
the process remains alive. Probe-caused restarts and storage failures can
coexist with OOMs; obtain container termination reasons/events when diagnosing
another deployment.
