//go:build !windows

package procutil

import (
	"os/exec"
	"syscall"
)

func shellCommand(s string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", s)
}

func setGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillTree kills a process and everything in its process group.
func KillTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
