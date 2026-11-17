# Console screens

The console has nine tabs: Home, Clusters, Storage, Versions, Users, Security,
Activity, Slowness and faults, Requests. Every change is one screen with its fields
on the left and "What will happen" on the right, one button, and a green "Done" bar
afterwards (or a red bar that stays, with "Try again"). There is no shared draft and
no review step. How the console behaves is in the [reference](../REFERENCE.md#the-console).

These were taken from a running simulator at 1440 × 900 (phone shots 390 wide)
with a stand-in tool connected. The address shown is an example. No real vCenter
or VCF Operations was involved.

| Job | Screens |
|---|---|
| First time: choose passwords, sign in, see what is running | [console password](01-welcome.png), [tools password](02-welcome-tools-password.png), [sign in](03-sign-in.png), [Home, first time](04-home-first-time.png), [Home with tools connected](05-home.png), [how busy it has been](50b-home-stats-since-start.png) |
| Add a cluster and hosts | [Clusters](07-clusters.png), [add a cluster](08-add-cluster.png), [one cluster](10-cluster.png), [add hosts](11-add-hosts.png) |
| Add iSCSI and NFS storage | [Storage, empty](15a-storage-empty.png), [add iSCSI](16-add-iscsi.png), [add NFS](17-add-nfs.png), [Storage](15-storage.png) |
| Change the vCenter and ESX versions | [Versions](20-versions.png), [change vCenter version](21-change-vcenter-version.png) |
| Add, change or remove a user | [Users](23-users.png), [add a user](24-add-user.png) |
| Certificate, idle sign-out, console password, how licensing looks | [Security](28-security.png), [how licensing looks to tools](28b-licensing.png) |
| Make requests slow or fail | [Slowness and faults](33-slowness-and-faults.png), [set one up](34-set-up-a-fault.png) |
| See what a tool asks for, and report what got no answer | [Requests](36-requests.png), [one request](37-request-details.png) |
| Make the environment change by itself | [Activity, not started](51a-activity-before-start.png), [starting asks first](51b-activity-start-asks-first.png), [running](51-activity.png), [one cluster's dials](52-activity-one-cluster.png), ["Do it now" asks first](52b-activity-do-it-now-asks-first.png), [an exact number](53-activity-exact-number.png), [what has happened](55-what-has-happened.png), [Home with Activity](56-home-with-activity.png) |
| Dark theme and phone | [Home, dark](42-home-dark.png), [Activity, dark](59-activity-dark.png), [Home on a phone](45-phone-home.png), [the menu](46-phone-menu.png), [Activity on a phone](57-phone-activity.png) |
