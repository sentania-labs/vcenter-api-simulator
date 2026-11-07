package sim

import (
	"fmt"
	"sync"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// taskWaiter completes vcsim tasks without the property collector's 20 ms
// update ticker: it is a registry handler that sees every task state
// change the moment it happens. Inventory creation calls vcsim's method
// handlers directly and waits here, so building ten clusters takes well
// under a second instead of 20 ms per task.
type taskWaiter struct {
	mu   sync.Mutex
	done map[types.ManagedObjectReference]chan struct{}
}

var taskWaiterRef = types.ManagedObjectReference{Type: "VcsimulatorTaskWaiter", Value: "task-waiter"}

func newTaskWaiter(ctx *simulator.Context) *taskWaiter {
	w := &taskWaiter{done: map[types.ManagedObjectReference]chan struct{}{}}
	ctx.Map.AddHandler(w)
	return w
}

func (w *taskWaiter) Reference() types.ManagedObjectReference    { return taskWaiterRef }
func (w *taskWaiter) PutObject(*simulator.Context, mo.Reference) {}
func (w *taskWaiter) RemoveObject(*simulator.Context, types.ManagedObjectReference) {
}

func (w *taskWaiter) UpdateObject(_ *simulator.Context, o mo.Reference, changes []types.PropertyChange) {
	ref := o.Reference()
	if ref.Type != "Task" {
		return
	}
	for _, c := range changes {
		if c.Name != "info.state" {
			continue
		}
		if st, ok := c.Val.(types.TaskInfoState); ok && (st == types.TaskInfoStateSuccess || st == types.TaskInfoStateError) {
			w.signal(ref)
		}
	}
}

func (w *taskWaiter) signal(ref types.ManagedObjectReference) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch, ok := w.done[ref]
	if !ok {
		ch = make(chan struct{})
		w.done[ref] = ch
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// wait blocks until the task completes and returns its result.
func (w *taskWaiter) wait(ctx *simulator.Context, ref types.ManagedObjectReference) (types.AnyType, error) {
	w.mu.Lock()
	ch, ok := w.done[ref]
	if !ok {
		ch = make(chan struct{})
		w.done[ref] = ch
	}
	w.mu.Unlock()
	<-ch
	w.mu.Lock()
	delete(w.done, ref)
	w.mu.Unlock()
	t := ctx.Map.Get(ref).(*simulator.Task)
	var info types.TaskInfo
	ctx.Map.WithLock(&simulator.Context{}, t, func() { info = t.Info })
	if info.State == types.TaskInfoStateError {
		if info.Error != nil && info.Error.Fault != nil {
			return nil, fmt.Errorf("%s: %T: %s", info.Name, info.Error.Fault, info.Error.LocalizedMessage)
		}
		return nil, fmt.Errorf("%s failed", info.Name)
	}
	return info.Result, nil
}

// result unwraps a method body: the fault, if any, else the task result.
func (w *taskWaiter) result(ctx *simulator.Context, body soap.HasFault, taskRef types.ManagedObjectReference) (types.AnyType, error) {
	if f := body.Fault(); f != nil {
		return nil, fmt.Errorf("%s", f.String)
	}
	return w.wait(ctx, taskRef)
}
