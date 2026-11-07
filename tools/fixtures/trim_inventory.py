#!/usr/bin/env python3
"""Third pass for the JSON inventory capture: trim and mask a small set of
records from an already-sanitized PropertyCollector dump.

    python3 -I tools/fixtures/trim_inventory.py --in <sanitized> --out <dir> \
        --map <mapping.json>

<sanitized> is the output of the identity pass (pass 1, outside this
repository); its records live in <sanitized>/inv/<Type>.json as arrays of
{"_moref": "Type:id", "props": {...}}. Data objects carry "_type" and
references are {"_moref": "Type:id"}. Only the sanitized set is read, never the
raw capture.

The mapping file is NOT in this repository and must not be committed (see
mask_hardware.py for its shape). This script reuses mask_hardware.Masker for
the string-level hardware rules (model strings, t10.NVMe canonical names, vDS
uuids) and adds JSON-aware rules. Only string VALUES are rewritten, never keys;
numeric PCI ids are set to 65535.

What it does, on top of the hardware rules:
  * keeps a fixed set of records and properties per type (see SELECT)
  * drops alarm/permission/task lists, option/feature lists, PCI device and
    CPU feature lists, certificates, thumbprints, checksums, and any string
    that looks like a PEM block
  * guest host names -> guest-<4 hex>; VSPHERE.LOCAL -> SSO.INVALID;
    WLD0n.DOMAIN -> WLD0X.INVALID; _001c/ FCD directories -> _fcd/;
    any leftover sentania.* domain -> lab.invalid
  * any IPv4 outside the placeholder ranges -> 10.255.x.y; any IPv6 other
    than :: and ::1 -> fd00::/fe80:: plus a hash; MACs outside 02:00:00 ->
    02:00:00:xx:xx:xx
  * firmware/BIOS revisions, serials, NVMe qualified names, asset/service
    tags -> salted hashes (random per-run salt, never written)
A final scan (the mask_hardware vendor list, the catalog_test lab host-name
and NQN checks, the mapping's real strings, non-placeholder IPs) fails the
run if anything got through.
"""
from __future__ import annotations

import argparse
import copy
import importlib.util
import ipaddress
import json
import os
import re
import sys
from collections import Counter

sys.dont_write_bytecode = True  # no __pycache__ next to the tools
HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("mask_hardware", os.path.join(HERE, "mask_hardware.py"))
mask_hardware = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mask_hardware)

# Keys removed wherever they appear.
DROP_ANYWHERE = {
    "declaredAlarmState", "triggeredAlarmState", "availableField", "permission",
    "disabledMethod", "recentTask", "certificate", "sslThumbprintData",
    "sslThumbprintInfo", "sslCertificate", "sslThumbprint", "hostConfigCheckSum",
    "scriptCheckSum", "descriptionTreeCheckSum", "option", "optionDef",
    "featureCapability", "maskedFeatureCapability", "cpuFeature", "pciDevice",
}
DROP_KEY_RE = re.compile(r"thumbprint|certificate", re.IGNORECASE)

# Per-type selection: (file, record filter, props kept, sub-key allowlists).
HOST_CONFIG = ["network", "storageDevice", "vsanHostConfig", "fileSystemVolume", "dateTimeInfo",
               "product", "lockdownMode", "hyperThread", "virtualNicManagerInfo", "multipathState",
               "datastorePrincipal", "adminDisabled", "wakeOnLanCapable", "vmotion",
               "systemResources", "host"]
VM_CONFIG = ["name", "uuid", "instanceUuid", "version", "guestId", "guestFullName", "files",
             "hardware", "template", "createDate", "datastoreUrl", "tools", "bootOptions",
             "firmware", "cpuAllocation", "memoryAllocation"]
NET_PROPS = ["name", "parent", "config", "summary", "runtime", "capability", "portgroup",
             "uuid", "key", "host", "vm"]

# Strings keyed by these names are hashed (hardware identity).
HASH_KEYS = {"revision", "firmware", "fwVersion", "firmwareVersion", "biosVersion",
             "serialNumber", "serial", "sn", "assetTag", "serviceTag", "identifierValue"}
