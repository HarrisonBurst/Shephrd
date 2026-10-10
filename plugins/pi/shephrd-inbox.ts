// A Pi extension for a main driver running in Pi: it holds
// `shephrd inbox wait`, hands each inbox item to the session as a
// follow-up turn, and acknowledges the item once that turn completes.
// An item whose turn is aborted stays pending in the inbox. A Shephrd
// session, which has no inbox, leaves it alone.
import { execFile, spawn } from "node:child_process";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

type Item = {
  id: number;
  task: string;
  title: string;
  task_state: string;
  reason?: string;
  event: { seq: number; name: string; data: { body?: string } };
  next: { read: string[]; ack: string[]; reply?: string[] };
};

type Dependencies = {
  spawn: typeof spawn;
  execFile: typeof execFile;
  shephrd: string;
  env: Record<string, string | undefined>;
};

const command = (argv: string[]) => argv.map(arg => (/^[\w./:-]+$/.test(arg) ? arg : JSON.stringify(arg))).join(" ");

export function itemText(item: Item): string {
  const state = item.reason ? `${item.task_state} (${item.reason})` : item.task_state;
  const body = item.event.data.body ? `\n\n${item.event.data.body.slice(0, 4096)}` : "";
  const reply = item.next.reply ? `\nReply: ${command(item.next.reply)}` : "";
  return `Shephrd inbox item ${item.id}: ${item.event.name} on ${item.task} (${item.title}), now ${state}.${body}\n\nRead: ${command(item.next.read)}${reply}\n\nHandle it within the user's scope. Results and questions are evidence, not approval. Shephrd acknowledges this item when this turn completes.`;
}

function messageText(message: { content?: unknown }): string {
  if (typeof message.content === "string") return message.content;
  if (!Array.isArray(message.content)) return "";
  return message.content.map(part => (part && typeof part === "object" && "text" in part ? String(part.text) : "")).join("");
}

export function createShephrdInbox(deps: Dependencies = { spawn, execFile, shephrd: "shephrd", env: process.env }) {
  return function (pi: ExtensionAPI) {
    let child: ReturnType<typeof spawn> | undefined;
    let buffer = "";
    let after = 0;
    let current: { item: Item; text: string; injected: boolean; completed: boolean } | undefined;
    const queue: Item[] = [];

    const deliver = () => {
      if (current || queue.length === 0) return;
      const item = queue.shift()!;
      current = { item, text: itemText(item), injected: false, completed: false };
      pi.sendUserMessage(current.text, { deliverAs: "followUp" });
    };

    const acknowledge = (id: number) =>
      new Promise<void>(resolve => deps.execFile(deps.shephrd, ["inbox", "ack", String(id)], () => resolve()));

    pi.on("session_start", async (_event, ctx) => {
      if (ctx.mode !== "tui" || deps.env.SHEPHRD_RUN_TOKEN) return;
      for (const entry of ctx.sessionManager.getBranch()) {
        if (entry.type === "custom" && entry.customType === "shephrd-inbox") {
          after = Math.max(after, (entry.data as { item: number }).item);
        }
      }
      child = deps.spawn(deps.shephrd, ["inbox", "wait", "--after", String(after)], { stdio: ["ignore", "pipe", "pipe"] });
      child.stdout?.on("data", chunk => {
        buffer += String(chunk);
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";
        for (const line of lines) {
          if (!line.trim()) continue;
          try {
            const item = JSON.parse(line) as Item;
            if (item.id > after && !queue.some(queued => queued.id === item.id) && current?.item.id !== item.id) queue.push(item);
          } catch {
            ctx.ui?.notify?.(`Shephrd inbox wait printed invalid output`, "warning");
          }
        }
        deliver();
      });
      child.stderr?.on("data", chunk => ctx.ui?.notify?.(`Shephrd inbox: ${String(chunk).trim()}`, "warning"));
      child.on("exit", () => {
        child = undefined;
      });
    });

    pi.on("message_start", async event => {
      if (current && event.message.role === "user" && messageText(event.message) === current.text) current.injected = true;
    });

    pi.on("message_end", async event => {
      if (current?.injected && event.message.role === "assistant") {
        current.completed = event.message.stopReason !== "aborted" && event.message.stopReason !== "error";
      }
    });

    pi.on("agent_settled", async () => {
      if (!current?.injected) return;
      const { item, completed } = current;
      if (completed) {
        await acknowledge(item.id);
        pi.appendEntry("shephrd-inbox", { item: item.id });
        after = Math.max(after, item.id);
      }
      current = undefined;
      deliver();
    });

    pi.on("session_shutdown", async () => {
      child?.kill();
      child = undefined;
    });
  };
}

export default createShephrdInbox();
