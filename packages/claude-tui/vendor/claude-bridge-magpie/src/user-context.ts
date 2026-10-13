import { lstatSync, readFileSync, renameSync, writeFileSync } from "node:fs";

export type UserContextPayload = { id: string; text: string; tool_call_ids: string[] };

export function consumeUserContext(input: Record<string, unknown>, env: NodeJS.ProcessEnv): Record<string, unknown> | null {
  const event = input.hook_event_name;
  const path = env.CLAUDE_BRIDGE_USER_CONTEXT_PATH;
  if (!path || env.CLAUDE_BRIDGE_RUNTIME_HOOK !== "1" || !["PostToolUse", "PostToolUseFailure"].includes(String(event))) return null;
  if (typeof input.tool_name !== "string" || !input.tool_name.startsWith("mcp__magpie__") || typeof input.tool_use_id !== "string") return null;
  let payload: UserContextPayload;
  try {
    const stat = lstatSync(path);
    if (!stat.isFile() || stat.size > 1_100_000 || (stat.mode & 0o077) !== 0) return null;
    payload = JSON.parse(readFileSync(path, "utf8"));
    if (!payload || typeof payload.id !== "string" || !/^[a-f0-9]{16,64}$/.test(payload.id) || typeof payload.text !== "string" || payload.text.length === 0 || Buffer.byteLength(payload.text) > 1_048_576 || !Array.isArray(payload.tool_call_ids) || !payload.tool_call_ids.every(id => typeof id === "string" && id !== "") || !payload.tool_call_ids.includes(input.tool_use_id)) return null;
    // Atomic claim ensures parallel post-tool hooks consume the message once.
    renameSync(path, path + ".claimed-" + payload.id);
  } catch {
    return null;
  }
  const context = "The outer client sent this new user message while the external tool was running. It is user input, separate from the tool's returned data:\n\n" + payload.text;
  writeFileSync(path + ".ack", JSON.stringify({ id: payload.id, tool_use_id: input.tool_use_id }) + "\n", { mode: 0o600 });
  return { hookSpecificOutput: { hookEventName: event, additionalContext: context } };
}
