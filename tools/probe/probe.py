#!/usr/bin/env python3
"""Read-only API walk for the vCenter API Simulator (tools/probe/probe.py).

Walks a vCenter once, read-only, and writes one tarball of evidence the
simulator can be built against: an inventory dump, the read-only vSphere /
vSAN / PBM / VSLM / REST calls, and a summary of what answered. The tarball
holds the sanitized tree only and is what to send back; the raw tree stays
on disk, and only with --zip.

Usage
    python3 tools/probe/probe.py <target> [--out DIR] [--dry-run] [--zip]

    <target>      "vcsim" or a SOAP base URL, e.g. https://vc.example:443
    --out DIR     where to write the tarball (default: cwd)
    --dry-run     print the call plan and exit; needs no server or credential
    --zip         keep the work directory (raw/ and sanitized/) on disk

Credentials come from VCENTER_USER and VCENTER_PASSWORD, or from an
interactive prompt (the password is not echoed) when a variable is missing.
Nothing else writes them; nothing is written but the tarball (and, with
--zip, the work directory).

Needs python3 with pyVmomi (its own bundled vSAN bindings: vsanmgmtObjects,
vsanapiutils). TLS certificate checks are off, so it can walk lab vCenters.

Every call is a read-only query. A call that faults is recorded with its
fault name and the walk continues.
"""
from __future__ import annotations

import argparse
import datetime as dt
import getpass
import hashlib
import json
import os
import re
import shutil
import ssl
import sys
import tarfile
import tempfile
import urllib.error
import urllib.request
import uuid
from urllib.parse import urlsplit

READ_ONLY_PRIVS = ("System.Anonymous", "System.Read", "System.View")

PLAIN_GETS = (
    "/", "/sdk", "/sdk/", "/sdk/vimService.wsdl", "/sdk/vim.wsdl",
    "/sdk/vimServiceVersions.xml", "/sdk/vsanServiceVersions.xml",
    "/sdk/pbmService.wsdl", "/sdk/vsan.wsdl", "/vsanHealth", "/vsanHealth/",
    "/vsanHealth/vsanServiceVersions.xml", "/vsan/", "/pbm", "/sms/sdk",
    "/vslm/sdk", "/eam/sdk", "/lookupservice/sdk", "/sts/STSService",
    "/api", "/rest", "/rest/vcenter/vm", "/ui/", "/websso/",
    "/api/vcenter/vm",
)

REST_COLLECTIONS = (
    "/api/vcenter/datacenter", "/api/vcenter/folder", "/api/vcenter/cluster",
    "/api/vcenter/host", "/api/vcenter/vm", "/api/vcenter/datastore",
    "/api/vcenter/network", "/api/vcenter/resource-pool",
    "/api/appliance/system/version", "/api/appliance/health/system",
    "/api/vcenter/system-config/deployment-type", "/api/cis/tagging/category",
    "/api/cis/tagging/tag", "/api/content/library", "/api/content/local-library",
    "/api/vcenter/storage/policies", "/api/vcenter/namespace-management/clusters",
    "/api/vcenter/vm-template/library-items", "/api/vcenter/vcha/cluster/mode",
    "/api/vcenter/inventory/datastore", "/api/vcenter/topology/nodes",
)

# One representative per-entity path set per object kind (the {id} is a MoRef).
REST_BY_ID = {
    "cluster": ("/api/vcenter/cluster/{id}",
                "/api/esx/settings/clusters/{id}/software",
                "/api/esx/settings/clusters/{id}/enablement/software",
                "/api/vcenter/vsan/clusters/{id}/perf-service"),
    "datastore": ("/api/vcenter/datastore/{id}",),
    "vm": ("/api/vcenter/vm/{id}", "/api/vcenter/vm/{id}/hardware",
           "/api/vcenter/vm/{id}/hardware/disk",
           "/api/vcenter/vm/{id}/guest/identity",
           "/api/vcenter/vm/{id}/storage/policy"),
    "host": ("/api/vcenter/host/{id}", "/api/esx/settings/hosts/{id}/software"),
}

# vSAN Performance Service entity families. The first block is answered on
# both architectures; the OSA block is the disk-group / cache-disk /
# capacity-disk set that carries the data on an OSA cluster and only echoes
# empty on ESA; the ESA block is the reverse. Every one of them is queried.
PERF_FAMILIES_COMMON = (
    "cluster-domclient", "host-domclient", "virtual-machine", "vsan-cpu",
    "host-cpu", "vsan-pnic-net", "vsan-distribution", "cmmds-workload",
    "heap-memory", "slab-memory",
)
PERF_FAMILIES_OSA = ("disk-group", "cache-disk", "capacity-disk", "lsom-world-cpu",
                     "ddh-disk", "clom-disk", "clom-host")
PERF_FAMILIES_ESA = ("vsan-esa-disk-layer", "vsan-esa-disk-scsifw",
                     "vsan-esa-dedup-hash", "vsan-esa-dedup-xip",
                     "vsan-esa-dedup-journal", "vsan-esa-dedup-reorder",
                     "vsan-esa-nvme-namespace")
PERF_FAMILIES = PERF_FAMILIES_COMMON + PERF_FAMILIES_OSA + PERF_FAMILIES_ESA

# Families the live 9.1.1 service rejects when queried with the `name:*`
# wildcard (InvalidArgument on entityRefId); they need an explicit ref built
# from the disk/host inventory. They are still queried, so the fault is
# recorded as the finding.
WILDCARD_REJECTING = ("ddh-disk", "clom-disk", "clom-host", "vsan-esa-disk-scsifw",
                      "vsan-esa-dedup-hash", "vsan-esa-dedup-xip", "vsan-esa-dedup-journal",
                      "vsan-esa-dedup-reorder", "vsan-esa-nvme-namespace")

WINDOW_MINUTES = 15


# --------------------------------------------------------------------------- #
# plumbing
# --------------------------------------------------------------------------- #
def fault_name(exc: BaseException) -> str:
    return f"{type(exc).__module__}.{type(exc).__name__}"


def fault_info(exc: BaseException) -> dict:
    info = {"fault": fault_name(exc), "msg": str(getattr(exc, "msg", "") or str(exc))[:300]}
    for attr in ("invalidProperty", "privilegeId", "faultMessage"):
        val = getattr(exc, attr, None)
        if val:
            info[attr] = json.loads(json.dumps(val, default=str))
    return info


def make_serializer():
    """Return a `ser(obj)` that turns pyVmomi objects into JSON-able data."""
    from pyVmomi import VmomiSupport

    def ser(obj):
        if obj is None or isinstance(obj, (bool, int, float, str)):
            return obj
        if isinstance(obj, dt.datetime):
            return obj.isoformat()
        if isinstance(obj, (bytes, bytearray)):
            return {"_bytes": len(obj)}
        if isinstance(obj, VmomiSupport.ManagedObject):
            return {"_moref": f"{obj._wsdlName}:{obj._moId}"}
        if isinstance(obj, VmomiSupport.Enum):
            return str(obj)
        if isinstance(obj, (list, tuple)):
            return [ser(x) for x in obj]
        if isinstance(obj, dict):
            return {str(k): ser(v) for k, v in obj.items()}
        if isinstance(obj, VmomiSupport.DataObject):
            out = {"_type": obj._wsdlName}
            for prop in obj._GetPropertyList():
                try:
                    val = getattr(obj, prop.name, None)
                except Exception as exc:                      # noqa: BLE001
                    val = {"_err": fault_name(exc)}
                if val is None:
                    continue
                if isinstance(val, (list, tuple)) and not val:
                    continue
                out[prop.name] = ser(val)
            return out
        return str(obj)

    return ser


