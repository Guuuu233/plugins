package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---- minimal proc equivalents (extracted boundary) ----

// findTool is proc.FindTool for the plugin: on PATH, else in the common
// user-level install folders a Claude CLI would land in when the login
// shell hasn't exported PATH into the launch context (a GUI-started
// plugin host, a systemd --user unit). Deliberately shallower than
// production's proc.UserBinDirs: no shell/registry probing, so the plugin
// runtime stays an ordinary user process with no extra authority.
func findTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".yarn", "bin"),
	}
	if matches, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "v*", "bin")); len(matches) > 0 {
		dirs = append(dirs, matches[len(matches)-1])
	}
	names := []string{name}
	if runtime.GOOS == "windows" {
		names = []string{name + ".exe", name + ".cmd", name}
	}
	for _, d := range dirs {
		for _, n := range names {
			p := filepath.Join(d, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// commandContext is proc.CommandContext for the plugin: no console window
// on Windows, a bounded WaitDelay after ctx ends so a detached child can't
// hold its pipes forever.
func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	hideWindow(cmd)
	return cmd
}

// ---- netproxy.EnvWith equivalent ----

// envWithProxy hands env the proxy choice: "" means ambient (system or
// inherited env is left alone), "direct" strips proxy vars, anything else
// is set as HTTP(S)_PROXY. The plugin's runtime supports only this single
// knob — per-account proxy routing stays the host's job.
func envWithProxy(choice string, env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	s := strings.TrimSpace(choice)
	if s == "" {
		return env
	}
	if s == "direct" {
		out := env[:0:len(env)]
		for _, e := range env {
			k, _, _ := strings.Cut(e, "=")
			if !proxyVar(k) {
				out = append(out, e)
			}
		}
		return out
	}
	out := make([]string, 0, len(env)+4)
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if proxyVar(k) {
			continue
		}
		out = append(out, e)
	}
	out = append(out,
		"HTTP_PROXY="+s, "HTTPS_PROXY="+s,
		"http_proxy="+s, "https_proxy="+s)
	return out
}

func proxyVar(k string) bool {
	switch strings.ToUpper(k) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	}
	return false
}
