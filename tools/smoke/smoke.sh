#!/usr/bin/env bash
# Phase 2 acceptance smoke: a plain vSphere client (govc) against the
# running simulator, the way an operator would check a vCenter.
#
#   tools/smoke/smoke.sh [--scenario examples/scenarios/ten-clusters.yaml] [--port 18443] [--keep]
#
# Builds the simulator, starts it with the scenario in a fresh data
# directory, then with govc: logs in with the scenario's credentials
# (and checks that a wrong password is refused), lists datacenters,
# clusters, hosts and VMs and compares the counts with what the simulator
# says it built, and dumps every identity a client keys on (MoRefs, vSAN
# cluster uuids, host hardware and node uuids, VM uuids and instanceUuids,
# disk uuids). Then it stops the simulator, starts it again on the same
# data directory and checks that the dump is byte-identical: identities
# survive a restart. Exit status is non-zero on any mismatch.
#
# Needs: go, govc (release binary: https://github.com/vmware/govmomi/releases/tag/v0.56.2),
# python3. No management pack, no Docker.
set -euo pipefail

here=$(cd "$(dirname "$0")/../.." && pwd)
scenario=$here/examples/scenarios/ten-clusters.yaml
port=18443
keep=no
storage=no
while [ $# -gt 0 ]; do
  case $1 in
    --scenario) scenario=$2; shift 2 ;;
    --port) port=$2; shift 2 ;;
    --keep) keep=yes; shift ;;
    --storage) storage=yes; shift ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done

work=$(mktemp -d)
cleanup() {
  stop_sim
  if [ $keep = no ]; then rm -rf "$work"; else echo "work dir kept: $work"; fi
}
trap cleanup EXIT

fail() { echo "FAIL $*" >&2; exit 1; }
pass() { echo "PASS $*"; }

sim_pid=
start_sim() {
  "$work/vcsimulator" -scenario "$scenario" -data "$work/data" -listen "127.0.0.1:$port" > "$work/sim-$1.log" 2>&1 &
  sim_pid=$!
  # ready when this process says it listens (not when something else holds the port)
  for _ in $(seq 1 100); do
    if grep -q "vcsimulator: listening on" "$work/sim-$1.log" 2>/dev/null; then
      return 0
    fi
    if ! kill -0 "$sim_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  cat "$work/sim-$1.log" >&2
  fail "simulator did not start"
}
stop_sim() {
  if [ -n "$sim_pid" ]; then
    kill "$sim_pid" 2>/dev/null || true
    wait "$sim_pid" 2>/dev/null || true
    sim_pid=
  fi
}

( cd "$here" && go build -o "$work/vcsimulator" ./cmd/vcsimulator )

# credentials: the first user of the scenario (password from the file or,
# with passwordEnv, from the environment), or any name/password
creds=$(python3 - "$scenario" <<'PY'
import os, re, sys
text = open(sys.argv[1]).read()
m = re.search(r"^auth:\s*\n\s+users:\s*\n\s+- name:\s*['\"]?([^'\"\n]+?)['\"]?\s*\n\s+(password|passwordEnv):\s*['\"]?([^'\"\n]+?)['\"]?\s*$", text, re.M)
if not m:
    print("smoke\tsmoke")
elif m.group(2) == "password":
    print(f"{m.group(1)}\t{m.group(3)}")
else:
    pw = os.environ.get(m.group(3))
    if pw is None:
        sys.exit(f"scenario user {m.group(1)} takes its password from ${m.group(3)}, which is not set")
    print(f"{m.group(1)}\t{pw}")
PY
) || exit 2
IFS=$'\t' read -r username password <<< "$creds"

export GOVC_URL="https://127.0.0.1:$port/sdk"
export GOVC_INSECURE=1
export GOVC_PERSIST_SESSION=false   # a cached session would hide a refused login
export GOVC_USERNAME=$username
export GOVC_PASSWORD=$password

