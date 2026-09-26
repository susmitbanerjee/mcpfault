// Package procutil starts and stops the processes mcpfault manages
// (agents, services, setup and check commands) the same way on every OS.
package procutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mcpfault/mcpfault/internal/config"
)

// Build creates a command. Shell strings run through the platform shell (sh -c / cmd /C);
// argument lists run directly.
func Build(c config.Command, dir string, extraEnv map[string]string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	switch {
	case c.Shell != "":
		cmd = shellCommand(c.Shell)
	case len(c.Argv) > 0:
		cmd = exec.Command(c.Argv[0], c.Argv[1:]...)
	default:
		return nil, fmt.Errorf("empty command")
	}
	cmd.Dir = dir
	cmd.Env = Env(extraEnv)
	setGroup(cmd)
	return cmd, nil
}

// Env returns the current environment plus extras, with this binary's directory
// prepended to PATH so child processes can launch `mcpfault stdio` without installing it.
func Env(extra map[string]string) []string {
	env := os.Environ()
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for i, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
				env[i] = k + "=" + dir + string(os.PathListSeparator) + v
			}
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// Run executes a command to completion with a timeout, killing its whole process tree on timeout.
type Result struct {
	ExitCode int
	TimedOut bool
	Err      error
}

func Run(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) Result {
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case err := <-done:
		return Result{ExitCode: exitCode(cmd, err), Err: nonExitErr(err)}
	case <-timer:
		KillTree(cmd)
		<-done
		return Result{ExitCode: -1, TimedOut: true}
	case <-ctx.Done():
		KillTree(cmd)
		<-done
		return Result{ExitCode: -1, Err: ctx.Err()}
	}
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func nonExitErr(err error) error {
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	return err
}

// WaitReady polls target until it answers. http(s):// URLs are ready on any HTTP
// response; tcp://host:port is ready when a connection succeeds.
func WaitReady(ctx context.Context, target string, timeout time.Duration, alive func() bool) error {
	deadline := time.Now().Add(timeout)
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("bad ready target %q: %w", target, err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		switch u.Scheme {
		case "tcp":
			if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
				c.Close()
				return nil
			}
		case "http", "https":
			if resp, err := client.Get(target); err == nil {
				resp.Body.Close()
				return nil
			}
		default:
			return fmt.Errorf("ready must be an http(s):// or tcp:// URL, got %q", target)
		}
		if alive != nil && !alive() {
			return fmt.Errorf("process exited before %s was ready", target)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s", target, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
