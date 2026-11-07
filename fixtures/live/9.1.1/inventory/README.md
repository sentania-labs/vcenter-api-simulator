# Live inventory capture: vCenter 9.1.1, trimmed

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
