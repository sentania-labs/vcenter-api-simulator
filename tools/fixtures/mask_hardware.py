#!/usr/bin/env python3
"""Second sanitization pass: mask hardware identity in an already-sanitized
capture set.

The first pass (identity sanitization: host names, domains, users, UUIDs,
IPs, MACs, serial suffixes, session cookies) runs against the raw captures and
is not part of this repository because it needs the lab's secret file for its
mapping. This pass reads a sanitized set and writes a copy in which every
hardware model string is replaced by a generic placeholder, so the XML shapes,
the canonical-name widths and the joins between files are unchanged.

    python3 tools/fixtures/mask_hardware.py --in <sanitized> --out <masked> \
        --map <mapping.json> [--rename-json <file>] [--exclude NAME ...]

The mapping file is NOT in this repository and must not be committed: it
pairs the lab's real model strings with their placeholders, which is exactly
what the masking hides. Shape:

    {"disk_models": {"<real disk model>": "SIM NVMe 4TB Model A", ...},
     "strings":     {"<real controller/NIC/CPU/host string>": "SIM ...", ...}}

What is masked, and how:
  * disk model strings (ScsiDisk model, HCL productId, controllerRawData mn),
    including the model field inside t10.NVMe____<model>____<serial> canonical
    names, which is hex-encoded ("-" is "2D") and padded to 40 characters
  * controller and NIC display names, host vendor and model, CPU model:
    replaced outright (no same-width padding, which would reveal lengths)
  * PCI vendor/device/subsystem ids: all set to 65535 (reserved, invalid)
  * firmware revisions, NVMe qualified names and serials inside
    controllerRawData, the vDS uuid: replaced by a hash salted with a random
    per-run value that is never written anywhere, so they are consistent
    within one output set and cannot be reversed
  * optional: VMDK base names given by --rename-json {"old": "new"} (the
    first pass maps VM names but not VMDK files named after a VM)

Driver names and versions (nvme_pcie, i40en, 1.4.0.8-2vmw...) are VMware
software identifiers and are kept. A final scan for hardware vendor names
fails the run if anything got through.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import secrets
import sys

# Hardware vendors, not lab parts: the scan must not itself name what the
# lab runs. The same list lives in internal/catalog/catalog_test.go.
VENDORS = re.compile(
    r"Samsung|Sabrent|Seagate|FireCuda|\bTEAM\b|hynix|phison|\bAMD\b|\bIntel\b|"
    r"Realtek|Mellanox|Broadcom Inc|Micron|Kingston|Western Digital|\bWDC\b|Crucial|"
    r"\bDell\b|\bHPE\b|Lenovo|Supermicro|\bASUS\b|Gigabyte|Minisforum|Beelink|"
    r"Micro Computer|JetKVM|Sk hynix|SKHynix|Toshiba|Kioxia|Solidigm|Nvidia|Qualcomm",
    re.IGNORECASE,
)
NQN_RE = re.compile(r"nqn\.\d{4}-\d{2}\.[a-z.]+")
VDS_UUID_RE = re.compile(r"\b[0-9a-f]{2}(?: [0-9a-f]{2}){7}-[0-9a-f]{2}(?: [0-9a-f]{2}){7}\b")


class Masker:
    def __init__(self, mapping: dict, renames: dict[str, str]):
        self.salt = secrets.token_hex(16)  # never written anywhere
        self.disk_models: dict[str, str] = mapping["disk_models"]
        self.strings: dict[str, str] = mapping["strings"]
        self.renames = renames
        self.reps = self._build()

    def hsh(self, s: str, n: int) -> str:
        return hashlib.sha256((self.salt + "|" + s).encode()).hexdigest()[:n]

    @staticmethod
    def t10_encode(model: str) -> str:
        """Model string as it appears inside a t10.NVMe canonical name."""
        out = []
        for ch in model:
            if ch.isalnum():
                out.append(ch)
            elif ch == " ":
                out.append("_")
            else:
                out.append("%02X" % ord(ch))
        return "".join(out)

    @staticmethod
    def pad_like(new: str, old: str, fill: str = " ") -> str:
        if len(new) >= len(old):
            return new[: len(old)]
        return new + fill * (len(old) - len(new))

    def _build(self):
        reps = []
        enc = {self.t10_encode(k): self.t10_encode(v) for k, v in self.disk_models.items()}

        def canon(m: re.Match) -> str:
            # the model field is nominally 40 wide, but hex-escaped characters
            # widen it, so match the field by its trailing padding
            field, serial = m.group(1), m.group(2)
            new = enc.get(field.rstrip("_"))
            if new is None:
                raise SystemExit(f"unmapped NVMe model field in canonical name: {field!r}")
            return "t10.NVMe____" + self.pad_like(new, field, "_") + serial

        reps.append((re.compile(r"t10\.NVMe____([A-Za-z0-9]+(?:_[A-Za-z0-9]+)*_{2,})([0-9A-Fa-f]{16})(?![A-Za-z0-9])"), canon))
        # SCSI model fields are 40 columns, space padded: keep that width
        for old, new in sorted(self.disk_models.items(), key=lambda kv: -len(kv[0])):
            reps.append((re.compile(re.escape(old) + r"( *)"),
                         lambda m, new=new, old=old: self.pad_like(new, old + m.group(1))))
        for old, new in sorted(self.strings.items(), key=lambda kv: -len(kv[0])):
            reps.append((re.compile(re.escape(old) + r" *"), new))
        # PCI ids
        reps.append((re.compile(r"<(vendorId|subVendorId|deviceId|subDeviceId)>\d+</\1>"), r"<\1>65535</\1>"))
        reps.append((re.compile(r"(&quot;(?:vid|ssvid)&quot;: )\d+"), r"\g<1>65535"))

        def fw(m: re.Match) -> str:
            return m.group(1) + self.hsh(m.group(2), 8).upper() + m.group(3)

        reps.append((re.compile(r"(<revision(?: xmlns=\"urn:vim25\")?>)([^<]+)(</revision>)"), fw))
        reps.append((re.compile(r"(<fwVersion>)(?!N/A<)([^<]+)(</fwVersion>)"), fw))
        reps.append((re.compile(r"(&quot;fr&quot;: &quot;)([^&]+)(&quot;)"), fw))
        reps.append((re.compile(r'("(?:revision|firmware|fwVersion)": ")(?!N/A")([^"]+)(")'), fw))

        def nqn(m: re.Match) -> str:
            return m.group(1) + "nqn.2000-01.invalid:nvme:sim:" + self.hsh(m.group(2), 14) + m.group(3)

        reps.append((re.compile(r"(&quot;(?:name|subnqn)&quot;: &quot;)(nqn\.[^&]*?)( *&quot;)"), nqn))
        reps.append((re.compile(r"(&quot;sn&quot;: &quot;)([^&]*?) *(&quot;)"),
                     lambda m: m.group(1) + self.hsh(m.group(2), 14).upper() + m.group(3)))
        reps.append((re.compile(r"(<serialNumber(?: xmlns=\"urn:vim25\")?>)([^<]+)(</serialNumber>)"),
                     lambda m: m.group(1) + "NVMe____SIM_SERIAL" + self.hsh(m.group(2), 16).upper() + m.group(3)))
        # vDS uuid in its spaced form ("50 03 9c ... -bd 21 ..."), which the
        # first pass's UUID remapping does not match
        reps.append((VDS_UUID_RE, lambda m: self.vds_uuid(m.group(0))))
        return reps

    def vds_uuid(self, old: str) -> str:
        h = self.hsh(old, 32)
        pairs = [h[i:i + 2] for i in range(0, 32, 2)]
        return " ".join(pairs[:8]) + "-" + " ".join(pairs[8:])

    def mask(self, text: str) -> str:
        for pat, rep in self.reps:
            text = pat.sub(rep, text)
        for old, new in sorted(self.renames.items(), key=lambda kv: -len(kv[0])):
            text = re.sub(r"(?<![A-Za-z0-9_-])" + re.escape(old) + r"(?=[_.-]|\b)", new, text)
        return text


def scan(text: str) -> list[str]:
    hits = [m.group(0) for m in VENDORS.finditer(text)]
    hits += [m.group(0) for m in NQN_RE.finditer(text) if m.group(0) != "nqn.2000-01.invalid"]
    return hits


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--in", dest="src", required=True)
    ap.add_argument("--out", dest="dst", required=True)
    ap.add_argument("--map", required=True, help="private mapping file (never committed)")
    ap.add_argument("--rename-json", help='{"old-basename": "new-basename"} for VMDK file names')
    ap.add_argument("--exclude", action="append", default=[], help="file name to leave out (repeatable)")
    args = ap.parse_args()

    with open(args.map) as fh:
        mapping = json.load(fh)
    renames = {}
    if args.rename_json:
        with open(args.rename_json) as fh:
            renames = json.load(fh)
    masker = Masker(mapping, renames)
    os.makedirs(args.dst, exist_ok=True)
    n, hits = 0, []
    for fn in sorted(os.listdir(args.src)):
        src = os.path.join(args.src, fn)
        if not os.path.isfile(src) or fn in args.exclude:
            continue
        with open(src, encoding="utf-8") as fh:
            text = masker.mask(fh.read())
        hits += [(fn, h) for h in scan(text)]
        # the mapping's real strings must be gone too
        for real in list(mapping["disk_models"]) + list(mapping["strings"]):
            if real in text:
                hits.append((fn, "<mapped string still present>"))
        with open(os.path.join(args.dst, fn), "w", encoding="utf-8") as fh:
            fh.write(text)
        n += 1
    print(f"masked {n} files into {args.dst}")
    if hits:
        print("scan FAILED; unmasked strings remain:", file=sys.stderr)
        for fn, h in hits[:20]:
            print(f"  {fn}: {h!r}", file=sys.stderr)
        return 1
    print("vendor scan clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
