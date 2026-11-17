# Reference

Everything the [README](../README.md) leaves out: every start-up flag, what a
scenario file controls and what the simulator guarantees, request capture,
the console's password and certificate, how the checks are run, and the
details of running it with Docker, Compose, Helm and Argo CD.

## Standard vSphere performance

`QueryPerf`, `QueryPerfComposite`, counter discovery, and host/VM quickStats
share a workload model. Six VM cohorts range from idle to busy, with a daily
cycle, a weekly cycle, smooth shorter variation and brief daily contention
and latency peaks. Values depend on telemetry seed, VM identity and sample
time, without stored series or random request-time noise. A repeat of the
same historical query against unchanged inventory returns the same values,
including after restart.

The inventory reader supplies power state, connection state, maintenance,
placement, hardware capacity, network interfaces and datastore membership.
Powered-off VMs have no samples. VMs on unavailable or maintenance hosts
contribute no load. VM placement changes move the load. Host CPU and memory
budgets cap usage under overcommit; host values sum their VMs, clusters sum
hosts, and datacenters sum clusters. Datastore I/O partitions each VM's load
among its attached datastores. Unused datastores have zero I/O. No VM lifecycle
operations, GPU devices, guest agents or alarms are invented.

History is a replay of the synthetic workload at the requested times using
**current inventory state and placement**. Past power transitions or migrations
are not retained or reconstructed. This is a model of that inventory's workload,
not an audit history of inventory changes.

| Interval | Retention | Providers |
|---|---|---|
| 20 seconds | 1 hour | VM, host, resource pool |
| 300 seconds | 1 day | VM, host, pool, cluster, datastore, datacenter |
| 1,800 seconds | 1 week | Same historical providers |
| 7,200 seconds | 30 days | Same historical providers |
| 86,400 seconds | 1 year | Same historical providers |

Samples are ascending on UTC interval boundaries, strictly after `startTime`
and at or before `endTime`. Future samples are excluded. `maxSample` selects
the latest samples in the requested window. Historical average, minimum,
maximum and summation counters roll up the underlying 20-second values;
latest counters take the endpoint. Independent integer averages can differ
from the sum of child averages by less than one native unit per child.
Percent counters use hundredths of a percent, CPU rates MHz, memory KiB,
throughput KiB/s, ready/wait time milliseconds over the interval, and latency
the counter's advertised unit. Normal XML preserves zero positions; CSV uses
the same samples. Wildcards expand into the aggregate empty instance and
inventory-backed CPU, NIC, virtual-disk or datastore instances. Unknown or
unsupported metrics are omitted. The published catalog includes inherited
vSphere metadata beyond the available synthetic subset; availability discovery
is the supported-entity/instance contract.

Requests are bounded to two million returned values and one hundred million
inventory-node/sample evaluations, plus thirty million counter evaluations.
Requests exceeding these limits return
`InvalidArgument`; split broad history into smaller windows. The evaluator
builds each shared timestamp once per request and discards it after use.
It retains no cross-request cache or background telemetry series.

Operations 9.1.1 uses CSV queries, 20-second VM/host samples and configured
historical container intervals (normally 300 seconds), including wildcard
instances. Its datacenter CPU wait, contention, usage and storage throughput
come from host/VM aggregation in `LWManagedObject` and `LWDatacenter`; the
adapter's container counter configuration directly requests VM-operation
counters. Cluster resource metrics can also be queried through this service.
Both paths therefore use the same underlying load. The adapter's
`QueryPerfCounterInt` discovery receives 28 enabled guest counters, with
names and units checked through a read-only vCenter 9.1.1 metadata query.
These counters use stable simulator-owned IDs starting at 20001.

The default vStats specification in the 9.1.1 adapter requests only GPU and
vGPU metrics. The three GPU-less real-vCenter adapter logs contain `/stats`
read timeouts, without the simulator's "Empty snapshots from VC stats registry"
message. The simulator now keeps these streams open without emitting empty
frames. Query creation, attachment, deletion and cancellation still work;
there are no fabricated GPU snapshots. ESX-direct collection remains outside
this simulator. API decoding is checked with the adapter's own VLSI and vAPI
classes; chart ingestion in Operations must still be checked after deployment.

## The simulator, its flags and what a scenario controls

