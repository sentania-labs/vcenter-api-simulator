# Control console screenshots

Taken from the running app (light and dark, 1440 × 900) against a 50-cluster, 396-host scenario with one pending topology edit and one pending latency edit. Built from the owner-approved Rev 2.1 mockups with Lattice tokens as shipped.

| Screen | Light | Dark |
|---|---|---|
| Overview | [01-overview-light.png](01-overview-light.png) | [01-overview-dark.png](01-overview-dark.png) |
| Topology: quick generator and presets | [02-generator-light.png](02-generator-light.png) | [02-generator-dark.png](02-generator-dark.png) |
| Topology: cluster editor | [03-topology-editor-light.png](03-topology-editor-light.png) | [03-topology-editor-dark.png](03-topology-editor-dark.png) |
| Topology: review before apply | [04-review-light.png](04-review-light.png) | [04-review-dark.png](04-review-dark.png) |
| Telemetry | [05-telemetry-light.png](05-telemetry-light.png) | [05-telemetry-dark.png](05-telemetry-dark.png) |
| API Activity | [06-api-activity-light.png](06-api-activity-light.png) | [06-api-activity-dark.png](06-api-activity-dark.png) |
| API Activity: zero-metrics diagnosis | [07-zero-metrics-light.png](07-zero-metrics-light.png) | [07-zero-metrics-dark.png](07-zero-metrics-dark.png) |
| Compatibility | [09-compatibility-light.png](09-compatibility-light.png) | [09-compatibility-dark.png](09-compatibility-dark.png) |
| Confirm: Regenerate | [12-confirm-regenerate-light.png](12-confirm-regenerate-light.png) | [12-confirm-regenerate-dark.png](12-confirm-regenerate-dark.png) |
| Confirm: Reset cluster seed | [12-confirm-reset-seed-light.png](12-confirm-reset-seed-light.png) | [12-confirm-reset-seed-dark.png](12-confirm-reset-seed-dark.png) |
| Confirm: Delete cluster | [12-confirm-delete-light.png](12-confirm-delete-light.png) | [12-confirm-delete-dark.png](12-confirm-delete-dark.png) |
| Confirm: Delete fault rule | [11-confirm-delete-rule-light.png](11-confirm-delete-rule-light.png) | [11-confirm-delete-rule-dark.png](11-confirm-delete-rule-dark.png) |

## Departures from the mockups

Only where the backend has no data or behaviour for it:

- **Benchmarks** (screen 08, the `bm-…` chips, the "Hold / Leave running / Stop" options, the stop-then-apply confirm): no benchmark feature exists yet (roadmap Phase 4).
- **Storage editing**: the per-cluster editor sets the vSAN architecture (ESA or OSA) and whether the performance stats primary is online, and the generator picks an architecture. Shared NFS datastores are edited per datacenter, and iSCSI storage per cluster, in Topology (see [SCENARIO](../SCENARIO.md)). "Add datastore", FC counts, capacity and HA/DRS/sockets × cores/vmknics fields are omitted; the cluster storage table is read-only. Cluster names are generated `cluster-NN`, so there is no name pattern, UUID namespace or MoRef start.
- **Telemetry**: workload profiles, history depth, clock offset, the 5-minute sample preview, max concurrency, per-cluster latency, per-rule on/off, rate and path scoping, and the "drop connection" fault do not exist in the engine. Jitter is uniform `0..N` ms, shown as `+0–N`, not `±N`. The dashboard scope is 35 kinds (the recon report's list plus the ESA dedup kinds), not the mockup's 33, and is labelled as carried in the console, not read from the pack.
- **API Activity**: no history-depth/restart diagram, backfill, recent-cycles table, "Same cycle", "Copy as fixture" or "Open rule". The trace column is the call's sequence number. Request bodies are kept for `/vsanHealth` only, never `/sdk`. Authentication refusals are their own `denied` result.
- **Compatibility**: a static matrix of what is verified against the captured fixtures, plus counts seen in traffic; no fixture browser.
- **Topology**: no topology log, so a discarded draft cannot be restored; the review has no cluster-tree column and adds a "Discard draft…" button. Pages scroll rather than fitting 900 px.
- **Added**: scenario presets, a scheduled-apply banner with Cancel, a paused banner, a persistent apply-failure banner.

Removing a cluster, host or VM keeps the MoRefs of everything that remains, so the review does not warn about renumbering. See [SCENARIO](../SCENARIO.md#identity-seed-registry-regenerate).

## Independent UX walkthrough

An evaluation agent that had not seen the code walked workflows A to F of [UX-REQUIREMENTS](../UX-REQUIREMENTS.md) against the built UI three times:

| Round | Score | High | Notes |
|---|---:|---:|---|
| 1 | 68 | 3 | review said other clusters were unaffected when MoRefs were renumbered (since fixed: removal no longer renumbers); lost clicks from a periodic re-render; a telemetry edit bumped the topology version |
| 2 | 76 | 0 | 15 medium findings (contradictory identity table, focus loss, 1024 px layout, auth failures counted as faults, …) |
| 3 | 70 | 3 | diagnosis merged two collections; an apply that failed because the disk was full was only a toast; MoRef renumbering on removal (since fixed) |

After each round the findings were fixed (each fix has a test where it is logic, and the third round's diagnosis and failure-banner findings are fixed), except per-cluster latency (removal-driven MoRef renumbering was fixed afterwards). The target of 85 with no high finding was **not reached in any round**, and the last fixes were not re-scored. The score is a design-assessment target, not proof of usability; it needs the owner's review.