class Capture:
    """Writes JSON records and one call log for the walk."""

    def __init__(self, raw_dir: str):
        self.raw = raw_dir
        self.calls: list[dict] = []
        self.noted: list[dict] = []
        self.t0 = 0.0

    def path(self, *parts: str) -> str:
        full = os.path.join(self.raw, *parts)
        os.makedirs(os.path.dirname(full), exist_ok=True)
        return full

    def write(self, parts: tuple[str, ...], obj) -> int:
        if not parts[-1].endswith(".json"):
            parts = (*parts[:-1], parts[-1] + ".json")
        full = self.path(*parts)
        with open(full, "w", encoding="utf-8") as fh:
            json.dump(obj, fh, default=str)
            fh.write("\n")
        return os.path.getsize(full)

    def call(self, parts: tuple[str, ...], func, *args, **kwargs):
        """Call a read-only method, record the result or the fault."""
        import time
        started = time.time()
        name = "/".join(parts)
        try:
            result = func(*args, **kwargs)
            payload = _jsonable(_SER(result))
            size = self.write(parts, payload)
            self.calls.append({"name": name, "ok": True, "s": round(time.time() - started, 3),
                               "bytes": size})
            return result
        except Exception as exc:                              # noqa: BLE001
            info = fault_info(exc)
            self.write(parts, {"_fault": info})
            self.calls.append({"name": name, "ok": False, "s": round(time.time() - started, 3), **info})
            return None

    def note(self, step: str, **fields):
        import time
        self.noted.append({"step": step, "t": round(time.time() - self.t0, 2), **fields})


_SER = None  # set once pyVmomi is imported


def _jsonable(value):
    """Collapse the serializer's output into plain JSON-able data."""
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    if isinstance(value, (list, tuple)):
        return [_jsonable(v) for v in value]
    if isinstance(value, dict):
        return {str(k): _jsonable(v) for k, v in value.items()}
    return str(value)


# --------------------------------------------------------------------------- #
# credentials
# --------------------------------------------------------------------------- #
def read_credentials() -> dict:
    """VCENTER_USER/PASSWORD from the environment, prompting for gaps."""
    keys = ("VCENTER_USER", "VCENTER_PASSWORD")
    creds = {k: os.environ.get(k, "").strip() for k in keys}
    for key in keys:
        if creds[key]:
            continue
        try:
            prompt = f"{key}="
            value = (getpass.getpass(prompt) if key == "VCENTER_PASSWORD" else input(prompt)).strip()
        except EOFError:
            value = ""
        creds[key] = value
    if not creds["VCENTER_USER"] or not creds["VCENTER_PASSWORD"]:
        raise SystemExit("no credentials: set VCENTER_USER and VCENTER_PASSWORD")
    return creds


# --------------------------------------------------------------------------- #
# the walk
# --------------------------------------------------------------------------- #
def walk(base: str, creds: dict, cap: Capture, timeout: int = 30):
    from pyVim.connect import Disconnect
    sessions: list = []
    try:
        _walk(base, creds, cap, timeout, sessions)
    finally:
        for si in sessions:
            try:
                Disconnect(si)
            except Exception:                                   # noqa: BLE001
                pass


