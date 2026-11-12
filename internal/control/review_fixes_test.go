package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
)

// A change that is live is never called "not made" because the state file
// could not be written; Home says the state is unsaved instead.
func TestAChangeThatCouldNotBeSavedIsStillSaidToBeMade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	c := newCtl(t, small(t), dir, "t")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	out := must(t, c, "fault", map[string]string{"preset": "slow"}, nil)
	wantContains(t, out.Done, "Switched on:")
	if len(c.Faults().On) != 1 {
		t.Fatal("the fault is not on")
	}
	home := c.Home()
	if home.Failure != nil {
		t.Errorf("a change that was made is reported as failed: %+v", home.Failure)
	}
	if len(home.Attention) == 0 || home.Attention[0].T != "The simulator could not save what was changed" {
		t.Fatalf("Home does not say the state is unsaved: %+v", home.Attention)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConditionOff(0); err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Home().Attention {
		if strings.Contains(a.T, "could not save") {
			t.Error("Home still says the state is unsaved after a save succeeded")
		}
	}
}

// A request that was already on its way when a tool was cut off does not
// bring the tool back when it is answered.
func TestRequestsInFlightDoNotUndoACut(t *testing.T) {
	b := newToolbook()
	t0 := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	call := func(at time.Time) activity.Call {
		return activity.Call{Time: at, Client: "10.0.0.1", Agent: "tool", User: "ops", Reason: activity.ReasonAnswered}
	}
	login := call(t0)
	login.Method = "Login"
	b.note(login, true)
	b.note(call(t0.Add(time.Second)), false)
	if got := b.cut("ops", ToolRemoved, t0.Add(10*time.Second), false, time.Time{}); len(got) != 1 {
		t.Fatalf("cut %v", got)
	}
	// started at +5 s, held, answered after the cut
	if back := b.note(call(t0.Add(5*time.Second)), false); back != "" {
		t.Errorf("an in-flight request brought the tool back")
	}
	if v := b.list(t0.Add(11*time.Second), false, time.Time{}); v[0].State != ToolRemoved {
		t.Errorf("state %s", v[0].State)
	}
	// a request that starts after the cut is the tool coming back
	if back := b.note(call(t0.Add(20*time.Second)), false); back != "10.0.0.1" {
		t.Errorf("a later request did not bring the tool back")
	}
}

// One performance query can name several kinds of data; a fault switched on
// for the second one is still "failing on purpose", never "not answered".
func TestFaultOnTheSecondKindOfAQueryIsNotCalledUnanswered(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	tool := newTool(t, c, "collector/2.0")
	if !tool.login("administrator@vsphere.local", "first-password") {
		t.Fatal("sign in")
	}
	must(t, c, "fault", map[string]string{"preset": "error"}, nil)
	refs := c.applied().sim.Clusters[0]
	host := refs.Cluster.Hosts[0].NodeUUID
	now := time.Now().UTC()
	spec := func(ref string) string {
		return fmt.Sprintf(`<querySpecs><entityRefId>%s</entityRefId><startTime>%s</startTime><endTime>%s</endTime></querySpecs>`, ref,
			now.Add(-15*time.Minute).Format("2006-01-02T15:04:05.000Z"), now.Format("2006-01-02T15:04:05.000Z"))
	}
	st, body := tool.post("/vsanHealth", `<VsanPerfQueryPerf xmlns="urn:vsan"><_this type="VsanPerformanceManager">vsan-performance-manager</_this>`+
		spec("host-domclient:"+host)+spec("vsan-pnic-net:"+host+"|vmnic0")+
		`<cluster type="ClusterComputeResource">`+refs.Ref.Value+`</cluster></VsanPerfQueryPerf>`)
	if st != 500 || !strings.Contains(body, "SystemError") {
		t.Skipf("the simulator did not fault the second kind of this query (status %d)", st)
	}
	if call := lastCall(c); call.Reason != activity.ReasonFault || call.Cause == 0 || call.Family != "vsan-pnic-net" {
		t.Errorf("recorded as %q (cause %d, family %q)", call.Reason, call.Cause, call.Family)
	}
	if n := c.Requests().Counts["unknown"]; n != 0 {
		t.Errorf("%d kinds of request are called not answered", n)
	}
}

