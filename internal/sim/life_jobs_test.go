package sim

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/life"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

// Export real SOAP responses for the adapter's Java decoder. Opt-in evidence
// remains outside the repository; no vendor client classes are copied here.
func TestActivityJobWireEvidence(t *testing.T) {
	dir := os.Getenv("VCSIM_JOB_EVIDENCE")
	if dir == "" {
		t.Skip("set VCSIM_JOB_EVIDENCE to a scratch evidence directory")
	}
	s := lifeSim(t, lifeScenario(t), registry.InMemory("wire"))
	l := s.Life()
	srv := httptest.NewServer(s.Handler)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	call := func(name, body string) {
		t.Helper()
		req, err := http.NewRequest("POST", srv.URL+"/sdk", bytes.NewBufferString(`<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body>`+body+`</soapenv:Body></soapenv:Envelope>`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("SOAPAction", `"urn:vim25/9.1.1.0"`)
		req.Header.Set("Content-Type", "text/xml")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: HTTP %d, %v", name, res.StatusCode, err)
		}
		if name != "" {
			if err := os.WriteFile(filepath.Join(dir, name+".xml"), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	call("", `<Login xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this><userName>local-proof</userName><password>local-proof</password></Login>`)
	defer call("", `<Logout xmlns="urn:vim25"><_this type="SessionManager">SessionManager</_this></Logout>`)
	refs := s.Clusters[0]
	hs := l.host(refs.Hosts[0])
	cluster := refs.Cluster.Key
	vm := l.vms[cluster][l.VMNames(cluster)[0]]
	property := func(name string) {
		objects := fmt.Sprintf(`<objectSet><obj type="HostSystem">%s</obj></objectSet><objectSet><obj type="VirtualMachine">%s</obj></objectSet>`, hs.Self.Value, vm.ref.Value)
		for _, old := range l.old {
			objects += `<objectSet><obj type="Task">` + old.ref.Value + `</obj></objectSet>`
		}
		for _, ref := range l.reg.AlarmManager().GetAlarmResponse.Returnval {
			objects += `<objectSet><obj type="Alarm">` + ref.Value + `</obj></objectSet>`
		}
		call(name, `<RetrievePropertiesEx xmlns="urn:vim25"><_this type="PropertyCollector">propertyCollector</_this><specSet><propSet><type>HostSystem</type><pathSet>runtime</pathSet><pathSet>config.product</pathSet><pathSet>triggeredAlarmState</pathSet></propSet><propSet><type>VirtualMachine</type><pathSet>runtime</pathSet><pathSet>snapshot</pathSet></propSet><propSet><type>Task</type><pathSet>info</pathSet></propSet><propSet><type>Alarm</type><pathSet>info</pathSet></propSet>`+objects+`</specSet><options/></RetrievePropertiesEx>`)
	}
	vms, err := l.CrashHost(cluster, hs.Name)
	if err != nil {
		t.Fatal(err)
	}
	property("crash-raised")
	if _, err := l.RestartVMs(cluster, hs.Name, vms); err != nil {
		t.Fatal(err)
	}
	if err := l.RecoverHost(cluster, hs.Name); err != nil {
		t.Fatal(err)
	}
	property("crash-cleared")
	if err := l.SnapshotVM(cluster, vm.vm.Name); err != nil {
		t.Fatal(err)
	}
	property("backup-visible")
	if err := l.RemoveBackup(cluster, vm.vm.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := l.EnterMaintenance(cluster, hs.Name); err != nil {
		t.Fatal(err)
	}
	if err := l.PatchHost(cluster, hs.Name); err != nil {
		t.Fatal(err)
	}
	property("patch-reboot")
	if err := l.FinishReboot(cluster, hs.Name); err != nil {
		t.Fatal(err)
	}
	l.ExitMaintenance(cluster, hs.Name, 1)
	property("patch-finished")
	call("events", `<QueryEvents xmlns="urn:vim25"><_this type="EventManager">EventManager</_this><filter><eventTypeId>TaskEvent</eventTypeId><eventTypeId>HostConnectionLostEvent</eventTypeId><eventTypeId>HostConnectedEvent</eventTypeId><eventTypeId>HostShutdownEvent</eventTypeId><eventTypeId>AlarmStatusChangedEvent</eventTypeId><eventTypeId>VmPoweredOnEvent</eventTypeId><eventTypeId>EnteringMaintenanceModeEvent</eventTypeId><eventTypeId>EnteredMaintenanceModeEvent</eventTypeId><eventTypeId>ExitMaintenanceModeEvent</eventTypeId><eventTypeId>HostDasEnabledEvent</eventTypeId><eventTypeId>HostDasDisabledEvent</eventTypeId><eventTypeId>com.vmware.vc.HA.DasHostFailedEvent</eventTypeId><eventTypeId>com.vmware.vc.HA.HostDasAgentHealthyEvent</eventTypeId><eventTypeId>com.vmware.vc.ha.VmRestartedByHAEvent</eventTypeId><maxCount>1000</maxCount></filter></QueryEvents>`)
	t.Log("exported real SOAP events, snapshot, product, runtime, and raised/cleared alarm properties")
}

// The real world implementation must expose each transition to API readers,
// keep its inventory relationships, and use the same state for performance.
func TestActivityCrashRecoveryAndAlarms(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("jobs"))
	l := s.Life()
	c := l.Clusters()[0]
	hs := l.host(s.Clusters[0].Hosts[0])
	ctx := l.as("")
	vms, err := l.CrashHost(c.Key, hs.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 5 || hs.Runtime.ConnectionState != types.HostSystemConnectionStateNotResponding {
		t.Fatalf("crash: %d VMs, %s", len(vms), hs.Runtime.ConnectionState)
	}
	if len(hs.TriggeredAlarmState) != 2 {
		t.Fatalf("host alarms: %+v", hs.TriggeredAlarmState)
	}
	state := readPerformanceState(ctx)
	if state.nodes[hs.Self].active {
		t.Fatal("crashed host still carries load")
	}
	for _, name := range vms {
		if state.nodes[l.vms[c.Key][name].ref].active {
			t.Fatal("unreachable VM still carries load")
		}
	}
	moves, err := l.RestartVMs(c.Key, hs.Name, vms)
	if err != nil || len(moves) != 5 {
		t.Fatalf("HA: %v, %d moves", err, len(moves))
	}
	state = readPerformanceState(ctx)
	for _, name := range vms {
		vm := l.vms[c.Key][name]
		if l.hostOf(ctx, vm) == hs.Self || !state.nodes[vm.ref].active {
			t.Fatal("HA VM did not restart on another host")
		}
	}
	if err := l.RecoverHost(c.Key, hs.Name); err != nil {
		t.Fatal(err)
	}
	if len(hs.TriggeredAlarmState) != 0 || hs.Runtime.ConnectionState != types.HostSystemConnectionStateConnected {
		t.Fatal("host did not recover and clear alarms")
	}
	var restart, raised, cleared int
	for _, event := range events(s, 0) {
		switch e := event.(type) {
		case *types.EventEx:
			if e.EventTypeId == "com.vmware.vc.ha.VmRestartedByHAEvent" {
				restart++
			}
		case *types.AlarmStatusChangedEvent:
			if e.To == "red" {
				raised++
			}
			if e.To == "green" {
				cleared++
			}
		}
	}
	if restart != 5 || raised != 2 || cleared != 2 {
		t.Fatalf("events: HA %d, raised %d, cleared %d", restart, raised, cleared)
	}
}

func TestActivityBackupAndPatchVisibleAcrossRestore(t *testing.T) {
	sc := lifeScenario(t)
	reg := registry.InMemory("jobs")
	s := lifeSim(t, sc, reg)
	l := s.Life()
	c := l.Clusters()[0]
	name := l.VMNames(c.Key)[0]
	if err := l.SnapshotVM(c.Key, name); err != nil {
		t.Fatal(err)
	}
	vm := l.reg.Get(l.vms[c.Key][name].ref).(*simulator.VirtualMachine)
	if vm.Snapshot == nil || vm.Snapshot.CurrentSnapshot == nil {
		t.Fatal("snapshot is not visible")
	}
	live := l.Snapshot()
	s2 := lifeSim(t, sc, reg)
	l2 := s2.Life()
	l2.Restore(live)
	vm2 := l2.reg.Get(l2.vms[c.Key][name].ref).(*simulator.VirtualMachine)
	if vm2.Snapshot == nil {
		t.Fatal("snapshot disappeared on restore")
	}
	used := func(l *Life, vm *simulator.VirtualMachine) (ds, committed, files int64) {
		d := l.reg.Get(l.s.Clusters[0].Datastore).(*simulator.Datastore)
		for _, f := range vm.LayoutEx.File {
			files += f.Size
		}
		return d.Summary.Capacity - d.Summary.FreeSpace, vm.Storage.PerDatastoreUsage[0].Committed, files
	}
	ds1, c1, f1 := used(l, vm)
	ds2, c2, f2 := used(l2, vm2)
	if ds1 != ds2 || c1 != c2 || f1 != f2 || f2 != c2 || vm2.Summary.Storage.Committed != c2 {
		t.Errorf("restored VM and datastore use %d/%d/%d, before the restart %d/%d/%d", ds2, c2, f2, ds1, c1, f1)
	}
	if err := l2.RemoveBackup(c.Key, name); err != nil {
		t.Fatal(err)
	}
	if ds3, c3, f3 := used(l2, vm2); c3 >= c2 || ds3 >= ds2 || f3 != c3 {
		t.Errorf("removing the restored snapshot left %d/%d/%d, was %d/%d/%d", ds3, c3, f3, ds2, c2, f2)
	}
	if vm2.Snapshot != nil {
		t.Fatal("snapshot not removed")
	}
	if err := l.RemoveBackup(c.Key, name); err != nil {
		t.Fatal(err)
	}
	if vm.Snapshot != nil {
		t.Fatal("original snapshot not removed")
	}
	hs := l.host(s.Clusters[0].Hosts[0])
	before := hs.Config.Product.Build
	if _, err := l.EnterMaintenance(c.Key, hs.Name); err != nil {
		t.Fatal(err)
	}
	if err := l.PatchHost(c.Key, hs.Name); err != nil {
		t.Fatal(err)
	}
	if hs.Config.Product.Build == before || hs.Runtime.ConnectionState != types.HostSystemConnectionStateNotResponding {
		t.Fatal("new build and reboot are not visible")
	}
	if err := l.FinishReboot(c.Key, hs.Name); err != nil {
		t.Fatal(err)
	}
	l.ExitMaintenance(c.Key, hs.Name, 1)
	if hs.Runtime.InMaintenanceMode || hs.Runtime.ConnectionState != types.HostSystemConnectionStateConnected {
		t.Fatal("patched host not back in service")
	}
}

func TestActivityJobsOnCompressedClock(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("clock"))
	w := s.Life()
	settings := life.Defaults(40)
	for k := range settings.Global {
		if !life.IsLoad(k) {
			settings.Global[k] = life.Dial{}
		}
	}
	settings.Global[life.Patching] = life.Dial{Level: 1}
	settings.Global[life.Backups] = life.Dial{Level: 1}
	at := time.Date(2026, 11, 17, 17, 59, 0, 0, time.UTC)
	e := life.New("clock", time.UTC, settings)
	e.Start(at, w)
	for i := 1; i <= 12*60; i++ {
		now := at.Add(time.Duration(i) * time.Minute)
		w.Now = func() time.Time { return now }
		e.Step(now, w)
		w.Sweep(now)
	}
	n := map[string]int{}
	for _, en := range e.Log {
		for k, v := range en.Counts {
			n[k] += v
		}
	}
	if n["patched"] != 4 || n["snapshot"] != 40 || n["snapshotRemoved"] != 40 {
		t.Fatalf("night counts: %v", n)
	}
	for _, c := range w.Clusters() {
		for _, h := range c.Hosts {
			if h.Down || h.Maintenance {
				t.Fatal("host left unhealthy")
			}
		}
	}
	if len(e.State.Jobs.Snapshots) != 0 {
		t.Fatal("backup snapshots remained after night")
	}
}

func TestActivityJobPauseAndStop(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("pause-jobs"))
	w := s.Life()
	c := w.Clusters()[0]
	settings := life.Defaults(40)
	for b := range settings.Global {
		if !life.IsLoad(b) {
			settings.Global[b] = life.Dial{}
		}
	}
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	e := life.New("pause-jobs", time.UTC, settings)
	e.Start(at, w)
	if !e.DoNow(at, w, c.Key, life.Crash) {
		t.Fatal("crash did not start")
	}
	e.Skip(at.Add(time.Hour))
	e.Step(at.Add(61*time.Minute), w)
	if len(e.State.Jobs.Crashes) != 1 || len(e.State.Jobs.Crashes[0].VMs) == 0 {
		t.Fatal("pause consumed HA recovery time")
	}
	e.Stop(at.Add(61*time.Minute), w)
	for _, cl := range w.Clusters() {
		for _, h := range cl.Hosts {
			if h.Down {
				t.Fatal("stop left a crashed host")
			}
		}
	}
	for _, vm := range w.vms[c.Key] {
		obj := w.reg.Get(vm.ref).(*simulator.VirtualMachine)
		if obj.Runtime.ConnectionState != types.VirtualMachineConnectionStateConnected {
			t.Fatal("stop left unreachable VM")
		}
	}
	e.Start(at.Add(62*time.Minute), w)
	if !e.DoNow(at.Add(62*time.Minute), w, c.Key, life.Backups) {
		t.Fatal("backups did not start")
	}
	e.Step(at.Add(63*time.Minute), w)
	if len(e.State.Jobs.Snapshots) == 0 {
		t.Fatal("no visible backup snapshot")
	}
	e.Stop(at.Add(63*time.Minute), w)
	for _, vm := range w.vms[c.Key] {
		if obj := w.reg.Get(vm.ref).(*simulator.VirtualMachine); findBackup(obj) != nil {
			t.Fatal("stop left a backup snapshot")
		}
	}
	e.Start(at.Add(64*time.Minute), w)
	if !e.DoNow(at.Add(64*time.Minute), w, c.Key, life.Patching) {
		t.Fatal("patch did not start")
	}
	for i := 65; i <= 92; i++ {
		e.Step(at.Add(time.Duration(i)*time.Minute), w)
	}
	if e.State.Jobs.Patch == nil || !e.State.Jobs.Patch.Reboot {
		t.Fatal("patch did not enter reboot phase")
	}
	e.Stop(at.Add(92*time.Minute), w)
	for _, cl := range w.Clusters() {
		for _, h := range cl.Hosts {
			if h.Down || h.Maintenance {
				t.Fatal("stop left patching host unavailable")
			}
		}
	}
}

func TestActivityPatchAndCrashShareHostLimit(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("host-limit"))
	w := s.Life()
	settings := life.Defaults(40)
	for b := range settings.Global {
		if !life.IsLoad(b) {
			settings.Global[b] = life.Dial{}
		}
	}
	at := time.Date(2026, 11, 17, 17, 59, 0, 0, time.UTC)
	e := life.New("host-limit", time.UTC, settings)
	e.Start(at, w)
	settings = e.Settings
	settings.Global[life.Patching] = life.Dial{Level: 1}
	cluster := w.Clusters()[0].Key
	settings.Clusters = map[string]map[string]life.Dial{cluster: {life.Crash: {Exact: &life.Exact{N: 1, Unit: life.UnitMinutes}}}}
	e.State.Acc = map[string]float64{cluster + "|" + life.Crash: 1}
	e.Apply(at, settings, w)
	// Every minute a crash is due, including the minute patching starts.
	for i := 1; i <= 35; i++ {
		e.Step(at.Add(time.Duration(i)*time.Minute), w)
		for _, c := range w.Clusters() {
			out := 0
			for _, h := range c.Hosts {
				if !h.InService() {
					out++
				}
			}
			if out > len(c.Hosts)/4 {
				t.Fatalf("%s has %d of %d hosts out", c.Key, out, len(c.Hosts))
			}
		}
	}
}

func TestActivityVMsOnADownHostWaitAndAreNotFailures(t *testing.T) {
	s := lifeSim(t, lifeScenario(t), registry.InMemory("down-host"))
	w := s.Life()
	c := w.Clusters()[0]
	settings := life.Defaults(40)
	for b := range settings.Global {
		if !life.IsLoad(b) {
			settings.Global[b] = life.Dial{}
		}
	}
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	e := life.New("down-host", time.UTC, settings)
	e.Start(at, w)
	name := w.VMNames(c.Key)[0]
	ctx := w.as(lifeUser)
	host := w.host(w.hostOf(ctx, w.vms[c.Key][name])).Name
	if !w.power(c.Key, name, false) {
		t.Fatal("could not power the VM off")
	}
	if _, err := w.CrashHost(c.Key, host); err != nil {
		t.Fatal(err)
	}
	if err := w.PowerOn(c.Key, name); !errors.Is(err, life.ErrHostUnavailable) {
		t.Fatalf("power on with the host down: %v", err)
	}
	if err := w.SnapshotVM(c.Key, name); !errors.Is(err, life.ErrHostUnavailable) {
		t.Fatalf("snapshot with the host down: %v", err)
	}
	if !e.DoNow(at, w, c.Key, life.Backups) {
		t.Fatal("backups did not start")
	}
	for i := 1; i <= 10; i++ {
		e.Step(at.Add(time.Duration(i)*time.Minute), w)
	}
	if e.State.Held[life.Backups] != 0 || e.State.Fails[life.Backups] != 0 {
		t.Fatalf("a VM on a down host counted as a failure: held %v, fails %v", e.State.Held, e.State.Fails)
	}
	if err := w.RecoverHost(c.Key, host); err != nil {
		t.Fatal(err)
	}
	if err := w.PowerOn(c.Key, name); err != nil {
		t.Fatalf("power on after the host is back: %v", err)
	}
}
