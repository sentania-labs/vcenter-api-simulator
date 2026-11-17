package sim

import (
	"testing"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/simulator/vpx"
	"github.com/vmware/govmomi/vim25/types"
)

func TestConstructionTaskReleasesRunner(t *testing.T) {
	m := &simulator.Model{ServiceContent: vpx.ServiceContent, RootFolder: vpx.RootFolder}
	if err := m.Create(); err != nil {
		t.Fatal(err)
	}
	defer m.Remove()
	ctx := m.Service.Context
	w := newTaskWaiter(ctx)
	defer ctx.Map.RemoveHandler(w)
	task := simulator.CreateTask(ctx.Map.Get(m.ServiceContent.RootFolder), "construct", func(*simulator.Task) (types.AnyType, types.BaseMethodFault) { return "built", nil })
	ref := task.Run(ctx)
	value, err := w.wait(ctx, ref)
	if err != nil || value != "built" {
		t.Fatalf("%v %v", value, err)
	}
	if task.Execute != nil {
		t.Fatal("completed task retains construction input")
	}
	if task.Info.State != types.TaskInfoStateSuccess || task.Info.Result != "built" {
		t.Fatal("task history lost")
	}
}
