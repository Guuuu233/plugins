#!/usr/bin/env node
// Ordinary-user dependency check for the Magpie Claude TUI plugin.
// Prints PASS/FAIL lines and exits non-zero when a required dependency is
// missing. Never touches credentials, native Claude auth files, system
// services or any root-owned resource.
import { spawnSync } from "node:child_process";
import { existsSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const failures = [];
const ok = (name, detail) => console.log(`PASS ${name}${detail ? ` — ${detail}` : ""}`);
const fail = (name, detail) => { failures.push(name); console.log(`FAIL ${name} — ${detail}`); };

function probe(name, command, args, pattern) {
  const result = spawnSync(command, args, { encoding: "utf8", timeout: 10000 });
  if (result.error || result.status !== 0) {
    fail(name, `not found on PATH (${command})`);
    return null;
  }
  const version = (result.stdout || result.stderr || "").trim().split("\n")[0];
  if (pattern && !pattern.test(version)) {
    fail(name, `unexpected version: ${version}`);
    return null;
  }
  ok(name, version);
  return version;
}

// The helper scripts run under the package host's JS runtime. Magpie's own
// plugin host is Bun, and `process.versions.node` under Bun reports its Node
// compatibility floor (e.g. 26.x) — not an installed Node. Report the real
// engine so a Bun-only VPS is not told it needs Node it does not have.
const isBun = typeof globalThis.Bun !== "undefined" || !!process.versions.bun;
if (isBun) {
  ok("js-host", `bun v${process.versions.bun} (plugin host runtime; Bun implements the node: APIs used)`);
} else {
  const major = Number(process.versions.node.split(".")[0]);
  if (major >= 18) ok("js-host", `node v${process.versions.node} (host requirement >=18)`);
  else fail("js-host", `node v${process.versions.node} is too old, need >=18`);
}

// The TUI runtime drives the existing Claude CLI inside tmux, matching the
// deployed -p-to-Wrapper architecture. Both are required for the full chain;
// neither is invoked here.
probe("tmux", "tmux", ["-V"], /^tmux \d/);
const claude = spawnSync("claude", ["--version"], { encoding: "utf8", timeout: 10000 });
if (claude.error || claude.status !== 0) {
  fail("claude", "Claude CLI not found on PATH (runtime drives the installed native CLI)");
} else {
  ok("claude", (claude.stdout || "").trim().split("\n")[0]);
}

// The runtime is usable either as a prebuilt binary (binary archives ship
// bin/magpie-tui-runtime) or as package Go sources that build one. Report
// which actually exists — never claim a missing source tree is present.
const rtBin = join(root, "bin/magpie-tui-runtime");
const rtHelperBin = join(root, "bin/magpie-tui-mcp-helper");
const helperSource = join(root, "runtime/internal/claudebridge/mcp.go");
const helperMain = join(root, "runtime/cmd/magpie-tui-mcp-helper/main.go");
const runtimeEntry = join(root, "runtime/cmd/magpie-tui-runtime/main.go");
const statIsExecutable = p => { try { return statSync(p).isFile() && (statSync(p).mode & 0o111) !== 0; } catch { return false; } };
const hasRuntimeBin = statIsExecutable(rtBin);
const hasRuntimeSrc = existsSync(runtimeEntry);
const hasHelperBin = statIsExecutable(rtHelperBin);
const hasHelperSrc = existsSync(helperSource) && existsSync(helperMain);
hasHelperBin || hasHelperSrc
  ? ok("runtime-mcp-helper", hasHelperBin ? "prebuilt binary present (bin/magpie-tui-mcp-helper)" : "vendored source present")
  : fail("runtime-mcp-helper", "no packaged MCP helper: bin/magpie-tui-mcp-helper and runtime/{internal/claudebridge,cmd} both missing");
hasRuntimeBin || hasRuntimeSrc
  ? ok("runtime-entry", hasRuntimeBin ? "prebuilt binary present (bin/magpie-tui-runtime)" : "extracted runtime entry present")
  : fail("runtime-entry", "no packaged runtime: bin/magpie-tui-runtime and runtime/cmd/magpie-tui-runtime both missing");

// Vendored Wrapper (claude-bridge-magpie) is shipped in the package; its
// only non-node/bun dep is zod, pinned in root package.json dependencies and
// packed via bundleDependencies, so the Wrapper resolves it through the
// package's own node_modules after install. bun must run the .ts entry.
const bridgeCli = join(root, "vendor/claude-bridge-magpie/src/cli.ts");
const zodPkg = join(root, "node_modules/zod/package.json");
existsSync(bridgeCli)
  ? ok("bridge-wrapper", "vendored claude-bridge-magpie cli.ts present")
  : fail("bridge-wrapper", "vendor/claude-bridge-magpie/src/cli.ts missing from package");
existsSync(zodPkg)
  ? ok("bridge-dep-zod", "bundled zod dependency present (root node_modules)")
  : fail("bridge-dep-zod", "bundled zod missing — package root node_modules/zod absent");
probe("bun", "bun", ["--version"], /^\d+\.\d+/);

if (failures.length) {
  console.error(`\n${failures.length} dependency check(s) failed: ${failures.join(", ")}`);
  process.exit(1);
}
console.log("\nAll dependencies satisfied.");
