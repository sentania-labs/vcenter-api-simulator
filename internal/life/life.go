// Package life makes the simulated environment change by itself: VMs move
// between hosts, are powered off and on, come and go; hosts go into and out
// of maintenance mode; datastores fill and are cleaned up; and the whole
// place looks more or less loaded.
//
// The package decides what happens and when. It knows nothing about vcsim:
// the inventory it acts on is a World, which package sim implements on the
// running vCenter. Everything it decides is a function of its settings, its
// saved state, the seed and the clock it is stepped with, so the same inputs
// do the same things in the same order.
//
// How much happens follows the size of the environment. Each behaviour has
// a baseline (so many per 100 VMs a day, a share of the hosts, a share of
// datastore capacity) and a dial around it, for the whole vCenter and, where
// one is set, for a single cluster. A dial can also carry an exact number in
// place of a level.
package life

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"time"
)

// The behaviours. Each has one dial.
const (
	Patching    = "patching"
	Backups     = "backups"
	Crash       = "crash"
	Moves       = "moves"       // load balancing: VMs move between hosts of a cluster
	Power       = "power"       // VMs are powered off, and on again a little later
	Adds        = "adds"        // new VMs
	Removes     = "removes"     // VMs removed
	Maintenance = "maintenance" // hosts kept in maintenance mode, swapped now and then
	DiskGrowth  = "diskGrowth"  // datastores fill
	DiskShrink  = "diskShrink"  // datastores are cleaned up
	CPU         = "cpu"         // how busy the VMs' CPUs look, and how much they contend
	Memory      = "memory"      // how much memory the VMs use
)

// Order lists the behaviours as the console shows them.
var Order = []string{Moves, Power, Adds, Removes, Maintenance, Patching, Backups, Crash, DiskGrowth, DiskShrink, CPU, Memory}

// Counted are the behaviours whose baseline is a number of things per 100
// VMs a day.
var Counted = []string{Moves, Power, Adds, Removes}

// Baseline is what a dial at level 1 means.
var Baseline = struct {
	Per100       map[string]float64 // things per 100 VMs in a working day
	MaintPercent float64            // share of hosts kept in maintenance mode
	MaintSwap    time.Duration      // how long a host stays in before another takes its place
	GrowPercent  float64            // datastore capacity filled in a day
	ShrinkPct    float64            // datastore capacity freed by the nightly cleanup
}{
	Per100:       map[string]float64{Moves: 25, Power: 2, Adds: 1, Removes: 1},
	MaintPercent: 5,
	MaintSwap:    7 * 24 * time.Hour,
	GrowPercent:  0.4,
	ShrinkPct:    0.3,
}

// Levels are the positions of a dial: off, two slower, the baseline, two
// busier. LoadLevels are the positions of the two load dials, which cannot
// be off.
var (
	Levels     = []float64{0, 0.25, 0.5, 1, 2, 4}
	LoadLevels = []float64{0.5, 0.75, 1, 1.25, 1.5, 2}
)

// Units of an exact number.
const (
	UnitMinutes = "minutes" // one every N minutes
	UnitHours   = "hours"   // one every N hours
	UnitDays    = "days"    // one every N days
	UnitPerDay  = "perDay"  // N a day
	UnitPercent = "percent" // N percent (of the hosts, or of capacity a day)
)

// Exact is a number typed in place of a level. It stays as typed whatever
// the size of the environment.
type Exact struct {
	N    float64 `json:"n"`
	Unit string  `json:"unit"`
}

// Dial is one behaviour's setting: a level around the baseline, or an
// exact number.
type Dial struct {
	RecoveryMinutes int     `json:"recoveryMinutes,omitempty"`
	Level           float64 `json:"level"`
	Exact           *Exact  `json:"exact,omitempty"`
}

// Settings are everything the owner chose.
type Settings struct {
	// Running is false while activity is stopped: the settings are kept and
	// nothing happens.
	Running bool `json:"running"`
	// Global is the dial of each behaviour for the whole vCenter.
	Global map[string]Dial `json:"global"`
	// Clusters holds a cluster's own dials, by cluster key. A behaviour a
	// cluster has no dial for follows Global.
	Clusters map[string]map[string]Dial `json:"clusters,omitempty"`
	// MinVMs and MaxVMs bound the number of VMs in the whole vCenter; adding
	// waits at the most and removing at the fewest.
	MinVMs int `json:"minVMs"`
	MaxVMs int `json:"maxVMs"`
}

