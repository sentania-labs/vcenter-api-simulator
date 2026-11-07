#!/usr/bin/env python3
"""Run the reference pack's `mp-test connect` and `mp-test collect` (twice)
against the simulator and judge the collections, as recon section 9 test 5
defines it.

    python3 tools/mp-harness/mp_test_run.py --sim-port 8443 --mp /path/to/vsanhostmetrics \
        [--sim-host 172.17.0.1] [--scope dashboard|all] [--report out.json]

Needs: the VCF Operations integration SDK CLI (`mp-test`, package
vmware-aria-operations-integration-sdk==1.3.1) on PATH and a Docker daemon;
mp-test builds the pack's adapter container from its Dockerfile and runs it,
so the simulator must be reachable from a container. By default the host is
the Docker bridge gateway, which is where a simulator bound to 0.0.0.0 answers.

mp-test draws spinners and probes for a terminal, so it runs under a pty
(the pack ships tools/mp_build_pty.py for the same reason). It logs each
response JSON to <project>/logs/test.log, which is where the results are read.

Pass criteria: connect reports no error; both collects succeed with no
errorMessage and no "problem" in the adapter's own warnings; both produce the
same object keys (kind + identifiers); the expected resource kinds are present
with counts matching the simulator's topology; every relationship parent is
a vCenter object when attach is on.
"""
from __future__ import annotations

import argparse
import json
import os
import pty
import re
import select
import shutil
import subprocess
import sys
import time

ANSI = re.compile(rb"\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07|\r")


def bridge_ip() -> str:
    out = subprocess.run(["docker", "network", "inspect", "bridge", "-f", "{{(index .IPAM.Config 0).Gateway}}"],
                         capture_output=True, text=True, check=True).stdout.strip()
    return out or "172.17.0.1"


def write_connection(project: str, name: str, host: str, scope: str, attach: bool, user: str = "user", password: str = "pass") -> None:
    path = os.path.join(project, "connections.json")
    cfg = {"connections": [], "suite_api_hostname": None, "suite_api_username": None, "suite_api_password": None}
    if os.path.exists(path):
        with open(path) as fh:
            cfg = json.load(fh)
    cfg["connections"] = [c for c in cfg.get("connections", []) if c.get("name") != name]
    ident = lambda v, uniq=False: {"value": v, "required": True, "part_of_uniqueness": uniq}  # noqa: E731
    cfg["connections"].append({
        "name": name,
        "identifiers": {
            "vcenter_host": ident(host, True),
            "verify_certs": ident("false"),
            "metric_scope": ident(scope),
            "attach_to_vcenter": ident("true" if attach else "false"),
        },
        "credential": {
            "credential_kind_key": "vsan_vc_credential",
            "vcenter_username": {"value": user, "required": True, "password": False},
            "vcenter_password": {"value": password, "required": True, "password": True},
        },
        # [] means endpointURLs was called and found none; without it mp-test
        # calls the adapter's endpointURLs first, which is fine too.
        "certificates": [],
    })
    with open(path, "w") as fh:
        json.dump(cfg, fh, indent=2)


def run_pty(argv: list[str], cwd: str, timeout: int) -> tuple[int, str]:
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(cwd)
        os.execvp(argv[0], argv)
    out = bytearray()
    start = time.time()
    status = None
    while time.time() - start < timeout:
        ready, _, _ = select.select([fd], [], [], 2)
        if ready:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            if not data:
                break
            out += data
        done, st = os.waitpid(pid, os.WNOHANG)
        if done:
            status = st
            # drain
            while True:
                try:
                    ready, _, _ = select.select([fd], [], [], 0.5)
                    if not ready:
                        break
                    data = os.read(fd, 65536)
                    if not data:
                        break
                    out += data
                except OSError:
                    break
            break
    if status is None:
        os.kill(pid, 9)
        _, status = os.waitpid(pid, 0)
    text = ANSI.sub(b"", bytes(out)).decode("utf-8", "replace")
    return os.waitstatus_to_exitcode(status), text


