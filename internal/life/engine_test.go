package life

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// world is an inventory in memory: which host each VM is on, which are
// off, which hosts are in maintenance mode, and how full the datastores are.
type world struct {
	keys  []string
	hosts map[string][]string          // cluster -> host names
	at    map[string]map[string]string // cluster -> vm -> host
	off   map[string]bool              // cluster/vm
	maint map[string]bool              // cluster/host
	next  map[string]int
	used  map[string]float64 // cluster -> share of capacity used
	load  map[string][2]float64
	fail  error
}

func newWorld(clusters, hosts, perHost int) *world {
	w := &world{hosts: map[string][]string{}, at: map[string]map[string]string{}, off: map[string]bool{}, maint: map[string]bool{}, next: map[string]int{}, used: map[string]float64{}, load: map[string][2]float64{}}
	for c := 1; c <= clusters; c++ {
		key := fmt.Sprintf("dc-01/cluster-%02d", c)
		w.keys = append(w.keys, key)
		w.at[key] = map[string]string{}
		for h := 1; h <= hosts; h++ {
			w.hosts[key] = append(w.hosts[key], fmt.Sprintf("esx%02d.cluster-%02d.sim.invalid", h, c))
		}
		for v := 1; v <= hosts*perHost; v++ {
			w.at[key][fmt.Sprintf("cluster-%02d-vm-%03d", c, v)] = w.hosts[key][(v-1)%hosts]
		}
		w.next[key] = hosts*perHost + 1
		w.used[key] = 0.5
	}
	return w
}

func (w *world) Clusters() []Cluster {
	var out []Cluster
	for _, key := range w.keys {
		c := Cluster{Key: key, Name: key[strings.IndexByte(key, '/')+1:], VMs: len(w.at[key])}
		n := map[string]int{}
		for vm, h := range w.at[key] {
			n[h]++
			if w.off[key+"/"+vm] {
				c.Off++
			}
		}
		for _, h := range w.hosts[key] {
			c.Hosts = append(c.Hosts, Host{Name: h, VMs: n[h], Maintenance: w.maint[key+"/"+h]})
		}
		out = append(out, c)
	}
	return out
}

func (w *world) vms(cluster string, keep func(vm, host string) bool) []string {
	var out []string
	for vm, h := range w.at[cluster] {
		if keep(vm, h) {
			out = append(out, vm)
		}
	}
	sort.Strings(out)
	return out
}

func (w *world) free(cluster, not string) []string {
	var out []string
	for _, h := range w.hosts[cluster] {
		if !w.maint[cluster+"/"+h] && h != not {
			out = append(out, h)
		}
	}
	return out
}

func (w *world) Balance(cluster string, n int, pick uint64) []Move {
	hosts := w.free(cluster, "")
	if len(hosts) < 2 {
		return nil
	}
	var out []Move
	for i := 0; i < n; i++ {
		from, to := hosts[(int(pick%uint64(len(hosts)))+i)%len(hosts)], hosts[(int(pick%uint64(len(hosts)))+i+1)%len(hosts)]
		vms := w.vms(cluster, func(vm, h string) bool { return h == from && !w.off[cluster+"/"+vm] })
		if len(vms) == 0 {
			continue
		}
		vm := vms[pick%uint64(len(vms))]
		w.at[cluster][vm] = to
		out = append(out, Move{vm, from, to})
	}
	return out
}

func (w *world) PowerOff(cluster string, pick uint64) string {
	vms := w.vms(cluster, func(vm, _ string) bool { return !w.off[cluster+"/"+vm] })
	if len(vms) == 0 {
		return ""
	}
	vm := vms[pick%uint64(len(vms))]
	w.off[cluster+"/"+vm] = true
	return vm
}

func (w *world) PowerOn(cluster, vm string) bool {
	if _, ok := w.at[cluster][vm]; !ok {
		return false
	}
	delete(w.off, cluster+"/"+vm)
	return true
}

func (w *world) AddVM(cluster string, pick uint64) (string, string, error) {
	if w.fail != nil {
		return "", "", w.fail
	}
	hosts := w.free(cluster, "")
	vm := fmt.Sprintf("%s-vm-%03d", cluster[strings.IndexByte(cluster, '/')+1:], w.next[cluster])
	w.next[cluster]++
	w.at[cluster][vm] = hosts[pick%uint64(len(hosts))]
	return vm, w.at[cluster][vm], nil
}

