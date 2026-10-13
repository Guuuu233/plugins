package bridge

// WSL stub for the extracted runtime. The production bridge supports
// running a WSL-installed claude.exe; the interactive PTY path this plugin
// ships is explicitly refused under WSL upstream (startInteractive refuses
// binary.wsl), and the plugin's first release targets the host the native
// CLI was installed on directly. The shape stays so the SDK stream path
// can be re-enabled later without touching the run code.

import (
	"context"
	"errors"
	"os/exec"
)

type wslTool struct{}

func wslFind(name string) (wslTool, bool) { return wslTool{}, false }

func (wslTool) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "false")
	return cmd
}

func (wslTool) Env(env []string, names ...string) []string { return env }

func (wslTool) Exe() (string, error) {
	return "", errors.New("wsl not supported by the plugin runtime")
}