def response_json(log_path: str) -> dict | None:
    """The response JSON mp-test logged (logger.info(result_bundle))."""
    with open(log_path, encoding="utf-8", errors="replace") as fh:
        lines = fh.read().splitlines()
    for i, line in enumerate(lines):
        if " INFO {" not in line:
            continue
        rest = line[line.index(" INFO {") + len(" INFO "):].rstrip()
        if rest.endswith("{"):
            buf = ["{"]
            for j in range(i + 1, len(lines)):
                buf.append(lines[j])
                if lines[j] == "}":
                    try:
                        return json.loads("\n".join(buf))
                    except json.JSONDecodeError:
                        break
        else:
            try:
                return json.loads(rest)  # a one-line response, e.g. {} from connect
            except json.JSONDecodeError:
                continue
    return None


def object_keys(collect: dict) -> set[tuple]:
    keys = set()
    for obj in collect.get("result", []):
        k = obj["key"]
        idents = tuple(sorted((i["key"], i.get("value", "")) for i in k.get("identifiers", []) if i.get("isPartOfUniqueness", True)))
        keys.add((k["adapterKind"], k["objectKind"], idents))
    return keys


FAMILY_RE = re.compile(r"^perf-domain-c6004-(.+)\.resp\.xml$")
LABEL_RE = re.compile(r"<label>([^<]*)</label>")
REF_RE = re.compile(r"<entityRefId[^>]*>([^<]*)</entityRefId>")
UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")


def fixture_labels(fixtures: str) -> dict[str, list[str]]:
    """family -> ordered labels the live service returned (data families only)."""
    out = {}
    for fn in sorted(os.listdir(fixtures)):
        m = FAMILY_RE.match(fn)
        if not m:
            continue
        text = open(os.path.join(fixtures, fn), encoding="utf-8").read()
        if not [r for r in REF_RE.findall(text) if "*" not in r]:
            continue
        seen, labels = set(), []
        for lab in LABEL_RE.findall(text):
            if lab not in seen:
                seen.add(lab)
                labels.append(lab)
        out[m.group(1)] = labels
    return out