def _walk(base: str, creds: dict, cap: Capture, timeout: int, sessions: list):
    from pyVmomi import SoapStubAdapter, VmomiSupport, pbm, sms, vim, vmodl, vslm, eam
    from pyVim.connect import SmartConnect
    import vsanapiutils

    global _SER
    _SER = make_serializer()
    ctx = ssl._create_unverified_context()
    parts = urlsplit(base if "//" in base else f"//{base}")
    vc_host, port = parts.hostname, parts.port or 443

    # Step 0: connect, about, read-only confirmation.
    si = SmartConnect(host=vc_host, port=port, user=creds["VCENTER_USER"],
                      pwd=creds["VCENTER_PASSWORD"], disableSslCertValidation=True)
    sessions.append(si)
    content = si.RetrieveContent()
    cap.write(("about.json",), {"about": _jsonable(_SER(content.about))})
    cookie = si._stub.cookie
    privs = []
    try:
        sess = content.sessionManager.currentSession
        res = content.authorizationManager.HasPrivilegeOnEntities(
            [content.rootFolder], sess.key, list(READ_ONLY_PRIVS))
        privs = sorted(p.privId for p in res[0].privAvailability if p.isGranted)
    except Exception:                                           # noqa: BLE001
        try:
            user = content.sessionManager.currentSession.userName
            res = content.authorizationManager.FetchUserPrivilegeOnEntities(
                [content.rootFolder], user)
            privs = sorted(set(res[0].privileges) & set(READ_ONLY_PRIVS))
        except Exception as exc:                                # noqa: BLE001
            privs = [fault_name(exc)]
    cap.note("readonly_check", available=privs)

    # Step 1: /sdk inventory through the PropertyCollector only.
    pc = content.propertyCollector

    def retrieve(objs, cls, max_objects, path=None):
        prop_spec = vmodl.query.PropertyCollector.PropertySpec(
            type=cls, all=(path is None), pathSet=([path] if path else None))
        filter_spec = vmodl.query.PropertyCollector.FilterSpec(
            propSet=[prop_spec],
            objectSet=[vmodl.query.PropertyCollector.ObjectSpec(obj=o, skip=False) for o in objs])
        options = vmodl.query.PropertyCollector.RetrieveOptions(maxObjects=max_objects)
        out = []
        result = pc.RetrievePropertiesEx([filter_spec], options)
        while result:
            for oc in result.objects:
                rec = {"_moref": f"{oc.obj._wsdlName}:{oc.obj._moId}", "props": {}}
                for p in oc.propSet:
                    rec["props"][p.name] = _SER(p.val)
                if oc.missingSet:
                    rec["missing"] = [{"path": m.path, "fault": fault_name(m.fault)}
                                      for m in oc.missingSet]
                out.append(rec)
            result = pc.ContinueRetrievePropertiesEx(result.token) if result.token else None
        return out

    def retrieve_all(objs, type_name, max_objects=50):
        """all=true per batch; falls back per object, then per property."""
        cls = type(objs[0])
        try:
            return retrieve(objs, cls, max_objects)
        except Exception as exc:                                # noqa: BLE001
            last = exc
        out = []
        for obj in objs:
            try:
                out.extend(retrieve([obj], cls, 1))
                continue
            except Exception as exc:                            # noqa: BLE001
                last = exc
            rec = {"_moref": f"{obj._wsdlName}:{obj._moId}", "props": {}, "prop_errors": {}}
            for prop in cls._GetPropertyList():
                try:
                    got = retrieve([obj], cls, 1, prop.name)
                    if got and got[0]["props"]:
                        rec["props"].update(got[0]["props"])
                except Exception as exc:                        # noqa: BLE001
                    rec["prop_errors"][prop.name] = fault_name(exc)
            if rec["props"]:
                out.append(rec)
        if not out:
            return [{"_fault": fault_info(last), "type": type_name}]
        return out

    view = content.viewManager.CreateContainerView(content.rootFolder, [], True)
    all_objs = list(view.view)
    view.DestroyView()
    by_type: dict[str, list] = {}
    for obj in all_objs:
        by_type.setdefault(obj._wsdlName, []).append(obj)
    cap.write(("inv", "INDEX.json"), {k: len(v) for k, v in sorted(by_type.items())})
    submos: dict[str, set] = {}
    known = set(by_type)
    for type_name, objs in sorted(by_type.items()):
        try:
            records = _jsonable(_SER(retrieve_all(objs, type_name)))
        except Exception as exc:                                # noqa: BLE001
            records = [{"_fault": fault_info(exc), "type": type_name}]
        cap.write(("inv", f"{type_name}.json"), records)
        for rec in records:
            if isinstance(rec, dict):
                _walk_moref(rec.get("props") or {}, submos, known)
    for mtype, refs in sorted(submos.items()):
        recs = []
        for ref in sorted(refs)[:20]:
            moid = ref.split(":", 1)[1]
            try:
                cls = VmomiSupport.GetWsdlType("urn:vim25", mtype)
            except Exception:                                   # noqa: BLE001
                cls = None
            if cls is None:
                recs.append({"_moref": ref, "_skip": "no class"})
                continue
            try:
                recs.extend(_jsonable(_SER(retrieve_all([cls(moid, si._stub)], mtype, 1))))
            except Exception as exc:                            # noqa: BLE001
                recs.append({"_moref": ref, "_fault": fault_info(exc)})
        cap.write(("submo", f"{mtype}.json"), recs)

    # ServiceContent managers, properties only.
    excluded = {"licenseManager", "cryptoManager", "propertyCollector", "viewManager"}
    managers = {}
    for prop in content._GetPropertyList():
        mo = getattr(content, prop.name, None)
        if mo is None or prop.name in excluded or not isinstance(mo, VmomiSupport.ManagedObject):
            continue
        if prop.name == "sessionManager":
            try:
                managers[prop.name] = {"currentSession": _jsonable(_SER(mo.currentSession))}
            except Exception as exc:                            # noqa: BLE001
                managers[prop.name] = fault_info(exc)
            continue
        try:
            managers[prop.name] = {"type": mo._wsdlName,
                                   "records": _jsonable(_SER(retrieve_all([mo], mo._wsdlName, 1)))}
        except Exception as exc:                                # noqa: BLE001
            managers[prop.name] = {"type": mo._wsdlName, "fault": fault_info(exc)}
    cap.write(("managers", "INDEX.json"), managers)

    # Step 1b: read-only /sdk method calls, bounded.
    now = dt.datetime.now(dt.timezone.utc)
    start = now - dt.timedelta(minutes=WINDOW_MINUTES)
    clusters = by_type.get("ClusterComputeResource", [])
    hosts = by_type.get("HostSystem", [])
    vms = by_type.get("VirtualMachine", [])
    datastores = by_type.get("Datastore", [])
    pools = by_type.get("ResourcePool", [])
    switches = by_type.get("VmwareDistributedVirtualSwitch", []) + by_type.get("DistributedVirtualSwitch", [])

    def vsan_enabled(cluster):
        try:
            return bool(cluster.configurationEx.vsanConfigInfo.enabled)
        except Exception:                                       # noqa: BLE001
            return False

    cluster0 = next((c for c in clusters if vsan_enabled(c)), clusters[0] if clusters else None)
    cluster_hosts = list(cluster0.host) if cluster0 else hosts
    host0 = cluster_hosts[0] if cluster_hosts else (hosts[0] if hosts else None)
    vm0 = next((v for v in vms if getattr(v, "runtime", None)
                and v.runtime.powerState == "poweredOn"), vms[0] if vms else None)

    cap.call(("methods", "ServiceInstance.CurrentTime"), si.CurrentTime)
    perf = content.perfManager
    for label, ent in (("cluster", cluster0), ("host", host0), ("vm", vm0),
                       ("datastore", datastores[0] if datastores else None),
                       ("resourcepool", pools[0] if pools else None),
                       ("vds", switches[0] if switches else None)):
        if ent is None:
            continue
        cap.call(("methods", f"PerfManager.QueryPerfProviderSummary-{label}"),
                 perf.QueryPerfProviderSummary, ent)
        for interval in (300, 20) if label in ("host", "vm") else (300,):
            cap.call(("methods", f"PerfManager.QueryAvailablePerfMetric-{label}-{interval}"),
                     perf.QueryAvailablePerfMetric, ent, None, None, interval)
    for label, ent in (("host", host0), ("vm", vm0)):
        if ent is None:
            continue
        for interval in (20, 300):
            try:
                metric_ids = list(perf.QueryAvailablePerfMetric(ent, None, None, interval))
            except Exception:                                   # noqa: BLE001
                metric_ids = []
            if interval == 20:
                spec = vim.PerformanceManager.QuerySpec(entity=ent, intervalId=20, maxSample=3,
                                                        metricId=metric_ids)
            else:
                spec = vim.PerformanceManager.QuerySpec(entity=ent, intervalId=300,
                                                        startTime=start, endTime=now,
                                                        metricId=metric_ids)
            cap.call(("methods", f"PerfManager.QueryPerf-{label}-{interval}"), perf.QueryPerf, [spec])
            spec.format = "csv"
            cap.call(("methods", f"PerfManager.QueryPerf-{label}-{interval}-csv"),
                     perf.QueryPerf, [spec])
    for host in list(dict.fromkeys(cluster_hosts + hosts)):
        mo_id = host._moId
        for name, method in (("HostVsanSystem.QueryDisksForVsan", "QueryDisksForVsan"),
                             ("HostVsanSystem.QueryHostStatus", "QueryHostStatus")):
            try:
                mgr = host.configManager.vsanSystem or _MissingMo("vsanSystem")
            except Exception as exc:                            # noqa: BLE001
                cap.write(("methods", f"{name}-{mo_id}.json"), {"_fault": fault_info(exc)})
                continue
            cap.call(("methods", f"{name}-{mo_id}"), getattr(mgr, method))
        try:
            vis = host.configManager.vsanInternalSystem or _MissingMo("vsanInternalSystem")
            cap.call(("methods", f"HostVsanInternalSystem.QueryPhysicalVsanDisks-{mo_id}"),
                     vis.QueryPhysicalVsanDisks, [])
            cap.call(("methods", f"HostVsanInternalSystem.QueryVsanObjectUuidsByFilter-{mo_id}"),
                     vis.QueryVsanObjectUuidsByFilter, "", 10, None)
        except Exception as exc:                                # noqa: BLE001
            cap.write(("methods", f"HostVsanInternalSystem-{mo_id}.json"), {"_fault": fault_info(exc)})
        cap.call(("methods", f"HostSystem.RetrieveHardwareUptime-{mo_id}"), host.RetrieveHardwareUptime)
        cap.call(("methods", f"HostSystem.QueryHostConnectionInfo-{mo_id}"),
                 host.QueryHostConnectionInfo)
    if cluster0 is not None:
        cap.call(("methods", "ClusterComputeResource.RetrieveDasAdvancedRuntimeInfo"),
                 cluster0.RetrieveDasAdvancedRuntimeInfo)
        try:
            browser = cluster0.environmentBrowser or _MissingMo("environmentBrowser")
            cap.call(("methods", "EnvironmentBrowser.QueryConfigOptionDescriptor-cluster"),
                     browser.QueryConfigOptionDescriptor)
            cap.call(("methods", "EnvironmentBrowser.QueryConfigTarget-cluster"),
                     browser.QueryConfigTarget, None)
        except Exception as exc:                                # noqa: BLE001
            cap.write(("methods", "EnvironmentBrowser-cluster.json"), {"_fault": fault_info(exc)})
    if vm0 is not None:
        try:
            browser = vm0.environmentBrowser or _MissingMo("environmentBrowser")
            cap.call(("methods", "EnvironmentBrowser.QueryConfigOptionDescriptor-vm"),
                     browser.QueryConfigOptionDescriptor)
            cap.call(("methods", "EnvironmentBrowser.QueryConfigOptionEx-vm"),
                     browser.QueryConfigOptionEx, vim.EnvironmentBrowser.ConfigOptionQuerySpec())
        except Exception as exc:                                # noqa: BLE001
            cap.write(("methods", "EnvironmentBrowser-vm.json"), {"_fault": fault_info(exc)})
    for switch in switches[:1]:
        cap.call(("methods", "DistributedVirtualSwitch.FetchDVPorts"),
                 switch.FetchDVPorts, vim.dvs.PortCriteria())
    cap.call(("methods", "AlarmManager.GetAlarm"), content.alarmManager.GetAlarm, None)
    cap.call(("methods", "EventManager.QueryEvents-15min"), content.eventManager.QueryEvents,
             vim.event.EventFilterSpec(time=vim.event.EventFilterSpec.ByTime(
                 beginTime=start, endTime=now)))

    # Step 1c: other SOAP endpoints, read-only, with the shared session cookie.
    def stub_for(path, ns):
        version = VmomiSupport.newestVersions.Get(ns)
        stub = SoapStubAdapter(host=vc_host, port=port, version=version, path=path, sslContext=ctx)
        stub.cookie = cookie
        return stub, version

    endpoints = {}
    try:
        pbm_stub, pbm_ver = stub_for("/pbm", "pbm")
        pbm_si = pbm.ServiceInstance("ServiceInstance", pbm_stub)
        pbm_content = cap.call(("other", "pbm.RetrieveServiceContent"), pbm_si.PbmRetrieveServiceContent)
        profile_mgr = pbm_content.profileManager
        ids = cap.call(("other", "pbm.QueryProfile-STORAGE"),
                       lambda: profile_mgr.PbmQueryProfile(
                           pbm.profile.ResourceType(resourceType="STORAGE"), None))
        if ids:
            cap.call(("other", "pbm.RetrieveContent"), profile_mgr.PbmRetrieveContent, list(ids))
        cap.call(("other", "pbm.FetchCapabilityMetadata"),
                 profile_mgr.PbmFetchCapabilityMetadata, None, None)
        cap.call(("other", "pbm.FetchResourceType"), profile_mgr.PbmFetchResourceType)
        cap.call(("other", "pbm.FetchVendorInfo"), profile_mgr.PbmFetchVendorInfo, None)
        for ds in datastores[:8]:
            cap.call(("other", f"pbm.QueryDefaultRequirementProfile-{ds._moId}"),
                     lambda d=ds: profile_mgr.PbmQueryDefaultRequirementProfile(
                         pbm.placement.PlacementHub(hubType="Datastore", hubId=d._moId)))
        if vm0 is not None:
            cap.call(("other", "pbm.QueryAssociatedProfiles-vm"),
                     lambda: profile_mgr.PbmQueryAssociatedProfiles(
                         [pbm.ServerObjectRef(objectType="virtualMachine", key=vm0._moId)]))
            if ids:
                cap.call(("other", "pbm.QueryAssociatedEntity"),
                         lambda: profile_mgr.PbmQueryAssociatedEntity(list(ids)[0], None))
        endpoints["pbm"] = {"version": pbm_ver}
    except Exception as exc:                                    # noqa: BLE001
        endpoints["pbm"] = fault_info(exc)
    try:
        sms_stub, sms_ver = stub_for("/sms/sdk", "sms")
        sms_si = sms.ServiceInstance("ServiceInstance", sms_stub)
        storage_mgr = cap.call(("other", "sms.QueryStorageManager"), sms_si.QueryStorageManager)
        if storage_mgr is not None:
            cap.call(("other", "sms.QueryProvider"), storage_mgr.QueryProvider)
        endpoints["sms"] = {"version": sms_ver}
    except Exception as exc:                                    # noqa: BLE001
        endpoints["sms"] = fault_info(exc)
    try:
        vslm_stub, vslm_ver = stub_for("/vslm/sdk", "vslm")
        vslm_si = vslm.ServiceInstance("ServiceInstance", vslm_stub)
        vslm_content = cap.call(("other", "vslm.RetrieveContent"), vslm_si.RetrieveContent)
        if vslm_content is not None:
            cap.call(("other", "vslm.ListVStorageObjectForSpec"),
                     vslm_content.vStorageObjectManager.VslmListVStorageObjectForSpec, None, 50)
        endpoints["vslm"] = {"version": vslm_ver}
    except Exception as exc:                                    # noqa: BLE001
        endpoints["vslm"] = fault_info(exc)
    try:
        eam_stub, eam_ver = stub_for("/eam/sdk", "eam")
        eam_mgr = eam.EsxAgentManager("EsxAgentManager", eam_stub)
        agencies = cap.call(("other", "eam.QueryAgency"), eam_mgr.QueryAgency)
        endpoints["eam"] = {"version": eam_ver}
        if agencies:
            for agency in list(agencies)[:10]:
                cap.call(("other", f"eam.Agency-{agency._moId}"),
                         lambda a=agency: {"config": a.QueryConfig(), "runtime": a.runtime,
                                           "agents": [g.runtime for g in a.QueryAgent()]})
    except Exception as exc:                                    # noqa: BLE001
        endpoints["eam"] = fault_info(exc)
    cap.note("other_soap", **{k: ("ok" if "version" in v else v.get("fault"))
                              for k, v in endpoints.items()})

    # Step 2: plain HTTP GETs (no body, no session).
    gets = {}
    for path in PLAIN_GETS:
        gets[path] = http_get(base, path, ctx, timeout, cap, ("http",))
    cap.write(("http", "GETS.json"), gets)
    cap.note("http_gets", **{p: g["status"] for p, g in gets.items()})

    # Step 3: /api REST, GET only, one session, deleted at the end.
    rest = {}
    api_token, rest_info = rest_session(base, creds, ctx, timeout)
    prefix = rest_info.get("prefix", "/api")
    rest["_session"] = rest_info
    if api_token:
        for path in REST_COLLECTIONS:
            rest[path] = http_get(base, path, ctx, timeout, cap, ("rest",),
                                  headers={"vmware-api-session-id": api_token},
                                  prefix=prefix)
        for label, entity in (("cluster", cluster0), ("datastore", datastores[0] if datastores else None),
                              ("vm", vm0), ("host", host0)):
            if entity is None:
                continue
            for template in REST_BY_ID[label]:
                path = template.replace("{id}", entity._moId)
                rest[path] = http_get(base, path, ctx, timeout, cap, ("rest",),
                                      headers={"vmware-api-session-id": api_token},
                                      prefix=prefix)
        rest["_logout"] = rest_delete(base, api_token, ctx, timeout, prefix)
    cap.write(("rest", "INDEX.json"), rest)
    cap.note("rest", n=len(rest), session=rest.get("_session", {}).get("status"))

    # Step 4: /vsanHealth, read-only methods only.
    vsan_version = VmomiSupport.newestVersions.Get("vsan")
    try:
        mos = vsanapiutils.GetVsanVcMos(si._stub, context=ctx, version=vsan_version)
    except Exception as exc:                                    # noqa: BLE001
        mos = {}
        cap.note("vsan_mos_unavailable", fault=fault_info(exc))
    cap.write(("vsan", "INDEX.json"),
              {"binding_version": vsan_version, "mos": sorted(mos)})

    def mo(key):
        return mos.get(key) or _MissingMo(key)

    if cluster0 is not None and mos:
        config_sys = mo("vsan-cluster-config-system")
        perf_svc = mo("vsan-performance-manager")
        health_sys = mo("vsan-cluster-health-system")
        disk_sys = mo("vsan-disk-management-system")
        stretch_sys = mo("vsan-stretched-cluster-system")
        space_sys = mo("vsan-cluster-space-report-system")
        object_sys = mo("vsan-cluster-object-system")
        iscsi_sys = mo("vsan-cluster-iscsi-target-system")
        cap_sys = mo("vsan-vc-capability-system")
        upgrade_sys = mo("vsan-upgrade-systemex")
        power_sys = mo("vsan-cluster-power-system")
        file_sys = mo("vsan-cluster-file-service-system")
        vum_sys = mo("vsan-vum-system")
        remote_sys = mo("vsan-remote-datastore-system")
        check_sys = mo("vsan-cluster-resource-check-system")
        diag_sys = mo("vsan-cluster-diagnostics-system")
        phone_sys = mo("vsan-phonehome-system")
        mass_sys = mo("vsan-mass-collector")

        cap.call(("vsan", "VsanClusterGetConfig"), config_sys.VsanClusterGetConfig, cluster0)
        cap.call(("vsan", "VsanClusterGetRuntimeStats"), config_sys.VsanClusterGetRuntimeStats,
                 cluster0, None)
        cap.call(("vsan", "VsanClusterGetClaimedCapacity"), config_sys.VsanClusterGetClaimedCapacity,
                 cluster0)
        cap.call(("vsan", "VsanQueryClusterDrsStats"), config_sys.VsanQueryClusterDrsStats,
                 cluster0, None)
        cap.call(("vsan", "VsanPerfQueryNodeInformation"), perf_svc.VsanPerfQueryNodeInformation,
                 cluster0)
        ent_types = cap.call(("vsan", "VsanPerfGetSupportedEntityTypes"),
                             perf_svc.VsanPerfGetSupportedEntityTypes)
        cap.call(("vsan", "VsanPerfGetAggregatedEntityTypes"),
                 perf_svc.VsanPerfGetAggregatedEntityTypes)
        cap.call(("vsan", "VsanPerfGetSupportedDiagnosticExceptions"),
                 perf_svc.VsanPerfGetSupportedDiagnosticExceptions)
        cap.call(("vsan", "VsanPerfQueryStatsObjectInformation"),
                 perf_svc.VsanPerfQueryStatsObjectInformation, cluster0)
        cap.call(("vsan", "VsanPerfQueryClusterHealth"), perf_svc.VsanPerfQueryClusterHealth,
                 cluster0)
        cap.call(("vsan", "VsanPerfQueryTimeRanges"), perf_svc.VsanPerfQueryTimeRanges,
                 cluster0, vim.cluster.VsanPerfTimeRangeQuerySpec())
        cap.call(("vsan", "QueryRemoteServerClusters"), perf_svc.QueryRemoteServerClusters,
                 cluster0, None)
        # every advertised family plus every known family, with the wildcard ref;
        # a family the service rejects is recorded by its fault
        families = list(PERF_FAMILIES)
        if isinstance(ent_types, list):
            items = ent_types
        else:
            items = list(getattr(ent_types, "entityTypes", None) or []) if ent_types else []
        for item in items:
            name = getattr(item, "name", None)
            if name and name not in families:
                families.append(name)
        for family in families:
            cap.call(("vsan", f"VsanPerfQueryPerf-{family}"), perf_svc.VsanPerfQueryPerf,
                     [vim.cluster.VsanPerfQuerySpec(entityRefId=f"{family}:*",
                                                    startTime=start, endTime=now)], cluster0)
        cap.call(("vsan", "VsanQueryVcClusterHealthSummary-cache"),
                 health_sys.VsanQueryVcClusterHealthSummary, cluster0, None, None, False, None, True)
        cap.call(("vsan", "VsanQueryAllSupportedHealthChecks"),
                 health_sys.VsanQueryAllSupportedHealthChecks)
        cap.call(("vsan", "VsanHealthQueryVsanClusterHealthConfig"),
                 health_sys.VsanHealthQueryVsanClusterHealthConfig, cluster0)
        cap.call(("vsan", "VsanHealthQueryVsanClusterHealthCheckInterval"),
                 health_sys.VsanHealthQueryVsanClusterHealthCheckInterval, cluster0)
        cap.call(("vsan", "VsanHealthGetVsanClusterSilentChecks"),
                 health_sys.VsanHealthGetVsanClusterSilentChecks, cluster0)
        cap.call(("vsan", "VsanVcClusterQueryVerifyHealthSystemVersions"),
                 health_sys.VsanVcClusterQueryVerifyHealthSystemVersions, cluster0)
        cap.call(("vsan", "VsanVcClusterGetHclInfo"), health_sys.VsanVcClusterGetHclInfo,
                 cluster0, True, False)
        cap.call(("vsan", "VsanHealthIsRebalanceRunning"), health_sys.VsanHealthIsRebalanceRunning,
                 cluster0)
        cap.call(("vsan", "VsanQueryVcClusterSmartStatsSummary"),
                 health_sys.VsanQueryVcClusterSmartStatsSummary, cluster0)
        cap.call(("vsan", "VsanHealthQueryVsanProxyConfig"),
                 health_sys.VsanHealthQueryVsanProxyConfig)
        cap.call(("vsan", "VsanQueryVcClusterVmdkWorkloadTypes"),
                 health_sys.VsanQueryVcClusterVmdkWorkloadTypes)
        cap.call(("vsan", "VsanClusterQueryFileServiceHealthSummary"),
                 health_sys.VsanClusterQueryFileServiceHealthSummary, cluster0)
        cap.call(("vsan", "VsanQuerySpaceUsage"), space_sys.VsanQuerySpaceUsage, cluster0)
        cap.call(("vsan", "QueryVsanManagedStorageSpaceUsage"),
                 space_sys.QueryVsanManagedStorageSpaceUsage, cluster0,
                 vim.cluster.QueryVsanManagedStorageSpaceUsageSpec(datastoreTypes=["vsan"]))
        cap.call(("vsan", "VsanQueryEntitySpaceUsage-vm"), space_sys.VsanQueryEntitySpaceUsage,
                 cluster0, vim.cluster.VsanSpaceQuerySpec(entityType="vm"))
        identities = cap.call(("vsan", "VsanQueryObjectIdentities"),
                              object_sys.VsanQueryObjectIdentities, cluster0, None, None,
                              True, True, True)
        uuids = [i.uuid for i in (getattr(identities, "identities", None) or [])[:3]]
        if uuids:
            cap.call(("vsan", "VosQueryVsanObjectInformation"), object_sys.VosQueryVsanObjectInformation,
                     cluster0, [vim.cluster.VsanObjectQuerySpec(uuid=u) for u in uuids])
        cap.call(("vsan", "QuerySyncingVsanObjectsSummary"), object_sys.QuerySyncingVsanObjectsSummary,
                 cluster0)
        cap.call(("vsan", "VsanQueryInaccessibleVmSwapObjects"),
                 object_sys.VsanQueryInaccessibleVmSwapObjects, cluster0)
        cap.call(("vsan", "VsanGetCapabilities-cluster"), cap_sys.VsanGetCapabilities, [cluster0])
        cap.call(("vsan", "VsanGetCapabilities-vc"), cap_sys.VsanGetCapabilities, None)
        cap.call(("vsan", "VsanQueryUpgradeStatusEx"), upgrade_sys.VsanQueryUpgradeStatusEx, cluster0)
        cap.call(("vsan", "RetrieveSupportedVsanFormatVersion"),
                 upgrade_sys.RetrieveSupportedVsanFormatVersion, cluster0)
        if stretch_sys:
            cap.call(("vsan", "VSANVcGetPreferredFaultDomain"), stretch_sys.VSANVcGetPreferredFaultDomain,
                     cluster0)
            cap.call(("vsan", "VSANVcGetWitnessHosts"), stretch_sys.VSANVcGetWitnessHosts, cluster0)
            cap.call(("vsan", "VSANVcRetrieveStretchedClusterVcCapability"),
                     stretch_sys.VSANVcRetrieveStretchedClusterVcCapability, cluster0, False)
        if power_sys:
            cap.call(("vsan", "QueryClusterPowerContext"), power_sys.QueryClusterPowerContext, cluster0)
        cap.call(("vsan", "VsanVitQueryIscsiTargetServiceVersion"),
                 iscsi_sys.VsanVitQueryIscsiTargetServiceVersion)
        for name in ("VsanVitGetIscsiTargets", "VsanVitGetIscsiLUNs", "VsanVitGetIscsiInitiatorGroups",
                     "VsanVitGetHomeObject"):
            cap.call(("vsan", name), getattr(iscsi_sys, name), cluster0)
        if file_sys:
            cap.call(("vsan", "VsanClusterQueryFsDomains"), file_sys.VsanClusterQueryFsDomains, None,
                     cluster0)
            cap.call(("vsan", "VsanClusterQueryFileShares"), file_sys.VsanClusterQueryFileShares,
                     vim.vsan.FileShareQuerySpec(), cluster0)
        cap.call(("vsan", "GetVsanVumConfig"), vum_sys.GetVsanVumConfig)
        if remote_sys:
            cap.call(("vsan", "VsanQueryDatastoreSource"), remote_sys.VsanQueryDatastoreSource)
            cap.call(("vsan", "VsanQueryHciMeshDatastores"), remote_sys.VsanQueryHciMeshDatastores)
        if check_sys:
            cap.call(("vsan", "VsanGetResourceCheckStatus"), check_sys.VsanGetResourceCheckStatus,
                     None, cluster0)
        if diag_sys:
            cap.call(("vsan", "VsanQueryNetworkDiagnostics"), diag_sys.VsanQueryNetworkDiagnostics,
                     cluster0)
            cap.call(("vsan", "VsanGetThresholds"), diag_sys.VsanGetThresholds, cluster0)
            cap.call(("vsan", "GetIOTripAnalyzerSchedulerConfig"),
                     diag_sys.GetIOTripAnalyzerSchedulerConfig, cluster0)
        cap.call(("vsan", "QueryClusterDataEfficiencyCapacityState"),
                 disk_sys.QueryClusterDataEfficiencyCapacityState, cluster0)
        cap.call(("vsan", "RetrieveAllFlashCapabilities"), disk_sys.RetrieveAllFlashCapabilities,
                 cluster0)
        for name in ("VsanQueryObjectSnapshotsInfo", "VsanQueryNvmeCriticalWarningStats",
                     "VsanQueryZdomScrubberData", "VsanQueryLSOMwbsize"):
            cap.call(("vsan", f"PhoneHome.{name}"), getattr(phone_sys, name), cluster0)
        cap.call(("vsan", "VsanMassCollector.VsanRetrieveProperties"),
                 mass_sys.VsanRetrieveProperties,
                 [vim.VsanMassCollectorSpec(objects=[cluster0], properties=["name"])])
        for host in list(dict.fromkeys(cluster_hosts + hosts)):
            cap.call(("vsan", f"QueryVsanManagedDisks-{host._moId}"), disk_sys.QueryVsanManagedDisks,
                     host)
            cap.call(("vsan", f"QueryDiskMappings-{host._moId}"), disk_sys.QueryDiskMappings, host)
            if stretch_sys:
                cap.call(("vsan", f"VSANVcIsWitnessHost-{host._moId}"), stretch_sys.VSANVcIsWitnessHost,
                         host)

    cap.note("done", calls=len(cap.calls), faults=len([c for c in cap.calls if not c["ok"]]))
    cap.write(("summary.json",), {
        "about": _json_load(cap, ".", "about"),
        "window_minutes": WINDOW_MINUTES,
        "readonly_privileges": privs,
        "endpoints": endpoints,
        "counts": {k: v for k, v in (json.load(open(cap.path("inv", "INDEX.json"))) or {}).items()},
        "calls": cap.calls,
        "steps": cap.noted,
    })