func (w *world) RemoveVM(cluster string, pick uint64) (string, error) {
	vms := w.vms(cluster, func(string, string) bool { return true })
	if len(vms) == 0 {
		return "", nil
	}
	vm := vms[pick%uint64(len(vms))]
	delete(w.at[cluster], vm)
	delete(w.off, cluster+"/"+vm)
	return vm, nil
}

func (w *world) EnterMaintenance(cluster, host string) ([]Move, error) {
	others := w.free(cluster, host)
	var out []Move
	for i, vm := range w.vms(cluster, func(_, h string) bool { return h == host }) {
		to := others[i%len(others)]
		w.at[cluster][vm] = to
		out = append(out, Move{vm, host, to})
	}
	w.maint[cluster+"/"+host] = true
	return out, nil
}

func (w *world) ExitMaintenance(cluster, host string, pick uint64) []Move {
	delete(w.maint, cluster+"/"+host)
	var out []Move
	for _, vm := range w.vms(cluster, func(_, h string) bool { return h != host }) {
		if len(out) >= len(w.at[cluster])/len(w.hosts[cluster])/2 {
			break
		}
		out = append(out, Move{vm, w.at[cluster][vm], host})
		w.at[cluster][vm] = host
	}
	return out
}

func (w *world) Fill(cluster string, share float64) []Store {
	before := w.used[cluster]
	w.used[cluster] = min(0.9, max(0.1, before+share))
	return []Store{{Name: "vsanDatastore", Bytes: int64((w.used[cluster] - before) * 100 * (1 << 40)), UsedPercent: int(w.used[cluster]*100 + 0.5)}}
}

func (w *world) SetLoad(cluster string, cpu, memory float64) { w.load[cluster] = [2]float64{cpu, memory} }

// monday is a working day; the engine is given UTC as its local time.
var monday = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

func run(e *Engine, w World, from time.Time, d time.Duration) {
	for t := from; !t.After(from.Add(d)); t = t.Add(time.Minute) {
		e.Step(t, w)
	}
}

func sum(log []Entry, key string) (n int) {
	for _, en := range log {
		n += en.Counts[key]
	}
	return
}

func started(w *world, seed string, change func(*Settings)) *Engine {
	vms := 0
	for _, c := range w.Clusters() {
		vms += c.VMs
	}
	s := Defaults(vms)
	if change != nil {
		change(&s)
	}
	e := New(seed, time.UTC, s)
	e.Start(monday, w)
	return e
}

func TestSameSeedSameDay(t *testing.T) {
	a, b := newWorld(4, 8, 25), newWorld(4, 8, 25)
	ea, eb := started(a, "seed", nil), started(b, "seed", nil)
	run(ea, a, monday, 24*time.Hour)
	run(eb, b, monday, 24*time.Hour)
	if len(ea.Log) < 50 || !reflect.DeepEqual(ea.Log, eb.Log) {
		t.Fatalf("two runs with one seed differ, or little happened: %d and %d lines", len(ea.Log), len(eb.Log))
	}
	c := newWorld(4, 8, 25)
	ec := started(c, "another", nil)
	run(ec, c, monday, 24*time.Hour)
	if reflect.DeepEqual(ea.Log, ec.Log) {
		t.Fatal("a different seed made the same day")
	}
}

// The amounts follow the size: twice the VMs, about twice as much.
func TestAmountsFollowSize(t *testing.T) {
	day := func(clusters, hosts, perHost int) (moves, power, adds, removes int) {
		w := newWorld(clusters, hosts, perHost)
		e := started(w, "size", func(s *Settings) { s.Global[Maintenance] = Dial{} })
		run(e, w, monday, 24*time.Hour)
		return sum(e.Log, "move"), sum(e.Log, "off"), sum(e.Log, "add"), sum(e.Log, "remove")
	}
	near := func(what string, got int, want, slack float64) {
		t.Helper()
		if float64(got) < want*(1-slack) || float64(got) > want*(1+slack) {
			t.Errorf("%s: %d in a day, want about %.0f", what, got, want)
		}
	}
	m, p, a, r := day(4, 8, 25) // 800 VMs
	near("VM moves at 800 VMs", m, 200, 0.25)
	near("power at 800 VMs", p, 16, 0.4)
	near("adds at 800 VMs", a, 8, 0.5)
	near("removes at 800 VMs", r, 8, 0.5)
	m2, _, _, _ := day(1, 16, 25) // 400 VMs in one cluster
	near("VM moves at 400 VMs", m2, 100, 0.25)
}

