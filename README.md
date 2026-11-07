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
| Contract: every captured request replayed, responses compared by element structure, labels, bucket timestamps and fault type; identity joins across `/sdk` and `/vsanHealth`; determinism across instances and seeds; fixture sanitization; login restriction | `go test ./...` | CI `ci` workflow, `go` job |
| Chart: `helm lint --strict` with every `charts/vcenter-api-simulator/ci/*-values.yaml`, `helm template` and `kubeconform` of the rendered manifests and the Argo CD example | `.github/workflows/package.yml` | CI `package` workflow, `chart` job |
| Image: build, OCI label and non-root check, `docker compose` boot, then a vSphere client smoke ([`tools/smoke/smoke.ps1`](tools/smoke/smoke.ps1)): `Connect-VIServer`, `Get-VMHost`, `Get-VM`, expected hosts and VMs back, wrong password refused | PowerCLI on `pwsh` | CI `package` workflow, `image` job |
| Chart install (manual): the chart in a disposable kind cluster with default values, the same smoke through the Service, the certificate unchanged across a pod restart, an upgrade to `tls.existingSecret` and restricted login that verifies against the issuing CA | [`tools/smoke/helm-kind.sh`](tools/smoke/helm-kind.sh) | Optional: run manually (needs `kind`, `helm`, `kubectl`, `openssl`, Docker, `pwsh`), not required CI |
| The pack's own `perfsvc.connect/probe/collect/build_name_map/build_parent_map/build_disk_hardware` (pyVmomi 8.0.3.0.1, pack at the pinned commit) against a running simulator, both metric scopes, label sets equal to the live capture, every object named and parented, second collection identical, verify-on with the simulator's certificate pinned | `tools/mp-harness/harness.py` | Optional: `mp-acceptance` workflow (`gh workflow run mp-acceptance.yml`), not required CI |
| `mp-test connect` and `mp-test collect` twice through the Operations integration SDK 1.3.1 (builds and runs the pack's adapter container) | `tools/mp-harness/mp_test_run.py` | Optional: `mp-acceptance` workflow, not required CI |

Smoke a simulator you are running yourself (installs VMware.PowerCLI for
the current user when missing):

```sh
pwsh tools/smoke/smoke.ps1 -Server 127.0.0.1 -Port 8443 -Install
```

The management pack checks run locally with the pack checked out at
`af976fb9fe97bbac2fd94b666a3f79aa12656f2a` of `sentania-labs/vsanhostmetrics`:

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

Out of scope for this milestone: UI, OSA and non-vSAN storage, more than one
cluster, and any live vCenter access.

## Running it

The image is `ghcr.io/sentania-labs/vcenter-api-simulator`, built from
[`Dockerfile`](Dockerfile) on every push to `main` (tags `main` and
`sha-<commit>`) and on every `vMAJOR.MINOR.PATCH` tag (that tag, plus
`latest` for the highest version). It is a static binary on a distroless
non-root base, labeled with the source commit
(`org.opencontainers.image.revision`) and version. Pull requests build and
test the image without publishing it.

GHCR creates the package private on first publish. To pull it without
credentials (what the compose file and the chart's defaults assume), set it to
public once under the package's settings on GitHub. To keep it private, give
the chart an `imagePullSecrets` entry naming a registry pull secret.

Login accepts any user name and password unless `VCSIM_USERNAME` and
`VCSIM_PASSWORD` (or `-username`/`-password`) are set, in which case only
that pair logs in. Use placeholders: this is a simulator, never point real
credentials at it.

### docker run

```sh
docker run -d --name vcsim -p 8443:8443 -v vcsim-tls:/data/tls \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  ghcr.io/sentania-labs/vcenter-api-simulator:main
curl -sk https://localhost:8443/sdk/vimServiceVersions.xml
```

The certificate is created on first start and kept in the volume mounted at
`/data/tls`, so the fingerprint a management pack pinned survives restarts.
Add `-tls-names=<hostname>` (as a command argument) to put the name
collectors use into it, and `--read-only` needs the `/tmp` tmpfs: vcsim keeps
scratch files there.

### docker compose

[`docker-compose.yml`](docker-compose.yml) runs the same thing with a named
volume for the certificate:

```sh
docker compose up -d                 # pulls :main
docker compose up -d --build         # builds from this checkout
VCSIM_USERNAME=operator VCSIM_PASSWORD=placeholder docker compose up -d   # restricted login
docker compose down -v               # also discards the certificate
```

`VCSIM_TAG`, `VCSIM_PORT`, `VCSIM_SEED` and `VCSIM_TLS_NAMES` are the other
knobs.

### Helm

[`charts/vcenter-api-simulator`](charts/vcenter-api-simulator) installs one
simulated vCenter per release. It creates a Deployment (one replica, non-root,
read-only root filesystem, no capabilities), a ClusterIP Service on 443, a
ServiceAccount with no token mounted, a PVC for the certificate, and
optionally a Secret for credentials, an Ingress for `/sdk` and `/vsanHealth`
and a cert-manager Certificate. No RBAC, no privileges, nothing exposed
outside the cluster until `ingress.enabled` or a LoadBalancer Service asks
for it.

```sh
helm install vcsim-01 charts/vcenter-api-simulator \
  --set hostname=vcsim-01.example.test \
  --set credentials.enabled=true --set credentials.existingSecret=vcsim-01-login \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set-string 'ingress.annotations.nginx\.ingress\.kubernetes\.io/backend-protocol=HTTPS' \
  --set ingress.tls.enabled=true --set ingress.tls.secretName=vcsim-01-ingress-tls
```

Certificate options, in order of precedence:

- `tls.existingSecret`: a `kubernetes.io/tls` Secret to serve as is.
- `tls.certManager.enabled`: a cert-manager Certificate for `hostname` (and
  `simulator.tlsNames`) from `tls.certManager.issuerRef`. A renewal changes
  the fingerprint a pack pinned.
- Neither: a self-signed certificate created on first start, kept in the
  PVC (`persistence.enabled`, default on; the PVC is annotated to survive
  `helm uninstall` and Argo CD prune or Application deletion). With
  persistence off every new pod issues a new certificate.

A replaced certificate (a rotated Secret, a cert-manager renewal) is served
to the next connection without a restart: the simulator re-reads the mounted
files when they change.

The Ingress backend is HTTPS; tell the controller
(`nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"` for ingress-nginx).
When the Ingress terminates TLS, collectors pin the ingress certificate, not
the simulator's.

[`charts/vcenter-api-simulator/ci`](charts/vcenter-api-simulator/ci) holds
example values for the Ingress, an existing TLS Secret, cert-manager, and no
persistence; `helm lint` runs each of them.

### Argo CD

[`examples/argocd-application.yaml`](examples/argocd-application.yaml) is
one Application per simulated vCenter, pointing at the chart in this
repository with the hostname, seed, credentials Secret name and Ingress in
`valuesObject`. Copy it, change the name, hostname and seed, and it is
another endpoint. The chart needs nothing cluster-wide, so the Application
fits any project that may create a Deployment, Service, PVC and Ingress in
its namespace.

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
