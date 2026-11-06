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

	"github.com/santhosh-tekuri/jsonschema/v6"

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
		{"no state files means . (terraform missing here, so partial)", []string{"--inventory-file", td("inventory.json"), "--tf-binary", "no-such-terraform-xyz"}, scan.ExitPartial},
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
		Scan struct {
			Complete      bool `json:"complete"`
			StateComplete bool `json:"state_complete"`
		} `json:"scan"`
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
	if rep.Scan.Complete || rep.Scan.StateComplete || rep.Summary.NoMatch != 0 || rep.Summary.UnableToVerify != 2 {
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

func TestReportsMatchSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	schema, err := c.Compile(filepath.Join("..", "..", "docs", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios := map[string][]string{
		"complete":          {"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json"), "--state-file", td("shared.tfstate.json")},
		"partial source":    {"--inventory-file", td("inventory.json"), "--state-file", td("partial.tfstate.json"), "--state-file", td("broken.tfstate.json"), "--state-file", td("nope.json")},
		"region failed":     {"--inventory-file", td("inventory_region_failed.json"), "--state-file", td("app.tfstate.json")},
		"empty inventory":   {"--inventory-file", td("inventory_empty.json"), "--state-file", td("app.tfstate.json")},
		"only not covered":  {"--inventory-file", td("inventory_not_covered_only.json"), "--state-file", td("app.tfstate.json")},
		"no cost requested": {"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json"), "--no-cost"},
		"duplicate state":   {"--inventory-file", td("inventory.json"), "--state-file", td("app.tfstate.json"), "--state-file", td("app-copy.tfstate.json")},
	}
	if err := os.WriteFile(td("app-copy.tfstate.json"), mustRead(t, td("app.tfstate.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(td("app-copy.tfstate.json")) })

	for name, args := range scenarios {
		t.Run(name, func(t *testing.T) {
			_, out, stderr := oos(t, append(args, "--format", "json")...)
			doc, err := jsonschema.UnmarshalJSON(strings.NewReader(out))
			if err != nil {
				t.Fatalf("%v; stderr: %s", err, stderr)
			}
			if err := schema.Validate(doc); err != nil {
				t.Errorf("report does not match schema: %v", err)
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type summaryReport struct {
	Scan struct {
		Complete          bool `json:"complete"`
		StateComplete     bool `json:"state_complete"`
		DiscoveryComplete bool `json:"discovery_complete"`
	} `json:"scan"`
	Regions []struct {
		Status        string `json:"status"`
		ErrorCategory string `json:"error_category"`
		Error         string `json:"error"`
	} `json:"regions"`
	Findings []struct {
		Identifier string `json:"identifier"`
		Status     string `json:"status"`
	} `json:"findings"`
}

func jsonReport(t *testing.T, args ...string) (int, summaryReport) {
	t.Helper()
	code, out, stderr := oos(t, append(args, "--format", "json")...)
	var r summaryReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v; stderr: %s", err, stderr)
	}
	return code, r
}

func statuses(r summaryReport) map[string]string {
	m := map[string]string{}
	for _, f := range r.Findings {
		m[f.Identifier] = f.Status
	}
	return m
}

func TestFailedRegionKeepsNoMatchFromOtherRegions(t *testing.T) {
	code, r := jsonReport(t, "--inventory-file", td("inventory_region_failed.json"),
		"--state-file", td("app.tfstate.json"), "--state-file", td("shared.tfstate.json"))
	if code != scan.ExitPartial {
		t.Errorf("exit = %d, want %d", code, scan.ExitPartial)
	}
	if r.Scan.Complete || !r.Scan.StateComplete || r.Scan.DiscoveryComplete {
		t.Errorf("scan = %+v", r.Scan)
	}
	if got := statuses(r)["analytics-db"]; got != "no_match" {
		t.Errorf("analytics-db = %s, want no_match (a failed region must not change other findings)", got)
	}
}

func TestPartialSourceKeepsValidMatches(t *testing.T) {
	code, r := jsonReport(t, "--inventory-file", td("inventory.json"), "--state-file", td("partial.tfstate.json"))
	if code != scan.ExitPartial || r.Scan.StateComplete {
		t.Errorf("exit = %d, scan = %+v", code, r.Scan)
	}
	st := statuses(r)
	if st["orders-db"] != "tracked" || st["analytics-db"] != "unable_to_verify" {
		t.Errorf("statuses = %v", st)
	}
}

func TestOutputFailureBeatsPartialScan(t *testing.T) {
	code, _, _ := oos(t, "--inventory-file", td("inventory.json"), "--state-file", td("broken.tfstate.json"),
		"--output", filepath.Join("testdata", "no-such-dir", "x.json"))
	if code != scan.ExitFatal {
		t.Errorf("exit = %d, want %d", code, scan.ExitFatal)
	}
}

func TestRegionErrorsAreFixedMessages(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		code, out, _ := oos(t, "--format", format, "--inventory-file", td("inventory_hostile_region_error.json"),
			"--state-file", td("app.tfstate.json"))
		if code != scan.ExitPartial {
			t.Errorf("%s: exit = %d", format, code)
		}
		for _, bad := range []string{"\x1b", "AccessDenied", "user/someone", `\u001b`} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output contains raw region error text %q", format, bad)
			}
		}
	}
	_, r := jsonReport(t, "--inventory-file", td("inventory_hostile_region_error.json"), "--state-file", td("app.tfstate.json"))
	failed := r.Regions[len(r.Regions)-1]
	if failed.ErrorCategory != "api_error" || failed.Error != "the AWS API returned an error" {
		t.Errorf("region = %+v", failed)
	}
}

func TestSchemaRejectsBadReports(t *testing.T) {
	schema, err := jsonschema.NewCompiler().Compile(filepath.Join("..", "..", "docs", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(mustRead(t, td("report.golden.json")))
	tampered := map[string]string{
		"unknown status":         strings.Replace(good, `"status": "no_match"`, `"status": "bogus"`, 1),
		"unknown matched_on key": strings.Replace(good, `"matched_on": "resource_id"`, `"matched_on": "name"`, 1),
		"wrong schema version":   strings.Replace(good, `"schema_version": 1`, `"schema_version": 2`, 1),
		"missing required field": strings.Replace(good, `"import_id"`, `"import_identifier"`, 1),
	}
	for name, doc := range tampered {
		if doc == good {
			t.Fatalf("%s: replacement did not apply", name)
		}
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(doc))
		if err != nil {
			t.Fatal(err)
		}
		if schema.Validate(v) == nil {
			t.Errorf("%s: schema accepted an invalid report", name)
		}
	}
}

func fakeTerraform(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "internal", "tfrun", "testdata", "fake-terraform"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSourceDirectories(t *testing.T) {
	state, err := filepath.Abs(td("app.tfstate.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_MODE", "ok")
	t.Setenv("FAKE_STATE", state)
	t.Setenv("TF_WORKSPACE", "prod")
	dir := t.TempDir()

	code, out, stderr := oos(t, "--format", "json", "--inventory-file", td("inventory.json"),
		"--tf-binary", fakeTerraform(t), "--source", dir)
	if code != scan.ExitNoMatch {
		t.Fatalf("exit = %d; stderr: %s", code, stderr)
	}
	var rep struct {
		Sources []struct {
			Name, Kind, Workspace, Status string
		} `json:"sources"`
		Findings []struct {
			Identifier string `json:"identifier"`
			Status     string `json:"status"`
			Matches    []struct {
				Workspace string `json:"workspace"`
			} `json:"matches"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	src := rep.Sources[0]
	if src.Name != dir || src.Kind != "directory" || src.Workspace != "prod" || src.Status != "ok" {
		t.Errorf("source: %+v", src)
	}
	for _, f := range rep.Findings {
		if f.Identifier == "orders-db" && (f.Status != "tracked" || f.Matches[0].Workspace != "prod") {
			t.Errorf("orders-db: %+v", f)
		}
	}
}

func TestFailedSourceShowsHintNotTerraformOutput(t *testing.T) {
	t.Setenv("FAKE_MODE", "auth")
	dir := t.TempDir()
	code, out, _ := oos(t, "--inventory-file", td("inventory.json"), "--tf-binary", fakeTerraform(t), "--source", dir)
	if code != scan.ExitPartial {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "could not authenticate to the state backend") ||
		!strings.Contains(out, "to see Terraform's output, run: TF_WORKSPACE=default") ||
		!strings.Contains(out, "state pull > /dev/null") {
		t.Errorf("missing message or hint:\n%s", out)
	}
	if strings.Contains(out, "AccessDenied") {
		t.Error("Terraform's stderr reached the report")
	}
}

func TestMissingAWSProfileIsFatal(t *testing.T) {
	// Isolate from any real AWS configuration on the machine.
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Setenv(k, "")
	}
	code, _, stderr := oos(t, "--profile", "oos-test-no-such-profile", "--state-file", td("app.tfstate.json"))
	if code != scan.ExitFatal || !strings.Contains(stderr, "profile does not exist") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = oos(t, "--state-file", td("app.tfstate.json"))
	if code != scan.ExitFatal || !strings.Contains(stderr, "no region is configured") {
		t.Errorf("no region: exit = %d, stderr = %q", code, stderr)
	}
}
