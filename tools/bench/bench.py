"""Replay a sanitized trace from the pinned vSAN Host Metrics collector.

The fixture records the collector against the built-in four-host topology.
For each simulated cluster, this replay substitutes inventory identities and
expands recorded host and VM calls to cover its cardinality. It opens a session
per cluster, so aggregate session and discovery traffic differs from one real
collector run over multiple clusters. It records client timings and the server
query log, and exports a report, the scenario and per-call CSVs.

Run under a memory limit, for example:
 systemd-run --user --scope -p MemoryMax=4G -- python3 tools/bench/bench.py examples/scenarios/scale-10.yaml --runs 1 --out /tmp/vcs-bench
"""
from __future__ import annotations

import argparse
import html
import json
import os
import ssl
import shutil
import xml.etree.ElementTree as ET
import subprocess
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))

# The reference pack's pinned source (sentania-labs/vsanhostmetrics).
MP_SHA = "af976fb9fe97bbac2fd94b666a3f79aa12656f2a"

# Buckets a 15-minute window yields on the live service (recon section 5.3).
BUCKETS_15MIN = 4

# The simulator serves a self-signed certificate; the harness does not verify.
CTX = ssl._create_unverified_context()

ENVELOPE = ('<?xml version="1.0" encoding="UTF-8"?>'
            '<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" '
            'xmlns:xsd="http://www.w3.org/2001/XMLSchema" '
            'xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">')


def body(method, this_type, this_value, *chunks):
    """One SOAP request: `method` with `_this` and then the given element chunks.
    Namespace is urn:vim25 for the ServiceInstance/PropertyCollector objects and
    urn:vsan for the vSAN ones, as the captured requests show."""
    ns = "urn:vim25" if this_type in ("ServiceInstance", "PropertyCollector", "ServiceContent") else "urn:vsan"
    parts = [ENVELOPE, f'<soapenv:Body><{method} xmlns="{ns}">',
             f'<_this type="{this_type}">{this_value}</_this>']
    parts.extend(chunks)
    parts.append(f"</{method}></soapenv:Body></soapenv:Envelope>")
    return "".join(parts)


def sdk_login(user, pw):
    return (ENVELOPE + '<soapenv:Body><Login xmlns="urn:vim25">'
            '<_this type="SessionManager">SessionManager</_this>'
            f"<userName>{html.escape(user)}</userName><password>{html.escape(pw)}</password>"
            "</Login></soapenv:Body></soapenv:Envelope>")


def sdk_service_content():
    return body("RetrieveServiceContent", "ServiceInstance", "ServiceInstance")


def sdk_props(typ, paths, obj_type, obj_value):
    """One RetrieveProperties. The element order matches the captured request
    fixture: `specSet`, then `propSet` (with `type`, `all`, `pathSet`), then
    `objectSet` (with `obj`, `skip`)."""
    spec = ("<specSet><propSet>" + f"<type>{typ}</type><all>false</all>"
            + "".join(f"<pathSet>{p}</pathSet>" for p in paths)
            + f"</propSet><objectSet><obj type=\"{obj_type}\">{obj_value}</obj>"
            "<skip>true</skip><selectSet xsi:type='TraversalSpec'><name>view</name><type>ContainerView</type><path>view</path><skip>false</skip></selectSet></objectSet></specSet>")
    return body("RetrieveProperties", "PropertyCollector", "propertyCollector", spec)


def sdk_container_view(typ):
    return (ENVELOPE + '<soapenv:Body><CreateContainerView xmlns="urn:vim25">'
            '<_this type="ViewManager">ViewManager</_this>'
            '<container type="Folder">group-d1</container>'
            f"<type>{typ}</type><recursive>true</recursive>"
            "</CreateContainerView></soapenv:Body></soapenv:Envelope>")


def sdk_logout():
    return (ENVELOPE + '<soapenv:Body><Logout xmlns="urn:vim25">'
            '<_this type="SessionManager">SessionManager</_this>'
            "</Logout></soapenv:Body></soapenv:Envelope>")