func TestExactNumbersAreSteady(t *testing.T) {
	w := newWorld(4, 8, 25)
	e := started(w, "exact", func(s *Settings) {
		s.Global[Adds] = Dial{Exact: &Exact{N: 30, Unit: UnitMinutes}}
		s.Global[Removes] = Dial{Exact: &Exact{N: 45, Unit: UnitMinutes}}
		s.Global[Maintenance] = Dial{}
		s.MinVMs, s.MaxVMs = 0, 100000
	})
	run(e, w, monday, 24*time.Hour)
	if a := sum(e.Log, "add"); a < 44 || a > 52 {
		t.Errorf("a VM every 30 minutes made %d in a day, want about 48", a)
	}
	if r := sum(e.Log, "remove"); r < 29 || r > 35 {
		t.Errorf("a VM removed every 45 minutes made %d in a day, want about 32", r)
	}
	night := 0
	for _, en := range e.Log {
		if en.Kind == KindAdd && en.At.Hour() < 6 {
			night++
		}
	}
	if night < 8 {
		t.Errorf("an exact rhythm runs day and night, but only %d VMs were added before 6 AM", night)
	}
}

func TestVMCountStaysInBounds(t *testing.T) {
	w := newWorld(2, 8, 25)
	e := started(w, "bounds", func(s *Settings) {
		s.Global[Adds] = Dial{Exact: &Exact{N: 10, Unit: UnitMinutes}}
		s.Global[Removes] = Dial{}
	})
	run(e, w, monday, 48*time.Hour)
	vms, _ := totalVMs(w.Clusters())
	if vms != e.Settings.MaxVMs || vms != 420 {
		t.Fatalf("VMs = %d, want the ceiling %d (5%% above 400)", vms, e.Settings.MaxVMs)
	}
	waits := 0
	for _, en := range e.Log {
		if en.Kind == KindWait {
			waits++
		}
	}
	if waits != 2 {
		t.Errorf("waiting at the limit was said %d times in two days, want once a day", waits)
	}
}

func TestMaintenanceShareAndSwap(t *testing.T) {
	w := newWorld(4, 8, 25)
	e := started(w, "maint", func(s *Settings) {
		for _, b := range Counted {
			s.Global[b] = Dial{}
		}
	})
	inMaint := func() (n int, perCluster map[string]int) {
		perCluster = map[string]int{}
		for k := range w.maint {
			n++
			perCluster[k[:strings.LastIndexByte(k, '/')]]++
		}
		return
	}
	n, per := inMaint()
	if n != 2 {
		t.Fatalf("5%% of 32 hosts: %d in maintenance mode straight after start, want 2", n)
	}
	for c, k := range per {
		if k > 2 {
			t.Errorf("%s has %d hosts out, more than a quarter of 8", c, k)
		}
	}
	if got := sum(e.Log, "move"); got != 50 {
		t.Errorf("two hosts of 25 VMs went in: %d VMs moved, want 50", got)
	}
	for _, en := range e.Log {
		if en.Kind == KindIn && (!en.You || len(en.Details) != 25) {
			t.Errorf("the hosts that go in at Start follow from the button and name each VM: %+v", en)
		}
	}
	first := fmt.Sprint(w.maint)
	run(e, w, monday, 8*24*time.Hour)
	if n, _ := inMaint(); n != 2 {
		t.Fatalf("%d hosts in maintenance mode after a week, want 2", n)
	}
	if fmt.Sprint(w.maint) == first {
		t.Error("the same hosts are in maintenance mode after 8 days: they should have been swapped")
	}
	// no VM is ever on a host that is in maintenance mode
	for c, vms := range w.at {
		for vm, h := range vms {
			if w.maint[c+"/"+h] {
				t.Fatalf("%s is on %s, which is in maintenance mode", vm, h)
			}
		}
	}
}