# values under HASH_KEYS that are vSphere enums or placeholders, not identity
HASH_KEEP = {"", "N/A", "None", "unknown", "efi", "bios", "Default string"}
PCI_KEYS = ("vendorId", "subVendorId", "deviceId", "subDeviceId")
# t10 names of SATA devices: 8-char vendor, 40-char model, 20-char serial
T10_ATA_RE = re.compile(r"t10\.ATA_____([A-Za-z0-9]+(?:_[A-Za-z0-9]+)*_+)([A-Za-z0-9]{1,20})(?![A-Za-z0-9])")
# VM file names derived from the real VM name (the identity pass maps VM names
# but not the files named after them): base name -> vmfile-<hash>
VMFILE_RE = re.compile(r"(?<=/)([A-Za-z0-9][A-Za-z0-9._ -]*?)((?:_\d+)?(?:-(?:flat|ctk|delta|sesparse|digest|[0-9]{6}))*"
                       r"\.(?:vmdk|vmx|vmxf|vmsd|nvram|vswp|vmem|vmss|log|hlog|vmtx))\b")
VMFILE_KEEP = re.compile(r"^(?:vm-\d+|vmfile-[0-9a-f]+|[0-9a-f]{32}|[0-9a-f-]{36}|vmware(?:-\d+)?|vmx-.*)$")

PLACEHOLDER_V4 = re.compile(r"^(?:10\.255\.|0\.|127\.|255\.|224\.|169\.254\.)")
IPV4_RE = re.compile(r"(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?![\d.])")
MAC_RE = re.compile(r"(?<![0-9A-Fa-f:])([0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5})(?![0-9A-Fa-f:])")
PEM_RE = re.compile(r"-----BEGIN [A-Z0-9 ]+-----")
HOSTS_RE = re.compile(r"\b[a-z0-9-]+\.(?:sentania|local|lan|home|internal|corp|int)\b")
DASHED_IP_RE = re.compile(r"\b(?:172|192|10)-\d{1,3}-\d{1,3}-\d{1,3}\b")


