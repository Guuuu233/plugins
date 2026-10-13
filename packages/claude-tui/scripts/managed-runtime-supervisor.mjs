// scripts/managed-runtime-supervisor.mjs — a tiny owned child of the plugin
// host that supervises exactly one magpie-tui-runtime process.
//
//   node scripts/managed-runtime-supervisor.mjs \
//     --runtime <entry> [--runtime-arg <arg>]... \
//     --state-dir <dir> --ready-file <path> [--managed-key <64-hex>]
//
// Contract:
//   * Spawns `<runtime> --serve --port-file <state>/port --key-file <state>/key
//     --models-file <state>/models.json --state-dir <state> <runtime-args…>`
//     as an explicit argv — never a shell string — with a scrubbed
//     environment (no ANTHROPIC_*/proxy/credential variables).
//   * <state-dir> ownership is proven by a .managed-owner marker bound to the
//     managed key (or to the runtime+path when none). A previously owned dir
//     resumes — persistent key and checkpoints are kept; a foreign or
//     conflicting dir is refused — never cleared, claimed or deleted.
//   * --managed-key makes <state>/key persistent: the runtime is launched
//     with --reuse-key against this exact 0600 file, and an existing
//     mismatching key is refused as conflicting owned state.
//   * Once the handshake files (key, models.json, port) exist, writes
//     --ready-file and holds the child.
//   * Its own stdin EOF (the plugin host process is gone or SIGKILLed) or
//     SIGTERM → SIGTERM the runtime, wait a bounded grace, then SIGKILL.
//   * Runtime exits before ready or after ready → the supervisor exits
//     accordingly; it never restarts, double-spawns or orphans.
//   * Prints nothing the runtime writes; the key file is never read, never
//     logged, never copied out of the state dir.
import { spawn } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { chmodSync, existsSync, lstatSync, mkdirSync, readFileSync, realpathSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { dirname, isAbsolute, join, resolve, sep } from "node:path";

const sha256 = v => createHash("sha256").update(v).digest("hex");

const argv = process.argv.slice(2);
const opt = name => {
  const i = argv.indexOf(name);
  return i >= 0 ? argv[i + 1] : null;
};
const runtime = opt("--runtime");
const stateDir = opt("--state-dir");
const readyFile = opt("--ready-file");
const managedKey = opt("--managed-key");
const runtimeArgs = argv.flatMap((v, i) => (v === "--runtime-arg" ? [argv[i + 1]] : []));

const die = message => {
  console.error(`managed-supervisor: ${message}`);
  process.exit(2);
};

function checkNoSymlinkComponents(path, finalKind = "any") {
  const target = resolve(path);
  let current = sep;
  for (const component of target.split(sep).filter(Boolean)) {
    current = join(current, component);
    let st;
    try { st = lstatSync(current); } catch (error) {
      if (error?.code === "ENOENT") continue;
      throw Object.assign(new Error(`cannot inspect ${current}: ${error.message}`), { code: "MANAGED_PATH_ERROR" });
    }
    if (st.isSymbolicLink()) throw Object.assign(new Error(`path contains symlink ${current}; refusing`), { code: "MANAGED_PATH_ERROR" });
    const final = current === target;
    if (!final && !st.isDirectory()) throw Object.assign(new Error(`path ancestor is not a directory: ${current}`), { code: "MANAGED_PATH_ERROR" });
    if (final && finalKind === "directory" && !st.isDirectory()) throw Object.assign(new Error(`path is not a directory: ${current}`), { code: "MANAGED_PATH_ERROR" });
    if (final && finalKind === "file" && !st.isFile()) throw Object.assign(new Error(`path is not a regular file: ${current}`), { code: "MANAGED_PATH_ERROR" });
  }
}

const assertNoSymlinkComponents = (path, finalKind = "any") => {
  try { checkNoSymlinkComponents(path, finalKind); }
  catch (error) { die(error.message); }
};

function writePrivateAtomic(path, content) {
  checkNoSymlinkComponents(path, "file");
  const temp = `${path}.tmp-${process.pid}-${randomBytes(8).toString("hex")}`;
  checkNoSymlinkComponents(temp, "file");
  try {
    writeFileSync(temp, content, { flag: "wx", mode: 0o600 });
    renameSync(temp, path);
  } catch (error) {
    try { rmSync(temp, { force: true }); } catch {}
    throw error;
  }
}

if (!runtime || !isAbsolute(runtime)) die("--runtime must be an absolute entry path");
if (!stateDir || !isAbsolute(stateDir)) die("--state-dir must be absolute");
if (!readyFile || !isAbsolute(readyFile)) die("--ready-file must be absolute");
if (runtimeArgs.some(a => a === "--serve" || a === "--port-file" || a === "--key-file" || a === "--models-file" || a === "--state-dir")) {
  die("runtime args must not override the owned handshake flags");
}
assertNoSymlinkComponents(dirname(resolve(readyFile)), "directory");
assertNoSymlinkComponents(readyFile, "file");
assertNoSymlinkComponents(stateDir, "directory");

// The state dir is ours alone. A previously owned dir may hold checkpoints
// and must resume; a foreign or conflicting dir is refused — never cleared,
// claimed or deleted.
const parent = dirname(resolve(stateDir));
if (!existsSync(parent)) die(`state-dir parent missing: ${parent}`);
const inside = (p, base) => {
  const r = resolve(base), t = resolve(p);
  return t === r || t.startsWith(r + sep);
};
// A state dir must stay under the ready-file's own directory (the
// per-account root the caller allocated); outside paths are not owned state.
if (!inside(stateDir, dirname(resolve(readyFile)))) die("state-dir escapes the supervisor root; refusing");

const ownerHash = sha256("managed-owner/v1\0" + (managedKey ?? (runtime + "\0" + resolve(stateDir))));
const marker = join(stateDir, ".managed-owner");
const liveFile = join(stateDir, ".managed-live");
for (const file of [marker, liveFile, join(stateDir, "key"), join(stateDir, "port"), join(stateDir, "models.json")]) {
  assertNoSymlinkComponents(file, "file");
}
// Realpath the ancestors too — a symlinked parent would put the real dir
// outside the ready-file root even though lstat on the leaf looks clean.
try {
  const realParent = realpathSync(parent);
  const realReadyParent = realpathSync(dirname(resolve(readyFile)));
  if (realParent !== realReadyParent && !realParent.startsWith(realReadyParent + sep)) {
    die(`state-dir realpath ${realParent} escapes the supervisor root ${realReadyParent}; refusing`);
  }
} catch (error) { if (error?.code !== "ENOENT") die(`ancestor realpath: ${error.message}`); }
try {
  const st = lstatSync(stateDir);
  if (st.isSymbolicLink()) die(`state-dir ${stateDir} is a symlink; refusing`);
  if (!st.isDirectory()) die(`state-dir ${stateDir} exists and is not a directory; refusing`);
  assertNoSymlinkComponents(marker, "file");
  let prior;
  try { prior = readFileSync(marker, "utf8").trim(); } catch {}
  if (prior !== ownerHash) die(`state-dir ${stateDir} holds foreign or conflicting state; refusing (checkpoint/dirty state is never cleared)`);
} catch (error) {
  if (error?.code !== "ENOENT") die(`state-dir: ${error.message}`);
  mkdirSync(stateDir, { recursive: true, mode: 0o700 });
  assertNoSymlinkComponents(stateDir, "directory");
  try { writePrivateAtomic(marker, ownerHash + "\n"); }
  catch (error) { die(`owner marker could not be written safely: ${error.message}`); }
}
try { chmodSync(stateDir, 0o700); }
catch (error) { die(`owned state-dir permissions could not be secured: ${error.message}`); }
// Exclusive live claim across host processes: a second supervisor on the
// same state must refuse, never double-spawn over the first's port/key or
// remove its ready marker. The claim is atomic (O_EXCL); a stale file is
// refused, not stolen. Released only when OUR runtime child exits.
const claimId = `${process.pid} ${Date.now()}`;
try {
  assertNoSymlinkComponents(liveFile, "file");
  writeFileSync(liveFile, claimId + "\n", { flag: "wx", mode: 0o600 });
} catch {
  let prior = "";
  try { prior = readFileSync(liveFile, "utf8").trim(); } catch {}
  die(`state-dir ${stateDir} already has a live supervisor claim (${prior || "unreadable"}); refusing to double-start`);
}
const release = () => { try { if (readFileSync(liveFile, "utf8").trim() === claimId) rmSync(liveFile, { force: true }); } catch {} };
process.once("exit", release);

const keyFile = join(stateDir, "key");
const portFile = join(stateDir, "port");
const modelsFile = join(stateDir, "models.json");
for (const file of [keyFile, portFile, modelsFile, readyFile]) assertNoSymlinkComponents(file, "file");

// Managed key: the plugin derives the connection key from the account's
// managed secret so <state>/key is persistent across supervised restarts.
// With --reuse-key the runtime validates this file (0600, 64 hex); an
// existing mismatching key means conflicting owned state — refuse.
if (managedKey) {
  if (!/^[a-f0-9]{64}$/.test(managedKey)) die("--managed-key must be 64 lowercase hex chars");
  assertNoSymlinkComponents(keyFile, "file");
  try {
    const prior = readFileSync(keyFile, "utf8");
    if (prior.trim() !== managedKey) die(`key file ${keyFile} holds different bytes; refusing conflicting owned state`);
  } catch (error) {
    if (error?.code !== "ENOENT") die(`key file: ${error.message}`);
  }
  // Idempotent write: the key is derived state, so on resume rewrite it
  // (checkpoint files under the dir are untouched).
  try { writePrivateAtomic(keyFile, managedKey); }
  catch (error) { die(`key file could not be written safely: ${error.message}`); }
} else {
  try { lstatSync(keyFile); die(`key file ${keyFile} already exists without --managed-key; refusing`); } catch (e) { if (e?.code !== "ENOENT") die(`state file: ${e.message}`); }
}

// Stale handshake descriptors from a dead previous run are ours to remove
// (the runtime rewrites them atomically); checkpoints are never touched.
for (const f of [portFile, modelsFile]) {
  assertNoSymlinkComponents(f, "file");
  try { rmSync(f, { force: true }); } catch {}
}

// Scrubbed child env: the runtime authenticates through its own key file;
// inherited user credentials or proxies are never passed down.
const env = {};
for (const keep of ["PATH", "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMPDIR", "LANG", "LC_ALL", "TERM", "SYSTEMROOT", "USERPROFILE", "CLAUDE_CONFIG_DIR"]) {
  if (process.env[keep] !== undefined) env[keep] = process.env[keep];
}
if (process.env.MANAGED_RUNTIME_EXTRA_ENV) {
  for (const name of process.env.MANAGED_RUNTIME_EXTRA_ENV.split(",")) {
    if (name && /^[A-Za-z_][A-Za-z0-9_]*$/.test(name) && process.env[name] !== undefined) env[name] = process.env[name];
  }
}

const child = spawn(runtime, [
  "--serve",
  "--port-file", portFile,
  "--key-file", keyFile,
  "--models-file", modelsFile,
  "--state-dir", stateDir,
  ...runtimeArgs,
], {
  env,
  stdio: ["ignore", "inherit", "inherit"],
});

let stopping = false;
let childExit = null;
child.once("exit", (code, signal) => { childExit = { code, signal }; });

const GRACE_MS = 7000;
const killTimer = () => setTimeout(() => { try { child.kill("SIGKILL"); } catch {} }, GRACE_MS);

function shutdown(exitCode) {
  if (stopping) return;
  stopping = true;
  try { child.kill("SIGTERM"); } catch {}
  const killer = killTimer();
  const done = new Promise(res => {
    if (childExit) return res();
    child.once("exit", res);
  });
  done.then(() => { clearTimeout(killer); release(); process.exit(exitCode); });
}

// Parent-host retirement: stdin EOF means the Bun host that spawned us is
// gone (host restart, update, removal, or SIGKILL). The runtime must not be
// orphaned — terminate it and leave.
if (process.stdin.readable) {
  process.stdin.resume(); // keep the FD open so close is observable
  process.stdin.once("end", () => shutdown(0));
  process.stdin.once("close", () => shutdown(0));
} else {
  shutdown(0);
}
process.on("SIGTERM", () => shutdown(0));
process.on("SIGINT", () => shutdown(0));

// Handshake: poll for the committed descriptor set (key+models+port written
// in that order by the runtime), bounded, then mark ready.
const deadline = Date.now() + 30000;
const poll = setInterval(() => {
  if (stopping) { clearInterval(poll); return; }
  if (childExit) {
    clearInterval(poll);
    console.error(`managed-supervisor: runtime exited before ready (code ${childExit.code})`);
    try { rmSync(readyFile, { force: true }); } catch {}
    release();
    process.exit(1);
  }
  try {
    if (existsSync(keyFile) && existsSync(modelsFile) && existsSync(portFile)) {
      try {
        for (const file of [keyFile, modelsFile, portFile, readyFile]) checkNoSymlinkComponents(file, "file");
      } catch (error) {
        if (error?.code === "MANAGED_PATH_ERROR") {
          clearInterval(poll);
          console.error(`managed-supervisor: unsafe runtime handshake path: ${error.message}`);
          shutdown(1);
          return;
        }
        throw error;
      }
      const port = readFileSync(portFile, "utf8").trim();
      const portNumber = Number(port);
      if (!/^\d+$/.test(port) || !Number.isInteger(portNumber) || portNumber < 1 || portNumber > 65535) throw new Error("invalid port");
      clearInterval(poll);
      if (!Number.isSafeInteger(child.pid) || child.pid < 1) {
        console.error("managed-supervisor: runtime child did not expose an owned pid");
        shutdown(1);
        return;
      }
      try { writePrivateAtomic(readyFile, JSON.stringify({ supervisorPid: process.pid, pid: child.pid, port: portNumber, stateDir: resolve(stateDir) }) + "\n"); }
      catch (error) {
        clearInterval(poll);
        console.error(`managed-supervisor: ready descriptor could not be written safely: ${error.message}`);
        shutdown(1);
        return;
      }
      child.once("exit", code => { release(); process.exit(code ?? 0); });
      return;
    }
  } catch {}
  if (Date.now() > deadline) {
    clearInterval(poll);
    console.error("managed-supervisor: runtime handshake timed out");
    shutdown(1);
  }
}, 60);
