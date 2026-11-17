/**
 * Goblin extension for the Pi coding agent (https://pi.dev): tells Goblin what the chat is doing, so Switchboard shows
 * it as working / done and the phone gets an alert when Pi finishes.
 *
 * It sends Claude Code-shaped hook events to `goblin hook claude` (one mapping for both harnesses). It never blocks
 * Pi, never prints and ignores every error; outside a Goblin chat (no GOBLIN_KIND) it does nothing.
 *
 * Install: copy to ~/.pi/agent/extensions/goblin.ts (Windows: %USERPROFILE%\.pi\agent\extensions\, or the folder in
 * PI_CODING_AGENT_DIR). The goblin program is found as GOBLIN_BIN, else C:\AI\goblin\goblin.exe on Windows, else
 * `goblin` on the PATH.
 */
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";

function textOf(message: any): string {
  const c = message?.content;
  if (typeof c === "string") return c;
  if (!Array.isArray(c)) return "";
  return c.filter((b: any) => b?.type === "text").map((b: any) => b.text ?? "").join("\n").trim();
}

function goblinBin(): string {
  if (process.env.GOBLIN_BIN) return process.env.GOBLIN_BIN;
  if (process.platform === "win32" && existsSync("C:\\AI\\goblin\\goblin.exe")) return "C:\\AI\\goblin\\goblin.exe";
  return "goblin";
}

export default function (pi: any) {
  if (!process.env.GOBLIN_KIND) return; // not a Goblin chat
  const bin = goblinBin();
  let lastText = "";

  const send = (name: string, extra: Record<string, unknown> = {}) => {
    try {
      const p = spawn(bin, ["hook", "claude"], { stdio: ["pipe", "ignore", "ignore"], windowsHide: true });
      p.on("error", () => {});
      p.stdin.on("error", () => {});
      p.stdin.end(JSON.stringify({ hook_event_name: name, ...extra }));
    } catch {
      // never break Pi
    }
  };

  pi.on("before_agent_start", async (event: any) => {
    if (event?.messageType !== "user") return;
    lastText = "";
    send("UserPromptSubmit", { prompt: String(event.prompt ?? "") });
  });
  // Pi has no approval system of its own: any dialog an extension opens (permission gate, question) is a prompt
  pi.on("ui_prompt_start", async (event: any) => {
    const title = String(event?.title ?? "");
    if (event?.kind === "confirm") send("PermissionRequest", { tool_name: "Pi", tool_input: { description: title } });
    else send("PreToolUse", { tool_name: "AskUserQuestion", tool_input: { questions: [{ question: title, options: [] }] } });
  });
  pi.on("ui_prompt_end", async () => send("PostToolUse", {}));
  pi.on("tool_execution_start", async (event: any) => {
    if (!event?.parentToolCallId) send("PreToolUse", { tool_name: event?.toolName });
  });
  pi.on("message_end", async (event: any) => {
    if (event?.message?.role !== "assistant") return;
    const t = textOf(event.message);
    if (t) lastText = t;
  });
  // agent_settled = Pi will not continue on its own (agent_end can be followed by retries / queued messages)
  pi.on("agent_settled", async (event: any) => {
    if (event?.aborted) send("Notification", { message: "Stopped: waiting for your input" });
    else send("Stop", { last_assistant_message: lastText || "Finished." });
  });
  pi.on("session_shutdown", async (event: any) => {
    if (event?.reason === "quit") send("SessionEnd");
  });
}