def vsan_node_info(cluster):
    return body("VsanPerfQueryNodeInformation", "VsanPerformanceManager",
                "vsan-performance-manager",
                f'<cluster type="ClusterComputeResource">{cluster}</cluster>')


def vsan_entity_types():
    return body("VsanPerfGetSupportedEntityTypes", "VsanPerformanceManager",
                "vsan-performance-manager")


def vsan_config(cluster):
    return body("VsanClusterGetConfig", "VsanVcClusterConfigSystem",
                "vsan-cluster-config-system",
                f'<cluster type="ClusterComputeResource">{cluster}</cluster>')


def vsan_health(cluster):
    return body("VsanQueryVcClusterHealthSummary", "VsanVcClusterHealthSystem",
                "vsan-cluster-health-system",
                f'<cluster type="ClusterComputeResource">{cluster}</cluster>'
                "<includeObjUuids>false</includeObjUuids><fields>hclInfo</fields>")


def vsan_disks(host):
    return body("QueryVsanManagedDisks", "VimClusterVsanVcDiskManagementSystem",
                "vsan-disk-management-system",
                f'<host type="HostSystem">{host}</host>')


def vsan_perf(cluster, ref, start, end):
    return body("VsanPerfQueryPerf", "VsanPerformanceManager", "vsan-performance-manager",
                f"<querySpecs><entityRefId>{ref}</entityRefId><startTime>{start}</startTime>"
                f"<endTime>{end}</endTime></querySpecs>",
                f'<cluster type="ClusterComputeResource">{cluster}</cluster>')


# Property paths per container view, as the inventory capture shows them.
# Property paths per object kind, as the trimmed inventory capture shows them
# (fixtures/live/9.1.1/inventory). One container view per kind, as govc and the
# native vCenter adapter do.
VIEW_PATHS = {
    "datacenters": ("Datacenter", ["name", "overallStatus"]),
    "clusters": ("ClusterComputeResource",
                 ["name", "parent", "summary.numEntries", "configurationEx", "overallStatus"]),
    "hosts": ("HostSystem",
              ["name", "parent", "runtime", "summary", "config", "hardware"]),
    "vms": ("VirtualMachine",
            ["name", "parent", "runtime", "summary", "guest"]),
    "datastores": ("Datastore", ["name", "parent", "summary", "info"]),
    # the folder's children, so the datacenter count comes from /sdk itself
    "root": ("Folder", ["childEntity.name"]),
}


def call(base, path, payload, cookie=None):
    req = urllib.request.Request(base + path, data=payload.encode(), method="POST")
    req.add_header("Content-Type", "text/xml")
    if cookie:
        req.add_header("Cookie", cookie)
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=120, context=CTX) as resp:
        data = resp.read()
        set_cookie = resp.headers.get("Set-Cookie") or cookie
    root = ET.fromstring(data)
    fault = root.find(".//{http://schemas.xmlsoap.org/soap/envelope/}Fault")
    if fault is not None:
        raise RuntimeError("SOAP fault: " + ET.tostring(fault, encoding="unicode")[:1000])
    return time.perf_counter() - t0, len(data), data, set_cookie


def tag_value(text, tag):
    """The text of the first element named `tag`."""
    i = text.find("<" + tag)
    if i < 0:
        return None
    j = text.find(">", i)
    k = text.find("<", j + 1)
    return text[j + 1:k] if k > 0 else None


def moref_values(text, kind):
    """Every `<... type="kind">VALUE<` in a properties response, in order."""
    root = ET.fromstring(text)
    return [el.text for el in root.iter() if el.tag.split("}")[-1] == "obj"
            and el.get("type") == kind and el.text]


