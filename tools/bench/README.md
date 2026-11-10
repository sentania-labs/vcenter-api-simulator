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

On this shared four-CPU host, with simulator and driver in a 4 GiB cgroup, one
106-cluster run (848 hosts, 3,392 VMs) completed 40,280 replay calls in 75.592 s,
532.9 calls/s, p50 1.473 ms, p95 4.421 ms, p99 7.933 ms, and 296 MiB simulator
peak RSS. One 50-cluster run completed 19,000 calls in 95.609 s, 198.7 calls/s,
p50 2.993 ms, p95 14.286 ms, p99 31.192 ms, and 166 MiB simulator peak RSS;
there were zero unexpected HTTP statuses. Both were single runs on a shared
host, not controlled capacity comparisons. 106 clusters is the highest profile
tested here, not a demonstrated failure limit or a claim of sustained capacity.
The earlier 106-cluster run preceded the per-call status column; query logging
was enabled for both. Rerun on the target host for current conditions.
