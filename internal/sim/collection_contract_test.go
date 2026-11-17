package sim_test

import (
	"context"
	"encoding/json"
	stdxml "encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// Oracle: Operations 9.1.1 PropertyRetriever and LWClusterComputeResource
// read configuration.dasConfig, then traverse each host's configManager.
// Four clusters ensure a successful first cluster does not hide a later fault.
func TestOperationsInventoryPropertyPass(t *testing.T) {
	sc := scenario.Phase1("ops-property-pass")
	sc.Datacenters[0].Clusters = 4
	e := startTopology(t, topology.Generate(sc, registry.InMemory(sc.Seed)))
	ctx := context.Background()
	pc := property.DefaultCollector(e.client)
	vmCount, hostCount := 0, 0
	for _, cl := range e.sim.Clusters {
		var cluster mo.ClusterComputeResource
		if err := pc.RetrieveOne(ctx, cl.Ref, []string{"configuration", "configurationEx", "host"}, &cluster); err != nil {
			t.Fatal(err)
		}
		for _, das := range []types.ClusterDasConfigInfo{cluster.Configuration.DasConfig, cluster.ConfigurationEx.(*types.ClusterConfigInfoEx).DasConfig} {
			if das.HostMonitoring != "enabled" || das.VmMonitoring == "" || das.Enabled == nil || das.AdmissionControlEnabled == nil || das.AdmissionControlPolicy == nil || das.DefaultVmSettings == nil {
				t.Fatal("incomplete HA configuration")
			}
		}
		for _, ref := range cluster.Host {
			var host mo.HostSystem
			if err := pc.RetrieveOne(ctx, ref, []string{"configManager", "vm", "hardware"}, &host); err != nil {
				t.Fatal(err)
			}
			managers := reflect.ValueOf(host.ConfigManager)
			for i := 0; i < managers.NumField(); i++ {
				ref, ok := managers.Field(i).Interface().(*types.ManagedObjectReference)
				if !ok || ref == nil {
					continue
				}
				// All=true must resolve the object, even when it has no optional properties.
				_, err := methods.RetrievePropertiesEx(ctx, e.client, &types.RetrievePropertiesEx{This: pc.Reference(), SpecSet: []types.PropertyFilterSpec{{PropSet: []types.PropertySpec{{Type: ref.Type, All: types.NewBool(true)}}, ObjectSet: []types.ObjectSpec{{Obj: *ref}}}}})
				if err != nil {
					t.Fatalf("host manager %s is dangling: %v", ref.Type, err)
				}
			}
			var memory mo.HostMemorySystem
			if err := pc.RetrieveOne(ctx, *host.ConfigManager.MemoryManager, []string{"consoleReservationInfo"}, &memory); err != nil {
				t.Fatal(err)
			}
			if memory.ConsoleReservationInfo == nil || memory.ConsoleReservationInfo.Unreserved != host.Hardware.MemorySize {
				t.Fatal("missing memory reservation fields")
			}
			level, err := methods.HostImageConfigGetAcceptance(ctx, e.client, &types.HostImageConfigGetAcceptance{This: *host.ConfigManager.ImageConfigManager})
			if err != nil || level.Returnval != "vmware_accepted" {
				t.Fatal("host image acceptance read failed")
			}
			hostCount++
			for _, ref := range host.Vm {
				var vm mo.VirtualMachine
				if err := pc.RetrieveOne(ctx, ref, []string{"config", "datastore", "runtime", "storage"}, &vm); err != nil {
					t.Fatal(err)
				}
				if vm.Config == nil || vm.Config.Hardware.NumCPU == 0 || vm.Config.Files.VmPathName == "" || vm.Runtime.Host == nil || len(vm.Datastore) == 0 {
					t.Fatal("incomplete VM inventory")
				}
				for _, ref := range vm.Datastore {
					var ds mo.Datastore
					if err := pc.RetrieveOne(ctx, ref, []string{"info", "summary"}, &ds); err != nil {
						t.Fatal(err)
					}
					if ds.Summary.Type != "vsan" || !strings.Contains(ds.Info.GetDatastoreInfo().Url, "/vsan:") || !strings.HasSuffix(ds.Info.GetDatastoreInfo().Url, "/") || ds.Info.GetDatastoreInfo().Url != ds.Summary.Url {
						t.Fatal("adapter cannot identify the VM's vSAN datastore")
					}
				}
				vmCount++
			}
		}
	}
	if len(e.sim.Clusters) != 4 || hostCount != 16 || vmCount != 16 {
		t.Fatalf("incomplete inventory: clusters=%d hosts=%d VMs=%d", len(e.sim.Clusters), hostCount, vmCount)
	}
}

// Oracle: DynamicProperty.val is non-optional in VMware's VMODL binding.
// Optional config fields absent on page 2 used to become name-only properties.
func TestOperationsPagedOptionalVMProperties(t *testing.T) {
	e := start(t, "ops-pages")
	ctx := context.Background()
	container, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{"VirtualMachine"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer container.Destroy(ctx)
	for _, custom := range []bool{false, true} {
		pc := e.client.ServiceContent.PropertyCollector
		if custom {
			created, err := methods.CreatePropertyCollector(ctx, e.client, &types.CreatePropertyCollector{This: pc})
			if err != nil {
				t.Fatal(err)
			}
			pc = created.Returnval
			if pc.Type != "PropertyCollector" {
				t.Fatalf("session property collector has type %q", pc.Type)
			}
			defer methods.DestroyPropertyCollector(ctx, e.client, &types.DestroyPropertyCollector{This: pc})
		}
		req := &types.RetrievePropertiesEx{This: pc, SpecSet: []types.PropertyFilterSpec{{PropSet: []types.PropertySpec{{Type: "VirtualMachine", PathSet: []string{"name", "config.hardware", "config.managedBy", "config.ftInfo", "config.keyId", "snapshot"}}}, ObjectSet: []types.ObjectSpec{{Obj: container.Reference(), Skip: types.NewBool(true), SelectSet: []types.BaseSelectionSpec{&types.TraversalSpec{Type: "ContainerView", Path: "view"}}}}}}, Options: types.RetrieveOptions{MaxObjects: 1}}
		result, err := methods.RetrievePropertiesEx(ctx, e.client, req)
		if err != nil {
			t.Fatal(err)
		}
		pages, objects := 0, 0
		page := result.Returnval
		for page != nil {
			pages++
			objects += len(page.Objects)
			for _, o := range page.Objects {
				for _, p := range o.PropSet {
					if p.Val == nil {
						t.Fatalf("page %d: required val absent for %s", pages, p.Name)
					}
				}
			}
			if page.Token == "" {
				break
			}
			next, err := methods.ContinueRetrievePropertiesEx(ctx, e.client, &types.ContinueRetrievePropertiesEx{This: pc, Token: page.Token})
			if err != nil {
				t.Fatal(err)
			}
			page = &next.Returnval
		}
		if pages < 2 || objects != 4 {
			t.Fatalf("paging did not exercise continuation: pages=%d objects=%d", pages, objects)
		}
	}
}

func TestOperationsResourceQuery(t *testing.T) {
	e := start(t, "ops-dp")
	ref := e.cl().Hosts[0]
	body := `<DataProviderQuery xmlns="urn:vim25"><_this type="DataProviderResourceModel">ResourceModel</_this><querySpec><properties>@modelKey</properties><properties>dc</properties><resourceModel>HostSystem</resourceModel><filter><criteria><property>@modelKey</property><operator>In</operator><comparableList xsi:type="ManagedObjectReference" type="HostSystem">` + ref.Value + `</comparableList></criteria></filter><returnTotalCount>true</returnTotalCount></querySpec></DataProviderQuery>`
	status, out := postTo(t, e.srv, "/sdk", `"urn:vim25/9.1.1.0"`, soapEnvelope(body), &http.Cookie{Name: "vmware_soap_session", Value: e.cookie})
	if status.StatusCode != 200 {
		t.Fatalf("query failed: %s", out)
	}
	var response struct {
		Body struct {
			Response struct {
				Result struct {
					Properties []string `xml:"properties"`
					Items      []struct {
						Values []struct {
							Value struct {
								Attrs []stdxml.Attr `xml:",any,attr"`
								Ref   string        `xml:",chardata"`
							} `xml:"value"`
						} `xml:"propertyValues"`
					} `xml:"items"`
					Total int64 `xml:"totalCount"`
				} `xml:"returnval"`
			} `xml:"DataProviderQueryResponse"`
		} `xml:"Body"`
	}
	if err := stdxml.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	result := response.Body.Response.Result
	if !reflect.DeepEqual(result.Properties, []string{"@modelKey", "dc"}) || len(result.Items) != 1 || len(result.Items[0].Values) != 2 || result.Total != 1 {
		t.Fatalf("invalid positional result: %s", out)
	}
	if result.Items[0].Values[0].Value.Ref != ref.Value || result.Items[0].Values[1].Value.Ref == "" {
		t.Fatal("missing host or datacenter reference")
	}
	for i, value := range result.Items[0].Values {
		attrs := map[stdxml.Name]string{}
		for _, attr := range value.Value.Attrs {
			attrs[attr.Name] = attr.Value
		}
		want := []string{"HostSystem", "Datacenter"}[i]
		if attrs[stdxml.Name{Local: "type"}] != want || attrs[stdxml.Name{Space: "http://www.w3.org/2001/XMLSchema-instance", Local: "type"}] != "ManagedObjectReference" {
			t.Fatal("required typed reference annotations absent")
		}
	}
}

func TestOperationsCounterQueries(t *testing.T) {
	e := start(t, "ops-counters")
	ctx := context.Background()
	ref := *e.client.ServiceContent.PerfManager
	status, out := postTo(t, e.srv, "/sdk", `"urn:vim25/9.1.1.0"`, soapEnvelope(`<QueryPerfCounterInt xmlns="urn:vim25"><_this type="PerformanceManager">`+ref.Value+`</_this></QueryPerfCounterInt>`), &http.Cookie{Name: "vmware_soap_session", Value: e.cookie})
	if status.StatusCode != 200 || !strings.Contains(string(out), "QueryPerfCounterIntResponse") {
		t.Fatalf("internal counter query failed: %s", out)
	}
	result, err := methods.QueryPerfCounterByLevel(ctx, e.client, &types.QueryPerfCounterByLevel{This: ref, Level: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Returnval) == 0 {
		t.Fatal("no counters")
	}
	var gpu int32
	for _, c := range result.Returnval {
		if c.Key == 0 || c.GroupInfo == nil || c.NameInfo == nil || c.UnitInfo == nil || c.RollupType == "" || c.StatsType == "" || c.Level == 0 || c.GroupInfo.GetElementDescription().Key == "" || c.NameInfo.GetElementDescription().Key == "" {
			t.Fatal("required counter fields absent")
		}
		if c.GroupInfo.GetElementDescription().Key == "gpu" {
			gpu = c.Key
		}
	}
	if gpu == 0 {
		t.Fatal("GPU counters missing")
	}
	_, err = methods.UpdateCounterLevelMapping(ctx, e.client, &types.UpdateCounterLevelMapping{This: ref, CounterLevelMap: []types.PerformanceManagerCounterLevelMapping{{CounterId: gpu, AggregateLevel: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := methods.QueryPerfCounterByLevel(ctx, e.client, &types.QueryPerfCounterByLevel{This: ref, Level: 1})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range updated.Returnval {
		if c.Key == gpu {
			found = true
		}
	}
	if !found {
		t.Fatal("counter mapping did not round-trip")
	}
	direct, err := methods.QueryPerfCounter(ctx, e.client, &types.QueryPerfCounter{This: ref, CounterId: []int32{gpu}})
	if err != nil || len(direct.Returnval) != 1 || direct.Returnval[0].Level != 1 {
		t.Fatal("direct counter read lost updated level")
	}

	v, err := view.NewManager(e.client).CreateContainerView(ctx, e.client.ServiceContent.RootFolder, []string{"Datacenter"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Destroy(ctx)
	var dcs []mo.Datacenter
	if err := v.Retrieve(ctx, []string{"Datacenter"}, []string{"name"}, &dcs); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stats, err := methods.QueryPerf(ctx, e.client, &types.QueryPerf{This: ref, QuerySpec: []types.PerfQuerySpec{{Entity: dcs[0].Reference(), MetricId: []types.PerfMetricId{{CounterId: 256, Instance: "*"}}, MaxSample: 2, StartTime: &now}}})
	if err != nil {
		t.Fatal(err)
	}
	metric := stats.Returnval[0].(*types.PerfEntityMetric)
	series := metric.Value[0].(*types.PerfMetricIntSeries)
	if series.Id.Instance != "" || len(series.Value) != len(metric.SampleInfo) {
		t.Fatal("datacenter VM operations must have aggregate instances and all samples")
	}
}

func TestOperationsLegacySessionAndApplianceReads(t *testing.T) {
	e := start(t, "ops-rest")
	req, _ := http.NewRequest("POST", e.srv.URL+"/rest/com/vmware/cis/session", nil)
	req.SetBasicAuth("user", "pass")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		Value string `json:"value"`
	}
	err = json.NewDecoder(res.Body).Decode(&login)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || login.Value == "" {
		t.Fatal("legacy session response must contain value")
	}
	for path, required := range map[string][]string{
		"/api/appliance/local-accounts/root":          {"enabled", "has_password", "roles", "last_password_change", "warn_days_before_password_expiration", "min_days_between_password_change", "max_days_between_password_change"},
		"/api/appliance/local-accounts/global-policy": {"minimum_length", "max_days", "min_days", "warn_days", "failed_attempt_count_before_account_lockout", "length_of_lockout_period_in_seconds", "minimum_numerics_char_count", "minimum_special_char_count", "minimum_uppercase_char_count", "minimum_lowercase_char_count", "prior_password_remember_count", "fail_interval_between_attempts", "managed_at_fleet"},
		"/api/appliance/networking":                   {"interfaces", "dns", "vcenter_base_url"}, "/api/appliance/tls/profiles/global": {"profile"},
		"/api/vcenter/namespace-management/supervisors/summaries": {"items"}, "/api/vcenter/services/vsan-health": {"name_key", "startup_type", "health_messages", "health", "description_key", "state"},
	} {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		req.Header.Set("vmware-api-session-id", login.Value)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]json.RawMessage
		err = json.NewDecoder(res.Body).Decode(&values)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s failed", path)
		}
		for _, field := range required {
			if _, ok := values[field]; !ok {
				t.Errorf("%s lacks required %s", path, field)
			}
		}
	}
	for _, path := range []string{"/api/appliance/access/consolecli", "/api/appliance/access/ssh", "/api/appliance/recovery/backup/job", "/api/appliance/recovery/backup/schedules", "/api/appliance/logging/forwarding", "/api/appliance/networking/firewall/inbound", "/api/appliance/timesync", "/api/vcenter/topology/nodes", "/api/vcenter/storage/policies", "/api/vcenter/cluster", "/api/vcenter/namespaces/instances/v2"} {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		req.Header.Set("vmware-api-session-id", login.Value)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !json.Valid(body) {
			t.Fatalf("%s invalid: %s", path, body)
		}
	}
	req, _ = http.NewRequest("DELETE", e.srv.URL+"/rest/com/vmware/cis/session", nil)
	req.Header.Set("vmware-api-session-id", login.Value)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("legacy logout failed")
	}
	req, _ = http.NewRequest("GET", e.srv.URL+"/api/appliance/networking", nil)
	req.Header.Set("vmware-api-session-id", login.Value)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var refusal struct {
		Type     string            `json:"error_type"`
		Messages []json.RawMessage `json:"messages"`
	}
	err = json.NewDecoder(res.Body).Decode(&refusal)
	res.Body.Close()
	if err != nil || res.StatusCode != 401 || refusal.Type != "UNAUTHENTICATED" || len(refusal.Messages) == 0 {
		t.Fatal("refusal lacks typed error body")
	}
}

// PropertyRetriever's filter path must expose the same public datastore URL.
func TestOperationsDatastoreUpdateProperties(t *testing.T) {
	e := start(t, "ops-ds-updates")
	ctx := context.Background()
	pc := e.client.ServiceContent.PropertyCollector
	_, err := methods.CreateFilter(ctx, e.client, &types.CreateFilter{This: pc, Spec: types.PropertyFilterSpec{PropSet: []types.PropertySpec{{Type: "Datastore", PathSet: []string{"info", "info.url", "summary"}}}, ObjectSet: []types.ObjectSpec{{Obj: e.cl().Datastore}}}})
	if err != nil {
		t.Fatal(err)
	}
	update, err := methods.WaitForUpdatesEx(ctx, e.client, &types.WaitForUpdatesEx{This: pc, Options: &types.WaitOptions{MaxWaitSeconds: types.NewInt32(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if update.Returnval == nil || len(update.Returnval.FilterSet) == 0 {
		t.Fatal("missing initial inventory update")
	}
	found := 0
	for _, filter := range update.Returnval.FilterSet {
		for _, object := range filter.ObjectSet {
			for _, change := range object.ChangeSet {
				if change.Name == "info" {
					info := change.Val.(types.VsanDatastoreInfo)
					if !strings.Contains(info.Url, "/vsan:") {
						t.Fatal("update exposes backing directory")
					}
					found++
				}
				if change.Name == "info.url" {
					if !strings.Contains(change.Val.(string), "/vsan:") {
						t.Fatal("scalar update exposes backing directory")
					}
					found++
				}
			}
		}
	}
	if found != 2 {
		t.Fatal("required datastore changes absent")
	}
}

// Oracle: Operations 9.1.1 AssetPropertiesFetcher queries hostClusterName,
// Datacenter.name and ClusterComputeResource.dcName. Return the actual parent
// names in the requested positions, selecting only references in the In filter.
func TestOperationsLicenseTopologyQueries(t *testing.T) {
	e := start(t, "ops-license-topology")
	ctx := context.Background()
	host := e.cl().Hosts[0]
	var hm mo.HostSystem
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, host, []string{"parent"}, &hm); err != nil {
		t.Fatal(err)
	}
	cluster := *hm.Parent
	var cm mo.ClusterComputeResource
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, cluster, []string{"name", "parent"}, &cm); err != nil {
		t.Fatal(err)
	}
	var folder mo.Folder
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, *cm.Parent, []string{"parent"}, &folder); err != nil {
		t.Fatal(err)
	}
	dc := *folder.Parent
	var dm mo.Datacenter
	if err := property.DefaultCollector(e.client).RetrieveOne(ctx, dc, []string{"name"}, &dm); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ref        types.ManagedObjectReference
		prop, want string
	}{
		{host, "hostClusterName", cm.Name}, {dc, "name", dm.Name}, {cluster, "dcName", dm.Name},
	} {
		t.Run(tc.ref.Type+"."+tc.prop, func(t *testing.T) {
			body := fmt.Sprintf(`<DataProviderQuery xmlns="urn:vim25" xmlns:vim25="urn:vim25"><_this type="DataProviderResourceModel">ResourceModel</_this><querySpec><properties>%s</properties><properties>@modelKey</properties><resourceModel>%s</resourceModel><filter><criteria><property>@modelKey</property><operator>In</operator><comparableList xsi:type="vim25:ManagedObjectReference" type="%s">%s</comparableList></criteria></filter><returnTotalCount>true</returnTotalCount></querySpec></DataProviderQuery>`, tc.prop, tc.ref.Type, tc.ref.Type, tc.ref.Value)
			status, out := postTo(t, e.srv, "/sdk", `"urn:vim25/6.0"`, soapEnvelope(body), &http.Cookie{Name: "vmware_soap_session", Value: e.cookie})
			if status.StatusCode != 200 {
				t.Fatalf("query failed: %s", out)
			}
			var response struct {
				Body struct {
					Response struct {
						Result struct {
							Properties []string `xml:"properties"`
							Items      []struct {
								Values []struct {
									Value string `xml:"value"`
								} `xml:"propertyValues"`
							} `xml:"items"`
							Total int64 `xml:"totalCount"`
						} `xml:"returnval"`
					} `xml:"DataProviderQueryResponse"`
				} `xml:"Body"`
			}
			if err := stdxml.Unmarshal(out, &response); err != nil {
				t.Fatal(err)
			}
			result := response.Body.Response.Result
			if result.Total != 1 || len(result.Items) != 1 || len(result.Items[0].Values) != 2 {
				t.Fatalf("unexpected rows: %s", out)
			}
			if result.Items[0].Values[0].Value != tc.want || result.Items[0].Values[1].Value != tc.ref.Value {
				t.Fatalf("incorrect relationship: %s", out)
			}
		})
	}
}

func TestHealthProbeUsesSimulatorSOAP(t *testing.T) {
	e := start(t, "health-soap")
	health := &serve.Health{}
	completed := make(chan *httptest.ResponseRecorder, 1)
	health.Install(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual := httptest.NewRecorder()
		e.sim.Handler.ServeHTTP(actual, r)
		for key, values := range actual.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(actual.Code)
		_, _ = w.Write(actual.Body.Bytes())
		completed <- actual
	}))
	w := httptest.NewRecorder()
	health.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	select {
	case actual := <-completed:
		if actual.Code != 200 || !strings.Contains(actual.Body.String(), "RetrieveServiceContentResponse") {
			t.Fatalf("SOAP liveness probe failed: %s", actual.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SOAP probe stalled")
	}
}
