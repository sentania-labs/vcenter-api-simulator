# vCenter API Simulator

A configurable, deployable vCenter and vSAN ESA/OSA API simulator for **VCF Operations management-pack compatibility and scale testing**.

> Project status: Phase 3 (first real Operations collection verified with the Python vSAN Host Metrics pack) with the Phase 5 control console. No production-grade vCenter emulation is claimed.

## Mission
Emulate enough of a vCenter-managed inventory and the **vSAN Performance Service** to let the real VCF Operations vCenter adapter and custom Tier 2 management packs discover entities, query metrics, and ingest simulated telemetry. The first executable target is [nickfirefoot/vsanhostmetrics](https://github.com/nickfirefoot/vsanhostmetrics), being ported to a VCF Content Factory Tier 2 MP.

**Priority:** accurate API contract + many ESA clusters + high entity cardinality + observable collection performance. General-purpose vSphere simulation is a longer-term direction.

## Initial strategy
- Evaluate [govmomi/vcsim](https://github.com/vmware/govmomi/tree/main/vcsim) as the vSphere /sdk base. Extend it rather than reimplementing SOAP inventory if viable.
- Add vSAN /vsanHealth API handling grounded in live read-only API captures, especially `VsanPerfQueryPerf`, `VsanPerfQueryNodeInformation`, supported entity types, hardware/inventory joins, and correct SOAP fault behavior.
- Use one Helm release / public hostname per simulated vCenter initially. Many clusters may exist inside each instance; no runtime Kubernetes/ingress administration required.
- Produce stable inventory UUIDs / MoRefs and deterministic, 5-minute-bucket ESA telemetry. Support scale from a tiny compatibility fixture to 100+ clusters as a **test target**, not a claimed capacity.
- A small **Lattice**-styled control console with topology editor, generators, performance controls and API/collector observability (built from owner-approved mockups after an independent end-user UX evaluation).

## Phase 2: a functional simulator

The simulator is a Go service on [govmomi vcsim](https://github.com/vmware/govmomi/tree/main/vcsim)
with an out-of-tree `/vsanHealth` extension. A **scenario file** describes
the simulated vCenter: datacenters, vSAN ESA and OSA clusters with their own
host, device, VM, CPU, DOM-world and uplink counts, who may log in, the
certificate, and failure and latency knobs. An **identity registry** in the
data directory keeps every MoRef and uuid stable across restarts and
scenario edits. The Performance Service serves every entity family the
sanitized live capture ([`fixtures/live/9.1.1`](fixtures/live/9.1.1/README.md))
showed, with five-minute buckets and gauge values that stay inside the envelope
the live service showed for each metric (cumulative `Raw` counters only climb
from the observed minimum and never wrap).

```sh
go build ./cmd/vcsimulator
./vcsimulator -scenario examples/scenarios/ten-clusters.yaml -data ./data -listen 0.0.0.0:8443
# add -listen 0.0.0.0:443 (needs CAP_NET_BIND_SERVICE) for the 1.4.7 pack line, which ignores the port
```

| Flag | Meaning |
|---|---|
| `-scenario FILE` | the scenario ([reference](docs/SCENARIO.md)); without it the built-in one-cluster Phase 1 shape runs under `-seed` |
| `-data DIR` | holds `registry.json` (identities) and `tls/` (the self-signed certificate, created once); default `$VCSIM_DATA`, else `data`; the image sets `VCSIM_DATA=/data`, its state volume |
| `-describe` | print the scenario's cardinality (clusters, hosts, VMs, devices, per cluster) as JSON and exit |
| `-query-log PATH` | append one JSON line per request (method, path, operation, status, bytes, duration) to `PATH`, created owner-only; SOAP request bodies are included except for `SessionManager` operations, so it can hold inventory identifiers; see [scale replay](tools/bench/README.md) |
| `-regenerate` | discard the registry; required after a seed change (identities change only if the seed did) |
| `-tls-cert`, `-tls-key` | serve a provided certificate instead of the self-signed one (the scenario's `tls:` does the same) |
| `-session-timeout` | idle session timeout; sessions never expire by default |
| `-listen ADDR` | repeatable; default `0.0.0.0:8443` |
| `-admin ADDR` | the control console (plain HTTP, no login); default `127.0.0.1:9090`, env `VCSIM_ADMIN`; empty turns it off |
| `-admin-allowed-hosts NAMES` | comma-separated Host names the console answers to besides `localhost`, `127.0.0.1` and `::1` (any port); every other Host gets 403, which stops a web page from reaching the console through DNS rebinding. Add the name your proxy or ingress forwards; env `VCSIM_ADMIN_ALLOWED_HOSTS` |

What the scenario controls, and what the simulator guarantees:

- **Topology.** Any number of datacenters and clusters. Each cluster draws
  its own counts from the file's `vary` distributions with its own seed, so
  ten generated clusters differ; explicit `clusters:` overrides pin any of
  them. One vDS per datacenter with management, vMotion, vSAN and VM
  portgroups; every host has management, vMotion and vSAN vmkernel NICs,
  a local VMFS datastore per unclaimed NVMe device, and the cluster's vSAN
  datastore.
- **Identity.** uuids are UUID v5 of (seed namespace, kind, registry
  serial); the registry also reserves MoRefs. A restart on the same data
  directory and a removal preserve the identities of surviving entities.
  A seed change plus `-regenerate` changes identities.
- **Metrics.** Values are a pure function of (telemetry seed, entity,
  label, bucket); nothing is stored. Each label stays inside the min..max
  the live capture showed for it, follows a business-hours day and drifts
  smoothly; labels the live service held constant stay constant.
- **Auth and TLS.** Configured users are enforced on `/sdk` (password
  login; the token and certificate logins vcsim would accept from anyone
  are refused), and `/vsanHealth` shares the session (no cookie,
  `NotAuthenticated`). With no users configured any name and password log
  in. The certificate persists,
  so the 2.0.0 pack's certificate pin survives restarts.
- **Faults and latency.** Per-family or per-method SOAP faults
  (`invalid-argument`, `not-supported`, `system-error`, `timeout`) by
  cluster and time window; per-family and `/sdk` latency with jitter.

Known gaps: the vDS is served under vcsim's base type
`DistributedVirtualSwitch` rather than `VmwareDistributedVirtualSwitch`;
populated iSCSI target service, file service, NFS/FC/iSCSI datastores and
successful REST inventory reads are not modeled (see [ROADMAP](docs/ROADMAP.md)).
The OSA health summary covers cluster and host status and versions; many
additional live health-detail fields are not yet modeled. The real 9.1.1
server returns full documents for `/sdk/vimService.wsdl` and `/sdk/vim.wsdl`.
The simulator serves the captured service descriptor but does not serve
`vim.wsdl`; a complete sanitized capture is needed to add it.

### Control console

Open `http://127.0.0.1:9090/` (the `-admin` address). The console is the
way to change a running simulator without editing YAML: it shows the
service, projects what a topology costs a collector per five-minute cycle,
edits clusters and the generator, loads scenario presets, sets latency and
fault rules, and shows every request the simulator served. It follows the
Lattice design system, in light and dark. It answers only to the Host names
`localhost`, `127.0.0.1` and `::1` (any port, so a port-forward and
`docker compose` work) plus those in `-admin-allowed-hosts`; behind a proxy,
add the name the proxy forwards. Helm: `admin.allowedHosts`; the console
listens on the pod's loopback unless `admin.exposeToPodNetwork` is set.

- **Nothing reaches the API by accident.** Every edit goes into one pending
  draft. *Review & apply* shows what is kept, added and removed (hosts,
  disks, VMs, cluster identities) and applies at the next five-minute
  bucket boundary, or immediately if you ask. Latency and fault edits swap
  in place: the vCenter, its sessions and its history are untouched. A
  topology change builds a new vCenter next to the old one and swaps them;
  sessions end at the swap, as after a vCenter restart.
- **Identity actions are separate and confirmed.** *Regenerate* gives one
  cluster new identities under the same seed; *Reset cluster seed* changes
  every sample of that cluster and no identity; *Delete cluster* removes it
  (its identities remain reserved for a later re-add). The global seed changes only in the
  generator.
- **Applied state survives a restart.** The console writes
  `<data>/console.json`; the next start resumes it unless the scenario file
  changed, in which case the file wins and the console says so in its
  timeline.
- **No login.** The console binds to loopback by default; the container
  image listens on 9090 inside the container and `docker-compose.yml`
  publishes it on `127.0.0.1` only; the Helm chart exposes no Service for it
  (use `kubectl port-forward`). Put your own authentication in front of it
  before reaching it from anywhere else. It never serves the simulator's API
  and the API never serves it; state-changing requests must be same-origin
  JSON.
- **The request log is redacted.** It keeps method, family, cluster, client
  address, duration, size and result, plus the request body of a
  `/vsanHealth` call; never a header, cookie or `/sdk` body (a login carries
  a password). It holds the last 20,000 calls in memory.

Projections (entities, series, calls and bytes per cycle) are computed from
the topology and the catalog, labelled as projections, and never presented
as measured scale.

### Checks

The first real Operations collection gate passed against the installed vSAN
Host Metrics 2.0.0 pack; its scope, procedure and measured result are in
[OPERATIONS-ACCEPTANCE](docs/OPERATIONS-ACCEPTANCE.md).

| Check | How | Where it runs |
|---|---|---|
| Contract: every captured request replayed against the Phase 1 shape, responses compared by element structure, labels, bucket timestamps and fault type; identity joins across `/sdk` and `/vsanHealth`; determinism across instances and seeds; fixture sanitization; login restriction | `go test ./...` | CI `go` job |
| Phase 2: the ten-cluster scenario (identities on both surfaces per cluster, per-cluster cardinality, overrides), restart and edit identity stability through the registry, credentials, fault and latency knobs, inventory realism against the trimmed live inventory capture, gauge values inside the live envelopes, `Raw` counters monotonic | `go test ./...` | CI `go` job |
| Control plane: draft, review and apply keep identities (and rebuild only for topology changes), Delete / Regenerate / Reset seed, saved state across a restart and a changed scenario file, scheduled apply, request log classification of ok / injected / unexpected / refused calls, redaction, admin API guards | `go test ./...` | CI `go` job |
| Smoke: a plain vSphere client (govc) connects with the scenario's credentials (and is refused with a wrong password), lists clusters, hosts and VMs and compares the counts with what the simulator built, dumps every identity, restarts the simulator on the same data directory and checks the dump is identical | `tools/smoke/smoke.sh` | CI `smoke` job |
| Chart: `helm lint --strict` with every `charts/vcenter-api-simulator/ci/*-values.yaml`, `helm template` and `kubeconform` of the rendered manifests and the Argo CD example | `.github/workflows/package.yml` | CI `package` workflow, `chart` job |
| Image: build, OCI label and non-root check, `docker compose` boot, then a vSphere client smoke ([`tools/smoke/smoke.ps1`](tools/smoke/smoke.ps1)): `Connect-VIServer`, `Get-VMHost`, `Get-VM`, expected hosts and VMs back, wrong password refused | PowerCLI on `pwsh` | CI `package` workflow, `image` job |
| Chart install (manual): the chart in a disposable kind cluster with default values, the same smoke through the Service, the certificate unchanged across a pod restart, an upgrade to `tls.existingSecret` and restricted login that verifies against the issuing CA | [`tools/smoke/helm-kind.sh`](tools/smoke/helm-kind.sh) | Optional: run manually (needs `kind`, `helm`, `kubectl`, `openssl`, Docker, `pwsh`), not required CI |
| The reference pack's own `perfsvc.connect/probe/collect/build_name_map/build_parent_map/build_disk_hardware` against every cluster of a scenario, both metric scopes, label sets equal to the live capture, every object named and parented, second collection identical, verify-on with the simulator's certificate pinned | `tools/mp-harness/harness.py` | manual `mp-acceptance` workflow |
| `mp-test connect` and `mp-test collect` twice through the Operations integration SDK 1.3.1 (builds and runs the pack's adapter container) | `tools/mp-harness/mp_test_run.py` | manual `mp-acceptance` workflow |

Locally, with govc installed (release binary: https://github.com/vmware/govmomi/releases/tag/v0.56.2):

```sh
tools/smoke/smoke.sh                                   # the ten-cluster scenario
tools/smoke/smoke.sh --scenario examples/scenarios/phase1.yaml
```

Smoke a simulator you are running yourself with PowerCLI (installs
VMware.PowerCLI for the current user when missing):

```sh
pwsh tools/smoke/smoke.ps1 -Server 127.0.0.1 -Port 8443 -Install
```

The pack harness needs the pack checked out at
`af976fb9fe97bbac2fd94b666a3f79aa12656f2a` of `sentania-labs/vsanhostmetrics`:

```sh
python3.12 -m venv .venv && .venv/bin/pip install pyvmomi==8.0.3.0.1 vmware-aria-operations-integration-sdk==1.3.1
# Python 3.12: the Operations SDK 1.3.1 does not install on 3.13 or newer
./vcsimulator -scenario examples/scenarios/ten-clusters.yaml -data ./data -listen 0.0.0.0:8443 &
.venv/bin/python tools/mp-harness/harness.py --sim 127.0.0.1:8443 --mp ../vsanhostmetrics \
    --user administrator@vsphere.local --password 'VMware1!' \
    --fixtures fixtures/live/9.1.1 --ca data/tls/cert.pem
PATH=.venv/bin:$PATH python3 tools/mp-harness/mp_test_run.py --mp ../vsanhostmetrics --sim-port 8443 \
    --user administrator@vsphere.local --password 'VMware1!' --clusters 10 --hosts 82 --vms 446 --attach false
```

`mp-test` runs the adapter in a container and reaches the simulator through
the Docker bridge gateway, so bind the simulator to `0.0.0.0`. It needs a TTY
and gets a pty from the runner. `attach_to_vcenter` must be off under
`mp-test`, which has no Operations Suite API to resolve vCenter objects.

The OSA scenario and its evidence limits are described in the
[scenario reference](docs/SCENARIO.md#osa-evidence-and-limits). The REST
surface currently models sessions and selected captured failure statuses;
successful inventory REST reads are not implemented. Non-vSAN storage and
live vCenter access remain outside this milestone.

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

Login accepts any user name and password unless credentials are
configured: the scenario's `auth.users`, or `VCSIM_USERNAME` and
`VCSIM_PASSWORD` (`-username`/`-password`), which add one more user. Then only
the configured users log in. Use placeholders: this is a simulator, never point real
credentials at it.

### docker run

```sh
docker run -d --name vcsim -p 8443:8443 -p 127.0.0.1:9090:9090 -v vcsim-data:/data \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  ghcr.io/sentania-labs/vcenter-api-simulator:main
curl -sk https://localhost:8443/sdk/vimServiceVersions.xml
```

The second `-p` publishes the control console on loopback only (it has no
login). The identity registry, the certificate and the console's applied
scenario are created on first start and kept
in the volume mounted at `/data`, so identities and the fingerprint a
management pack pinned survive restarts.
Add `-tls-names=<hostname>` (as a command argument) to put the name
collectors use into it, and `--read-only` needs the `/tmp` tmpfs: vcsim keeps
scratch files there.

### docker compose

[`docker-compose.yml`](docker-compose.yml) runs the same thing with a named
volume for the state directory (registry, certificate and console state):

```sh
docker compose up -d                 # pulls :main
docker compose up -d --build         # builds from this checkout
VCSIM_USERNAME=operator VCSIM_PASSWORD=placeholder docker compose up -d   # restricted login
docker compose down -v               # also discards the identities and certificate
```

`VCSIM_TAG`, `VCSIM_PORT`, `VCSIM_ADMIN_PORT` (the console's loopback port),
`VCSIM_SEED` and `VCSIM_TLS_NAMES` are the other knobs.

### Helm

[`charts/vcenter-api-simulator`](charts/vcenter-api-simulator) installs one
simulated vCenter per release. It creates a Deployment (one replica, non-root,
read-only root filesystem, no capabilities), a ClusterIP Service on 443, a
ServiceAccount with no token mounted, a PVC for the state directory (identity registry, certificate and console state), and
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
  PVC.

The PVC (`persistence.enabled`, default on) is mounted at `/data` in every
mode: it holds the identity registry and, when no TLS Secret is set, the
certificate. A Secret is overlaid read-only on `/data/tls`. The PVC is
annotated to survive `helm uninstall` and Argo CD prune or Application
deletion. With persistence off every new pod issues new identities (and a new
certificate when self-signed).

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
- [Scenario file reference](docs/SCENARIO.md)
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
