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
  * object counts match the simulator's topology (hosts, VMs, uplinks,
    threads and claimed disks per cluster, read from /sdk)
  * a second collection produces the same object keys

Every vSAN cluster the simulator serves is collected in turn (Phase 2
scenarios have many); the checks above run per cluster.
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
    check("clusters found", len(clusters) >= 1, len(clusters))
    perf = mos["vsan-performance-manager"]

    ent_for_kind = {spec["kind"]: ent for ent, spec in perfsvc.MODEL.ENTITIES.items()}
    dashboard = {ent_for_kind[k]: set(m) for k, m in metric_scope.COVERED.items() if k in ent_for_kind}

    names = perfsvc.build_name_map(si, clusters, perf, mos)
    parents = perfsvc.build_parent_map(si, clusters, perf, mos)
    disk_hw = perfsvc.build_disk_hardware(si, clusters, mos.get("vsan-cluster-health-system"))
    all_hosts = [h for c in clusters for h in c.host]
    check("parent map: vcid", bool(parents.get("vcid")))
    check("parent map: every host joined by node uuid", len(parents["hosts"]) == len(all_hosts), (len(parents["hosts"]), len(all_hosts)))
    check("parent map: every cluster joined by vsan uuid", len(parents["clusters"]) == len(clusters), (len(parents["clusters"]), len(clusters)))
    check("disk hardware for every claimed disk", set(disk_hw) == set(parents["disks"]), (len(disk_hw), len(parents["disks"])))
    check("disk hardware has firmware/model/vendor/driver/driver_version",
          all({"firmware", "model", "vendor", "driver", "driver_version"} <= set(v) for v in disk_hw.values()),
          {k: sorted(v) for k, v in list(disk_hw.items())[:1]})

    report["clusters"] = []
    for cluster in clusters:
        cname = cluster.name
        hosts = list(cluster.host)
        check(f"{cname}: vsan_enabled", perfsvc.vsan_enabled(cluster) is True, perfsvc.vsan_enabled(cluster))
        n, problem = perfsvc.probe(perf, cluster)
        check(f"{cname}: probe returns one cluster-domclient object", n == 1 and problem is None, (n, problem))
        claimed = 0
        uplinks = 0
        threads = 0
        for h in hosts:
            threads += int(h.hardware.cpuInfo.numCpuThreads)
            for p in h.config.network.proxySwitch:
                uplinks += len(p.pnic)
            for d in h.configManager.vsanSystem.QueryDisksForVsan():
                if d.vsanUuid:
                    claimed += 1
        results = {}
        for scope_name, scope in (("all", None), ("dashboard", dashboard)):
            objs, problems = perfsvc.collect(perf, cluster, scope=scope)
            check(f"{cname}: collect[{scope_name}]: zero problems", not problems, problems[:3])
            per_family: dict[str, list] = {}
            for key, g in objs.items():
                per_family.setdefault(key.entity, []).append((key, g))
            queried = set(perfsvc.MODEL.ENTITIES if scope is None else scope)
            wanted = {f for f in expected if f in queried}
            # the ESA dedup families carry data only where dedup is on
            dedup_on = bool(getattr(mos["vsan-cluster-config-system"].VsanClusterGetConfig(cluster).dataEfficiencyConfig, "dedupEnabled", False))
            if not dedup_on:
                wanted = {f for f in wanted if "-esa-dedup-" not in f}
            missing = sorted(wanted - set(per_family))
            extra = sorted(set(per_family) - wanted)
            check(f"{cname}: collect[{scope_name}]: every data family returned objects", not missing, missing)
            check(f"{cname}: collect[{scope_name}]: no family the live service lacked", not extra, extra)
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
            check(f"{cname}: collect[{scope_name}]: gauge label set per family equals the live set", not bad_labels, bad_labels[:3])
            unresolved = [key.display(names) for key in objs if any(
                re.fullmatch(r"[0-9a-f-]{36}", v) for v in key.display(names).split(" ")[:1])]
            check(f"{cname}: collect[{scope_name}]: every object name resolved", not unresolved, unresolved[:3])
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
            check(f"{cname}: collect[{scope_name}]: every object has a vCenter parent", not orphans, orphans[:3])
            counts = {f: len(v) for f, v in per_family.items()}
            results[scope_name] = {"objects": len(objs), "families": len(per_family), "counts": counts}
            print(f"  {cname} {scope_name}: {len(objs)} objects in {len(per_family)} families")

        counts = results["all"]["counts"]
        nh = len(hosts)
        view = content.viewManager.CreateContainerView(cluster, [perfsvc.vim.VirtualMachine], True)
        vms_expected = len(list(view.view))
        view.DestroyView()
        check(f"{cname}: cardinality: host families = hosts", counts.get("host-domclient") == nh and counts.get("vsan-cpu") == nh, counts.get("host-domclient"))
        check(f"{cname}: cardinality: cluster families = 1", counts.get("cluster-domclient") == 1)
        check(f"{cname}: cardinality: virtual-machine = VMs in the cluster", counts.get("virtual-machine") == vms_expected, (counts.get("virtual-machine"), vms_expected))
        check(f"{cname}: cardinality: vsan-pnic-net = vDS uplinks", counts.get("vsan-pnic-net") == uplinks, (counts.get("vsan-pnic-net"), uplinks))
        check(f"{cname}: cardinality: host-cpu = threads + one aggregate per host", counts.get("host-cpu") == threads + nh, (counts.get("host-cpu"), threads + nh))
        check(f"{cname}: cardinality: vsan-esa-disk-layer = claimed disks", counts.get("vsan-esa-disk-layer") == claimed, (counts.get("vsan-esa-disk-layer"), claimed))

        # Second collection: same keys.
        objs2, problems2 = perfsvc.collect(perf, cluster, scope=None)
        objs1, _ = perfsvc.collect(perf, cluster, scope=None)
        check(f"{cname}: second collect: same object keys", set(objs1) == set(objs2) and not problems2, (len(objs1), len(objs2)))
        report["clusters"].append({"name": cname, "hosts": nh, "vms": vms_expected, "results": results})
    report["timing_s"] = round(time.time() - t0, 2)
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
        check("connect (verify on, accepted PEM pinned)", len(clusters2) == len(clusters))
        perfsvc.Disconnect(si2)

    if args.report:
        with open(args.report, "w") as fh:
            json.dump(report, fh, indent=1)
    print(f"{len(report['checks']) - len(report['failures'])}/{len(report['checks'])} checks passed in {report['timing_s']}s")
    return 1 if report["failures"] else 0


if __name__ == "__main__":
    sys.exit(main())
