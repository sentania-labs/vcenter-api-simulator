package life

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// JobWorld implements operations that remain visible between engine steps.
type JobWorld interface {
	World
	VMNames(cluster string) []string
	SnapshotVM(cluster, vm string) error
	RemoveBackup(cluster, vm string) error
	PatchHost(cluster, host string) error
	FinishReboot(cluster, host string) error
	CrashHost(cluster, host string) ([]string, error)
	RestartVMs(cluster, host string, vms []string) ([]Move, error)
	RecoverHost(cluster, host string) error
}

type PatchJob struct {
	Cluster string   `json:"cluster"`
	Pending []string `json:"pending"`
	Host    string   `json:"host,omitempty"`
	Due     int64    `json:"due"`
	Reboot  bool     `json:"reboot,omitempty"`
}
type BackupJob struct {
	Cluster string   `json:"cluster"`
	Pending []string `json:"pending"`
	Batch   int      `json:"batch"`
	Manual  bool     `json:"manual,omitempty"`
}
type Backup struct {
	Cluster string `json:"cluster"`
	VM      string `json:"vm"`
	Until   int64  `json:"until"`
}
type CrashJob struct {
	Cluster string   `json:"cluster"`
	Host    string   `json:"host"`
	VMs     []string `json:"vms,omitempty"`
	Restart int64    `json:"restart"`
	Return  int64    `json:"return"`
}
type Jobs struct {
	Next      map[string]int64 `json:"next,omitempty"`
	Patch     *PatchJob        `json:"patch,omitempty"`
	Backup    *BackupJob       `json:"backup,omitempty"`
	Snapshots []Backup         `json:"snapshots,omitempty"`
	Crashes   []CrashJob       `json:"crashes,omitempty"`
}

func Recovery(d Dial) int {
	if d.RecoveryMinutes > 0 {
		return d.RecoveryMinutes
	}
	return 15
}

// PeriodDays scales calendar jobs, keeping backups to at most once a night.
func PeriodDays(b string, d Dial) int {
	if d.Exact != nil {
		return int(d.Exact.N)
	}
	if d.Level <= 0 {
		return 0
	}
	base := 30.0
	if b == Backups {
		base = 1
	}
	return max(1, int(math.Ceil(base/d.Level)))
}

func (e *Engine) jobWait(t int64, c Cluster, b, why string) {
	key := c.Key + "|" + b
	if e.State.Waited == nil {
		e.State.Waited = map[string]int64{}
	}
	if last, ok := e.State.Waited[key]; ok && t-last < 60 {
		return
	}
	e.State.Waited[key] = t
	e.add(t, Entry{Kind: KindWait, Cluster: c.Key, Why: b, Text: why, Counts: map[string]int{"wait": 1}})
}

