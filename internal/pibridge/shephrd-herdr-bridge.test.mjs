import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import bridgeExtension, { extractShephrdEnvelopes, isValidTerminalEvent, parseBridgeEvent, subdriverSession } from "./shephrd-herdr-bridge.ts";

const checkpoint = `<shephrd-event>{"type":"checkpoint","payload":"progress","checkpoint":{"schema_version":1,"summary":"progress","completed":[],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`;
const done = `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:shephrd/task"}</shephrd-event>`;
const framingOversized = `<shephrd-event>${JSON.stringify({ type: "progress", payload: '"'.repeat(60 * 1024) })}</shephrd-event>`;

test("sub-driver environment aliases require one complete matching bridge identity", () => {
  const bridge = { SHEPHRD_BRIDGE_ATTEMPT_ID: "coord_one", SHEPHRD_BRIDGE_RUN_GENERATION: "3" };
  const current = { SHEPHRD_SUBDRIVER_ID: "coord_one", SHEPHRD_SUBDRIVER_GENERATION: "3", SHEPHRD_SUBDRIVER_TOKEN: "session_one" };
  const legacy = { SHEPHRD_COORDINATOR_ID: "coord_one", SHEPHRD_COORDINATOR_GENERATION: "3", SHEPHRD_COORDINATOR_TOKEN: "session_one" };
  assert.equal(subdriverSession(bridge), false);
  for (const identity of [current, legacy, { ...current, ...legacy }]) {
    assert.equal(subdriverSession({ ...bridge, ...identity }), true);
    assert.throws(() => subdriverSession({ ...bridge, ...identity, SHEPHRD_WORKER: "1" }), /Invalid/);
    assert.throws(() => subdriverSession({ ...bridge, ...identity, SHEPHRD_BRIDGE_RUN_GENERATION: "4" }), /conflicts/);
    assert.throws(() => subdriverSession({ ...bridge, ...identity, SHEPHRD_BRIDGE_ATTEMPT_ID: "other" }), /conflicts/);
  }
  for (const suffix of ["ID", "GENERATION", "TOKEN"]) {
    for (const value of ["", "other"]) {
      assert.throws(() => subdriverSession({ ...bridge, ...current, ...legacy, [`SHEPHRD_COORDINATOR_${suffix}`]: value }), /Conflicting/);
    }
    assert.throws(() => subdriverSession({ ...bridge, ...current, [`SHEPHRD_SUBDRIVER_${suffix}`]: "" }), /Invalid/);
    assert.throws(() => subdriverSession({ ...bridge, ...legacy, [`SHEPHRD_COORDINATOR_${suffix}`]: "" }), /Invalid/);
  }
});

test("extracts only exact bounded Shephrd envelopes", () => {
  const secret = "ordinary assistant secret";
  const value = extractShephrdEnvelopes(`${secret}\n${checkpoint}\n${done}`);
  assert.deepEqual(value, { envelopes: [checkpoint, done], valid: true });
  assert.equal(value.envelopes.join("\n").includes(secret), false);
  assert.equal(extractShephrdEnvelopes(`${checkpoint}\n<shephrd-event>{}`).valid, false);
});

test("recognizes only strict terminal envelopes for shutdown", () => {
  assert.equal(isValidTerminalEvent(parseBridgeEvent(done)), true);
  assert.equal(isValidTerminalEvent(parseBridgeEvent(checkpoint)), false);
  const malformed = `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:x","extra":true}</shephrd-event>`;
  assert.equal(isValidTerminalEvent(parseBridgeEvent(malformed)), false);
});

test("frames session and structured events and shuts down after terminal", async () => {
  const capture = await openCapture();
  const old = bridgeEnvironment(capture.path);
  try {
    const handlers = new Map();
    const pi = { on(name, handler) { handlers.set(name, handler); } };
    bridgeExtension(pi);
    let shutdowns = 0;
    const context = {
      mode: "tui",
      sessionManager: { getSessionId: () => "native-session" },
      shutdown() { shutdowns++; },
    };
    await handlers.get("session_start")({}, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: "prompt thinking tool arguments results paths and errors" }] } }, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: checkpoint }] } }, context);
    assert.equal(shutdowns, 0);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: done }] } }, context);
    assert.equal(shutdowns, 1);
    await handlers.get("session_shutdown")();
    await capture.closed;
    const frames = capture.lines.map((line) => JSON.parse(line));
    assert.deepEqual(frames.map((frame) => frame.kind), ["session", "event_candidate", "event_candidate"]);
    assert.deepEqual(frames.map((frame) => frame.seq), [1, 2, 3]);
    assert.equal(frames[0].session_id, "native-session");
    assert.equal(JSON.stringify(frames).includes("prompt thinking"), false);
  } finally {
    restoreEnvironment(old);
    await capture.close();
  }
});

