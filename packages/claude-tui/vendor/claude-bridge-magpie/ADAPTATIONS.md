# claude-bridge-magpie — vendored Wrapper

This directory ships the **Magpie-adapted** `@desplega.ai/claude-bridge`
Wrapper the MAGPIE-TUI runtime actually verifies against. It is *not* the
upstream npm `@desplega.ai/claude-bridge@0.2.2` — that release lacks the
`CLAUDE_BRIDGE_USER_CONTEXT_PATH` and workdir `settings.local.json` hook
behavior the runtime depends on (see "adaptations" below). Do not replace
this copy with upstream 0.2.2; they are not interchangeable.

## Provenance

- Upstream project: <https://github.com/desplega-ai/claude-bridge>
- Upstream version base: `0.2.2` (`package.json` retained verbatim).
- Upstream licence: MIT — `LICENSE` is the verbatim upstream text fetched
  from `https://raw.githubusercontent.com/desplega-ai/claude-bridge/master/LICENSE`
  (Copyright (c) 2026 desplega.sh).
- The vendored sources other than `src/transcript.ts` are byte-identical to
  the reviewed snapshot
  `phase30/reference/wrapper` and to the copy exercised in the Linux
  acceptance run DAV-1826 (`/home/magpie/.local/share/claude-bridge-magpie`).
  `cli.ts` SHA256 is `51cfa7f822efcb78360ae6293e497d07490f94fc0698f0d5e51e96d40ca19c48`.
- `src/transcript.ts` carries a post-vendoring **plugin patch** (2026-10-12,
  DAV-1970): `claudeProjectsRoot`/`claudeProjectFolder` honor
  `CLAUDE_CONFIG_DIR` (`<config>/projects`) when no explicit home is passed,
  so supervised custom native profiles find their live transcripts. Its hash
  therefore differs from the snapshot; `SHA256SUMS.txt` reflects the patched
  file.
- `SHA256SUMS.txt` records the per-file hashes at vendoring time.

## Adaptations vs upstream 0.2.2

- `CLAUDE_BRIDGE_USER_CONTEXT_PATH` user-context hook injection
  (`src/user-context.ts`, `installUserContextHooks` in `hook-install.ts`).
- Runtime-hook plumbing: `CLAUDE_BRIDGE_RUNTIME_HOOK` /
  `CLAUDE_BRIDGE_RUN_DIR` handling in `cli.ts` + `stop-hook.ts`, installed
  into the per-run workdir `settings.local.json` (not the global
  `~/.claude/settings.json`).
- Miscellaneous Magpie-only fixes carried in `cli.ts` (see snapshot history
  in the source checkout; `.bak*` files were intentionally not vendored).
- `src/transcript.ts` CLAUDE_CONFIG_DIR profile root (plugin patch, not part
  of the byte-identical snapshot): when the supervisors select a dedicated
  `--native-config-dir` profile, transcript discovery follows
  `$CLAUDE_CONFIG_DIR/projects` instead of always `$HOME/.claude/projects`.
  An explicit `home` argument (test override) still wins; behavior is
  unchanged when `CLAUDE_CONFIG_DIR` is unset.

## What was vendored

Only the 15 source files in `cli.ts`'s real import closure plus
`package.json`, `LICENSE`, this note and `SHA256SUMS.txt`:

```
src/args.ts  src/auth-env.ts  src/claude-compat.ts  src/cli.ts
src/hook-install.ts  src/json-schema.ts  src/launch-command.ts
src/model-pricing-data.ts  src/model-pricing.ts  src/preaccept.ts
src/print-result.ts  src/stop-hook.ts  src/transcript.ts
src/user-context.ts  src/view.ts
```

`src/bridge.ts` and `src/mcp-channel.ts` (upstream's standalone MCP channel
entry, which needs `@modelcontextprotocol/sdk`) are **not** in `cli.ts`'s
closure and are intentionally omitted. All `test-*.ts` and `*.bak*` files
are omitted — they are development artifacts, not needed to run.

## Dependency closure

`cli.ts`'s only non-`node:`/`bun` import is `zod`. This package ships
`node_modules/zod` (zod 4.4.3, MIT — license at `node_modules/zod/LICENSE`)
so the Wrapper runs from a clean install without a registry fetch. The
runtime remains an external prerequisite: the `cli.ts` shebang is
`#!/usr/bin/env bun`, so `bun` must be on `PATH` (Bun ≥ 1.1 per
`package.json` engines; verified with 1.4.2).

## Usage

Point the runtime's `--bridge` at the packaged entry:

```sh
magpie-tui-runtime --serve ... \
  --bridge "<install>/node_modules/magpie-claude-tui/vendor/claude-bridge-magpie/src/cli.ts"
```

`cli.ts` is mode 0755 with a `#!/usr/bin/env bun` shebang, so the runtime's
`exec.LookPath` + regular-executable check accepts it directly. No extra
shim is required, but a thin wrapper script that `exec bun <path>/cli.ts`
also works.
