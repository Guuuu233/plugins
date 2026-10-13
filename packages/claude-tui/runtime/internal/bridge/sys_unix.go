//go:build !windows

package bridge

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

func runtimeGOOS() string                               { return runtime.GOOS }
func filepathJoinImpl(elem ...string) string            { return filepath.Join(elem...) }
func filepathGlobImpl(pattern string) ([]string, error) { return filepath.Glob(pattern) }

// hideWindow is a no-op on Unix.
func hideWindow(*exec.Cmd) {}

// startTree starts cmd in its own session so the group can be ended with it.
type procTree struct {
	cmd *exec.Cmd
}

func startTree(cmd *exec.Cmd) (*procTree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procTree{cmd: cmd}, nil
}

// Wait waits for the command, then kills its leftover group.
func (t *procTree) Wait() error {
	err := t.cmd.Wait()
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}

// Kill ends the process and its group.
func (t *procTree) Kill() {
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		_ = t.cmd.Process.Kill()
	}
}