class Trimmer:
    def __init__(self, mapping: dict):
        self.m = mask_hardware.Masker(mapping, {})
        self.real = list(mapping["disk_models"]) + list(mapping["strings"])
        self.counts: Counter = Counter()

    # --- string rules -----------------------------------------------------
    def ipv6(self, s: str) -> str | None:
        if ":" not in s or not re.fullmatch(r"[0-9A-Fa-f:]+(?:/\d+)?", s) or s.count(":") < 2:
            return None
        addr, _, plen = s.partition("/")
        try:
            ip = ipaddress.IPv6Address(addr)
        except ValueError:
            return None
        if ip in (ipaddress.IPv6Address("::"), ipaddress.IPv6Address("::1")):
            return None
        h = self.m.hsh(addr.lower(), 12)
        pre = "fe80::" if ip.is_link_local else "fd00::"
        new = pre + ":".join([h[0:4], h[4:8], h[8:12]])
        self.counts["ipv6 masked"] += 1
        return new + ("/" + plen if plen else "")

    def v4(self, mt: re.Match) -> str:
        ip = mt.group(0)
        if PLACEHOLDER_V4.match(ip) or ip.startswith("255.") or ip.endswith(".0") and ip.startswith("255"):
            return ip
        parts = [int(p) for p in mt.groups()]
        if parts[0] >= 240 or all(p in (0, 255) for p in parts):  # masks, broadcast
            return ip
        h = self.m.hsh(ip, 4)
        self.counts["ipv4 masked"] += 1
        return f"10.255.{int(h[:2], 16)}.{int(h[2:], 16)}"

    def mac(self, mt: re.Match) -> str:
        v = mt.group(1)
        if v.lower().startswith("02:00:00:") or v in ("00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff"):
            return v
        h = self.m.hsh(v.lower(), 6)
        self.counts["mac masked"] += 1
        return "02:00:00:" + ":".join([h[0:2], h[2:4], h[4:6]])

    def t10_ata(self, mt: re.Match) -> str:
        field, serial = mt.group(1), mt.group(2)
        self.counts["t10.ATA names masked"] += 1
        return ("t10.ATA_____" + self.m.pad_like("SIM_SATA_SSD_Model_E", field, "_")
                + self.m.hsh(serial, len(serial)).upper())

    def vmfile(self, mt: re.Match) -> str:
        base = mt.group(1)
        if VMFILE_KEEP.match(base):
            return mt.group(0)
        return "vmfile-" + self.m.hsh(base, 6) + mt.group(2)

    def guest_host(self, v: str) -> str:
        if not v:
            return v
        self.counts["guest host names"] += 1
        return "guest-" + self.m.hsh(v.lower(), 4)

    def string(self, key: str | None, v: str, path: str):
        if PEM_RE.search(v):
            self.counts["PEM strings dropped"] += 1
            return None
        if key in PCI_KEYS:
            self.counts["pci ids masked"] += 1
            return "0xffff" if v.lower().startswith("0x") else "65535"
        if path.endswith("biosInfo.vendor") and v:
            self.counts["bios vendor masked"] += 1
            return "SIM BIOS Vendor"
        if key == "annotation" and v:
            self.counts["annotations masked"] += 1
            return "sim annotation"
        if key in HASH_KEYS and v not in HASH_KEEP:
            self.counts[f"hashed {key}"] += 1
            return "SIM" + self.m.hsh(v, 12).upper()
        if v.startswith("nqn."):
            self.counts["nqn masked"] += 1
            return "nqn.2000-01.invalid:nvme:sim:" + self.m.hsh(v, 14)
        orig = v
        v = T10_ATA_RE.sub(self.t10_ata, v)
        v = self.m.mask(v)
        if v != orig:
            self.counts["hardware strings masked"] += 1
        new = re.sub(r"VSPHERE\.LOCAL", "SSO.INVALID", v)
        new = re.sub(r"vsphere\.local", "sso.invalid", new, flags=re.IGNORECASE)
        new = re.sub(r"WLD0\d\.DOMAIN", "WLD0X.INVALID", new)
        new = re.sub(r"wld0\d\.domain", "wld0x.invalid", new, flags=re.IGNORECASE)
        if new != v:
            self.counts["identity domains masked"] += 1
        v = new
        new = re.sub(r"(^|[\s/\]])_001c(?=/)", r"\1_fcd", v)
        if new != v:
            self.counts["fcd dirs renamed"] += 1
        v = new
        new = re.sub(r"(?:[A-Za-z0-9-]+\.)*sentania\.[a-z]+", "lab.invalid", v, flags=re.IGNORECASE)
        if new != v:
            self.counts["lab domains masked"] += 1
        v = new
        new = VMFILE_RE.sub(self.vmfile, v)
        if new != v:
            self.counts["vm file names renamed"] += 1
        v = new
        if key == "remotePath":
            # an NFS export path names the NAS layout; keep only the shape
            new = "/export/" + self.m.hsh(v, 8)
            if new != v:
                self.counts["nfs export paths masked"] += 1
            v = new
        v6 = self.ipv6(v)
        if v6 is not None:
            return v6
        v = IPV4_RE.sub(self.v4, v)
        v = MAC_RE.sub(self.mac, v)
        # vendor names the mapping does not cover (BIOS family, EVC mode keys
        # such as "<vendor>-zen5", cpuid feature names): generic placeholder
        new = mask_hardware.VENDORS.sub(lambda mt: "simvendor" if mt.group(0).islower() else "SIM-VENDOR", v)
        new = re.sub(r"(?i)\bzen(\d)", r"gen\1", new)
        if new != v:
            self.counts["vendor names generalized"] += 1
        return new

    # --- tree walk --------------------------------------------------------
    def walk(self, node, path: str = "", key: str | None = None, guest: bool = False):
        if isinstance(node, dict):
            out = {}
            for k, v in node.items():
                if k in DROP_ANYWHERE or DROP_KEY_RE.search(k):
                    self.counts[f"dropped {k}"] += 1
                    continue
                if k in ("vendorId", "subVendorId", "deviceId", "subDeviceId") and isinstance(v, int):
                    out[k] = 65535
                    self.counts["pci ids masked"] += 1
                    continue
                if guest and k == "hostName" and isinstance(v, str):
                    out[k] = self.guest_host(v)
                    continue
                nv = self.walk(v, f"{path}.{k}" if path else k, k, guest)
                if nv is None and isinstance(v, str):
                    continue
                out[k] = nv
            return out
        if isinstance(node, list):
            res = [self.walk(x, path, key, guest) for x in node]
            return [x for x in res if x is not None]
        if isinstance(node, str):
            return self.string(key, node, path)
        return node


