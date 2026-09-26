//go:build windows

package procutil

import (
	"os/exec"
	"strconv"
	"syscall"
)

func shellCommand(s string) *exec.Cmd {
	cmd := exec.Command("cmd")
	// Pass the line through untouched; Go's argument escaping would re-quote it for cmd.exe.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /S /C \"" + s + "\""}
	return cmd
}

func setGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr.HideWindow = true
}

// KillTree kills a process and everything it started.
func KillTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if kill.Run() != nil {
		_ = cmd.Process.Kill()
	}
}
