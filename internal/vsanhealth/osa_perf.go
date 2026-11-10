package vsanhealth

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Generated from the sanitized OSA walk by tools/fixtures/build_osa_perf.py.
// It contains labels and value envelopes only, not captured identities.
//
//go:embed fixtures/osa-perf.json
var osaPerfJSON []byte

type osaFamily struct {
	Labels []string                 `json:"labels"`
	Stats  map[string]catalog.Stats `json:"stats"`
	None   []string                 `json:"none"`
}

var osaOnce sync.Once
var osaFamilies map[string]osaFamily

func loadOSAFamilies() map[string]osaFamily {
	osaOnce.Do(func() {
		if err := json.Unmarshal(osaPerfJSON, &osaFamilies); err != nil {
			panic(err)
		}
	})
	return osaFamilies
}

func osaFamilyFor(name string) (osaFamily, bool) {
	f, ok := loadOSAFamilies()[name]
	return f, ok
}

func osaEntities(cl *topology.Cluster, family string) []string {
	var refs []string
	add := func(id string) { refs = append(refs, family+":"+id) }
	switch family {
	case "cluster-domclient", "cluster-domcompmgr", "cluster-domowner", "cluster-esa-dedup-domclient-io", "cluster-rdt-network-latency", "cluster-resync", "vsan-cluster-capacity":
		add(cl.VsanUUID)
	default:
		for _, h := range cl.Hosts {
			switch family {
			case "disk-group", "cache-disk":
				add(h.DiskGroupUUID)
			case "capacity-disk":
				for _, d := range h.Devices {
					if d.Claimed {
						add(d.VsanUUID)
					}
				}
			case "host-cpu":
				for i := 0; i < h.Threads; i++ {
					add(fmt.Sprintf("%s|cpu-%d", h.NodeUUID, i))
				}
			case "dom-world-cpu":
				for _, w := range h.Worlds {
					add(fmt.Sprintf("%s|%s|%d", h.NodeUUID, w.Name, w.ID))
				}
			case "lsom-world-cpu":
				for _, d := range h.Devices {
					if d.Claimed {
						for _, suffix := range []string{"IORETRY_Ingress-4-worlds", "IORETRY_Egress-4-worlds", "Ingress-4-worlds", "Egress-4-worlds"} {
							add(h.NodeUUID + "|PLOG-" + d.VsanUUID + "-" + suffix)
						}
					}
				}
			case "vsan-pnic-net":
				for _, nic := range h.Pnics {
					add(h.NodeUUID + "|" + nic)
				}
			case "vsan-vnic-net":
				add(h.NodeUUID + "|" + h.Stack + "|" + h.VsanVmknic)
			case "vsan-tcpip-stats":
				add(h.NodeUUID + "|" + h.Stack)
			case "vnic-rdt-network-latency":
				add(h.NodeUUID + "|" + h.VsanVmknic)
			default:
				add(h.NodeUUID)
			}
		}
	}
	return refs
}

// osaEntityCount is len(osaEntities) without building the identifiers.
func osaEntityCount(cl *topology.Cluster, family string) int {
	switch family {
	case "cluster-domclient", "cluster-domcompmgr", "cluster-domowner", "cluster-esa-dedup-domclient-io", "cluster-rdt-network-latency", "cluster-resync", "vsan-cluster-capacity":
		return 1
	}
	n := 0
	for _, h := range cl.Hosts {
		switch family {
		case "capacity-disk", "lsom-world-cpu":
			claimed := 0
			for _, d := range h.Devices {
				if d.Claimed {
					claimed++
				}
			}
			if family == "lsom-world-cpu" {
				claimed *= 4
			}
			n += claimed
		case "host-cpu":
			n += h.Threads
		case "dom-world-cpu":
			n += len(h.Worlds)
		case "vsan-pnic-net":
			n += len(h.Pnics)
		default:
			n++
		}
	}
	return n
}

func osaValueIsNone(f osaFamily, label string) bool {
	for _, v := range f.None {
		if v == label {
			return true
		}
	}
	return false
}

// Wildcard outcomes observed in the OSA walk with the stats primary online.
var osaOtherBehaviors = func() map[string]catalog.Behaviour {
	result := map[string]catalog.Behaviour{}
	for _, name := range []string{"cluster-esa-dedup-domcompmgr-io", "cluster-esa-dedup-domowner-io", "cluster-esa-dedup-store-chunksvc", "cluster-esa-dedup-store-hashsvc", "cluster-remotedomclient", "cluster-zdom-top-stats", "computeHost-remotedomclient", "host-esa-dedup-domcompmgr-io", "host-esa-dedup-domowner-io", "host-esa-dedup-store-chunksvc", "host-esa-dedup-store-hashsvc", "host-remotedomclient", "host-zdom-top-stats", "ioinsight", "virtual-disk", "virtual-machine", "vsan-direct-cluster", "vsan-direct-host", "vsan-esa-disk-layer", "vsan-esa-disk-scsifw", "vsan-file-service", "vsan-iscsi-host", "vsan-iscsi-lun", "vsan-iscsi-target", "vscsi", "zdom-vtx"} {
		result[name] = catalog.Empty
	}
	for _, name := range []string{"clom-disk", "clom-host", "cmmds-workload", "computeCluster-remotedomclient", "ddh-disk", "heap-memory", "remote-virtual-disk", "remote-virtual-machine", "remote-vscsi", "slab-memory", "vsan-dp-historical-stats", "vsan-esa-dedup-hash", "vsan-esa-dedup-journal", "vsan-esa-dedup-reorder", "vsan-esa-dedup-xip", "vsan-esa-nvme-namespace"} {
		result[name] = catalog.InvalidArgument
	}
	for _, name := range []string{"host-pmem", "cluster-pmem"} {
		result[name] = catalog.NotSupported
	}
	return result
}()

func osaOtherBehavior(family string) (catalog.Behaviour, bool) {
	behaviour, ok := osaOtherBehaviors[family]
	return behaviour, ok
}