def start_sim(port, scenario, data_dir):
    data_dir = os.path.abspath(data_dir)
    exe = os.path.join(data_dir, "vcsimulator")
    subprocess.run(["go", "build", "-o", exe, os.path.join(ROOT, "cmd/vcsimulator")],
                   cwd=ROOT, check=True)
    log = os.path.join(data_dir, "sim.log")
    proc = subprocess.Popen(
        [exe, "-query-log", os.path.join(data_dir, "queries.jsonl"), "-scenario", os.path.abspath(scenario), "-data", data_dir,
         "-listen", f"127.0.0.1:{port}"],
        stdout=open(log, "w"), stderr=subprocess.STDOUT)
    described = None
    for _ in range(4000):
        try:
            txt = open(log).read()
        except OSError:
            txt = ""
        for line in txt.splitlines():
            if line.startswith("VCSIM_JSON "):
                described = json.loads(line[len("VCSIM_JSON "):])
        if described:
            return proc, described
        if proc.poll() is not None:
            raise SystemExit("simulator exited: " + txt)
        time.sleep(0.05)
    proc.kill()
    proc.wait()
    raise SystemExit("simulator did not report its topology")


def one_pass(described, base, user, pw):
    """Replay the recorded collector trace, substituting simulator identities."""
    import re
    import urllib.error
    from datetime import datetime, timezone, timedelta
    fixture = json.load(open(os.path.join(HERE, "fixtures", "collector-v2.0.0-rc.2.json")))
    calls = []
    # Discover identities using the simulator inventory. Discovery calls count
    # toward the pass wall time but not toward the replay call count.
    _, _, _, cookie = call(base, "/sdk", sdk_login(user, pw))
    refs = {}
    for typ in ("ClusterComputeResource", "HostSystem", "VirtualMachine"):
        _, _, data, _ = call(base, "/sdk", sdk_container_view(typ), cookie)
        view = tag_value(data.decode(), "returnval")
        _, _, data, _ = call(base, "/sdk", sdk_props(typ, ["name"], "ContainerView", view), cookie)
        refs[typ] = moref_values(data.decode(), typ)
    call(base, "/sdk", sdk_logout(), cookie)
    clusters, hosts, vms = refs.values()
    if len(clusters) != len(described["perCluster"]):
        raise RuntimeError("inventory does not match topology")
    hpos = vpos = 0
    for ci, desc in enumerate(described["perCluster"]):
        hs = hosts[hpos:hpos + desc["hosts"]]; hpos += desc["hosts"]
        vs = vms[vpos:vpos + desc["vms"]]; vpos += desc["vms"]
        if len(hs) < 4 or len(vs) < 4:
            raise RuntimeError("trace requires at least four hosts and VMs per cluster")
        views = {}; next_view = 0; cookie = None
        for item in fixture["requests"]:
            op, path, template = item["operation"], item["path"], item["request"]
            match = re.search(r"{{(hostVsan|host|vm)([0-3])}}", template)
            if match:
                kind, index = match.group(1), int(match.group(2))
                choices = (vs if kind == "vm" else hs)[index::4]
            else:
                choices = [None]
            for choice in choices:
                payload = template.replace("{{cluster}}", clusters[ci])
                if match:
                    replacement = "vsanSystem-" + choice if kind == "hostVsan" else choice
                    payload = payload.replace(match.group(0), replacement)
                for key, value in views.items(): payload = payload.replace("{{" + key + "}}", value)
                now = datetime.now(timezone.utc)
                # The first time is start and the second is end in a query.
                payload = payload.replace("{{time}}", (now-timedelta(minutes=15)).isoformat(), 1)
                payload = payload.replace("{{time}}", now.isoformat())
                if op == "Login": payload = sdk_login(user, pw)
                t0 = time.perf_counter()
                if item["method"] == "GET":
                    req = urllib.request.Request(base + path)
                else:
                    req = urllib.request.Request(base + path, data=payload.encode(), method="POST", headers={"Content-Type":"text/xml"})
                if cookie: req.add_header("Cookie", cookie)
                try:
                    resp = urllib.request.urlopen(req, timeout=120, context=CTX)
                except urllib.error.HTTPError as exc:
                    resp = exc
                with resp:
                    data = resp.read(); status = resp.status
                    cookie = resp.headers.get("Set-Cookie") or cookie
                elapsed = time.perf_counter() - t0
                if status != item["status"] and op in ("Login", "CreateContainerView"):
                    raise RuntimeError(f"{op} status {status}, expected {item['status']}: {data[:300]!r}")
                if op == "CreateContainerView":
                    view = tag_value(data.decode(), "returnval")
                    if not view: raise RuntimeError("missing view")
                    views["view" + str(next_view)] = view; next_view += 1
                calls.append((op or "GET", path, elapsed, len(data), data.count(b"<entityRefId"), status, item["status"]))
    return calls


