// Package tfrun runs "terraform state pull" (or the OpenTofu equivalent) in
// a working directory under a strict contract:
//
//   - never init, refresh, plan or anything that writes;
//   - stdin closed, TF_INPUT=0, TF_IN_AUTOMATION=1, so nothing can prompt;
//   - a deadline, and cancellation kills the child;
//   - stdout and stderr captured separately and capped;
//   - Terraform logging and CLI-argument variables removed from the child
//     environment, everything else (backend credentials) inherited;
//   - the pull is pinned to the workspace reported by "workspace show";
//   - failures are fixed categories. stderr is classified in memory and
//     discarded: it can contain state fragments or secrets.
package tfrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Category is a fixed failure class with a fixed message.
type Category string

// Failure categories.
const (
	CategoryBinaryNotFound     Category = "binary_not_found"
	CategoryDirectoryMissing   Category = "directory_missing"
	CategoryNotInitialized     Category = "not_initialized"
	CategoryBackendAuth        Category = "backend_auth"
	CategoryWorkspaceMissing   Category = "workspace_missing"
	CategoryVersionUnsupported Category = "version_unsupported"
	CategoryTimeout            Category = "timeout"
	CategoryOutputTooLarge     Category = "output_too_large"
	CategoryCancelled          Category = "cancelled"
	CategoryExitError          Category = "exit_error"
)

// Error describes a failed pull using only values oos controls.
type Error struct {
	Category Category
	Binary   string
	Dir      string
	ExitCode int
	Timeout  time.Duration
	// Workspace is set when it was known before the failure.
	Workspace string
}

func (e *Error) Error() string { return e.Message() }

// Message is the fixed, printable explanation for the category.
func (e *Error) Message() string {
	switch e.Category {
	case CategoryBinaryNotFound:
		return fmt.Sprintf("%s is not on PATH", e.Binary)
	case CategoryDirectoryMissing:
		return "the directory does not exist"
	case CategoryNotInitialized:
		return fmt.Sprintf("the directory is not initialised. Run '%s init' there", e.Binary)
	case CategoryBackendAuth:
		return "could not authenticate to the state backend"
	case CategoryWorkspaceMissing:
		return "the selected workspace does not exist"
	case CategoryVersionUnsupported:
		return fmt.Sprintf("this %s version cannot read the state", e.Binary)
	case CategoryTimeout:
		return fmt.Sprintf("timed out after %s", e.Timeout)
	case CategoryOutputTooLarge:
		return "the state is larger than the output limit"
	case CategoryCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("%s exited with code %d", e.Binary, e.ExitCode)
	}
}

// Hint is the command to run by hand to see Terraform's own output.
func (e *Error) Hint() string {
	switch e.Category {
	case CategoryBinaryNotFound, CategoryDirectoryMissing, CategoryCancelled:
		return ""
	}
	cmd := fmt.Sprintf("%s -chdir=%s state pull > /dev/null", shellQuote(e.Binary), shellQuote(e.Dir))
	if e.Workspace != "" {
		cmd = "TF_WORKSPACE=" + shellQuote(e.Workspace) + " " + cmd
	}
	return cmd
}

// Options for Pull.
type Options struct {
	Binary    string        // "terraform" or "tofu"; default "terraform"
	Timeout   time.Duration // per subprocess; default 2m
	StdoutCap int64         // default 256 MiB
	StderrCap int64         // default 1 MiB
	// Environ is the base environment; default os.Environ(). Tests set it.
	Environ []string
}

const (
	defaultStdoutCap = 256 << 20
	defaultStderrCap = 1 << 20
)

func (o *Options) defaults() {
	if o.Binary == "" {
		o.Binary = "terraform"
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Minute
	}
	if o.StdoutCap <= 0 {
		o.StdoutCap = defaultStdoutCap
	}
	if o.StderrCap <= 0 {
		o.StderrCap = defaultStderrCap
	}
	if o.Environ == nil {
		o.Environ = os.Environ()
	}
}

// Pull returns the workspace and the raw state bytes for dir. The bytes are
// only for tfstate.Parse: never log or store them.
func Pull(ctx context.Context, dir string, opts Options) (workspace string, state []byte, err *Error) {
	opts.defaults()
	base := &Error{Binary: opts.Binary, Dir: dir, Timeout: opts.Timeout}

	if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
		base.Category = CategoryDirectoryMissing
		return "", nil, base
	}
	bin, lookErr := exec.LookPath(opts.Binary)
	if lookErr != nil {
		base.Category = CategoryBinaryNotFound
		return "", nil, base
	}

	env := childEnv(opts.Environ)

	out, runErr := run(ctx, bin, dir, env, opts, "workspace", "show")
	if runErr != nil {
		runErr.Binary, runErr.Dir, runErr.Timeout = opts.Binary, dir, opts.Timeout
		return "", nil, runErr
	}
	workspace = strings.TrimSpace(string(out))
	if workspace == "" || strings.ContainsAny(workspace, "\n\r\x00") {
		base.Category = CategoryExitError
		return "", nil, base
	}

	env = append(env, "TF_WORKSPACE="+workspace)
	state, runErr = run(ctx, bin, dir, env, opts, "state", "pull")
	if runErr != nil {
		runErr.Binary, runErr.Dir, runErr.Timeout, runErr.Workspace = opts.Binary, dir, opts.Timeout, workspace
		return workspace, nil, runErr
	}
	return workspace, state, nil
}

