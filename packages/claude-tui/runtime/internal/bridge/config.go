package bridge

// Package bridge is the extracted Magpie Claude TUI subscription runtime:
// the genuine Claude Code CLI driven through its interactive PTY wrapper
// (or the SDK stream path), with caller tools parked over the packaged
// stdio MCP helper. It is a standalone ordinary-user process; the host
// boundary below replaces the deployed gateway's appdir/provider/proc/
// netproxy coupling. No root broker, no attestation protocol.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Config is the runtime's external-facing settings: everything the
// production gateway took from its own appdir/provider/proc wiring.
type Config struct {
	// StateDir is where the interactive checkpoint file lives
	// (claude-interactive-sessions.json), the plugin-owned equivalent of
	// appdir.Config(). Default: ~/.config/magpie-tui-runtime (0600 tree).
	StateDir string

	// ClaudeConfigDir overrides ~/.claude discovery (CLAUDE_CONFIG_DIR).
	ClaudeConfigDir string

	// BridgePath is the PTY wrapper executable (the packaged claude-bridge
	// CLI). The MAGPIE_CLAUDE_INTERACTIVE_BRIDGE environment variable still
	// wins when set, matching production behavior; otherwise this is the
	// packaged wrapper entrypoint. Empty disables the interactive path.
	BridgePath string

	// HelperCommand is argv[0]…argv[n-1] that invokes the vendored MCP
	// helper: "<binary>" invoked as "<binary> claude-mcp-helper", or the
	// packaged standalone helper binary path invoked with no subcommand.
	// Production calls the Magpie executable's claude-mcp-helper
	// subcommand; the plugin ships its own binary instead.
	// Deprecated in favour of HelperArgv: a single string cannot carry an
	// executable path containing spaces (macOS install dirs do).
	HelperCommand string
	// HelperArgv is the helper invocation as an explicit argv: argv[0] is
	// the binary, the rest literal arguments, so spaced paths stay whole.
	// When set it wins over HelperCommand.
	HelperArgv []string

	// CallbackPort is the loopback port the runtime's HTTP listener binds;
	// it is needed by callbackBaseURL so the MCP helper can reach us.
	CallbackPort int

	// Models is the advertised model set (id → display name). The first
	// entry is the default when a request names none; the plugin mirrors
	// this list into its own configuration.
	Models map[string]string

	// Proxy, when set, is handed to Claude Code's environment (HTTP(S)_PROXY
	// style value or "direct"). "" leaves the ambient env alone.
	Proxy string

	// SearchHandler answers the runtime's synthetic magpie web_search tool.
	// nil disables the synthetic tool (production: the server's search
	// chain). Failure surfaces to the caller as a tool error, not a replay.
	SearchHandler func(ctx context.Context, query string) (string, []Hit, error)

	// NoteClaudeLimits / SignInFailure / ClaudeCodeMovedOff mirror the
	// production provider.* hooks. The standalone runtime has no saved
	// account store, so the defaults are no-ops; they are kept as fields so
	// a host embedding the package can wire real ones.
	NoteLimits       func(user string, limits []ClaudeLimit)
	NoteSignInFailed func(user, version, message string)
	MovedOff         func(user string) bool
	// LoginVersion reports the credential fingerprint the run began on,
	// used to match NoteSignInFailed to the account that served the run.
	// "" when the runtime has no notion of saved credentials.
	LoginVersion func(user string) string
	// HandedOver is invoked when the runtime learns a saved account was
	// moved over to Claude Code's own sign-in. The standalone runtime does
	// not watch that; hosts that do can register a func here. It is called
	// with the account user; the default drops idle runs owned by it.
	HandedOver func(user string)
}

// ClaudeLimit is one allowance window as Claude Code tells it: its kind
// (five_hour, weekly, sonnet, ...), how much of it is used (0–1 of the
// window, or the flat count some info types give), and when it resets.
// Kept identical in shape to provider.ClaudeLimit.
type ClaudeLimit struct {
	Kind     string
	Used     float64
	ResetsAt int64
}

var config = &Config{
	Models:           map[string]string{"claude-tui": "Claude TUI"},
	NoteLimits:       func(string, []ClaudeLimit) {},
	NoteSignInFailed: func(string, string, string) {},
	MovedOff:         func(string) bool { return false },
	LoginVersion:     func(string) string { return "" },
}

