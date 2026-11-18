package sim

import (
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

// Publication must preserve the optional snapshot parent when it is cleared.
// govmomi otherwise allocates it while applying the child notifications.
func TestLifePublicationOptionalSnapshot(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("publication"))
	l := s.Life()
	ctx := l.as("")
	vm := l.reg.Get(s.Clusters[0].VMs[0]).(*simulator.VirtualMachine)
	ref := types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: "snapshot-proof"}
	ctx.WithLock(vm, func() {
		updateLife(ctx, vm, []types.PropertyChange{{Name: "snapshot", Val: &types.VirtualMachineSnapshotInfo{CurrentSnapshot: &ref}}})
		if vm.Snapshot == nil || vm.Snapshot.CurrentSnapshot == nil || *vm.Snapshot.CurrentSnapshot != ref {
			t.Fatal("snapshot reference was lost")
		}
		updateLife(ctx, vm, []types.PropertyChange{{Name: "snapshot", Val: (*types.VirtualMachineSnapshotInfo)(nil)}})
		if vm.Snapshot != nil {
			t.Fatal("leaf clears recreated the removed snapshot parent")
		}
	})
}
