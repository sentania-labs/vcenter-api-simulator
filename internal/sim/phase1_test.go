package sim_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

func (e *env) post(path, payload string) (int, []byte) {
	e.t.Helper()
	r, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(payload))
	r.Header.Set("Content-Type", "text/xml; charset=utf-8")
	r.AddCookie(&http.Cookie{Name: soap.SessionCookieName, Value: e.cookie})
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

// Unlisted objects and unsupported operations retain typed faults.
func TestVsanHealthServesOnlyTheSlice(t *testing.T) {
	e := start(t, "narrow")
	call := func(typ, id, op string) shape {
		t.Helper()
		_, body := e.post("/vsanHealth", `<?xml version="1.0" encoding="UTF-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><`+op+` xmlns="urn:vsan"><_this type="`+typ+`">`+id+`</_this><cluster type="ClusterComputeResource">`+e.cl().Ref.Value+`</cluster></`+op+`></soapenv:Body></soapenv:Envelope>`)
		return parseShape(t, body)
	}
	got := call(vsan.VsanVcStretchedClusterSystem.Type, vsan.VsanVcStretchedClusterSystem.Value, "VSANVcConvertToStretchedCluster")
	if got.fault != "MethodNotFoundFault" {
		t.Errorf("stretched-cluster system: fault %q", got.fault)
	}
	got = call("VsanUnlistedSystem", "vsan-unlisted-system", "VsanClusterGetConfig")
	if got.fault != "ManagedObjectNotFoundFault" {
		t.Errorf("unlisted object: fault %q", got.fault)
	}
	got = call(vsan.VsanVcClusterConfigSystemInstance.Type, vsan.VsanVcClusterConfigSystemInstance.Value, "VsanClusterGetConfig")
	if got.fault != "" {
		t.Errorf("listed object: fault %q", got.fault)
	}
}

// The identities /vsanHealth emits for NICs must exist on /sdk: the vSAN
// uplinks are the host's proxy switch uplinks on the one vDS, the vSAN
// vmkernel NIC is on the vDS and on the stack its entities name, and every
// host is a vDS member. /sdk carries more (a management uplink, the
// management and vMotion vmkernel NICs, the extra TCP/IP stacks), as a
// real host does.
func TestHostNetworkMatchesVsanHealthEntities(t *testing.T) {
	e := start(t, "network")
	ctx := context.Background()
	c := e.client
	cl := e.cl().Cluster

	var dvs mo.DistributedVirtualSwitch
	dvsRef := firstOf(t, e, "DistributedVirtualSwitch")
	if err := property.DefaultCollector(c).RetrieveOne(ctx, dvsRef, []string{"name", "uuid", "summary.hostMember"}, &dvs); err != nil {
		t.Fatal(err)
	}
	if dvs.Name != cl.VDS() || dvs.Uuid != cl.VDSUUID() {
		t.Fatalf("vDS %q %q, topology %q %q", dvs.Name, dvs.Uuid, cl.VDS(), cl.VDSUUID())
	}

	// vsanHealth entities per kind of NIC identity, by node uuid
	pnics, vnics, stacks := map[string][]string{}, map[string][]string{}, map[string][]string{}
	for name, f := range e.sim.Catalog.Families {
		if f.Behaviour != catalog.Data {
			continue
		}
		for _, shape := range f.RefShapes {
			var into map[string][]string
			var field int
			switch shape {
			case "node|vmnicN":
				into, field = pnics, 1
			case "node|vmkN":
				into, field = vnics, 1
			case "node|defaultTcpipStack":
				into, field = stacks, 1
			case "node|defaultTcpipStack|vmkN":
				into, field = vnics, 2
			default:
				continue
			}
			for _, ref := range e.entities(name) {
				parts := strings.Split(strings.TrimPrefix(ref, name+":"), "|")
				if len(parts) <= field {
					t.Fatalf("%s: unexpected ref %q", name, ref)
				}
				into[parts[0]] = append(into[parts[0]], parts[field])
			}
		}
	}

	for i, href := range e.cl().Hosts {
		h := cl.Hosts[i]
		var hs mo.HostSystem
		if err := property.DefaultCollector(c).RetrieveOne(ctx, href, []string{"config.network"}, &hs); err != nil {
			t.Fatal(err)
		}
		n := hs.Config.Network
		pnicKey := map[string]string{}
		for _, p := range n.Pnic {
			pnicKey[p.Key] = p.Device
		}
		vnic := map[string]types.HostVirtualNic{}
		for _, v := range n.Vnic {
			vnic[v.Device] = v
		}
		stack := map[string]bool{}
		for _, s := range n.NetStackInstance {
			stack[s.Key] = true
		}
		if len(n.ProxySwitch) != 1 || n.ProxySwitch[0].DvsUuid != dvs.Uuid {
			t.Fatalf("%s proxy switches %+v", h.Name, n.ProxySwitch)
		}
		var uplinks []string
		for _, k := range n.ProxySwitch[0].Pnic {
			if pnicKey[k] == "" {
				t.Errorf("%s proxy switch uplink %s is not a pnic", h.Name, k)
			}
			uplinks = append(uplinks, pnicKey[k])
		}
		if !sameSet(uplinks, pnics[h.NodeUUID]) {
			t.Errorf("%s uplinks: /sdk proxy switch %v, /vsanHealth %v", h.Name, uplinks, pnics[h.NodeUUID])
		}
		for _, dev := range vnics[h.NodeUUID] {
			v, ok := vnic[dev]
			if !ok {
				t.Errorf("%s: /vsanHealth names %s, /sdk has no such vmkernel NIC", h.Name, dev)
				continue
			}
			if v.Spec.DistributedVirtualPort == nil || v.Spec.DistributedVirtualPort.SwitchUuid != dvs.Uuid {
				t.Errorf("%s %s is not on the vDS: %+v", h.Name, dev, v.Spec.DistributedVirtualPort)
			}
			if v.Spec.NetStackInstanceKey != h.Stack {
				t.Errorf("%s %s stack %q", h.Name, dev, v.Spec.NetStackInstanceKey)
			}
		}
		for _, st := range stacks[h.NodeUUID] {
			if !stack[st] {
				t.Errorf("%s: /vsanHealth names stack %s, /sdk lacks it", h.Name, st)
			}
		}
		if len(vnic) < 3 || !stack["vmotion"] {
			t.Errorf("%s: a real host carries management and vMotion NICs too: %v", h.Name, n.Vnic)
		}
		member := false
		for _, m := range dvs.Summary.HostMember {
			member = member || m == href
		}
		if !member {
			t.Errorf("%s is not a vDS member", h.Name)
		}
	}
}