func run(ctx context.Context, bin, dir string, env []string, opts Options, args ...string) ([]byte, *Error) {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	full := append([]string{"-chdir=" + dir}, args...)
	cmd := exec.CommandContext(ctx, bin, full...) //nolint:gosec // binary and dir are the user's own CLI arguments
	cmd.Env = env
	cmd.Stdin = nil // reads from the null device
	// The child runs in its own process group. Cancellation (deadline, cap,
	// Ctrl-C) kills the whole group, and so does the cleanup below once the
	// main process has exited, so helper processes cannot outlive the run.
	setProcessGroup(cmd)

	// oos owns the pipes, so cmd.Wait returns as soon as the main process
	// exits instead of waiting for whoever else holds the pipe open.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, &Error{Category: CategoryExitError, ExitCode: -1}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return nil, &Error{Category: CategoryExitError, ExitCode: -1}
	}
	cmd.Stdout, cmd.Stderr = outW, errW

	kill := func() { killGroup(cmd); cancel() }
	stdout := &capBuffer{max: opts.StdoutCap, onExceed: kill}
	stderr := &capBuffer{max: opts.StderrCap, onExceed: kill}

	if err := cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW)
		return nil, &Error{Category: CategoryExitError, ExitCode: -1}
	}
	closeAll(outW, errW) // the child has its own copies

	var readers sync.WaitGroup
	for _, p := range []struct {
		r   *os.File
		buf *capBuffer
	}{{outR, stdout}, {errR, stderr}} {
		readers.Add(1)
		go func() {
			defer readers.Done()
			_, _ = io.Copy(p.buf, p.r)
		}()
	}

	waitErr := cmd.Wait()
	killGroup(cmd) // anything the child left behind
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		// Something outside the process group still holds a pipe.
	}
	closeAll(outR, errR)
	<-drained

	switch {
	case stdout.isExceeded() || stderr.isExceeded():
		return nil, &Error{Category: CategoryOutputTooLarge}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, &Error{Category: CategoryTimeout}
	case errors.Is(ctx.Err(), context.Canceled):
		return nil, &Error{Category: CategoryCancelled}
	case waitErr != nil:
		e := &Error{Category: classify(stderr.bytes()), ExitCode: -1}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			e.ExitCode = exitErr.ExitCode()
		}
		return nil, e
	}
	return stdout.bytes(), nil
}

// drainTimeout bounds how long oos waits for output after the process group
// has been killed.
const drainTimeout = 2 * time.Second

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}

// workspaceMissingRE matches Terraform's and OpenTofu's messages for a
// selected workspace that does not exist, not any mention of "workspace".
var workspaceMissingRE = regexp.MustCompile(`workspace "[^"\n]*" (does not exist|doesn't exist)|currently selected workspace[^\n]*(does not exist|doesn't exist)`)

// classify maps Terraform's stderr to a category. stderr is not kept.
func classify(stderr []byte) Category {
	s := strings.ToLower(string(stderr))
	has := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
	switch {
	case has("terraform init", "tofu init", "initialization required", "initialisation required", "not initialized",
		"required plugins are not installed", "inconsistent dependency lock file", "provider requirements cannot be satisfied"):
		return CategoryNotInitialized
	case workspaceMissingRE.MatchString(s):
		return CategoryWorkspaceMissing
	case has("no valid credential sources", "accessdenied", "access denied", "expiredtoken", "invalidclienttokenid",
		"signaturedoesnotmatch", "unauthorized", "status code: 403", "statuscode: 403", "sso session", "token has expired",
		"failed to refresh cached credentials", "invalid credentials"):
		return CategoryBackendAuth
	case has("unsupported state file format", "state snapshot was created by", "newer version of terraform",
		"newer version of opentofu", "unsupported state format"):
		return CategoryVersionUnsupported
	}
	return CategoryExitError
}

// childEnv removes variables that could log state contents (every TF_LOG*) or change the
// commands oos runs, then forces non-interactive behaviour. TF_WORKSPACE is
// kept for "workspace show" (it is how users select a workspace) and set
// explicitly for "state pull".
func childEnv(base []string) []string {
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "TF_LOG", strings.HasPrefix(name, "TF_LOG_"),
			name == "TF_INPUT", name == "TF_IN_AUTOMATION",
			name == "TF_CLI_ARGS", strings.HasPrefix(name, "TF_CLI_ARGS_"):
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TF_INPUT=0", "TF_IN_AUTOMATION=1")
}

// capBuffer stores up to max bytes, then records that the limit was hit and
// kills the command. The buffer is a field, not embedded: an embedded
// bytes.Buffer would expose ReadFrom, and io.Copy would bypass Write.
type capBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	max      int64
	exceeded bool
	onExceed func()
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exceeded {
		return len(p), nil // keep draining so the child is not blocked
	}
	if int64(c.buf.Len())+int64(len(p)) > c.max {
		c.exceeded = true
		c.buf.Reset()
		c.onExceed()
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capBuffer) isExceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exceeded
}

func (c *capBuffer) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Bytes()
}

// shellQuote quotes s for a POSIX shell when it contains anything unusual.
func shellQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && !strings.ContainsRune("-_./:@+=,", r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