def pick(props: dict, keep: list[str]) -> dict:
    return {k: copy.deepcopy(props[k]) for k in keep if k in props}


def restrict(d: dict | None, keep: list[str]) -> dict | None:
    if not isinstance(d, dict):
        return d
    out = {k: d[k] for k in ["_type"] + keep if k in d}
    return out


def ref_type(r) -> str:
    return (r or {}).get("_moref", ":").split(":")[0]


def select(src: str) -> dict[str, list]:
    def load(t):
        with open(os.path.join(src, "inv", t + ".json"), encoding="utf-8") as fh:
            return json.load(fh)

    out = {}
    cl = load("ClusterComputeResource")
    out["ClusterComputeResource"] = [
        {"_moref": r["_moref"], "props": pick(r["props"], ["name", "parent", "configurationEx", "summary",
                                                             "summaryEx", "host", "datastore", "network",
                                                             "resourcePool"])}
        for r in cl]
    out["Datastore"] = [
        {"_moref": r["_moref"], "props": pick(r["props"], ["name", "parent", "info", "summary", "capability",
                                                             "iormConfiguration", "host", "vm"])}
        for r in load("Datastore")]
    hosts = [r for r in load("HostSystem") if ref_type(r["props"].get("parent")) == "ClusterComputeResource"]
    h = hosts[0]
    hp = pick(h["props"], ["name", "parent", "summary", "runtime", "hardware", "config", "configManager",
                           "datastore", "network", "vm"])
    hp["config"] = restrict(hp.get("config"), HOST_CONFIG)
    out["HostSystem"] = [{"_moref": h["_moref"], "props": hp}]
    vms = [r for r in load("VirtualMachine")
           if (r["props"].get("runtime") or {}).get("powerState") == "poweredOn"
           and not (r["props"].get("config") or {}).get("template")][:2]
    recs = []
    for r in vms:
        p = pick(r["props"], ["name", "parent", "config", "guest", "runtime", "summary", "resourceConfig",
                              "datastore", "network", "resourcePool"])
        p["config"] = restrict(p.get("config"), VM_CONFIG)
        if isinstance(p.get("guest"), dict):
            # guest disk mounts and ip stacks name the appliance's own
            # directories and mounts; the shape (one entry) is enough
            p["guest"] = {k: v for k, v in p["guest"].items() if k not in ("screen", "guestDetailedData", "ipStack")}
            disks = p["guest"].get("disk")
            if isinstance(disks, list) and disks:
                root = dict(disks[0])
                root["diskPath"] = "/"
                root.pop("mappings", None)
                p["guest"]["disk"] = [root]
        recs.append({"_moref": r["_moref"], "props": p})
    out["VirtualMachine"] = recs
    for t in ("VmwareDistributedVirtualSwitch", "DistributedVirtualPortgroup"):
        out[t] = [{"_moref": r["_moref"], "props": pick(r["props"], NET_PROPS)} for r in load(t)]
    return out


def shrink_host(rec: dict, counts: Counter):
    """Size fallback for HostSystem: drop the plug-store topology and keep one
    candidate vnic per netConfig."""
    cfg = rec["props"].get("config") or {}
    sd = cfg.get("storageDevice") or {}
    if "plugStoreTopology" in sd:
        del sd["plugStoreTopology"]
        counts["dropped plugStoreTopology"] += 1
    vnm = cfg.get("virtualNicManagerInfo") or {}
    for nc in vnm.get("netConfig") or []:
        cands = nc.get("candidateVnic")
        if isinstance(cands, list) and len(cands) > 1:
            counts["dropped candidateVnic entries"] += len(cands) - 1
            nc["candidateVnic"] = cands[:1]


