import { createHash, createHmac, randomBytes, timingSafeEqual } from "node:crypto";

export const PROVIDER_ID = "claude-tui";
const sha = value => createHash("sha256").update(value).digest("hex");
const mac = (key, value) => createHmac("sha256", key).update(value).digest("hex");
const generatedSession = /^magpie-[a-f0-9]{24}$/;

function stableSession(value) {
  if (typeof value !== "string" || !value || value.length > 512 ||
      /[\r\n\0]/.test(value) || generatedSession.test(value)) {
    throw new Error("Claude TUI requires an explicit stable conversation id");
  }
  return sha("magpie-interactive-bridge/v1\0" + value);
}

function normalizeConfig(config) {
  const url = new URL(config.runtimeURL);
  // The first release owns a local runtime, never an arbitrary network proxy.
  if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" ||
      !url.port || url.username || url.password || url.search || url.hash ||
      url.pathname !== "/v1/") {
    throw new Error("runtimeURL must be http://127.0.0.1:<port>/v1/");
  }
  if (!config.models || typeof config.models !== "object" || Array.isArray(config.models) ||
      Object.keys(config.models).length === 0) {
    throw new Error("Claude TUI models must be configured from the selected runtime");
  }
  const models = {};
  for (const [id, model] of Object.entries(config.models)) {
    if (!/^[A-Za-z0-9_.:/-]{1,128}$/.test(id) || ["__proto__", "constructor", "prototype"].includes(id) || !model ||
        typeof model !== "object" || Array.isArray(model)) {
      throw new Error("Invalid Claude TUI model configuration");
    }
    models[id] = structuredClone(model);
  }
  return { url, models };
}

async function boundedBody(request, limit = 16 << 20) {
  if (!request.body) return "";
  const reader = request.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > limit) {
        await reader.cancel();
        throw new Error("Claude TUI request exceeds 16 MiB");
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  return new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks));
}

async function requireAuth(getAuth) {
  const auth = await getAuth();
  if (auth?.type !== "api" || typeof auth.key !== "string" ||
      auth.key.length < 16 || auth.key.length > 4096 || /[\r\n\0]/.test(auth.key)) {
    throw new Error("Claude TUI runtime connection is not signed in");
  }
  return auth.key;
}

// Reuses the existing bridge-auth-v1 protocol. No root ticket or new execution
// authority is introduced by the plugin adapter. The optional proofKey is a
// managed-mode seam: the plugin's own chat.headers proof and the signed fetch
// must share it when hooks live in different objects; explicit-options callers
// never pass it and keep per-instance behaviour.
export function createHooks(configuration, transport = globalThis.fetch, proofKey) {
  const { url, models } = normalizeConfig(configuration);
  const key = proofKey ?? randomBytes(32);
  const proof = digest => mac(key, "hook-proof/v1\0" + digest);
  return {
    async config(config) {
      config.provider ??= {};
      config.provider[PROVIDER_ID] = {
        name: "Claude TUI",
        npm: "@ai-sdk/anthropic",
        api: url.href,
        models: structuredClone(models),
      };
    },
    "chat.headers"(input, output) {
      if (input?.model?.providerID !== PROVIDER_ID) return;
      const session = stableSession(input.sessionID);
      output.headers["x-g3-hook-session"] = session;
      output.headers["x-g3-hook-proof"] = proof(session);
    },
    auth: {
      provider: PROVIDER_ID,
      methods: [{ type: "api", label: "Local TUI runtime connection key" }],
      async loader(getAuth) {
        await requireAuth(getAuth);
        return {
          baseURL: url.href,
          apiKey: "local-runtime-managed",
          async fetch(input, init) {
            const key = await requireAuth(getAuth);
            const request = new Request(input, init);
            const target = new URL(request.url);
            if (request.method !== "POST" || target.origin !== url.origin ||
                target.pathname !== "/v1/messages" || target.search || target.hash) {
              throw new Error("Claude TUI accepts only the configured messages endpoint");
            }
            if (request.signal.aborted) throw request.signal.reason;
            const digest = request.headers.get("x-g3-hook-session");
            const signature = request.headers.get("x-g3-hook-proof");
            // Host hooks swallow their errors. Requiring the internal proof
            // keeps missing/invalid session context from starting a runtime.
            if (!/^[a-f0-9]{64}$/.test(digest ?? "") ||
                !/^[a-f0-9]{64}$/.test(signature ?? "") ||
                !timingSafeEqual(Buffer.from(signature, "hex"), Buffer.from(proof(digest), "hex"))) {
              throw new Error("Claude TUI conversation context was not supplied by its hook");
            }
            const body = await boundedBody(request);
            if (request.signal.aborted) throw request.signal.reason;
            const session = mac(key, "magpie-bridge-account/v1\0" + digest);
            const timestamp = String(Math.floor(Date.now() / 1000));
            const nonce = randomBytes(16).toString("hex");
            const canonical = ["bridge-auth-v1", "POST", "/v1/messages",
              timestamp, nonce, session, sha(body)].join("\n");
            const headers = {
              "content-type": "application/json",
              "accept": request.headers.get("accept") ?? "text/event-stream",
              "anthropic-version": request.headers.get("anthropic-version") ?? "2023-06-01",
              "x-bridge-auth-version": "1",
              "x-bridge-timestamp": timestamp,
              "x-bridge-nonce": nonce,
              "x-bridge-session": session,
              "x-bridge-mac": mac(key, canonical),
            };
            const response = await transport(new URL("messages", url).href, {
              method: "POST", headers, body, signal: request.signal, redirect: "manual",
            });
            if (response.status >= 300 && response.status < 400) {
              await response.body?.cancel();
              throw new Error("Claude TUI runtime redirects are refused");
            }
            // Preserve SSE, usage, terminal errors and cancellation as supplied
            // by the existing runtime. Never replay a failed request here.
            return response;
          },
        };
      },
    },
  };
}

// Test/support surface only — object-valued, never a plugin function, so the
// host ignores it (documented _internal convention).
export const _internal = {
  stableSession,
  proofHmac: key => digest => mac(key, "hook-proof/v1\0" + digest),
};

export default {
  id: PROVIDER_ID,
  async server(input, options) {
    if (options) return createHooks(options);
    const raw = process.env.MAGPIE_TUI_PLUGIN_CONFIG;
    if (raw) return createHooks(JSON.parse(raw));
    // Plugin-center path: a plain `magpie plugin add` supplies no options.
    // Managed mode loads safely with no config side effects; the owned runtime
    // only starts inside account-scoped hooks after an explicit sign-in.
    const { createManagedHooks } = await import("./managed-runtime.mjs");
    return createManagedHooks({ input });
  },
};
