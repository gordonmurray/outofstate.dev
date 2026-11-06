package report

import (
	"bytes"
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