class _MissingMo:
    """Stands in for a vSAN management object the vCenter does not offer, so
    each call on it is recorded as a fault instead of aborting the walk."""

    def __init__(self, key: str):
        self.key = key

    def __bool__(self):
        return False

    def __getattr__(self, name):
        def absent(*args, **kwargs):
            raise LookupError(f"vSAN management object {self.key} is not offered")
        return absent


def _json_load(cap: Capture, *parts: str):
    full = os.path.join(cap.raw, *parts) + ".json"
    if not os.path.exists(full):
        return None
    with open(full, encoding="utf-8") as fh:
        return json.load(fh)


def _walk_moref(node, submos: dict, known: set, depth: int = 0):
    if depth > 6:
        return
    if isinstance(node, dict):
        ref = node.get("_moref")
        if isinstance(ref, str) and len(node) == 1 and ":" in ref:
            mtype = ref.split(":", 1)[0]
            if mtype not in known and mtype not in (
                    "PropertyCollector", "ContainerView", "SessionManager", "LicenseManager",
                    "CryptoManagerKmip", "HostCryptoManager"):
                submos.setdefault(mtype, set()).add(ref)
            return
        for value in node.values():
            _walk_moref(value, submos, known, depth + 1)
    elif isinstance(node, list):
        for item in node[:50]:
            _walk_moref(item, submos, known, depth + 1)


