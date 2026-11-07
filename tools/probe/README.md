# `tools/probe` — read-only vCenter API walk

One script, `probe.py`, walks a vCenter once with read-only calls and writes a
single tarball of the evidence the simulator is built against. Both a raw and a
sanitized copy are inside; the sanitized copy is what to send back.

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
| `--zip` | also keep the uncompressed tree next to the tarball |

Credentials are read from `VCENTER_URL`, `VCENTER_USER` and `VCENTER_PASSWORD`,
and any missing one is asked for interactively. Only these three variables are
read. The password is never echoed and never written; only the tarball is
written.

## What is called

Only the read-only allowlist: `Retrieve*`, `Query*`, `Get*`, `Fetch*`,
`RetrieveContent`, the plain HTTP `GET`s, the vSAN Performance Service queries
for every entity family — including the OSA families `disk-group`,
`cache-disk`, `capacity-disk`, `lsom-world-cpu`, `ddh-disk`, `clom-disk`,
`clom-host` — the vSAN cluster config and health readers, PBM and VSLM profile
queries, and the `/api` REST `GET`s. The time windows are bounded (15 minutes
by default). `--dry-run` prints the full plan. Any method not on that list is
refused without being sent, and the refusal is recorded in `summary.json`.

## What comes back

`vcs-walk-<timestamp>.tar.gz`, containing:

```
raw/         about.json, inv/<Type>.json, submo/<Type>.json, managers/INDEX.json,
             methods/<Call>.json, other/*.json, http/GET-*.body + GETS.json,
             rest/INDEX.json, vsan/<Call>.json, summary.json
sanitized/   the same tree with names, UUIDs, IP/MAC addresses, users and
             hardware model strings masked consistently (one mapping per run)
```

`summary.json` is the important part: `about`, per-type inventory counts, the
per-call log with `ok`, elapsed seconds and the fault name and message for any
call that did not answer, the endpoint outcomes, and the step timings. It is
"what answered", which is what the simulator's fixtures are derived from.

## Sending the result back

Send the `.tar.gz` — the sanitized tree inside it is enough, so a single file
is all that is needed. Keep the raw tree only if you were asked for it, since
it holds the real host names and addresses. No credential appears in either
tree: the password is never serialized and the session id is redacted.

## Reading the output

- `summary.json` → `counts` is the inventory size per managed type, `calls`
  one record per call, `endpoints` the four SOAP endpoints other than `/sdk`,
  `steps` the timings.
- A fault record is `{"_fault": {"fault": ..., "msg": ...}}`; the same
  information is in the call log. `MethodNotFound` and `ManagedObjectNotFound`
  mean that part of the API is not served by that endpoint, which is itself the
  finding for the simulator.
- `sanitized/SANITIZE-INFO.json` counts what was masked in that run.

## Checks against vcsim (no live system)

`tools/probe/test_probe.py` starts the simulator on a free port with the
`phase1` scenario, runs the walk, and asserts the tree is complete: the
inventory types, the performance-service families, the fault records, and that
the sanitized copy carries masked names and no credential. It prints one
summary line and exits non-zero on any failure.

```sh
( cd . && python3 tools/probe/test_probe.py )
```
