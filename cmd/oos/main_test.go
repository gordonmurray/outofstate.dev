package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, exitOK, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "oos dev") {
		t.Errorf("stdout = %q, want prefix %q", stdout.String(), "oos dev")
	}
}

func TestUnknownFlagIsFatal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--no-such-flag"}, &stdout, &stderr); code != exitFatal {
		t.Fatalf("exit code = %d, want %d", code, exitFatal)
	}
}

func TestStubIsFatal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != exitFatal {
		t.Fatalf("exit code = %d, want %d", code, exitFatal)
	}
}
