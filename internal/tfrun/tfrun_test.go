package tfrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const secretMarker = "OOS-SECRET-MARKER"

func fake(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "fake-terraform"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func stateFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"version":4,"lineage":"l","serial":1,"resources":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func opts(t *testing.T, mode string, extra ...string) Options {
	t.Helper()
	env := append([]string{"PATH=/usr/bin:/bin", "FAKE_MODE=" + mode, "FAKE_STATE=" + stateFile(t)}, extra...)
	return Options{Binary: fake(t), Timeout: 5 * time.Second, Environ: env}
}

func checkNoLeak(t *testing.T, e *Error) {
	t.Helper()
	if e == nil {
		return
	}
	if strings.Contains(e.Error()+e.Hint(), secretMarker) {
		t.Errorf("error text leaks subprocess output: %q / %q", e.Error(), e.Hint())
	}
}

func TestPullOK(t *testing.T) {
	ws, data, err := Pull(context.Background(), t.TempDir(), opts(t, "ok"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ws != "default" || !strings.Contains(string(data), `"lineage":"l"`) {
		t.Errorf("ws=%q data=%q", ws, data)
	}
}

func TestPullPinsReportedWorkspace(t *testing.T) {
	// "workspace show" reports staging even though the environment says
	// prod; "state pull" must receive exactly the reported workspace.
	dir := t.TempDir()
	envOut := filepath.Join(t.TempDir(), "env")
	argvOut := filepath.Join(t.TempDir(), "argv")
	ws, _, err := Pull(context.Background(), dir, opts(t, "ok",
		"TF_WORKSPACE=prod", "FAKE_WS=staging", "FAKE_ENV_OUT="+envOut, "FAKE_ARGV_OUT="+argvOut))
	if err != nil || ws != "staging" {
		t.Fatalf("ws=%q err=%v", ws, err)
	}
	pullEnv, readErr := os.ReadFile(envOut + ".state")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(pullEnv), "\nTF_WORKSPACE=staging\n") || strings.Contains(string(pullEnv), "TF_WORKSPACE=prod") {
		t.Error("state pull was not pinned to the reported workspace")
	}
	for which, want := range map[string]string{
		"workspace": "-chdir=" + dir + "\nworkspace\nshow\n",
		"state":     "-chdir=" + dir + "\nstate\npull\n",
	} {
		got, readErr := os.ReadFile(argvOut + "." + which)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != want {
			t.Errorf("%s argv = %q, want %q", which, got, want)
		}
	}
}

func TestHelperProcessesAreKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, data, err := Pull(context.Background(), t.TempDir(), opts(t, "helper", "FAKE_HELPER_PID="+pidFile))
	if err != nil || len(data) == 0 {
		t.Fatalf("data=%q err=%v", data, err)
	}
	// The helper would hold the pipe for 30s. Returning well before that
	// (allowing for a loaded CI machine) shows it was killed, not waited on;
	// the liveness check below proves it.
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s: a helper holding the pipe must not delay the run", d)
	}
	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if convErr != nil {
		t.Fatal(convErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("helper process %d survived the run", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestChildEnvironment(t *testing.T) {
	envOut := filepath.Join(t.TempDir(), "env")
	o := opts(t, "ok", "FAKE_ENV_OUT="+envOut,
		"TF_LOG=TRACE", "TF_LOG_PATH=/tmp/x", "TF_LOG_CORE=TRACE", "TF_LOG_PROVIDER=TRACE", "TF_LOG_SOMETHING_NEW=1",
		"TF_CLI_ARGS=-lock=false", "TF_CLI_ARGS_state_pull=-foo", "TF_INPUT=1", "TF_IN_AUTOMATION=",
		"AWS_PROFILE=kept")
	if _, _, err := Pull(context.Background(), t.TempDir(), o); err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"workspace", "state"} {
		b, err := os.ReadFile(envOut + "." + which)
		if err != nil {
			t.Fatal(err)
		}
		env := string(b)
		for _, gone := range []string{"TF_LOG=", "TF_LOG_PATH=", "TF_LOG_CORE=", "TF_LOG_PROVIDER=", "TF_LOG_SOMETHING_NEW=", "TF_CLI_ARGS=", "TF_CLI_ARGS_state_pull=", "TF_INPUT=1"} {
			if strings.Contains(env, "\n"+gone) || strings.HasPrefix(env, gone) {
				t.Errorf("%s: %s should be removed", which, gone)
			}
		}
		for _, kept := range []string{"TF_INPUT=0\n", "TF_IN_AUTOMATION=1\n", "AWS_PROFILE=kept\n"} {
			if !strings.Contains(env, kept) {
				t.Errorf("%s: missing %q", which, kept)
			}
		}
	}
}

func TestPullFailures(t *testing.T) {
	tests := []struct {
		mode string
		want Category
		code int
	}{
		{"ws_fail", CategoryNotInitialized, 1},
		{"auth", CategoryBackendAuth, 1},
		{"auth_short", CategoryExitError, 1},
		{"workspace_missing", CategoryWorkspaceMissing, 1},
		{"version", CategoryVersionUnsupported, 1},
		{"exit7", CategoryExitError, 7},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			_, data, err := Pull(context.Background(), t.TempDir(), opts(t, tt.mode))
			if err == nil || err.Category != tt.want {
				t.Fatalf("err = %v, want %s", err, tt.want)
			}
			if data != nil {
				t.Error("no data expected on failure")
			}
			if tt.want == CategoryExitError && err.ExitCode != tt.code {
				t.Errorf("exit code = %d, want %d", err.ExitCode, tt.code)
			}
			checkNoLeak(t, err)
		})
	}
}

func TestEmptyAndInvalidOutputAreReturnedForTheParser(t *testing.T) {
	// "state pull" exits 0 with no output when no snapshot exists. The
	// parser turns that into no_snapshot; the runner must not fail.
	_, data, err := Pull(context.Background(), t.TempDir(), opts(t, "empty"))
	if err != nil || len(data) != 0 {
		t.Errorf("empty: data=%q err=%v", data, err)
	}
	_, data, err = Pull(context.Background(), t.TempDir(), opts(t, "invalid"))
	if err != nil || len(data) == 0 {
		t.Errorf("invalid: data=%q err=%v", data, err)
	}
}

func TestTimeoutKillsTheChild(t *testing.T) {
	o := opts(t, "hang")
	o.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, _, err := Pull(context.Background(), t.TempDir(), o)
	if err == nil || err.Category != CategoryTimeout {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s: the process group was not killed promptly", d)
	}
	if !strings.Contains(err.Message(), "300ms") {
		t.Errorf("message = %q", err.Message())
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, _, err := Pull(ctx, t.TempDir(), opts(t, "hang"))
	if err == nil || err.Category != CategoryCancelled {
		t.Fatalf("err = %v", err)
	}
}

func TestOutputCaps(t *testing.T) {
	o := opts(t, "big")
	o.StdoutCap = 1 << 20
	if _, _, err := Pull(context.Background(), t.TempDir(), o); err == nil || err.Category != CategoryOutputTooLarge {
		t.Errorf("stdout cap: err = %v", err)
	}
	o = opts(t, "bigerr")
	o.StderrCap = 1 << 20
	if _, _, err := Pull(context.Background(), t.TempDir(), o); err == nil || err.Category != CategoryOutputTooLarge {
		t.Errorf("stderr cap: err = %v", err)
	}
}

func TestCapBoundary(t *testing.T) {
	o := opts(t, "exact", "FAKE_BYTES=1024")
	o.StdoutCap = 1024
	if _, data, err := Pull(context.Background(), t.TempDir(), o); err != nil || len(data) != 1024 {
		t.Errorf("exactly at the cap: len=%d err=%v", len(data), err)
	}
	o = opts(t, "exact", "FAKE_BYTES=1025")
	o.StdoutCap = 1024
	if _, _, err := Pull(context.Background(), t.TempDir(), o); err == nil || err.Category != CategoryOutputTooLarge {
		t.Errorf("one byte over the cap: err = %v", err)
	}
}

func TestClassify(t *testing.T) {
	tests := map[string]Category{
		`Error: Backend initialization required, please run "terraform init"`:                             CategoryNotInitialized,
		`Error: Inconsistent dependency lock file`:                                                        CategoryNotInitialized,
		`Error: Currently selected workspace "dev" does not exist`:                                        CategoryWorkspaceMissing,
		`Workspace "dev" doesn't exist.`:                                                                  CategoryWorkspaceMissing,
		`Error: Failed to get existing workspaces: AccessDenied: Access Denied status code: 403`:          CategoryBackendAuth,
		`Error: error loading state: ExpiredToken: The security token included in the request is expired`: CategoryBackendAuth,
		`Error: state snapshot was created by Terraform v1.99.0, which is newer than current v1.17.1`:     CategoryVersionUnsupported,
		// Mentions a workspace and "not found", but is not a missing workspace.
		`Error: object not found in bucket (workspace default)`: CategoryExitError,
		// Init wins over auth when Terraform asks for init.
		`Error: Backend initialization required, please run "terraform init". Credentials expired.`: CategoryNotInitialized,
		``: CategoryExitError,
	}
	for in, want := range tests {
		if got := classify([]byte(in)); got != want {
			t.Errorf("classify(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStdinIsClosed(t *testing.T) {
	_, data, err := Pull(context.Background(), t.TempDir(), opts(t, "stdin"))
	if err != nil || len(data) != 0 {
		t.Errorf("data=%q err=%v", data, err)
	}
}

func TestMissingDirectoryAndBinary(t *testing.T) {
	_, _, err := Pull(context.Background(), filepath.Join(t.TempDir(), "nope"), opts(t, "ok"))
	if err == nil || err.Category != CategoryDirectoryMissing || err.Hint() != "" {
		t.Errorf("dir: %v", err)
	}
	o := opts(t, "ok")
	o.Binary = "definitely-not-terraform-xyz"
	_, _, err = Pull(context.Background(), t.TempDir(), o)
	if err == nil || err.Category != CategoryBinaryNotFound || !strings.Contains(err.Message(), "definitely-not-terraform-xyz") {
		t.Errorf("binary: %v", err)
	}
}

func TestHintQuoting(t *testing.T) {
	e := &Error{Category: CategoryBackendAuth, Binary: "tofu", Dir: "./shared infra", Workspace: "prod's"}
	want := `TF_WORKSPACE='prod'"'"'s' tofu -chdir='./shared infra' state pull > /dev/null`
	if got := e.Hint(); got != want {
		t.Errorf("Hint() = %s\nwant      %s", got, want)
	}
	e = &Error{Category: CategoryExitError, Binary: "terraform", Dir: "./infra"}
	if got := e.Hint(); got != "terraform -chdir=./infra state pull > /dev/null" {
		t.Errorf("Hint() = %s", got)
	}
}

// processAlive reports whether pid is running. A zombie (killed, but not yet
// reaped by its new parent) counts as dead: it holds no pipes and runs no
// code. In a container without an init process nobody reaps it.
func processAlive(pid int) bool {
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// Format: pid (comm) state ...; comm can contain spaces.
		if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) {
			return stat[i+2] != 'Z'
		}
	}
	return syscall.Kill(pid, 0) == nil
}
