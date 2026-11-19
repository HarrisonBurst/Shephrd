import { strict as assert } from "node:assert";
import { EventEmitter } from "node:events";
import { test } from "node:test";
import { createShephrdInbox, itemText } from "./shephrd-inbox.ts";

const item = (id: number) => ({
  id,
  task: "t_1",
  title: "Build the feature",
  task_state: "done",
  event: { seq: 10 + id, name: "task.result", data: { body: "Both parts are built." } },
  next: { read: ["shephrd", "task", "show", "t_1"], ack: ["shephrd", "inbox", "ack", String(id)] },
});

function harness(env: Record<string, string> = {}, entries: any[] = [], sessionId = "session-1") {
  const handlers: Record<string, (event: any, ctx?: any) => Promise<void>> = {};
  const tools: Record<string, any> = {};
  const sent: string[] = [];
  const spawned: string[][] = [];
  const children: Array<EventEmitter & { stdout: EventEmitter; stderr: EventEmitter; kill(): void }> = [];
  const killed: number[] = [];
  const pi = {
    on: (name: string, handler: any) => (handlers[name] = handler),
    registerTool: (tool: any) => (tools[tool.name] = tool),
    appendEntry: (customType: string, data: unknown) => entries.push({ type: "custom", customType, data }),
    sendUserMessage: (text: string) => sent.push(text),
  };
  const deps = {
    spawn: ((_cmd: string, args: string[]) => {
      spawned.push(args);
      const child = Object.assign(new EventEmitter(), {
        stdout: new EventEmitter(), stderr: new EventEmitter(), kill() { killed.push(children.indexOf(child)); },
      });
      children.push(child);
      return child;
    }) as any,
    shephrd: "shephrd",
    env,
  };
  createShephrdInbox(deps)(pi as any);
  const ctx = { mode: "tui", ui: { notify() {} }, sessionManager: { getBranch: () => entries, getSessionId: () => sessionId } };
  const arm = async () => tools.shephrd_inbox_watch.execute("call", {}, undefined, undefined, ctx);
  return { handlers, tools, entries, arm, sent, spawned, children, killed, ctx };
}

async function turn(h: ReturnType<typeof harness>, text: string) {
  await h.handlers.message_start({ message: { role: "user", content: [{ type: "text", text }] } });
  await h.handlers.agent_settled({});
}

test("includes the title, ID and exact read and ack commands", () => {
  const text = itemText(item(1));
  assert.match(text, /on Build the feature,/);
  assert.match(text, /Shephrd inbox item 1/);
  assert.match(text, /task show t_1/);
  assert.match(text, /inbox ack 1/);
});

test("falls back to the task ID when the title is empty", () => {
  assert.match(itemText({ ...item(1), title: "" }), /on t_1,/);
});

test("keeps the full bounded report body in the turn", () => {
  const body = "x".repeat(5000);
  const longItem = { ...item(1), event: { ...item(1).event, data: { body } } };
  assert.ok(itemText(longItem).includes(body));
});

test("delivers real items sequentially without automatically acknowledging", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  await h.arm();
  assert.deepEqual(h.spawned[0], ["inbox", "wait", "--after", "0"]);
  h.children[0].stdout.emit("data", JSON.stringify(item(1)) + "\n" + JSON.stringify(item(2)) + "\n");
  assert.deepEqual(h.sent, [itemText(item(1))]);
  await turn(h, h.sent[0]);
  assert.deepEqual(h.sent, [itemText(item(1)), itemText(item(2))]);
  await turn(h, h.sent[1]);
  assert.deepEqual(h.spawned, [["inbox", "wait", "--after", "0"]]);
  await h.handlers.session_shutdown({});
});

test("a completed but unhandled turn leaves its item pending", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  await h.arm();
  h.children[0].stdout.emit("data", JSON.stringify(item(1)) + "\n");
  await turn(h, h.sent[0]);
  assert.equal(h.sent.length, 1);
  await h.handlers.session_shutdown({});
});

test("an item not acknowledged, including after a failed ack, is rescanned on restart", async () => {
  const first = harness();
  await first.handlers.session_start({}, first.ctx);
  await first.arm();
  first.children[0].stdout.emit("data", JSON.stringify(item(1)) + "\n");
  await first.handlers.session_shutdown({});
  const second = harness();
  await second.handlers.session_start({}, second.ctx);
  await second.arm();
  assert.deepEqual(second.spawned[0], ["inbox", "wait", "--after", "0"]);
  second.children[0].stdout.emit("data", JSON.stringify(item(1)) + "\n");
  assert.deepEqual(second.sent, [itemText(item(1))]);
  await second.handlers.session_shutdown({});
});