func (e *Engine) startJob(t int64, world World, c Cluster, b string) bool {
	w, ok := world.(JobWorld)
	if !ok || e.State.Held[b] != 0 {
		return false
	}
	j := &e.State.Jobs
	switch b {
	case Patching:
		if j.Patch != nil || (j.Backup != nil && e.State.Held[Backups] == 0) || len(j.Snapshots) > 0 {
			e.jobWait(t, c, b, "Patching is waiting for the current patch or backup to finish.")
			return false
		}
		if room(c) == 0 {
			e.jobWait(t, c, b, "Patching needs at least four hosts so no more than a quarter are out.")
			return false
		}
		pending := make([]string, 0, len(c.Hosts))
		for _, h := range c.Hosts {
			pending = append(pending, h.Name)
		}
		sort.Strings(pending)
		j.Patch = &PatchJob{Cluster: c.Key, Pending: pending}
		e.add(t, Entry{Kind: "patch", Cluster: c.Key, Why: b, Text: "Started patching " + c.Name + ", one host at a time."})
		e.patchStep(t, w, c)
	case Backups:
		if (j.Patch != nil && e.State.Held[Patching] == 0) || j.Backup != nil || len(j.Snapshots) > 0 {
			e.jobWait(t, c, b, "Backups are waiting for the current patch or backup to finish.")
			return false
		}
		names := w.VMNames(c.Key)
		j.Backup = &BackupJob{Cluster: c.Key, Pending: names, Batch: max(1, (len(names)+29)/30), Manual: e.you}
		e.add(t, Entry{Kind: "backup", Cluster: c.Key, Why: b, Text: "Started backups in " + c.Name + ". Each snapshot stays for ten minutes."})
	case Crash:
		// A patch may have taken a host out earlier in this same minute.
		// Enforce the shared limit against the live inventory, not that minute's
		// initial cluster view.
		for _, current := range clusters(w) {
			if current.Key == c.Key {
				c = current
				break
			}
		}
		var free []string
		out := 0
		for _, h := range c.Hosts {
			if h.InService() {
				free = append(free, h.Name)
			} else {
				out++
			}
		}
		if out >= room(c) || len(free) < 2 {
			e.jobWait(t, c, b, "Host crash skipped: a quarter of the cluster's hosts would be out of action.")
			return false
		}
		sort.Strings(free)
		host := free[e.next(c.Key, b)%uint64(len(free))]
		vms, err := w.CrashHost(c.Key, host)
		if err != nil {
			e.failed(t, b, c, reason(err))
			return false
		}
		d, _ := e.Settings.Dial(c.Key, b)
		j.Crashes = append(j.Crashes, CrashJob{Cluster: c.Key, Host: host, VMs: vms, Restart: t + int64(min(2, Recovery(d))), Return: t + int64(Recovery(d))})
		e.add(t, Entry{Kind: "crash", Cluster: c.Key, Why: b, Text: fmt.Sprintf("%s stopped responding. Host alarms were raised. HA will restart %s within two minutes; the host returns in %d minutes.", short(host), count(len(vms), "VM"), Recovery(d)), Counts: map[string]int{"crash": 1}, Details: vms})
	}
	delete(e.State.Fails, b)
	return true
}

func (e *Engine) patchStep(t int64, w JobWorld, c Cluster) {
	p := e.State.Jobs.Patch
	if p == nil || p.Cluster != c.Key || e.State.Held[Patching] != 0 {
		return
	}
	if p.Host != "" {
		if t < p.Due {
			return
		}
		if !p.Reboot {
			if err := w.PatchHost(c.Key, p.Host); err != nil {
				e.failed(t, Patching, c, reason(err))
				return
			}
			delete(e.State.Fails, Patching)
			p.Reboot = true
			p.Due = t + 2
			e.add(t, Entry{Kind: "patch", Cluster: c.Key, Why: Patching, Text: short(p.Host) + " has a new build and is rebooting."})
			return
		}
		if err := w.FinishReboot(c.Key, p.Host); err != nil {
			e.failed(t, Patching, c, reason(err))
			return
		}
		moves := w.ExitMaintenance(c.Key, p.Host, e.next(c.Key, Patching))
		e.add(t, Entry{Kind: "patch", Cluster: c.Key, Why: Patching, Text: fmt.Sprintf("%s finished patching and left maintenance mode. %s moved back.", short(p.Host), count(len(moves), "VM")), Counts: map[string]int{"patched": 1, "mout": 1, "move": len(moves)}, Details: moveLines(moves)})
		delete(e.State.Fails, Patching)
		p.Host = ""
		// Refresh the host state after bringing this host back.
		for _, live := range clusters(w) {
			if live.Key == c.Key {
				c = live
				break
			}
		}
	}
	if len(p.Pending) == 0 {
		e.State.Jobs.Patch = nil
		return
	}
	out := 0
	live := map[string]Host{}
	for _, h := range c.Hosts {
		live[h.Name] = h
		if !h.InService() {
			out++
		}
	}
	// Make room by returning an Activity-owned maintenance host. Patching
	// must not wait a week for the normal maintenance rotation.
	for _, m := range e.State.Maint {
		if m.Cluster != c.Key {
			continue
		}
		needed := out >= room(c)
		for _, h := range p.Pending {
			if h == m.Host {
				needed = true
			}
		}
		if needed {
			moves := w.ExitMaintenance(c.Key, m.Host, e.next(c.Key, Patching))
			e.dropMaint(c.Key, m.Host)
			out--
			h := live[m.Host]
			h.Maintenance = false
			live[m.Host] = h
			e.add(t, Entry{Kind: KindOut, Cluster: c.Key, Why: Patching, Text: short(m.Host) + " left its maintenance rotation so patching can take its turn.", Counts: map[string]int{"mout": 1, "move": len(moves)}, Details: moveLines(moves)})
			break
		}
	}
	if out >= room(c) {
		e.jobWait(t, c, Patching, "Patching is waiting: a quarter of the hosts are already out of action.")
		return
	}
	for i, host := range p.Pending {
		h, exists := live[host]
		if !exists {
			p.Pending = append(p.Pending[:i], p.Pending[i+1:]...)
			return
		}
		if !h.InService() {
			continue
		}
		moves, err := w.EnterMaintenance(c.Key, host)
		if err != nil {
			e.failed(t, Patching, c, reason(err))
			return
		}
		p.Pending = append(p.Pending[:i], p.Pending[i+1:]...)
		p.Host = host
		p.Reboot = false
		p.Due = t + 28
		e.add(t, Entry{Kind: "patch", Cluster: c.Key, Why: Patching, Text: fmt.Sprintf("%s entered maintenance mode for patching. %s evacuated first.", short(host), count(len(moves), "VM")), Counts: map[string]int{"min": 1, "move": len(moves)}, Details: moveLines(moves)})
		return
	}
}