// Defaults are the settings of a normal day: every behaviour at its
// baseline, and the number of VMs kept within 5% of what there is.
func Defaults(vms int) Settings {
	s := Settings{Global: map[string]Dial{}}
	for _, k := range Order {
		s.Global[k] = Dial{Level: 1}
	}
	s.Global[Crash] = Dial{}
	s.MinVMs, s.MaxVMs = Bounds(vms)
	return s
}

// Bounds is the default floor and ceiling for a number of VMs: 5% either
// way, and always room for at least one.
func Bounds(vms int) (minVMs, maxVMs int) {
	d := max(1, int(math.Round(float64(vms)*0.05)))
	return max(0, vms-d), vms + d
}

// Dial returns the dial in force for a behaviour in a cluster, and whether
// it is the cluster's own.
func (s *Settings) Dial(cluster, behaviour string) (Dial, bool) {
	if d, ok := s.Clusters[cluster][behaviour]; ok {
		return d, true
	}
	if d, ok := s.Global[behaviour]; ok {
		return d, false
	}
	if behaviour == Crash {
		return Dial{}, false
	}
	return Dial{Level: 1}, false
}

// IsLoad reports whether a behaviour is one of the two load dials.
func IsLoad(behaviour string) bool { return behaviour == CPU || behaviour == Memory }

// ExactUnits lists the units an exact number of a behaviour may have; none
// means the behaviour takes a level only.
func ExactUnits(behaviour string) []string {
	switch behaviour {
	case Moves:
		return []string{UnitPerDay}
	case Patching, Backups:
		return []string{UnitDays}
	case Power, Adds, Removes, Crash:
		return []string{UnitMinutes, UnitHours, UnitDays}
	case Maintenance, DiskGrowth, DiskShrink:
		return []string{UnitPercent}
	}
	return nil
}

// Check says what is wrong with a dial for a behaviour, or nothing.
func (d Dial) Check(behaviour string) error {
	if d.RecoveryMinutes < 0 || d.RecoveryMinutes > 1440 {
		return fmt.Errorf("host recovery must be 1 to 1440 minutes")
	}
	if behaviour != Crash && d.RecoveryMinutes != 0 {
		return fmt.Errorf("recovery time belongs to host crashes")
	}
	if d.Exact == nil {
		lo, hi := 0.0, 4.0
		if IsLoad(behaviour) {
			lo, hi = 0.5, 2
		}
		if math.IsNaN(d.Level) || d.Level < lo || d.Level > hi {
			return fmt.Errorf("the level must be between %g and %g", lo, hi)
		}
		return nil
	}
	ok := false
	for _, u := range ExactUnits(behaviour) {
		ok = ok || u == d.Exact.Unit
	}
	if !ok {
		return fmt.Errorf("that kind of number does not fit this")
	}
	n := d.Exact.N
	switch {
	case math.IsNaN(n) || math.IsInf(n, 0) || n <= 0:
		return fmt.Errorf("type a number above 0")
	case d.Exact.Unit == UnitPercent && behaviour == Maintenance && n > 25:
		return fmt.Errorf("at most 25: more than a quarter of the hosts would leave VMs nowhere to run")
	case d.Exact.Unit == UnitPercent && behaviour != Maintenance && n > 10:
		return fmt.Errorf("at most 10 percent of capacity a day")
	case d.Exact.Unit == UnitDays && behaviour != Patching && n > 30:
		return fmt.Errorf("the longest is every 30 days")
	case d.Exact.Unit == UnitDays && n > 120:
		return fmt.Errorf("the longest is every 120 days")
	case d.Exact.Unit == UnitPerDay && n > 100000:
		return fmt.Errorf("at most 100,000 a day")
	case (d.Exact.Unit == UnitMinutes || d.Exact.Unit == UnitHours || d.Exact.Unit == UnitDays) && n != math.Trunc(n):
		return fmt.Errorf("type a whole number")
	}
	return nil
}

