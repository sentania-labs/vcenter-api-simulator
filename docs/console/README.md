# Control console screenshots

Taken from the running app at 1440 × 900 (phone shots at 390 wide), on a throwaway instance started from `examples/scenarios/ten-clusters.yaml` with a stand-in tool collecting from it. No real vCenter or VCF Operations was involved. The screens the redesign replaced are in [`before/`](before/).

The console has eight tabs: Home, Clusters, Storage, Versions, Users, Security, Slowness and faults, Requests. Every change is one screen with its fields on the left and "What will happen" on the right, one button, and a green "Done" bar (or a red bar that stays) afterwards. There is no shared draft and no review step.

## The ten jobs

Each job was done start to finish in the running app; the shots are in the order the person sees them.

| Job | Before | After |
|---|---|---|
| 1. First time: choose passwords, sign in, see what is running | [claim](before/01-claim.png), [sign in](before/35-sign-in.png), [overview](before/02-overview.png) | [welcome](01-welcome.png), [tools password](02-welcome-tools-password.png), [sign in](03-sign-in.png), [Home, first time](04-home-first-time.png), [Home with a tool connected](05-home.png), [all changes](06-all-changes.png) |
| 2. Add a cluster and hosts | [topology](before/03-topology.png), [add cluster](before/04-add-cluster-clicked.png), [generator](before/29-generator.png), [applied](before/12-applied.png) | [Clusters](07-clusters.png), [add a cluster](08-add-cluster.png), [a wrong value](08b-add-cluster-error.png), [added](09-clusters-added.png), [one cluster](10-cluster.png), [add hosts](11-add-hosts.png), [remove hosts](12-remove-hosts.png), [change hardware](13-change-hardware.png), [replace everything](14-replace-everything.png), [confirm](14b-replace-confirm.png), [remove a cluster](10b-remove-cluster-confirm.png) |
| 3. Add iSCSI storage and an NFS datastore | [iSCSI form](before/05-iscsi-form.png), [iSCSI saved](before/06-iscsi-saved.png), [NFS form](before/07-nfs-form.png), [NFS saved](before/08-nfs-saved.png) | [Storage, empty](15a-storage-empty.png), [add iSCSI](16-add-iscsi.png), [add NFS](17-add-nfs.png), [Storage](15-storage.png), [one datastore](18-storage-item.png), [disconnect hosts](19-disconnect-hosts.png) |
| 4. vCenter and ESX versions, a mixed cluster | [settings](before/13-settings.png), [cluster open](before/14-versions-cluster-open.png), [other version](before/15-version-other.png), [saved](before/16-versions-saved.png) | [Versions](20-versions.png), [change vCenter version](21-change-vcenter-version.png), [change a cluster's versions](22-change-cluster-versions.png) |
| 5. Add a user, change one, remove one | [users draft](before/17-users-draft.png), [after edits](before/18-users-after-edits.png) | [Users](23-users.png), [add a user](24-add-user.png), [change a user](25-change-user.png), [remove confirm](26-remove-user-confirm.png), [Home afterwards](26b-home-after-removing-user.png) |
| 6. Schedule a change and cancel it | [review](before/09-review.png), [scheduled](before/10-scheduled.png), [cancelled](before/11-schedule-cancelled.png) | [later today](27a-schedule-later-today.png), [scheduled bar](27-scheduled-bar.png), [cancel](27b-cancel-scheduled.png) |
| 7. See what the tool asks for | [activity](before/24-activity.png), [unanswered](before/25-activity-unanswered.png), [request detail](before/31-request-detail.png) | [Requests](36-requests.png), [details](37-request-details.png) |
| 8. Certificate, idle sign-out, console password | [certificate confirm](before/19-certificate-confirm.png), [settings review](before/20-settings-review.png) | [Security](28-security.png), [idle sign-out](29-idle-sign-out.png), [new certificate](30-new-certificate.png), [Home afterwards](30b-home-after-new-certificate.png), [own certificate](31-own-certificate.png), [wrong key](31b-own-certificate-wrong-key.png), [console password](32-console-password.png) |
| 9. Slowness or a fault, and removing it | [telemetry](before/21-telemetry.png), [draft](before/22-telemetry-draft.png), [apply](before/23-telemetry-apply.png), [delete rule](before/28-delete-rule-confirm.png) | [Slowness and faults](33-slowness-and-faults.png), [set one up](34-set-up-a-fault.png), [switched on](35-fault-switched-on.png), [Requests after it is off](36c-requests-after-fault-off.png) |
| 10. Capture requests and download them | [request log](before/26-request-log.png), [compatibility](before/27-compatibility.png) | [capturing everything](36b-requests-capturing-all.png), [every request](38-every-request.png), [what the simulator can answer](39-what-the-simulator-can-answer.png) |

On every screen:

| | Before | After |
|---|---|---|
| A change failed | | [red bar with Try again](40-a-change-failed.png) |
| Paused | [pause confirm](before/30-pause-confirm.png) | [paused bar](41-paused.png) |
| Dark theme | | [Home](42-home-dark.png), [Requests](43-requests-dark.png), [add a cluster](44-add-cluster-dark.png) |
| Phone | [overview](before/32-phone-overview.png), [settings](before/33-phone-settings.png), [topology](before/34-phone-topology.png) | [Home](45-phone-home.png), [menu](46-phone-menu.png), [Clusters](47-phone-clusters.png), [add a cluster](48-phone-add-cluster.png), [Requests](49-phone-requests.png) |

## Where the built console differs from the approved mockup

Only where the simulator behaves differently from what the mockup assumed, so that the screen stays true:

- **Hosts per cluster are 3 to 64**, not 1 to 64, and removing hosts leaves at least three: a vSAN cluster needs three.
- **Clusters and datastores are addressed by datacenter and name**, and the datacenter is shown where a name repeats.
- **Remove hosts** says how many VMs go with them and that the highest-numbered VMs are the ones removed, which is how the simulator places VMs.
- **Two more tool states**: "quiet" (signed in, nothing asked for 15 minutes) and "signed out" (its session ended and it has not signed in again).
- **Requests refused while paused** are listed under "Failing on purpose".
- **The console password is read-only** on Security when the deployment supplies it (the `VCSIM_ADMIN_PASSWORD` environment variable, which is how the Helm chart sets it).
- **Scheduled changes do not survive a restart**; the history says which were dropped.
- **Replace everything** shows exact counts in "What will happen".
- **"Something tried…"** is used on Home when a wrong-password attempt cannot be tied to a known tool.
- **"Check the address works"** says so when the public name cannot be reached from the simulator itself, instead of claiming success.
- **Dates and times** follow the browser's locale.
