# Proposed Architecture

## Component map

```text
VCF Operations native vCenter adapter     Tier 2 Content Factory MP
               |                                     |
               +------------ TLS / 443 --------------+
                                  |
                          vCenter façade
                     /sdk          /vsanHealth
                      |                |
              govmomi/vcsim      vSAN SOAP adapter
                      |                |
                      +------ shared ---+
                          topology graph
                         identity registry
                        deterministic clock
                         metrics scenarios
                          query telemetry
                                 |
                       Lattice operator UI
```

This is the target architecture, not an assertion that stock vcsim exposes an extension interface suitable for /vsanHealth. Prove routing, SOAP serialization, vSAN type compatibility and session behavior in Phase 1. Consider a shared reverse proxy only if it preserves SOAP details, cookies, TLS, hostnames and fault shapes.

## Source of truth
One immutable versioned topology model backs both API surfaces. Objects have stable deterministic identities and relationships: datacenter, cluster, host, VM, storage, vDS, vSAN storage pool, NVMe device, native MoRefs and Performance Service entityRefIds. Store UUID namespaces and seed. Support create/clone/edit/reset operations with clear identity consequences; default restart preserves existing topology.

## Modules
- **Inventory engine:** topology generation, validation, stable object identifiers, per-cluster/host overrides, vm placement, datastore and network associations.
- **/sdk handler:** delegate to vcsim where compatible; supply necessary inventory properties, sessions, authentication, container views, and property collectors.
- **/vsanHealth handler:** register vSAN managed-object/type bindings and serve MP-required operations. Read-only by default; unsupported operations return faithful SOAP faults, not fake success.
- **Perf engine:** return synthetic but schema-correct five-minute buckets for requested wildcard entity family and time range. Determine metric source catalog from pinned upstream model and sanitized live captures.
- **Scenario engine:** controlled baseline/workload/failure/latency presets, correlated values, deterministic replay.
- **Admin API/UI:** private control plane, topology and scenario state, display cards, schema and compatibility coverage, request drill-down.
- **Observability:** Prometheus + structured traces of operations, family, request result and byte volume with secret redaction. No logging of authentication headers or session cookies.
- **Deployment:** standalone local container / compose, one Helm release per simulated vCenter, Argo-compatible values and ingress/TLS examples, optional PVC.

## Critical implementation decisions
1. **Session semantics:** `SmartConnect` authenticates once to /sdk and the vSAN binding uses `si._stub` and a separate /vsanHealth path. Test whether the actual MP's session cookie can be used across handlers, and preserve expiry/logoff/fault semantics as observed.
2. **vSAN model:** support `vim.cluster.VsanPerfQuerySpec`, metric series, sample dates, metric ID/value arrays, node information and advertised entities; use pyVmomi/vendored bindings as fixtures for compatibility, not arbitrary JSON.
3. **Payload design:** avoid preallocating all cardinality × metric × timestamp combinations. Compute seeded values lazily, with bounded caches and an explicit max response size/concurrency control. Preserve stable time bins.
4. **Consistency:** host-to-cluster and vSAN node UUID joins, VM instanceUuid, disk UUID/hardware properties, and datastore paths must agree across both API interfaces. Golden fixture validation should assert this.
5. **Concurrency:** protect topology snapshots during requests; expose latency profiles and bounded concurrency. Do not corrupt identities while collection is active.
6. **Security:** separate simulator auth plane from admin plane; create test credentials only. For live probes, require operator-provided secret-source instructions, read-only methods, capture sanitization, and no network mutations.
7. **Version:** advertise user-selected vCenter/ESX profile based on verified live responses; no fabricated build numbers.

## Alternatives to evaluate
- Extend vcsim directly with vSAN SOAP type handlers.
- Sidecar/reverse-proxy for /vsanHealth with shared session validation and one canonical topology store.
- Separate SOAP façade if vcsim cannot preserve required vSAN method dispatch.

Document licensing/dependency and maintenance implications before deciding. Prefer the least amount of new emulator logic that passes real MP compatibility tests.

## Scale-sensitive design
Per-family configuration (CPU/core objects, DOM worlds, VMs, disks, NICs, etc.) is first-class; cluster and host count alone underestimate response volume. Service should support fixed cardinality seed runs, large result pages, CPU/memory envelopes, slow-response injection, and request statistics for both /sdk and /vsanHealth. Export a manifest with run seed, versions, object/metric counts, scenario, API errors, raw benchmark metrics and MP/Operations version.