def pct(values, p):
    if not values:
        return 0.0
    s = sorted(values)
    k = max(0, min(len(s) - 1, int(round(p / 100 * (len(s) - 1)))))
    return s[k]


def rss_kb(pid):
    try:
        for line in open(f"/proc/{pid}/status"):
            if line.startswith("VmHWM:"):
                return int(line.split()[1])
    except OSError:
        pass
    return 0


def mem_total_mb():
    try:
        for line in open("/proc/meminfo"):
            if line.startswith("MemTotal:"):
                return int(line.split()[1]) // 1024
    except OSError:
        pass
    return 0


def memory_cap_mb():
    try:
        with open("/proc/self/cgroup") as f:
            path = next(line.strip().split(":", 2)[2] for line in f if line.startswith("0::"))
        with open(os.path.join("/sys/fs/cgroup", path.lstrip("/"), "memory.max")) as f:
            value = f.read().strip()
        return None if value == "max" else int(value) // (1024 * 1024)
    except (OSError, StopIteration, ValueError):
        return None

def git_commit():
    try:
        head = subprocess.run(["git", "-C", ROOT, "rev-parse", "--short", "HEAD"],
                              capture_output=True, text=True, timeout=10, check=True).stdout.strip()
        dirty = subprocess.run(["git", "-C", ROOT, "status", "--porcelain"],
                               capture_output=True, text=True, timeout=10, check=True).stdout
        return head + ("-dirty" if dirty else "")
    except Exception:
        return "unknown"


def run(args):
    if os.path.isdir(args.out) and os.listdir(args.out):
        raise SystemExit(f"--out {args.out} exists and is not empty; choose a new or empty directory")
    os.umask(0o077)
    os.makedirs(args.out, exist_ok=True)
    data_dir = os.path.join(args.out, "data")
    os.makedirs(data_dir)
    proc, described = start_sim(args.port, args.scenario, data_dir)
    base = f"https://127.0.0.1:{args.port}"
    runs = []
    try:
        for i in range(args.runs):
            t0 = time.perf_counter()
            calls = one_pass(described, base, args.username, args.password)
            wall = time.perf_counter() - t0
            lat = [c[2] for c in calls]
            runs.append({
                "run": i + 1,
                "wallSeconds": round(wall, 3),
                "calls": len(calls),
                "requestsPerSecond": round(len(calls) / wall, 1) if wall else 0.0,
                "latencyMs": {"p50": round(pct(lat, 50) * 1e3, 3),
                              "p95": round(pct(lat, 95) * 1e3, 3),
                              "p99": round(pct(lat, 99) * 1e3, 3),
                              "max": round(max(lat) * 1e3, 3)},
                "responseBytesTotal": sum(c[3] for c in calls),
                "entitiesReturned": sum(c[4] for c in calls),
                "unexpectedStatuses": sum(c[5] != c[6] for c in calls),
                "peakRssKb": rss_kb(proc.pid),
            })
            with open(os.path.join(args.out, f"calls-run{i + 1}.csv"), "w") as fh:
                fh.write("call,path,latencyMs,responseBytes,entitiesReturned,status,expectedStatus\n")
                for name, path, t, n, e, status, expected in calls:
                    fh.write(f"{name},{path},{t * 1e3:.3f},{n},{e},{status},{expected}\n")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
    summary = {
        "host": {"cpus": os.cpu_count(), "memoryMb": mem_total_mb(), "commandCapMb": memory_cap_mb(),
                 "capNote": "cgroup memory.max; null means no finite limit detected"},
        "simulator": {"binary": "cmd/vcsimulator", "scenario": args.scenario,
                      "seed": described["seed"], "sourceMpSha": MP_SHA,
                      "build": git_commit()},
        "topology": described,
        "projection": {
            "advertisedFamilies": len(described["projection"]["families"]),
            "dataFamilies": sum(f["behaviour"] == "data" for f in described["projection"]["families"]),
            "totalEntities": described["projection"]["all"]["entities"],
            "totalMetricSeries": described["projection"]["all"]["series"],
            "samplesPerPassIn15MinWindow": described["projection"]["all"]["series"] * BUCKETS_15MIN,
            "perCluster": described["projection"]["perCluster"],
            "note": "Projected from scenario topology and the captured metric catalog; not measured ingestion.",
        },
        "runs": runs,
    }
    with open(os.path.join(args.out, "benchmark.json"), "w") as fh:
        json.dump(summary, fh, indent=1)
    txt = render(summary)
    with open(os.path.join(args.out, "benchmark.md"), "w") as fh:
        fh.write(txt + "\n")
    print(txt)
    shutil.copyfile(os.path.join(data_dir, "queries.jsonl"), os.path.join(args.out, "queries.jsonl"))
    shutil.copyfile(args.scenario, os.path.join(args.out, "scenario.yaml"))
    if not args.keep:
        shutil.rmtree(data_dir)


