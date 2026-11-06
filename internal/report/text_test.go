package report

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestClean(t *testing.T) {
	tests := map[string]string{
		"orders-db":       "orders-db",
		"café":            "café",
		"evil\x1b[31mred": `evil\x1b[31mred`,
		"line\nbreak":     `line\nbreak`,
		"bell\a":          `bell\a`,
		"zero\u200bwidth": `zero\u200bwidth`,
	}
	for in, want := range tests {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTextNeverPrintsRawEscapes(t *testing.T) {
	r := &Report{
		Scan:    Scan{Time: time.Unix(0, 0), AccountID: "123456789012", Regions: []string{"eu-west-1"}, Complete: true},
		Sources: []Source{{Name: "src\x1b]0;title\a", Workspace: "default", Status: "ok"}},
		Findings: []Finding{{
			Status: "no_match", Identifier: "db\x1b[2J", ARN: "arn", Tags: map[string]string{"k\x1b": "v\x07"},
		}},
	}
	var buf bytes.Buffer
	if err := WriteText(&buf, r); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(buf.String(), "\x1b\x07") {
		t.Errorf("raw control characters in text output:\n%q", buf.String())
	}
}

func TestColour(t *testing.T) {
	r := &Report{
		Scan:    Scan{Time: time.Unix(0, 0), AccountID: "123456789012", Regions: []string{"eu-west-1"}, Complete: true},
		Sources: []Source{{Name: "src", Workspace: "default", Status: "ok"}},
		Findings: []Finding{
			{Status: "no_match", Identifier: "candidate\x1b[2J"},
			{Status: "tracked", Identifier: "tracked-db", Matches: []Match{{Source: "src", Workspace: "default", Address: "aws_db_instance.x", Lifecycle: "current", MatchedOn: "resource_id"}}},
		},
	}
	var plain, coloured bytes.Buffer
	if err := WriteText(&plain, r); err != nil {
		t.Fatal(err)
	}
	if err := WriteTextWith(&coloured, r, TextOptions{Color: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "\x1b") {
		t.Error("plain output has escape codes")
	}
	if !strings.Contains(coloured.String(), "\x1b[32m") {
		t.Error("coloured output has no green")
	}
	// The only escapes in coloured output are the ones oos adds.
	stripped := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(coloured.String(), "")
	if stripped != plain.String() {
		t.Errorf("colour changed the words:\n%s\nvs\n%s", stripped, plain.String())
	}
	if strings.Contains(stripped, "\x1b") {
		t.Error("a raw escape from the data reached the terminal")
	}
}

func TestCostLines(t *testing.T) {
	base := func(c *CostBlock) string {
		r := &Report{Scan: Scan{Time: time.Unix(0, 0), AccountID: "123456789012", Regions: []string{"us-west-2"}, Complete: true}, Cost: c}
		var buf bytes.Buffer
		if err := WriteText(&buf, r); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	out := base(&CostBlock{Available: true, Currency: "USD", Periods: []CostPeriod{
		{Name: "last_6_months", Label: "May–Oct 2026", Total: "2140.17"},
		{Name: "last_30_days", Label: "7 Oct–5 Nov 2026", Total: "410.0249", Estimated: true,
			Breakdown: map[string]string{"instances": "300", "multi_az": "89.51", "storage": "20.5149", "backup": "0", "proxy": "0", "other": "0.0001"}},
	}})
	for _, want := range []string{
		"RDS charges in us-west-2 (amortised, USD): $2,140.17 May–Oct 2026 · $410.02 7 Oct–5 Nov 2026 (estimated)",
		"last 30 days: instances $300.00 · Multi-AZ $89.51 · storage $20.51 · other $0.00",
		"not the cost of the import candidates",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "backup") || strings.Contains(out, "proxy") {
		t.Errorf("zero buckets should be hidden:\n%s", out)
	}
	if out := base(&CostBlock{Available: false, Reason: "access_denied", Message: "access denied (needs ce:GetCostAndUsage)"}); !strings.Contains(out, "RDS charges: unavailable (access denied (needs ce:GetCostAndUsage))") {
		t.Errorf("unavailable:\n%s", out)
	}
	if out := base(nil); strings.Contains(out, "RDS charges") {
		t.Errorf("no cost block should print nothing:\n%s", out)
	}
}

// Two instances with the same name in different regions must be
// distinguishable in the text report.
func TestRowsShowTheRegion(t *testing.T) {
	r := &Report{
		Scan: Scan{Time: time.Unix(0, 0), AccountID: "123456789012", Regions: []string{"eu-west-1", "us-east-1"}, Complete: true},
		Findings: []Finding{
			{Status: "no_match", Identifier: "orders-db", Region: "eu-west-1"},
			{Status: "no_match", Identifier: "orders-db", Region: "us-east-1"},
			{Status: "tracked", Identifier: "users-db", Region: "us-east-1", Matches: []Match{
				{Source: "a", Workspace: "default", Address: "aws_db_instance.x", Lifecycle: "current", MatchedOn: "resource_id"},
				{Source: "b", Workspace: "default", Address: "aws_db_instance.y", Lifecycle: "current", MatchedOn: "resource_id"}}},
			{Status: "not_covered", Identifier: "aurora-1", Region: "eu-west-1", NotCoveredReason: "member of an Aurora or Multi-AZ DB cluster"},
			{Status: "unable_to_verify", Identifier: "maybe-db", Region: "ap-southeast-3"},
		},
	}
	var buf bytes.Buffer
	if err := WriteText(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"  orders-db                eu-west-1       ",
		"  orders-db                us-east-1       ",
		"  users-db                 us-east-1             a (default) aws_db_instance.x",
		"                                                 b (default) aws_db_instance.y",
		"  aurora-1                 eu-west-1             member of",
		"  maybe-db                 ap-southeast-3  ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
