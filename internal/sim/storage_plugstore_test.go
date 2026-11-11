package sim

import (
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// Local device paths use the runtime path form from the live 9.1.1 capture
// (vmhba0:C0:T0:L0), not the HBA key.
func TestStorageDevicePlugStorePathNamesUseHBADevice(t *testing.T) {
	h := &topology.Host{Devices: []*topology.Device{{HBA: "vmhba0", ScsiKey: "key-vim.host.ScsiDisk-a"}}}
	pst := storageDevice(h).PlugStoreTopology
	if len(pst.Path) != 1 || pst.Path[0].Name != "vmhba0:C0:T0:L0" {
		t.Fatalf("plug store paths = %+v, want one path named vmhba0:C0:T0:L0", pst.Path)
	}
}
