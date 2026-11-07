#!/usr/bin/env python3
"""Offline check for tools/probe/probe.py against the built simulator.

    python3 tools/probe/test_probe.py [work-dir]

Builds the simulator, starts it on an ephemeral port with the `phase1`
scenario, and runs the walk three times (twice with --zip, once without). It
asserts:

  * the inventory holds the expected managed types with the scenario's counts
  * every `/vsanHealth` family answered with a `VsanPerfEntityMetricCSV`
  * every call record is a `.json` file and every fault in the call log is
    backed by a `_fault` record of the same name on disk
  * the sanitized copy keeps managed-object type names, masks host names, the
    login user, hardware identifiers and credentials, and remaps UUIDs
  * the tarball holds the sanitized tree only, and a run without --zip leaves
    nothing but the tarball
  * two walks agree on the set of call names

Exit status is non-zero on any failure. No live system is contacted.
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import re
import socket
import subprocess
import sys
import tarfile
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
USER, PASSWORD = "ro-user", "VMware1-sim"
MASKED_KEYS = ("serialNumber", "model", "vendor")
UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

FAILURES: list[str] = []


def check(name: str, ok, detail="") -> None:
    ok = bool(ok)
    print(("PASS " if ok else "FAIL ") + name + (f": {detail}" if detail and not ok else ""))
    if not ok:
        FAILURES.append(f"{name}: {detail}")


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def build() -> str:
    binary = os.path.join(ROOT, "vcsimulator")
    subprocess.run(["go", "build", "-o", binary, "./cmd/vcsimulator"], cwd=ROOT, check=True)
    return binary


def start(binary: str, out_dir: str, port: int):
    log = open(os.path.join(out_dir, "simulator.log"), "w")
    proc = subprocess.Popen(
        [binary, "-scenario", os.path.join(ROOT, "examples/scenarios/phase1.yaml"),
         "-data", os.path.join(out_dir, "data"), "-listen", f"127.0.0.1:{port}"],
        cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
    for _ in range(60):
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return proc, log
        except OSError:
            time.sleep(0.25)
    proc.kill()
    raise SystemExit("simulator did not start")


def run_walk(base: str, out_dir: str, keep: bool) -> dict:
    env = dict(os.environ, VCENTER_USER=USER, VCENTER_PASSWORD=PASSWORD)
    cmd = [sys.executable, os.path.join(HERE, "probe.py"), base, "--out", out_dir]
    if keep:
        cmd.append("--zip")
    result = subprocess.run(cmd, env=env, capture_output=True, text=True, check=True)
    return json.loads(result.stdout)


def work_dir(info: dict) -> str:
    return info["tarball"][: -len(".tar.gz")]


def load(*parts: str):
    with open(os.path.join(*parts), encoding="utf-8") as fh:
        return json.load(fh)


def pairs(node, key=None):
    """Every (key, string value) in a JSON document."""
    if isinstance(node, dict):
        for k, v in node.items():
            yield from pairs(v, k)
    elif isinstance(node, list):
        for v in node:
            yield from pairs(v, key)
    elif isinstance(node, str):
        yield key, node


def tree_files(root: str):
    for dirpath, _d, files in os.walk(root):
        for fname in files:
            yield os.path.join(dirpath, fname)


def tarball_files(path: str) -> dict:
    out = {}
    with tarfile.open(path) as tf:
        for member in tf.getmembers():
            if member.isfile():
                out[member.name] = tf.extractfile(member).read().decode("utf-8", "replace")
    return out


def load_probe():
    spec = importlib.util.spec_from_file_location("probe", os.path.join(HERE, "probe.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def unit_checks(work: str) -> None:
    """In-process checks of behaviour the simulator cannot exercise."""
    probe = load_probe()

    forms = {'"abc123"': "abc123", '{"value": "abc123"}': "abc123", "": None, "{}": None}
    check("REST session token: JSON string and {value} forms parse to the bare token",
          all(probe._session_token(body) == want for body, want in forms.items()),
          {b: probe._session_token(b) for b in forms})

    raw = os.path.join(work, "unit-raw")
    os.makedirs(os.path.join(raw, "methods"))
    os.makedirs(os.path.join(raw, "inv"))
    docs = {
        "inv/HostSystem.json": [{"_moref": "HostSystem:host-1", "props": {"name": "esx-real-1"}}],
        "about.json": {"about": {"apiVersion": "9.1.1.0", "version": "9.1.1",
                                 "fullName": "VMware vCenter Server 9.1.1.0 build-24000000"}},
        "methods/Events.json": [{"fullFormattedMessage": "Host 10.1.2.3. down; peer 10.4.5.6, mac "
                                                         "AA:BB:CC:DD:EE:FF; driver 1.2.3.4.5"}],
    }
    for rel, doc in docs.items():
        with open(os.path.join(raw, rel), "w", encoding="utf-8") as fh:
            json.dump(doc, fh)
    san = os.path.join(work, "unit-san")
    probe.sanitize_tree(raw, san)
    message = load(san, "methods", "Events.json")[0]["fullFormattedMessage"]
    about = load(san, "about.json")["about"]
    check("sanitizer masks an IPv4 followed by a period or comma, and a MAC",
          not any(t in message for t in ("10.1.2.3", "10.4.5.6", "AA:BB:CC:DD:EE:FF")), message)
    check("sanitizer keeps a five-part version", "driver 1.2.3.4.5" in message, message)
    check("sanitizer keeps the about version strings",
          about == docs["about.json"]["about"], about)
    check("sanitizer masks inventory host names",
          "esx-real-1" not in json.dumps(load(san, "inv", "HostSystem.json")))

    cap = probe.Capture(os.path.join(work, "unit-cap"))
    result = cap.call(("methods", "Absent"), probe._MissingMo("vsanSystem").QueryHostStatus)
    check("a missing management object is recorded as a fault and the walk goes on",
          result is None and len(cap.calls) == 1 and not cap.calls[0]["ok"]
          and os.path.exists(os.path.join(work, "unit-cap", "methods", "Absent.json")), cap.calls)

    out = os.path.join(work, "unit-main")
    os.makedirs(out)

    def boom(*_args, **_kwargs):
        raise RuntimeError("sanitize failed")

    saved = (probe.walk, probe.sanitize_tree, sys.argv, dict(os.environ))
    probe.walk, probe.sanitize_tree = (lambda *a, **k: None), boom
    sys.argv = ["probe.py", "https://127.0.0.1:1", "--out", out]
    os.environ.update(VCENTER_USER=USER, VCENTER_PASSWORD=PASSWORD)
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            probe.main()
        failed = False
    except RuntimeError:
        failed = True
    finally:
        probe.walk, probe.sanitize_tree, sys.argv = saved[:3]
        os.environ.clear()
        os.environ.update(saved[3])
    check("a failed sanitize still removes the raw work directory",
          failed and os.listdir(out) == [], os.listdir(out))


def main() -> int:
    out_dir = sys.argv[1] if len(sys.argv) > 1 else tempfile.mkdtemp(prefix="probe-test-")
    os.makedirs(out_dir, exist_ok=True)
    unit_checks(out_dir)
    port = free_port()
    base = f"https://127.0.0.1:{port}"
    binary = build()
    proc, log = start(binary, out_dir, port)
    try:
        plan = subprocess.run([sys.executable, os.path.join(HERE, "probe.py"), "--dry-run"],
                              capture_output=True, text=True, check=True).stdout
        check("--dry-run prints a plan without a server", "step 0" in plan and "disk-group" in plan,
              plan[:120])
        walks = os.path.join(out_dir, "walks")
        info = run_walk(base, walks, keep=True)
        work = work_dir(info)
        raw, san = os.path.join(work, "raw"), os.path.join(work, "sanitized")

        summary = load(raw, "summary.json")
        check("about.apiVersion 9.1.1.0",
              summary["about"]["about"].get("apiVersion") == "9.1.1.0",
              summary["about"]["about"].get("apiVersion"))
        check("read-only privileges confirmed",
              summary["readonly_privileges"] == ["System.Anonymous", "System.Read", "System.View"],
              summary["readonly_privileges"])
        other_dir = os.path.join(raw, "other")
        other = set(os.listdir(other_dir)) if os.path.isdir(other_dir) else set()
        wanted_other = {"pbm.RetrieveServiceContent.json", "sms.QueryStorageManager.json",
                        "vslm.RetrieveContent.json", "eam.QueryAgency.json"}
        check("every other SOAP endpoint is called and leaves a record, answer or fault",
              wanted_other <= other, sorted(wanted_other - other))
        check("sms, vslm and eam answered with a version through the vCenter host name",
              all("version" in summary["endpoints"].get(e, {}) for e in ("sms", "vslm", "eam")),
              summary["endpoints"])
        counts = summary["counts"]
        check("inventory: one cluster, four hosts, four VMs, five datastores",
              counts.get("ClusterComputeResource") == 1 and counts.get("HostSystem") == 4
              and counts.get("VirtualMachine") == 4 and counts.get("Datastore") == 5, counts)
        inv = load(raw, "inv", "HostSystem.json")
        check("inv/HostSystem.json: one record per host with props",
              len(inv) == 4 and all(isinstance(r, dict) and r.get("props") for r in inv),
              [list(r) for r in inv[:1]])
        check("every host reports a vsanHostConfig with vsanEsaEnabled",
              all(r["props"].get("config", {}).get("vsanHostConfig", {}).get("vsanEsaEnabled")
                  is True for r in inv),
              [r["props"].get("config", {}).get("vsanHostConfig") for r in inv][:1])
        hosts = [r["props"]["name"] for r in inv]

        families = {}
        for fname in sorted(os.listdir(os.path.join(raw, "vsan"))):
            if fname.startswith("VsanPerfQueryPerf-"):
                families[fname[len("VsanPerfQueryPerf-"):-len(".json")]] = load(raw, "vsan", fname)
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

        bare = [os.path.relpath(f, raw) for f in tree_files(raw) if not f.endswith(".json")]
        check("every raw record is a .json file", not bare, bare[:3])
        unbacked = []
        for call in summary["calls"]:
            if call["ok"]:
                continue
            record = load(raw, *(call["name"] + ".json").split("/"))
            if not call.get("fault") or record.get("_fault", {}).get("fault") != call["fault"]:
                unbacked.append(call["name"])
        check("every faulted call is backed by a _fault record of the same name",
              any(not c["ok"] for c in summary["calls"]) and not unbacked, unbacked[:3])
        check("the OSA disk mapping call is recorded (empty on ESA)",
              any(x["name"].startswith("vsan/QueryDiskMappings-") for x in summary["calls"]))
        check("unsupported methods are recorded as MethodNotFound, not skipped",
              any(x.get("fault", "").endswith("MethodNotFound") for x in summary["calls"]))

        shape_diffs, shape_seen = [], set()
        for f in tree_files(raw):
            rel = os.path.relpath(f, raw)
            want = [(k, v) for k, v in pairs(load(f)) if k in ("_type", "_moref")]
            got = [(k, v) for k, v in pairs(load(san, rel)) if k in ("_type", "_moref")]
            shape_seen.update(v for _k, v in want)
            if got != want:
                shape_diffs.append(rel)
        check("sanitized: managed-object types and morefs are untouched in every record",
              "VirtualMachineConfigInfo" in shape_seen and not shape_diffs, shape_diffs[:3])
        raw_about, san_about = summary["about"]["about"], load(san, "about.json")["about"]
        check("sanitized: about keeps the version strings",
              all(raw_about.get(k) and san_about.get(k) == raw_about[k]
                  for k in ("apiVersion", "version", "fullName")),
              {k: (raw_about.get(k), san_about.get(k)) for k in ("apiVersion", "version", "fullName")})
        http_dir = os.listdir(os.path.join(raw, "http"))
        check("plain GETs with and without a trailing slash keep separate records",
              "sdk.json" in http_dir and "sdk_.json" in http_dir, sorted(http_dir)[:6])
        san_hosts = load(san, "inv", "HostSystem.json")
        check("sanitized: host names masked",
              san_hosts[0]["props"]["name"] != hosts[0] and san_hosts[0]["props"]["name"].startswith("esx"),
              (san_hosts[0]["props"]["name"], hosts[0]))

        raw_secret = unmasked = 0
        for f in tree_files(raw):
            raw_secret += sum(1 for k, v in pairs(load(f)) if k in MASKED_KEYS and len(v) > 3)
        for f in tree_files(san):
            unmasked += sum(1 for k, v in pairs(load(f))
                            if k in MASKED_KEYS and len(v) > 3 and not v.startswith("<"))
        check("sanitized: serial numbers, models and vendors masked in every record",
              raw_secret > 0 and unmasked == 0, (raw_secret, unmasked))
        blob = "".join(open(f, encoding="utf-8").read() for f in tree_files(san))
        check("sanitized: password, login user and real host names absent",
              PASSWORD not in blob and USER not in blob and not any(h in blob for h in hosts),
              [t for t in (PASSWORD, USER, *hosts) if t in blob])
        raw_uuid = summary["about"]["about"].get("instanceUuid")
        san_uuid = load(san, "about.json")["about"].get("instanceUuid")
        check("sanitized: a uuid is remapped, not dropped",
              bool(raw_uuid) and san_uuid != raw_uuid and bool(UUID_RE.match(str(san_uuid))),
              (raw_uuid, san_uuid))
        check("sanitized: no salt is shipped",
              "salt" not in load(san, "SANITIZE-INFO.json"))

        tar_files = tarball_files(info["tarball"])
        check("tarball holds the sanitized tree only",
              tar_files and not any("/raw/" in n or n.endswith("/raw") for n in tar_files)
              and any(n.endswith("/summary.json") for n in tar_files), sorted(tar_files)[:3])
        tar_blob = "".join(tar_files.values())
        check("tarball carries no real host name, login user or password",
              not any(t in tar_blob for t in (PASSWORD, USER, *hosts)))

        names1 = [c["name"] for c in summary["calls"]]
        info2 = run_walk(base, walks, keep=True)
        summary2 = load(work_dir(info2), "raw", "summary.json")
        check("a second walk reproduces the call set",
              [c["name"] for c in summary2["calls"]] == names1,
              (len(names1), len(summary2["calls"])))
        check("a second walk reproduces the per-type counts",
              summary2["counts"] == summary["counts"], (summary2["counts"], summary["counts"]))
        check("each walk gets its own tarball and work directory",
              len(os.listdir(walks)) == 4 and info["tarball"] != info2["tarball"],
              sorted(os.listdir(walks)))

        plain = os.path.join(out_dir, "plain")
        info3 = run_walk(base, plain, keep=False)
        check("a run without --zip leaves only the tarball",
              os.listdir(plain) == [os.path.basename(info3["tarball"])], os.listdir(plain))
        check("a run without --zip still reports the walk",
              info3["calls"] == len(summary["calls"]), (info3["calls"], len(summary["calls"])))
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
