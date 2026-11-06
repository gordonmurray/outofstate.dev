package scan

import (
	"bytes"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/gordonmurray/outofstate.dev/internal/cost"
	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/report"
)

func rat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

func period(label, start, end, total string, estimated bool) *cost.Period {
	p := &cost.Period{Label: label, Start: start, End: end, Total: rat(total), Estimated: estimated, Breakdown: map[cost.Bucket]*big.Rat{}}
	for _, b := range cost.Buckets {
		p.Breakdown[b] = new(big.Rat)
	}
	p.Breakdown[cost.BucketInstances] = rat(total)
	return p
}

func inventory() *discover.Inventory {
	return &discover.Inventory{Partition: "aws", AccountID: "123456789012", Regions: []discover.RegionResult{{Region: "us-west-2"}}}
}

func TestCostBlockAndSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	schema, err := c.Compile(filepath.Join("..", "..", "docs", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	available := &cost.Result{Available: true, Currency: "USD",
		Months: period("May–Oct 2026", "2026-05-01", "2026-11-01", "2140.1700", false),
		Last30: period("7 Oct–5 Nov 2026", "2026-10-07", "2026-11-06", "410.02", true)}
	tests := map[string]*cost.Result{
		"available":   available,
		"unavailable": {Reason: cost.ReasonAccessDenied},
		"no cost":     nil,
	}
	for name, c := range tests {
		t.Run(name, func(t *testing.T) {
			rep := Run(Input{Inventory: inventory(), Cost: c, Version: "test", Time: time.Unix(0, 0)})
			var buf bytes.Buffer
			if err := report.WriteJSON(&buf, rep); err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(doc); err != nil {
				t.Errorf("schema: %v", err)
			}
		})
	}

	b := costBlock(available)
	if !b.Available || b.Metric != "AmortizedCost" || b.Scope != CostScope || len(b.Periods) != 2 {
		t.Fatalf("%+v", b)
	}
	if b.Periods[0].Name != "last_6_months" || b.Periods[0].Total != "2140.17" || b.Periods[0].Breakdown["instances"] != "2140.17" ||
		b.Periods[0].Breakdown["storage"] != "0" || b.Periods[1].Name != "last_30_days" || !b.Periods[1].Estimated {
		t.Errorf("%+v", b.Periods)
	}
	u := costBlock(&cost.Result{Reason: cost.ReasonNotEnabled})
	if u.Available || u.Reason != "cost_explorer_not_enabled" || !strings.Contains(u.Message, "not enabled") {
		t.Errorf("%+v", u)
	}
}

func TestSchemaRejectsBadCostBlocks(t *testing.T) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	schema, err := c.Compile(filepath.Join("..", "..", "docs", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	good := Run(Input{Inventory: inventory(), Version: "test", Time: time.Unix(0, 0), Cost: &cost.Result{Available: true, Currency: "USD",
		Months: period("May–Oct 2026", "2026-05-01", "2026-11-01", "10", false),
		Last30: period("7 Oct–5 Nov 2026", "2026-10-07", "2026-11-06", "1", false)}})
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, good); err != nil {
		t.Fatal(err)
	}
	doc := buf.String()
	tampered := map[string]string{
		"available without periods":  strings.Replace(doc, `"periods"`, `"periodz"`, 1),
		"unavailable without reason": strings.Replace(strings.Replace(doc, `"available": true`, `"available": false`, 1), `"scope"`, `"scope"`, 1),
		"numeric amount":             strings.Replace(doc, `"total": "10"`, `"total": 10`, 1),
		"float-like amount":          strings.Replace(doc, `"total": "10"`, `"total": "1e3"`, 1),
		"missing bucket":             strings.Replace(doc, `"proxy": "0",`, ``, 1),
		"bad date":                   strings.Replace(doc, `"start": "2026-05-01"`, `"start": "May 2026"`, 1),
	}
	for name, d := range tampered {
		if d == doc {
			t.Fatalf("%s: replacement did not apply", name)
		}
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(d))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if schema.Validate(v) == nil {
			t.Errorf("%s: schema accepted an invalid cost block", name)
		}
	}
}
