//go:build unix

package tfrun

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the child in a new process group, and makes
// cancellation kill the whole group.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		killGroup(cmd)
		return nil
	}
}

// killGroup kills every process in the child's process group. The group ID
// stays reserved while any member is alive, so this cannot hit an unrelated
// process; when the group is empty it is a no-op.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