def http_get(base, path, ctx, timeout, cap: Capture, parts, headers=None, prefix=""):
    url = base + (prefix + path[4:] if (prefix and path.startswith("/api/")) else path)
    req = urllib.request.Request(url, method="GET", headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as resp:
            status, body = resp.status, resp.read()
    except urllib.error.HTTPError as exc:
        status, body = exc.code, exc.read()
    except Exception as exc:                                    # noqa: BLE001
        status, body = None, repr(exc).encode()
    name = path.strip("/").replace("/", "_") or "root"
    if path != "/" and path.endswith("/"):
        name += "_"
    try:
        payload = json.loads(body)
    except Exception:                                           # noqa: BLE001
        payload = {"_raw": body[:2000].decode("utf-8", "replace")}
    cap.write((*parts, f"{name}.json"), {"status": status, "body": payload})
    return {"status": status, "bytes": len(body)}


def _session_token(body: str):
    """/api/session answers a JSON string, /rest/... answers {"value": ...}."""
    try:
        parsed = json.loads(body)
    except ValueError:
        parsed = body
    if isinstance(parsed, dict):
        parsed = parsed.get("value")
    return parsed if isinstance(parsed, str) and parsed else None


def rest_session(base, creds, ctx, timeout):
    """Create a REST session; returns (token_or_None, info) and the prefix used."""
    import base64
    auth = base64.b64encode(f"{creds['VCENTER_USER']}:{creds['VCENTER_PASSWORD']}".encode()).decode()
    for path, prefix in (("/api/session", "/api"), ("/rest/com/vmware/cis/session", "/rest")):
        req = urllib.request.Request(base + path, method="POST",
                                     headers={"Authorization": "Basic " + auth,
                                              "Content-Length": "0"})
        try:
            with urllib.request.urlopen(req, timeout=timeout, context=ctx) as resp:
                body = resp.read().decode("utf-8", "replace").strip()
                return _session_token(body), {"status": resp.status, "prefix": prefix}
        except urllib.error.HTTPError as exc:
            info = {"status": exc.code, "prefix": prefix}
        except Exception as exc:                                # noqa: BLE001
            info = {"error": repr(exc), "prefix": prefix}
        if prefix == "/rest":
            return None, info
    return None, {"status": None, "prefix": "/api"}


def rest_delete(base, token, ctx, timeout, prefix):
    path = "/api/session" if prefix == "/api" else "/rest/com/vmware/cis/session"
    req = urllib.request.Request(base + path, method="DELETE",
                                 headers={"vmware-api-session-id": token})
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as resp:
            return resp.status
    except urllib.error.HTTPError as exc:
        return exc.code
    except Exception as exc:                                    # noqa: BLE001
        return repr(exc)


# --------------------------------------------------------------------------- #
# sanitizing
# --------------------------------------------------------------------------- #
SECRET_KEYS = {"password", "sslThumbprint", "thumbprint", "certificate", "serialNumber",
               "identifierValue", "licenseKey", "model", "vendor"}
USER_KEYS = {"userName", "principal", "user", "login", "owner", "createdBy",
             "lastModifiedUser", "userAgent"}
SENSITIVE_OPTION = re.compile(r"community|passw|secret|token|credential|guestinfo|user-?data",
                              re.IGNORECASE)
STRUCTURAL_KEYS = {"_type", "_moref", "type"}
VERSION_KEY = re.compile(r"(?:[vV]ersion|^build)$")
IPV4 = None  # compiled below
PRESERVE = None


def _compile():
    import re
    global IPV4, PRESERVE
    IPV4 = re.compile(r"(?<!\w)(?<!\d\.)(?:\d{1,3}\.){3}\d{1,3}(?!\w)(?!\.\d)")
    PRESERVE = (re.compile(r"urn:(?:vsan|vim25|pbm|sms|vslm|eam)/\d+(?:\.\d+)+"),
                re.compile(r'versionId="[^"]+"'),
                re.compile(r"\d+(?:\.\d+){2,}-\d*vmw\.[\w.]+"),
                re.compile(r"\d+(?:\.\d+){3}(?=\s+build\b)"),
                re.compile(r"(?<=<version>)[^<]*"))


def sanitize_tree(raw: str, san: str, extra_names: dict | None = None) -> dict:
    """Rewrite raw/ into san/ with stable masks; returns the mapping used."""
    import re
    _compile()
    if os.path.isdir(san):
        for root, _, files in os.walk(san):
            for fn in files:
                os.remove(os.path.join(root, fn))
    os.makedirs(san, exist_ok=True)
    names: dict[str, list[str]] = {}
    for fname, group in (("inv/HostSystem.json", "host"), ("inv/ClusterComputeResource.json", "cluster"),
                         ("inv/ComputeResource.json", "cluster"),
                         ("inv/Datacenter.json", "datacenter"),
                         ("inv/VmwareDistributedVirtualSwitch.json", "vds"),
                         ("inv/DistributedVirtualSwitch.json", "vds"),
                         ("inv/DistributedVirtualPortgroup.json", "portgroup"),
                         ("inv/Network.json", "portgroup"), ("inv/Folder.json", "folder"),
                         ("inv/ResourcePool.json", "pool"),
                         ("inv/Datastore.json", "datastore"), ("inv/VirtualMachine.json", "vm")):
        full = os.path.join(raw, fname)
        if not os.path.exists(full):
            continue
        try:
            recs = json.load(open(full, encoding="utf-8"))
        except Exception:                                       # noqa: BLE001
            continue
        bucket = names.setdefault(group, [])
        for rec in (recs if isinstance(recs, list) else []):
            if not isinstance(rec, dict):
                continue
            value = (rec.get("props") or {}).get("name")
            if isinstance(value, str) and value and value not in bucket:
                bucket.append(value)
    # host names also appear in the vSAN node information and health records
    for sub, fname, path in (("vsan", "VsanPerfQueryNodeInformation", "hostname"),
                             ("vsan", "VsanQueryVcClusterSmartStatsSummary", "hostname")):
        full = os.path.join(raw, sub, fname)
        if not os.path.exists(full):
            continue
        try:
            recs = json.load(open(full, encoding="utf-8"))
        except Exception:                                       # noqa: BLE001
            continue
        bucket = names.setdefault("host", [])
        for rec in (recs if isinstance(recs, list) else [recs]):
            if isinstance(rec, dict) and isinstance(rec.get(path), str):
                value = rec[path]
                if value not in bucket:
                    bucket.append(value)
    standard = {"vm", "host", "datastore", "network", "Datacenters", "Resources",
                "Discovered virtual machine", "VM Network", "Management Network"}
    mapping: dict[str, str] = {}
    prefixes = {"host": "esx", "cluster": "cluster-", "datacenter": "dc-", "datastore": "ds-",
                "vm": "vm-", "vds": "vds-", "portgroup": "pg-", "folder": "folder-",
                "pool": "pool-"}
    for group, values in sorted(names.items()):
        for index, value in enumerate(sorted(v for v in values if v and v not in standard), 1):
            mapping[value] = f"{prefixes[group]}{index:02d}"
    if not mapping:
        mapping = {"vcenter": "vcenter.lab.invalid"}
    for name, mask in (extra_names or {}).items():
        if len(name) >= 3 and not IPV4.fullmatch(name):
            mapping.setdefault(name, mask)
    salt = uuid.uuid4()

    def digest(text: str, size: int = 8) -> str:
        return hashlib.sha256((str(salt) + text).encode()).hexdigest()[:size]

    keys = sorted((k for k in mapping if len(k) >= 3), key=len, reverse=True)
    name_re = (re.compile(r"(?<![A-Za-z0-9_-])(" + "|".join(re.escape(k) for k in keys) + r")"
                          r"(?![A-Za-z0-9_-])") if keys else None)
    uuid_re = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}"
                         r"-[0-9a-fA-F]{12}\b")
    mac_re = re.compile(r"\b(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}\b")
    pem_re = re.compile(r"-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----", re.S)
    thumb_re = re.compile(r"\b(?:[0-9A-Fa-f]{2}:){19,63}[0-9A-Fa-f]{2}\b")
    license_re = re.compile(r"\b[A-Z0-9]{5}(?:-[A-Z0-9]{5}){4}\b")
    hex_id_re = re.compile(r"^[0-9a-fA-F]{32,}$")

    def text(value: str) -> str:
        holes: list[str] = []
        for pattern in PRESERVE:
            value = pattern.sub(lambda m: (holes.append(m.group(0)) or f"\x00{len(holes)-1}\x00"),
                                value)
        value = pem_re.sub("<pem-redacted>", value)
        value = thumb_re.sub(lambda m: f"<thumbprint-{digest(m.group(0))}>", value)
        value = license_re.sub("<license-redacted>", value)
        value = uuid_re.sub(lambda m: str(uuid.uuid5(salt, m.group(0).lower())), value)
        value = mac_re.sub(lambda m: "02:00:00:" + ":".join(
            digest(m.group(0), 6)[k:k + 2] for k in (0, 2, 4)), value)
        value = IPV4.sub(lambda m: m.group(0) if m.group(0).startswith(("127.", "0.0.0.0", "255."))
                         else "10." + ".".join(str(int(digest(m.group(0), 6)[k:k + 2], 16))
                                                for k in (0, 2, 4)), value)
        if name_re:
            value = name_re.sub(lambda m: mapping[m.group(1)], value)
        return re.sub(r"\x00(\d+)\x00", lambda m: holes[int(m.group(1))], value)

    def walk_json(value, key=None):
        if isinstance(value, dict):
            out = {k: walk_json(v, k) for k, v in value.items()}
            option, setting = value.get("key"), value.get("value")
            if isinstance(option, str) and isinstance(setting, str) and SENSITIVE_OPTION.search(option):
                out["value"] = f"<option-{digest(setting)}>"
            return out
        if isinstance(value, list):
            return [walk_json(v, key) for v in value]
        if not isinstance(value, str):
            return value
        if key in STRUCTURAL_KEYS or (key and VERSION_KEY.search(key)):
            return value
        if key in SECRET_KEYS and len(value) > 3 and not value.startswith("<"):
            return f"<{key}-{digest(value)}>"
        if key == "key" and hex_id_re.match(value):
            return f"<session-key-{digest(value)}>"
        if key in USER_KEYS and not IPV4.fullmatch(value):
            if value in mapping:
                return mapping[value]
            return f"user-{digest(value, 6)}"
        return text(value)

    count = 0
    for root, _, files in os.walk(raw):
        rel = os.path.relpath(root, raw)
        target = os.path.join(san, rel) if rel != "." else san
        os.makedirs(target, exist_ok=True)
        for fname in sorted(files):
            src = os.path.join(root, fname)
            dst = os.path.join(target, fname)
            if fname.endswith(".json"):
                try:
                    obj = json.load(open(src, encoding="utf-8"))
                except Exception:                               # noqa: BLE001
                    obj = None
                if obj is not None:
                    with open(dst, "w", encoding="utf-8") as fh:
                        json.dump(walk_json(obj), fh)
                    count += 1
                    continue
            with open(src, encoding="utf-8", errors="replace") as fh:
                body = fh.read()
            with open(dst, "w", encoding="utf-8") as fh:
                fh.write(text(body))
            count += 1
    info = {"files": count, "names": len(mapping)}
    with open(os.path.join(san, "SANITIZE-INFO.json"), "w", encoding="utf-8") as fh:
        json.dump(info, fh)
        fh.write("\n")
    return info


