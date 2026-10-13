import { describe, expect, test } from "bun:test"
import { ClaudeTUIPlugin } from "./index.mjs"

describe("Claude TUI community entry", () => {
  test("loads without starting Claude and exposes the provider contract", async () => {
    const hooks = await ClaudeTUIPlugin({ directory: process.cwd() })

    expect(hooks.auth.provider).toBe("claude-tui")
    expect(typeof hooks.auth.loader).toBe("function")
    expect(hooks.auth.methods).toHaveLength(1)
    expect(hooks.auth.methods[0].type).toBe("oauth")
    expect(typeof hooks.auth.methods[0].authorize).toBe("function")

    const config = { provider: {} }
    await hooks.config(config)
    expect(config.provider["claude-tui"].npm).toBe("@ai-sdk/anthropic")
    expect(Object.keys(config.provider["claude-tui"].models).length).toBeGreaterThan(0)
  })
})
