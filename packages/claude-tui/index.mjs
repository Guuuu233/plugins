import plugin from "./src/index.mjs"

const setupModels = {
  "claude-opus-4-6": { name: "Claude Opus 4.6" },
  "claude-sonnet-4-6": { name: "Claude Sonnet 4.6" },
}

export async function ClaudeTUIPlugin(input) {
  const hooks = await plugin.server(input)
  const configure = hooks.config

  return {
    ...hooks,
    async config(config) {
      await configure?.(config)
      const provider = config.provider?.[hooks.auth.provider]
      if (provider && Object.keys(provider.models ?? {}).length === 0) {
        provider.models = structuredClone(setupModels)
      }
    },
  }
}
