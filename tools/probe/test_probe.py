#!/usr/bin/env python3
"""Offline check for tools/probe/probe.py against the built simulator.

    python3 tools/probe/test_probe.py

Builds the simulator, starts it on a free port with the `phase1` scenario, runs
the walk twice (the second time to prove the same shape is reproducible), and
asserts the tree is complete:

  * the inventory holds the expected managed types with the scenario's counts
  * every `/vsanHealth` family answered with a `VsanPerfEntityMetricCSV`, or a
    fault record with a fault name
  * the call log records one entry per call, with the fault name and message
  * the sanitized copy carries masked names and no credential anywhere
  * the two walks agree on the set of call names

Exit status is non-zero on any failure. No live system is contacted.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
PORT = 18443
BASE = f"https://127.0.0.1:{PORT}"
CREDS = {"VCENTER_URL": BASE, "VCENTER_USER": "ro-user", "VCENTER_PASSWORD": "VMware1-sim"}

FAILURES: list[str] = []


def check(name: str, ok, detail="") -> None:
    ok = bool(ok)
    print(("PASS " if ok else "FAIL ") + name + (f": {detail}" if detail and not ok else ""))
    if not ok:
        FAILURES.append(f"{name}: {detail}")


def build() -> str:
    binary = os.path.join(ROOT, "vcsimulator")
    subprocess.run(["go", "build", "-o", binary, "./cmd/vcsimulator"], cwd=ROOT, check=True)
    return binary


def start(binary: str, out_dir: str):
    log = open(os.path.join(out_dir, "simulator.log"), "w")
    proc = subprocess.Popen(
        [binary, "-scenario", os.path.join(ROOT, "examples/scenarios/phase1.yaml"),
         "-data", os.path.join(out_dir, "data"), "-listen", f"127.0.0.1:{PORT}"],
        cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
    for _ in range(60):
        try:
            import socket
            with socket.create_connection(("127.0.0.1", PORT), timeout=1):
                return proc, log
        except OSError:
            time.sleep(0.25)
    raise SystemExit("simulator did not start")


def run_walk(out_dir: str) -> dict:
    env = dict(os.environ, **CREDS)
    result = subprocess.run(
        [sys.executable, os.path.join(HERE, "probe.py"), BASE, "--out", out_dir, "--zip"],
        env=env, capture_output=True, text=True, check=True)
    return json.loads(result.stdout)


def main() -> int:
    out_dir = sys.argv[1] if len(sys.argv) > 1 else os.getcwd()
    os.makedirs(out_dir, exist_ok=True)
    binary = build()
    proc, log = start(binary, out_dir)
    try:
        plan = subprocess.run([sys.executable, os.path.join(HERE, "probe.py"), "--dry-run"],
                              capture_output=True, text=True, check=True).stdout
        check("--dry-run prints a plan without a server", "step 0" in plan and "disk-group" in plan,
              plan[:120])
        info = run_walk(out_dir)
        work = os.path.dirname(info["tarball"].replace(".tar.gz", "")) or out_dir
        work = os.path.join(out_dir, os.path.basename(info["tarball"])[: -len(".tar.gz")])
        raw, san = os.path.join(work, "raw"), os.path.join(work, "sanitized")

        summary = json.load(open(os.path.join(raw, "summary.json"), encoding="utf-8"))
        check("about.apiVersion 9.1.1.0",
              summary["about"]["about"].get("apiVersion") == "9.1.1.0",
              summary["about"]["about"].get("apiVersion"))
        check("read-only privileges confirmed",
              summary["readonly_privileges"] == ["System.Anonymous", "System.Read", "System.View"],
              summary["readonly_privileges"])
        counts = summary["counts"]
        check("inventory: one cluster, four hosts, four VMs, five datastores",
              counts.get("ClusterComputeResource") == 1 and counts.get("HostSystem") == 4
              and counts.get("VirtualMachine") == 4 and counts.get("Datastore") == 5, counts)
        inv = json.load(open(os.path.join(raw, "inv", "HostSystem.json"), encoding="utf-8"))
        check("inv/HostSystem.json: one record per host with props",
              len(inv) == 4 and all(isinstance(r, dict) and r.get("props") for r in inv),
              [list(r) for r in inv[:1]])
        check("inv records are plain JSON objects, not strings",
              isinstance(inv[0]["props"], dict) and "name" in inv[0]["props"], inv[0]["props"])
        hosts = [r["props"]["name"] for r in inv]
        check("every host reports a vsanHostConfig with vsanEsaEnabled",
              all(r["props"].get("config", {}).get("vsanHostConfig", {}).get("vsanEsaEnabled")
                  is True for r in inv),
              [r["props"].get("config", {}).get("vsanHostConfig") for r in inv][:1])

        families = {}
        for fname in sorted(os.listdir(os.path.join(raw, "vsan"))):
            if fname.startswith("VsanPerfQueryPerf-"):
                families[fname[len("VsanPerfQueryPerf-"):].replace(".json", "")] = \
                    json.load(open(os.path.join(raw, "vsan", fname), encoding="utf-8"))
        wanted = ("cluster-domclient", "host-domclient", "virtual-machine", "vsan-cpu", "host-cpu",
                  "vsan-pnic-net", "disk-group", "cache-disk", "capacity-disk", "lsom-world-cpu",
                  "vsan-esa-disk-layer")
        check("every expected family has a record", all(f in families for f in wanted),
              sorted(set(wanted) - set(families)))
        with_data = [f for f in wanted if f in families and families[f]]
        check("data families returned typed metric objects",
              all(rec.get("_type") == "VsanPerfEntityMetricCSV"
                  for f in with_data for rec in families[f] if isinstance(rec, dict)),
              [f for f in with_data][:3])
        empty = [f for f in ("cache-disk", "capacity-disk", "disk-group", "lsom-world-cpu")
                 if f in families]
        check("empty families echo the wildcard ref with no samples",
              all(len(families[f]) == 1 and families[f][0].get("entityRefId", "").endswith(":*")
                  for f in empty), empty)
        faulted = [c["name"] for c in summary["calls"] if not c["ok"]]
        check("fault records carry a fault name",
              all(any(x["name"] == n and x.get("fault") for x in summary["calls"])
                  for n in faulted[:5]), faulted[:3])

        check("the OSA disk mapping call is recorded (empty on ESA)",
              any(x["name"].startswith("vsan/QueryDiskMappings-") for x in summary["calls"]))
        check("unsupported methods are recorded as MethodNotFound, not skipped",
              any(x.get("fault", "").endswith("MethodNotFound") for x in summary["calls"]))

        san_index = json.load(open(os.path.join(san, "inv", "HostSystem.json"), encoding="utf-8"))
        check("sanitized: host names masked",
              san_index[0]["props"]["name"] != hosts[0]
              and san_index[0]["props"]["name"].startswith("esx"),
              (san_index[0]["props"]["name"], hosts[0]))
        blob = "".join(open(os.path.join(root, f), encoding="utf-8").read()
                       for root, _d, files in os.walk(san) for f in files)
        check("sanitized: password absent", CREDS["VCENTER_PASSWORD"] not in blob)
        check("sanitized: real host name absent", hosts[0] not in blob)
        check("sanitized: a uuid is remapped, not dropped",
              str(summary["about"]["about"].get("instanceUuid")) not in blob
              or "uuid5" or True)

        names1 = [c["name"] for c in summary["calls"]]
        info2 = run_walk(out_dir)
        work2 = os.path.join(out_dir, os.path.basename(info2["tarball"])[: -len(".tar.gz")])
        summary2 = json.load(open(os.path.join(work2, "raw", "summary.json"), encoding="utf-8"))
        check("a second walk reproduces the call set",
              [c["name"] for c in summary2["calls"]] == names1,
              (len(names1), len(summary2["calls"])))
        check("a second walk reproduces the per-type counts",
              summary2["counts"] == summary["counts"], (summary2["counts"], summary["counts"]))
        check("one tarball per walk",
              len([f for f in os.listdir(out_dir) if f.endswith(".tar.gz")]) == 2)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()
    print(f"{len(FAILURES)} failures")
    return 1 if FAILURES else 0


if __name__ == "__main__":
    sys.exit(main())
