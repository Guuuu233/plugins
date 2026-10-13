// src/managed-runtime.mjs — plugin-center managed mode.
//
// Loaded when `server(input)` is called without options (a plain
// `magpie plugin add`, including Discover one-click). The default server must
// load safely with NO config side effects; the owned runtime only starts
// lazily inside account-scoped hooks after the user signs in.
//
// Sign-in flow (documented magpie OAuth contract):
//   methods[0].prompts — collected by the GUI before authorize:
//       profileDir  absolute path of the dedicated native Claude config dir
//                   (selected profile is read by Claude only after opt-in;
//                   never modified by this plugin)
//       models      comma-separated real model ids the runtime should serve
//   authorize(inputs) — validates inputs, runs actionable dependency probes,
//       and returns an auto OAuth flow. Its callback returns an opaque
//       success key/metadata; magpie stores that callback key as API auth.
//       The callback does not perform or verify a Claude login. The opaque
//       key carries profile, models, state path and managed secret; the actual
//       runtime connection key is never handed to the GUI or logged.
//   loader(getAuth) / provider.models / fetch — the single lazy startup per
//       account, behind an in-process lock. Each account owns one supervised
//       runtime under its own 0700 state dir with a persistent 0600 key and
//       checkpoints; a hot host reload must not double-start or corrupt it.
//
// Boundaries: win32 is unsupported (WSL is required — the runtime drives
// tmux/Linux PTY; explain, never pretend); no postinstall, no launchagents,
// no global installs, no guessed pid kills, no auth-status probing.
import { createHash, createHmac, randomBytes } from "node:crypto";
import { lstatSync, readFileSync, realpathSync, statSync } from "node:fs";
import { chmod, mkdir, readFile } from "node:fs/promises";
import { homedir, platform } from "node:os";
import { delimiter, dirname, isAbsolute, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn, spawnSync } from "node:child_process";
import { createHooks, PROVIDER_ID, _internal } from "./index.mjs";

const packageRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const SUPERVISOR = join(packageRoot, "scripts", "managed-runtime-supervisor.mjs");

const hex = n => randomBytes(n).toString("hex");
const sha = value => createHash("sha256").update(value).digest("hex");

// ---------- serialized account key (opaque to the host GUI) ----------
// The stored api key carries everything the lazy hooks need to start the
// account's own runtime: validated inputs + a managed secret used only for
// local proof/session derivation. The runtime's own connection key is
// separate, generated per-state-dir, and never leaves the state dir.
const encodeKey = spec => "mgtui1." + Buffer.from(JSON.stringify(spec), "utf8").toString("base64url");
function decodeKey(key) {
  if (typeof key !== "string" || !key.startsWith("mgtui1.")) return null;
  let spec;
  try { spec = JSON.parse(Buffer.from(key.slice(7), "base64url").toString("utf8")); } catch { return null; }
  if (!spec || spec.v !== 1 || typeof spec.profile !== "string" ||
      !Array.isArray(spec.models) || spec.models.length === 0 ||
      typeof spec.managed !== "string" || typeof spec.state !== "string") return null;
  return spec;
}

// ---------- input validation ----------
const MODEL_ID = /^[A-Za-z0-9_.:/-]{1,128}$/;
const modelList = raw => {
  const ids = String(raw ?? "").split(",").map(s => s.trim()).filter(Boolean);
  if (ids.length === 0 || ids.length > 32 || ids.some(id => !MODEL_ID.test(id) || ["__proto__", "constructor", "prototype"].includes(id))) {
    throw new Error("models must be 1-32 explicit runtime model ids (comma-separated)");
  }
  return [...new Set(ids)];
};

// The selected profile must be an absolute, real directory owned by the
// account. Relative paths, escapes and symlinked dirs are refused — the
// profile is read by Claude after opt-in, never modified here.
function profileDir(raw, home = realpathSync(homedir())) {
  const value = String(raw ?? "").trim();
  if (!value || !isAbsolute(value)) throw new Error("Claude profile dir must be an absolute path");
  let st;
  try { st = lstatSync(value); } catch { throw new Error(`Claude profile dir does not exist: ${value}`); }
  if (st.isSymbolicLink()) throw new Error(`Claude profile dir ${value} is a symlink; pass the real directory`);
  if (!st.isDirectory()) throw new Error(`Claude profile dir is not a directory: ${value}`);
  const real = realpathSync(value);
  if (real !== home && !real.startsWith(home + sep)) {
    throw new Error(`Claude profile dir must live under the user's home (${home}); outside paths are not owned state`);
  }
  return real;
}

