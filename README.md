# vCenter API Simulator

A configurable, deployable vCenter and vSAN ESA API simulator for **VCF Operations management-pack compatibility and scale testing**.

> Project status: specification / discovery. No production-grade vCenter emulation is claimed.

## Mission
Emulate enough of a vCenter-managed inventory and the **vSAN Performance Service** to let the real VCF Operations vCenter adapter and custom Tier 2 management packs discover entities, query metrics, and ingest simulated telemetry. The first executable target is [nickfirefoot/vsanhostmetrics](https://github.com/nickfirefoot/vsanhostmetrics), being ported to a VCF Content Factory Tier 2 MP.

**Priority:** accurate API contract + many ESA clusters + high entity cardinality + observable collection performance. General-purpose vSphere simulation is a longer-term direction.

## Initial strategy
- Evaluate [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/vcsim) as the vSphere /sdk base. Extend it rather than reimplementing SOAP inventory if viable.
- Add vSAN /vsanHealth API handling grounded in live read-only API captures, especially `VsanPerfQueryPerf`, `VsanPerfQueryNodeInformation`, supported entity types, hardware/inventory joins, and correct SOAP fault behavior.
- Use one Helm release / public hostname per simulated vCenter initially. Many clusters may exist inside each instance; no runtime Kubernetes/ingress administration required.
- Produce stable inventory UUIDs / MoRefs and deterministic, 5-minute-bucket ESA telemetry. Support scale from a tiny compatibility fixture to 100+ clusters as a **test target**, not a claimed capacity.
- Build a small **Lattice**-styled operator UI with topology editor, generators, performance controls and API/collector observability. A mockup and independent end-user UX evaluation **precede UI implementation**.

## Documents
- [Kickoff specification](docs/SPECIFICATION.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Roadmap](docs/ROADMAP.md)
- [Testing and benchmarks](docs/TESTING.md)
- [UX requirements](docs/UX-REQUIREMENTS.md)
- [Firstmate orchestration kickoff](FIRSTMATE-KICKOFF.md)

## Guardrails
**No live-vCenter access until the operator identifies how the worker should obtain a read-only credential.** Never request that the operator paste passwords or tokens into an issue, repository, PR, chat transcript, logs, or agent handoff. Do not commit raw customer/lab inventory captures. Sanitize and review fixtures before publishing.

This simulator is designed for a controlled lab; never advertise it as VMware/Broadcom vCenter, ESX or vSAN software or a supported emulator. See [SPECIFICATION](docs/SPECIFICATION.md) for explicit validation gates.

## References
- https://github.com/nickfirefoot/vsanhostmetrics
- https://github.com/vmware/govmomi/tree/main/vcsim
