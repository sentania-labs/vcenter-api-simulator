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

Phase 1 proved the routing: one vcsim service serves `/sdk` and a registered `/vsanHealth` registry from one session store. Phase 2 built the rest of the left-hand column, as implemented:

| Box | Package | What it is |
|---|---|---|
| topology graph | `internal/scenario`, `internal/topology` | the operator's scenario file ([reference](SCENARIO.md)) materialized into one `Topology` with per-cluster seeded variation |
| identity registry | `internal/registry` | `registry.json`: logical key to serial, never reused; uuids are UUID v5 of (seed namespace, kind, serial) |
| deterministic clock | `internal/perf` | the five-minute bucket rule measured live; values as a pure function of (telemetry seed, entity, label, bucket) inside each label's captured envelope; cumulative `Raw` counters start at the envelope minimum and only climb, never wrapping, so they may pass its maximum |
| metrics scenarios | `internal/vsanhealth` (`Knobs`) | per-family and per-method faults with cluster and bucket-aligned time selectors; per-family and `/sdk` latency |
| vCenter façade | `internal/sim` | builds the vcsim inventory in registry serial order through vcsim's own method handlers (an event-driven task waiter instead of the property collector's poll), shapes cluster, host, datastore and VM properties after the trimmed live inventory capture, enforces credentials |

The control plane (`internal/control`) owns the served scenario, the running vCenter, the request log (`internal/activity`), the list of connected tools and the changes scheduled for later; `internal/admin` serves its JSON API and the embedded Lattice console on a separate address, never on the API listeners. Every change the console makes is a task (`task*.go`): one function plans it from the form's values, producing both the sentences shown as "What will happen" and the change itself, so the screen cannot promise one thing and do another. A change is made when its button is pressed, or planned again and made at a time later the same day; there is no shared draft and no lock. A change to clusters, hosts, storage or versions builds a new `sim.Simulator` next to the running one and swaps them (the old one drains for 30 s); slowness and faults (`scenario.Condition`, evaluated by `vsanhealth.Knobs`), users, the certificate and the idle sign-out change in place.

Two pieces exist so that every screen says the same true thing. `activity.Classify` gives each request exactly one reason for the way it was answered (answered, empty, refused as a real vCenter refuses it, failed by a fault that was switched on, paused, sign-in refused, or unknown to the simulator); the Requests headline, Home and request capture are all counted from it. The toolbook (`tools.go`) is the one place that knows which tools are connected, as which account, and if not, since when and why. Prometheus export is not built yet.

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
