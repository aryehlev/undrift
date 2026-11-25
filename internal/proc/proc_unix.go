//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group and kills the
// whole group on cancellation, so agents cannot leave orphans behind.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
