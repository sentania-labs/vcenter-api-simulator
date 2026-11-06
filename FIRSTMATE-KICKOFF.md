# Firstmate Kickoff — vCenter API Simulator

Copy the following to Firstmate as an orchestration task. This is the controlling instruction, with supporting engineering artifacts under docs/.

## Mission
Build an open-source configurable vCenter / vSAN ESA API simulator at https://github.com/sentania/vcenter-api-simulator. Its first job is **scale testing a Tier 2 VCF Content Factory management pack** being ported from https://github.com/nickfirefoot/vsanhostmetrics against a real VCF Operations deployment. Prioritize vSAN Performance Service emulation, very large ESA cluster counts, high-cardinality response objects, realistic 5-minute metric buckets, and reproducible measurement of end-to-end collection.

Read README.md, docs/SPECIFICATION.md, docs/ARCHITECTURE.md, docs/ROADMAP.md, docs/TESTING.md and docs/UX-REQUIREMENTS.md thoroughly. Inspect the current upstream MP source and pin the exact commit SHA. Do not substitute a generic mocked HTTP API for a SOAP-compatible endpoint.

## FIRST ACTION — STOP AND ASK THE OPERATOR
**Before assigning any worker a task that accesses the live vCenter**, ask me explicitly:

> "Where should the worker obtain a read-only vCenter credential for API discovery? Please provide the approved secret source/access method (for example an existing vault item, injected environment variables or an authorized runner secret), along with the vCenter endpoint and any network/access constraints. Do not paste the password, token or private key into this chat or the repository."

Wait for this answer **before live probing**. The workers may start *offline* source inspection and design in parallel. Do not infer a credential source from machine state, agent memory or another project. Never echo secrets, embed them in Git commits, tests, fixtures, issue text, logs or agent handoffs. Confirm that the credential is genuinely read-only; only perform allowlisted discovery/metric queries. If live endpoint details or privilege boundaries are missing, ask before contacting it.

Also ask where to obtain the **Lattice design reference** if its actual design tokens/components are not already available.

## Implementation and research instructions
1. Establish read-only acquisition plan and preflight safety; inspect the upstream MP: particularly app/perfsvc.py, app/perfsvc_model.py, app/metric_scope.py, app/adapter.py, docs/COLLECTION-DESIGN.md, and requirements. Extract exact SOAP operations, UUID joins, entityRefIds, metric catalogs, unit mappings, fault modes and collection frequency.
2. Assign a compatibility/research worker to evaluate govmomi/vcsim and the feasibility of serving /sdk plus /vsanHealth together. Probe live vCenter 9.1.1-era behavior read-only **only after** the credential gate. Produce sanitized response fixtures, a method-coverage matrix, and an architecture decision record. Redact and operator-review before publishing any live fixtures.
3. In parallel, assign a topology/data worker to propose a unified per-vCenter/datacenter/cluster/host/VM/datastore/vDS/vSAN ESA graph with stable MoRefs, cluster/host/vSAN/disk/VM UUIDs, deterministic per-family cardinality and history. Preserve relationships across restart. Propose load/payload sizing and lazy sample generation.
4. Assign a UX design worker to deliver Lattice-based **full-desktop mockups** of Overview, topology editor and benchmark/API activity. **Before coding frontend**, run an independent end-user UX evaluation agent through realistic operator tasks using an explicit quantitative rubric and revise to ≥85/100 with no unresolved critical issues. Present the mockups and evaluation findings to me and require approval.
5. Implement only the minimum SOAP-compatible vertical slice required to satisfy the active MP collector: /sdk auth/inventory plus /vsanHealth `VsanPerfQueryPerf` with wildcard entity and UTC time window, node names, disk/host joins and needed metadata. Validate with real MP `mp-test` and compare output against sanitized golden fixtures. Do not mark success based solely on cURL or HTTP 200.
6. Extend to many distinct ESA clusters with independent host counts, CPU/world/disk/network/VM cardinalities and seeded metric profiles. Track timestamps, returned bytes, query latency/error/throughput, peak CPU and memory. Implement benchmark/replay harness and reproducible run manifest.
7. Run full integration against actual VCF Operations / Tier 2 MP if an approved test installation is available; capture resource discovery and metric ingestion counts, DATA_RECEIVING, collection duration and no relationship corruption. If unavailable, explicitly record this as an unverified gate; do not claim end-to-end compatibility.
8. Package Docker developer mode and one-vCenter-per-release Helm chart compatible with Argo CD, avoiding runtime cluster-admin privileges and ingress provisioning. Admin UI private by default.

## Work style
Use bounded work packets and delegated agents. For each stage produce objective artifacts/tests and a PR. The process is **Build → Self-review (independent subagent) → Address findings → PR → External review (one round) → Address findings → CI pass → Merge → Tag release**. Avoid enormous unreviewable changes. Frontend design is subject to the explicit mockup/UX/operator approval gate; live-vCenter access is subject to the explicit credential gate.

## First report to me
Return:
- The read-only credential-source question (immediately) and a note that live probing is blocked pending my answer.
- Exact upstream MP SHA and API inventory from offline source inspection.
- vcsim versus alternate architecture proposal, major compatibility risks.
- The minimal API vertical slice and its objective acceptance tests.
- Proposed issue/PR sequence aligned with docs/ROADMAP.md.
- Lattice mockup plan and UX-agent rubric, without implementing the UI yet.

Treat project completion as measured success against the real management pack and VCF Operations, not generic feature completeness. Preserve the longer-term goal of generic vSphere, ESX, vSAN, datastores, vDS and VM simulation, but avoid spending MVP effort on unrelated APIs.