expected() { # counts from the simulator's own banner
  python3 - "$work/sim-$1.log" <<'PY'
import re, sys
log = open(sys.argv[1]).read()
m = re.search(r"vcsimulator: (\d+) clusters, (\d+) hosts, (\d+) vms, (\d+) devices", log)
print(" ".join(m.groups()) if m else "0 0 0 0")
PY
}

collect() { # govc object.collect over a container view, as "<kind> <moref> <v1> <v2> ..." lines
  local kind=$1 type=$2; shift 2
  govc object.collect -type "$type" -json / "$@" | python3 -c '
import json, sys
kind = sys.argv[1]
def flat(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, dict):
        if "value" in v and "type" in v:
            return v["value"]
        if "_value" in v:
            return flat(v["_value"])
        return json.dumps(v, sort_keys=True)
    if isinstance(v, list):
        return ",".join(flat(x) for x in v)
    return "-" if v is None or v == "" else str(v)
# govc writes one JSON document per object, back to back
text = sys.stdin.read()
dec = json.JSONDecoder()
objs, pos = [], 0
while True:
    while pos < len(text) and text[pos].isspace():
        pos += 1
    if pos >= len(text):
        break
    doc, pos = dec.raw_decode(text, pos)
    objs.extend(doc if isinstance(doc, list) else [doc])
for obj in objs:
    if not isinstance(obj, dict) or "obj" not in obj:
        continue
    vals = {c["name"]: c.get("val") for c in obj.get("changeSet", []) if isinstance(c, dict)}
    print(kind, obj["obj"]["value"], " ".join(flat(vals.get(p)) for p in sys.argv[2:]))' "$kind" "$@"
}

dump() { # every identity a client keys on, sorted, to $1
  {
    govc about -json | python3 -c 'import json,sys; a=json.load(sys.stdin)["about"]; print("about", a["instanceUuid"], a["apiVersion"], a["build"])'
    collect datacenter Datacenter name parent
    collect cluster ClusterComputeResource name configurationEx.vsanConfigInfo.defaultConfig.uuid configurationEx.vsanConfigInfo.enabled host datastore
    collect host HostSystem name parent summary.hardware.uuid config.vsanHostConfig.clusterInfo.nodeUuid config.vsanHostConfig.clusterInfo.uuid config.storageDevice.scsiLun datastore
    collect vm VirtualMachine name config.uuid config.instanceUuid runtime.host runtime.powerState config.files.vmPathName
    collect datastore Datastore name summary.type summary.url summary.capacity info.membershipUuid info.vmfs.uuid
    # vcsim serves the vDS under the base type (a known gap; a container
    # view of VmwareDistributedVirtualSwitch is empty and govc would wait on it)
    collect vds DistributedVirtualSwitch name uuid summary.hostMember
    collect portgroup DistributedVirtualPortgroup name key host
  } | sort > "$1"
}

echo "== first start"
start_sim first
set -- $(expected first)
want_clusters=$1 want_hosts=$2 want_vms=$3

# a wrong password must be refused, the configured one accepted
if GOVC_PASSWORD="not-the-password" govc about >/dev/null 2>&1; then
  if [ "$username" != smoke ]; then fail "login with a wrong password succeeded"; fi
else
  pass "wrong password refused"
fi
govc about >/dev/null || fail "login with the configured credentials"
pass "Connect: govc about as $username"