// perDayExact is how many a day an exact number comes to.
func (e Exact) perDay() float64 {
	switch e.Unit {
	case UnitMinutes:
		return 1440 / e.N
	case UnitHours:
		return 24 / e.N
	case UnitDays:
		return 1 / e.N
	}
	return e.N
}

// Host is one host as the engine sees it.
type Host struct {
	Name        string
	VMs         int
	Maintenance bool
	Down        bool
}

// InService reports whether a host can run VMs.
func (h Host) InService() bool { return !h.Maintenance && !h.Down }

// Cluster is one cluster as the engine sees it.
type Cluster struct {
	Key, Name string
	Hosts     []Host
	VMs       int // all VMs, powered on or not
	Off       int // of them, powered off
}

// Move is one VM that changed host.
type Move struct{ VM, From, To string }

// Store is what a change to a cluster's datastores came to.
type Store struct {
	Name        string
	Bytes       int64 // how much the used space changed by
	UsedPercent int   // how full it is now
}

// ErrHostUnavailable says a VM cannot be acted on now because its host is
// down or in maintenance mode. It passes, so the work is tried again later
// and is not counted as a failure.
var ErrHostUnavailable = errors.New("VM host is not available")

// World is the inventory the engine acts on. Every method changes the
// running vCenter in place, with the events and tasks a real one records,
// and reports what it did. A pick is a number the engine drew; a World uses
// it wherever it has a choice, so the same pick over the same inventory
// makes the same choice.
type World interface {
	Clusters() []Cluster
	// Balance moves up to n running VMs of a cluster from its fullest hosts
	// to its emptiest.
	Balance(cluster string, n int, pick uint64) []Move
	// PowerOff powers one running VM off and names it; "" when none can be.
	PowerOff(cluster string, pick uint64) string
	// PowerOn powers a VM on again. ErrHostUnavailable means its host is
	// down or in maintenance mode and the VM should be tried again later;
	// any other error means it is no longer there.
	PowerOn(cluster, vm string) error
	AddVM(cluster string, pick uint64) (vm, host string, err error)
	RemoveVM(cluster string, pick uint64) (vm string, err error)
	// EnterMaintenance moves every VM off a host and puts it in maintenance
	// mode. PickHost names an in-service host the engine may use.
	EnterMaintenance(cluster, host string) ([]Move, error)
	// ExitMaintenance takes a host out of maintenance mode and moves some
	// VMs back onto it.
	ExitMaintenance(cluster, host string, pick uint64) []Move
	// Fill changes how full the cluster's datastores are by a share of
	// their capacity, positive or negative, within what a datastore may hold.
	Fill(cluster string, share float64) []Store
	// SetLoad says how loaded the cluster's VMs look: 1 is as built.
	SetLoad(cluster string, cpu, memory float64)
}

// pick draws a number for the k-th occurrence of something. It is the only
// source of choice in the package.
func pick(seed, cluster, what string, k uint64) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d", seed, cluster, what, k)
	// fnv alone leaves the low bits of close inputs close; mix them
	x := h.Sum64()
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return x
}

// share of a working day's count that falls in the hour of t, for the
// behaviours people cause (power, new VMs, removed VMs): nine in ten
// between 8 and 6 on a working day, a few in the evening and early
// morning, none in the small hours; a tenth of a working day at weekends.
func workShape(t time.Time) float64 {
	h := t.Hour()
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		if h >= 9 && h < 18 {
			return 0.1 / 9
		}
		return 0
	}
	switch {
	case h >= 8 && h < 18:
		return 0.09
	case h >= 18 && h < 23:
		return 0.016
	case h >= 6 && h < 8:
		return 0.01
	}
	return 0
}

// balanceShape is the same for load balancing, which follows the load: most
// as the working day starts, at lunch, in the early evening and late
// evening; little overnight; half as much at weekends, evenly.
var balanceHours = [24]float64{0.0114, 0.0114, 0.0114, 0.0114, 0.0114, 0.0115, 0.0115, 0.06, 0.12, 0.10, 0.05, 0.04, 0.07, 0.06, 0.04, 0.04, 0.05, 0.08, 0.06, 0.03, 0.02, 0.02, 0.05, 0.03}

func balanceShape(t time.Time) float64 {
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return 0.5 / 24
	}
	return balanceHours[t.Hour()]
}
