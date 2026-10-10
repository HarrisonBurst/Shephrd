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

function harness(entries: unknown[] = [], env: Record<string, string> = {}) {
  const handlers: Record<string, (event: any, ctx?: any) => Promise<void>> = {};
  const sent: string[] = [];
  const appended: unknown[] = [];
  const acks: string[][] = [];
  const spawned: string[][] = [];
  const child = Object.assign(new EventEmitter(), { stdout: new EventEmitter(), stderr: new EventEmitter(), kill() {} });
  const pi = {
    on: (name: string, handler: any) => (handlers[name] = handler),
    sendUserMessage: (text: string) => sent.push(text),
    appendEntry: (_type: string, data: unknown) => appended.push(data),
  };
  const deps = {
    spawn: ((_cmd: string, args: string[]) => (spawned.push(args), child)) as any,
    execFile: ((_cmd: string, args: string[], done: () => void) => (acks.push(args), done())) as any,
    shephrd: "shephrd",
    env,
  };
  createShephrdInbox(deps)(pi as any);
  const ctx = { mode: "tui", sessionManager: { getBranch: () => entries }, ui: { notify() {} } };
  return { handlers, sent, appended, acks, spawned, child, ctx };
}

async function turn(h: ReturnType<typeof harness>, text: string, stopReason: string) {
  await h.handlers.message_start({ message: { role: "user", content: [{ type: "text", text }] } });
  await h.handlers.message_end({ message: { role: "assistant", stopReason } });
  await h.handlers.agent_settled({});
}

test("delivers each item as a turn and acknowledges it once handled", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  assert.deepEqual(h.spawned[0], ["inbox", "wait", "--after", "0"]);
  h.child.stdout.emit("data", JSON.stringify(item(1)) + "\n" + JSON.stringify(item(2)) + "\n");
  assert.equal(h.sent.length, 1);
  assert.equal(h.sent[0], itemText(item(1)));
  await turn(h, h.sent[0], "stop");
  assert.deepEqual(h.acks, [["inbox", "ack", "1"]]);
  assert.deepEqual(h.appended, [{ item: 1 }]);
  assert.equal(h.sent.length, 2);
  await turn(h, h.sent[1], "stop");
  assert.deepEqual(h.acks.at(-1), ["inbox", "ack", "2"]);
});

test("an aborted turn leaves its item pending", async () => {
  const h = harness();
  await h.handlers.session_start({}, h.ctx);
  h.child.stdout.emit("data", JSON.stringify(item(1)) + "\n");
  await turn(h, h.sent[0], "aborted");
  assert.deepEqual(h.acks, []);
  assert.deepEqual(h.appended, []);
});

test("a restarted session resumes after the last acknowledged item", async () => {
  const h = harness([{ type: "custom", customType: "shephrd-inbox", data: { item: 7 } }]);
  await h.handlers.session_start({}, h.ctx);
  assert.deepEqual(h.spawned[0], ["inbox", "wait", "--after", "7"]);
  h.child.stdout.emit("data", JSON.stringify(item(5)) + "\n");
  assert.equal(h.sent.length, 0);
});

test("print mode does not hold the inbox", async () => {
  const h = harness();
  await h.handlers.session_start({}, { ...h.ctx, mode: "print" });
  assert.equal(h.spawned.length, 0);
});

test("a Shephrd session does not hold the inbox", async () => {
  const h = harness([], { SHEPHRD_RUN_TOKEN: "token" });
  await h.handlers.session_start({}, h.ctx);
  assert.equal(h.spawned.length, 0);
});
