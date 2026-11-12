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
| `go test ./...` (control console) | CI, required | `internal/control`: draft, review and apply keep every identity they should and rebuild the vCenter only for topology changes; Delete, Regenerate and Reset seed; saved state across a restart and a changed scenario file; scheduled apply; projections equal what the Performance Service enumerates. `internal/activity`, `internal/admin`: request classification (ok, injected, unexpected, refused, timeout), redaction (no `/sdk` body, no credential), cycle diagnosis, admin API guards (console claim and login, sessions, login rate limit, Host allowlist against DNS rebinding, cross-origin and content-type checks); `internal/credentials` (hashed vCenter users, persistence), `internal/querylog` (unanswered and full capture, redaction) |
| `tools/smoke/smoke.sh` | CI, required | the principal's acceptance (2026-11-07): a plain vSphere client (govc) connects, lists hosts and VMs, counts match the scenario, identities identical across a restart |
| `tools/mp-harness/harness.py` | manual `mp-acceptance` workflow, local | the reference pack's collector against every cluster of a scenario |
| `tools/mp-harness/mp_test_run.py` | manual `mp-acceptance` workflow, local | `mp-test connect` and two collects through the Operations integration SDK |
| `tools/probe/test_probe.py` | local, not in CI (needs `go` and `pyVmomi`) | the read-only vCenter walk `tools/probe/probe.py` against the built simulator: complete evidence tree, fault records, sanitized copy and tarball contents (see [`tools/probe/README.md`](../tools/probe/README.md)) |

The first real VCF Operations collection gate passed with the installed Python
vSAN Host Metrics 2.0.0 pack; scope, procedure and measured result are in
[OPERATIONS-ACCEPTANCE](OPERATIONS-ACCEPTANCE.md). Native-vCenter attachment,
dashboards, the Java Content Factory pack, and live scenario-change and
failure tests (steps 5 and 7 above) remain unverified.

Comparison against a live vCenter was never run and remains unverified.

## Determinism tests
Given the same seed, entityRefId, metric and 5-minute UTC slot, return equal values regardless of request order or process restart. Different entities should not accidentally alias. Configured changes must take effect at a defined bucket boundary. Time window requests must contain only relevant bucket timestamps and honor unavailable or not-yet-populated bins.

## UX acceptance
The control console was built from the owner-approved Rev 2.1 mockups. Screenshots of every screen in light and dark, taken from the running app, are in [`docs/console/`](console/), and an independent evaluation agent walked workflows A-F of [UX-REQUIREMENTS](UX-REQUIREMENTS.md) against the built UI (scores and findings in [`docs/console/`](console/README.md)). The original gate:
Before building UI, provide real desktop mockups, walk through generation, topology editing, scenario injection, query inspection and export flow using an independent end-user agent. Test novice and expert paths and require operator review.

## CI gates
Unit/property tests for topology/identity, SOAP contract fixture tests, MP integration harness, container/Helm lint, secret scan, dependency/security scan, reproducible build. Live-vCenter tests are opt-in, never CI default, and must not require credentials in repository settings unless operator explicitly arranges controlled secrets.
