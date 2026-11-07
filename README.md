# vCenter API Simulator

A configurable, deployable vCenter and vSAN ESA API simulator for **VCF Operations management-pack compatibility and scale testing**.

> Project status: Phase 1 (API compatibility milestone). No production-grade vCenter emulation is claimed.

## Mission
Emulate enough of a vCenter-managed inventory and the **vSAN Performance Service** to let the real VCF Operations vCenter adapter and custom Tier 2 management packs discover entities, query metrics, and ingest simulated telemetry. The first executable target is [nickfirefoot/vsanhostmetrics](https://github.com/nickfirefoot/vsanhostmetrics), being ported to a VCF Content Factory Tier 2 MP.

**Priority:** accurate API contract + many ESA clusters + high entity cardinality + observable collection performance. General-purpose vSphere simulation is a longer-term direction.

## Initial strategy
- Evaluate [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/vcsim) as the vSphere /sdk base. Extend it rather than reimplementing SOAP inventory if viable.
- Add vSAN /vsanHealth API handling grounded in live read-only API captures, especially `VsanPerfQueryPerf`, `VsanPerfQueryNodeInformation`, supported entity types, hardware/inventory joins, and correct SOAP fault behavior.
- Use one Helm release / public hostname per simulated vCenter initially. Many clusters may exist inside each instance; no runtime Kubernetes/ingress administration required.
- Produce stable inventory UUIDs / MoRefs and deterministic, 5-minute-bucket ESA telemetry. Support scale from a tiny compatibility fixture to 100+ clusters as a **test target**, not a claimed capacity.
- Build a small **Lattice**-styled operator UI with topology editor, generators, performance controls and API/collector observability. A mockup and independent end-user UX evaluation **precede UI implementation**.

## Phase 1: run the simulator against the reference management pack

The first milestone is a Go service on [govmomi vcsim](https://github.com/vmware/govmomi/tree/main/vcsim)
with an out-of-tree `/vsanHealth` extension. It serves one datacenter, one
vSAN ESA cluster of four hosts (two NVMe devices each, one claimed), one vDS
and four VMs on the vSAN datastore, and exactly the operations the reference
pack's collection path calls: `/sdk` login, inventory reads, `HostVsanSystem`,
and on `/vsanHealth` `VsanClusterGetConfig`, `VsanPerfQueryNodeInformation`,
`VsanPerfGetSupportedEntityTypes`, `VsanPerfQueryPerf`, `QueryVsanManagedDisks`
and `VsanQueryVcClusterHealthSummary`, sharing one session. Entity types,
label sets, fault shapes and the five-minute bucket rule come from the
sanitized live capture in [`fixtures/live/9.1.1`](fixtures/live/9.1.1/README.md).
Identities are derived from a seed, so a restart reproduces every MoRef, UUID
and metric value.

```sh
go build ./cmd/vcsimulator
./vcsimulator -listen 0.0.0.0:8443 -tls-dir ./data/tls -seed phase1
# add -listen 0.0.0.0:443 (needs CAP_NET_BIND_SERVICE) for the 1.4.7 pack line, which ignores the port
```

Any user name and password log in. The certificate in `-tls-dir` is created
once and kept, so the 2.0.0 pack's certificate pin survives restarts.

### Checks

| Check | How | Where it runs |
|---|---|---|
| Contract: every captured request replayed, responses compared by element structure, labels, bucket timestamps and fault type; identity joins across `/sdk` and `/vsanHealth`; determinism across instances and seeds; fixture sanitization | `go test ./...` | CI `go` job |
| The pack's own `perfsvc.connect/probe/collect/build_name_map/build_parent_map/build_disk_hardware` (pyVmomi 8.0.3.0.1, pack at the pinned commit) against a running simulator, both metric scopes, label sets equal to the live capture, every object named and parented, second collection identical, verify-on with the simulator's certificate pinned | `tools/mp-harness/harness.py` | CI `mp-collector` job |
| `mp-test connect` and `mp-test collect` twice through the Operations integration SDK 1.3.1 (builds and runs the pack's adapter container) | `tools/mp-harness/mp_test_run.py` | CI `mp-test` job (Docker) |

Locally, with the pack checked out at `af976fb9fe97bbac2fd94b666a3f79aa12656f2a`
of `sentania-labs/vsanhostmetrics`:

```sh
python3.12 -m venv .venv && .venv/bin/pip install pyvmomi==8.0.3.0.1 vmware-aria-operations-integration-sdk==1.3.1
# Python 3.12: the Operations SDK 1.3.1 does not install on 3.13 or newer
.venv/bin/python tools/mp-harness/harness.py --sim 127.0.0.1:8443 --mp ../vsanhostmetrics \
    --fixtures fixtures/live/9.1.1 --ca data/tls/cert.pem
PATH=.venv/bin:$PATH python3 tools/mp-harness/mp_test_run.py --mp ../vsanhostmetrics --sim-port 8443 --attach false
```

`mp-test` runs the adapter in a container and reaches the simulator through
the Docker bridge gateway, so bind the simulator to `0.0.0.0`. It needs a TTY
and gets a pty from the runner. `attach_to_vcenter` must be off under
`mp-test`, which has no Operations Suite API to resolve vCenter objects.

Out of scope for this milestone: UI, Docker and Helm packaging, OSA and
non-vSAN storage, more than one cluster, and any live vCenter access.

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
