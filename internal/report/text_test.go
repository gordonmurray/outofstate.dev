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
