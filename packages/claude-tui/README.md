# @magpie-community/opencode-claude-tui-auth

Uses an existing signed-in [Claude Code](https://claude.com/claude-code)
subscription through Claude's interactive TUI in OpenCode and magpie. Provider
id: `claude-tui`.

## Requirements

- macOS or Linux on arm64 or x86_64
- Bun, `tmux`, and the native `claude` command on `PATH`
- a dedicated Claude configuration directory under the user's home, already
  signed in with `claude`

Native Windows is not supported. Windows users can run Magpie and this plugin
inside an existing WSL2 Linux distribution. A Windows or Mac used only as a
remote Magpie client does not install the plugin locally.

The package includes the runtime and helper binaries for the four supported
OS/architecture combinations. It does not install global software, run an
install script, or include Claude Code itself.

## Install and connect

In Magpie, install **Claude TUI** from Plugins, then connect it in Providers.
From the command line:

```sh
magpie plugin add @magpie-community/opencode-claude-tui-auth
magpie plugin login claude-tui
```

The connection form asks for:

1. the absolute path to the dedicated Claude configuration directory; and
2. the real Claude model ids to expose, separated by commas.

The plugin checks that Bun, tmux and Claude are available. It stores Magpie's
opaque local connection record in `plugin-auth.json`; Claude's own login stays
in the selected Claude directory and is read by Claude itself. The plugin does
not copy, refresh or replace Claude's OAuth credentials.

## How requests run

Each connected account gets a private local runtime and state directory. The
runtime drives the original interactive Claude TUI inside tmux, preserving
streaming, tool continuation, cancellation, account separation and stable
conversation identity. Requests with uncertain history are refused instead of
replaying tools. A narrowly scoped read-only recovery can reconnect a known
dirty conversation without repeating its failed tool turn.

Images in the current user message are supported. Historical image replay and
native Windows PTY execution are not supported.

## Platform evidence

- Linux x86_64: real Claude CLI deployment exercised on a VPS.
- macOS arm64: packaged runtime exercised with an isolated mock Claude.
- Linux arm64 and macOS x86_64: cross-built package binaries; no native host
  execution evidence yet.
- Windows: WSL2 Linux only; native Windows is refused.

See [COMPATIBILITY.md](COMPATIBILITY.md) for the detailed evidence and limits.

## Archived community PR status and binary provenance

This branch was used for the **declined and closed** community PR #107; it is not an official distribution. The source here now includes the 30-minute MCP tool timeout fix, but the checked-in prebuilt Runtime files are **older** and are not part of this fix. For the complete updated source-and-binary release, see the separate independent [`v0.1.1`](https://github.com/Guuuu233/magpie-claude-tui-plugin/releases/tag/v0.1.1). Do not install the archived branch expecting its bundled Runtime binaries to include the fix.
