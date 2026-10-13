#!/usr/bin/env node
// Configures the Magpie plugin against an ALREADY-RUNNING magpie-tui-runtime.
//
//   node scripts/magpie-tui-runtime-setup.mjs [--state-dir <dir>]
//
// Workflow (ordinary user, nothing here needs root):
//
//   1. Start the runtime once and keep it running in your own session:
//        go run ./cmd/magpie-tui-runtime --serve \
//          --port-file  ~/.config/magpie-tui-runtime/port \
//          --key-file   ~/.config/magpie-tui-runtime/key \
//          --models-file ~/.config/magpie-tui-runtime/models.json \
//          [--state-dir ~/.config/magpie-tui-runtime] [--bridge <claude-bridge>]
//      (or build once: go build -o ~/bin/magpie-tui-runtime ./cmd/magpie-tui-runtime
//       and run that binary; both follow contract/v1 in src/runtime-contract.mjs)
//   2. Run this script. It reads the LIVE runtime's port + models descriptor
//      files and prints the `magpie plugin options` JSON for them — it never
//      spawns or kills the runtime, and never prints a dead port.
//   3. `magpie plugin options <installed-name> '<json>'`
//   4. `magpie plugin login claude-tui` and paste the connection key from the
//      runtime's own --key-file (0600, private). This script never reads,
//      prints or stores it.
//   5. Stop: SIGTERM the runtime process (it cleans sessions per contract).
//      Uninstall: `magpie plugin rm <name>` then remove the install dir and
//      the state dir.
//
// Exits 2 with a diagnostic when no live runtime descriptor is found, when the
// port does not answer, or when the models file is invalid — never prints a
// configuration that cannot connect.
import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { parseRuntimeModels, CONTRACT_VERSION } from "../src/runtime-contract.mjs";

const args = process.argv.slice(2);
const dirIdx = args.indexOf("--state-dir");
const stateDir = dirIdx >= 0
  ? args[dirIdx + 1]
  : join(process.env.XDG_CONFIG_HOME || join(homedir(), ".config"), "magpie-tui-runtime");

function refuse(message, hint) {
  console.error(`runtime setup: ${message}`);
  if (hint) console.error(hint);
  console.error("No configuration was written or printed.");
  process.exit(2);
}

const portFile = join(stateDir, "port");
const modelsFile = join(stateDir, "models.json");
const keyFile = join(stateDir, "key");

if (!existsSync(portFile) || !existsSync(modelsFile)) {
  refuse(`no live runtime descriptor under ${stateDir}`,
    "Start magpie-tui-runtime --serve with --port-file/--key-file/--models-file\n" +
    "pointed at that directory first (see the header comment of this script).");
}

const port = Number(readFileSync(portFile, "utf8").trim());
if (!Number.isInteger(port) || port < 1 || port > 65535) {
  refuse(`runtime port file ${portFile} is invalid`);
}

let models;
try {
  models = parseRuntimeModels(readFileSync(modelsFile, "utf8"));
} catch (error) {
  refuse(`runtime models file ${modelsFile} rejected: ${error.message}`);
}

// The descriptor is only trustworthy if the port actually answers. Probe it
// once with an unsigned request — a live runtime must reject it (401/403),
// while a dead port or a foreign service must not produce options.
const url = `http://127.0.0.1:${port}/v1/messages`;
try {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 3000);
  // Unsigned probe: any response means something is listening; the runtime
  // contract requires it to refuse unsigned calls with 401 or 403.
  const response = await fetch(url, { method: "POST", signal: controller.signal });
  clearTimeout(timer);
  await response.body?.cancel();
  if (response.status !== 401 && response.status !== 403) {
    refuse(`port ${port} answered ${response.status}, not the signed-request refusal (401/403) expected from magpie-tui-runtime`,
      "Another service may own this port; refusing to emit a configuration for it.");
  }
} catch {
  refuse(`port ${port} does not answer — the runtime is not running`,
    "Start it first, then re-run this setup; a stopped port is never emitted.");
}

const options = { runtimeURL: `http://127.0.0.1:${port}/v1/`, models };
console.log(`contract: ${CONTRACT_VERSION}`);
console.log(`live runtime confirmed on 127.0.0.1:${port} (state-dir ${stateDir})`);
console.log("\nmagpie plugin options <installed-name> '" + JSON.stringify(options) + "'");
console.log("\nThen: magpie plugin login claude-tui");
console.log(`  # paste the connection key from ${keyFile}`);
console.log("  # (0600, written by the runtime itself; never printed here)");
console.log("\nStop:  SIGTERM the runtime process (it cleans sessions per contract).");
console.log("Remove: magpie plugin rm <name>, then delete the install dir and " + stateDir);
