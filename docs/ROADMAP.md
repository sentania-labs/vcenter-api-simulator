# Development Roadmap

This is an outcome-based roadmap; stages have exit gates, not dates. Build an ESA collector target first, then expand into a general vCenter API simulator.

| Phase | Deliverable | Exit gate |
|---|---|---|
| 0 — Recon | Lock reference MP commit; inspect exact API calls and schemas; arrange read-only worker access; capture sanitized golden fixtures; assess vcsim | API contract, fixture inventory, risk matrix, design proposal reviewed |
| 1 — Integration spike | Minimal /sdk + /vsanHealth endpoint, session/identity sharing, 1 cluster / 4 hosts / ESA disks, correct Performance Service response | Actual MP `mp-test` and a repeat query pass; no invented API fields |
| 2 — Functional simulator | Deterministic topology engine, per-entity cardinality, seeded five-minute history, auth/TLS, failure handling | Real MP collects repeatedly, stable object identities, MP and source metrics match |
| 3 — First end-to-end | Register simulated endpoint against real VCF Operations and Content Factory Tier 2 MP; verify discovered resource relationships and ingestion | 2+ collection cycles, expected resource and metric counts, no destructive relationships |
| 4 — Scale lab | Profiles for 10/50/100+ ESA clusters, query logs and exportable benchmark bundles, adjustable latency/errors, replay harness | Reproducible throughput/latency/RSS reports with tested ceiling |
| 5 — Control UI | Lattice prototype, operator approval, independent UX walkthrough, topology editor, query observability and scenario presets | UX evaluation completed; operator can configure and run scenarios without editing code |
| 6 — Delivery | Docker, Helm, Argo CD example, persistent identity/seeds, Prometheus, documentation and CI | Installation/restart/upgrade/release acceptance tests |
| 7 — Expansion | vSphere perf counters, richer vDS/VMFS/NFS, generic vCenter adapter realism, multi-instance management | Additional MP scenario demonstrates value; separate planning review |

Phase 3's first collection gate was demonstrated on 2026-11-07 as a standalone
adapter test; native-vCenter attachment and the Java Content Factory pack remain
unverified. See the [acceptance procedure and result](OPERATIONS-ACCEPTANCE.md).

Phase 4's scale profiles and replay harness are in place and the replay was
tested to 400 clusters on a shared host under a 4 GiB cap without reaching a
failure criterion; the ceiling was not found. See the
[measured results](../tools/bench/README.md#measured-results).

## Immediate worker work packets

1. **Discovery:** write a read-only probe plan, obtain credential-source direction from user, inspect reference repository at pinned SHA.
2. **Compatibility table:** operation, request/response SOAP type, owning library, MP call site, available fixture, gap in vcsim.
3. **Architecture decision record:** extend vcsim vs alternative, integration and licensing concerns, deterministic simulation data model and session semantics.
4. **Mockup gate:** Lattice-based full desktop control panel mockup and UX-agent evaluation before any front-end implementation.
5. **Spike PR:** smallest working simulated Performance Service endpoint and contract tests.
6. **MP verification:** use actual MP against fake endpoint, then VCF Operations when available.
7. **Scale profiles:** independent cardinality matrices, benchmarking harness, measured results.

## Engineering workflow
Build → independent self-review → address findings → PR → external review (one round) → address findings → green CI → merge → tag release. No skipping external review gate. Break large milestones into reviewable PRs. Capture objective evidence in PR descriptions.

## Decision gates
Do not implement broad UI or full generic vCenter support before confirming the MP API contract. Do not probe live endpoints before approved credential sourcing. Do not describe anticipated scale as measured scale. Stop and escalate when a required SOAP operation is destructive or when fixture sanitization is uncertain.

## Future work
Support topology import from *sanitized* read-only live inventory; workload scenario playback; sampled query latency histograms; MP memory/cpu pressure experiments; profile comparison dashboards; optional multiple Helm releases managed by existing Argo workflows instead of in-app Kubernetes administration.