func TestClusterDialOverridesAndFollows(t *testing.T) {
	w := newWorld(2, 8, 25)
	e := started(w, "dials", func(s *Settings) {
		for _, b := range []string{Power, Adds, Removes, Maintenance} {
			s.Global[b] = Dial{}
		}
		s.Clusters = map[string]map[string]Dial{"dc-01/cluster-02": {Moves: Dial{Level: 4}}}
	})
	run(e, w, monday, 24*time.Hour)
	per := map[string]int{}
	for _, en := range e.Log {
		per[en.Cluster] += en.Counts["move"]
	}
	if a, b := per["dc-01/cluster-01"], per["dc-01/cluster-02"]; a < 35 || a > 65 || b < 3*a {
		t.Errorf("cluster-01 follows the vCenter (%d moves, want about 50); cluster-02 is four times busier (%d)", a, b)
	}
	if got := w.load["dc-01/cluster-01"]; got != [2]float64{1, 1} {
		t.Errorf("load of a cluster that follows the vCenter = %v", got)
	}
	s := e.Settings
	s.Global[CPU] = Dial{Level: 2}
	s.Clusters["dc-01/cluster-02"][CPU] = Dial{Level: 0.5}
	e.Apply(monday.Add(25*time.Hour), s, w)
	if a, b := w.load["dc-01/cluster-01"][0], w.load["dc-01/cluster-02"][0]; a != 2 || b != 0.5 {
		t.Errorf("CPU load: cluster-01 = %v (follows 2), cluster-02 = %v (its own 0.5)", a, b)
	}
}

func TestStopSettlesAndNothingMoreHappens(t *testing.T) {
	w := newWorld(4, 8, 25)
	e := started(w, "stop", func(s *Settings) { s.Global[Power] = Dial{Level: 4} })
	run(e, w, monday.Add(9*time.Hour), 2*time.Hour)
	if len(w.off) == 0 || len(w.maint) == 0 {
		t.Fatalf("expected VMs off and hosts in maintenance mode before stopping: %d, %d", len(w.off), len(w.maint))
	}
	at := monday.Add(11*time.Hour + time.Minute)
	e.Stop(at, w)
	if len(w.off) != 0 || len(w.maint) != 0 {
		t.Fatalf("after Stop: %d VMs off, %d hosts in maintenance mode, want none", len(w.off), len(w.maint))
	}
	n := len(e.Log)
	run(e, w, at, 6*time.Hour)
	if len(e.Log) != n {
		t.Fatalf("%d lines were written while stopped", len(e.Log)-n)
	}
}

func TestThreeFailuresStopOneBehaviour(t *testing.T) {
	w := newWorld(1, 8, 25)
	w.fail = errors.New("FileFault: no space left on device")
	e := started(w, "fail", func(s *Settings) { s.Global[Adds] = Dial{Exact: &Exact{N: 5, Unit: UnitMinutes}} })
	run(e, w, monday.Add(9*time.Hour), 3*time.Hour)
	if e.State.Held[Adds] == 0 {
		t.Fatal("adding VMs did not stop itself after failing three times")
	}
	if sum(e.Log, "fail") != 3 || sum(e.Log, "move") == 0 {
		t.Errorf("want 3 failures and everything else carrying on: %d failures, %d moves", sum(e.Log, "fail"), sum(e.Log, "move"))
	}
	w.fail = nil
	e.Resume(Adds)
	run(e, w, monday.Add(12*time.Hour+time.Minute), time.Hour)
	if sum(e.Log, "add") == 0 {
		t.Error("nothing was added after it was switched on again")
	}
}

func TestDisksFillAndAreCleanedUp(t *testing.T) {
	w := newWorld(1, 8, 25)
	e := started(w, "disk", func(s *Settings) { s.Global[DiskGrowth] = Dial{Level: 4} })
	run(e, w, monday, 26*time.Hour+time.Minute)
	var lines []Entry
	for _, en := range e.Log {
		if en.Kind == KindDisk {
			lines = append(lines, en)
		}
	}
	// 2 AM on the first day has almost nothing to report; 2 AM on the second has a day's growth
	if len(lines) != 2 || lines[1].Counts["grownGB"] < 1500 || lines[1].Counts["freedGB"] < 250 {
		t.Fatalf("want a line a day with what filled and what was freed, got %+v", lines)
	}
	if u := w.used["dc-01/cluster-01"]; u < 0.505 || u > 0.53 {
		t.Errorf("after a day at 1.6%% growth and one 0.3%% cleanup the datastore is %.3f full", u)
	}
}

func TestNothingIsCaughtUp(t *testing.T) {
	w := newWorld(4, 8, 25)
	e := started(w, "gap", nil)
	run(e, w, monday.Add(9*time.Hour), time.Hour)
	n := len(e.Log)
	e.Step(monday.Add(40*time.Hour), w) // the simulator was off for more than a day
	if added := len(e.Log) - n; added > 3 {
		t.Fatalf("%d lines were written for the time the simulator was off", added)
	}
}
