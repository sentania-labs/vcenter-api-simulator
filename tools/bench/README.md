# Scale replay

The replay fixture records one run of the unmodified `v2.0.0-rc.2` collector
at commit `af976fb9fe97bbac2fd94b666a3f79aa12656f2a` against the built-in
four-host scenario. Credentials and generated session IDs are omitted, and
managed object references and query timestamps are placeholders. The capture
ran connection, name and parent mapping, disk hardware lookup, probe and a
full collection. It returned 240 objects and no collection problems.

Run the replay on demand, keeping the simulator and driver in the same memory
scope:

```sh
systemd-run --user --scope -p MemoryMax=4G -- python3 tools/bench/bench.py examples/scenarios/scale-50.yaml --runs 1 --out /tmp/vcs-bench
```

The output directory is an exportable bundle with the scenario, JSON and Markdown
summary, per-call CSV and server query log. The log contains request bodies and
may contain inventory identifiers; login bodies and HTTP headers are omitted.
Use a private output directory and review it before sharing. The query log can
also be enabled independently with `vcsimulator -query-log PATH`.

The replay substitutes simulator inventory identities and repeats recorded host
and VM requests to cover the profile's cardinality. It opens a session for each
cluster, unlike the real collector's single session across clusters. Discovery
traffic and its wall time are included in the reported pass time but not in the
replay call count. It is sequential and does not simulate Operations ingestion,
concurrent collectors, retries or Cloud Proxy resource use. Expected HTTP 500
responses for unsupported families are retained; differences from the recorded
status are counted in JSON and CSV. Existing scenario latency and fault knobs
apply; a fault affecting login or view creation aborts the run. `VmHWM` is the
simulator process high-water RSS, not the combined scope's peak. Series totals
are projections from the captured catalog, not measured ingestion.

The output directory must not exist or must be empty; the harness refuses
anything else and never changes the permissions of a directory it did not
create. Bundle files are created owner-only.

## Measured results

Conditions: shared four-CPU host with other workloads running, simulator and
driver together in one `systemd-run --user --scope -p MemoryMax=4G` scope,
commit `bf5f4ad` plus the harness fixes in this change, sequential replay, query
log enabled, per-call status column present. Runs for a profile were
consecutive in one process. Throughput in this table is replay calls over the
pass wall time, which also includes discovery calls. "Peak RSS" is the
simulator's `VmHWM` after the pass, so it is cumulative across a profile's runs.

| clusters | hosts | VMs | runs | replay calls | req/s | p50 ms | p95 ms | p99 ms | peak RSS MiB | unexpected statuses |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 | 100 | 552 | 3 | 5,412 | 487.8 to 603.8 | 1.38 to 1.63 | 2.89 to 4.43 | 5.56 to 7.67 | 93 to 98 | 0 |
| 50 | 400 | 1,600 | 3 | 19,000 | 463.8 to 482.7 | 1.63 to 1.68 | 4.61 to 4.74 | 7.88 to 8.51 | 180 to 183 | 0 |
| 106 | 848 | 3,392 | 3 | 40,280 | 407.8 to 463.2 | 1.63 to 1.77 | 5.13 to 5.88 | 10.3 to 12.3 | 316 to 319 | 0 |
| 200 | 1,600 | 6,400 | 1 | 76,000 | 393.9 | 1.78 | 5.95 | 14.1 | 542 | 0 |
| 400 | 3,200 | 12,800 | 1 | 152,000 | 306.7 | 1.95 | 7.53 | 27.9 | 1,040 | 0 |

Ranges are minimum to maximum across the runs. The 10-cluster spread is the
widest (about 20 percent in throughput); the 50 and 106 cluster profiles
repeat within 4 and 13 percent. The 200 and 400 cluster rows are the ramp, one
pass each, with scenarios made from `scale-100.yaml` by changing only the seed
and `clusters`.

The ramp was run with these failure criteria: any unexpected HTTP status or
aborted run, p99 above 1 s, or the simulator hitting the 4 GiB memory cap.
**Tested to 400 clusters (3,200 hosts, 12,800 VMs, 152,000 replay calls in one
495 s pass) without reaching a failure criterion; the ceiling was not found.**
400 clusters is the highest tested point, four times the 100+ target, and is
not a ceiling. The next step, 800 clusters, would take at least about 17
minutes per pass at the observed rate and was not run.

Estimate, not a measurement: within the tested range the simulator's RSS grew
roughly 2.6 MiB per cluster, which would reach the 4 GiB cap near 1,500
clusters if the trend holds. Throughput fell and pass time grew faster than
cluster count past 106 clusters (200 to 400 clusters took 2.6 times as long for
twice the calls), so a sequential single-collector pass of this shape slows as
the vCenter grows. Treat the numbers as one shared-host sample, not sustained
capacity, and rerun on the target host.

To continue the ramp on a quieter host, make a scenario from `scale-100.yaml`
with a new seed and cluster count, then run one pass under the same cap and
step up until a criterion above is met:

```sh
sed 's/clusters: 106/clusters: 800/; s/^seed: .*/seed: scale-ramp-800/' examples/scenarios/scale-100.yaml > /tmp/ramp-800.yaml
systemd-run --user --scope -p MemoryMax=4G -- python3 tools/bench/bench.py /tmp/ramp-800.yaml --runs 1 --out /tmp/vcs-bench-800
```
