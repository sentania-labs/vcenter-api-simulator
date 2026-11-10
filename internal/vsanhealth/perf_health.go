package vsanhealth

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/sentania/vcenter-api-simulator/internal/topology"
	vim "github.com/vmware/govmomi/vim25/types"
	vsan "github.com/vmware/govmomi/vsan/types"
)

//go:embed fixtures/perf-health-online.json
var perfHealthOnline []byte

//go:embed fixtures/perf-health-offline.json
var perfHealthOffline []byte

type capturedHealthGroup struct {
	GroupID     string `json:"groupId"`
	GroupName   string `json:"groupName"`
	GroupHealth string `json:"groupHealth"`
	Tests       []struct {
		ID          string `json:"testId"`
		Name        string `json:"testName"`
		Description string `json:"testDescription"`
		Short       string `json:"testShortDescription"`
		Health      string `json:"testHealth"`
		Details     []struct {
			Label   string                                   `json:"label"`
			Columns []vsan.VsanClusterHealthResultColumnInfo `json:"columns"`
			Rows    []vsan.VsanClusterHealthResultRow        `json:"rows"`
		} `json:"testDetails"`
		Actions []vsan.VsanClusterHealthAction `json:"testActions"`
	} `json:"groupTests"`
}

func (m *PerfManager) perfHealthGroup(cl *topology.Cluster, primary bool) (*vsan.VsanClusterHealthGroup, error) {
	source := perfHealthOnline
	if !primary {
		source = perfHealthOffline
	}
	var groups []capturedHealthGroup
	if err := json.Unmarshal(source, &groups); err != nil {
		return nil, err
	}
	raw := groups[0]
	group := &vsan.VsanClusterHealthGroup{GroupId: raw.GroupID, GroupName: raw.GroupName, GroupHealth: raw.GroupHealth}
	for _, test := range raw.Tests {
		out := vsan.VsanClusterHealthTest{TestId: test.ID, TestName: test.Name, TestDescription: test.Description, TestShortDescription: test.Short, TestHealth: test.Health, TestActions: test.Actions}
		for _, detail := range test.Details {
			table := &vsan.VsanClusterHealthResultTable{VsanClusterHealthResultBase: vsan.VsanClusterHealthResultBase{Label: detail.Label}, Columns: detail.Columns, Rows: detail.Rows}
			if detail.Label == "Node Information" && cl != nil {
				table.Rows = nil
				for i, h := range cl.Hosts {
					yes := "No"
					if i == 0 {
						yes = "Yes"
					}
					table.Rows = append(table.Rows, vsan.VsanClusterHealthResultRow{Values: []string{fmt.Sprintf("mor:ManagedObjectReference:HostSystem:%s", m.env.hostRefs[h].Value), yes, yes}})
				}
			}
			out.TestDetails = append(out.TestDetails, table)
		}
		group.GroupTests = append(group.GroupTests, out)
	}
	return group, nil
}

var _ vim.BaseDynamicData = (*vsan.VsanClusterHealthGroup)(nil)