func (e *Engine) jobs(t int64, world World) {
	w, ok := world.(JobWorld)
	if !ok {
		return
	}
	j := &e.State.Jobs
	if j.Next == nil {
		j.Next = map[string]int64{}
	}
	cs := clusters(w)
	by := map[string]Cluster{}
	for _, c := range cs {
		by[c.Key] = c
	}
	var crashes []CrashJob
	for _, x := range j.Crashes {
		c, exists := by[x.Cluster]
		if !exists {
			continue
		}
		if x.Restart <= t && len(x.VMs) > 0 {
			moves, err := w.RestartVMs(x.Cluster, x.Host, x.VMs)
			if err != nil {
				e.failed(t, Crash, c, reason(err))
				crashes = append(crashes, x)
				continue
			}
			e.add(t, Entry{Kind: "crash", Cluster: x.Cluster, Why: Crash, Text: fmt.Sprintf("HA restarted %s from %s on other hosts.", count(len(moves), "VM"), short(x.Host)), Counts: map[string]int{"restart": len(moves)}, Details: moveLines(moves)})
			x.VMs = nil
		}
		if x.Return <= t && len(x.VMs) == 0 {
			if err := w.RecoverHost(x.Cluster, x.Host); err != nil {
				e.failed(t, Crash, c, reason(err))
				crashes = append(crashes, x)
				continue
			}
			moves := w.Balance(x.Cluster, max(1, c.VMs/max(1, len(c.Hosts))/2), e.next(x.Cluster, Crash+"/return"))
			e.add(t, Entry{Kind: "crash", Cluster: x.Cluster, Why: Crash, Text: short(x.Host) + " is responding again. Its host alarms cleared, and DRS balanced the load again.", Counts: map[string]int{"recovered": 1, "move": len(moves)}, Details: moveLines(moves)})
		} else {
			crashes = append(crashes, x)
		}
	}
	j.Crashes = crashes
	var kept []Backup
	for _, s := range j.Snapshots {
		if s.Until > t {
			kept = append(kept, s)
			continue
		}
		if err := w.RemoveBackup(s.Cluster, s.VM); err != nil {
			e.failed(t, Backups, by[s.Cluster], reason(err))
			kept = append(kept, s)
			continue
		}
		e.backupLog(t, s.Cluster, s.VM, false)
	}
	j.Snapshots = kept
	if j.Patch != nil {
		if c, exists := by[j.Patch.Cluster]; exists {
			e.patchStep(t, w, c)
		} else {
			j.Patch = nil
		}
	}
	at := time.Unix(t*60, 0).In(e.Loc)
	if b := j.Backup; b != nil && e.State.Held[Backups] == 0 {
		window := b.Manual || at.Hour() >= 22 || at.Hour() < 3 || (at.Hour() == 3 && at.Minute() < 50)
		if window {
			live := map[string]bool{}
			for _, vm := range w.VMNames(b.Cluster) {
				live[vm] = true
			}
			n := min(b.Batch, len(b.Pending))
			retry, later := []string{}, []string{}
			for i, vm := range b.Pending[:n] {
				if !live[vm] {
					continue
				}
				if err := w.SnapshotVM(b.Cluster, vm); errors.Is(err, ErrHostUnavailable) {
					later = append(later, vm)
					continue
				} else if err != nil {
					e.failed(t, Backups, by[b.Cluster], reason(err))
					retry = append(retry, vm)
					if e.State.Held[Backups] != 0 {
						retry = append(retry, b.Pending[i+1:n]...)
						break
					}
					continue
				}
				delete(e.State.Fails, Backups)
				j.Snapshots = append(j.Snapshots, Backup{b.Cluster, vm, t + 10})
				e.backupLog(t, b.Cluster, vm, true)
			}
			b.Pending = append(append(retry, b.Pending[n:]...), later...)
			if len(b.Pending) == 0 {
				j.Backup = nil
			}
		}
	}

	for i, c := range cs {
		for _, b := range []string{Patching, Backups} {
			d, _ := e.Settings.Dial(c.Key, b)
			days := PeriodDays(b, d)
			if days == 0 || e.State.Held[b] != 0 {
				continue
			}
			key := c.Key + "|" + b
			next, exists := j.Next[key]
			if !exists {
				hour := 18
				offset := i
				if b == Backups {
					hour = 22
					offset = 0
				}
				next = time.Date(at.Year(), at.Month(), at.Day()+offset, hour, 0, 0, 0, e.Loc).Unix() / 60
				if b == Backups && at.Hour() < 4 {
					next = time.Date(at.Year(), at.Month(), at.Day()-1, 22, 0, 0, 0, e.Loc).Unix() / 60
				}
				j.Next[key] = next
			}
			window := at.Hour() >= 18
			if b == Backups {
				window = at.Hour() >= 22 || at.Hour() < 3 || (at.Hour() == 3 && at.Minute() < 20)
			}
			if t >= next && window && e.startJob(t, w, c, b) {
				hour := 18
				date := at
				if b == Backups {
					hour = 22
					if at.Hour() < 4 {
						date = at.AddDate(0, 0, -1)
					}
				}
				j.Next[key] = time.Date(date.Year(), date.Month(), date.Day()+days, hour, 0, 0, 0, e.Loc).Unix() / 60
			}
		}
	}
	// One global crash stream shared by follower clusters, with independent overrides.
	g, _ := e.Settings.Dial("", Crash)
	rate := g.Level
	if g.Exact != nil {
		rate = g.Exact.perDay()
	}
	if c, ok := e.follower(cs, Crash); ok && e.due(t, "*", Crash, rate, true) {
		e.startJob(t, w, c, Crash)
	}
	for _, c := range cs {
		d, own := e.Settings.Dial(c.Key, Crash)
		if !own {
			continue
		}
		rate := d.Level
		if d.Exact != nil {
			rate = d.Exact.perDay()
		}
		if e.due(t, c.Key, Crash, rate, true) {
			e.startJob(t, w, c, Crash)
		}
	}
}