def walk_guest(tr: Trimmer, rec: dict) -> dict:
    """VM records: host names under guest and summary.guest are masked."""
    p = rec["props"]
    out = {}
    for k, v in p.items():
        if k == "guest":
            out[k] = tr.walk(v, k, k, guest=True)
        elif k == "summary" and isinstance(v, dict):
            s = {}
            for sk, sv in v.items():
                if sk in DROP_ANYWHERE or DROP_KEY_RE.search(sk):
                    tr.counts[f"dropped {sk}"] += 1
                    continue
                s[sk] = tr.walk(sv, "summary." + sk, sk, guest=(sk == "guest"))
            out[k] = s
        else:
            out[k] = tr.walk(v, k, k)
    return {"_moref": rec["_moref"], "props": out}


def scan(text: str, real: list[str]) -> list[str]:
    hits = mask_hardware.scan(text)
    hits += HOSTS_RE.findall(text)
    hits += DASHED_IP_RE.findall(text)
    hits += [m.group(0) for m in IPV4_RE.finditer(text) if not PLACEHOLDER_V4.match(m.group(0))
             and int(m.group(1)) < 240 and not all(int(g) in (0, 255) for g in m.groups())]
    hits += re.findall(r"(?i)sentania|vsphere\.local|wld0\d\.domain|_001c/|vmware_soap_session=", text)
    hits += ["<mapped string>" for r in real if r in text]
    hits += PEM_RE.findall(text)
    hits += re.findall(r"t10\.ATA_____(?!SIM_)\w{8}", text)
    hits += [m.group(0) for m in VMFILE_RE.finditer(text) if not VMFILE_KEEP.match(m.group(1))]
    hits += re.findall(r'"(?:vendorId|subVendorId|deviceId|subDeviceId)": "?(?!0xffff|65535)\w+', text)
    return hits