// Init installs cfg's non-nil fields. Called once by cmd before serving.
func Init(cfg *Config) {
	if cfg == nil {
		return
	}
	c := *config // copy defaults
	if cfg.StateDir != "" {
		c.StateDir = cfg.StateDir
	}
	if cfg.ClaudeConfigDir != "" {
		c.ClaudeConfigDir = cfg.ClaudeConfigDir
	}
	if cfg.BridgePath != "" {
		c.BridgePath = cfg.BridgePath
	}
	if cfg.HelperCommand != "" {
		c.HelperCommand = cfg.HelperCommand
	}
	if len(cfg.HelperArgv) > 0 {
		c.HelperArgv = slices.Clone(cfg.HelperArgv)
	}
	if cfg.CallbackPort != 0 {
		c.CallbackPort = cfg.CallbackPort
	}
	if len(cfg.Models) > 0 {
		c.Models = cfg.Models
	}
	if cfg.Proxy != "" {
		c.Proxy = cfg.Proxy
	}
	if cfg.SearchHandler != nil {
		c.SearchHandler = cfg.SearchHandler
	}
	if cfg.NoteLimits != nil {
		c.NoteLimits = cfg.NoteLimits
	}
	if cfg.NoteSignInFailed != nil {
		c.NoteSignInFailed = cfg.NoteSignInFailed
	}
	if cfg.MovedOff != nil {
		c.MovedOff = cfg.MovedOff
	}
	if cfg.LoginVersion != nil {
		c.LoginVersion = cfg.LoginVersion
	}
	if cfg.HandedOver != nil {
		c.HandedOver = cfg.HandedOver
	}
	config = &c
}

// stateDir resolves the runtime's private state directory, creating it.
// The interactive checkpoint file must never land in a shared config dir:
// a production Magpie on the same machine writes its own, and the two
// would race on the same sessions file. Production keeps it under
// appdir.Config(); the plugin keeps its own under StateDir.
func stateDir() (string, error) {
	d := config.StateDir
	if d == "" {
		// XDG_CONFIG_HOME is honored like production's appdir.Config() so
		// tests can sandbox state, but the folder is ours, not magpie's.
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
			d = filepath.Join(x, "magpie-tui-runtime")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			d = filepath.Join(home, ".config", "magpie-tui-runtime")
		}
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// mustStateDir is stateDir that can't fail: on the odd case of no home or
// an unwritable config folder, the checkpoint falls back to a temp dir so
// a turn is refused rather than replayed without one. Kept lossy on
// purpose: a checkpoint that doesn't persist means a turn won't resume,
// which is the fail-closed behavior the interactive path needs.
func mustStateDir() string {
	if d, err := stateDir(); err == nil {
		return d
	}
	d := filepath.Join(os.TempDir(), "magpie-tui-runtime-"+strconv.Itoa(os.Getuid()))
	_ = os.MkdirAll(d, 0o700)
	return d
}

// claudeConfigDir is Claude Code's config root: CLAUDE_CONFIG_DIR from the
// runtime's own env, else the configured override, else ~/.claude. This is
// where the native sign-in lives; the runtime never writes credentials.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	if config.ClaudeConfigDir != "" {
		return config.ClaudeConfigDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// SetCallbackPort is called by the cmd after the loopback listener binds.
// callbackBaseURL reads it; the runtime never picks its own port.
func SetCallbackPort(port int) {
	config.CallbackPort = port
}

// mcpHelperCommand resolves the packaged MCP helper invocation. The production
// gateway re-enters its own executable as "claude-mcp-helper"; the plugin
// runtime ships a dedicated binary. It may also be the runtime binary
// itself re-invoked with the claude-mcp-helper subcommand.
func mcpHelperCommand() (string, []string, error) {
	if len(config.HelperArgv) > 0 {
		return config.HelperArgv[0], config.HelperArgv[1:], nil
	}
	if config.HelperCommand != "" {
		fields := strings.Fields(config.HelperCommand)
		if len(fields) == 0 {
			return "", nil, errors.New("helper command is empty")
		}
		return fields[0], fields[1:], nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	return exe, []string{"claude-mcp-helper"}, nil
}

// claudeProxy reads the proxy choice a run's context carries, else the
// runtime's configured one. The production chain asks netproxy.Choice(ctx)
// first (per-account override); the standalone runtime has no per-account
// proxy table, so ctx carries none — kept here as a single named knob.
func claudeProxy(ctx context.Context) string {
	if v, ok := ctx.Value(ctxProxy{}).(string); ok && v != "" {
		return v
	}
	return config.Proxy
}

type ctxProxy struct{}

// withProxy tags ctx with a proxy override for Claude Code's env, the
// extracted equivalent of provider.Via(ctx) + netproxy.Choice(ctx).
func withProxy(ctx context.Context, choice string) context.Context {
	return context.WithValue(ctx, ctxProxy{}, choice)
}
