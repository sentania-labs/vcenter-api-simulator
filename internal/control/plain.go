package control

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
)

// The console says everything in plain words. This file holds the words:
// small sentence helpers, the plain name of every kind of vSAN performance
// data the simulator knows, and the plain name of every request a tool can
// make. A name that is missing here is shown by a worded fallback, never as
// a bare identifier on its own.

// num formats a count with thousands separators.
func num(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// plural is "1 host" or "4 hosts"; many overrides the plural form.
func plural(n int, one string, many ...string) string {
	word := one + "s"
	if len(many) > 0 {
		word = many[0]
	}
	if n == 1 {
		word = one
	}
	return num(n) + " " + word
}

// listAnd is "a", "a and b", "a, b and c".
func listAnd(a []string) string {
	switch len(a) {
	case 0:
		return ""
	case 1:
		return a[0]
	}
	return strings.Join(a[:len(a)-1], ", ") + " and " + a[len(a)-1]
}

func lower1(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func upper1(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func bold(s string) string { return "**" + s + "**" }

// timeToken marks a moment inside a sentence. The console shows it in the
// viewer's own local time, which the simulator does not know.
func timeToken(t time.Time) string { return "{{time:" + t.UTC().Format(time.RFC3339) + "}}" }

// megabytes is a size in MB with one decimal, never below 0.1.
func megabytes(b int64) string {
	mb := float64(b) / (1 << 20)
	if mb < 0.1 {
		mb = 0.1
	}
	return fmt.Sprintf("%.1f MB", mb)
}

// sizeText is a datastore size as a person types it: "2 TB", "500 GB".
func sizeText(b int64) string {
	const gb, tb = 1 << 30, 1 << 40
	trim := func(v float64, unit string) string {
		s := fmt.Sprintf("%.1f", v)
		s = strings.TrimSuffix(s, ".0")
		return s + " " + unit
	}
	if b >= tb {
		return trim(float64(b)/tb, "TB")
	}
	return trim(float64(b)/gb, "GB")
}

// ---------------------------------------------------------------------------
// kinds of vSAN performance data

// familyNames is the plain name of each entity family the performance
// service advertises. TestEveryFamilyHasAPlainName keeps it complete.
var familyNames = map[string]string{
	"cache-disk":                       "Cache disks",
	"capacity-disk":                    "Capacity disks",
	"clom-disk":                        "Placement decisions, per disk",
	"clom-host":                        "Placement decisions, per host",
	"cluster-domclient":                "Cluster disk activity",
	"cluster-domcompmgr":               "Cluster disk activity at the disks",
	"cluster-domowner":                 "Cluster disk activity inside vSAN",
	"cluster-esa-dedup-domclient-io":   "Space saving",
	"cluster-esa-dedup-domcompmgr-io":  "Space saving at the disks, per cluster",
	"cluster-esa-dedup-domowner-io":    "Space saving inside vSAN, per cluster",
	"cluster-esa-dedup-store-chunksvc": "Space saving data blocks, per cluster",
	"cluster-esa-dedup-store-hashsvc":  "Space saving fingerprints, per cluster",
	"cluster-pmem":                     "Persistent memory, per cluster",
	"cluster-rdt-network-latency":      "Network delay between hosts, per cluster",
	"cluster-remotedomclient":          "Remote clusters",
	"cluster-resync":                   "Resync activity",
	"cluster-zdom-top-stats":           "Storage engine summary, per cluster",
	"cmmds-net":                        "Cluster membership network",
	"cmmds-workload":                   "Cluster membership workload",
	"computeCluster-remotedomclient":   "Compute-only clusters that borrow storage",
	"computeHost-remotedomclient":      "Compute-only hosts that borrow storage",
	"ddh-disk":                         "Disk health monitoring",
	"disk-group":                       "Disk groups",
	"dom-world-cpu":                    "Processor use of vSAN's internal tasks",
	"heap-memory":                      "Internal memory heaps",
	"host-cpu":                         "Host processors",
	"host-domclient":                   "Host disk activity",
	"host-domcompmgr":                  "Host disk activity at the disks",
	"host-domowner":                    "Host disk activity inside vSAN",
	"host-esa-dedup-domclient-io":      "Space saving, per host",
	"host-esa-dedup-domcompmgr-io":     "Space saving at the disks, per host",
	"host-esa-dedup-domowner-io":       "Space saving inside vSAN, per host",
	"host-esa-dedup-store-chunksvc":    "Space saving data blocks, per host",
	"host-esa-dedup-store-hashsvc":     "Space saving fingerprints, per host",
	"host-pmem":                        "Persistent memory",
	"host-remotedomclient":             "Hosts that lend storage to remote clusters",
	"host-vsansparse":                  "Snapshots",
	"host-zdom-top-stats":              "Storage engine summary, per host",
	"ioinsight":                        "IOInsight recordings",
	"lsom-world-cpu":                   "Processor use of the disk layer",
	"rdt-net":                          "Network between hosts",
	"rdt-network-latency":              "Network delay between hosts, per host",
	"remote-virtual-disk":              "Virtual disks on borrowed storage",
	"remote-virtual-machine":           "Virtual machines on borrowed storage",
	"remote-vscsi":                     "Virtual disk controllers on borrowed storage",
	"slab-memory":                      "Internal memory pools",
	"system-mem":                       "Host memory",
	"virtual-disk":                     "Virtual disks",
	"virtual-machine":                  "Virtual machines",
	"vnic-rdt-network-latency":         "Network delay between hosts, per network adapter",
	"vsan-cluster-capacity":            "Cluster capacity",
	"vsan-cpu":                         "Processor use of vSAN",
	"vsan-direct-cluster":              "vSAN Direct, per cluster",
	"vsan-direct-host":                 "vSAN Direct, per host",
	"vsan-distribution":                "Object placement",
	"vsan-dp-historical-stats":         "Data protection history",
	"vsan-esa-disk-layer":              "vSAN disks",
	"vsan-esa-disk-scsifw":             "vSAN disk devices",
	"vsan-file-service":                "File shares",
	"vsan-host-net":                    "Host vSAN network",
	"vsan-iscsi-host":                  "iSCSI target service, per host",
	"vsan-iscsi-lun":                   "iSCSI LUNs that vSAN serves",
	"vsan-iscsi-target":                "iSCSI targets",
	"vsan-memory":                      "Memory use of vSAN",
	"vsan-pnic-net":                    "Host network cards",
	"vsan-tcpip-stats":                 "Host network traffic",
	"vsan-vnic-net":                    "Host vSAN network adapters",
	"vscsi":                            "Virtual disk controllers",
	"zdom-vtx":                         "Storage engine transactions",
}

// mainFamilies are the kinds tools ask for most, listed first where a kind
// is chosen.
var mainFamilies = []string{"vsan-pnic-net", "host-domclient", "cluster-domclient", "virtual-machine", "virtual-disk",
	"vsan-esa-disk-layer", "host-cpu", "system-mem", "rdt-net", "vsan-cluster-capacity", "cluster-esa-dedup-domclient-io"}

// familyName is the plain name of a kind of vSAN performance data. A kind
// with no name yet is described rather than shown bare.
func familyName(family string) string {
	if n, ok := familyNames[family]; ok {
		return n
	}
	return `The kind tools call "` + family + `"`
}

// faultMethodNames are the plain names of the other vSAN requests a
// scenario file can aim a fault at.
var faultMethodNames = map[string]string{
	"VsanPerfQueryNodeInformation":    "the hosts that run the vSAN performance service",
	"VsanClusterGetConfig":            "the vSAN settings of a cluster",
	"QueryVsanManagedDisks":           "the vSAN disks in each host",
	"VsanQueryVcClusterHealthSummary": "the vSAN health summary",
}

// ---------------------------------------------------------------------------
// kinds of request

// Kind titles shared by several requests.
const (
	kindSignIn    = "Signing in and out"
	kindInventory = "Lists of clusters, hosts, VMs and datastores"
	kindLicense   = "Which license this vCenter has"
)

var soapKinds = map[string]string{}

func init() {
	add := func(title string, ops ...string) {
		for _, op := range ops {
			soapKinds[op] = title
		}
	}
	add(kindSignIn, "Login", "Logout", "LoginByToken", "LoginBySSPI", "LoginExtensionByCertificate", "LoginExtensionBySubjectName",
		"SessionIsActive", "RetrieveServiceContent", "RetrieveContent", "CurrentTime", "CloneSession", "AcquireCloneTicket", "AcquireGenericServiceTicket")
	add(kindInventory, "CreateContainerView", "CreateListView", "CreateListViewFromView", "CreateInventoryView", "DestroyView",
		"RetrieveProperties", "RetrievePropertiesEx", "ContinueRetrievePropertiesEx", "CancelRetrievePropertiesEx",
		"CreatePropertyCollector", "DestroyPropertyCollector", "CreateFilter", "DestroyPropertyFilter",
		"WaitForUpdates", "WaitForUpdatesEx", "CheckForUpdates", "CancelWaitForUpdates",
		"FindByUuid", "FindAllByUuid", "FindByInventoryPath", "FindByDnsName", "FindAllByDnsName", "FindByIp", "FindAllByIp", "FindChild", "FindByDatastorePath", "Fetch")
	add("What the signed-in account is allowed to do", "HasPrivilegeOnEntity", "HasPrivilegeOnEntities", "HasUserPrivilegeOnEntities",
		"FetchUserPrivilegeOnEntities", "RetrieveEntityPermissions", "RetrieveAllPermissions", "RetrieveRolePermissions", "CheckPrivileges")
	add("The list of measurements vCenter can report", "QueryPerfCounter", "QueryPerfCounterByLevel", "QueryAvailablePerfMetric", "QueryPerfProviderSummary")
	add("vCenter's own performance measurements", "QueryPerf", "QueryPerfComposite")
	add(kindLicense, "QueryAssignedLicenses", "QueryLicenseSourceAvailability", "QueryLicenseUsage", "QuerySupportedFeatures", "DecodeLicense", "CheckLicenseFeature")
	add("vCenter's advanced settings", "QueryOptions", "QueryView")
	add("Add-ons registered with vCenter", "FindExtension", "QueryExtensionIpAllocationUsage", "GetPublicKey")
	add("Events and tasks", "CreateCollectorForEvents", "CreateCollectorForTasks", "QueryEvents", "ReadNextEvents", "ReadPreviousEvents",
		"ReadNextTasks", "ReadPreviousTasks", "DestroyCollector", "ResetCollector", "RewindCollector", "SetCollectorPageSize", "RetrieveArgumentDescription")
	add("Alarms", "GetAlarm", "GetAlarmState", "AreAlarmActionsEnabled")
	add("Custom fields and tags set on objects", "AddCustomFieldDef", "SetField")
	add("vSAN settings of each cluster", "VsanClusterGetConfig")
	add("vSAN health summary", "VsanQueryVcClusterHealthSummary")
	add("vSAN disks in each host", "QueryVsanManagedDisks", "QueryDisksForVsan")
	add("The kinds of vSAN performance data there are", "VsanPerfGetSupportedEntityTypes")
	add("Which hosts run the vSAN performance service", "VsanPerfQueryNodeInformation")
	add("State of the vSAN performance service", "VsanPerfQueryStatsObjectInformation", "VsanPerfQueryTimeRanges", "VsanPerfQueryClusterHealth")
	add("vSAN capacity", "VsanQuerySpaceUsage", "VsanQueryEntitySpaceUsage")
	add("Whether a vSAN cluster is stretched across sites", "VSANVcGetPreferredFaultDomain", "VSANVcRetrieveStretchedClusterVcCapability", "VSANIsWitnessVirtualAppliance", "VSANVcGetWitnessHosts")
	add("iSCSI storage that vSAN itself serves", "VsanVitGetIscsiTargets", "VsanVitGetIscsiLUNs", "VsanVitGetIscsiInitiatorGroups", "VsanVitQueryIscsiTargetServiceVersion", "VsanVitGetHomeObject")
	add("What this vSAN can do", "VsanGetCapabilities")
}

var restKinds = []struct {
	re    *regexp.Regexp
	title string
}{
	{regexp.MustCompile(`^/(api|rest)/(com/vmware/cis/)?session`), kindSignIn},
	{regexp.MustCompile(`^/(api|rest)/vcenter/namespace-management`), "Whether Kubernetes is turned on for any cluster"},
	{regexp.MustCompile(`^/(api|rest)/vcenter/(cluster|host|vm|datastore|datacenter|folder|network|resource-pool)(/|$)`), kindInventory},
	{regexp.MustCompile(`^/(api|rest)/vcenter/inventory`), kindInventory},
	{regexp.MustCompile(`^/(api|rest)/appliance/system/version`), "Which version vCenter is"},
	{regexp.MustCompile(`^/(api|rest)/appliance/`), "Health and settings of the vCenter appliance"},
	{regexp.MustCompile(`^/(api|rest)/(com/vmware/)?cis/tagging`), "Tags and categories"},
	{regexp.MustCompile(`^/(api|rest)/(com/vmware/)?content/`), "Content libraries"},
	{regexp.MustCompile(`^/(api|rest)/vcenter/storage/policies`), "Storage policies"},
	{regexp.MustCompile(`^/(api|rest)/vcenter/vm/[^/]+/storage/policy`), "Storage policies"},
	{regexp.MustCompile(`^/(api|rest)/esx/settings/`), "Host image settings"},
	{regexp.MustCompile(`^/(api|rest)/vcenter/(topology|system-config|vcha)/`), "How this vCenter is installed"},
	{regexp.MustCompile(`^/ls/|/license`), kindLicense},
	{regexp.MustCompile(`^/sdk/(vimServiceVersions|vsanServiceVersions)\.xml$`), "Which versions of its interface vCenter offers"},
	{regexp.MustCompile(`\.(wsdl|xsd)$`), "The file that describes how to talk to vCenter"},
	{regexp.MustCompile(`^/(sdk|vsanHealth)/?$`), "A plain visit to the address, the way a browser makes one"},
}

// idSegment is a path part that names one object: a MoRef or a UUID.
var idSegment = regexp.MustCompile(`^([a-z]+-[a-z]*[0-9]+|[0-9a-f]{8}-[0-9a-f-]{20,}|[0-9]+)$`)

// techName is the name a tool's own logs use for a request, with the
// identity of single objects left out so that like requests count together.
func techName(call activity.Call) string {
	m := call.Method
	i := strings.IndexByte(m, ' ')
	if i < 0 || !strings.HasPrefix(m[i+1:], "/") {
		return m
	}
	parts := strings.Split(m[i+1:], "/")
	for j, p := range parts {
		if idSegment.MatchString(p) {
			parts[j] = "{id}"
		}
	}
	return m[:i] + " " + strings.Join(parts, "/")
}

// kindOf names what a request asked for in plain words. Performance queries
// and refused sign-ins are worded by the caller, which knows more.
func kindOf(call activity.Call) string {
	m := call.Method
	if i := strings.IndexByte(m, ' '); i > 0 && strings.HasPrefix(m[i+1:], "/") {
		path := m[i+1:]
		for _, k := range restKinds {
			if k.re.MatchString(path) {
				return k.title
			}
		}
		return "Something at the address " + techName(call)[i+1:]
	}
	if t, ok := soapKinds[m]; ok {
		return t
	}
	if strings.Contains(m, "License") {
		return kindLicense
	}
	return `Something tools call "` + m + `"`
}