test("one authoritative repair response queues a tool-free correction turn", async () => {
  const malformed = `<shephrd-event>{"type":"question","payload":"SECRET malformed question","question":{"options":["A","B"]}}</shephrd-event>`;
  const corrected = checkpoint + `<shephrd-event>{"type":"question","payload":"Choose A or B. Options: A, B. Recommendation: A."}</shephrd-event>`;
  const capture = await openCapture((frame) => {
    if (frame.kind === "session") return { result: "accepted_nonterminal" };
    if (frame.kind === "event_candidate" && frame.envelope === `${malformed}\n${done}`) {
      return {
        result: "repair",
        repair_id: "repair_exact",
        diagnostic: { code: "EVENT_SCHEMA_UNKNOWN_FIELD", phase: "adapter", field: "question", message: "unknown top-level field", requires_checkpoint: false },
      };
    }
    if (frame.kind === "event_candidate" && frame.envelope === corrected) return { result: "accepted_terminal" };
    return { result: "accepted_nonterminal" };
  });
  const old = bridgeEnvironment(capture.path);
  try {
    const handlers = new Map();
    const messages = [];
    const pi = {
      on(name, handler) { handlers.set(name, handler); },
      sendMessage(message, options) { messages.push({ message, options }); },
    };
    bridgeExtension(pi);
    let shutdowns = 0;
    const context = {
      mode: "tui",
      sessionManager: { getSessionId: () => "native-session" },
      hasPendingMessages: () => true,
      shutdown() { shutdowns++; },
    };
    await handlers.get("session_start")({}, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: checkpoint }] } }, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: `${malformed}\n${done}` }] } }, context);
    assert.equal(messages.length, 1);
    assert.equal(messages[0].options.deliverAs, "followUp");
    assert.equal(messages[0].options.triggerTurn, true);
    assert.match(messages[0].message.content, /EVENT_SCHEMA_UNKNOWN_FIELD/);
    assert.match(messages[0].message.content, /Diagnostic: unknown top-level field/);
    assert.match(messages[0].message.content, /one assistant message/);
    assert.match(messages[0].message.content, /objects/);
    assert.match(messages[0].message.content, /Allowed terminal fields: type, payload, artifact/);
    assert.match(messages[0].message.content, /put the complete prompt, options, consequences, and recommendation in payload/);
    assert.equal(messages[0].message.content.includes("SECRET"), false);
    assert.deepEqual(await handlers.get("tool_call")({}, context), { block: true, reason: "Shephrd protocol correction turn cannot run tools" });
    assert.equal(shutdowns, 0);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: corrected }] } }, context);
    assert.equal(shutdowns, 1);
    await handlers.get("session_shutdown")();
    await capture.closed;
    const frames = capture.lines.map((line) => JSON.parse(line));
    assert.deepEqual(frames.map((frame) => frame.kind), ["session", "event_candidate", "event_candidate", "event_candidate"]);
    assert.deepEqual(frames.map((frame) => frame.seq), [1, 2, 3, 4]);
    assert.equal(frames.some((frame) => frame.envelope === done), false);
  } finally {
    restoreEnvironment(old);
    await capture.close();
  }
});

