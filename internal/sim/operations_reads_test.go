package sim_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/activity"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan"
	vsanmethods "github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

// Operations 9.1.1 queries every host's extended vSAN object. The existing
// live QuerySyncingVsanObjectsSummary capture defines the idle result: no
// objects, bytes, or ETA. Unknown host references must still fault.
func TestOperationsSyncingSummary(t *testing.T) {
	e := startTopology(t, topology.Phase1("ops-syncing"))
	vc, err := vsan.NewClient(context.Background(), e.client)
	if err != nil {
		t.Fatal(err)
	}
	for _, cl := range e.sim.Clusters {
		for _, host := range cl.Hosts {
			ref := types.ManagedObjectReference{Type: "VsanSystemEx", Value: "vsanSystemEx-" + strings.TrimPrefix(host.Value, "host-")}
			res, err := vsanmethods.VsanQuerySyncingVsanObjects(context.Background(), vc.Client, &vsantypes.VsanQuerySyncingVsanObjects{This: ref, IncludeSummary: types.NewBool(true)})
			if err != nil {
				t.Fatal(err)
			}
			v := res.Returnval
			if v.TotalObjectsToSync != 0 || v.TotalBytesToSync != 0 || v.TotalRecoveryETA != 0 || len(v.Objects) != 0 || v.SyncingObjectRecoveryDetails == nil {
				t.Fatalf("unexpected syncing summary: %+v", v)
			}
		}
	}
	_, err = vsanmethods.VsanQuerySyncingVsanObjects(context.Background(), vc.Client, &vsantypes.VsanQuerySyncingVsanObjects{This: types.ManagedObjectReference{Type: "VsanSystemEx", Value: "missing"}})
	if err == nil {
		t.Fatal("unknown host accepted")
	}
}

func TestOperationsHostSecurityReads(t *testing.T) {
	e := startTopology(t, topology.Phase1("ops-host-security"))
	ctx := context.Background()
	images := map[types.ManagedObjectReference]bool{}
	for _, cl := range e.sim.Clusters {
		for _, ref := range cl.Hosts {
			var host mo.HostSystem
			if err := property.DefaultCollector(e.client).RetrieveOne(ctx, ref, []string{"configManager"}, &host); err != nil {
				t.Fatal(err)
			}
			res, err := methods.QueryLockdownExceptions(ctx, e.client, &types.QueryLockdownExceptions{This: *host.ConfigManager.HostAccessManager})
			if err != nil || len(res.Returnval) != 0 {
				t.Fatalf("lockdown exceptions: %v, %v", res, err)
			}
			if images[*host.ConfigManager.ImageConfigManager] {
				t.Fatal("hosts share the template manager instead of receiving their own manager")
			}
			images[*host.ConfigManager.ImageConfigManager] = true
			res2, err := methods.HostImageConfigGetAcceptance(ctx, e.client, &types.HostImageConfigGetAcceptance{This: *host.ConfigManager.ImageConfigManager})
			if err != nil || res2.Returnval != "vmware_accepted" {
				t.Fatalf("acceptance: %v, %v", res2, err)
			}
		}
	}
	res, err := methods.HostImageConfigGetAcceptance(ctx, e.client, &types.HostImageConfigGetAcceptance{This: types.ManagedObjectReference{Type: "HostImageConfigManager", Value: "ha-image-config-manager"}})
	if err != nil || res.Returnval != "vmware_accepted" {
		t.Fatalf("retained image manager reference: %v, %v", res, err)
	}
}

// Exact SearchIndex query captured from Operations, also sent to the live
// 9.1.1 management vCenter: QueryResponse contains an empty returnval.
func TestOperationsSearchIndexQuery(t *testing.T) {
	e := startTopology(t, topology.Phase1("ops-search"))
	body := fmt.Sprintf(`<Query xmlns="urn:vim25"><_this type="SearchIndex">%s</_this><querySpec><properties>@moRef</properties><properties>triggeredAlarmState</properties><resourceType>Datastore</resourceType><filters><predicates><propertyPath>summary.type</propertyPath><operator>Equal</operator><comparableValue xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xsi:type="xsd:string">VVOL</comparableValue></predicates></filters><limit>1000</limit></querySpec></Query>`, e.client.ServiceContent.SearchIndex.Value)
	for _, valid := range []bool{true, false} {
		payload := body
		if !valid {
			payload = strings.ReplaceAll(payload, "VVOL", "vsan")
		}
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/sdk", strings.NewReader(`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>`+payload+`</soap:Body></soap:Envelope>`))
		req.Header.Set("Content-Type", "text/xml")
		req.AddCookie(&http.Cookie{Name: "vmware_soap_session", Value: e.cookie})
		res, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if valid && (res.StatusCode != 200 || !strings.Contains(string(data), "<returnval>")) {
			t.Fatalf("query: %d %s", res.StatusCode, data)
		}
		if !valid && (res.StatusCode != 500 || !strings.Contains(string(data), "InvalidArgument")) {
			t.Fatalf("unsupported query accepted: %d %s", res.StatusCode, data)
		}
	}
}

