package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gordonmurray/outofstate.dev/internal/scan"
)

var update = flag.Bool("update", false, "rewrite golden files")

const secretMarker = "OOS-SECRET-MARKER"

func fixedNow() time.Time { return time.Date(2026, 11, 6, 10, 42, 0, 0, time.UTC) }

func td(name string) string { return filepath.Join("testdata", name) }

func oos(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb, fixedNow)
	if strings.Contains(out.String()+errb.String(), secretMarker) {
		t.Fatalf("secret marker in output of %v", args)
	}
	return code, out.String(), errb.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := td(name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from golden file; run 'go test ./cmd/oos -update' and review the diff.\n--- got ---\n%s", name, got)
	}
}

func TestVersionFlag(t *testing.T) {
	code, out, _ := oos(t, "--version")
	if code != scan.ExitOK || !strings.HasPrefix(out, "oos dev") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestReportJSONAndText(t *testing.T) {
	args := []string{"--inventory-file", td("inventory.json"),
		"--state-file", td("app.tfstate.json"), "--state-file", td("shared.tfstate.json")}

	code, out, stderr := oos(t, append(args, "--format", "json")...)
	if code != scan.ExitNoMatch {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, scan.ExitNoMatch, stderr)
	}
	golden(t, "report.golden.json", out)

	var rep struct {
		Summary  map[string]int `json:"summary"`
		Findings []struct {
			Identifier string `json:"identifier"`
			Status     string `json:"status"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"analytics-db": "no_match", "orders-db": "tracked",
		"legacy-reports": "tracked", "billing-cluster-1": "not_covered",
	}
	for _, f := range rep.Findings {
		if want[f.Identifier] != f.Status {
			t.Errorf("%s: status %s, want %s", f.Identifier, f.Status, want[f.Identifier])
		}
	}
	if rep.Findings[0].Identifier != "analytics-db" {
		t.Errorf("import candidates must come first, got %s", rep.Findings[0].Identifier)
	}

	code, out, _ = oos(t, args...)
	if code != scan.ExitNoMatch {
		t.Fatalf("text exit = %d", code)
	}
	golden(t, "report.golden.txt", out)
}

func TestOutputFileMatchesStdout(t *testing.T) {
	args := []string{"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json")}
	_, stdout, _ := oos(t, args...)
	path := filepath.Join(t.TempDir(), "outofstate.txt")
	code, out, _ := oos(t, append(args, "--output", path)...)
	if code != scan.ExitNoMatch || out != "" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != stdout {
		t.Error("--output content differs from stdout content")
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"everything tracked", []string{"--inventory-file", td("inventory_all_tracked.json"), "--state-file", td("app.tfstate.json")}, scan.ExitOK},
		{"unmatched instances", []string{"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json")}, scan.ExitNoMatch},
		{"a region failed", []string{"--inventory-file", td("inventory_region_failed.json"), "--state-file", td("app.tfstate.json"), "--state-file", td("shared.tfstate.json")}, scan.ExitPartial},
		{"a state file is broken", []string{"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json"), "--state-file", td("broken.tfstate.json")}, scan.ExitPartial},
		{"a state file is missing", []string{"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json"), "--state-file", td("nope.json")}, scan.ExitPartial},
		{"bad format", []string{"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json"), "--format", "yaml"}, scan.ExitFatal},
		{"no state files means . which is not supported yet", []string{"--inventory-file", td("inventory.json")}, scan.ExitFatal},
		{"--source not supported yet", []string{"--inventory-file", td("inventory.json"), "--source", "."}, scan.ExitFatal},
		{"no inventory: live discovery not implemented yet", []string{"--state-file", td("app.tfstate.json")}, scan.ExitFatal},
		{"unknown flag", []string{"--no-such-flag"}, scan.ExitFatal},
		{"unwritable output", []string{"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json"), "--output", filepath.Join("testdata", "no-such-dir", "x.txt")}, scan.ExitFatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, out, stderr := oos(t, tt.args...); code != tt.want {
				t.Errorf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tt.want, out, stderr)
			}
		})
	}
}

func TestPartialScanMarksUnverified(t *testing.T) {
	_, out, _ := oos(t, "--format", "json", "--inventory-file", td("inventory.json"),
		"--state-file", td("app.tfstate.json"), "--state-file", td("broken.tfstate.json"))
	var rep struct {
		Scan    struct{ Complete, StateComplete bool } `json:"scan"`
		Summary struct {
			NoMatch        int `json:"no_match"`
			UnableToVerify int `json:"unable_to_verify"`
		} `json:"summary"`
		Sources []struct {
			Status        string `json:"status"`
			ErrorCategory string `json:"error_category"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Scan.Complete || rep.Summary.NoMatch != 0 || rep.Summary.UnableToVerify != 2 {
		t.Errorf("%+v", rep)
	}
	if rep.Sources[1].Status != "failed" || rep.Sources[1].ErrorCategory != "invalid_json" {
		t.Errorf("sources: %+v", rep.Sources)
	}
}

func TestDuplicateStateFilesAreDeduplicated(t *testing.T) {
	_, out, _ := oos(t, "--format", "json", "--inventory-file", td("inventory.json"),
		"--state-file", td("app.tfstate.json"), "--state-file", "./"+td("app.tfstate.json"))
	var rep struct{ Sources []any }
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Sources) != 1 {
		t.Errorf("sources = %d, want 1", len(rep.Sources))
	}
}