// A half-hour line groups the snapshot work without consuming thousands of log entries.
func (e *Engine) backupLog(t int64, cluster, vm string, taken bool) {
	key := "snapshotRemoved"
	verb := "removed"
	if taken {
		key = "snapshot"
		verb = "taken"
	}
	for i := len(e.Log) - 1; i >= 0; i-- {
		en := &e.Log[i]
		if en.At.Unix()/1800 != t/30 {
			break
		}
		if en.Kind == "backup" && en.Cluster == cluster && en.Counts[key] > 0 {
			en.Counts[key]++
			en.Details = append(en.Details, vm)
			en.Text = fmt.Sprintf("%s %s in this half hour.", count(en.Counts[key], "backup snapshot"), verb)
			return
		}
	}
	e.add(t, Entry{Kind: "backup", Cluster: cluster, Why: Backups, Text: "1 backup snapshot " + verb + " in this half hour.", Counts: map[string]int{key: 1}, Details: []string{vm}})
}

func (e *Engine) stopJobs(t int64, world World) {
	w, ok := world.(JobWorld)
	if !ok {
		return
	}
	j := &e.State.Jobs
	by := map[string]Cluster{}
	for _, c := range clusters(w) {
		by[c.Key] = c
	}
	var crashes []CrashJob
	for _, x := range j.Crashes {
		if err := w.RecoverHost(x.Cluster, x.Host); err != nil {
			e.failed(t, Crash, by[x.Cluster], reason(err))
			crashes = append(crashes, x)
			continue
		}
		e.add(t, Entry{Kind: "crash", Cluster: x.Cluster, Why: Crash, Text: short(x.Host) + " returned early because activity stopped. Its alarms cleared.", Counts: map[string]int{"recovered": 1}})
	}
	j.Crashes = crashes
	if p := j.Patch; p != nil {
		p.Pending = nil
		if p.Host != "" {
			var err error
			if p.Reboot {
				err = w.FinishReboot(p.Cluster, p.Host)
			}
			if err != nil {
				e.failed(t, Patching, by[p.Cluster], reason(err))
			} else {
				moves := w.ExitMaintenance(p.Cluster, p.Host, e.next(p.Cluster, Patching))
				e.add(t, Entry{Kind: "patch", Cluster: p.Cluster, Why: Patching, Text: short(p.Host) + " left maintenance mode because activity stopped.", Counts: map[string]int{"mout": 1, "move": len(moves)}, Details: moveLines(moves)})
				j.Patch = nil
			}
		} else {
			j.Patch = nil
		}
	}
	var snapshots []Backup
	for _, s := range j.Snapshots {
		if err := w.RemoveBackup(s.Cluster, s.VM); err != nil {
			e.failed(t, Backups, by[s.Cluster], reason(err))
			snapshots = append(snapshots, s)
			continue
		}
		e.backupLog(t, s.Cluster, s.VM, false)
	}
	j.Snapshots = snapshots
	j.Backup = nil
}