# --------------------------------------------------------------------------- #
# call plan (--dry-run)
# --------------------------------------------------------------------------- #
def call_plan() -> list[str]:
    plan = [
        "step 0  Login, RetrieveContent (about), currentSession, read-only privilege check",
        "step 1  /sdk ContainerView of all ManagedEntity; RetrievePropertiesEx all=true per type "
        "(paged, fallback per object then per property); sub managed objects behind configManager.*; "
        "every ServiceContent manager except licenseManager and cryptoManager",
        "step 1b /sdk read-only methods: ServiceInstance.CurrentTime; PerformanceManager "
        "QueryPerfProviderSummary and QueryAvailablePerfMetric (interval 300, plus 20 for host and "
        "VM) for one cluster, host, VM, datastore, resource pool and vDS; QueryPerf normal and csv "
        f"for one host and one VM over a {WINDOW_MINUTES}-minute window; HostVsanSystem "
        "QueryDisksForVsan and QueryHostStatus per host; HostVsanInternalSystem "
        "QueryPhysicalVsanDisks and QueryVsanObjectUuidsByFilter(limit 10) per host; "
        "HostSystem RetrieveHardwareUptime and QueryHostConnectionInfo; "
        "ClusterComputeResource RetrieveDasAdvancedRuntimeInfo; EnvironmentBrowser "
        "QueryConfigOptionDescriptor, QueryConfigTarget, QueryConfigOptionEx; "
        "DistributedVirtualSwitch.FetchDVPorts; AlarmManager.GetAlarm; "
        f"EventManager.QueryEvents (last {WINDOW_MINUTES} minutes)",
        "step 1c /pbm PbmRetrieveServiceContent, PbmQueryProfile(STORAGE), PbmRetrieveContent, "
        "PbmFetchCapabilityMetadata, PbmFetchResourceType, PbmFetchVendorInfo, "
        "PbmQueryDefaultRequirementProfile per datastore (max 8), PbmQueryAssociatedProfiles, "
        "PbmQueryAssociatedEntity; /sms/sdk QueryStorageManager and QueryProvider; "
        "/vslm/sdk RetrieveContent and VslmListVStorageObjectForSpec(50); /eam/sdk QueryAgency "
        "and per-agency QueryConfig/QueryAgent (max 10)",
        f"step 2  plain HTTP GET, {len(PLAIN_GETS)} paths: " + ", ".join(PLAIN_GETS),
        f"step 3  REST: POST /api/session (or /rest/com/vmware/cis/session), then GET "
        f"{len(REST_COLLECTIONS)} collections " + ", ".join(REST_COLLECTIONS)
        + " plus per-id paths " + ", ".join(p for group in REST_BY_ID.values() for p in group)
        + "; DELETE /api/session",
        f"step 4  /vsanHealth read-only methods: VsanClusterGetConfig, VsanClusterGetRuntimeStats, "
        f"VsanClusterGetClaimedCapacity, VsanQueryClusterDrsStats; performance service "
        f"VsanPerfQueryNodeInformation, VsanPerfGetSupportedEntityTypes, "
        f"VsanPerfGetAggregatedEntityTypes, VsanPerfGetSupportedDiagnosticExceptions, "
        f"VsanPerfQueryStatsObjectInformation, VsanPerfQueryClusterHealth, VsanPerfQueryTimeRanges, "
        f"QueryRemoteServerClusters, and VsanPerfQueryPerf for every family ({len(PERF_FAMILIES)} "
        f"known plus any advertised): {', '.join(PERF_FAMILIES)}; health system 12 readers with "
        f"VsanQueryVcClusterHealthSummary(fetchFromCache=true); space report 3; object system 4; "
        f"capability 2; upgrade 2; stretched cluster 4; power 1; iSCSI target 5; file service 2; "
        f"PhoneHome VsanQueryObjectSnapshotsInfo, VsanQueryNvmeCriticalWarningStats, "
        f"VsanQueryZdomScrubberData, VsanQueryLSOMwbsize; VUM 1; remote datastore 2; resource check 1; diagnostics 3; disk management 2 plus "
        f"QueryVsanManagedDisks, QueryDiskMappings and VSANVcIsWitnessHost per host; "
        f"VsanMassCollector.VsanRetrieveProperties",
        "step 6  Logout and DELETE /api/session",
        "output  one tarball of the sanitized tree (names, uuids, ips, users, hardware "
        "models masked consistently) with summary.json, what answered; raw/ stays on disk "
        "only with --zip",
    ]
    return plan


