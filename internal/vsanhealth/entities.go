package vsanhealth

import (
	"fmt"
	"strings"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Entities enumerates the entityRefIds of a family on a cluster, driven by
// the identifier shapes the catalog derived from the capture, so the
// simulator can only emit the shapes the live service did.
func Entities(c *catalog.Catalog, cl *topology.Cluster, family string) ([]string, error) {
	f, ok := c.Families[family]
	if !ok || f.Behaviour != catalog.Data {
		return nil, nil
	}
	var out []string
	for _, shape := range f.RefShapes {
		refs, err := byShape(shape, cl)
		if err != nil {
			return nil, fmt.Errorf("family %s: %w", family, err)
		}
		for _, r := range refs {
			out = append(out, family+":"+r)
		}
	}
	return out, nil
}

func byShape(shape string, cl *topology.Cluster) ([]string, error) {
	var out []string
	perHost := func(f func(h *topology.Host) []string) {
		for _, h := range cl.Hosts {
			out = append(out, f(h)...)
		}
	}
	switch shape {
	case "cluster":
		out = append(out, cl.VsanUUID)
	case "node":
		perHost(func(h *topology.Host) []string { return []string{h.NodeUUID} })
	case "node|cpu-N":
		perHost(func(h *topology.Host) []string {
			var r []string
			for i := 0; i < h.Threads; i++ {
				r = append(r, fmt.Sprintf("%s|cpu-%d", h.NodeUUID, i))
			}
			return r
		})
	case "node|VSAN_0xHEX_NAME|N":
		perHost(func(h *topology.Host) []string {
			var r []string
			for _, w := range h.Worlds {
				r = append(r, fmt.Sprintf("%s|%s|%d", h.NodeUUID, w.Name, w.ID))
			}
			return r
		})
	case "node|defaultTcpipStack":
		perHost(func(h *topology.Host) []string { return []string{h.NodeUUID + "|" + h.Stack} })
	case "node|defaultTcpipStack|vmkN":
		perHost(func(h *topology.Host) []string {
			return []string{h.NodeUUID + "|" + h.Stack + "|" + h.VsanVmknic}
		})
	case "node|vmkN":
		perHost(func(h *topology.Host) []string { return []string{h.NodeUUID + "|" + h.VsanVmknic} })
	case "node|vmnicN":
		perHost(func(h *topology.Host) []string {
			var r []string
			for _, p := range h.Pnics {
				r = append(r, h.NodeUUID+"|"+p)
			}
			return r
		})
	case "disk":
		perHost(func(h *topology.Host) []string {
			var r []string
			for _, d := range h.Devices {
				if d.Claimed {
					r = append(r, d.VsanUUID)
				}
			}
			return r
		})
	case "vm":
		for _, vm := range cl.VMs {
			out = append(out, vm.InstanceUUID)
		}
	case "vm|scsiN:M":
		for _, vm := range cl.VMs {
			for _, d := range vm.Disks {
				out = append(out, vm.InstanceUUID+"|"+d.Device)
			}
		}
	case "vdisk-path":
		for _, vm := range cl.VMs {
			for _, d := range vm.Disks {
				out = append(out, d.Path)
			}
		}
	default:
		return nil, fmt.Errorf("no enumerator for identifier shape %q", shape)
	}
	return out, nil
}

// splitRef splits "<family>:<tail>".
func splitRef(ref string) (family, tail string, ok bool) {
	return strings.Cut(ref, ":")
}
