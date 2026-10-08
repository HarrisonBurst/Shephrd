import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const { session, mode, headless, envelopes, corrected, correction } = JSON.parse(readFileSync(0, "utf8"));
const handlers = new Map();
let shutdown = false;
let repairs = 0;
let pending = false;
const context = {
  mode: "tui",
  sessionManager: { getSessionId: () => session },
  isIdle: () => !pending,
  hasPendingMessages: () => pending,
  shutdown() { shutdown = true; },
};
if (!headless) {
  const { default: bridge } = await import(process.argv[2]);
  bridge({
    on(name, handler) { handlers.set(name, handler); },
    sendMessage(message, options) {
      assert.match(message.content, /SHEPHRD_PROTOCOL_REPAIR/);
      assert.equal(options.deliverAs, "followUp");
      repairs++;
      pending = true;
    },
  });
}
const emit = async (type, fields = {}) => {
  if (headless) console.log(JSON.stringify({ type, ...fields }));
  else await handlers.get(type)?.(fields, context);
};
const message = async (text, stopReason = "stop", errorMessage) => {
  await emit("message_end", { message: { role: "assistant", content: [{ type: "text", text }], stopReason, ...(errorMessage ? { errorMessage } : {}) } });
};
try {
  await emit(headless ? "session" : "session_start", { id: session });
  await emit("agent_start");
  if (mode.startsWith("repair-transport") && !correction) {
    await message(envelopes);
    if (headless) process.exit(0);
    assert.equal(repairs, 1);
    assert.equal((await handlers.get("tool_call")({}, context)).block, true);
    pending = false;
    await emit("agent_start");
  }
  if (!correction && !mode.startsWith("repair-transport")) {
    await message('<shephrd-event>{"type":"progress","payload":"Completed fixture action"}</shephrd-event>', "toolUse");
    await emit("message_end", { message: { role: "toolResult", toolCallId: "once", toolName: "fixture", content: [{ type: "text", text: "Completed once" }], isError: false } });
  }
  const result = mode.startsWith("repair-transport") ? corrected : envelopes;
  const failedText = mode.includes("empty") ? "" : mode.includes("partial") ? '<shephrd-event>{"type":"checkpoint"' : result.replace(/"(payload|summary)":\s*"[^"]*"/g, '"$1":"FAILED TRANSPORT"');
  const error = mode.includes("auth") ? "401 Unauthorized" : mode.includes("schema") ? "Invalid schema for response_format" : "WebSocket error";
  const aborted = mode.includes("abort");
  if (!mode.includes("normal")) {
    await emit("message_update", { assistantMessageEvent: { type: "text_delta", contentIndex: 0, delta: failedText } });
    await message(failedText, aborted ? "aborted" : "error", aborted ? "Request aborted" : error);
    await emit("agent_end", { willRetry: !aborted && !mode.includes("auth") && !mode.includes("schema") });
    if (shutdown) throw new Error("bridge shut down before Pi retry/settlement");
    if (!headless) assert.equal(repairs, mode.startsWith("repair-transport") ? 1 : 0);
    if (mode.includes("exhausted")) {
      for (let attempt = 1; attempt <= 3; attempt++) {
        await emit("auto_retry_start", { attempt, maxAttempts: 3, delayMs: 2000 * 2 ** (attempt - 1), errorMessage: error });
        await emit("agent_start");
        await message(failedText, "error", error);
        await emit("agent_end", { willRetry: attempt < 3 });
      }
      await emit("auto_retry_end", { success: false, attempt: 3, finalError: error });
    } else if (mode.includes("cancel")) {
      await emit("auto_retry_start", { attempt: 1, maxAttempts: 3, delayMs: 2000, errorMessage: error });
      await emit("auto_retry_end", { success: false, attempt: 1, finalError: "Retry cancelled" });
    } else if (!aborted && !mode.includes("auth") && !mode.includes("schema")) {
      await emit("auto_retry_start", { attempt: 1, maxAttempts: 3, delayMs: 2000, errorMessage: error });
      await emit("agent_start");
      await message(result);
      await emit("auto_retry_end", { success: true, attempt: 1 });
      await emit("agent_end", { willRetry: false });
    }
  } else {
    await message(envelopes);
    await emit("agent_end", { willRetry: false });
  }
  await emit("agent_settled");
} finally {
  await handlers.get("session_shutdown")?.({}, context);
}
process.exit(0);
