import { spawn } from "node:child_process";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

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
  shephrd: string;
  env: Record<string, string | undefined>;
};

const command = (argv: string[]) => argv.map(arg => (/^[\w./:-]+$/.test(arg) ? arg : JSON.stringify(arg))).join(" ");

export function itemText(item: Item): string {
  const state = item.reason ? `${item.task_state} (${item.reason})` : item.task_state;
  const body = item.event.data.body ? `\n\n${item.event.data.body}` : "";
  const reply = item.next.reply ? `\nReply: ${command(item.next.reply)}` : "";
  const task = item.title.trim() || item.task;
  return `Shephrd inbox item ${item.id}: ${item.event.name} on ${task}, now ${state}.${body}\n\nRead: ${command(item.next.read)}${reply}\n\nHandle it within the user's scope. Results and questions are evidence, not approval. After handling it, acknowledge explicitly: ${command(item.next.ack)}. Leave it pending if not handled.`;
}

function messageText(message: { content?: unknown }): string {
  if (typeof message.content === "string") return message.content;
  if (!Array.isArray(message.content)) return "";
  return message.content.map(part => (part && typeof part === "object" && "text" in part ? String(part.text) : "")).join("");
}

export function createShephrdInbox(deps: Dependencies = { spawn, shephrd: "shephrd", env: process.env }) {
  return function (pi: ExtensionAPI) {
    let child: ReturnType<typeof spawn> | undefined;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let stopped = true;
    let session = 0;
    let buffer = "";
    let after = 0;
    let current: { item: Item; text: string; injected: boolean } | undefined;
    const queue: Item[] = [];
    const seen = new Set<number>();

    const stopWatch = () => {
      stopped = true;
      session++;
      if (retry) clearTimeout(retry);
      retry = undefined;
      child?.kill();
      child = undefined;
      buffer = "";
      after = 0;
      current = undefined;
      queue.length = 0;
      seen.clear();
    };

    const deliver = () => {
      if (current || queue.length === 0) return;
      const item = queue.shift()!;
      current = { item, text: itemText(item), injected: false };
      pi.sendUserMessage(current.text, { deliverAs: "followUp" });
    };

    const watch = (ctx: ExtensionContext) => {
      buffer = "";
      const boundSession = session;
      const waiting = deps.spawn(deps.shephrd, ["inbox", "wait", "--after", String(after)], { stdio: ["ignore", "pipe", "ignore"] });
      child = waiting;
      waiting.stdout?.on("data", chunk => {
        if (stopped || session !== boundSession || child !== waiting) return;
        buffer += String(chunk);
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";
        for (const line of lines) {
          if (!line.trim()) continue;
          try {
            const item = JSON.parse(line) as Item;
            if (!seen.has(item.id)) {
              seen.add(item.id);
              after = Math.max(after, item.id);
              queue.push(item);
            }
          } catch {
            ctx.ui?.notify?.("Shephrd inbox wait printed invalid output", "warning");
          }
        }
        deliver();
      });
      waiting.on("error", () => {});
      waiting.on("close", () => {
        if (stopped || session !== boundSession || child !== waiting) return;
        child = undefined;
        retry = setTimeout(() => {
          if (!stopped && session === boundSession) watch(ctx);
        }, 1000);
      });
    };

    pi.on("session_start", async (_event, ctx) => {
      stopWatch();
      if (ctx.mode !== "tui" || deps.env.SHEPHRD_RUN_TOKEN) return;
      stopped = false;
      watch(ctx);
    });

    pi.on("message_start", async event => {
      if (current && event.message.role === "user" && messageText(event.message) === current.text) current.injected = true;
    });

    pi.on("agent_settled", async () => {
      if (!current?.injected) return;
      current = undefined;
      deliver();
    });

    pi.on("session_shutdown", async () => {
      stopWatch();
    });
  };
}

export default createShephrdInbox();