n=$(govc find -type c | wc -l); [ "$n" -eq "$want_clusters" ] || fail "clusters: govc sees $n, simulator built $want_clusters"
pass "clusters: $n"
n=$(govc find -type h | wc -l); [ "$n" -eq "$want_hosts" ] || fail "hosts: govc sees $n, simulator built $want_hosts"
pass "Get-VMHost: $n hosts"
n=$(govc find -type m | wc -l); [ "$n" -eq "$want_vms" ] || fail "VMs: govc sees $n, simulator built $want_vms"
pass "Get-VM: $n VMs"
govc ls /*/host/* >/dev/null || fail "govc ls host folder"

if [ "$storage" = yes ]; then
  govc datastore.info -json shared-nfs shared-vmfs > "$work/storage-datastores.json"
  govc host.storage.info -json -t hba -host esx02.cluster-01.dc-01.sim.invalid > "$work/storage-hbas.json"
  govc host.storage.info -json -t lun -host esx02.cluster-01.dc-01.sim.invalid > "$work/storage-luns.json"
  govc metric.sample -n 1 -json /dc-01/datastore/shared-vmfs disk.capacity.latest disk.numberReadAveraged.average > "$work/storage-metrics.json"
  python3 - "$work" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
stores = {d['name']: d for d in json.loads((p/'storage-datastores.json').read_text())['datastores']}
assert stores['shared-nfs']['summary']['type'] == 'NFS'
vmfs = stores['shared-vmfs']
assert vmfs['summary']['type'] == 'VMFS' and vmfs['summary']['freeSpace'] == 10737418240
assert vmfs['info']['vmfs']['extent']
for d in stores.values():
    assert len(d['host']) == 4 and any(not m['mountInfo']['accessible'] for m in d['host'])
hbas = json.loads((p/'storage-hbas.json').read_text())['storageDeviceInfo']['hostBusAdapter']
iscsi = [h for h in hbas if 'iScsiName' in h]
assert len(iscsi) == 2 and all(h['configuredSendTarget'] and h['configuredStaticTarget'] for h in iscsi)
info = json.loads((p/'storage-luns.json').read_text())['storageDeviceInfo']
luns = [lun for lun in info['scsiLun'] if lun.get('vendor') == 'SIMULATR']
assert len(luns) == 1 and luns[0]['operationalState'] == ['lostCommunication']
paths = info['multipathInfo']['lun'][0]['path']
assert len(paths) == 2 and all(path['state'] == 'dead' for path in paths)
assert vmfs['info']['vmfs']['extent'][0]['diskName'] == luns[0]['canonicalName']
metrics = {v['name']: v['value'] for v in json.loads((p/'storage-metrics.json').read_text())['sample'][0]['value']}
assert metrics['disk.capacity.latest'] == [1073741824]
assert metrics['disk.numberReadAveraged.average'] == [0]
PY
  pass "govc storage datastores, adapters, LUN and fault states"
fi

# every host's node uuid on /sdk is unique, every cluster has a vSAN uuid
# and is enabled, every VM is powered on on a host of the inventory
dump "$work/dump-1.txt"
nodes=$(grep '^host ' "$work/dump-1.txt" | awk '{print $6}' | sort | uniq -d | wc -l)
[ "$nodes" -eq 0 ] || fail "duplicate vSAN node uuids"
grep '^cluster ' "$work/dump-1.txt" | awk '$4 == "-" || $5 != "true" {bad=1} END {exit bad}' || fail "a cluster lacks a vSAN uuid or is not vSAN-enabled"
grep '^vm ' "$work/dump-1.txt" | awk '$7 != "poweredOn" {bad=1} END {exit bad}' || fail "a VM is not powered on"
[ "$(grep -c '^vds ' "$work/dump-1.txt")" -ge 1 ] || fail "no vDS in the inventory"
grep -q 'key-vim.host.ScsiDisk' "$work/dump-1.txt" || fail "no host disks dumped"
pass "identities dumped: $(wc -l < "$work/dump-1.txt") lines"

echo "== restart on the same data directory"
stop_sim
start_sim second
dump "$work/dump-2.txt"
if ! diff -u "$work/dump-1.txt" "$work/dump-2.txt" > "$work/dump.diff"; then
  head -40 "$work/dump.diff" >&2
  fail "identities changed across the restart"
fi
pass "identities identical across restart ($(wc -l < "$work/dump-2.txt") lines)"
echo "smoke: all checks passed"