test("compound output is sent intact once, including duplicate terminals", async () => {
  const batch = `${checkpoint}\n${done}\n${done}`;
  const capture = await openCapture((frame) => ({ result: frame.kind === "session" ? "accepted_nonterminal" : "fatal" }));
  const old = bridgeEnvironment(capture.path);
  try {
    const handlers = new Map();
    bridgeExtension({ on(name, handler) { handlers.set(name, handler); } });
    let shutdowns = 0;
    const context = { mode: "tui", sessionManager: { getSessionId: () => "native-session" }, shutdown() { shutdowns++; } };
    await handlers.get("session_start")({}, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: checkpoint }, { type: "text", text: `${done}\n${done}` }] } }, context);
    await handlers.get("session_shutdown")();
    await capture.closed;
    const frames = capture.lines.map(JSON.parse);
    assert.equal(frames.length, 2);
    assert.equal(frames[1].envelope, batch);
    assert.equal(shutdowns, 1);
  } finally {
    restoreEnvironment(old);
    await capture.close();
  }
});

test("mismatched response identity fails closed", async () => {
  const capture = await openCapture(() => ({ result: "accepted_nonterminal", token: "wrong" }));
  const old = bridgeEnvironment(capture.path);
  try {
    const handlers = new Map();
    const pi = { on(name, handler) { handlers.set(name, handler); } };
    bridgeExtension(pi);
    let shutdowns = 0;
    const context = {
      mode: "tui",
      sessionManager: { getSessionId: () => "native-session" },
      shutdown() { shutdowns++; },
    };
    await handlers.get("session_start")({}, context);
    assert.equal(shutdowns, 1);
    await handlers.get("session_shutdown")();
    await capture.closed;
    assert.deepEqual(capture.lines.map((line) => JSON.parse(line).kind), ["session"]);
  } finally {
    restoreEnvironment(old);
    await capture.close();
  }
});

test("accepted terminal output ignores later oversized output and repeats shutdown when settled", async () => {
  const capture = await openCapture();
  const old = bridgeEnvironment(capture.path);
  try {
    const handlers = new Map();
    const pi = { on(name, handler) { handlers.set(name, handler); } };
    bridgeExtension(pi);
    let shutdowns = 0;
    const context = {
      mode: "tui",
      sessionManager: { getSessionId: () => "native-session" },
      shutdown() { shutdowns++; },
    };
    await handlers.get("session_start")({}, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: done }] } }, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: framingOversized }] } }, context);
    await handlers.get("agent_settled")({}, context);
    await handlers.get("session_shutdown")();
    await capture.closed;
    const frames = capture.lines.map((line) => JSON.parse(line));
    assert.deepEqual(frames.map((frame) => frame.kind), ["session", "event_candidate"]);
    assert.equal(shutdowns, 2);
  } finally {
    restoreEnvironment(old);
    await capture.close();
  }
});

test("settlement and malformed or oversized output fail closed before terminal", async () => {
  const capture = await openCapture();
  const old = bridgeEnvironment(capture.path);
  try {
    const handlers = new Map();
    const pi = { on(name, handler) { handlers.set(name, handler); } };
    bridgeExtension(pi);
    let shutdowns = 0;
    const context = {
      mode: "tui",
      sessionManager: { getSessionId: () => "native-session" },
      shutdown() { shutdowns++; },
    };
    await handlers.get("session_start")({}, context);
    const malformed = `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:x","extra":true}</shephrd-event>`;
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: malformed }] } }, context);
    await handlers.get("message_end")({ message: { role: "assistant", stopReason: "stop", content: [{ type: "text", text: framingOversized }] } }, context);
    await handlers.get("agent_settled")({}, context);
    await handlers.get("session_shutdown")();
    await capture.closed;
    const frames = capture.lines.map((line) => JSON.parse(line));
    assert.deepEqual(frames.map((frame) => frame.kind), ["session", "event_candidate"]);
    assert.deepEqual(frames.map((frame) => frame.seq), [1, 2]);
    assert.equal(shutdowns, 2);
  } finally {
    restoreEnvironment(old);
    await capture.close();
  }
});

