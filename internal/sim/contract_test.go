package sim_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan"
	vsanmethods "github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"

	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/sentania/vcenter-api-simulator/internal/vsanhealth"
)

const (
	fixtures      = "../../fixtures/live/9.1.1"
	capturedMoid  = "domain-c6004" // the captured cluster
	capturedHost  = "host-6011"    // the captured host in manageddisks-*-host-6011
	capturedHost2 = "d9037b33-d1a2-53c7-8a49-26d1c5ef683c"
)

// captureTime is when the live capture ran; the Performance Service clock
// is frozen there so the fixture windows produce the fixture buckets.
var captureTime = time.Date(2026, 11, 6, 23, 38, 34, 0, time.UTC)

type env struct {
	t      *testing.T
	sim    *sim.Simulator
	srv    *httptest.Server
	client *vim25.Client
	cookie string
}

func start(t *testing.T, seed string) *env {
	t.Helper()
	s, err := sim.New(sim.Options{Topology: topology.Phase1(seed), Now: func() time.Time { return captureTime }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s.Handler)
	t.Cleanup(srv.Close)
	u, _ := soap.ParseURL(srv.URL + "/sdk")
	u.User = nil
	sc := soap.NewClient(u, true)
	c, err := vim25.NewClient(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewManager(c).Login(context.Background(), url.UserPassword("user", "pass")); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, sim: s, srv: srv, client: c}
	for _, ck := range sc.Jar.Cookies(u) {
		if ck.Name == soap.SessionCookieName {
			e.cookie = ck.Value
		}
	}
	if e.cookie == "" {
		t.Fatal("no session cookie after login")
	}
	return e
}

// replay posts a fixture request (with the captured MoRefs rewritten to this
// instance's) to /vsanHealth and returns the response body.
func (e *env) replay(name string, withCookie bool) (int, []byte) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		e.t.Fatal(err)
	}
	req := strings.ReplaceAll(string(b), capturedMoid, e.sim.ClusterRef.Value)
	req = strings.ReplaceAll(req, capturedHost, e.sim.HostRefs[0].Value)
	req = strings.ReplaceAll(req, capturedHost2, e.sim.Topology.Cluster.Hosts[0].NodeUUID)
	r, _ := http.NewRequest("POST", e.srv.URL+"/vsanHealth", strings.NewReader(req))
	r.Header.Set("Content-Type", "text/xml; charset=utf-8")
	r.Header.Set("SOAPAction", "urn:vsan/8.0.0.4")
	if withCookie {
		r.AddCookie(&http.Cookie{Name: soap.SessionCookieName, Value: e.cookie})
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// shape is the element structure of one XML document: every element path
// under the SOAP body (local names), and the text of label/entityRefId/
// sampleInfo elements where they occur, so responses compare by what they
// contain rather than by identifiers or values.
type shape struct {
	paths   map[string]bool // every distinct element path
	first   []string        // element paths of the first returnval, in order
	labels  []string        // metric labels of the first returnval, in order
	refs    []string
	samples []string
	fault   string // detail element name, e.g. InvalidArgumentFault
	text    map[string]string
}

func parseShape(t *testing.T, doc []byte) shape {
	t.Helper()
	s := shape{paths: map[string]bool{}, text: map[string]string{}}
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var stack []string
	returnvals := 0
	inFirst := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			stack = append(stack, el.Name.Local)
			path := strings.Join(stack, "/")
			s.paths[path] = true
			if el.Name.Local == "returnval" && len(stack) == 4 {
				returnvals++
				inFirst = returnvals == 1
			}
			if inFirst && len(stack) > 4 {
				s.first = append(s.first, strings.Join(stack[4:], "/"))
			}
			if len(stack) == 4 && stack[2] == "Fault" && stack[3] == "detail" {
				// next element is the fault type
			}
			if len(stack) == 5 && stack[3] == "detail" {
				s.fault = el.Name.Local
			}
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			leaf := stack[len(stack)-1]
			txt := string(el)
			switch {
			case leaf == "label" && inFirst:
				s.labels = append(s.labels, txt)
			case leaf == "entityRefId":
				s.refs = append(s.refs, txt)
			case leaf == "sampleInfo" && inFirst:
				s.samples = strings.Split(txt, ",")
			case leaf == "faultstring" || leaf == "invalidProperty" || leaf == "key" || leaf == "message":
				if strings.Contains(strings.Join(stack, "/"), "Fault") {
					s.text[leaf] = txt
				}
			}
		case xml.EndElement:
			if el.Name.Local == "returnval" && len(stack) == 4 {
				inFirst = false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return s
}

// noInventedFields: every element path the simulator emits exists in the
// capture.
func noInventedFields(t *testing.T, name string, got, want shape) {
	t.Helper()
	var extra []string
	for p := range got.paths {
		if !want.paths[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("%s: simulator emits elements the live service did not: %v", name, extra)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestContractPerfFamilies(t *testing.T) {
	e := start(t, "contract")
	entries, _ := filepath.Glob(filepath.Join(fixtures, "perf-"+capturedMoid+"-*.req.xml"))
	if len(entries) != 69 {
		t.Fatalf("%d perf fixtures", len(entries))
	}
	for _, reqFile := range entries {
		name := filepath.Base(reqFile)
		family := strings.TrimSuffix(strings.TrimPrefix(name, "perf-"+capturedMoid+"-"), ".req.xml")
		t.Run(family, func(t *testing.T) {
			status, body := e.replay(name, true)
			want := parseShape(t, fixture(t, strings.Replace(name, ".req.xml", ".resp.xml", 1)))
			got := parseShape(t, body)
			noInventedFields(t, family, got, want)
			if want.fault != "" {
				if status != 500 || got.fault != want.fault {
					t.Fatalf("status %d fault %q, want fault %q", status, got.fault, want.fault)
				}
				for _, k := range []string{"faultstring", "invalidProperty", "key", "message"} {
					if got.text[k] != want.text[k] {
						t.Errorf("%s: %q want %q", k, got.text[k], want.text[k])
					}
				}
				return
			}
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			if len(want.refs) == 1 && strings.HasSuffix(want.refs[0], ":*") {
				// empty echo
				if !equalStrings(got.refs, want.refs) || !equalStrings(got.first, want.first) {
					t.Fatalf("empty echo: refs %v first %v", got.refs, got.first)
				}
				return
			}
			if !equalStrings(got.labels, want.labels) {
				t.Errorf("labels differ\n got %v\nwant %v", got.labels, want.labels)
			}
			if !equalStrings(got.first, want.first) {
				t.Errorf("first entity structure differs\n got %v\nwant %v", got.first, want.first)
			}
			if !equalStrings(got.samples, want.samples) {
				t.Errorf("sampleInfo %v want %v", got.samples, want.samples)
			}
			if len(got.refs) == 0 {
				t.Error("no entities")
			}
			for _, r := range got.refs {
				if strings.Contains(r, "*") {
					t.Errorf("wildcard echoed alongside data: %s", r)
				}
			}
		})
	}
}

func TestContractEdgeCases(t *testing.T) {
	e := start(t, "contract")
	cases, _ := filepath.Glob(filepath.Join(fixtures, "edge-"+capturedMoid+"-*.req.xml"))
	if len(cases) != 10 {
		t.Fatalf("%d edge fixtures", len(cases))
	}
	for _, reqFile := range cases {
		name := filepath.Base(reqFile)
		t.Run(strings.TrimSuffix(strings.TrimPrefix(name, "edge-"+capturedMoid+"-"), ".req.xml"), func(t *testing.T) {
			status, body := e.replay(name, true)
			want := parseShape(t, fixture(t, strings.Replace(name, ".req.xml", ".resp.xml", 1)))
			got := parseShape(t, body)
			noInventedFields(t, name, got, want)
			if got.fault != want.fault {
				t.Fatalf("fault %q want %q (status %d): %s", got.fault, want.fault, status, body)
			}
			for _, k := range []string{"faultstring", "invalidProperty", "key", "message"} {
				if got.text[k] != want.text[k] {
					t.Errorf("%s: %q want %q", k, got.text[k], want.text[k])
				}
			}
			if want.fault == "" {
				if !equalStrings(got.samples, want.samples) {
					t.Errorf("sampleInfo %v want %v", got.samples, want.samples)
				}
				if !equalStrings(got.labels, want.labels) {
					t.Errorf("labels %v want %v", got.labels, want.labels)
				}
				// the capture has five hosts, the Phase 1 topology four
				if (len(got.refs) == 0) != (len(want.refs) == 0) {
					t.Errorf("%d entities want %d", len(got.refs), len(want.refs))
				}
			}
		})
	}
}

func TestContractOtherMethods(t *testing.T) {
	e := start(t, "contract")
	for _, name := range []string{"clusterconfig", "nodeinfo", "entitytypes", "healthsummary"} {
		t.Run(name, func(t *testing.T) {
			status, body := e.replay(name+"-"+capturedMoid+".req.xml", true)
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			want := parseShape(t, fixture(t, name+"-"+capturedMoid+".resp.xml"))
			got := parseShape(t, body)
			noInventedFields(t, name, got, want)
			if name == "nodeinfo" && !equalStrings(got.first, want.first) {
				t.Errorf("master entry differs\n got %v\nwant %v", got.first, want.first)
			}
			if name == "entitytypes" {
				var missing []string
				for p := range want.paths {
					if !got.paths[p] {
						missing = append(missing, p)
					}
				}
				sort.Strings(missing)
				if len(missing) > 0 {
					t.Errorf("entity types lack captured elements: %v", missing)
				}
				if len(got.refs) != len(want.refs) || !equalStrings(got.first, want.first) {
					t.Errorf("entity type list differs")
				}
			}
		})
	}
	t.Run("manageddisks", func(t *testing.T) {
		status, body := e.replay("manageddisks-"+capturedMoid+"-"+capturedHost+".req.xml", true)
		if status != 200 {
			t.Fatalf("status %d: %s", status, body)
		}
		want := parseShape(t, fixture(t, "manageddisks-"+capturedMoid+"-"+capturedHost+".resp.xml"))
		got := parseShape(t, body)
		noInventedFields(t, "manageddisks", got, want)
		for _, p := range []string{"storagePools/storagePoolDisks/disk/vsanDiskInfo/vsanUuid", "storagePools/storagePoolDisks/vsanUuid", "storagePools/storagePoolDisks/disk/model"} {
			if !got.paths["Envelope/Body/QueryVsanManagedDisksResponse/returnval/"+p] {
				t.Errorf("missing %s", p)
			}
		}
	})
	t.Run("nocookie", func(t *testing.T) {
		status, body := e.replay("nocookie-vsanHealth.req.xml", false)
		got := parseShape(t, body)
		if status != 500 || got.fault != "NotAuthenticatedFault" {
			t.Fatalf("status %d fault %q: %s", status, got.fault, body)
		}
	})
	t.Run("versions", func(t *testing.T) {
		for _, p := range []string{"/sdk/vimServiceVersions.xml", "/sdk/vsanServiceVersions.xml"} {
			res, err := http.Get(e.srv.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if !bytes.Contains(b, []byte("<version>9.1.1.0</version>")) {
				t.Errorf("%s: %s", p, b)
			}
		}
	})
}

// TestIdentityConsistency: every join the management pack makes holds across
// /sdk and /vsanHealth (recon section 9 test 2).
func TestIdentityConsistency(t *testing.T) {
	e := start(t, "identity")
	ctx := context.Background()
	c := e.client
	topo := e.sim.Topology

	var cl mo.ClusterComputeResource
	if err := property.DefaultCollector(c).RetrieveOne(ctx, e.sim.ClusterRef, []string{"name", "host", "configurationEx"}, &cl); err != nil {
		t.Fatal(err)
	}
	sdkUUID := cl.ConfigurationEx.(*types.ClusterConfigInfoEx).VsanConfigInfo.DefaultConfig.Uuid
	if sdkUUID != topo.Cluster.VsanUUID {
		t.Fatalf("/sdk cluster uuid %s", sdkUUID)
	}
	vc, err := vsan.NewClient(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := vc.VsanClusterGetConfig(ctx, e.sim.ClusterRef)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultConfig.Uuid != sdkUUID {
		t.Fatalf("/vsanHealth cluster uuid %s, /sdk %s", cfg.DefaultConfig.Uuid, sdkUUID)
	}
	if cfg.VsanEsaEnabled == nil || !*cfg.VsanEsaEnabled {
		t.Error("vsanEsaEnabled")
	}

	// host node uuids: vsanSystem.config on /sdk vs node information
	nodes, err := vsanmethods.VsanPerfQueryNodeInformation(ctx, vc.Client, &vsantypes.VsanPerfQueryNodeInformation{
		This: vsanhealth.PerfManagerRef, Cluster: &e.sim.ClusterRef})
	if err != nil {
		t.Fatal(err)
	}
	nodeUUIDs := map[string]string{}
	for _, n := range nodes.Returnval {
		nodeUUIDs[n.VsanNodeUuid] = n.Hostname
	}
	if len(nodeUUIDs) != len(cl.Host) {
		t.Fatalf("%d node entries for %d hosts", len(nodeUUIDs), len(cl.Host))
	}
	for _, href := range cl.Host {
		var hs mo.HostSystem
		if err := property.DefaultCollector(c).RetrieveOne(ctx, href, []string{"name", "configManager.vsanSystem", "config.storageDevice"}, &hs); err != nil {
			t.Fatal(err)
		}
		var vs mo.HostVsanSystem
		if err := property.DefaultCollector(c).RetrieveOne(ctx, *hs.ConfigManager.VsanSystem, []string{"config"}, &vs); err != nil {
			t.Fatal(err)
		}
		node := vs.Config.ClusterInfo.NodeUuid
		if nodeUUIDs[node] != hs.Name {
			t.Errorf("host %s node %s: node information says %q", hs.Name, node, nodeUUIDs[node])
		}
		if vs.Config.ClusterInfo.Uuid != sdkUUID {
			t.Errorf("host %s clusterInfo.uuid %s", hs.Name, vs.Config.ClusterInfo.Uuid)
		}
		// disks: QueryDisksForVsan vs QueryVsanManagedDisks vs perf entities
		dres, err := methods.QueryDisksForVsan(ctx, c, &types.QueryDisksForVsan{This: *hs.ConfigManager.VsanSystem})
		if err != nil {
			t.Fatal(err)
		}
		disks := dres.Returnval
		var claimed []string
		luns := map[string]bool{}
		for _, l := range hs.Config.StorageDevice.ScsiLun {
			luns[l.GetScsiLun().Key] = true
		}
		driver := map[string]string{}
		hba := map[string]string{}
		for _, a := range hs.Config.StorageDevice.HostBusAdapter {
			hba[a.GetHostHostBusAdapter().Key] = a.GetHostHostBusAdapter().Driver
		}
		for _, a := range hs.Config.StorageDevice.ScsiTopology.Adapter {
			for _, tg := range a.Target {
				for _, lun := range tg.Lun {
					driver[lun.ScsiLun] = hba[a.Adapter]
				}
			}
		}
		for _, d := range disks {
			if !luns[d.Disk.Key] {
				t.Errorf("disk %s not in config.storageDevice.scsiLun", d.Disk.Key)
			}
			if driver[d.Disk.Key] == "" {
				t.Errorf("disk %s has no driver through scsiTopology", d.Disk.Key)
			}
			if d.VsanUuid != "" {
				claimed = append(claimed, d.VsanUuid)
				if d.State != "ineligible" || d.Error == nil {
					t.Errorf("claimed disk state %q error %v", d.State, d.Error)
				}
				if _, ok := d.Error.Fault.(*types.VsanFault); !ok {
					t.Errorf("claimed disk fault %T", d.Error.Fault)
				}
			}
		}
		if len(claimed) != 1 {
			t.Errorf("host %s: %d claimed disks", hs.Name, len(claimed))
		}
		_, body := e.replayHost("manageddisks-"+capturedMoid+"-"+capturedHost+".req.xml", href)
		for _, u := range claimed {
			if !bytes.Contains(body, []byte("<vsanUuid>"+u+"</vsanUuid>")) {
				t.Errorf("host %s: claimed disk %s not in QueryVsanManagedDisks", hs.Name, u)
			}
			for _, fam := range []string{"vsan-esa-disk-layer", "vsan-esa-disk-scsifw"} {
				if n := e.entities(fam); !contains(n, fam+":"+u) {
					t.Errorf("%s lacks %s", fam, u)
				}
			}
		}
	}
	// host-scoped families use node uuids
	for _, fam := range []string{"host-domclient", "vsan-pnic-net", "host-cpu", "dom-world-cpu", "vsan-vnic-net"} {
		for _, ref := range e.entities(fam) {
			head := strings.Split(strings.TrimPrefix(ref, fam+":"), "|")[0]
			if nodeUUIDs[head] == "" {
				t.Errorf("%s: %s is not a node uuid", fam, ref)
			}
		}
	}
	// VMs: instanceUuid and backing paths
	v, err := view.NewManager(c).CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var vms []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name", "config.uuid", "config.instanceUuid", "config.hardware.device"}, &vms); err != nil {
		t.Fatal(err)
	}
	instance := map[string]bool{}
	paths := map[string]bool{}
	for _, vm := range vms {
		instance[vm.Config.InstanceUuid] = true
		for _, dev := range vm.Config.Hardware.Device {
			if d, ok := dev.(*types.VirtualDisk); ok {
				fn := d.Backing.(*types.VirtualDiskFlatVer2BackingInfo).FileName
				if !strings.HasPrefix(fn, "["+topology.Datastore+"] ") {
					t.Errorf("disk %s not on the vSAN datastore", fn)
				}
				paths[strings.TrimPrefix(fn, "["+topology.Datastore+"] ")] = true
			}
		}
	}
	if len(vms) != len(topo.Cluster.VMs) {
		t.Fatalf("%d VMs", len(vms))
	}
	for _, ref := range e.entities("virtual-machine") {
		if !instance[strings.TrimPrefix(ref, "virtual-machine:")] {
			t.Errorf("%s is not a VM instanceUuid", ref)
		}
	}
	for _, ref := range e.entities("vscsi") {
		if !instance[strings.Split(strings.TrimPrefix(ref, "vscsi:"), "|")[0]] {
			t.Errorf("%s is not a VM instanceUuid", ref)
		}
	}
	for _, ref := range e.entities("virtual-disk") {
		if !paths[strings.TrimPrefix(ref, "virtual-disk:")] {
			t.Errorf("%s is not a VM backing path", ref)
		}
	}
	// about
	if c.ServiceContent.About.InstanceUuid != topo.InstanceUUID || c.ServiceContent.About.ApiVersion != "9.1.1.0" {
		t.Errorf("about: %+v", c.ServiceContent.About)
	}
}

func (e *env) replayHost(name string, host types.ManagedObjectReference) (int, []byte) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		e.t.Fatal(err)
	}
	req := strings.ReplaceAll(string(b), capturedHost, host.Value)
	r, _ := http.NewRequest("POST", e.srv.URL+"/vsanHealth", strings.NewReader(req))
	r.AddCookie(&http.Cookie{Name: soap.SessionCookieName, Value: e.cookie})
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

var refRe = regexp.MustCompile(`<entityRefId[^>]*>([^<]*)</entityRefId>`)

// entities queries a family with the fixture window and returns its refs.
func (e *env) entities(family string) []string {
	e.t.Helper()
	req := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><VsanPerfQueryPerf xmlns="urn:vsan"><_this type="VsanPerformanceManager">vsan-performance-manager</_this><querySpecs><entityRefId>%s:*</entityRefId><startTime>2026-11-06T23:23:22.340Z</startTime><endTime>2026-11-06T23:38:22.340Z</endTime></querySpecs><cluster type="ClusterComputeResource">%s</cluster></VsanPerfQueryPerf></soapenv:Body></soapenv:Envelope>`, family, e.sim.ClusterRef.Value)
	r, _ := http.NewRequest("POST", e.srv.URL+"/vsanHealth", strings.NewReader(req))
	r.AddCookie(&http.Cookie{Name: soap.SessionCookieName, Value: e.cookie})
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var out []string
	for _, m := range refRe.FindAllSubmatch(body, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestDeterminism: the same seed reproduces MoRefs, identities and values
// across instances (and therefore restarts); another seed changes identities
// and values but not structure.
func TestDeterminism(t *testing.T) {
	a := start(t, "det")
	b := start(t, "det")
	c := start(t, "other")
	if a.sim.ClusterRef != b.sim.ClusterRef || a.sim.ClusterRef != c.sim.ClusterRef {
		t.Fatalf("cluster morefs %v %v %v", a.sim.ClusterRef, b.sim.ClusterRef, c.sim.ClusterRef)
	}
	for i := range a.sim.HostRefs {
		if a.sim.HostRefs[i] != b.sim.HostRefs[i] {
			t.Fatalf("host morefs differ")
		}
	}
	for _, name := range []string{"perf-" + capturedMoid + "-host-domclient.req.xml", "nodeinfo-" + capturedMoid + ".req.xml", "clusterconfig-" + capturedMoid + ".req.xml"} {
		_, ba := a.replay(name, true)
		_, bb := b.replay(name, true)
		_, bc := c.replay(name, true)
		if !bytes.Equal(ba, bb) {
			t.Errorf("%s: same seed, different response", name)
		}
		if bytes.Equal(ba, bc) {
			t.Errorf("%s: different seed, identical response", name)
		}
		sa, sc := parseShape(t, ba), parseShape(t, bc)
		if !equalStrings(sa.first, sc.first) || len(sa.refs) != len(sc.refs) {
			t.Errorf("%s: seed changed structure", name)
		}
	}
}