test("same extension instance rescans an unacknowledged item after session switch", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  await h.arm();
  h.children[0].stdout.emit("data", JSON.stringify(item(1)) + "\n");
  await h.handlers.session_shutdown({});
  await h.handlers.session_start({ reason: "resume" }, h.ctx);
  assert.deepEqual(h.killed, [0]);
  assert.deepEqual(h.spawned[1], ["inbox", "wait", "--after", "0"]);
  h.children[1].stdout.emit("data", JSON.stringify(item(1)) + "\n");
  assert.deepEqual(h.sent, [itemText(item(1)), itemText(item(1))]);
  await h.handlers.session_shutdown({});
});

test("stale watcher output cannot enter a newly bound session", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  await h.arm();
  h.children[0].stdout.emit("data", JSON.stringify(item(1)) + "\n");
  await h.handlers.session_start({ reason: "reload" }, h.ctx);
  assert.deepEqual(h.killed, [0]);
  h.children[0].stdout.emit("data", JSON.stringify(item(2)) + "\n");
  h.children[0].emit("close", 255);
  assert.deepEqual(h.sent, [itemText(item(1))]);
  assert.deepEqual(h.spawned[1], ["inbox", "wait", "--after", "0"]);
  h.children[1].stdout.emit("data", JSON.stringify(item(1)) + "\n");
  assert.deepEqual(h.sent, [itemText(item(1)), itemText(item(1))]);
  await h.handlers.session_shutdown({});
});

test("reconnect rescans pending IDs and deduplicates within a session", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  await h.arm();
  h.children[0].stdout.emit("data", JSON.stringify(item(5)) + "\n");
  h.children[0].emit("close", 255);
  await new Promise(resolve => setTimeout(resolve, 1100));
  assert.deepEqual(h.spawned[1], ["inbox", "wait", "--after", "5"]);
  h.children[1].stdout.emit("data", JSON.stringify(item(5)) + "\n" + JSON.stringify(item(6)) + "\n");
  assert.equal(h.sent.length, 1);
  await turn(h, h.sent[0]);
  assert.equal(h.sent.length, 2);
  await h.handlers.session_shutdown({});
});

test("idle and reconnect do not inject empty turns", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  await h.arm();
  h.children[0].stdout.emit("data", "\n  \n");
  h.children[0].emit("close", 255);
  await new Promise(resolve => setTimeout(resolve, 1100));
  assert.deepEqual(h.sent, []);
  await h.handlers.session_shutdown({});
});

test("an unmarked Pi session stays unwatched until the tool is called", async () => {
  const h = harness();
  await h.handlers.session_start({ reason: "startup" }, h.ctx);
  assert.equal(h.spawned.length, 0);
  assert.deepEqual(h.entries, []);
  await h.arm();
  assert.deepEqual(h.spawned, [["inbox", "wait", "--after", "0"]]);
  assert.deepEqual(h.entries, [{ type: "custom", customType: "shephrd-inbox-watch", data: { sessionId: "session-1" } }]);
  await h.handlers.session_shutdown({});
});

test("double invocation stays on without restarting or adding markers", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  await h.arm();
  const result = await h.arm();
  assert.match(result.content[0].text, /already active/);
  assert.equal(h.spawned.length, 1);
  assert.equal(h.entries.length, 1);
  await h.handlers.session_shutdown({});
});

test("a marked session re-arms on reload and resume, but a new session does not", async () => {
  const first = harness();
  await first.handlers.session_start({}, first.ctx);
  await first.arm();
  await first.handlers.session_shutdown({});
  const resumed = harness({}, first.entries);
  await resumed.handlers.session_start({ reason: "resume" }, resumed.ctx);
  assert.deepEqual(resumed.spawned, [["inbox", "wait", "--after", "0"]]);
  await resumed.handlers.session_shutdown({});
  const fork = harness({}, first.entries, "session-2");
  await fork.handlers.session_start({ reason: "fork" }, fork.ctx);
  assert.equal(fork.spawned.length, 0);
  const fresh = harness();
  await fresh.handlers.session_start({ reason: "new" }, fresh.ctx);
  assert.equal(fresh.spawned.length, 0);
});

test("print mode refuses the tool and does not mark the session", async () => {
  const h = harness();
  const ctx = { ...h.ctx, mode: "print" };
  await h.handlers.session_start({}, ctx);
  const result = await h.tools.shephrd_inbox_watch.execute("call", {}, undefined, undefined, ctx);
  assert.match(result.content[0].text, /unavailable/);
  assert.equal(h.spawned.length, 0);
  assert.deepEqual(h.entries, []);
});

test("a Shephrd run has no tool and never watches even with a marker", async () => {
  const h = harness({ SHEPHRD_RUN_TOKEN: "token" }, [{ type: "custom", customType: "shephrd-inbox-watch", data: { sessionId: "session-1" } }]);
  assert.deepEqual(h.tools, {});
  await h.handlers.session_start({}, h.ctx);
  assert.equal(h.spawned.length, 0);
});
