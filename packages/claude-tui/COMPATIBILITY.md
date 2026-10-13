# Compatibility evidence

Verified target host:
`work/auth-status-production-20261010/internal/plugin/host.js`

Host functions verified directly:

- Default v1 `{ id, server }` module loading.
- Per-package options passed as the server function's second argument.
- `config`, `chat.headers`, account-scoped `auth.loader/getAuth` and custom fetch.
- HTTP response head/chunks/terminal result over the real host RPC.

Actual-host test covers loading, provider listing, body preservation, signed
local messages, SSE, same-session tool continuation, separate account scope,
and refusal when the host supplies an ephemeral conversation id.

The HTTP runtime in this test is synthetic. This does not prove actual
Claude/Wrapper/MCP behavior inside the extracted runtime, provider installation
through the full GUI/CLI, or compatibility with every Magpie/OpenCode version.

The source snapshot is the specific target, not a claim of the newest released
Magpie API. Original source SHA256 is recorded in evidence/source-manifest.json.

Node test suite: request byte preservation, stable session/account separation,
untrusted session rejection, single-attempt behavior, stream failure, abort,
signout and endpoint/redirect checks.

End-to-end acceptance suite (DAV-1822) verified real wrapper integration
under tmux with mock Claude CLI, confirming 48/48 E2E assertions and 32/32
packaging assertions.

Runtime extraction retains the real production adapter's state matching
and MCP parked-run semantics. Gate4d/4i synthetic prompts/models remain
test fixtures and cannot become a production fallback.

## Platform and Environment Prerequisites

- **Tested platforms (actual evidence)**:
  - **Linux x86_64**: the frozen `0.1.0-dev.2` linux-amd64 archive
    (`releases/linux-amd64-dev2/`) was installed and is serving on the
    authorized VPS deployment (`VPS-DEPLOYMENT-REPORT-20261011`), and the
    dev.1 archive previously passed DAV-1834 independent acceptance on an
    ordinary-user VPS sandbox (49 pass / 0 fail / 1 skip, Bun-only, no
    Node/npm/Go needed).
  - **VPS R5 runtime (the runtime source shipped in dev.5 archives)**: the
    R5 snapshot — frozen in
    `evidence/distributable-dev5-r5-runtime-baseline-20261012.json` (R4
    manifest retained as history) — was deployed on the authorized VPS and
    verified with real Claude CLI 2.1.287: R4 recovery semantics
    (recovery first-round tool-less merge, bounded wait for PTY cleanup,
    duplicate-request refusal, consecutive real gateway requests HTTP 200)
    plus the R5 image-bootstrap fix (private Linux real Claude image
    viewing/colour + immediate same-inner text PASS, gateway image + text
    PASS; 75 original checkpoint entries preserved). Evidence:
    `image-bootstrap-r5-native-receipt-20261012.json`,
    `image-bootstrap-r5-gateway-receipt-20261012.json`,
    `VPS-409-R4-DEPLOYMENT-REPORT-20261012`. The original 1500+-line
    Desktop session and all rewind/compaction combinations were NOT
    retested; no claim that every 409 is gone.
  - **macOS arm64 (Apple Silicon)**: build host platform; runtime/helper
    build and execute natively, and the `scripts/macos/magpie-tui`
    install/lifecycle CLI passed isolated mock tests (fake deps/PATH,
    private HOME/state, spaced prefix). The packaged dev.5 runtime carries
    the R5 fixes but on macOS is verified **mock-only** (fake Claude):
    real Magpie GUI/CLI registration, real native Claude OAuth and
    LaunchAgent activation on this machine are **untested** — independent
    QA pending.
  - **macOS amd64 (Intel)**: cross-built only — the archive exists but has
    never executed on a real Intel Mac (never executed on the arm64 build
    host either — no Rosetta). Build-only, not verified.
- **Unsupported**: Windows natively (the interactive path intentionally
  refuses WSL execution and Windows PTY handling differs). A separate
  Windows **WSL2 entry + Linux payload** package exists — see
  `INSTALL.windows.md` (owned by the Windows WSL worker; requires an
  existing WSL2 distro with Bun/tmux/native Linux Claude/Magpie; the
  native Windows launcher path itself is unverified). **Remote
  clients**: a Windows/Mac machine used only as a Magpie *client* does not
  install this plugin locally — it connects to a Magpie server that
  already runs the plugin (e.g. the Linux amd64 archive on a VPS). Local
  native execution is only for running Magpie itself on macOS/Linux.
- **Go Toolchain**: needed only on the build host to compile
  `bin/magpie-tui-runtime` / `bin/magpie-tui-mcp-helper` from
  `runtime/` sources (`go.mod` declares `go 1.27`). Prebuilt archives
  (linux-amd64, darwin-arm64, darwin-amd64) need **no Go/Node/npm** on the
  install machine.
- **Bun**: required — the vendored `vendor/claude-bridge-magpie/src/cli.ts`
  shebang is `#!/usr/bin/env bun`; the runtime's `--bridge` entry must be an
  executable with `bun` on PATH. Node is not a substitute. Bun alone is
  sufficient for all shipped `scripts/*.mjs` helpers (`check-deps.mjs`
  reports `js-host: bun` honestly rather than claiming Node). A standalone
  Node >= 18 can run them too, but is **not** required when Bun is present.
- **Wrapper (shipped in this package)**: `vendor/claude-bridge-magpie/` is the
  Magpie-adapted `claude-bridge` the runtime is verified against — **not**
  interchangeable with upstream npm `@desplega.ai/claude-bridge@0.2.2` (which
  lacks `CLAUDE_BRIDGE_USER_CONTEXT_PATH` / workdir-hook behavior; see
  `vendor/claude-bridge-magpie/ADAPTATIONS.md`). No user-side install of the
  Wrapper is needed; `--bridge` points at the packaged entry, and its zod
  dependency is bundled under the package's own `node_modules/`.
- **Target Host Compatibility**: verified against deployed Magpie host
  snapshot `auth-status-production-20261010/internal/plugin/host.js`. Does
  not claim untested newer/arbitrary Magpie releases.

Third-party components redistributed or driven by this package:

- Magpie (upstream, MIT): `runtime/internal/claudebridge/mcp.go` and bridge modules
  under `runtime/internal/bridge/*` are extracted and adapted from deployed source;
  its MIT notice lives in `runtime/LICENSE.magpie`.
- claude-bridge-magpie (upstream `@desplega.ai/claude-bridge` 0.2.2 base,
  Magpie-adapted, MIT): the Wrapper the runtime drives to reach the tmux
  Claude TUI, vendored at `vendor/claude-bridge-magpie/` with the upstream
  MIT `LICENSE` retained verbatim and per-file hashes in `SHA256SUMS.txt`.
  Its only non-`node:`/`bun:` dependency, zod 4.4.3 (MIT,
  `node_modules/zod/LICENSE`), is bundled under this package's `node_modules/`
  via root `bundleDependencies`.
- Claude CLI (proprietary, Anthropic): invoked by the runtime as the
  ordinary user; never packaged, read or modified by the plugin.