test("only successful assistant completions publish before final settlement", async () => {
  for (const stopReason of ["error", "aborted", "pending", undefined, "unknown"]) {
    for (const text of ["", '<shephrd-event>{"type":"checkpoint"', `${checkpoint}\n${done}`, framingOversized]) {
      const capture = await openCapture();
      const old = bridgeEnvironment(capture.path);
      try {
        const handlers = new Map();
        bridgeExtension({ on(name, handler) { handlers.set(name, handler); } });
        let shutdowns = 0;
        const context = { mode: "tui", sessionManager: { getSessionId: () => "native-session" }, shutdown() { shutdowns++; } };
        await handlers.get("session_start")({}, context);
        await handlers.get("message_end")({ message: { role: "assistant", stopReason, content: [{ type: "text", text }] } }, context);
        await handlers.get("agent_end")?.({}, context);
        assert.equal(shutdowns, 0);
        assert.equal(capture.lines.length, 1);
        assert.equal(await handlers.get("tool_call")({}, context), undefined);
        assert.equal(handlers.has("auto_retry_start"), false);
        assert.equal(handlers.has("auto_retry_end"), false);
        await handlers.get("agent_settled")({}, context);
        assert.equal(shutdowns, 1);
        assert.deepEqual(capture.lines.map((line) => JSON.parse(line).kind), ["session", "settled"]);
        await handlers.get("session_shutdown")();
      } finally {
        restoreEnvironment(old);
        await capture.close();
      }
    }
  }
});

test("successful tool and deferred completions preserve text block shapes", async () => {
  const capture = await openCapture();
  const old = bridgeEnvironment(capture.path);
  try {
    const handlers = new Map();
    bridgeExtension({ on(name, handler) { handlers.set(name, handler); } });
    const context = { mode: "tui", sessionManager: { getSessionId: () => "native-session" }, shutdown() {} };
    await handlers.get("session_start")({}, context);
    for (const stopReason of ["stop", "length", "toolUse", "deferred"]) {
      await handlers.get("message_end")({ message: { role: "assistant", stopReason, content: [{ type: "thinking", thinking: "private" }, { type: "text", text: checkpoint }, { type: "toolCall", id: "once", name: "counter", arguments: {} }] } }, context);
      assert.equal(await handlers.get("tool_call")({}, context), undefined);
    }
    await handlers.get("session_shutdown")();
    assert.deepEqual(capture.lines.map((line) => JSON.parse(line).envelope).slice(1), Array(4).fill(checkpoint));
  } finally {
    restoreEnvironment(old);
    await capture.close();
  }
});

async function openCapture(responder = defaultResponse) {
  const dir = await mkdtemp(join(tmpdir(), "shephrd-extension-test-"));
  const path = join(dir, "bridge.sock");
  const lines = [];
  let buffer = "";
  let resolveClosed;
  const closed = new Promise((resolve) => { resolveClosed = resolve; });
  const server = createServer((socket) => {
    socket.on("data", (chunk) => {
      buffer += String(chunk);
      const split = buffer.split("\n");
      buffer = split.pop() || "";
      for (const line of split.filter(Boolean)) {
        lines.push(line);
        const frame = JSON.parse(line);
        const response = responder(frame);
        if (response) socket.write(JSON.stringify({ schema_version: 2, token: frame.token, attempt_id: frame.attempt_id, run_generation: frame.run_generation, ack_seq: frame.seq, ...response }) + "\n");
      }
    });
    socket.on("end", resolveClosed);
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(path, resolve);
  });
  return {
    path,
    lines,
    closed,
    async close() {
      await new Promise((resolve) => server.close(resolve));
      await rm(dir, { recursive: true, force: true });
    },
  };
}

function defaultResponse(frame) {
  if (frame.kind === "session") return { result: "accepted_nonterminal" };
  if (frame.kind !== "event_candidate") return { result: "fatal" };
  return { result: isValidTerminalEvent(parseBridgeEvent(frame.envelope)) ? "accepted_terminal" : "accepted_nonterminal" };
}

function bridgeEnvironment(path) {
  const keys = ["SHEPHRD_BRIDGE_SOCKET", "SHEPHRD_BRIDGE_TOKEN", "SHEPHRD_BRIDGE_ATTEMPT_ID", "SHEPHRD_BRIDGE_RUN_GENERATION"];
  const old = Object.fromEntries(keys.map((key) => [key, process.env[key]]));
  process.env.SHEPHRD_BRIDGE_SOCKET = path;
  process.env.SHEPHRD_BRIDGE_TOKEN = "token";
  process.env.SHEPHRD_BRIDGE_ATTEMPT_ID = "attempt_exact";
  process.env.SHEPHRD_BRIDGE_RUN_GENERATION = "7";
  return old;
}

function restoreEnvironment(old) {
  for (const [key, value] of Object.entries(old)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
}
