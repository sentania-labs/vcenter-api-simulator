# Development

For people changing the simulator. If you only want to use it, start with the
[README](../README.md).

## How it is built

The simulator is a Go service on [govmomi vcsim](https://github.com/vmware/govmomi/tree/main/vcsim).
vcsim serves `/sdk`. An out-of-tree handler serves `/vsanHealth`. Both share
one session store and one model of the environment.

| Part | Package | What it does |
|---|---|---|
| Topology | `internal/scenario`, `internal/topology` | Turns the [scenario file](SCENARIO.md) into one model of datacenters, clusters, hosts, VMs and storage. Each generated cluster draws its own counts from the seed |
| Identity registry | `internal/registry` | `registry.json` maps each entity's key to a serial that is never reused. UUIDs are UUID v5 of (seed, kind, serial). MoRefs are reserved the same way |
| vCenter | `internal/sim` | Builds the vcsim inventory in registry order and shapes properties after a trimmed capture of a real vCenter 9.1.1. Enforces accounts. Answers the REST, lookup, sign-on, token and license requests |
| vSAN and performance | `internal/vsanhealth`, `internal/perf`, `internal/catalog` | Performance values are a pure function of (telemetry seed, entity, counter, time bucket) inside the range the capture showed. Nothing is stored per sample |
| Sign-on key | `internal/signon` | The RSA key the token service signs with. Made on first use |
| Activity | `internal/life`, `internal/sim/life.go`, `internal/control/life.go` | An engine that decides from the seed what happens each minute. It acts on the running vCenter in place through a small interface, and records the events and tasks a real vCenter records |
| Control | `internal/control`, `internal/activity` | Owns the served scenario, the running vCenter, the request log, the connected tools and the scheduled changes |
| Console | `internal/admin` | The JSON API and the embedded console (the Lattice design system), on its own address |
| Request capture | `internal/querylog` | The unanswered list and the rotated full log. Removes credentials before anything is kept |

Every console change is a task (`internal/control/task*.go`). One function plans
it from the form's values and produces both the sentences shown as "What will
happen" and the change itself, so a screen cannot promise one thing and do
another. A change to clusters, hosts, storage or versions builds a new vCenter
next to the running one and swaps them. Slowness and faults, users, the
certificate, idle sign-out, licensing and Activity change the running vCenter in
place.

`activity.Classify` gives each request exactly one reason for the way it was
answered (answered, empty, refused as a real vCenter refuses it, failed by a
fault you switched on, paused, sign-in refused, or unknown). The Requests
screen, Home and request capture are all counted from it.

## Rules the code keeps

- **No invented API.** An operation the simulator cannot answer returns the
  fault a real vCenter would, not a fake success. Shapes come from captures of
  a real vCenter 9.1.1 (`fixtures/live`, sanitized) and are checked with the
  client libraries a collector uses.
- **Identities are stable.** A restart, a console change and a removal keep the
  identity of everything that remains. Only a new seed with `-regenerate`, or
  "Replace everything with a new environment", changes them.
- **Same inputs, same output.** The same seed and settings give the same
  inventory, values and Activity.
- **No secrets and no real inventory in the repository.** Fixtures are
  sanitized and reviewed before they are committed. Live probes (`tools/probe`)
  are read-only and need a credential source the owner approves.

## Checks

| Check | How | Where it runs |
|---|---|---|
| Contract and behaviour: captured requests replayed against the simulator, identities across `/sdk` and `/vsanHealth`, scenarios, restart stability, accounts, faults and latency, the console's tasks and screens, Activity, licensing and sign-on, request capture and redaction | `go test ./...` | CI `go` job |
| A plain vSphere client (govc): connect, refuse a wrong password, list clusters, hosts and VMs, compare counts, restart on the same data and compare every identity | `tools/smoke/smoke.sh` | CI `smoke` job |
| Chart: `helm lint --strict` with each `charts/vcenter-api-simulator/ci/*-values.yaml`, [`test.sh`](../charts/vcenter-api-simulator/test.sh) (default ingress paths, image tag, install notes), `helm template`, `kubeconform` | `.github/workflows/package.yml` | CI `chart` job |
| Image: build, label, non-root check, `docker compose` boot, then PowerCLI ([`smoke.ps1`](../tools/smoke/smoke.ps1)): `Connect-VIServer`, `Get-VMHost`, `Get-VM`, wrong password refused | PowerCLI on `pwsh` | CI `image` job |
| Chart install in a disposable kind cluster: default values, the smoke through the Service, the certificate unchanged across a pod restart, an upgrade to `tls.existingSecret` | [`tools/smoke/helm-kind.sh`](../tools/smoke/helm-kind.sh) | By hand (needs `kind`, `helm`, `kubectl`, `openssl`, Docker, `pwsh`) |
| A week of Activity at the large size | `VCSIM_LARGE=7 go test ./internal/control -run TestActivityADayAtTheLargeSize -v` | By hand. Builds 5,009 VMs. [Numbers](sizing.md#measurements) |
| A property collector subscription across the large size, two simulated hours of Activity | `VCSIM_LARGE_UPDATES=1 go test ./internal/control -run TestActivityLargePropertyStream -v` | By hand. Builds 5,009 VMs |
| The reference pack's collector and `mp-test` against every cluster of a scenario | `tools/mp-harness/harness.py`, `tools/mp-harness/mp_test_run.py` | Manual `mp-acceptance` workflow |
| The read-only vCenter walk | `tools/probe/test_probe.py` | By hand. See [`tools/probe/README.md`](../tools/probe/README.md) |
| Scale replay | `tools/bench/bench.py` | By hand. See [`tools/bench/README.md`](../tools/bench/README.md) |

Locally, with govc installed:

```sh
tools/smoke/smoke.sh                                   # the ten-cluster scenario
tools/smoke/smoke.sh --scenario examples/scenarios/phase1.yaml
pwsh tools/smoke/smoke.ps1 -Server 127.0.0.1 -Port 8443 -Install -User <username> -Password <password>
```

Shared storage. [`storage.yaml`](../examples/scenarios/storage.yaml) has shared
NFS and iSCSI datastores, an inaccessible NFS mount, an all-paths-down LUN on
one host and a near-full datastore. The govc smoke:

```sh
tools/smoke/smoke.sh --scenario examples/scenarios/storage.yaml --storage
```

For PowerCLI, start the simulator on that scenario with an account
(`VCSIM_USERNAME` and `VCSIM_PASSWORD`; there is no default password), then:

```sh
pwsh tools/smoke/smoke.ps1 -Server 127.0.0.1 -Port 8443 -Storage -User <username> -Password <password>
```

Both check datastore and host storage reads and the host fault. They do not
show that a real storage system or an Operations pack has been exercised.

The pack harness needs the reference pack checked out at
`af976fb9fe97bbac2fd94b666a3f79aa12656f2a` of `sentania-labs/vsanhostmetrics`,
Python 3.12 (the Operations SDK 1.3.1 does not install on 3.13 or newer), and
a simulator bound to `0.0.0.0`, because `mp-test` reaches it through the Docker
bridge. See the comments at the top of each script for the arguments.

Console screens are tested by `node --test` on `internal/admin/ui/js/console.test.mjs`,
which `go test` runs when node is installed. A change to a console screen is
walked by someone who did not write it, in the running app, doing the real job
start to finish: reading each screen once, without opening collapsed sections,
long scrolling or guessing a value, and able to tell afterwards that it worked.
Screenshots of the current screens are in [console](console/README.md).

### Repeating the Operations check

Use a development Operations instance and a credential source its owner
approves. Keep Operations credentials and tokens in the process environment,
never in a transcript, scenario or pull request. Use placeholder credentials for
the simulator. Run a pinned simulator image on a host the collector can reach,
on `examples/scenarios/phase1.yaml` with a persistent data volume (it holds the
registry and the certificate). Snapshot the adapter inventory first, then add
exactly one `VsanHostMetrics` adapter:

| Setting | Value |
|---|---|
| Endpoint | The simulator's host and port, reachable from the collector |
| Collection interval | Five minutes |
| `metric_scope` | `all` |
| `attach_to_vcenter` | `false`, so the entities stand alone |
| `container_memory_limit` | `1024` (MiB) |
| `verify_certs` | `false` for the first controlled-lab run |
| Credential | `vsan_vc_credential` with simulator-only placeholders (`vcenter_username`, `vcenter_password` in 2.0.0) |

Check for an existing adapter of the same name before creating one, and do not
blindly retry a timed-out creation. Assert, from the Suite API:

- The adapter (`/suite-api/api/adapters/{id}`) and its resources
  (`/adapters/{id}/resources`, following pagination) match the simulated
  entities and kinds, all `DATA_RECEIVING`.
- `/resources/stats/latest` for only the simulator's resource ids, with
  `maxSamples=3` and `currentOnly=false` (Operations refuses several samples
  with `currentOnly=true`), has the expected metric keys at two distinct
  collection timestamps.
- `POST /resources/bulk/relationships` for `PARENT` and for `CHILD` returns
  nothing, since the entities are standalone.
- Two nonempty collection cycles show the same resource ids and metric keys,
  with advancing timestamps. Discovery alone is not enough.
- The adapter inventory is unchanged apart from the one new adapter.

Leave the adapter and simulator running for inspection. Remove them only when
the owner authorizes it: delete only that adapter, decide whether to keep its
collected resources, delete its dedicated credential only when no adapter uses
it, and keep the data volume unless resetting identities and the certificate is
intended.

## What has been checked

Against VCF Operations, on 2026-11-07, with simulator v0.2.0, Operations 9.1.1.0
(build 25795063) and the installed vSAN Host Metrics 2.0.0 pack, which brings
its own collector:

| Assertion | Observed |
|---|---|
| Simulated entities / resource kinds | 240 / 41, the exact expected sets |
| Resources including the adapter instance | 241, all `DATA_RECEIVING` |
| Expected / ingested metric series | 5,321 / 5,321 on each cycle |
| Resource IDs and metric keys | Unchanged between two cycles |
| Source values against what was ingested | All 10,642 match within `1e-6` |
| Metric series that changed between cycles | 2,621 |
| Existing adapter configurations | All 26 unchanged. One simulator adapter added |

The pack's adapter was attached with `attach_to_vcenter` off, so the entities
stand alone. Native-vCenter attachment, scale, certificate verification from
the collector and restart under an active adapter were not tried.

**Not run, at any simulator version:** the built-in vCenter adapter of Operations 9.1.1 collecting from the
simulator. The simulator answers every request in the license chain, and these
were run against its answers with the adapter's own Java client classes:
lookup, license login and asset parsing, sign-on admin construction, the
licensing-group check, the inventory property collector (all 5,231 objects of
the large inventory, three full syncs), standard performance (879,322 series
decoded), vStats queries and streams, tags and appliance reads. The remaining
proof is starting the adapter on a development Operations instance
([#14](https://github.com/sentania-labs/vcenter-api-simulator/issues/14)).

Version surfaces (the `smoke` job, `smoke.sh --scenario examples/scenarios/ten-clusters.yaml --powercli`):

| Preset | Client | Asserts |
|---|---|---|
| 8.0 Update 3 | govc, REST, `/sdk/vsanServiceVersions.xml` | vCenter 8.0.3 build 24022515, API 8.0.3.0; every host 8.0.3 build 24022510; REST `8.0.3.00000`; vSAN namespace 8.0.0.4 |
| 9.1.1 | the same | vCenter 9.1.1 build 25784172, API 9.1.1.0; every host build 25782009; REST `9.1.1.0100`; vSAN namespace 9.1.1.0 |
| 9.0 vCenter with an 8.0 Update 3 cluster holding one 9.0 host (`tools/smoke/versions.yaml`) | govc and PowerCLI | vCenter 9.0.0 build 24755230; each host's own version and API version |

The vSAN namespace for 8.0 Update 3 is inferred from the order of the 9.1.1
capture and has not been checked on a real 8.0 Update 3 vCenter. The REST host
list has no product version, so host versions are checked through `/sdk`.

The simulator's answers have not been compared with a live vCenter beyond the
read-only captures the fixtures come from.

## Working on it

Changes go through the lab's delivery pipeline: build, independent self-review,
fix findings, pull request, external review, fix findings, green CI, merge, tag.
Break large changes into reviewable pull requests and put the evidence in the
description. Do not describe expected scale as measured scale.
