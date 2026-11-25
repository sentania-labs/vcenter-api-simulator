# Reference

Exact settings and behaviour. For how to get started, see the
[README](../README.md). For the scenario file, see [SCENARIO](SCENARIO.md). For
Kubernetes, see the [chart's README](../charts/vcenter-api-simulator/README.md).

- [Start-up flags and environment variables](#start-up-flags-and-environment-variables)
- [The data directory](#the-data-directory)
- [Accounts and the console password](#accounts-and-the-console-password)
- [The console](#the-console)
- [Request capture](#request-capture)
- [Activity](#activity)
- [Licensing and sign-on](#licensing-and-sign-on)
- [What is and is not answered](#what-is-and-is-not-answered)
- [Standard vSphere performance](#standard-vsphere-performance)
- [Docker Compose](#docker-compose)

## Start-up flags and environment variables

```sh
go build ./cmd/vcsimulator
./vcsimulator -scenario examples/scenarios/ten-clusters.yaml -data ./data -listen 0.0.0.0:8443
```

With no flags the simulator builds one cluster of 4 hosts and 4 VMs, listens on
`0.0.0.0:8443`, serves the console on `127.0.0.1:9090` and keeps its state in
`./data`. In the container, flags go after the image name.

| Flag | Meaning |
|---|---|
| `-listen ADDR` | Where the vCenter API listens, over HTTPS. Repeat for more than one. Default `0.0.0.0:8443`. A tool that ignores the port in its settings and uses 443 needs `-listen 0.0.0.0:443`, which needs permission to bind a low port |
| `-admin ADDR` | Where the console listens, over plain HTTP. Default `127.0.0.1:9090`. Env `VCSIM_ADMIN`. Empty turns the console off |
| `-admin-allowed-hosts NAMES` | Host names the console answers to besides `localhost`, `127.0.0.1` and `::1`. Any other name gets 403, which stops a web page reaching the console through DNS rebinding. Add the name your proxy forwards. Env `VCSIM_ADMIN_ALLOWED_HOSTS` |
| `-admin-secure-cookie` | Mark console cookies Secure. Use behind a proxy that ends TLS |
| `-admin-reset` | Remove the console password and exit. See [Accounts and the console password](#accounts-and-the-console-password) |
| `-scenario FILE` | The [scenario file](SCENARIO.md). Without it, the built-in one-cluster environment runs under `-seed` |
| `-seed NAME` | Identity seed for the built-in environment (default `phase1`). Ignored with `-scenario`, which names its own. The same seed gives the same vCenter identity, so give each simulator its own |
| `-regenerate` | Discard the identity registry. Needed after a seed change, which otherwise is refused at start |
| `-describe` | Print the environment's size (clusters, hosts, VMs, devices, and per cluster) as JSON and exit |
| `-data DIR` | Where state is kept (see below). Default `$VCSIM_DATA`, else `data`. The image sets `VCSIM_DATA=/data` |
| `-tls-names NAMES` | Extra DNS names or IPs for a newly made certificate. Used once, when the certificate is first created |
| `-tls-cert`, `-tls-key` | Serve this certificate and key (PEM) instead of a self-signed one. The scenario's `tls:` does the same |
| `-tls-dir DIR` | Where the self-signed certificate is kept. Default `<data>/tls` |
| `-tls-managed` | The deployment manages the certificate in `-tls-dir`: the console shows it read-only. The chart sets this |
| `-username`, `-password`, `-role` | One vCenter account supplied at start. Env `VCSIM_USERNAME`, `VCSIM_PASSWORD`, `VCSIM_ROLE`. `-role` is `administrator` (default) or `read-only` |
| `-any-login` | Accept any non-empty account name and password |
| `-session-timeout DURATION` | Idle timeout for vCenter sessions. Default none: sessions never expire |
| `-public-url URL` | The address tools should use, as shown in the console. Env `VCSIM_PUBLIC_URL`. Default `https://<name>:<port>` from the first `-tls-names` entry, or the machine's name. Set it when a proxy or ingress gives the simulator another name. Also the hostname appliance networking reads report |
| `-lookup-trust-cert FILE` | A PEM file with the certificate tools see at the simulator's address when a proxy in front ends TLS. Env `VCSIM_LOOKUP_TRUST_CERT`. See [behind a proxy](#behind-a-proxy-that-ends-tls) |
| `-appliance-dns-servers IPS` | Comma-separated DNS servers appliance networking reads report. Default: the process's own resolver |
| `-appliance-ipv4 CIDR` | The IPv4 address and prefix appliance networking reads report, for example `10.0.0.2/24`. Default: the first non-loopback address |
| `-report-url URL` | Where the console sends people to report a request the simulator cannot answer. Env `VCSIM_REPORT_URL`. Default this project's new-issue page |
| `-query-log PATH` | Start with full request capture on, writing to `PATH` |
| `-query-log-max-bytes N` | Bytes one log file may hold before it is compressed and a new one starts. Default 4194304 |
| `-query-log-max-files N` | How many log files to keep, oldest deleted first. Default 4 |
| `-query-log-max-total N` | Bytes the whole log may take. Default 25165824, or the data volume's size less 8 MiB when that is smaller |
| `-unanswered-stdout` | Start with unanswered requests also written to standard output as JSON lines. Has no console switch |
| `-debug-capture=false` | Start with the licensing and sign-on request record off. It is on by default |
| `-version` | Print the version and exit |

`VCSIM_ADMIN_PASSWORD` supplies the console password (see below).

## The data directory

Everything the simulator keeps is in `-data`, so a volume mounted there keeps
the environment across restarts.

| File | What it holds |
|---|---|
| `registry.json` | Every identity the simulator has handed out. Never reused |
| `tls/` | The self-signed certificate, made once |
| `sign-on-key.json` | The key sign-on tokens are signed with. Made when first needed, and kept while the seed stays the same |
| `console.json` | The environment as the console last changed it. Resumed at the next start, unless the scenario file changed, in which case the file wins |
| `vcenter-users.json` | Accounts made in the console, as password hashes |
| `console-password` | The console password, as a hash |
| `life.json` | Activity's dials and where it has put things |
| `capture-settings.json` | The request capture switches, once any has been used |
| `query-log.jsonl`, `query-log.N.jsonl.gz` | The full request log, when capture is on |
| `last-signin.json` | When tools last signed in, for Home |

If the volume is full, the simulator keeps serving from memory and says so in
its log and on Home. The environment may be lost at the next restart, and a
certificate that could not be saved changes then.

## Accounts and the console password

There is no published default password. Until an account exists nothing can sign
in to the simulated vCenter. An account comes from one of these:

- the console's first-run set-up, which chooses the password for
  `administrator@vsphere.local`;
- the scenario's `auth.users`;
- `-username` and `-password` (or `VCSIM_USERNAME` and `VCSIM_PASSWORD`), with
  `-role` or `VCSIM_ROLE` for a read-only account;
- `-any-login`, which accepts any non-empty name and password.

Accounts made in the console are kept, and win over the others until the
scenario file changes. Changing or removing an account ends its sessions.
Read-only accounts can read inventory and performance. Calls that change
something are refused. With the console off (`VCSIM_ADMIN=`), supplying an
account is the only way to sign in.

**The console has its own password**, separate from the vCenter accounts. The
first visit asks for it (at least 8 characters). It is kept as a hash in
`console-password`, and can be changed on Security. Set `VCSIM_ADMIN_PASSWORD`
(the chart's `admin.existingSecret`) to supply it instead. A supplied password
wins: if it differs from the stored one, the stored one is replaced at the next
start, console sessions end and one log line (never the value) says so. The
console shows it as read-only.

Console sessions end after 30 minutes without a person using the console (its
own background refresh does not count) and after 12 hours in any case. They do
not survive a restart.

**To reset the console password,** stop the simulator, run
`vcsimulator -admin-reset -data <dir>` (or delete `console-password` from the
data directory), start it again and choose a new one. Remove
`VCSIM_ADMIN_PASSWORD` first if your deployment supplies one.

**Upgrading from 0.3.x.** Earlier versions accepted a published default password
for `administrator@vsphere.local` when no account was supplied. It is gone.
After the upgrade a simulator with no supplied account refuses every sign-in
until the console's set-up chooses a password. A saved account keeps whatever
password it had, including the old default.

## The console

Open `http://127.0.0.1:9090/`. It has nine tabs: **Home, Clusters, Storage,
Versions, Users, Security, Activity, Slowness and faults, Requests**. It works
in light and dark, and at phone width.

It binds to loopback by default. It answers only to the Host names `localhost`,
`127.0.0.1` and `::1` (any port, so a port-forward and Docker work) and those in
`-admin-allowed-hosts`. Use TLS in front of it beyond loopback. State-changing
requests must be same-origin JSON.

- **Every change works the same way.** A short screen asks a few questions that
  are already answered. "What will happen" says in sentences what the change will
  do, and one button makes it: right away, or at a time later the same day. There
  is no shared draft. A scheduled change shows as a bar on every screen with "Do
  it now" and "Cancel it", locks nothing, and does not survive a restart.
- **Changes to clusters, hosts, storage and versions build a new vCenter** next
  to the running one and swap them. Connected tools are signed out and sign in
  again, as after a vCenter restart. Every identity the change keeps is
  unchanged. Users, the certificate, idle sign-out, slowness and faults, how
  licensing looks to tools, and everything Activity does change in place.
- **The simulator names clusters** (`cluster-12`) and invents storage connection
  details. Hosts are removed by name, and a removed host's number is not reused.
- **Home says what is true.** Whether a tool is connected, as which account and
  what it calls itself, and, if it is not, since when and why (its user was
  removed, the certificate was replaced, the simulator is paused, it signed
  itself out, or it has gone quiet). A tool counts as connected while it has been
  answered in the last 15 minutes and has not signed out since. Home also shows
  the build running (version, commit, the vCenter it presents, uptime) and how
  busy it has been: requests by outcome and by service, with a typical and a
  slow-end response time, for the last hour or since it started. Neither view
  survives a restart.
- **Requests says why each request was answered as it was.** "Not answered" means
  only requests the simulator has no answer for. A refused sign-in, a refusal a
  real vCenter makes too, and a fault you switched on are counted separately.
- **Add and remove, in scenario terms:** see
  [editing a running simulator](SCENARIO.md#editing-a-running-simulator-the-control-console).

The console's planning estimate for a change is 128Mi plus 2Mi per host, 256Ki per
VM and 16Ki per physical device, rounded up to 256Mi. When the simulator can read
its container's memory limit it warns before a change that exceeds it. It is a
planning number, not a forecast.

## Request capture

Open **Requests**. Requests the simulator has no answer for are captured from the
start. "Capture every request in full" keeps everything while it is on. Repeat the
action in your tool, then download from the same screen. Turning full capture
off deletes what it captured.

SOAP passwords, session operations and headers are left out or replaced, but
request bodies can contain inventory identifiers. Treat the downloads
accordingly. Unanswered records are kept in memory until deleted, and the console
shows the last 20,000 calls.

The full log is bounded. When a file reaches `-query-log-max-bytes` it is
compressed and a new one starts, and when the files together pass
`-query-log-max-total` the oldest are deleted. The chart's
`simulator.capture.queryLogMaxFileBytes`, `queryLogMaxFiles` and `queryLogMaxTotal`
set the three limits (0 keeps the default).

The start-up flags `-query-log`, `-unanswered-stdout` and `-debug-capture` only
set the first defaults. The first time a capture switch on Requests is used, all
the capture settings are saved in `capture-settings.json`, and from then on the
saved settings win at every start.

**Licensing and sign-on record.** On by default, with its own switch on Requests,
"Record what tools ask about licensing". It writes one JSON line to standard
output (`"event":"request-capture"`) for every request to the license service
(`/ls`), the lookup service (`/lookupservice`), the sign-on admin service
(`/sso-adminserver`) and the token service (`/sts`); for a `POST` to `/api`; and
for the four `/sdk` operations that carry a session from one connection to
another (`LoginBySessionTicket`, `AcquireSessionTicket`, `AcquireCloneTicket` and
`CloneSession`). Each line has the method, path, query, SOAP operation, status,
the fault type if the answer was one, whether a session cookie came with the
request (never its value), the names of all headers, the values of
Content-Type, SOAPAction, User-Agent, Accept and Content-Length, and the request
body.

Before logging, the content of any element, and the value of any attribute, JSON
or form field or query parameter whose name contains a credential word (password,
secret, token, cookie, ticket, credential, session, hash, assertion, license key,
signed entitlement, private key, security, signature, x509, sslTrust, user, owner
id or principal) is replaced by `[scrubbed:Name len=N]`, as is any run of more than
64 base64 or hex characters. A request to the token service is treated the other
way round so that its shape can be learned: every element and attribute is kept,
and the text of every element is removed unless it is a fixed name or a time. An
assertion loses everything inside it. A body over 64 KiB is cut and flagged.
Nothing of an answer is logged but its status and fault type.

## Activity

A vCenter nobody uses is still: no events, no tasks, a flat inventory. **Activity**
makes the simulated environment change by itself, the way one in use does. It is
off until you start it, and one button stops all of it.

**One dial for each thing that happens.** The middle of a dial is a baseline for an
environment of this size. Left is slower, down to off. Right is busier. The amounts
follow the number of VMs and hosts, so a larger environment changes more. Next to
each dial a sentence says what its position means in numbers.

| Dial | At the baseline | What a tool sees |
|---|---|---|
| Load balancing (DRS) | about 25 vMotions a day for every 100 VMs, in bursts of a few VMs, more in working hours | each VM on another host, with one DRS vMotion task and its events for each |
| VMs powered off and on | about 2 a day for every 100 VMs, mostly in working hours. A VM stays off for a while and comes back. Never more than one VM in ten is off | power state, guest state, the VM's performance numbers at zero while it is off |
| New VMs | about 1 a day for every 100 VMs, mostly in working hours. A cluster with fewer VMs than hosts counts as having one for each host | a new VM, powered on, with a number its cluster has not used since Activity was first started |
| VMs removed | about 1 a day for every 100 VMs | a VM powered off and removed. Its number and identities are not used again |
| Hosts in maintenance mode | 5% of the hosts, and others take their place about once a week | the host's VMs move off first, then the host says it is in maintenance mode. A host that comes out gets some VMs back |
| Rolling patching | each cluster every 30 days, from 6 PM, one host at a time | the host's VMs move off, it is in maintenance mode, its product info shows a new build, it reboots and returns half an hour later |
| Nightly backups | every VM every night, from 10 PM to 4 AM, after any patching | a snapshot task and a managed snapshot on every VM, removed ten minutes later |
| Host crash | off in the ready-made day; one a day across the vCenter when switched on | the host is `notResponding` with alarms raised, its VMs restart on other hosts within two minutes, and it returns 15 minutes later |
| Datastores filling | 0.4% of each vSAN datastore's capacity a day, a little every half hour | datastore free space, vSAN used capacity and the datastore's performance numbers |
| Datastore cleanup | 0.3% of capacity freed at 2 AM | the same numbers falling |
| CPU load | as the VMs were built (half to double) | CPU usage of VMs, hosts and clusters. Above the baseline, ready time (contention) rises too |
| Memory load | as the VMs were built (half to double) | memory used by VMs, hosts and clusters, up to what each VM has |

- **For the whole vCenter, and for each cluster.** "These dials are for" chooses.
  A cluster follows the vCenter's dials until you move one of its own. That dial
  then applies to that cluster only, and "Follow the vCenter again" puts it back.
- **Exact numbers.** "Type an exact number" sets a dial to one VM every so many
  minutes, hours or days, so many vMotions a day, or a percentage (hosts in
  maintenance mode, up to a quarter; datastore growth, up to 10% a day). An exact
  number stays as typed whatever the size of the environment. One VM every so often
  is spread evenly over day and night. An exact number of vMotions a day still
  comes in bursts, more in working hours. Moving the dial goes back to a level.
- **Limits.** The number of VMs stays between two numbers, 5% below and above the
  number there was before Activity changed anything unless you change them. They
  move with clusters and hosts you add or remove. At a limit, adding or removing
  waits and says so once a day. At most a quarter of a cluster's hosts are down or in
  maintenance mode at once. A vSAN datastore is not filled past 90%, and a cleanup
  frees only what Activity filled.
- **A tool that is subscribed sees each change as it happens.** What Activity changes
  (a host's maintenance mode, power state, VM placement, VM membership of hosts and
  datastores, datastore space, load) is sent to property collector subscriptions on
  the properties themselves, not only on the parent (for example
  `runtime.inMaintenanceMode` as well as `runtime`). A collection that asks for a leaf
  property and one that asks for the whole object both get it.
- **"Do it now"** on a cluster's dial does that thing once, straight away, after
  saying what it will do.
- **A change that moves hosts asks first.** Starting, stopping, and changing the
  share of hosts in maintenance mode each say how many hosts and VMs will move.
- **While the simulator is paused** nothing moves. Start, Stop, "Do it now" and a
  change to the hosts in maintenance mode are refused until you resume.
- **Stop** leaves the environment still and healthy: hosts come out of maintenance
  mode, crashed and patching hosts return, temporary backup snapshots are removed,
  and VMs that are off are powered on. VMs that were added or removed stay
  that way, and datastores stay as full as they are.
- **"What has happened"** lists every cause with what it set off as one line, newest
  first, with the detail one click away. It says which lines came from a button you
  pressed. The last 1,000 lines are kept.
- **The same seed and settings make the same things happen** at the same times.
  Times of day are the simulator's local time.
- **It survives a restart.** The dials, where each VM is, which are off, which hosts
  are in maintenance mode and how full the datastores are are in `life.json`. VMs
  that were added or removed are part of the saved scenario (`addedVMs` and
  `removedVMs`, see [the scenario file](SCENARIO.md#editing-a-running-simulator-the-control-console)).
  Nothing happens while the simulator is off or paused, and nothing is caught up
  afterwards. A change that rebuilds the vCenter keeps all of it. "Replace
  everything with a new environment" starts again.
- **If something fails three times in a row,** that one dial stops itself, the tab
  shows a problem, and "Switch it on again" resumes it. The rest carries on.

**What vCenter records.** Each operation is recorded as a real vCenter 9.1.1 records
it. A `TaskEvent` comes first, carrying the task as it was queued. Its key is the
chain id of every event of the operation and of the task itself.

| Operation | Task (description id), started by | Events after the `TaskEvent` |
|---|---|---|
| DRS moves a running VM | `Drm.ExecuteVMotionLRO`, the system | `EventEx` `com.vmware.vc.vm.VmHotMigratingWithEncryptionEvent`, `VmEmigratingEvent`, `DrsVmMigratedEvent` |
| A VM is powered on, or off | `VirtualMachine.powerOn`, `VirtualMachine.powerOff`, a user | `VmStartingEvent`, `VmPoweredOnEvent`; `VmStoppingEvent`, `VmPoweredOffEvent` |
| A VM is added | `Folder.createVm`, a user, then its power-on | `VmBeingCreatedEvent`, `VmInstanceUuidAssignedEvent`, `VmUuidAssignedEvent`, `VmCreatedEvent` |
| A VM is removed | its power-off, then `VirtualMachine.destroy`, a user | `VmRemovedEvent` |
| A host goes into maintenance mode | `HostSystem.enterMaintenanceMode`, a user. Each running VM leaves by a DRS vMotion of its own | `EnteringMaintenanceModeEvent`, `VmBeingRelocatedEvent` and `VmRelocatedEvent` for each VM that is off, `HostDasDisabledEvent`, `EnteredMaintenanceModeEvent` |
| A host comes out | `HostSystem.exitMaintenanceMode`, a user | `ExitMaintenanceModeEvent`, `HostDasEnabledEvent` |

The user is `VSPHERE.LOCAL\Administrator`. Finished tasks leave vCenter's
recent-task list after ten minutes, as on a real vCenter.

**What it does not do.**
- Datastore filling changes each cluster's vSAN datastore only, not local, NFS or
  iSCSI datastores.
- A real vCenter also records a `VmMacAssignedEvent` for a new VM, distributed
  switch port events around power and removal, and a vSphere HA state event when
  a host comes out of maintenance mode. The simulator does not.
- Only a crashed host raises an alarm. The five proposed extras (datastore-full
  alarms, hot VMs, network-link failures, forgotten snapshots, and a permanent
  powered-off VM share) are not built.
- An event collector created with a filter on one chain id does not receive that
  chain's later events as they happen, though a query made afterwards finds them.
  A tool streaming events at the instant one is posted can see it with its own key
  as chain id (for a new VM, about a millisecond). A tool that reads events in pages
  afterwards, as VCF Operations does, sees the finished chain.

**Patching, backups and host crashes.** Rolling patching starts from 6 PM, with each
cluster due every 30 days at the baseline. Clusters and hosts take turns. A host
evacuates its VMs, enters maintenance, receives a simulated build and patch-level
increment in its product info, reboots, and returns after half an hour. The
quarter-of-hosts limit counts both maintenance and failed hosts. Activity's own
maintenance rotation yields to patching so a host need not wait a week to be patched.

Nightly backups start between 10 PM and 4 AM, after patching, one cluster at a
time. Real snapshot tasks create managed snapshot objects on every VM; each
snapshot is removed ten minutes later. Half-hour history entries group the work.
The backup dial changes the number of nights between runs, with at most one run per
night. Patching and backups also accept an exact number of days.

Host crashes are off in the ready-made day. The baseline is one crash a day across
the vCenter, with independent cluster overrides. A failed host reports
`notResponding`; its running VMs become unavailable, then restart on other hosts
within two minutes. The host returns after 15 minutes by default, configurable from
1 to 1,440 minutes in its exact-number dialog. `Host connection and power state` and
`vSphere HA host status` raise and clear using the default names and expressions
read from lab vCenter 9.1.1. HA uses the current `EventEx` event IDs. Performance
reads exclude down or maintenance hosts and reflect the VMs' new placement. Existing
saved setups leave the three new dials off on upgrade.

Patching, backups and host crashes have global and cluster dials, exact numbers and a
cluster's **Do it now** button like the rest. Their products, failed hosts, snapshots
and job progress survive a rebuild or restart.

## Licensing and sign-on

VCF Operations asks a vCenter a chain of questions before it collects: where its
sign-on service is, whether the account may manage licenses, what license the
vCenter has, and which Operations manages that license. The simulator answers the
chain with its own values only. No key, token, certificate or license in it comes
from a real system. It presents as an evaluation vCenter and never as a licensed
one: a licensed vCenter has a signed entitlement that only the vendor can issue,
and the simulator does not make one, copy one or accept one.

| Operations asks | The simulator answers |
|---|---|
| The lookup service (`/lookupservice/sdk`) for vCenter and its sign-on service | One registration each, with addresses built on the address the tool used (`X-Forwarded-Host` when a proxy sets it, else `Host`) and the certificate to trust for them. Any other filter, and no filter, returns an empty list |
| The sign-on admin service (`/sso-adminserver/sdk/vsphere.local`) for its content, its domain, its SSL certificate manager and its signing certificate | `vsphere.local`, a typed manager reference, and the certificate of the simulator's own signing key. No sign-in needed |
| The token service (`/sts/STSService/vsphere.local`, and `/sts/STSService`) for a token, with a username and password | A signed SAML bearer token for one of the simulator's accounts, with an RSA-SHA256 signature made with an RSA 3072 key kept in `sign-on-key.json`. It lists the groups that make an account a licensing administrator, unless you take them out |
| The license service (`/ls/sdk`): `Login`, `Logout`, and signed in `SearchAssets` (plain and filtered by product family, license state and license id, with paging), `SearchLicenses` and `QuerySystemTime` | vCenter and each host in evaluation with 90 days left, or with the evaluation run out. No licenses. A license session never signs in to `/sdk` |
| Whether the extension `com.vmware.vrops.license.managed` is there (`/sdk`) | Not there, or there with the Operations id and address you typed, or as an Operations registered it. Registering, changing and removing it all work and are kept across restarts |
| To sign in with a session ticket (`LoginBySessionTicket`) | "The session ticket is invalid", as a real vCenter says to a ticket it did not issue |
| Host and datacenter references for usage collection | Answered, including the host's evaluation block and the datacenter and cluster names |

The settings `config.vpxd.sso.sts.uri`, `config.vpxd.sso.admin.uri` and
`config.vpxd.sso.groupcheck.uri` name the sign-on paths under `-public-url`.

**How it looks to tools** is on the console's Security screen ("How licensing
looks to tools") and in the [scenario file](SCENARIO.md#licensing): whether the
evaluation is running or has run out, whether accounts are licensing
administrators, and which VCF Operations, if any, vCenter says manages its
license. Changing it does not rebuild the vCenter.

### Behind a proxy that ends TLS

Give the simulator the proxy's certificate with `-lookup-trust-cert` (the chart's
`simulator.lookupTrustSecret`), and route `/lookupservice`, `/sso-adminserver`,
`/sts`, `/ls`, `/api`, `/rest`, `/stats`, `/wcp` and `/vsanHealth` to it as well
as `/sdk`. The chart routes these by default. The lookup
service tells a tool which certificate to trust at the sign-on addresses, and
behind a proxy that is the proxy's, not the simulator's. The file is read again
when it changes.

### What is not built, and what is unverified

**Not built:** a licensed vCenter, assigning a license to the simulator, the REST
lists of license assignments, session tickets that work (`AcquireSessionTicket`),
and an unfiltered lookup list (a real vCenter returns about 48 registrations, the
simulator none).

**Unverified.** The answers were built from what three real vCenter 9.1.1 systems
returned and from what the Operations adapter reads. These parts were not
observed, so the simulator's answer is its own:

- Lookup service: the values of the `GroupInternalId`, `ControlScript` and
  `Capabilities` attributes and of the site id (only their lengths were seen), the
  owner name of the sign-on registration, the text of the fault for a wrong
  reference (its type was seen), and whether the session cookie repeats from one
  answer to the next. The `localurl` attribute names port 8085, which the simulator
  does not serve.
- Sign-on admin: the value of `apiRevision` and the SSL certificate-manager
  reference value. Only the four reads above are answered.
- Token service: the request it accepts is the standard form a real vCenter
  accepted, not one captured from Operations. The answer to a wrong password is the
  standard WS-Security fault. The given name and surname in the token are taken
  from the account name. A read-only account is listed in `Everyone` only.
- License service: every real vCenter observed was licensed, so vCenter's asset in
  evaluation is put together from the evaluation block those assets carry and from
  one host that was in evaluation. For hosts, the asset id, the product id and name,
  and the cost units are the simulator's own. So are the hours and minutes left, the
  90 days, an evaluation that never counts down, what an expired evaluation looks
  like, and the fault for a read that is not signed in or a refused login. The
  `disallowedHostEditions` property and vSAN assets are left out.
- The "managed by" extension: the subject name is left out unless an Operations
  registered one, and the version is vCenter's own unless an Operations registered
  another.

The licensing and sign-on record (see [Request capture](#request-capture)) lets these
gaps be closed from what Operations really sends.

## What is and is not answered

**Answered.**
- Sign-in and sign-out on SOAP (`/sdk`) and REST (`/api/session`,
  `/rest/com/vmware/cis/session`), for the accounts you set up. A signed-out or
  anonymous `SessionManager.currentSession` read answers an empty result.
- Datacenters, clusters, hosts, VMs, datastores, networks, resource pools and
  distributed switches on SOAP, built on vcsim.
- The same lists on REST (`/api/vcenter/...`, and `/rest/vcenter/...` with a
  `value` envelope on the older paths). List endpoints honour `names`, the
  parent and state filters that apply to each kind and the id filter of each kind,
  in the plain and `filter.`-prefixed spellings. Any other parameter returns 400
  `INVALID_ARGUMENT`. REST reads share SOAP identities.
- vSAN (`/vsanHealth`): performance data for all 69 kinds on ESA and OSA clusters,
  settings, disk lists, health summary and the resync summary (always nothing
  to sync). Values are made up, the same every time for the same moment, and inside
  the range a real vCenter showed. The service-versions document
  (`/sdk/vsanServiceVersions.xml` and `/vsanHealth/vsanServiceVersions.xml`) is cut
  back to the namespaces the selected
  vCenter version speaks.
- Standard vSphere performance and counter discovery ([below](#standard-vsphere-performance)).
- The inventory-relationship query Operations uses for hosts, datacenters and
  clusters.
- Version, appliance, topology and supervisor reads on `/api`: the version,
  account settings and policy, access, firewall, logging forwarding, time
  synchronisation and TLS profile (invented values), the Salt minion settings,
  networking, DNS and hostname (the simulator's own hostname, DNS servers and
  address), the backup lists (none) and the topology nodes. `/wcp` answers as a
  vCenter with Kubernetes turned off does. Namespaces, storage policies and
  supervisors are empty.
- REST paths that answer with a refusal: appliance health, content libraries, ESX
  cluster settings and the deployment type get 403. Any other REST path the
  simulator does not know gets 404.
- Tag collection (`POST /api`): sign-in, an empty list, and sign-out.
- The root certificate (`/rest/vcenter/certificate-authority/get-root` and its
  `/api` forms), as the simulator's own certificate, once the request is signed in.
- Licensing, sign-on and token services, [above](#licensing-and-sign-on).
- Operations' GPU vStats query and live stream (`/api`, `/stats`): the query is
  accepted, and the stream stays open and silent, as on a vCenter with no GPUs.
  Tokens use only the simulator's signing key.
- iSCSI and NFS datastores, when you add them. Capacity matches inventory. I/O
  follows the VMs using each datastore and is zero on unused ones.
- Which version vCenter and its hosts are: 8.0 Update 3, 9.0, 9.1.1, or a number you
  type. One cluster can hold a mix of ESX versions.

**Not answered or only partly.**
- The simulator has no ESX endpoints. Direct connections to a host, and the host
  health reports Operations reads through them, get no answer. Operations logs "No
  ESX health reports found" for each host, as it does against real lab vCenters.
- Distributed switches are served as vcsim's base type `DistributedVirtualSwitch`,
  not `VmwareDistributedVirtualSwitch`.
- Fibre Channel storage, and vSAN acting as a file server or an iSCSI server: reported
  as absent. NFS 4.1, storage I/O, discovery or rescan, CHAP and live path
  transitions are not modelled. The iSCSI shape is plausible, not verified against
  an iSCSI host.
- Some nested VM and OSA health details are partial. OSA has no samples for VM-level
  families, stretched clusters, several disk groups per host, dedup or populated
  iSCSI ([OSA limits](SCENARIO.md#osa-evidence-and-limits)).
- `/sdk/vimService.wsdl` is served. `vim.wsdl` is not.
- GPU statistics, backups, tags, Kubernetes and storage policies answer with nothing,
  as above.
- A licensed vCenter, license assignment, session tickets and the unfiltered lookup
  list ([above](#what-is-not-built-and-what-is-unverified)).
- Events and tasks beyond what Activity and the build of the inventory make.

The console's Requests screen shows what your tool asked for that got no answer.

## Standard vSphere performance

`QueryPerf`, `QueryPerfComposite`, counter discovery and host and VM quickStats
share one workload model. Six VM groups range from idle to busy, with a daily
cycle, a weekly cycle, smooth shorter variation and brief daily peaks of
contention and latency. Values depend on the telemetry seed, the VM's identity
and the sample time, with no stored series and no random noise at request time.
Repeating a historical query on an unchanged environment returns the same values,
including after a restart.

The inventory supplies power state, connection state, maintenance mode, placement,
hardware capacity, network interfaces and datastore membership. Powered-off VMs
have no samples. VMs on unavailable hosts or hosts in maintenance mode contribute
no load. A VM that moves takes its load with it. Host CPU and memory budgets cap
usage when the VMs add up to more. Host values sum their VMs, clusters sum hosts,
and datacenters sum clusters. Datastore I/O is split among each VM's datastores,
and unused datastores have zero I/O.

History is a replay of the workload at the requested times using the **current**
inventory and placement. Past power changes and moves are not remembered.

| Interval | Retention | Providers |
|---|---|---|
| 20 seconds | 1 hour | VM, host, resource pool |
| 300 seconds | 1 day | VM, host, pool, cluster, datastore, datacenter |
| 1,800 seconds | 1 week | the same |
| 7,200 seconds | 30 days | the same |
| 86,400 seconds | 1 year | the same |

Samples are ascending on UTC interval boundaries, after `startTime` and at or
before `endTime`. Future samples are not returned. `maxSample` selects the latest
samples in the window. Historical average, minimum, maximum and summation
counters roll up the 20-second values, and latest counters take the endpoint.
Percent counters use hundredths of a percent, CPU rates MHz, memory KiB,
throughput KiB/s, ready and wait time milliseconds over the interval, and latency
the counter's own unit. Hosts and VMs also serve power (`power.power`, watts) and
energy (`power.energy`, joules over the 20-second sample); hosts add the power
capacity counters, and their usable power is at least 750 W and grows with host
size. A host in maintenance mode reports a reduced idle draw with no VM power. A
host that is not connected or not powered on is listed as supporting the counters
but answers -1 (no sample) for them. Wildcards expand into the aggregate (empty) instance and
the CPU, NIC, virtual disk or datastore instances. Metrics the simulator does not
produce are left out. A query may return at most two million values; a wider one
returns `InvalidArgument`, so split it into smaller time windows.

VCF Operations 9.1.1 uses CSV queries, 20-second VM and host samples and the
configured historical interval (normally 300 seconds), including wildcard
instances. Its `QueryPerfCounterInt` discovery receives 28 enabled guest counters
with stable simulator-owned ids starting at 20001. Operations' default vStats
request is for GPU metrics only, which a vCenter with no GPUs answers with nothing.

## Docker Compose

[`docker-compose.yml`](../docker-compose.yml) runs the same container as the
README's `docker run`, with a named volume for the state.

```sh
docker compose up -d                 # pulls ghcr.io/sentania-labs/vcenter-api-simulator:main
docker compose up -d --build         # builds from this checkout
VCSIM_USERNAME=operator VCSIM_PASSWORD=placeholder docker compose up -d   # starts with an account
docker compose down -v               # also discards the identities and certificate
```

Other settings: `VCSIM_TAG` (the image tag, default `main`), `VCSIM_PORT` (the API
port, default 8443), `VCSIM_ADMIN_PORT` (the console's loopback port, default 9090),
`VCSIM_SEED` and `VCSIM_TLS_NAMES`, and `VCSIM_ROLE` for the account.