# --------------------------------------------------------------------------- #
def main() -> int:
    ap = argparse.ArgumentParser(description="read-only vCenter API walk",
                                 formatter_class=argparse.RawDescriptionHelpFormatter,
                                 epilog=__doc__)
    ap.add_argument("target", nargs="?", help="'vcsim' or a base URL such as https://host:443")
    ap.add_argument("--out", default=".", help="directory for the tarball (default: cwd)")
    ap.add_argument("--dry-run", action="store_true", help="print the call plan and exit")
    ap.add_argument("--zip", action="store_true", help="keep the work directory (raw/ and sanitized/) on disk")
    args = ap.parse_args()

    if args.dry_run:
        for line in call_plan():
            print(line)
        return 0
    if not args.target:
        ap.error("a target is required: 'vcsim' or a base URL")

    base = args.target
    if base == "vcsim":
        base = os.environ.get("VCENTER_URL", "https://127.0.0.1:8443")
    if "//" not in base:
        base = f"https://{base}"
    base = base.rstrip("/")
    creds = read_credentials()

    os.makedirs(args.out, exist_ok=True)
    work = tempfile.mkdtemp(prefix=f"vcs-walk-{dt.datetime.now(dt.timezone.utc):%Y%m%dT%H%M%SZ}-",
                            dir=args.out)
    raw = os.path.join(work, "raw")
    san = os.path.join(work, "sanitized")
    os.makedirs(raw)
    cap = Capture(raw)
    import time
    cap.t0 = time.time()
    try:
        walk(base, creds, cap)
    finally:
        try:
            info = sanitize_tree(raw, san, {urlsplit(base).hostname or "": "vcenter.lab.invalid",
                                            creds["VCENTER_USER"]: "login-user"})
            tar = os.path.join(args.out, os.path.basename(work) + ".tar.gz")
            with tarfile.open(tar, "w:gz") as tf:
                tf.add(san, arcname=os.path.basename(work))
        finally:
            if not args.zip:
                shutil.rmtree(work)
        print(json.dumps({"tarball": tar, "sanitized": info,
                          "calls": len(cap.calls),
                          "faults": len([c for c in cap.calls if not c["ok"]])}, indent=1))
    return 0


if __name__ == "__main__":
    sys.exit(main())