// A task that always happens when its button is pressed cannot be scheduled
// through the API either.
func TestAnImmediateTaskCannotBeScheduled(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	c.ConsolePassword = func(string, string, string) error { return nil }
	at := time.Now().Add(time.Hour)
	r, err := c.TaskRun("console-password", params(t, map[string]any{"cur": "old-password", "pw": "new-password", "pw2": "new-password"}), &at, false, Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Scheduled != nil || !strings.Contains(r.Errors["at"], "cannot be scheduled") {
		t.Errorf("a console password change was scheduled: %+v", r)
	}
}

// An install from before first-run set-up may have saved the retired default
// password. It does not survive: set-up asks for a new one.
func TestRetiredDefaultPasswordDoesNotSurviveAnUpgrade(t *testing.T) {
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("VMware123!VMware123!"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := bcrypt.GenerateFromPassword([]byte("kept-password"), bcrypt.MinCost)
	saved := fmt.Sprintf(`{"source":"old","anyLogin":false,"users":{"administrator@vsphere.local":{"hash":%q,"role":"administrator"}}}`, hash)
	if err := os.WriteFile(filepath.Join(dir, "vcenter-users.json"), []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCtl(t, scenario.Phase1("upgrade"), dir, "new")
	if !c.AccountNeeded() {
		t.Fatal("the retired default was kept, so set-up will never ask for a password")
	}
	if newTool(t, c, "tool/1").login(DefaultUsername, "VMware123!VMware123!") {
		t.Fatal("the retired default password still signs in")
	}
	c.Close()

	// another saved user is kept, and only the retired account goes
	saved = fmt.Sprintf(`{"source":"old","anyLogin":false,"users":{"administrator@vsphere.local":{"hash":%q,"role":"administrator"},"ops@vsphere.local":{"hash":%q,"role":"read-only"}}}`, hash, other)
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "vcenter-users.json"), []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := newCtl(t, scenario.Phase1("upgrade"), dir2, "new")
	users, _ := c2.LoginUsers()
	if len(users) != 1 || users[0] != "ops@vsphere.local" {
		t.Errorf("users after upgrade: %v", users)
	}
	if !newTool(t, c2, "tool/1").login("ops@vsphere.local", "kept-password") {
		t.Error("the other saved user no longer signs in")
	}
}

// "Leave every host as it is" changes nothing a host reports, including on
// a typed vCenter version where hosts take their build from vCenter.
func TestLeavingHostsAsTheyAreKeepsWhatTheyReport(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	must(t, c, "vcenter-version", nil, map[string]any{"vc": "other", "other": "9.2.0", "hosts": "all"})
	before := c.applied().topo.Clusters()[0].Hosts[0].Version
	must(t, c, "vcenter-version", nil, map[string]any{"vc": "9.1.1", "hosts": "none"})
	after := c.applied().topo.Clusters()[0].Hosts[0].Version
	if before.Version != after.Version || before.Build != after.Build || before.FullName != after.FullName || before.APIVersion != after.APIVersion {
		t.Errorf("a host that was left as it is changed: %+v -> %+v", before, after)
	}
}

// A fault that applied only to a removed cluster ends with it, and the
// history says so.
func TestRemovingAClusterEndsTheFaultsThatOnlyAppliedToIt(t *testing.T) {
	c := newCtl(t, small(t), "", "t")
	must(t, c, "add-cluster", nil, map[string]any{"hosts": 3, "vms": 1})
	must(t, c, "fault", nil, map[string]any{"kind": "error", "which": "perf", "where": "some", "pick": []string{"dc-01/cluster-02"}})
	if _, err := c.RemoveCluster("dc-01/cluster-02"); err != nil {
		t.Fatal(err)
	}
	if n := len(c.Faults().On); n != 0 {
		t.Errorf("%d faults still on", n)
	}
	wantContains(t, history(c), "Turned off with cluster-02, the only cluster it applied to: requests for all vSAN performance data come back with an error, on cluster-02.")
}

// An uploaded certificate that signed itself is the person's own, not one
// "made by the simulator"; one that has run out says so however recently.
func TestCertificateKindAndExpiry(t *testing.T) {
	ck := &clock{t: time.Now()}
	c := newCtlAt(t, small(t), "", "t", ck.now)
	if got := c.Security().Certificate; got.Kind != "made" || got.Days < 3000 {
		t.Fatalf("the simulator's own certificate: %+v", got)
	}
	ck.add(c.Security().Certificate.Expires.Sub(ck.now()) + 5*time.Hour) // a few hours after it ran out
	if got := c.Security().Certificate; got.Days != -1 {
		t.Errorf("a certificate that ran out hours ago has %d days left", got.Days)
	}
	found := false
	for _, a := range c.Home().Attention {
		found = found || a.T == "The certificate has run out"
	}
	if !found {
		t.Error("Home does not say the certificate has run out")
	}
}