func TestOperationsDisabledWCP(t *testing.T) {
	e := startTopology(t, topology.Phase1("ops-wcp"))
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/session", nil)
	req.SetBasicAuth("reader", "password")
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, path := range []string{"/wcp", "/api/vcenter/namespace-management/clusters", "/api/vcenter/namespace-management/clusters/", "/api/vcenter/namespaces/instances", "/api/vcenter/namespaces/instances/v2", "/api/vcenter/namespace-management/supervisors/summaries"} {
		t.Run(path, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
			req.Header.Set("vmware-api-session-id", token)
			res, err := e.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if path == "/wcp" {
				reason := activity.Classify(activity.Facts{Surface: activity.SurfaceOther, Path: path, HTTPMethod: http.MethodGet, Status: res.StatusCode})
				if reason == activity.ReasonUnknown {
					t.Fatal("known WCP probe remains unanswered in console")
				}
				if res.StatusCode != 405 || !strings.Contains(string(data), `"code":-32600`) {
					t.Fatalf("probe: %d %s", res.StatusCode, data)
				}
				return
			}
			expected := "[]"
			if strings.HasSuffix(path, "/summaries") {
				expected = `{"items":[]}`
			}
			if res.StatusCode != 200 || strings.TrimSpace(string(data)) != expected {
				t.Fatalf("disabled workload management: %d %s", res.StatusCode, data)
			}
			req.Header.Del("vmware-api-session-id")
			res, err = e.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 401 {
				t.Fatal("anonymous namespace read allowed")
			}
		})
	}
}

// The boxed Longs in Operations distinguish absent values from zero. Decode
// the wire response independently of govmomi's optional scalar fields.
func TestOperationsSyncingCountersPresent(t *testing.T) {
	e := startTopology(t, topology.Phase1("ops-syncing-wire"))
	ref := "vsanSystemEx-" + strings.TrimPrefix(e.sim.Clusters[0].Hosts[0].Value, "host-")
	body := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><VsanQuerySyncingVsanObjects xmlns="urn:vsan"><_this type="VsanSystemEx">` + ref + `</_this><start>0</start><limit>0</limit><includeSummary>true</includeSummary></VsanQuerySyncingVsanObjects></s:Body></s:Envelope>`
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/vsanHealth", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "vmware_soap_session", Value: e.cookie})
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded struct {
		Body struct {
			Response struct {
				Result struct {
					Objects  *int64 `xml:"totalObjectsToSync"`
					Bytes    *int64 `xml:"totalBytesToSync"`
					ETA      *int64 `xml:"totalRecoveryETA"`
					Recovery *struct {
						Active    *int64 `xml:"activeObjectsToSync"`
						Queued    *int64 `xml:"queuedObjectsToSync"`
						Suspended *int64 `xml:"suspendedObjectsToSync"`
					} `xml:"syncingObjectRecoveryDetails"`
				} `xml:"returnval"`
			} `xml:"VsanQuerySyncingVsanObjectsResponse"`
		} `xml:"Body"`
	}
	if err := xml.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	v := decoded.Body.Response.Result
	if res.StatusCode != 200 || v.Objects == nil || v.Bytes == nil || v.ETA == nil || v.Recovery == nil {
		t.Fatalf("incomplete idle summary: %+v", v)
	}
	for _, counter := range []*int64{v.Objects, v.Bytes, v.ETA, v.Recovery.Active, v.Recovery.Queued, v.Recovery.Suspended} {
		if counter == nil || *counter != 0 {
			t.Fatalf("counter must be explicitly zero: %v", counter)
		}
	}
}
