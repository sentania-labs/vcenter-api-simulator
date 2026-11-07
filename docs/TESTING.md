# Test and Benchmark Plan

## Contract validation
- Pin upstream [nickfirefoot/vsanhostmetrics](https://github.com/nickfirefoot/vsanhostmetrics) commit SHA. Extract live MP calls and model before fixture generation.
- Acquire read-only credential *source* from operator, never password in handoff/repository.
- Use a narrow, allowlisted, read-only probe of the live vCenter. Collect method/type signatures and sanitized samples for representative ESA entity kinds. Secret, host, MAC, UUID, VM name, IP and sensitive inventory sanitization requires operator review before public commit.
- Golden SOAP fixtures: valid login, cluster discovery, node information, supported entity types, wildcard perf query, empty family, unsupported family, time window (including no data), malformed query, auth/expiry and transient failure.
- Verify with the exact pyVmomi / vSAN bindings bundled by the MP. Compare parsed object IDs, numeric values, timestamp arrays, sample shapes and errors, not only HTTP 200.

## Functional compatibility test
1. Create one datacenter, one four-host ESA cluster, two or more NVMe devices per host, a few VMs.
2. Validate /sdk inventory and all UUID relationships using govc or pyVmomi.
3. Run `mp-test` validation/collection twice and compare expected resource kinds/metrics. Note SDK commands may require a pseudo-TTY; do not assume headless runner works.
4. Connect actual Tier 2 Content Factory MP from a VCF Operations Cloud Proxy to simulator.
5. Assert adapter healthy and DATA_RECEIVING, objects have stable IDs and correct parent relationships, and metric values change predictably after a scenario change. Compare expected versus observed resources and metrics; record gaps.
6. Restart simulator between collections: identifiers must remain stable and synthetic time-bucket history reproducible.
7. Force timeout, unavailable family, permission error and partial cluster failure; ensure faults are surfaced, not silently treated as successful empty collection.

## Scale matrix (candidate targets, measure before declaring supported)
| Scenario | Clusters | Hosts/cluster | Purpose |
|---|---:|---:|---|
| Baseline | 1 | 4 | Golden parity |
| Moderate | 10 | 8 | First fan-out |
| Large | 50 | 8 | Collection and ingestion scaling |
| Stress | 100 | 8–16 | Locate bottlenecks |
| Cardinality stress | 10 | 8 | High per-host CPU/world/VM/disk entities |
| Latency stress | 50 | 8 | Inject bounded network/API delay |
| Fault stress | 50 | 8 | 5xx, partial family errors, dropped connections |

For each scenario sweep dashboard-only vs all modeled entity families, 5/10/15-minute collection intervals (where MP configurable), and deterministic seed. Include both one simulated endpoint with many clusters and, later, multiple independent simulated vCenters.

## Measured outputs
- Simulator: requests per operation/entity type/cluster, p50/p95/p99 latency, response bytes, returned entities and series, faults, peak RSS, CPU, GC, steady-state throughput.
- MP/Cloud Proxy: wall-clock per collection, request fan-out, peak RSS/CPU, memory limit and crashes/OOM, metrics transformed / rejected / missing, retry behavior.
- VCF Operations: advertised vs discovered resources, ingestion counts, DATA_RECEIVING status, metric freshness, relationship correctness, ingestion lag, resulting dashboards.
- Environment: seed/config checksum, simulator image/commit, source MP SHA, VCF Operations version, Cloud Proxy size, virtual/physical platform, test run duration.
- Explicitly distinguish expected generated metric *values* from metrics actually ingested. All totals must reconcile or list known exclusions.

## What runs today (Phase 2)

| Suite | Runs | Covers |
|---|---|---|
| `go test ./...` | CI, required | the Phase 1 contract replay; the ten-cluster scenario (identities on both surfaces per cluster, per-cluster cardinality and overrides); restart and edit identity stability through the registry; credentials; fault and latency knobs; inventory property paths against the trimmed live inventory capture; gauge values inside each label's live envelope and `Raw` counters never decreasing across more than the 90-day retention; fixture sanitization |
| `tools/smoke/smoke.sh` | CI, required | the principal's acceptance (2026-11-07): a plain vSphere client (govc) connects, lists hosts and VMs, counts match the scenario, identities identical across a restart |
| `tools/mp-harness/harness.py` | manual `mp-acceptance` workflow, local | the reference pack's collector against every cluster of a scenario |
| `tools/mp-harness/mp_test_run.py` | manual `mp-acceptance` workflow, local | `mp-test connect` and two collects through the Operations integration SDK |

The first real VCF Operations collection gate passed with the installed Python
vSAN Host Metrics 2.0.0 pack on Operations 9.1.1: two nonempty cycles, 240
simulated entities, 5,321 metric series, stable resource IDs, and
`DATA_RECEIVING`. Every source value matched within `1e-6` relative/absolute
tolerance. See [the acceptance procedure and measured result](OPERATIONS-ACCEPTANCE.md).
This used `attach_to_vcenter=false`; native-vCenter attachment, dashboards,
the Java Content Factory pack, and live scenario-change/failure tests remain
unverified by this gate.

## Determinism tests
Given the same seed, entityRefId, metric and 5-minute UTC slot, return equal values regardless of request order or process restart. Different entities should not accidentally alias. Configured changes must take effect at a defined bucket boundary. Time window requests must contain only relevant bucket timestamps and honor unavailable or not-yet-populated bins.

## UX acceptance
Before building UI, provide real desktop mockups, walk through generation, topology editing, scenario injection, query inspection and export flow using an independent end-user agent. Test novice and expert paths and require operator review.

## CI gates
Unit/property tests for topology/identity, SOAP contract fixture tests, MP integration harness, container/Helm lint, secret scan, dependency/security scan, reproducible build. Live-vCenter tests are opt-in, never CI default, and must not require credentials in repository settings unless operator explicitly arranges controlled secrets.
