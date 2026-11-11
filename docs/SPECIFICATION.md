# Kickoff Specification — vCenter API Simulator

Status: initial steering, 2026-11-06. Owners/agents must validate against current code and live lab rather than treating candidate interfaces as guarantees.

## 1. Problem and outcomes
A real VCF Operations deployment and a port of [nickfirefoot/vsanhostmetrics](https://github.com/nickfirefoot/vsanhostmetrics) to a Tier 2 VCF Content Factory management pack need repeatable, large-scale vSAN ESA collection scenarios. Provisioning hundreds of real ESA clusters is impractical. Build an API-faithful simulator that advertises vCenter and vSAN performance data, supports arbitrary topologies and workload cardinality, and makes collector bottlenecks measurable.

The **first success criterion** is that the actual MP, pointed at the simulated vCenter hostname, authenticates, discovers the advertised ESA clusters, retrieves correctly keyed vSAN metrics, and publishes objects/metrics to VCF Operations. API-response unit tests alone do not prove success.

## 2. Boundary and priority
P0: vSphere /sdk inventory + authentication/session primitives that the MP uses; vSAN /vsanHealth Performance Service; multi-cluster ESA data; controls for scale; API instrumentation.
P1: native vCenter adapter ingestion, richer topology editor, failure profiles, deterministic history/replay, Helm and UI.
P2: broader vSphere PerformanceManager metrics, regular datastore/vDS/VM realism, diverse datastore combinations, other optional services (vSAN OSA read side is now emulated; see SCENARIO), multi-vCenter orchestration.

The vCenter and ESX version **profile** targets 9.1.1-era compatibility. The worker must discover reported build strings and API behavior from the available live lab; do not hardcode unverified patch/build numbers or pretend real hypervisors exist.

## 3. Reference MP and measured facts
Read live upstream sources `app/perfsvc.py`, `app/adapter.py`, `app/perfsvc_model.py`, `app/metric_scope.py`, `docs/COLLECTION-DESIGN.md`, and `REQUIREMENTS.md` before building. As inspected 2026-11-06, active collection uses vCenter read-only login and `VsanPerfQueryPerf` via /vsanHealth with wildcard entityRefId and start/end timestamps. It takes the latest nonempty sample from fixed five-minute buckets. Historical host `/vsanmetrics` scraping is dormant. Docs report 848 Operations objects across 32 kinds from one four-host cluster; this is a measured reference, not a universal cardinality. Repository snapshots describe ~69 modeled entity kinds and ~1,636 metric keys, with view-based selection. Regenerate counts from the actual pinned commit; docs may drift.

Record the exact upstream commit SHA and generated model hash in test reports.

## 4. Inventory and topology
Single emulator instance can contain multiple datacenters and clusters. Per cluster:
- Name, deterministic UUID, location, HA/DRS/vSAN ESA features, optional services.
- Host count and independently overridden per-host CPU/socket/core count, RAM, NICs/vmk/vDS/uplinks, disk inventory, controller/firmware details, health, connection state.
- VM count and host placement, CPU/memory/disk count, UUID and instanceUuid, virtual disk paths.
- Storage: vSAN ESA pools/devices and datastore objects, local VMFS, and optional shared NFS v3 and iSCSI-backed VMFS. Datastore accessibility and host membership are relationships, not simple counts.
- vDS count, port groups and host/VM associations. Permit variable clusters with different device counts, not merely uniform duplication.

Support a wizard to generate topologies and a detailed per-cluster editor. Define reproducible seeds, globally unique stable identifiers (MoRef, host UUID, vSAN node UUID, cluster UUID, VM UUID/instance UUID, physical disk UUID), and reset semantics. No identity churn across service restart unless an explicit reset/regenerate operation occurs.

## 5. vSAN API compatibility contract
Derive **the exact exercised methods and schemas** from the pinned MP and live captures. Initial candidates:
- SOAP login/session and inventory traversal via `SmartConnect` / `RetrieveContent`, container views, `HostSystem`, `ClusterComputeResource`, `Datacenter`, properties required for naming and relationships.
- /vsanHealth service registration/binding and `VsanPerfQueryPerf([VsanPerfQuerySpec], cluster)` with `entityRefId='family:*'`, time filtering, correctly typed counter series/samples and five-minute alignment.
- `VsanPerfQueryNodeInformation`, `VsanPerfGetSupportedEntityTypes` as required, and vSAN cluster config UUID.
- For disk enrichment: `QueryDisksForVsan`, `QueryVsanManagedDisks`, relevant host storage topology and selected cluster-health summary methods, if MP paths require.
- SOAP faults and permission-denied / unsupported / empty-family behavior exactly where observed.

A `govmomi/vcsim` integration spike must confirm /sdk and /vsanHealth share authentication/session and routing correctly before substantial implementation. If vcsim cannot be cleanly extended, propose a modular alternative with evidence, not a second independent world model.

Build a versioned compatibility matrix showing method, request fixture, response fixture, expected behavior, implemented status, verified-by-vCenter, verified-by-MP.

## 6. Metric / entity model
Do not invent metric keys or response field names. Parse generated entity model, scopes, units, and sample semantics. Support dashboard-only and all modeled entities as distinct scenarios. Model cardinality separately for each entity family: cluster-scoped, host-scoped, per-CPU, per-world, per-NVMe device, per-vNIC/pNIC, per-VM, per-virtual-disk, etc. Correctly form `entityRefId` with all composite identity pieces; preserve joins to native inventory.

Five-minute time bins, monotonic continuity and seeded deterministic samples. Identical request/time/seed returns identical values. Configurable workload shapes (idle, business-hours, steady, burst, degradation), correlated VM/host/cluster/storage metrics, and optional explicit fault/lag patterns. Values must obey units, plausible ranges and basic conservation constraints; seed-based synthetic results are not actual vSAN performance predictions. Limit generation/caching to avoid O(clusters × entities × metrics × history) resident memory.

## 7. Scale, load and observability
Scale knobs must be independent: # vCenters (deployment-level), # clusters, hosts/cluster, VMs/host, NVMe devices/host, CPUs/worlds/host, entity family cardinality, metrics/entity, history depth, collection concurrency, response latency, error injection.
Measure requests by SOAP operation and entity family; rate, p50/p95/p99 latency, status/faults, active connections, serialized bytes, response object/sample counts, CPU/RAM/GC, peak RSS, and error rate. Export Prometheus metrics and structured redacted logs. Provide a trace/correlation ID without credentials or raw session cookies. Performance knobs should permit a fixed-latency response and bounded jitter for modeling real vCenter behavior.

## 8. Operator UI and control
Lattice visual language. Single simulated vCenter operator panel with topology tree, summary cards, configuration editor/generator, scenario controls, run/reset, live query activity, entity count estimates, metric scope toggles, status/compatibility. UI may edit the simulation; API traffic must not depend on UI availability. Require conceptual wireframe + desktop mockup + independent end-user usability evaluation and user approval BEFORE UI implementation. See UX-REQUIREMENTS.

## 9. Packaging and deployment
Local Docker Compose or equivalent developer workflow plus a Kubernetes Helm chart for a single simulated vCenter, designed for Argo CD. One Helm release = one vCenter endpoint; one instance can host many clusters. Avoid runtime Kubernetes privilege, ingress mutations or cluster-wide RBAC. Service and ingress hostname, /sdk, /vsanHealth paths, TLS CA and SAN, credentials, health probes, resource limits, persistence, optional externally secured management UI. Never expose admin UI publicly by default. Persistent state for inventory and seeds; stateless deterministic metrics where possible. Docker image should be reproducibly built and labeled with source commit.

## 10. Security and data protection
The operator must tell the orchestrator where a worker can obtain a **read-only credential** before any live probing. Ask for the source/location/method, not the secret value. Use secret manager, scoped environment injection or an already-authorized runner secret; least privilege, no hardcoded credentials. No side-effectful vSphere operations, no mutation of live vCenter. Redact logs and fixture captures; sanitize hostnames, UUIDs, IPs, users and certificates if necessary. Do not commit live captures or credentials. Default to synthetic fixtures. Confirm permission before capturing any potentially sensitive inventory.

## 11. Acceptance gates
**Gate A — discovery:** pinned upstream commit, exact API/field coverage matrix, safe credential sourcing established, read-only probes captured/sanitized, vcsim feasibility report and architecture signoff.
**Gate B — compatibility:** one four-host ESA cluster with documented entity kinds produces valid samples and stable identities; real MP succeeds across repeated collections; all unsupported/empty/error semantics tested.
**Gate C — scale:** 10, 50, 100 cluster profiles or feasible alternative measured, including resource counts, request rate/bytes, p95 query and collection duration, MP RSS/CPU, VCF Operations objects and accepted metric counts; repeat runs and record environment/spec. Do not claim 100 clusters supported until demonstrated.
**Gate D — release:** UI approved and end-user evaluated; Helm install/upgrade/restart deterministic; documentation, automated tests, CI, vulnerability and secrets scans, external review and release tag.

## 12. Explicit non-goals for MVP
Real ESX/vSAN infrastructure, write operations such as provisioning or migrations, full vCenter 9.1.1 API fidelity, exact simulation of device physics, large-scale multi-vCenter dynamic ingress automation, production authentication service, or claims of vendor support.

## 13. Open design decisions for Firstmate to resolve with evidence
SOAP server architecture, extension API compatibility, session routing between /sdk and /vsanHealth, counter-schema extraction, fixture-sanitization format, history generation/cache policy, large payload streaming, collection benchmark harness, identity linking with VCF Operations native vCenter adapter.
