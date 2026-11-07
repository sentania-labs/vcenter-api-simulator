# `tools/probe` — read-only vCenter API walk

One script, `probe.py`, walks a vCenter once with read-only calls and writes a
single tarball of the evidence the simulator is built against. The tarball
holds the sanitized tree only and is what to send back; the raw tree stays on
your disk, and only when you pass `--zip`.

## Requirements

- `python3` (3.9+) with `pyVmomi` and its bundled vSAN bindings on the path:
  `pip install pyvmomi` installs `pyVmomi`, `pyVim`, `vsanmgmtObjects` and
  `vsanapiutils`. No other packages are used.
- The simulator, or any vCenter, reachable over HTTPS. Nothing is installed by
  the script itself.

## Usage

```sh
# print the call plan only, no connection and no credential needed
python3 tools/probe/probe.py --dry-run

# walk the simulator on 127.0.0.1:18443
export VCENTER_URL=https://127.0.0.1:18443 VCENTER_USER=u VCENTER_PASSWORD=p
python3 tools/probe/probe.py https://127.0.0.1:18443 --out /tmp

# walk a real vCenter
export VCENTER_URL=https://vc.example:443 VCENTER_USER=ro@vsphere.local VCENTER_PASSWORD='…'
python3 tools/probe/probe.py "$VCENTER_URL" --out /tmp
```

| Argument | Meaning |
|---|---|
| `target` | `vcsim` (reads `VCENTER_URL`) or a base URL such as `https://host:443` |
| `--out DIR` | directory for the tarball; default the current directory |
| `--dry-run` | print the call plan and exit — no connection, no credential |
| `--zip` | keep the work directory (`raw/` and `sanitized/`) next to the tarball |

Credentials are read from `VCENTER_USER` and `VCENTER_PASSWORD`, and a missing
one is asked for interactively (the password is not echoed). `VCENTER_URL` is
read only when the target is `vcsim`. The password is never written; only the
tarball is written (plus the work directory with `--zip`).

TLS certificate verification is off, so the script can walk lab vCenters with
self-signed certificates, and the REST session is opened with Basic auth.
Point it only at a vCenter you trust.

## What is called

Read queries chosen by hand; nothing enforces at runtime that a call is
read-only. The only non-`GET` requests are the REST session `POST` and
`DELETE`. The calls are `Retrieve*`, `Query*`, `Get*` and `Fetch*` style
reads, the plain HTTP `GET`s, the vSAN Performance Service queries
for every entity family — including the OSA families `disk-group`,
`cache-disk`, `capacity-disk`, `lsom-world-cpu`, `ddh-disk`, `clom-disk`,
`clom-host` — the vSAN cluster config and health readers, PBM and VSLM profile
queries, and the `/api` REST `GET`s. The time windows are bounded (15 minutes
by default). `--dry-run` prints the full plan. A call that faults is recorded
with its fault name and the walk continues; a vSAN management object the
vCenter does not offer shows up as a fault on each call that needed it. The
vSAN calls run against the first vSAN-enabled cluster.

## What comes back

`vcs-walk-<timestamp>-<id>.tar.gz`, containing the sanitized tree:

```
about.json, inv/<Type>.json, submo/<Type>.json, managers/INDEX.json,
methods/<Call>.json, other/<Call>.json, http/<path>.json + GETS.json,
rest/INDEX.json + rest/<path>.json, vsan/<Call>.json, summary.json,
SANITIZE-INFO.json
```

Inventory names, the login user and the vCenter host name from the target,
UUIDs, IPv4 and MAC addresses, thumbprints, serial numbers, models and vendors
are masked consistently (one mapping per run). Other DNS names and IPv6
addresses are not masked, so skim the tree before sending it from a sensitive
environment. With `--zip` the work directory also holds `raw/`, the same tree
verbatim, with real host names, addresses, users and the vSphere session key.
Do not send `raw/`.

`summary.json` is the important part: `about`, per-type inventory counts, the
per-call log with `ok`, elapsed seconds and the fault name and message for any
call that did not answer, the endpoint outcomes, and the step timings. It is
"what answered", which is what the simulator's fixtures are derived from.

## Sending the result back

Send the `.tar.gz`; it holds only the sanitized tree, so a single file is all
that is needed. The password is never serialized, and the vSphere session key
is masked in the sanitized tree.

## Reading the output

- `summary.json` → `counts` is the inventory size per managed type, `calls`
  one record per call, `endpoints` the four SOAP endpoints other than `/sdk`,
  `steps` the timings.
- A fault record is `{"_fault": {"fault": ..., "msg": ...}}`; the same
  information is in the call log. `MethodNotFound` and `ManagedObjectNotFound`
  mean that part of the API is not served by that endpoint, which is itself the
  finding for the simulator.
- `SANITIZE-INFO.json` counts the files and names masked in that run.

## Checks against vcsim (no live system)

`tools/probe/test_probe.py` builds and starts the simulator on an ephemeral
port with the `phase1` scenario, runs the walk three times, and asserts the
tree is complete: the inventory types, the performance-service families, the
fault records, that the sanitized copy keeps type names and masks names, the
login user, hardware identifiers and credentials, that the tarball holds no
raw tree, and that a run without `--zip` leaves only the tarball. It needs
`go` and exits non-zero on any failure.

```sh
python3 tools/probe/test_probe.py [work-dir]
```
