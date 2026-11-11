package sim

import (
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vim25/xml"
)

// govmomi's encoder applies omitempty to individual integer slice elements;
// the response's required value elements must preserve the sample positions.
func TestStoragePerfWireZeroSamples(t *testing.T) {
	original, err := xml.Marshal(&types.PerfMetricIntSeries{Value: []int64{0, 1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(original), "<value>") != 1 {
		t.Fatalf("upstream encoding changed: %s", original)
	}
	shaped, err := xml.Marshal(storagePerfSeries{Type: "PerfMetricIntSeries", Value: []int64{0, 1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shaped), "<value>0</value><value>1</value><value>0</value>") {
		t.Fatalf("zero samples missing: %s", shaped)
	}
}

func TestStoragePerfWireHandlesEmptySeries(t *testing.T) {
	var missing *types.PerfMetricIntSeries
	var missingEntity *types.PerfEntityMetric
	body := storagePerfBody{Res: &storagePerfResponse{Returnval: []storagePerfEntity{{metric: missingEntity}, {metric: &types.PerfEntityMetric{Value: []types.BasePerfMetricSeries{nil, missing, &types.PerfMetricIntSeries{}}}}, {metric: &types.PerfEntityMetricCSV{}}}}}
	b, err := xml.Marshal(&body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "PerfEntityMetricCSV") || !strings.Contains(string(b), "PerfMetricIntSeries") {
		t.Fatalf("valid empty series lost: %s", b)
	}
}
