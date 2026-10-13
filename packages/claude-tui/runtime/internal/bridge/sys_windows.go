//go:build windows

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

// hideWindow makes sure a console child gets CREATE_NO_WINDOW.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}

// startTree starts cmd; on Windows the tree cleanup is best-effort via
// taskkill in Kill — the plugin runtime is a user process and exits with
// its host anyway.
type procTree struct {
	cmd *exec.Cmd
}

func startTree(cmd *exec.Cmd) (*procTree, error) {
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procTree{cmd: cmd}, nil
}

func (t *procTree) Wait() error {
	err := t.cmd.Wait()
	killTree(t.cmd)
	return err
}

func (t *procTree) Kill() { killTree(t.cmd) }

func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/t", "/f", "/pid", itoa(cmd.Process.Pid)).Run()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
