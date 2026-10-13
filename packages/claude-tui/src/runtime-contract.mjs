// Minimal plugin <-> runtime contract, v1.
//
// This file is the interface boundary negotiated with the runtime extraction
// task (DAV-1820). The plugin owns calling it; the runtime owns implementing
// it. Everything here is ordinary-user, loopback-only, no root services.
//
// Contract summary
// ----------------
// The extracted runtime is a single ordinary-user process exposing the
// existing Anthropic-messages-compatible bridge on loopback:
//
//   POST http://127.0.0.1:<port>/v1/messages
//     signed with the existing "bridge-auth-v1" headers the plugin already
//     emits (x-bridge-timestamp / nonce / session / mac over the body SHA-256).
//
// Startup handshake (implemented by scripts/magpie-tui-runtime-setup.mjs):
//
//   <runtime entry> --serve \
//       --port-file <path> --key-file <path> --models-file <path> \
//       [--state-dir <path>]
//
//   * --key-file   runtime writes a fresh random connection key (0600) that
//                  the user pastes via `magpie plugin login claude-tui`.
//                  The plugin never reads or stores it itself.
//   * --port-file  runtime binds 127.0.0.1:0 and writes the chosen port.
//   * --models-file runtime writes JSON {"models": {"<id>": {"name": ...}}}
//                  describing the TUI model ids it actually serves. The
//                  plugin refuses to advertise models the runtime did not
//                  report; there is no fabricated fallback model.
//   * exit contract: SIGTERM/SIGINT -> stop sessions, cancel parked MCP runs,
//                  remove ephemeral files, exit 0. A non-zero exit or missing
//                  files means "runtime unavailable", never retry silently.
//
// Until the runtime lands, checkDependencies() reports it as a named missing
// dependency and setup refuses to claim readiness.

import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

export const CONTRACT_VERSION = "magpie-tui-runtime-contract/v1";

const MODEL_ID = /^[A-Za-z0-9_.:/-]{1,128}$/;
const BAD_KEYS = new Set(["__proto__", "constructor", "prototype"]);

// Validates the JSON a runtime writes to --models-file. Returns a plain
// { id: model } object safe to hand to createHooks() as config.models.
export function parseRuntimeModels(json) {
  let parsed;
  try {
    parsed = JSON.parse(json);
  } catch {
    throw new Error("runtime models file is not valid JSON");
  }
  const models = parsed?.models;
  if (!models || typeof models !== "object" || Array.isArray(models) ||
      Object.keys(models).length === 0) {
    throw new Error("runtime reported no models; plugin cannot configure itself");
  }
  const out = {};
  for (const [id, model] of Object.entries(models)) {
    if (!MODEL_ID.test(id) || BAD_KEYS.has(id) || !model || typeof model !== "object" ||
        Array.isArray(model) || typeof model.name !== "string" || !model.name) {
      throw new Error(`runtime reported invalid model entry: ${id}`);
    }
    out[id] = structuredClone(model);
  }
  return out;
}

// Runs the runtime start handshake against an executable entry point and
// returns { url, models, stop }. Dry-run friendly: the caller owns the
// spawned process via stop().
export async function startRuntime(entry, { args = [], cwd, timeoutMs = 15000, spawnImpl = spawn } = {}) {
  const dir = await mkdtemp(join(tmpdir(), "magpie-tui-runtime-"));
  const portFile = join(dir, "port");
  const keyFile = join(dir, "key");
  const modelsFile = join(dir, "models.json");
  const child = spawnImpl(entry, [
    ...args, "--serve",
    "--port-file", portFile,
    "--key-file", keyFile,
    "--models-file", modelsFile,
  ], { stdio: ["ignore", "inherit", "inherit"], ...(cwd ? { cwd } : {}) });
  const deadline = Date.now() + timeoutMs;
  let handshakeDone = false;
  const exited = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", code => reject(new Error(`runtime exited before handshake (code ${code})`)));
  });
  try {
    while (Date.now() < deadline) {
      try {
        const [portRaw, modelsRaw] = await Promise.race([
          Promise.all([readFile(portFile, "utf8"), readFile(modelsFile, "utf8")]),
          exited,
        ]);
        const port = Number(portRaw.trim());
        if (!Number.isInteger(port) || port < 1 || port > 65535) {
          throw new Error("runtime wrote an invalid port");
        }
        handshakeDone = true;
        return {
          url: `http://127.0.0.1:${port}/v1/`,
          models: parseRuntimeModels(modelsRaw),
          stop: () => { child.kill("SIGTERM"); },
        };
      } catch (error) {
        if (error?.code !== "ENOENT") throw error;
        await new Promise(resolve => setTimeout(resolve, 50));
      }
    }
    throw new Error("runtime did not complete the start handshake in time");
  } finally {
    exited.catch(() => {});
    // On success the caller owns the process via stop(); on any failure or
    // timeout the child must not be orphaned holding our stdio pipes.
    if (child.exitCode === null && !child.killed && !handshakeDone) {
      child.kill("SIGKILL");
      await new Promise(resolve => child.once("exit", resolve));
    }
    if (child.exitCode !== null || child.killed || !handshakeDone) {
      await rm(dir, { recursive: true, force: true });
    }
  }
}