def render(s):
    t = s["topology"]
    p = s["projection"]
    out = [f"# Benchmark: {t['seed']}", ""]
    out.append(f"- topology: {t['clusters']} clusters, {t['hosts']} hosts, {t['vms']} VMs, "
               f"{t['devices']} devices")
    out.append(f"- projected across the scenario: {p['advertisedFamilies']} advertised families "
               f"({p['dataFamilies']} with data), {p['totalEntities']} entities, "
               f"{p['totalMetricSeries']} metric series; {p['samplesPerPassIn15MinWindow']} "
               "samples for four theoretical buckets across the scenario")
    out.append(f"- host: {s['host']['cpus']} CPUs, {s['host']['memoryMb']} MB total; "
               f"cgroup memory cap {s['host']['commandCapMb']} MB")
    out.append(f"- simulator build: {s['simulator']['build']}, "
               f"source pack {s['simulator']['sourceMpSha'][:12]}")
    out.append("")
    out.append("| run | wall s | calls | req/s | p50 ms | p95 ms | p99 ms | max ms | response MB | entities | unexpected statuses | peak RSS MB |")
    out.append("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
    for r in s["runs"]:
        lm = r["latencyMs"]
        out.append(f"| {r['run']} | {r['wallSeconds']} | {r['calls']} | {r['requestsPerSecond']} | "
                   f"{lm['p50']} | {lm['p95']} | {lm['p99']} | {lm['max']} | "
                   f"{r['responseBytesTotal'] / 1e6:.1f} | {r['entitiesReturned']} | {r['unexpectedStatuses']} | "
                   f"{r['peakRssKb'] / 1024:.0f} |")
    out.append("")
    out.append(f"Source management pack: sentania-labs/vsanhostmetrics @ {s['simulator']['sourceMpSha']}")
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description="Replay and benchmark the vCenter API Simulator")
    ap.add_argument("scenario")
    ap.add_argument("--port", default="18443")
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--out", default="bench-out")
    ap.add_argument("--keep", action="store_true", help="keep the data directory")
    ap.add_argument("--username", default="administrator@vsphere.local")
    ap.add_argument("--password", default="VMware1!")
    args = ap.parse_args()
    if args.runs < 1:
        ap.error("--runs must be positive")
    run(args)


if __name__ == "__main__":
    main()