func (e *Engine) reconcileJobs(w World) {
	hosts := map[string]map[string]bool{}
	for _, c := range clusters(w) {
		hosts[c.Key] = map[string]bool{}
		for _, h := range c.Hosts {
			hosts[c.Key][h.Name] = true
		}
	}
	j := &e.State.Jobs
	if p := j.Patch; p != nil {
		if hosts[p.Cluster] == nil {
			j.Patch = nil
		} else {
			if !hosts[p.Cluster][p.Host] {
				p.Host = ""
			}
			var pending []string
			for _, h := range p.Pending {
				if hosts[p.Cluster][h] {
					pending = append(pending, h)
				}
			}
			p.Pending = pending
		}
	}
	if b := j.Backup; b != nil && hosts[b.Cluster] == nil {
		j.Backup = nil
	}
	var crashes []CrashJob
	for _, c := range j.Crashes {
		if hosts[c.Cluster][c.Host] {
			crashes = append(crashes, c)
		}
	}
	j.Crashes = crashes
	var snapshots []Backup
	for _, b := range j.Snapshots {
		if hosts[b.Cluster] != nil {
			snapshots = append(snapshots, b)
		}
	}
	j.Snapshots = snapshots
}

// RecoverExpired is the restart exception to "no catch-up": a failed host
// whose return time passed while the process was off is healthy at startup.
// It does not replay ordinary VM activity, patches, or backup work.
func (e *Engine) RecoverExpired(now time.Time, world World) {
	w, ok := world.(JobWorld)
	if !ok {
		return
	}
	t := now.Unix() / 60
	var kept []CrashJob
	for _, x := range e.State.Jobs.Crashes {
		if x.Return > t {
			kept = append(kept, x)
			continue
		}
		if err := w.RecoverHost(x.Cluster, x.Host); err != nil {
			kept = append(kept, x)
			continue
		}
		e.add(t, Entry{Kind: "crash", Cluster: x.Cluster, Why: Crash, Text: short(x.Host) + " returned as the simulator started; its recovery time had passed and its alarms cleared.", Counts: map[string]int{"recovered": 1}})
	}
	e.State.Jobs.Crashes = kept
}

// JobsPending reports cleanup still outstanding after Stop.
func (e *Engine) JobsPending() bool {
	j := e.State.Jobs
	return j.Patch != nil || j.Backup != nil || len(j.Crashes) > 0 || len(j.Snapshots) > 0
}