function assertNoSymlinkPath(path, finalKind = "any") {
  const target = resolve(path);
  let current = sep;
  for (const component of target.split(sep).filter(Boolean)) {
    current = join(current, component);
    let st;
    try { st = lstatSync(current); } catch (error) {
      if (error?.code === "ENOENT") continue;
      throw error;
    }
    if (st.isSymbolicLink()) throw new Error(`managed state path contains a symlink: ${current}`);
    const final = current === target;
    if (!final && !st.isDirectory()) throw new Error(`managed state path has a non-directory ancestor: ${current}`);
    if (final && finalKind === "directory" && !st.isDirectory()) throw new Error(`managed state path is not a directory: ${current}`);
    if (final && finalKind === "file" && !st.isFile()) throw new Error(`managed state path is not a regular file: ${current}`);
  }
}

async function ensurePrivateDirectory(path) {
  assertNoSymlinkPath(path, "directory");
  await mkdir(path, { recursive: true, mode: 0o700 });
  assertNoSymlinkPath(path, "directory");
  await chmod(path, 0o700);
}

function profileRoutesLoopback(profile, env) {
  const setting = /ANTHROPIC_[A-Z_]*URL[^=:/]*[=:][^/]*\/\/(127\.0\.0\.1|localhost)(?::\d+)?(?:[/?#\s"']|$)/i;
  for (const name of ["settings.env", "settings.json"]) {
    const path = join(profile, name);
    assertNoSymlinkPath(path, "file");
    let content;
    try { content = readFileSync(path, "utf8"); }
    catch (error) { if (error?.code === "ENOENT") continue; throw error; }
    if (setting.test(content)) return true;
  }
  for (const name of ["ANTHROPIC_BASE_URL", "ANTHROPIC_API_URL", "ANTHROPIC_BEDROCK_BASE_URL"]) {
    if (/^https?:\/\/(?:127\.0\.0\.1|localhost)(?::\d+)?(?:[/?#]|$)/i.test(env[name] ?? "")) return true;
  }
  return false;
}

// ---------- dependency probes (actionable, no installs, no spawning) ----------
function probeDeps() {
  const found = cmd => {
    const r = spawnSync(process.platform === "win32" ? "where" : "command", ["-v", cmd], { encoding: "utf8", shell: true, timeout: 8000 });
    const path = (r.stdout || "").trim().split("\n")[0];
    return r.status === 0 && path ? path : null;
  };
  return {
    bun: found("bun") ?? (process.versions?.bun ? process.execPath : null), // a plugin host Bun is itself a usable runtime
    tmux: found("tmux"),     // runtime drives Claude's interactive TUI in tmux
    claude: found("claude"), // the native CLI the selected profile signs into
  };
}
const missingDeps = d => Object.entries(d).filter(([, v]) => !v).map(([k]) => k);

// The runtime's connection key is derived from the managed secret: the
// supervisor passes --managed-key so <state>/key holds exactly this value
// (persistent across restarts) and the plugin signs without ever surfacing
// the key to the host GUI or logs.
const runtimeKey = spec => createHmac("sha256", spec.managed).update("magpie-tui-runtime-key/v1").digest("hex");
const depsHint = missing =>
  `missing dependencies: ${missing.join(", ")}. ` +
  `Install them yourself (no automatic global install is attempted): ` +
  `bun → https://bun.sh, tmux → your package manager, claude → https://claude.com/claude-code. ` +
  `Windows requires an existing WSL2 distro; there is no native win32 runtime.`;

// ---------- runtime entry resolution (bundled prebuilt, no Go required) ----------
function runtimeEntry() {
  const exe = process.platform === "win32" ? "magpie-tui-runtime.exe" : "magpie-tui-runtime";
  const plat = { darwin: "darwin", linux: "linux" }[process.platform];
  const arch = { arm64: "arm64", x64: "amd64" }[process.arch] ?? process.arch;
  const candidates = [
    join(packageRoot, "bin", exe),
    plat ? join(packageRoot, "bin", `magpie-tui-runtime.${plat}-${arch}`) : null,
    plat ? join(packageRoot, "bin", `magpie-tui-runtime.${plat}-${arch}${process.platform === "win32" ? ".exe" : ""}`) : null,
  ].filter(Boolean);
  for (const c of candidates) {
    try { if (statSync(c).isFile() && (statSync(c).mode & 0o111)) return c; } catch {}
  }
  return null;
}
const bridgeEntry = () => {
  for (const name of ["magpie-tui-bridge"]) {
    const p = join(packageRoot, "bin", name);
    try { if (statSync(p).isFile()) return p; } catch {}
  }
  return null;
};
const helperEntry = () => {
  const plat = { darwin: "darwin", linux: "linux" }[process.platform];
  const arch = { arm64: "arm64", x64: "amd64" }[process.arch] ?? process.arch;
  for (const p of [join(packageRoot, "bin", "magpie-tui-mcp-helper"), plat && join(packageRoot, "bin", `magpie-tui-mcp-helper.${plat}-${arch}`)].filter(Boolean)) {
    try { if (statSync(p).isFile() && (statSync(p).mode & 0o111)) return p; } catch {}
  }
  return null;
};

// ---------- per-account lazy runtime ----------
// Lock identity is the stored key itself — account-scoped and stable across
// hot reloads within the same host process; never a shared default map.
const accounts = new Map(); // key -> Promise<owned>

export function createManagedHooks({
  input = {},
  deps = probeDeps,
  spawnRuntime,           // test seam: (argv, opts) => child process
  runtimeEntry: entryOf,  // test seam: () => runtime entry path
  transport,              // test seam: forwarded to createHooks fetch
  env = process.env,
} = {}) {
  if (platform() === "win32") {
    throw new Error(
      "magpie-claude-tui managed mode does not support native Windows: the runtime " +
      "drives the Claude TUI through tmux on a Linux PTY. Use an existing WSL2 " +
      "distribution and install the plugin inside it; there is no win32 build.");
  }
  const home = realpathSync(env.HOME || homedir());
  const stateRoot = typeof input.directory === "string" && isAbsolute(input.directory)
    ? join(input.directory, "magpie-claude-tui")
    : join(env.XDG_CONFIG_HOME || join(home, ".config"), "magpie-claude-tui");
  const launch = spawnRuntime ?? ((argv, opts) => spawn(process.execPath, argv, opts));
  const loaderCache = new Map(); // storedKey -> built loader options

  const safeChildEnv = profile => {
    const clean = {};
    for (const name of ["PATH", "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMPDIR", "LANG", "LC_ALL", "TERM", "SYSTEMROOT", "USERPROFILE"]) {
      if (env[name] !== undefined) clean[name] = env[name];
    }
    const hostDir = dirname(process.execPath);
    const pathParts = (clean.PATH ?? "").split(delimiter).filter(Boolean);
    if (process.versions?.bun && !pathParts.includes(hostDir)) clean.PATH = [hostDir, ...pathParts].join(delimiter);
    clean.CLAUDE_CONFIG_DIR = profile;
    return clean;
  };

  const waitForExit = (child, ms) => new Promise(resolveExit => {
    if (child.exitCode !== null || child.signalCode !== null) return resolveExit(true);
    const timer = setTimeout(() => resolveExit(false), ms);
    child.once("exit", () => { clearTimeout(timer); resolveExit(true); });
  });

  async function stopSupervisor(child) {
    if (child.exitCode !== null || child.signalCode !== null) return;
    try { child.stdin?.end(); } catch {}
    if (await waitForExit(child, 8000)) return;
    try { child.kill("SIGTERM"); } catch {}
    if (await waitForExit(child, 2000)) return;
    try { child.kill("SIGKILL"); } catch {}
    await waitForExit(child, 2000);
  }

  async function startAccount(spec) {
    if (profileDir(spec.profile, home) !== spec.profile || profileRoutesLoopback(spec.profile, env)) {
      throw new Error("Selected Claude profile is no longer safe for managed startup; choose a real dedicated profile without loopback Anthropic routing.");
    }
    const entry = entryOf ? entryOf() : runtimeEntry();
    if (!entry) throw new Error(
      "no bundled magpie-tui-runtime binary for this platform in the package " +
      "(expected bin/magpie-tui-runtime[.<os>-<arch>]). The package ships " +
      "darwin/linux arm64+x64; other platforms are unsupported.");
    const bridge = bridgeEntry();
    const helper = helperEntry();
    const accountDir = join(stateRoot, "accounts", sha(spec.managed).slice(0, 16));
    const stateDir = join(accountDir, "state");
    await ensurePrivateDirectory(stateRoot);
    await ensurePrivateDirectory(join(stateRoot, "accounts"));
    await ensurePrivateDirectory(accountDir);
    const readyFile = join(accountDir, `.ready-${hex(16)}.json`);
    assertNoSymlinkPath(readyFile, "file");

    // Persisted owned key: the runtime mints/validates it under --reuse-key
    // (0600 regular file, 64 hex). It never leaves the state dir.
    const args = [
      SUPERVISOR,
      "--runtime", entry,
      "--state-dir", stateDir,
      "--ready-file", readyFile,
      "--managed-key", runtimeKey(spec),
      "--runtime-arg", "--reuse-key",
    ];
    if (bridge) args.push("--runtime-arg", "--bridge", "--runtime-arg", bridge);
    if (helper) args.push("--runtime-arg", "--helper", "--runtime-arg", helper);
    args.push("--runtime-arg", "--claude-config-dir", "--runtime-arg", spec.profile);
    args.push("--runtime-arg", "--models", "--runtime-arg", spec.models.join(","));

    const child = launch(args, {
      env: safeChildEnv(spec.profile),
      stdio: ["pipe", "ignore", "inherit"],
    });
    const exited = new Promise((res, rej) => {
      child.once("error", rej);
      child.once("exit", code => rej(new Error(`managed runtime supervisor exited during startup (code ${code})`)));
    });
    try {
      const deadline = Date.now() + 30000;
      while (Date.now() < deadline) {
        try {
          assertNoSymlinkPath(readyFile, "file");
          const ready = JSON.parse(await Promise.race([readFile(readyFile, "utf8"), exited]));
          if (!Number.isSafeInteger(child.pid) || ready?.supervisorPid !== child.pid ||
              !Number.isSafeInteger(ready?.pid) || ready.pid < 1 ||
              !Number.isInteger(ready?.port) || ready.port < 1 || ready.port > 65535 ||
              ready.stateDir !== resolve(stateDir)) {
            throw new Error("managed runtime readiness did not match the current owned supervisor");
          }
          const keyPath = join(stateDir, "key");
          const modelsPath = join(stateDir, "models.json");
          assertNoSymlinkPath(stateDir, "directory");
          assertNoSymlinkPath(keyPath, "file");
          assertNoSymlinkPath(modelsPath, "file");
          const [keyHex, modelsRaw] = await Promise.all([
            readFile(keyPath, "utf8"),
            readFile(modelsPath, "utf8"),
          ]);
          if (keyHex.trim() !== runtimeKey(spec) || (statSync(keyPath).mode & 0o777) !== 0o600) {
            throw new Error("managed runtime key file did not match the account-owned private key");
          }
          const descriptor = JSON.parse(modelsRaw);
          const reported = descriptor?.models;
          if (!reported || typeof reported !== "object" || Array.isArray(reported)) {
            throw new Error("managed runtime returned a malformed model capability map");
          }
          for (const [id, model] of Object.entries(reported)) {
            if (!MODEL_ID.test(id) || !model || typeof model !== "object" || Array.isArray(model) ||
                (model.id !== undefined && model.id !== id) ||
                (model.name !== undefined && typeof model.name !== "string")) {
              throw new Error("managed runtime returned a malformed model capability id");
            }
          }
          const missing = spec.models.filter(id => !Object.hasOwn(reported, id));
          if (missing.length) throw new Error(`managed runtime did not report configured model id(s): ${missing.join(", ")}`);
          const models = Object.fromEntries(spec.models.map(id => [id, { ...reported[id] }]));
          return {
            url: `http://127.0.0.1:${ready.port}/v1/`,
            models,
            stop: () => { try { child.kill("SIGTERM"); } catch {} },
          };
        } catch (error) {
          if (error?.code !== "ENOENT" && !/Unexpected|JSON/.test(error?.message ?? "")) throw error;
          await new Promise(r => setTimeout(r, 60));
        }
      }
      throw new Error("managed runtime did not report ready in time");
    } catch (error) {
      await stopSupervisor(child);
      throw error;
    } finally {
      exited.catch(() => {});
    }
  }

  async function owned(getAuth) {
    const auth = await getAuth();
    const spec = auth?.type === "api" ? decodeKey(auth.key) : null;
    if (!spec) {
      throw Object.assign(
        new Error("Claude TUI managed runtime is not signed in — use the plugin's sign-in to select a profile and models"),
        { signIn: "expired" });
    }
    if (!accounts.has(auth.key)) {
      accounts.set(auth.key, startAccount(spec).catch(e => { accounts.delete(auth.key); throw e; }));
    }
    return { spec, runtime: await accounts.get(auth.key) };
  }

  // Managed mode shares one hooks object across config/chat.headers/loader —
  // one proof key binds them, the account key is only used inside fetch.
  const managedProofKey = randomBytes(32);
  const proof = _internal.proofHmac(managedProofKey);

  return {
    async config(config) {
      config.provider ??= {};
      // Safe no-side-effect surface: provider declared, real model list is
      // only populated after sign-in via provider.models.
      config.provider[PROVIDER_ID] ??= { name: "Claude TUI", npm: "@ai-sdk/anthropic", models: {} };
    },
    "chat.headers"(input, output) {
      if (input?.model?.providerID !== PROVIDER_ID) return;
      const session = _internal.stableSession(input.sessionID);
      output.headers["x-g3-hook-session"] = session;
      output.headers["x-g3-hook-proof"] = proof(session);
    },
    auth: {
      provider: PROVIDER_ID,
      methods: [{
        type: "oauth",
        label: "连接已登录 Claude",
        prompts: [
          { type: "text", key: "profileDir",
            message: "Dedicated native Claude config dir (absolute path under your home; Claude reads it after opt-in, the plugin never writes it)",
            placeholder: "/home/you/.magpie-claude-tui",
            validate: v => { try { profileDir(v, home); return undefined; } catch (e) { return e.message; } } },
          { type: "text", key: "models",
            message: "Model ids this runtime serves (comma-separated; real ids only — no fake capability list)",
            placeholder: "claude-opus-4-6, claude-sonnet-4-6",
            validate: v => { try { modelList(v); return undefined; } catch (e) { return e.message; } } },
        ],
        async authorize(inputs) {
          const profile = profileDir(inputs?.profileDir, home);
          if (profileRoutesLoopback(profile, env)) {
            throw new Error("Selected Claude profile routes Anthropic requests to a loopback gateway. Choose a dedicated profile that does not point to 127.0.0.1 or localhost.");
          }
          const spec = {
            v: 1,
            profile,
            models: modelList(inputs?.models),
            managed: hex(24),      // account-scoped state identity + proof secret
            state: null,           // filled below
          };
          spec.state = join(stateRoot, "accounts", sha(spec.managed).slice(0, 16), "state");
          const missing = missingDeps(await deps());
          if (missing.length) throw new Error(depsHint(missing));
          // The GUI saves the opaque callback key as API auth. The callback
          // only connects the host to the selected local profile; it performs
          // and verifies no Claude login or OAuth exchange.
          return {
            url: "",
            method: "auto",
            instructions: "This connects to the existing logged-in Claude CLI in the selected profile. The plugin does not sign in or verify Claude login status.",
            async callback() {
              return {
                type: "success",
                key: encodeKey(spec),
                metadata: { accountId: spec.profile },
              };
            },
          };
        },
      }],
      async loader(getAuth) {
        const auth0 = await getAuth();
        if (auth0?.key && loaderCache.has(auth0.key)) return loaderCache.get(auth0.key);
        const { spec, runtime } = await owned(getAuth);
        const hooks = createHooks({ runtimeURL: runtime.url, models: runtime.models }, transport, managedProofKey);
        const built = {
          baseURL: runtime.url,
          apiKey: "local-runtime-managed",
          async fetch(input, init) {
            // The signed request uses the internally derived connection key
            // (HMAC secret only); it is never read back from disk here and
            // never handed to the host GUI or stored in plugin-auth.json.
            const authShim = async () => ({ type: "api", key: runtimeKey(spec) });
            const signed = await hooks.auth.loader(authShim);
            return signed.fetch(input, init);
          },
        };
        if (auth0?.key) loaderCache.set(auth0.key, built);
        return built;
      },
    },
    provider: {
      id: PROVIDER_ID,
      async models(provider, { auth } = {}) {
        const spec = auth?.type === "api" ? decodeKey(auth.key) : null;
        if (!spec) return provider.models ?? {};
        return Object.fromEntries(spec.models.map(id => [id, { id, name: id }]));
      },
    },
  };
}