README = """# Live inventory capture: vCenter 9.1.1, trimmed

A small, trimmed slice of a read-only PropertyCollector dump of a lab vCenter
(9.1.1 build 25784172; one five-host vSAN ESA cluster plus one standalone
host), captured 2026-11-06 with a read-only probe that retrieved every
property of every inventory object. The records keep the capture's shape:
each file is a JSON array of `{"_moref": "Type:id", "props": {...}}`, data
objects carry `"_type"`, and references are `{"_moref": "Type:id"}`.
`internal/sim/inventory_fixture_test.go` compares the property paths here
with what the simulator's `/sdk` serves.

| File | Records | Properties kept |
|---|---|---|
| `ClusterComputeResource.json` | the vSAN cluster | name, parent, configurationEx, summary, summaryEx, host, datastore, network, resourcePool |
| `Datastore.json` | all 8 datastores (vSAN and local VMFS) | name, parent, info, summary, capability, iormConfiguration, host, vm |
| `HostSystem.json` | the first cluster host | name, parent, summary, runtime, hardware, a subset of config (network, storageDevice, vsanHostConfig, fileSystemVolume, dateTimeInfo, product, ...), configManager, datastore, network, vm |
| `VirtualMachine.json` | the first two powered-on, non-template VMs | name, parent, a subset of config, guest (no screen/guestDetailedData), runtime, summary, resourceConfig, datastore, network, resourcePool |
| `VmwareDistributedVirtualSwitch.json`, `DistributedVirtualPortgroup.json` | all | name, parent, config, summary, runtime, capability, portgroup, uuid, key, host, vm (where present) |
| `SANITIZE-INFO.json` | | counts of what this pass dropped and masked |

## Sanitization

Three passes; the raw capture is not in any repository.

1. Identity (run against the raw capture, outside this repository): vCenter,
   host, cluster, datastore, VM, vDS and portgroup names to numbered
   placeholders (`esx01`, `vm-07`, `ds-08`, `lab.invalid`); UUIDs remapped
   through UUID v5 under a per-run random namespace (joins preserved); IPv4 to
   `10.255.x.y` and MACs to `02:00:00:xx:xx:xx` by hash; session ids
   redacted.
2. Hardware, the rules of `tools/fixtures/mask_hardware.py` applied to JSON
   string values: disk, controller, NIC, CPU and server model strings
   replaced by placeholders, including the hex-encoded model inside
   `t10.NVMe____<model>____<serial>` canonical names; PCI vendor/device ids
   set to 65535; firmware revisions, BIOS versions, serials and NVMe
   qualified names replaced by hashes with a random per-run salt.
3. Trim (`tools/fixtures/trim_inventory.py`, this set): only the records and
   properties above; alarm, permission, task, option, feature, PCI device and
   CPU feature lists dropped; certificates, thumbprints, config checksums and
   PEM blocks dropped; guest host names to `guest-<hash>`; SSO and workload
   domain names to `SSO.INVALID` / `WLD0X.INVALID`; first-class-disk
   directories `_001c/` to `_fcd/`; NFS export paths to `/export/<hash>`;
   guest disk mounts reduced to the root file system and guest IP stacks
   dropped; leftover lab DNS search domains to
   `lab.invalid`; any IPv4 outside `10.255.0.0/16`, any IPv6 and any MAC
   outside `02:00:00` re-masked by hash. The script's scan (vendor names, lab
   host names, NVMe qualified names, non-placeholder addresses, the private
   mapping's real strings) must come back clean, and
   `internal/catalog/catalog_test.go` re-checks every file here on each CI run.

Known residue, by design: hex VMDK file names, VMware software version and
build strings, driver names and versions, and the vCenter MoRef ids.
"""


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--in", dest="src", required=True, help="sanitized capture directory (not raw/)")
    ap.add_argument("--out", dest="dst", required=True)
    ap.add_argument("--map", required=True, help="private mapping file (never committed)")
    ap.add_argument("--max-bytes", type=int, default=1_500_000)
    args = ap.parse_args()
    if os.path.basename(os.path.normpath(args.src)) == "raw":
        print("refusing to read the raw capture", file=sys.stderr)
        return 2
    with open(args.map, encoding="utf-8") as fh:
        mapping = json.load(fh)
    tr = Trimmer(mapping)
    sel = select(args.src)
    out = {}
    for t, recs in sel.items():
        res = []
        for r in recs:
            res.append(walk_guest(tr, r) if t == "VirtualMachine" else
                       {"_moref": r["_moref"], "props": tr.walk(r["props"])})
        out[t] = res
        tr.counts[f"records {t}"] = len(res)

    def size():
        return sum(len(json.dumps(v, indent=1)) for v in out.values())

    if size() > args.max_bytes:
        for r in out["HostSystem"]:
            shrink_host(r, tr.counts)
    os.makedirs(args.dst, exist_ok=True)
    hits = []
    total = 0
    for t, recs in out.items():
        text = json.dumps(recs, indent=1, ensure_ascii=False) + "\n"
        hits += [(t, h) for h in scan(text, tr.real)]
        with open(os.path.join(args.dst, t + ".json"), "w", encoding="utf-8") as fh:
            fh.write(text)
        total += len(text)
        print(f"{t}.json: {len(recs)} records, {len(text)} bytes")
    info = {"source": "vCenter 9.1.1 build 25784172, captured 2026-11-06",
            "pass": "trim_inventory.py",
            "counts": dict(sorted(tr.counts.items()))}
    with open(os.path.join(args.dst, "SANITIZE-INFO.json"), "w", encoding="utf-8") as fh:
        json.dump(info, fh, indent=1)
        fh.write("\n")
    with open(os.path.join(args.dst, "README.md"), "w", encoding="utf-8") as fh:
        fh.write(README)
    print(f"total {total} bytes")
    if hits:
        print("scan FAILED; unmasked strings remain:", file=sys.stderr)
        for t, h in hits[:40]:
            print(f"  {t}: {h!r}", file=sys.stderr)
        return 1
    print("scan clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
