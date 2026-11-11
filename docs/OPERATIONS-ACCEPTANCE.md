# First Operations acceptance

## Shared storage client smoke

The synthetic [`storage.yaml`](../examples/scenarios/storage.yaml) scenario
provides shared NFS and iSCSI VMFS datastores, an inaccessible NFS mount, an
all-paths-down LUN on one host and a near-full datastore. Run the govc smoke
with `tools/smoke/smoke.sh --scenario examples/scenarios/storage.yaml --storage`.
For PowerCLI, start the simulator with that scenario and run
`tools/smoke/smoke.ps1 -Server 127.0.0.1 -Port 8443 -Storage`. Both check
datastore and host storage reads and the host fault; the PowerCLI smoke also
uses `Get-Datastore`, `Get-ScsiLun`, and `Get-IScsiHbaTarget`. CI runs both
client paths. These checks do not establish that a real storage system or an
Operations management pack has been exercised.

This procedure exercises the installed Python **vSAN Host Metrics** pack
against the simulator through a real VCF Operations collector. It does not
install a pack or register a native vCenter adapter. Native vCenter
attachment and the separate Java Content Factory pack need their own gates.

## Measured result

Passed on 2026-11-07 with simulator **v0.2.0** (source
`824194df42f77e323414499a793a1a31cadb1377`), VCF Operations **9.1.1.0 build
25795063**, and the already-installed **vSAN Host Metrics 2.0.0** pack. The
reference collector was pinned to
`af976fb9fe97bbac2fd94b666a3f79aa12656f2a`. The image digest was
`sha256:cbc747ddaf2dfe9e86a6ee66c61c7864ea87039c7bca089fb070170d8eff32b4`.

| Assertion | Observed |
|---|---|
| Simulated entities / resource kinds | 240 / 41, exact expected identity and kind sets |
| Resources including adapter instance | 241, all `DATA_RECEIVING` |
| Expected / ingested metric series | 5,321 / 5,321 on each cycle |
| Collection 1 samples, America/Chicago | 09:04:28.641–09:04:28.658 CST |
| Collection 2 samples, America/Chicago | 09:09:33.707–09:09:33.728 CST |
| Resource IDs and expected metric keys | Unchanged between cycles |
| Source-value comparison | All 10,642 values match within `1e-6` relative/absolute tolerance |
| Metric series changing between cycles | 2,621 |
| Parent / child relationships | 0 / 0 across all 240 entities |
| Existing adapter configurations | All 26 unchanged; exactly one simulator adapter added |
| Installed solutions | Catalog unchanged |

The source-value comparison replayed the reference collector's time windows
against the running simulator for the 09:00 and 09:05 CST source buckets.
5,045 and 5,051 values respectively matched exactly; the remaining values
matched within floating-point storage tolerance. Operations-generated health
and alert metrics were excluded from the 5,321 source-series count.
The reference MP harness also passed all 32 checks against this endpoint.

Evidence came from the Suite API. UI screenshots were waived to avoid
interrupting an existing Operations UI session. No simulator defect was
found, so this milestone changes documentation only. Scale, native-vCenter
attachment, TLS verification from the Ops collector, restart under an active
Ops adapter, and the Java pack were not exercised here.

## Configuration

Use an explicitly approved development Operations instance and credential
source. Keep Operations credentials and API tokens in the process environment;
never include them in a command transcript, scenario, evidence bundle or PR.
Use separate placeholder credentials for the simulator.

Run a pinned simulator image on a host the Operations collector can reach.
The first acceptance uses `examples/scenarios/phase1.yaml`, a persistent data
volume, and HTTPS port 8443. Preserve the volume across restarts: it holds
both the identity registry and the certificate. The scenario has one ESA
cluster, four hosts, four VMs, four claimed devices and four local devices.

Snapshot the adapter inventory before creating anything. Add exactly one
`VsanHostMetrics` adapter with these settings:

| Setting | Value |
|---|---|
| Endpoint | Simulator host and port, reachable from the collector |
| Collection interval | Five minutes |
| `metric_scope` | `all` |
| `attach_to_vcenter` | `false` |
| `container_memory_limit` | `1024` MiB |
| `verify_certs` | `false` for this initial controlled-lab acceptance |
| Credential kind | `vsan_vc_credential`, using simulator-only placeholders |

Use the installed pack's schema rather than copying an older pack's fields.
The 2.0.0 credential fields are `vcenter_username` and `vcenter_password`.
`POST /suite-api/api/adapters?force=false` can create the adapter and its
dedicated placeholder credential together. Check for an existing adapter
with the chosen name first; do not blindly retry a timed-out creation request.
If the new adapter is stopped, start only its ID using
`PUT /suite-api/api/adapters/{id}/monitoringstate/start`.

With native attachment disabled, collected entities are intentionally
standalone. The pack's dashboards that depend on native vCenter ancestors
will not populate. This isolates endpoint collection and metric ingestion
without changing the child lists of existing vCenter resources.

## Evidence and assertions

1. Run `tools/mp-harness/harness.py` against the same endpoint and the pinned
   reference pack. Generate expected resource kinds, identifiers and metric
   keys from that pack's `perfsvc.collect(..., scope=None)` result. Compare
   identities and per-resource keys, not just grand totals.
2. Read `GET /suite-api/api/adapters/{id}` and
   `GET /suite-api/api/adapters/{id}/resources?pageSize=1000`. Follow pagination
   when needed. Separate the adapter's own resource from simulated entities.
   Record collection state, `lastCollected`, reported resource/metric counts,
   counts by kind and the exact resource identity set.
3. Read `GET /suite-api/api/resources/stats/latest` for **only** the discovered
   simulator resource IDs, batching requests. Use `maxSamples=3` or more and
   `currentOnly=false` for history: Operations rejects multiple samples with
   `currentOnly=true`. Match the expected metric keys and require samples at
   two distinct collection timestamps. Record missing or additional keys
   separately from Operations-generated health metrics.
4. Check relationships for all simulated entities. The read-only query
   `POST /suite-api/api/resources/bulk/relationships` accepts `resourceIds`,
   `relationshipType` (`PARENT` or `CHILD`) and `hierarchyDepth: 1`. Check both
   directions and pagination. In this standalone configuration, neither
   query should return a relationship to another resource.
5. Capture at least two nonempty collection cycles. Confirm stable resource
   IDs and metric keys, advancing timestamps, and `DATA_RECEIVING`. Initial
   discovery alone is insufficient: the adapter may expose resources before
   any metric samples are available.
6. Compare the final adapter inventory with the pre-create snapshot. The only
   new adapter should be the named simulator adapter; existing adapter
   configuration must be unchanged. Record the installed pack version before
   and after the test.

Keep raw proof outside the checkout. Include sanitized JSON excerpts in the
PR: counts by kind, expected versus ingested metric counts, collection
timestamps, a few changing metric samples, identity stability and relationship
query results. A timestamp from `stats/latest` is an Operations ingestion
timestamp; it is not necessarily the simulator's five-minute source bucket.
Do not claim exact source-value parity without comparing the corresponding
source buckets.

## Leaving and removing the test instance

Leave the accepted adapter and simulator running for inspection. Record the
adapter name and ID, dedicated credential ID, container name, image digest,
scenario hash and volume name in the private deployment baseline.

When the owner authorizes removal, select only that adapter in Operations
Integrations / Accounts. Decide explicitly whether its collected resources
should be retained before deleting it. Remove its dedicated simulator
credential only after checking that no adapter references it. Stop and remove
only the test container; retain its data volume unless identity/certificate
reset is intended. This acceptance procedure performs none of those removals.
