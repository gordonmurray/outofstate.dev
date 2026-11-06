//go:build !unix

package tfrun

import "os/exec"

// setProcessGroup is a no-op where process groups are not available; the
// default cancellation kills the child process only.
func setProcessGroup(*exec.Cmd) {}

// killGroup kills the child process only.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