func firstOf(t *testing.T, e *env, kind string) types.ManagedObjectReference {
	t.Helper()
	v, err := view.NewManager(e.client).CreateContainerView(context.Background(), e.client.ServiceContent.RootFolder, []string{kind}, true)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := v.Find(context.Background(), []string{kind}, nil)
	if err != nil || len(refs) != 1 {
		t.Fatalf("%s: %v %v", kind, refs, err)
	}
	return refs[0]
}

func sameSet(a, b []string) bool {
	uniq := func(s []string) []string {
		m := map[string]bool{}
		for _, v := range s {
			m[v] = true
		}
		var out []string
		for v := range m {
			out = append(out, v)
		}
		sort.Strings(out)
		return out
	}
	return equalStrings(uniq(a), uniq(b))
}

// With the cluster's dedup switch off the ESA dedup families echo empty and
// the cluster config reports dedup disabled; with it on they carry data.
func TestDedupSwitch(t *testing.T) {
	const req = "perf-" + capturedMoid + "-cluster-esa-dedup-domclient-io.req.xml"
	on := start(t, "dedup")
	_, body := on.replay(req, true)
	if got := parseShape(t, body); len(got.samples) == 0 || len(got.labels) == 0 {
		t.Fatalf("dedup on: no data: %s", body)
	}

	topo := topology.Phase1("dedup")
	topo.Clusters()[0].Dedup = false
	off := startTopology(t, topo)
	status, body := off.replay(req, true)
	got := parseShape(t, body)
	if status != 200 || got.fault != "" {
		t.Fatalf("dedup off: status %d fault %q", status, got.fault)
	}
	if len(got.refs) != 1 || !strings.HasSuffix(got.refs[0], ":*") || len(got.samples) != 0 || len(got.labels) != 0 {
		t.Errorf("dedup off must echo empty: refs %v samples %v labels %v", got.refs, got.samples, got.labels)
	}
	for _, name := range dedupFixtures(t) {
		_, body := off.replay(name, true)
		if g := parseShape(t, body); len(g.samples) != 0 || len(g.labels) != 0 || g.fault != "" {
			t.Errorf("%s: dedup off returned data or a fault", name)
		}
	}

	wantTypes := elementTexts(t, fixture(t, "entitytypes-"+capturedMoid+".resp.xml"), "id")
	_, body = off.replay("entitytypes-"+capturedMoid+".req.xml", true)
	gotTypes := elementTexts(t, body, "id")
	hasDedup := false
	for _, id := range gotTypes {
		hasDedup = hasDedup || strings.Contains(id, "dedup")
	}
	if !hasDedup || !equalStrings(gotTypes, wantTypes) {
		t.Errorf("dedup off: entity types differ from the capture (dedup advertised: %v, %d ids, want %d)", hasDedup, len(gotTypes), len(wantTypes))
	}

	vc, err := vsan.NewClient(context.Background(), off.client)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := vc.VsanClusterGetConfig(context.Background(), off.cl().Ref)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataEfficiencyConfig == nil || cfg.DataEfficiencyConfig.DedupEnabled {
		t.Errorf("dedup off: data efficiency config %+v", cfg.DataEfficiencyConfig)
	}
	vc, err = vsan.NewClient(context.Background(), on.client)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = vc.VsanClusterGetConfig(context.Background(), on.cl().Ref); err != nil || !cfg.DataEfficiencyConfig.DedupEnabled {
		t.Errorf("dedup on: %+v %v", cfg.DataEfficiencyConfig, err)
	}
}

func dedupFixtures(t *testing.T) []string {
	t.Helper()
	all, _ := filepath.Glob(filepath.Join(fixtures, "perf-"+capturedMoid+"-*-esa-dedup-*.req.xml"))
	if len(all) == 0 {
		t.Fatal("no dedup fixtures")
	}
	var out []string
	for _, f := range all {
		out = append(out, filepath.Base(f))
	}
	return out
}

func elementTexts(t *testing.T, doc []byte, name string) []string {
	t.Helper()
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if el, ok := tok.(xml.StartElement); ok && el.Name.Local == name {
			var txt string
			if err := dec.DecodeElement(&txt, &el); err == nil {
				out = append(out, txt)
			}
		}
	}
	return out
}