def check_labels_and_identifiers(collect: dict, project: str, fixtures: str, scope: str, fail) -> None:
    """Per object kind: the metric keys the adapter emitted equal the label
    set the live service returned for that family (minus the "<x>Actual"
    twins the pack drops, per its own rule), the identifier names are the
    pack's model identity for the family, host/cluster/VM identifiers are
    well-formed and consistent across kinds, and every metric has a number."""
    sys.path.insert(0, os.path.join(project, "app"))
    import perfsvc_model  # noqa: E402  (the pack's generated model)
    import metric_scope  # noqa: E402
    try:
        import perfsvc_model_extra  # noqa: E402
        for e, spec in perfsvc_model_extra.ENTITIES.items():
            perfsvc_model.ENTITIES.setdefault(e, spec)
    except ImportError:
        pass
    ent_for_kind = {spec["kind"]: ent for ent, spec in perfsvc_model.ENTITIES.items()}
    scoped = None
    if scope == "dashboard":
        scoped = {ent_for_kind[k]: set(m) for k, m in metric_scope.COVERED.items() if k in ent_for_kind}
    expected = fixture_labels(fixtures)
    bad_labels, bad_idents, bad_values = [], [], []
    host_uuids: dict[str, set] = {}
    cluster_uuids, vm_uuids = set(), set()
    for obj in collect.get("result", []):
        kind = obj["key"]["objectKind"]
        ent = ent_for_kind.get(kind)
        if ent is None:
            bad_idents.append((kind, "not a modelled kind"))
            continue
        spec = perfsvc_model.ENTITIES[ent]
        keep = set(spec["metrics"])
        if scoped is not None:
            keep &= scoped.get(ent, set())
        want = {l for l in expected.get(ent, []) if not (l.endswith("Actual") and l[:-6] in keep)}
        got = {m["key"] for m in obj.get("metrics", [])}
        if got != want:
            bad_labels.append((kind, sorted(want - got)[:4], sorted(got - want)[:4]))
        for m in obj.get("metrics", []):
            if not isinstance(m.get("numberValue"), (int, float)):
                bad_values.append((kind, m.get("key")))
        idents = {i["key"]: i.get("value", "") for i in obj["key"].get("identifiers", [])}
        if set(idents) != set(spec["identity"]):
            bad_idents.append((kind, sorted(idents), spec["identity"]))
        for name, v in idents.items():
            if name in ("host_uuid", "cluster_uuid", "vm_uuid", "disk_uuid") and not UUID_RE.match(v):
                bad_idents.append((kind, name, v[:40]))
        if "host_uuid" in idents:
            host_uuids.setdefault(kind, set()).add(idents["host_uuid"])
        if "cluster_uuid" in idents:
            cluster_uuids.add(idents["cluster_uuid"])
        if "vm_uuid" in idents:
            vm_uuids.add(idents["vm_uuid"])
    if bad_labels:
        fail(f"metric labels differ from the live capture for {len(bad_labels)} objects: {bad_labels[:3]}")
    else:
        print("PASS metric labels per kind equal the live capture's label sets")
    if bad_idents:
        fail(f"identifier problems: {bad_idents[:3]}")
    else:
        print("PASS identifier names per kind match the pack's model, values well-formed")
    if bad_values:
        fail(f"metrics without a numeric value: {bad_values[:3]}")
    sets = {frozenset(s) for s in host_uuids.values()}
    if len(sets) != 1:
        fail(f"host_uuid sets differ across kinds: {[sorted(s)[:1] for s in sets][:3]}")
    else:
        print(f"PASS the same {len(next(iter(sets)))} host uuids identify every host-scoped kind")
    if len(cluster_uuids) != 1:
        fail(f"cluster uuids: {sorted(cluster_uuids)}")
    print(f"identifier sets: hosts={len(next(iter(sets))) if sets else 0} cluster={len(cluster_uuids)} vms={len(vm_uuids)}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mp", required=True, help="vsanhostmetrics checkout (pinned commit); mp-test runs in it")
    ap.add_argument("--sim-port", type=int, required=True)
    ap.add_argument("--sim-host", help="address the adapter container uses (default: Docker bridge gateway)")
    ap.add_argument("--scope", default="dashboard", choices=["dashboard", "all"])
    ap.add_argument("--attach", default="true", choices=["true", "false"], help="attach_to_vcenter")
    ap.add_argument("--clusters", type=int, default=1, help="vSAN clusters in the scenario")
    ap.add_argument("--hosts", type=int, default=4, help="hosts in the scenario, all clusters together")
    ap.add_argument("--vms", type=int, default=4, help="VMs in the scenario, all clusters together")
    ap.add_argument("--user", default="user", help="vCenter user name the adapter logs in with")
    ap.add_argument("--password", default="pass")
    ap.add_argument("--timeout", type=int, default=1800, help="seconds per mp-test invocation (the first builds the image)")
    ap.add_argument("--fixtures", default="fixtures/live/9.1.1", help="sanitized capture, for the per-kind label comparison")
    ap.add_argument("--report")
    args = ap.parse_args()

    mp_test = shutil.which("mp-test")
    if not mp_test:
        print("mp-test not on PATH", file=sys.stderr)
        return 2
    host = args.sim_host or bridge_ip()
    target = f"{host}:{args.sim_port}"
    project = os.path.abspath(args.mp)
    os.makedirs(os.path.join(project, "logs"), exist_ok=True)
    write_connection(project, "sim", target, args.scope, args.attach == "true", args.user, args.password)
    report: dict = {"target": target, "scope": args.scope, "runs": [], "failures": []}

    def fail(msg: str):
        report["failures"].append(msg)
        print("FAIL", msg)

    log = os.path.join(project, "logs", "test.log")

    def run_mp_test(argv: list[str]) -> tuple[int, dict | None]:
        """One mp-test invocation; its response JSON comes from logs/test.log.
        mp-test gives the adapter container 20 s to come up and gives up
        otherwise. That is the SDK's own start-up race, not a result from the
        adapter, so it is retried once."""
        for attempt in (1, 2):
            code, text = run_pty(argv, project, args.timeout)
            print("\n".join(l for l in text.splitlines() if l.strip())[-2500:])
            res = response_json(log)
            if res is None and attempt == 1 and "HTTP Server did not start" in open(log, errors="replace").read():
                print("   adapter container did not start in time; retrying once")
                continue
            return code, res
        return code, res

    print(f"== mp-test connect against {target}")
    code, connect = run_mp_test([mp_test, "-p", project, "-c", "sim", "-v", "1", "connect"])
    report["runs"].append({"method": "connect", "exit": code, "response": connect})
    if code != 0:
        fail(f"mp-test connect exit {code}")
    if connect is None or connect.get("errorMessage"):
        fail(f"connect result: {connect}")

    collects = []
    for n in (1, 2):
        print(f"== mp-test collect #{n}")
        code, col = run_mp_test([mp_test, "-p", project, "-c", "sim", "-v", "1", "collect", "-n", str(n), "-t", "10m"])
        validation = ""
        vpath = os.path.join(project, "logs", "validation.log")
        if os.path.exists(vpath):
            validation = open(vpath).read()
        report["runs"].append({"method": f"collect-{n}", "exit": code, "objects": len((col or {}).get("result", [])),
                               "validation": validation[-2000:]})
        if code != 0:
            fail(f"collect #{n} exit {code}")
        if col is None:
            fail(f"collect #{n}: no response JSON in {log}")
            continue
        if col.get("errorMessage"):
            fail(f"collect #{n}: errorMessage {col['errorMessage']}")
        if "ERROR" in validation:
            fail(f"collect #{n}: validation errors: {validation[-500:]}")
        collects.append(col)

    if len(collects) == 2:
        k1, k2 = object_keys(collects[0]), object_keys(collects[1])
        if k1 != k2:
            fail(f"object keys differ between collections: {len(k1)} vs {len(k2)}")
        else:
            print(f"PASS both collections produced the same {len(k1)} object keys")
        kinds: dict[str, int] = {}
        for obj in collects[1].get("result", []):
            kinds[obj["key"]["objectKind"]] = kinds.get(obj["key"]["objectKind"], 0) + 1
        report["kinds"] = kinds
        # object kinds are the pack's (app/perfsvc_model.py ENTITIES[*]["kind"]).
        # Per-host device and uplink counts vary per cluster in Phase 2
        # scenarios, so the uplink and disk kinds are checked only for a
        # single-cluster run with the default shape.
        expect = {"VsanClusterDomclient": args.clusters, "VsanHostDomclient": args.hosts, "VsanCpu": args.hosts,
                  "VsanVirtualMachine": args.vms, "VsanVscsi": args.vms, "VsanVirtualDisk": args.vms}
        if args.clusters == 1:
            expect.update({"VsanPnic": args.hosts * 2, "VsanEsaDiskLayer": args.hosts})
        for kind, count in expect.items():
            if kinds.get(kind) != count:
                fail(f"kind {kind}: {kinds.get(kind)} objects, expected {count}")
        # relationships: every parent should be a vCenter object kind when attach is on
        if args.attach == "true":
            foreign = set()
            for rel in collects[1].get("relationships", []):
                pk = rel.get("parent", {})
                if pk.get("adapterKind") != "VMWARE":
                    foreign.add((pk.get("adapterKind"), pk.get("objectKind")))
            if foreign:
                fail(f"relationships parented outside the VMWARE adapter: {sorted(foreign)[:3]}")
            else:
                print(f"PASS {len(collects[1].get('relationships', []))} relationships, all parented on vCenter objects")
        print("kinds:", json.dumps(kinds, sort_keys=True))
        check_labels_and_identifiers(collects[1], project, args.fixtures, args.scope, fail)

    if args.report:
        with open(args.report, "w") as fh:
            json.dump(report, fh, indent=1)
    print("RESULT:", "PASS" if not report["failures"] else f"FAIL ({len(report['failures'])})")
    return 1 if report["failures"] else 0


if __name__ == "__main__":
    sys.exit(main())
