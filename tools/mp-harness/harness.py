#!/usr/bin/env python3
"""Run the reference management pack's own collection functions against the
simulator and judge the result the way recon section 9 test 4 defines it.

    python3 tools/mp-harness/harness.py --sim HOST:PORT --mp /path/to/vsanhostmetrics \
        --fixtures fixtures/live/9.1.1 [--ca cert.pem] [--report out.json]

Uses the pack's app/perfsvc.py unmodified (pyVmomi 8.0.3.0.1 plus the pack's
vendored vSAN bindings), so the SOAP it sends is what the pack sends. Passing
means, for both metric scopes:

  * perfsvc.connect, probe and collect succeed with zero problems
  * every object name resolves (no bare UUID), every object has a vCenter
    parent, every claimed disk has hardware detail
  * the set of gauges collected per entity family equals the label set the
    live service returned (fixtures), minus the "<x>Actual" twins the pack
    drops on purpose
  * object counts match the simulator's topology (4 hosts, 4 VMs, ...)
  * a second collection produces the same object keys
  * with --ca, connecting with verification on and the simulator's own
    certificate as the accepted PEM works, as it does in Operations

HTTP 200 is not success here; parsed objects, identifiers and labels are.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time

FAMILY_RE = re.compile(r"^perf-domain-c6004-(.+)\.resp\.xml$")
LABEL_RE = re.compile(r"<label>([^<]*)</label>")
REF_RE = re.compile(r"<entityRefId[^>]*>([^<]*)</entityRefId>")


def fixture_labels(fixtures: str) -> dict[str, list[str]]:
    """family -> ordered labels the live service returned (data families only)."""
    out = {}
    for fn in sorted(os.listdir(fixtures)):
        m = FAMILY_RE.match(fn)
        if not m:
            continue
        text = open(os.path.join(fixtures, fn), encoding="utf-8").read()
        refs = [r for r in REF_RE.findall(text) if "*" not in r]
        if not refs:
            continue
        seen, labels = set(), []
        for lab in LABEL_RE.findall(text):
            if lab not in seen:
                seen.add(lab)
                labels.append(lab)
        out[m.group(1)] = labels
    return out


def expected_gauges(labels: list[str], keep: set[str]) -> set[str]:
    """What perfsvc.collect stores for one entity: every label the service
    returned except an "<x>Actual" twin whose base is in `keep`, where keep is
    the pack's modelled metrics for the family narrowed by the scope. The scope
    never filters gauges; only the entity types queried (perfsvc.collect)."""
    return {l for l in labels if not (l.endswith("Actual") and l[:-6] in keep)}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--sim", required=True, help="simulator host:port")
    ap.add_argument("--mp", required=True, help="vsanhostmetrics checkout (pinned commit)")
    ap.add_argument("--fixtures", required=True)
    ap.add_argument("--user", default="user")
    ap.add_argument("--password", default="pass")
    ap.add_argument("--ca", help="simulator certificate PEM; also runs the verify-on path with it as the accepted certificate")
    ap.add_argument("--report", help="write a JSON report here")
    args = ap.parse_args()

    sys.path.insert(0, os.path.join(args.mp, "app"))
    import perfsvc  # noqa: E402  (the pack's module, unmodified)
    import metric_scope  # noqa: E402

    report: dict = {"sim": args.sim, "mp": args.mp, "checks": [], "failures": []}

    def check(name: str, ok: bool, detail=""):
        report["checks"].append({"name": name, "ok": bool(ok), "detail": str(detail)[:400]})
        if not ok:
            report["failures"].append(f"{name}: {detail}")
        print(("PASS " if ok else "FAIL ") + name + (f": {detail}" if detail and not ok else ""))

    host, port = perfsvc.split_host(args.sim)
    expected = fixture_labels(args.fixtures)
    check("fixtures: data families parsed", len(expected) >= 40, len(expected))

    t0 = time.time()
    si, clusters, mos = perfsvc.connect(host, args.user, args.password, verify=False, port=port)
    check("connect (verify off)", True)
    content = si.RetrieveContent()
    check("about.apiVersion 9.1.1.0", content.about.apiVersion == "9.1.1.0", content.about.apiVersion)
    check("about.instanceUuid present", bool(content.about.instanceUuid))
    check("clusters found", len(clusters) == 1, len(clusters))
    cluster = clusters[0]
    check("vsan_enabled", perfsvc.vsan_enabled(cluster) is True, perfsvc.vsan_enabled(cluster))
    perf = mos["vsan-performance-manager"]
    hosts = list(cluster.host)
    vms_expected = None

    n, problem = perfsvc.probe(perf, cluster)
    check("probe returns one cluster-domclient object", n == 1 and problem is None, (n, problem))

    ent_for_kind = {spec["kind"]: ent for ent, spec in perfsvc.MODEL.ENTITIES.items()}
    dashboard = {ent_for_kind[k]: set(m) for k, m in metric_scope.COVERED.items() if k in ent_for_kind}

    names = perfsvc.build_name_map(si, clusters, perf, mos)
    parents = perfsvc.build_parent_map(si, clusters, perf, mos)
    disk_hw = perfsvc.build_disk_hardware(si, clusters, mos.get("vsan-cluster-health-system"))
    check("parent map: vcid", bool(parents.get("vcid")))
    check("parent map: every host joined by node uuid", len(parents["hosts"]) == len(hosts), (len(parents["hosts"]), len(hosts)))
    check("parent map: cluster joined by vsan uuid", len(parents["clusters"]) == 1, parents["clusters"])
    check("parent map: disks joined to hosts", len(parents["disks"]) == len(hosts), len(parents["disks"]))
    check("disk hardware for every claimed disk", set(disk_hw) == set(parents["disks"]), (len(disk_hw), len(parents["disks"])))
    check("disk hardware has firmware/model/vendor/driver/driver_version",
          all({"firmware", "model", "vendor", "driver", "driver_version"} <= set(v) for v in disk_hw.values()),
          {k: sorted(v) for k, v in list(disk_hw.items())[:1]})

    results = {}
    for scope_name, scope in (("all", None), ("dashboard", dashboard)):
        objs, problems = perfsvc.collect(perf, cluster, scope=scope)
        check(f"collect[{scope_name}]: zero problems", not problems, problems[:3])
        per_family: dict[str, list] = {}
        for key, g in objs.items():
            per_family.setdefault(key.entity, []).append((key, g))
        queried = set(perfsvc.MODEL.ENTITIES if scope is None else scope)
        wanted = {f for f in expected if f in queried}
        missing = sorted(wanted - set(per_family))
        extra = sorted(set(per_family) - wanted)
        check(f"collect[{scope_name}]: every data family returned objects", not missing, missing)
        check(f"collect[{scope_name}]: no family the live service lacked", not extra, extra)
        bad_labels = []
        for fam, items in per_family.items():
            keep = set(perfsvc.MODEL.ENTITIES[fam]["metrics"])
            if scope is not None:
                keep &= scope.get(fam, set())
            want = expected_gauges(expected.get(fam, []), keep)
            for key, g in items:
                got = set(g.gauges)
                if got != want:
                    bad_labels.append((fam, sorted(want - got)[:5], sorted(got - want)[:5]))
                    break
        check(f"collect[{scope_name}]: gauge label set per family equals the live set", not bad_labels, bad_labels[:3])
        unresolved = [key.display(names) for key in objs if any(
            re.fullmatch(r"[0-9a-f-]{36}", v) for v in key.display(names).split(" ")[:1])]
        check(f"collect[{scope_name}]: every object name resolved", not unresolved, unresolved[:3])
        orphans = []
        for key in objs:
            idents = dict(key.idents)
            found = False
            for bucket, ident in (("hosts", "host_uuid"), ("vms", "vm_uuid"), ("disks", "disk_uuid"), ("vdisks", "vdisk_uuid"), ("clusters", "cluster_uuid")):
                v = idents.get(ident)
                if v and v in parents.get(bucket, {}):
                    found = True
                    break
            if not found:
                orphans.append(key.display(names))
        check(f"collect[{scope_name}]: every object has a vCenter parent", not orphans, orphans[:3])
        counts = {f: len(v) for f, v in per_family.items()}
        results[scope_name] = {"objects": len(objs), "families": len(per_family), "counts": counts}
        print(f"  {scope_name}: {len(objs)} objects in {len(per_family)} families")

    counts = results["all"]["counts"]
    nh = len(hosts)
    view = content.viewManager.CreateContainerView(content.rootFolder, [perfsvc.vim.VirtualMachine], True)
    vms_expected = len(list(view.view))
    view.DestroyView()
    check("cardinality: host families = hosts", counts.get("host-domclient") == nh and counts.get("vsan-cpu") == nh, counts.get("host-domclient"))
    check("cardinality: cluster families = 1", counts.get("cluster-domclient") == 1)
    check("cardinality: virtual-machine = VMs in inventory", counts.get("virtual-machine") == vms_expected, (counts.get("virtual-machine"), vms_expected))
    check("cardinality: vsan-pnic-net = hosts x 2 uplinks", counts.get("vsan-pnic-net") == nh * 2, counts.get("vsan-pnic-net"))
    check("cardinality: host-cpu = hosts x (threads + aggregate)", counts.get("host-cpu") == nh * 17, counts.get("host-cpu"))
    check("cardinality: vsan-esa-disk-layer = claimed disks", counts.get("vsan-esa-disk-layer") == len(parents["disks"]), counts.get("vsan-esa-disk-layer"))

    # Second collection: same keys.
    objs2, problems2 = perfsvc.collect(perf, cluster, scope=None)
    objs1, _ = perfsvc.collect(perf, cluster, scope=None)
    check("second collect: same object keys", set(objs1) == set(objs2) and not problems2, (len(objs1), len(objs2)))
    report["timing_s"] = round(time.time() - t0, 2)
    report["results"] = results
    perfsvc.Disconnect(si)

    # After logout, /vsanHealth must refuse the session (post-logout fixture).
    try:
        perf.VsanPerfGetSupportedEntityTypes()
        check("post-logout /vsanHealth refused", False, "call succeeded")
    except Exception as exc:  # noqa: BLE001
        check("post-logout /vsanHealth refused", "NotAuthenticated" in type(exc).__name__, type(exc).__name__)

    if args.ca:
        pem = open(args.ca).read()
        si2, clusters2, _ = perfsvc.connect(host, args.user, args.password, verify=True, accepted_pems=[pem], port=port)
        check("connect (verify on, accepted PEM pinned)", len(clusters2) == 1)
        perfsvc.Disconnect(si2)

    if args.report:
        with open(args.report, "w") as fh:
            json.dump(report, fh, indent=1)
    print(f"{len(report['checks']) - len(report['failures'])}/{len(report['checks'])} checks passed in {report['timing_s']}s")
    return 1 if report["failures"] else 0


if __name__ == "__main__":
    sys.exit(main())