The simulator is a Go service on [govmomi vcsim](https://github.com/vmware/govmomi/tree/main/vcsim)
with an out-of-tree `/vsanHealth` extension. A **scenario file** describes
the simulated vCenter: datacenters, vSAN ESA and OSA clusters with their own
host, device, VM, CPU, DOM-world and uplink counts, who may log in, the
certificate, and failure and latency knobs. An **identity registry** in the
data directory keeps every MoRef and uuid stable across restarts and
scenario edits. The Performance Service serves every entity family the
sanitized live capture ([`fixtures/live/9.1.1`](../fixtures/live/9.1.1/README.md))
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
| `-scenario FILE` | the scenario ([reference](SCENARIO.md)); without it the built-in one-cluster Phase 1 shape runs under `-seed` |
| `-data DIR` | holds `registry.json` (identities), `tls/` (the self-signed certificate, created once) and `sign-on-key.json` (the key sign-on tokens are signed with, created when first needed and kept for as long as the seed is the same); default `$VCSIM_DATA`, else `data`; the image sets `VCSIM_DATA=/data`, its state volume |
| `-describe` | print the scenario's cardinality (clusters, hosts, VMs, devices, per cluster) as JSON and exit |
| `-query-log PATH` | initially enable the full JSONL query log at `PATH`; it can contain inventory identifiers. The log is bounded: it rotates by size into gzip archives and the oldest files are deleted, so its total stays inside the cap. A start-up default: see the saved capture settings below |
| `-query-log-max-bytes N` | bytes one log file may hold before it is gzipped and a new one starts (default 4194304) |
| `-query-log-max-files N` | how many log files to keep at most, oldest deleted first (default 4) |
| `-query-log-max-total N` | total bytes the whole log may take across its files (default 25165824, or the data volume's capacity less 8 MiB kept for the state files when that is smaller) |
| `-unanswered-stdout` | initially send unanswered requests as JSON lines to stdout; a start-up default, and the console has no switch for it |
| `-debug-capture=false` | initially stop writing what tools ask about licensing and sign-on to stdout (the requests listed under "Debug capture" below); on by default. A start-up default: see the saved capture settings below |
| `-lookup-trust-cert FILE` | a PEM file with the certificate tools see at the simulator's address when a proxy in front of it ends TLS (env `VCSIM_LOOKUP_TRUST_CERT`). The lookup service names it as the certificate to trust for the sign-on addresses it hands out, in place of the simulator's own, which such a tool never sees. The file is read again when it changes. Without it the simulator's own certificate is named |
| `-regenerate` | discard the registry; required after a seed change (identities change only if the seed did) |
| `-tls-cert`, `-tls-key` | serve a provided certificate instead of the self-signed one (the scenario's `tls:` does the same) |
| `-session-timeout` | idle session timeout; sessions never expire by default |
| `-any-login` | explicitly allow any non-empty vCenter username and password |
| `-username`, `-password`, `-role` | one vCenter account supplied at start (env `VCSIM_USERNAME`, `VCSIM_PASSWORD`, `VCSIM_ROLE`); with no account supplied here or in the scenario, the console's first-run set-up chooses the password |
| `-public-url URL` | the address tools should use, shown in the console (env `VCSIM_PUBLIC_URL`); default `https://<name>:<port>` from the first `-tls-names` entry or the machine's name. Set it when a proxy or ingress gives the simulator another name |
| `-report-url URL` | where the console sends people to report a request the simulator cannot answer (env `VCSIM_REPORT_URL`); default this project's new-issue page |
| `-listen ADDR` | repeatable; default `0.0.0.0:8443` |
| `-admin ADDR` | the password-protected control console (plain HTTP); default `127.0.0.1:9090`, env `VCSIM_ADMIN`; empty turns it off |
| `-admin-allowed-hosts NAMES` | comma-separated Host names the console answers to besides `localhost`, `127.0.0.1` and `::1` (any port); every other Host gets 403, which stops a web page from reaching the console through DNS rebinding. Add the name your proxy or ingress forwards; env `VCSIM_ADMIN_ALLOWED_HOSTS` |

To capture what a client asks for, open **Requests** in the console. Requests
the simulator has no answer for are captured from the start; "Capture every
request in full" keeps everything while it is on (or start with
`-query-log /data/query-log.jsonl`). Repeat the action in the client, then
download from the same screen. The chart's `simulator.capture.queryLog` and
`simulator.capture.unansweredStdout` set the initial defaults; sending
unanswered requests to stdout is an install-time choice and has no switch in
the console. SOAP credentials, session operations and headers are omitted or
redacted, but request bodies can contain inventory identifiers. Treat the
records accordingly. Unanswered records are bounded in memory and kept until
deleted; turning full capture off deletes what it captured.

The full log lives in the data volume as `query-log.jsonl` plus its gzip
archives `query-log.N.jsonl.gz`. It cannot grow past its cap: when a file
reaches `-query-log-max-bytes` it is compressed and a new one starts, and when
the files together pass `-query-log-max-total` the oldest are deleted first.
Where the volume's capacity is known, the cap never passes that capacity less
8 MiB, so the state files always keep their room. The chart sets the three
limits with `simulator.capture.queryLogMaxFileBytes`, `queryLogMaxFiles` and
`queryLogMaxTotal` (0 keeps the default). Downloads and "every request, one at a
time" read across the rotated and compressed files.

The start-up flags `-query-log`, `-unanswered-stdout` and `-debug-capture` only
set the first defaults. The first time any capture switch on **Requests** is
used, all the capture settings (including the ones with no switch, such as
unanswered requests to stdout) are saved in `capture-settings.json` in the
data directory, and from then on the saved settings win over the flags at
every start.

Debug capture is on by default. **Requests** has a third capture switch,
"Record what tools ask about licensing", that turns it on and off; the choice
is saved with the other capture settings. It
writes one JSON line to stdout (`"event":"request-capture"`) for:

- every request to the license service (`/ls`), the lookup service
  (`/lookupservice`), the sign-on admin service (`/sso-adminserver`) and the
  token service (`/sts`);
- a `POST` to `/api` itself, which is how a tool opens a session on the
  interface licenses are assigned through;
- on `/sdk`, the four operations by which a tool carries a session from one
  connection to another: `LoginBySessionTicket`, `AcquireSessionTicket`,
  `AcquireCloneTicket` and `CloneSession`. No other `/sdk` request is logged.

Each line has the method, path, query, the SOAP operation, the status, the
type of the fault when the answer was one (so a refused `AcquireCloneTicket`
shows as `NotAuthenticated`, which no other list records), `cookieSent`
(whether a session cookie came with the request; its value is never logged),
the names of all headers, the values of Content-Type, SOAPAction, User-Agent,
Accept and Content-Length, and the request body. Before
logging, the content of any element, and the value of any attribute, JSON or
form field or query parameter, whose name contains a credential word (password,
secret, token, cookie, ticket, credential, session, hash, assertion, license
key, signed entitlement, private key, security, signature, x509, sslTrust,
user, owner id or principal) is
replaced by a `[scrubbed:Name len=N]` marker, as is any run of more than 64
base64 or hex characters. Element names, namespaces, other attributes and order
are kept. A request to the token service is treated the other way round, so
that its shape can be learned: every element and attribute is kept, and the
text of every element is removed unless the element is one whose text is a
fixed name or a time (the kind of token, of key and of signature asked for,
and its lifetime). An assertion loses everything inside it. Authorization,
Cookie and session headers never have their values
logged. A body over 64 KiB is cut and flagged. Nothing of an answer is logged
but its status and fault type.

The console uses a separate password from vCenter. The first time it is
opened it asks for two things: a password for the console itself (at least 8
characters), then, when the deployment supplied no vCenter account, the
password of the first account tools sign in with,
`administrator@vsphere.local`. There is no published default password: until
that step is done nothing can sign in to the simulated vCenter. A deployment
that supplies an account (the scenario's `auth.users`, `-username` and
`-password`, or `-any-login`) skips the step.

**Upgrading from 0.3.x.** Earlier versions accepted a published default
password for `administrator@vsphere.local` when no account was supplied. That
password is retired. After the upgrade a simulator with no supplied account
refuses every sign-in until the console's set-up step chooses a password. A
saved account keeps whatever password was set, including the old default
value. Tools that used the default need the new password, or supply an
account at install: `VCSIM_USERNAME` and `VCSIM_PASSWORD`, the chart's
`credentials`, or the scenario's `auth.users`. With the console turned off
(`VCSIM_ADMIN=`) supplying an account is the only way to sign in.

Set `VCSIM_ADMIN_PASSWORD` to supply the console password instead (the chart's
`admin.existingSecret` points to a Secret with key `password`). A chosen
password persists as a bcrypt hash in `console-password` under the data
directory and can be changed on the console's Security screen. A password
supplied by the deployment wins: if it differs from the stored hash, the hash is
replaced at start, existing sessions end, and one log line (never the value)
says so; the console then shows it as read-only.
Sessions expire after 30 minutes without a person using the console (the
console's own background refresh does not count) and after 12 hours in any
case; they do not survive restart. An ended session returns to the sign-in page
with a notice.

**Reset the console password.** It cannot be reset from the sign-in page. On
the machine the simulator runs on, stop it, remove `console-password` from the
data directory or run `vcsimulator -admin-reset -data <dir>`, then start it
again and choose a new password. Remove `VCSIM_ADMIN_PASSWORD` first if the
deployment supplies one.

The chart keeps the console on loopback unless `admin.service.enabled` is set;
an optional `admin.ingress` exposes that Service. Use TLS outside loopback. For
a TLS-terminating proxy, set `-admin-secure-cookie` so cookies require HTTPS;
the chart sets it automatically for its TLS ingress.

| Where to set it | Settings |
|---|---|
| Console | clusters, hosts and shared storage; vCenter and ESX versions; vCenter users and what they may do (full access or read-only; a deployment sets `role: read-only` on an `auth.users` entry, `VCSIM_ROLE`/`-role` for the `VCSIM_USERNAME` user, or the chart's `credentials.role`; an environment variable's name never changes a role); idle sign-out; request capture; slowness and faults; an unmanaged certificate; the console password |
| Start-up | API and console listen addresses and ports, data directory, console allowed hosts, the address shown to tools (`-public-url`), where unanswered requests are reported (`-report-url`), and sending unanswered requests to stdout. |

An uploaded or regenerated console certificate is kept in the data directory.
Changing it ends sessions, and clients must trust the new certificate. A
certificate supplied through `-tls-cert`/`-tls-key`, scenario `tls`, or the
chart's existing Secret or cert-manager takes precedence and is read-only in
the console. If the chart's supply is removed, the previously configured console certificate is used again.

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
  `NotAuthenticated`). With no users configured there is no default
  password: the console's first-run set-up chooses the password of
  `administrator@vsphere.local`, and nothing signs in before then. Set
  `auth.anyLogin: true` or `-any-login` to accept arbitrary non-empty credentials. The certificate persists,
  so the 2.0.0 pack's certificate pin survives restarts.
- **Lookup, sign-on and license services.** The chain VCF Operations walks
  before it collects ([what each answers and what is unverified](../README.md#licensing-and-sign-on)):
  - `/lookupservice/sdk`: `RetrieveServiceContent`, and `List` for three
    filters (`vcenterserver` with `vmomi` / `com.vmware.vim`; `cs.identity`
    with `vmomi` / `com.vmware.cis.cs.identity.admin`; `cs.identity` with
    `wsTrust` / `com.vmware.cis.cs.identity.sso`). Any other filter, and no
    filter, returns an empty list. Addresses are built on the address the
    client used: `X-Forwarded-Host` when a proxy sets it, else `Host`.
    `sslTrust` is the listener's certificate, or the one in
    `-lookup-trust-cert`. The node id is its own UUID and is the scope of the
    license service; the vCenter registration's service id is
    `about.instanceUuid`.
  - `/sso-adminserver/sdk/vsphere.local`: `SsoAdminServiceInstance`,
    `IdS_getSystemDomainName`, `IdS_getSslCertificateManager` and
    `GetIssuersCertificates`, without a
    sign-in.
  - `/sts/STSService/vsphere.local` (and `/sts/STSService`): a WS-Trust `Issue`
    for a bearer token with a username token, checked against the same accounts as `/sdk`. The
    SAML assertion carries an enveloped RSA-SHA256 signature over its
    exclusive canonical form, made with an RSA 3072 key kept in
    `sign-on-key.json`; `GetIssuersCertificates` returns that key's
    certificate. The vCenter settings `config.vpxd.sso.sts.uri`,
    `config.vpxd.sso.admin.uri` and `config.vpxd.sso.groupcheck.uri` name
    these two paths under `-public-url`.
  - `/ls/sdk`: `Login` and `Logout` of the session-management service, and,
    signed in, `SearchAssets` (the plain form and the forms by product family
    names, by license state and by license id, with `maxResults` and
    `offset`), `SearchLicenses` and `QuerySystemTime`. A license session never
    authenticates `/sdk`; the two stores are separate.
  - `/sdk`: `LoginBySessionTicket` answers `InvalidTicket`. The extension
    `com.vmware.vrops.license.managed` is seeded from the scenario, and
    `RegisterExtension`, `UpdateExtension` and `UnregisterExtension` of it
    are saved, so that a claim by an Operations outlives a restart.

  An anonymous or logged-out
  `SessionManager.currentSession` read answers an empty `FetchResponse`.
  `/rest/vcenter/certificate-authority/get-root` and its `/api` and numbered
  forms return the simulator's own certificate as PEM once the request carries
  a valid `vmware-api-session-id`.
- **Faults and latency.** Per-family or per-method SOAP faults
  (`invalid-argument`, `not-supported`, `system-error`, `timeout`) by
  cluster and time window; per-family and `/sdk` latency with jitter.

Known gaps: the vDS is served under vcsim's base type
`DistributedVirtualSwitch` rather than `VmwareDistributedVirtualSwitch`;
populated vSAN iSCSI target service, file service, and FC-backed datastores
are not modeled because the walk captured no populated examples.
A licensed vCenter (a signed entitlement), license assignment, the
`LicenseManager` managed object on `/sdk`, and the console's
per-service `currentSession` shapes beyond what was captured are not modelled.
Among the answered operations, the bad-credential fault of a real vCenter's
license service was never observed, so a refused license login, and a license
read that is not signed in, answer the
captured invalid-request fault shape and that shape is not claimed as verified;
the other unverified parts of the licensing and sign-on answers are listed in
the [README](../README.md#licensing-and-sign-on). Shared NFS
v3 and iSCSI-backed VMFS datastores can be configured in a scenario; the
iSCSI shape is plausible but not verified against a lab iSCSI host. REST
inventory reads share SOAP inventory identities. List endpoints honour `names`, the parent and state
filters that apply to each kind (for example `clusters`, `datacenters`,
`hosts`, `folders`, `resource_pools`, `power_states`) and the id filter of each
kind, in both the plain and `filter.`-prefixed spellings; any other parameter
returns a 400 `INVALID_ARGUMENT`. Some nested VM and OSA health details remain
partial. The real 9.1.1
server returns full documents for `/sdk/vimService.wsdl` and `/sdk/vim.wsdl`.
The simulator serves the captured service descriptor but does not serve
`vim.wsdl`; a complete sanitized capture is needed to add it.

### Control console

Open `http://127.0.0.1:9090/` (the `-admin` address). The console is the
way to change a running simulator without editing YAML. It has nine tabs,
named for what you want to do: **Home, Clusters, Storage, Versions, Users,
Security, Activity, Slowness and faults, Requests**. It follows the Lattice design
system, in light and dark, and works at phone width. It answers only to the
Host names `localhost`, `127.0.0.1` and `::1` (any port, so a port-forward and
`docker compose` work) plus those in `-admin-allowed-hosts`; behind a proxy,
add the name the proxy forwards. Helm: `admin.allowedHosts`; the console
listens on the pod's loopback unless `admin.exposeToPodNetwork` is set.

- **Every change works the same way.** A short screen asks a few questions
  that are already answered. Beside them "What will happen" says in sentences
  what the change will do, and one button makes it: right away, or at a time
  later the same day. There is no shared draft and no separate apply step.
  A scheduled change shows as a bar on every screen with "Do it now" and
  "Cancel it", locks nothing, and does not survive a restart (the history
  says so when that happens).
- **Changes to clusters, hosts, storage and versions build a new vCenter**
  next to the running one and swap them, so connected tools are signed out
  and sign in again, as after a vCenter restart. Every identity the change
  keeps is unchanged. Users, the certificate, idle sign-out, slowness and
  faults, and everything Activity does change in place. So does how licensing
  looks to tools (Security):
  whether the evaluation is running or has run out, whether accounts are
  licensing administrators, and which VCF Operations, if any, vCenter says
  manages its license.
- **The simulator names clusters** (`cluster-12`) and invents storage
  connection details; hosts are removed by name and a removed host's number
  is not reused. "Replace everything with a new environment" is the one
  action that starts a new identity registry.
- **Home says what is true.** Whether a tool is connected, as which account,
  what it calls itself, and if it is not connected, since when and why (its
  user was removed, the certificate was replaced, the simulator is paused, it
  signed itself out, or it has gone quiet). A tool counts as connected while
  it has been answered within the last 15 minutes and has not signed out
  since; one that signs out after each collection shows as connected again
  when it next signs in. Home also says which simulator build is running
  (version, commit, the vCenter it presents, uptime) and how busy it has been:
  requests by outcome and by service, with a typical and a slow-end response
  time, for the last hour or since it started. The last hour is counted from
  the request log (below), so on a busy simulator it says how far back the log
  reaches instead; neither view survives a restart.
- **Requests says why each request was answered as it was.** "Not answered"
  means only requests the simulator has no answer for; a refused sign-in, a
  refusal a real vCenter makes too, and a fault you switched on are each
  counted separately.
- **Applied state survives a restart.** The console writes
  `<data>/console.json`; the next start resumes it unless the scenario file
  changed, in which case the file wins and the history says so. Accounts
  made in the console survive that too when the deployment supplies none.
- **Console login.** The console binds to loopback by default and uses its own
  password. The chart's console Service and Ingress are opt-in. Use TLS when
  reaching it beyond loopback. State-changing requests must be same-origin JSON.
- **The request log is redacted.** The activity log holds the last 20,000 calls
  in memory. Unanswered requests can retain redacted SOAP bodies, including
  inventory identifiers; session operations and credentials are omitted.

Projections (entities, series, calls and bytes per cycle) are computed from
the topology and the catalog, labelled as projections, and never presented
as measured scale.

### Activity

A vCenter nobody uses is still: no events, no tasks, flat inventory. The
**Activity** tab makes the simulated environment change by itself the way one
in use does, so a monitoring tool has something to see. It is off until you
start it, and one button stops all of it.

**One dial for each thing that happens.** The middle of a dial is a baseline
for an environment of this size; left is slower (down to off), right is
busier. The amounts follow the number of VMs and hosts, so a larger
environment changes more. Next to each dial a sentence says what its position
means in numbers.

| Dial | At the baseline | What a tool sees |
|---|---|---|
| Load balancing (DRS) | about 25 vMotions a day for every 100 VMs, in bursts of a few VMs, more in working hours | each VM on another host; one DRS vMotion task and its events for each |
| VMs powered off and on | about 2 a day for every 100 VMs, mostly in working hours; a VM stays off for a while and comes back; never more than one VM in ten is off | power state, guest state, the VM's performance numbers at zero while it is off |
| New VMs | about 1 a day for every 100 VMs, mostly in working hours | a new VM with a number its cluster has not used since Activity was first started, powered on |
| VMs removed | about 1 a day for every 100 VMs | a VM powered off and removed; its number and identities are not used again |
| Hosts in maintenance mode | 5% of the hosts, and others take their place about once a week | the host's VMs move off first, then the host says it is in maintenance mode; a host that comes out gets some VMs back |
| Datastores filling | 0.4% of each vSAN datastore's capacity a day, a little every half hour | datastore free space, vSAN used capacity and the datastore's performance numbers |
| Datastore cleanup | 0.3% of capacity freed at 2 AM | the same numbers falling |
| CPU load | as the VMs were built (half to double) | CPU usage of VMs, hosts and clusters; above the baseline, ready time (contention) rises too |
| Memory load | as the VMs were built (half to double) | memory used by VMs, hosts and clusters, up to what each VM has |

- **For the whole vCenter, and for each cluster.** "These dials are for"
  chooses. A cluster follows the vCenter's dials until you move one of its
  own; that dial then applies to that cluster only, and "Follow the vCenter
  again" puts it back.
- **Exact numbers.** "Type an exact number" sets a dial to one VM every so
  many minutes, hours or days (a new VM every 30 minutes, one removed every
  45), so many vMotions a day, or a percentage (hosts in maintenance mode, up
  to a quarter; datastore growth, up to 10% a day). An exact number stays as
  typed whatever the size of the environment. One VM every so often is
  spread evenly over day and night; an exact number of vMotions a day still
  comes in DRS's bursts, more of them in working hours. Moving the dial goes
  back to a level.
- **Limits.** The number of VMs stays between two numbers (5% below and
  above the number there was before Activity changed anything, unless you
  change them; they move with clusters and hosts you add or remove); at a
  limit, adding or removing waits and says so once a day. At most a quarter
  of a cluster's hosts are in maintenance mode at once. A vSAN datastore is
  not filled past 90%, and a cleanup frees only what Activity filled.
- **The Clusters tab shows where Activity has put things**: each host's VM
  count and whether it is connected or in maintenance mode, and each
  cluster's VM count, follow what Activity has done.
- **"Do it now"** on a cluster's dial does that thing once, straight away,
  after saying what it will do.
- **A change that moves hosts asks first**: starting, stopping, and raising
  or lowering the share of hosts in maintenance mode each say how many hosts
  and VMs will move before anything happens.
- **While the simulator is paused** nothing moves: Start, Stop, "Do it now"
  and a change to the hosts in maintenance mode are refused until you resume.
- **Stop** leaves the environment still and healthy: hosts come out of
  maintenance mode and VMs that are off are powered on. VMs that were added
  or removed stay that way, and datastores stay as full as they are.
- **"What has happened"** lists every cause with what it set off as one
  line (a host going into maintenance mode is one line, however many VMs
  moved), newest first, with the detail one click away. It says which lines
  came from a button you pressed. The last 1,000 lines are kept.
- **The same seed and the same settings make the same things happen** at the
  same times. Times of day are the simulator's local time.
- **It survives a restart**: the dials, where each VM is, which are off,
  which hosts are in maintenance mode and how full the datastores are are
  written to `<data>/life.json`; VMs that were added or removed are part of
  the saved scenario (`addedVMs` and `removedVMs`, see
  [the scenario file](SCENARIO.md)). Nothing happens while the simulator is
  off or paused, and nothing is caught up afterwards. A change that rebuilds
  the vCenter (clusters, hosts, storage, versions) keeps all of it; "Replace
  everything with a new environment" starts again. When the scenario file
  changes, the file wins for what it says; the VMs Activity added and
  removed are kept for the clusters the file still has, and the limits are
  set again if the new size is outside them.
- **If something fails three times in a row**, that one dial stops itself,
  the tab shows a problem, and "Switch it on again" resumes it. The rest
  carries on.

**What vCenter records.** Each operation is recorded as a real vCenter 9.1.1
records it; the shapes were read from one (read-only) and are decoded by the
VCF Operations 9.1.1 vCenter adapter's own client classes in the checks. A
`TaskEvent` comes first, carrying the task as it was queued; its key is the
chain id of every event of the operation and of the task itself.

| Operation | Task (description id), started by | Events after the `TaskEvent` |
|---|---|---|
| DRS moves a running VM | `Drm.ExecuteVMotionLRO`, the system (no user) | `EventEx` `com.vmware.vc.vm.VmHotMigratingWithEncryptionEvent`, `VmEmigratingEvent`, `DrsVmMigratedEvent` |
| A VM is powered on, or off | `VirtualMachine.powerOn`, `VirtualMachine.powerOff`, a user | `VmStartingEvent`, `VmPoweredOnEvent`; `VmStoppingEvent`, `VmPoweredOffEvent` |
| A VM is added | `Folder.createVm`, a user; then its power-on | `VmBeingCreatedEvent`, `VmInstanceUuidAssignedEvent`, `VmUuidAssignedEvent`, `VmCreatedEvent` |
| A VM is removed | its power-off; then `VirtualMachine.destroy`, a user | `VmRemovedEvent` |
| A host goes into maintenance mode | `HostSystem.enterMaintenanceMode`, a user; each running VM leaves by a DRS vMotion of its own | `EnteringMaintenanceModeEvent`, `VmBeingRelocatedEvent` and `VmRelocatedEvent` for each VM that is off, `HostDasDisabledEvent`, `EnteredMaintenanceModeEvent` |
| A host comes out | `HostSystem.exitMaintenanceMode`, a user | `ExitMaintenanceModeEvent`, `HostDasEnabledEvent` |

The user is `VSPHERE.LOCAL\Administrator`. Finished tasks leave vCenter's
recent-task list after ten minutes, as on a real vCenter.

**What it does not do.** Datastore filling changes each cluster's vSAN
datastore only, not local, NFS or iSCSI datastores. A real vCenter also
records a `VmMacAssignedEvent` for a new VM, distributed-switch port events
around power and removal, and a vSphere HA state event when a host comes out
of maintenance mode; the simulator does not.

vcsim gives every event its own key as chain id when it is posted, and
Activity sets the operation's chain id straight afterwards. Two things follow.
An event collector that was created with a filter on one chain id does not
receive that chain's later events as they happen (a query made afterwards
finds them). And a tool that is streaming events at the instant one is posted
can see it with its own key as chain id; for a new VM that instant is the
whole creation, about a millisecond, during which its creation events also
carry the UUIDs vcsim first gave it. A tool that reads events in pages
afterwards, as the VCF Operations adapter does, sees the finished chain.

Nothing Activity does raises an alarm. Rolling patching, nightly backups and
snapshots, and a host that crashes are not built yet.

### Checks

The first real Operations collection gate passed against the installed vSAN
Host Metrics 2.0.0 pack; its scope, procedure and measured result are in
[OPERATIONS-ACCEPTANCE](OPERATIONS-ACCEPTANCE.md).

| Check | How | Where it runs |
|---|---|---|
| Contract: every captured request replayed against the Phase 1 shape, responses compared by element structure, labels, bucket timestamps and fault type; identity joins across `/sdk` and `/vsanHealth`; determinism across instances and seeds; fixture sanitization; login restriction | `go test ./...` | CI `go` job |
| Phase 2: the ten-cluster scenario (identities on both surfaces per cluster, per-cluster cardinality, overrides), restart and edit identity stability through the registry, credentials, fault and latency knobs, inventory realism against the trimmed live inventory capture, gauge values inside the live envelopes, `Raw` counters monotonic | `go test ./...` | CI `go` job |
| Control plane: every console task keeps the identities it should (and rebuilds only for cluster, storage and version changes), hosts removed by name, faults switched on and off in place, scheduled changes without a lock, first-run account set-up, connected tools across a pause, a removed user and a new certificate, saved state across a restart and a changed scenario file, the reason recorded for each request (with every live capture replayed and never called "not answered"), redaction, admin API guards, the console's screens | `go test ./...` | CI `go` job |
| Smoke: a plain vSphere client (govc) connects with the scenario's credentials (and is refused with a wrong password), lists clusters, hosts and VMs and compares the counts with what the simulator built, dumps every identity, restarts the simulator on the same data directory and checks the dump is identical | `tools/smoke/smoke.sh` | CI `smoke` job |
| Chart: `helm lint --strict` with every `charts/vcenter-api-simulator/ci/*-values.yaml`, `helm template` and `kubeconform` of the rendered manifests and the Argo CD example | `.github/workflows/package.yml` | CI `package` workflow, `chart` job |
| Image: build, OCI label and non-root check, `docker compose` boot, then a vSphere client smoke ([`tools/smoke/smoke.ps1`](../tools/smoke/smoke.ps1)): `Connect-VIServer`, `Get-VMHost`, `Get-VM`, expected hosts and VMs back, wrong password refused | PowerCLI on `pwsh` | CI `package` workflow, `image` job |
| Chart install (manual): the chart in a disposable kind cluster with default values, the same smoke through the Service, the certificate unchanged across a pod restart, an upgrade to `tls.existingSecret` and restricted login that verifies against the issuing CA | [`tools/smoke/helm-kind.sh`](../tools/smoke/helm-kind.sh) | Optional: run manually (needs `kind`, `helm`, `kubectl`, `openssl`, Docker, `pwsh`), not required CI |
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
pwsh tools/smoke/smoke.ps1 -Server 127.0.0.1 -Port 8443 -Install -User <username> -Password <password>
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
[scenario reference](SCENARIO.md#osa-evidence-and-limits). The REST
surface models sessions, inventory reads, selected detail reads and captured
failure statuses. Shared NFS v3 and iSCSI VMFS inventory are optional; live
vCenter access remains outside this simulator.

## Running it

The image is `ghcr.io/sentania-labs/vcenter-api-simulator`, built from
[`Dockerfile`](../Dockerfile) on every push to `main` (tags `main` and
`sha-<commit>`) and on every `vMAJOR.MINOR.PATCH` tag (that tag, plus
`latest` for the highest version). It is a static binary on a distroless
non-root base, labeled with the source commit
(`org.opencontainers.image.revision`) and version. Pull requests build and
test the image without publishing it.

GHCR creates the package private on first publish. To pull it without
credentials (what the compose file and the chart's defaults assume), set it to
public once under the package's settings on GitHub. To keep it private, give
the chart an `imagePullSecrets` entry naming a registry pull secret.

There is no published vCenter login. Supply one with the scenario's
`auth.users`, or `VCSIM_USERNAME` and `VCSIM_PASSWORD` (`-username`/`-password`;
`VCSIM_ROLE`/`-role` makes that user `read-only`); otherwise the console's
first-run set-up chooses the password of `administrator@vsphere.local`, and
nothing can sign in before then. Console changes to supplied accounts take
precedence until the scenario file changes. Explicitly
opt into arbitrary credentials with `auth.anyLogin: true` or `-any-login`.
Use simulator-only credentials and never point real credentials at it.

### docker run

```sh
docker run -d --name vcsim -p 8443:8443 -p 127.0.0.1:9090:9090 -v vcsim-data:/data \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  ghcr.io/sentania-labs/vcenter-api-simulator:main
curl -sk https://localhost:8443/sdk/vimServiceVersions.xml
```

The second `-p` publishes the control console on loopback only. Open it to
choose the console password and, since this command supplies no account, the
password tools sign in with. The identity registry, the certificate and the console's applied
scenario are created on first start and kept
in the volume mounted at `/data`, so identities and the fingerprint a
management pack pinned survive restarts.
Add `-tls-names=<hostname>` (as a command argument) to put the name
collectors use into it, and `--read-only` needs the `/tmp` tmpfs: vcsim keeps
scratch files there.

### docker compose

[`docker-compose.yml`](../docker-compose.yml) runs the same thing with a named
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

[`charts/vcenter-api-simulator`](../charts/vcenter-api-simulator) installs one
simulated vCenter per release. It creates a Deployment (one replica, non-root,
read-only root filesystem, no capabilities), a ClusterIP Service on 443, a
ServiceAccount with no token mounted, a PVC for the state directory (identity registry, certificate and console state), and
optionally a Secret for credentials, an Ingress for `/sdk` and `/vsanHealth`
and a cert-manager Certificate. No RBAC, no privileges, nothing exposed
outside the cluster until `ingress.enabled`, `admin.service.enabled` or a
LoadBalancer Service asks for it.

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
certificate, and the query log when full capture is on. A Secret is overlaid
read-only on `/data/tls`. The PVC is annotated to survive `helm uninstall` and
Argo CD prune or Application deletion. With persistence off every new pod
issues new identities (and a new certificate when self-signed).

`persistence.size` (default 1Gi) must comfortably hold the query log's total
cap (`simulator.capture.queryLogMaxTotal`, 24 MiB by default) plus the state
files. On an upgrade the chart patches the claim's size, and Kubernetes grows a
bound claim only when its storage class sets `allowVolumeExpansion: true`; a
class without it rejects the whole release update. So for an existing install
the options are: set `persistence.size` to the size the claim already has (for
example `100Mi`) and let the log limit itself to the space that is there, which
keeps room for the state files; or, where the storage class allows it, expand the
claim (`kubectl patch pvc <name> -p '{"spec":{"resources":{"requests":
{"storage":"1Gi"}}}}'`), or delete the claim so the next sync recreates it
with the new size (identities and the pinned certificate are recreated too,
so do that only when a fresh set is acceptable).

A replaced certificate (a rotated Secret, a cert-manager renewal) is served
to the next connection without a restart: the simulator re-reads the mounted
files when they change.

The Ingress backend is HTTPS; tell the controller
(`nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"` for ingress-nginx).
When the Ingress terminates TLS, collectors pin the ingress certificate, not
the simulator's.

For VCF Operations' own vCenter adapter, two more things are needed behind an
Ingress. Add `/lookupservice`, `/sso-adminserver`, `/sts`, `/ls`, `/api`, `/rest`
and `/stats` to `ingress.paths` (the default is `/sdk` and `/vsanHealth` only).
And set `simulator.lookupTrustSecret` to the name of the Secret that holds the
Ingress's certificate (its `tls.crt` is mounted, nothing else): the lookup
service then tells Operations to trust that certificate at the sign-on
addresses, which is the one Operations will see there.

[`charts/vcenter-api-simulator/ci`](../charts/vcenter-api-simulator/ci) holds
example values for the Ingress, an existing TLS Secret, cert-manager, and no
persistence; `helm lint` runs each of them.

### Argo CD

[`examples/argocd-application.yaml`](../examples/argocd-application.yaml) is
one Application per simulated vCenter, pointing at the chart in this
repository with the hostname, seed, credentials Secret name and Ingress in
`valuesObject`. Copy it, change the name, hostname and seed, and it is
another endpoint. The chart needs nothing cluster-wide, so the Application
fits any project that may create a Deployment, Service, PVC and Ingress in
its namespace.
