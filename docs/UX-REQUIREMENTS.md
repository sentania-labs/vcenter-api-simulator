# UX Requirements — Lattice Operator Panel

> **Superseded for the console's screens (2026-11-12).** The console was redesigned around the jobs a person does with it and rebuilt from an owner-approved design: eight tabs (Home, Clusters, Storage, Versions, Users, Security, Slowness and faults, Requests), one job per screen, no shared draft. The standing test for any console change is the walkthrough: a person who has never seen the code does each job, start to finish, in the running app, reading what is on the screen once, without opening collapsed sections, long scrolling, or guessing a value, and can tell afterwards that it worked. The sections proposed below describe the first console and are kept as the record of what it was asked to do. [docs/console/README.md](console/README.md) shows the current screens.

## Intent
Operator controls a *single simulated vCenter service* and many clusters without manually editing YAML. A single instance must be intelligible at 1 cluster and 100+ clusters. This is an engineering test instrument, not a general vSphere management UI.

## Pre-implementation gate: mandatory
Before implementing any UI components, produce:
1. A desktop-width information architecture and annotated navigation/wireframes.
2. A **high-fidelity screenshot/mockup of the complete main screen** in the user's established Lattice design language. Include genuine example topology values and controls, not generic marketing art.
3. Mockups for editing one nonuniform cluster and the active benchmark / query-inspection state.
4. Independent **end-user evaluation agent** simulating realistic tasks with an explicit, justified UX rubric (discoverability, clarity, efficiency, error prevention, density, accessibility, consistency). Record tasks, predicted failures, severity, proposed improvements and rerun evaluation after revisions.
5. Operator approval of the mockups. No frontend implementation before approval.

If the Lattice design system is not present in this repo, ask the operator where its tokens/components/reference UI can be found; do not invent brand tokens while claiming compliance. Use accessible color contrast, keyboard support and tabular numbers.

## Proposed sections
- **Overview:** endpoint hostname, simulated vCenter/version, running/paused, clusters/hosts/VMs/disks/vDS/datastores, entities and metric count estimates, API query rate/errors, active scenario, config seed.
- **Topology:** datacenter → cluster → host tree/table. Quick generator and per-cluster edits. Include vSAN ESA mode, disks, NICs, CPU/world cardinality, VM/disks, datastores and network attachments, consistency warnings.
- **Telemetry:** scope selection (dashboard/all), five-minute sample preview, profiles, scenario timeline, latency/failure settings and history controls.
- **API Activity:** /sdk and /vsanHealth request log (redacted), entity-family totals, duration distribution, response bytes, faults, filtering by cluster and family.
- **Benchmark Runs:** start/stage/stop benchmarks, reproducible scenario manifest, compare runs and export results.
- **Compatibility:** method matrix, verified vs unverified behavior, fixture coverage and MP collection health.

## Editing safeguards
Separate **Apply**, **Regenerate**, **Reset seed**, and **Delete**. Clearly warn when object identities will change or counts will fan out into huge resource totals. Do not reset telemetry accidentally when tuning latency. Maintain a read-only inspection path for the API.

## Example workflows for UX-agent walk-through
A. Generate 50 ESA clusters × 8 hosts with 2 NVMe devices/host and review projected entity/metric cardinality.
B. Override one cluster to 4 hosts and one to 16 hosts without affecting other clusters.
C. Switch to dashboard-only metric scope and view expected query reduction.
D. Apply 200 ms synthetic API latency with one family fault and inspect collector requests.
E. Restart a simulation, verify stable inventory identifiers and export a benchmark manifest.
F. Find the reason an MP collection returned zero metrics while authentication succeeded.

## Success rubric
Target an independently reasoned UX score ≥ 85/100, with **zero unresolved high-severity workflow blockers**. Score is a design-assessment target, not proof of real usability. Verify with human operator review and later usability testing. Ensure an experienced operator can find topology override and API request detail without guessing.
