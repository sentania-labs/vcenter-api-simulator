# Live capture: vCenter 9.1.1, one vSAN ESA cluster

Sanitized SOAP captures of a read-only probe of a lab vCenter (9.1.1 build
25784172, five ESXi 9.1.1 hosts, one vSAN ESA cluster with dedup and
compression on), taken 2026-11-06 through the reference management pack's own
connect and collection code (pyVmomi 8.0.3.0.1 plus its vendored vSAN
bindings). These files are the contract the simulator is tested against:
`internal/catalog/catalog.json` is generated from them and the Go tests replay
every request here against the simulator.

| Files | What |
|---|---|
| `00-vimServiceVersions.resp.xml`, `00-vsanServiceVersions.resp.xml` | the version documents the simulator serves verbatim |
| `clusterconfig-*`, `nodeinfo-*`, `entitytypes-*` | VsanClusterGetConfig, VsanPerfQueryNodeInformation, VsanPerfGetSupportedEntityTypes |
| `perf-<cluster>-<family>.{req,resp}.xml` | one VsanPerfQueryPerf wildcard query per advertised family, 15-minute window |
| `edge-<cluster>-<case>.*` | the query edge cases: bogus family, no times, two wildcards, start after end, future and year-old windows, 4 and 30 minute windows, a specific entity, no uuid |
| `manageddisks-<cluster>-<host>.*` | QueryVsanManagedDisks per host |
| `healthsummary-*` | VsanQueryVcClusterHealthSummary(fields=[hclInfo]) |
| `postlogout-*`, `nocookie-*` | fault shapes after Logout and with no session cookie |
| `summary.json`, `families-digest.json` | the probe's inventory summary and per-family digest |

## Sanitization

Two passes, both reviewed, and a leak check against the raw set:

1. Identity (run against the raw captures, outside this repository): vCenter
   and host names, DNS domain, SSO user, cluster/datacenter/datastore/VM/vDS/
   portgroup names replaced by numbered placeholders (`esx01`, `vm-07`,
   `ds-08`, `lab.invalid`); every UUID mapped through UUID v5 under a
   per-run random namespace (joins preserved, mapping discarded); IPv4 to
   `10.255.x.y` and MACs to `02:00:00:xx:xx:xx` by hash; NVMe serial
   suffixes and naa/eui ids hashed; session cookies and session ids
   redacted. Version strings (apiVersion, urn:vsan/8.0.0.4, driver versions)
   are preserved.
2. Hardware (`tools/fixtures/mask_hardware.py`, run on the output of pass 1):
   disk, controller, NIC, CPU and server model strings replaced by
   same-width placeholders (`SIM NVMe 4TB Model A`, `SIM-HOST`), including
   the model field inside `t10.NVMe____<model>____<serial>` canonical names;
   PCI vendor/device ids, firmware revisions, NVMe qualified names and
   serials inside the HCL `controllerRawData` blobs masked; VMDK base names
   derived from VM names renamed. The script's brand scan must come back
   clean.

The raw captures are not in any repository. `internal/catalog/catalog_test.go`
re-checks every file here for brand names, lab host names and NVMe qualified
names on every CI run.

Known residue, by design: `_001c`-style first-class-disk directory names,
hex VMDK file names, and VMware's own software version strings.
