/**
 * Goblin plugin for Kilo Code (OpenCode plugin API): tells Goblin what the chat is doing, so Switchboard shows it as
 * working / needs permission / has a question / done and the phone gets alerts.
 *
 * It sends Claude Code-shaped hook events to `goblin hook claude` (one mapping for every harness), only when the
 * state changes. It never blocks Kilo, never prints and ignores every error; outside a Goblin chat (no GOBLIN_KIND)
 * it does nothing.
 *
 * Install: copy to ~/.config/kilo/plugin/goblin.ts (Windows: %USERPROFILE%\.config\kilo\plugin\). The goblin program
 * is GOBLIN_BIN, else C:\AI\goblin\goblin.exe on Windows, else `goblin` on the PATH.
 */
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";

function goblinBin(): string {
  if (process.env.GOBLIN_BIN) return process.env.GOBLIN_BIN;
  if (process.platform === "win32" && existsSync("C:\\AI\\goblin\\goblin.exe")) return "C:\\AI\\goblin\\goblin.exe";
  return "goblin";
}

export const GoblinPlugin = async () => {
  if (!process.env.GOBLIN_KIND) return {};
  const bin = goblinBin();
  const assistant = new Set<string>(); // assistant message ids
  let lastText = "";
  let state = "";

  const send = (name: string, extra: Record<string, unknown> = {}) => {
    try {
      const p = spawn(bin, ["hook", "claude"], { stdio: ["pipe", "ignore", "ignore"], windowsHide: true });
      p.on("error", () => {});
      p.stdin.on("error", () => {});
      p.stdin.end(JSON.stringify({ hook_event_name: name, ...extra }));
    } catch {
      // never break Kilo
    }
  };
  const to = (s: string, name: string, extra: Record<string, unknown> = {}) => {
    if (s === state && s !== "permission" && s !== "question") return;
    state = s;
    send(name, extra);
  };

  return {
    event: async ({ event }: any) => {
      try {
        const t: string = event?.type ?? "";
        const p = event?.properties ?? {};
        if (t === "message.updated" && p.info?.role === "assistant" && p.info?.id) {
          assistant.add(p.info.id);
        } else if (t === "message.part.updated" && p.part?.type === "text" && assistant.has(p.part.messageID)) {
          if (String(p.part.text ?? "").trim()) lastText = p.part.text;
        } else if (t === "session.status" && p.status?.type === "busy") {
          if (state !== "working") lastText = "";
          to("working", "PreToolUse");
        } else if (t === "permission.asked" || t === "permission.updated") {
          const what = p.title ?? p.permission ?? p.type ?? "a tool";
          to("permission", "PermissionRequest", { tool_name: String(p.permission ?? p.type ?? "tool"), tool_input: { description: String(what) } });
        } else if (t === "question.asked") {
          const q = (p.questions ?? [])[0] ?? {};
          to("question", "PreToolUse", {
            tool_name: "AskUserQuestion",
            tool_input: { questions: [{ question: q.question ?? q.header ?? "", options: (q.options ?? []).map((o: any) => ({ label: o.label, description: o.description })) }] },
          });
        } else if (t === "permission.replied" || t === "question.replied") {
          state = "";
          to("working", "PostToolUse");
        } else if (t === "session.idle") {
          to("done", "Stop", { last_assistant_message: lastText || "Finished." });
        }
      } catch {
        // ignore
      }
    },
  };
};
