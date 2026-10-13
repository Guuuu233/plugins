// magpie-tui-runtime is the extracted Claude TUI subscription runtime the
// plugin starts: an ordinary-user process that holds the loopback HTTP
// surface the plugin's bridge-auth-v1 calls, and owns the live Claude runs
// the PTY wrapper drives.
//
// Contract (magpie-tui-runtime-contract/v1, src/runtime-contract.mjs):
//
//	magpie-tui-runtime --serve \
//	    --port-file <path> --key-file <path> --models-file <path> \
//	    [--state-dir <dir>] [--bridge <claude-bridge binary>] \
//	    [--helper <magpie-tui-mcp-helper>] [--proxy <url|direct>]
//
// Writes a connection key to --key-file (0600), binds 127.0.0.1:0 and
// writes the port to --port-file, writes its advertised models to
// --models-file, then serves until SIGTERM/SIGINT. Any missing or
// wrong-signed call is refused; the runtime never replays a caller's
// history for them. Credentials stay with the native Claude CLI.
//
// The key is fresh random per start by default (contract/v1). The
// optional --reuse-key flag is for supervised production restarts: when
// --key-file already exists it must be a non-symlink regular file, mode
// 0600 or stricter, containing exactly 64 hex bytes, and its exact bytes
// are reused as the session secret; a missing file is still generated
// fresh and stored atomically 0600. Malformed or insecure existing files
// are refused, never silently rotated.
//
// The same binary is its own MCP helper when invoked as
// `magpie-tui-runtime claude-mcp-helper <callback> <tools-file>`, or the
// packaged magpie-tui-mcp-helper binary can be named with --helper.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"magpie-tui-runtime/internal/bridge"
	"magpie-tui-runtime/internal/claudebridge"
)

const contract = "magpie-tui-runtime-contract/v1"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "claude-mcp-helper" {
		// The vendored helper speaks stdio MCP on behalf of the runtime's
		// HTTP callback: it never touches the plugin's auth key itself.
		if err := claudebridge.RunMCP(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "magpie-tui-runtime:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("magpie-tui-runtime", flag.ContinueOnError)
	var (
		serve        = fs.Bool("serve", false, "run the HTTP service")
		portFile     = fs.String("port-file", "", "write the bound port here")
		keyFile      = fs.String("key-file", "", "write the connection key here")
		modelsFile   = fs.String("models-file", "", "write the advertised models JSON here")
		stateDir     = fs.String("state-dir", "", "runtime state directory (default ~/.config/magpie-tui-runtime)")
		bridgePath   = fs.String("bridge", "", "claude-bridge PTY wrapper entrypoint (required for TUI runs)")
		helperBin    = fs.String("helper", "", "standalone MCP helper binary (default: this binary's claude-mcp-helper)")
		proxy        = fs.String("proxy", "", "proxy URL or 'direct' for Claude Code env")
		reuseKey     = fs.Bool("reuse-key", false, "reuse an existing --key-file (validated 0600 regular file, 64 hex bytes) instead of always generating fresh")
		claudeCfgDir = fs.String("claude-config-dir", "", "CLAUDE_CONFIG_DIR override for the native sign-in")
		modelsFlag   = fs.String("models", "", "comma-separated model ids to advertise (default: claude-tui)")
	)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*serve {
		return errors.New("expected --serve; run with --help for flags")
	}
	for _, f := range []string{*portFile, *keyFile, *modelsFile} {
		if strings.TrimSpace(f) == "" {
			return errors.New("--serve requires --port-file, --key-file and --models-file")
		}
	}

	// Connection key: fresh random per start (contract/v1), or a validated
	// existing key-file when --reuse-key was passed for supervised
	// restarts. The plugin hands the value to its auth loader; the runtime
	// never logs it.
	keyHex, err := loadOrGenerateKey(*keyFile, *reuseKey)
	if err != nil {
		return err
	}

	models := map[string]string{}
	for _, id := range strings.Split(*modelsFlag, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		models[id] = id
	}
	if len(models) == 0 {
		models["claude-tui"] = "Claude TUI"
	}
	// helper invocation as explicit argv: the executable path may contain
	// spaces (macOS install dirs like "~/my magpie tui"), which a joined
	// "path arg" string cannot carry through mcpHelperCommand's Fields().
	var helperArgv []string
	if *helperBin != "" {
		helperArgv = []string{*helperBin}
	} else if exe, err := os.Executable(); err == nil {
		helperArgv = []string{exe, "claude-mcp-helper"}
	}

	bridge.Init(&bridge.Config{
		StateDir:        *stateDir,
		ClaudeConfigDir: *claudeCfgDir,
		BridgePath:      *bridgePath,
		HelperArgv:      helperArgv,
		Proxy:           *proxy,
		Models:          models,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind loopback: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	// Tell the callback URL builder where the runtime actually bound.
	bridge.SetCallbackPort(port)

	rt := bridge.NewRuntime([]byte(keyHex))
	srv := &http.Server{
		Handler:           rt,
		ReadHeaderTimeout: 10 * time.Second,
		// tool calls block while the caller runs them; no global idle
		// timeout, matching production's mux behavior.
	}

	// Handshake files: write the key first so a stale port never matches a
	// fresh key, then models, then the port last so a poller that sees the
	// port knows all three are committed.
	if err := writeFile600(*keyFile, []byte(keyHex)); err != nil {
		return err
	}
	modelsJSON, err := json.Marshal(map[string]any{
		"contract": contract,
		"models":   modelsJSONMap(models),
	})
	if err != nil {
		return err
	}
	if err := writeFile600(*modelsFile, modelsJSON); err != nil {
		return err
	}
	if err := writeFile600(*portFile, []byte(fmt.Sprintf("%d", port))); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		rt.Shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func writeFile600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadOrGenerateKey resolves the hex connection key the runtime serves
// with. Default (reuse=false) is the contract/v1 behavior: always fresh
// random. With reuse=true an existing key-file is validated and its exact
// bytes reused; a missing file is generated and stored atomically 0600;
// anything malformed or insecure is an error rather than a silent rotate.
func loadOrGenerateKey(path string, reuse bool) (string, error) {
	if reuse {
		return reuseOrGenerateKey(path)
	}
	return generateKey()
}

func generateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

func reuseOrGenerateKey(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		// First supervised launch: mint and store atomically 0600 so the
		// next restart reuses the same bytes.
		keyHex, err := generateKey()
		if err != nil {
			return "", err
		}
		if err := writeFile600(path, []byte(keyHex)); err != nil {
			return "", fmt.Errorf("store generated key file: %w", err)
		}
		return keyHex, nil
	}
	if err != nil {
		return "", fmt.Errorf("key file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("key file %s is a symlink; refusing to reuse", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("key file %s is not a regular file; refusing to reuse", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("key file %s mode %04o is not private (must be 0600 or stricter); refusing to reuse", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read key file: %w", err)
	}
	keyHex := string(data)
	if len(keyHex) != 64 {
		return "", fmt.Errorf("key file %s holds %d bytes, expected exactly 64 hex chars; refusing to reuse", path, len(keyHex))
	}
	if _, err := hex.DecodeString(keyHex); err != nil {
		return "", fmt.Errorf("key file %s is not valid hex; refusing to reuse", path)
	}
	return keyHex, nil
}

func modelsJSONMap(models map[string]string) map[string]any {
	out := make(map[string]any, len(models))
	for id, name := range models {
		out[id] = map[string]any{"name": name}
	}
	return out
}
