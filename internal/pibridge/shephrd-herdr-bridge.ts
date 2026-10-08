import { createConnection, type Socket } from "node:net";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const openTag = "<shephrd-event>";
const closeTag = "</shephrd-event>";
const schemaVersion = 2;
const maxFrameBytes = 96 * 1024;
const responseTimeoutMilliseconds = 5_000;
const terminalTypes = new Set(["question", "done", "blocked", "failed"]);
const eventTypes = new Set(["progress", "checkpoint", ...terminalTypes]);
const responseResults = new Set(["accepted_nonterminal", "accepted_terminal", "repair", "fatal"]);
const repairCodes = new Set([
  "EVENT_FRAMING_MISSING_CLOSE",
  "EVENT_JSON_SYNTAX",
  "EVENT_JSON_TRAILING",
  "EVENT_SCHEMA_UNKNOWN_FIELD",
  "EVENT_SCHEMA_TYPE_MISMATCH",
  "EVENT_SEMANTIC_INVALID",
  "CHECKPOINT_REQUIRED",
  "CHECKPOINT_NOT_CURRENT",
  "CHECKPOINT_NEXT_STEPS_REQUIRED",
  "DONE_ARTIFACT_CONTRACT_MISMATCH",
  "DONE_BRANCH_ARTIFACT_MISMATCH",
]);

type BridgeFrame = {
  schema_version: 2;
  token: string;
  attempt_id: string;
  run_generation: number;
  seq: number;
  kind: "session" | "event_candidate" | "settled" | "invalid" | "repair_exhausted";
  session_id?: string;
  envelope?: string;
};

type BridgeDiagnostic = {
  code: string;
  phase: string;
  field?: string;
  offset?: number;
  message: string;
  requires_checkpoint: boolean;
};

type BridgeResponse = {
  schema_version: 2;
  token: string;
  attempt_id: string;
  run_generation: number;
  ack_seq: number;
  result: "accepted_nonterminal" | "accepted_terminal" | "repair" | "fatal";
  repair_id?: string;
  diagnostic?: BridgeDiagnostic;
};

type BridgeEvent = {
  type?: unknown;
  payload?: unknown;
  artifact?: unknown;
  checkpoint?: unknown;
};

type PendingResponse = {
  resolve: (response: BridgeResponse) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
};

export function extractShephrdEnvelopes(text: string): { envelopes: string[]; valid: boolean } {
  const envelopes: string[] = [];
  let offset = 0;
  while (true) {
    const start = text.indexOf(openTag, offset);
    if (start < 0) return { envelopes, valid: true };
    const end = text.indexOf(closeTag, start + openTag.length);
    if (end < 0) return { envelopes: [], valid: false };
    const envelope = text.slice(start, end + closeTag.length);
    if (Buffer.byteLength(envelope) > maxFrameBytes) return { envelopes: [], valid: false };
    envelopes.push(envelope);
    offset = end + closeTag.length;
  }
}

export function parseBridgeEvent(envelope: string): BridgeEvent | undefined {
  if (!envelope.startsWith(openTag) || !envelope.endsWith(closeTag)) return undefined;
  try {
    const value = JSON.parse(envelope.slice(openTag.length, -closeTag.length));
    if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
    const event = value as BridgeEvent;
    if (typeof event.type !== "string" || !eventTypes.has(event.type)) return undefined;
    return event;
  } catch {
    return undefined;
  }
}

export function isValidTerminalEvent(event: BridgeEvent | undefined): boolean {
  if (!event || typeof event.type !== "string" || !terminalTypes.has(event.type)) return false;
  const keys = Object.keys(event);
  if (keys.some((key) => !["type", "payload", "artifact"].includes(key))) return false;
  if (typeof event.payload !== "string" || !event.payload.trim() || event.payload.includes("\0") || Buffer.byteLength(event.payload) > 16 * 1024) return false;
  if (event.type === "done" && (typeof event.artifact !== "string" || !event.artifact.trim())) return false;
  return event.artifact === undefined || typeof event.artifact === "string";
}

function assistantTexts(message: unknown): string[] {
  if (!message || typeof message !== "object" || (message as { role?: unknown }).role !== "assistant") return [];
  const content = (message as { content?: unknown }).content;
  if (!Array.isArray(content)) return [];
  return content
    .filter((part): part is { type: "text"; text: string } => Boolean(part) && typeof part === "object" && (part as { type?: unknown }).type === "text" && typeof (part as { text?: unknown }).text === "string")
    .map((part) => part.text);
}

function hasExactKeys(value: Record<string, unknown>, allowed: string[]): boolean {
  return Object.keys(value).every((key) => allowed.includes(key));
}

function parseResponse(line: string, token: string, attemptID: string, runGeneration: number): BridgeResponse | undefined {
  let raw: unknown;
  try {
    raw = JSON.parse(line);
  } catch {
    return undefined;
  }
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const value = raw as Record<string, unknown>;
  if (!hasExactKeys(value, ["schema_version", "token", "attempt_id", "run_generation", "ack_seq", "result", "repair_id", "diagnostic"])) return undefined;
  if (value.schema_version !== schemaVersion || value.token !== token || value.attempt_id !== attemptID || value.run_generation !== runGeneration) return undefined;
  if (!Number.isSafeInteger(value.ack_seq) || (value.ack_seq as number) < 1 || typeof value.result !== "string" || !responseResults.has(value.result)) return undefined;
  if (value.result !== "repair") {
    if (value.repair_id !== undefined || value.diagnostic !== undefined) return undefined;
    return value as BridgeResponse;
  }
  if (typeof value.repair_id !== "string" || !value.repair_id.startsWith("repair_") || Buffer.byteLength(value.repair_id) > 128 || !value.diagnostic || typeof value.diagnostic !== "object" || Array.isArray(value.diagnostic)) return undefined;
  const diagnostic = value.diagnostic as Record<string, unknown>;
  if (!hasExactKeys(diagnostic, ["code", "phase", "field", "offset", "message", "requires_checkpoint"])) return undefined;
  if (typeof diagnostic.code !== "string" || !repairCodes.has(diagnostic.code) || typeof diagnostic.phase !== "string" || Buffer.byteLength(diagnostic.phase) > 32 || typeof diagnostic.message !== "string" || Buffer.byteLength(diagnostic.message) > 512 || typeof diagnostic.requires_checkpoint !== "boolean") return undefined;
  if (diagnostic.field !== undefined && (typeof diagnostic.field !== "string" || Buffer.byteLength(diagnostic.field) > 128)) return undefined;
  if (diagnostic.offset !== undefined && (!Number.isSafeInteger(diagnostic.offset) || (diagnostic.offset as number) < 1)) return undefined;
  return value as BridgeResponse;
}

export function subdriverSession(env: NodeJS.ProcessEnv): boolean {
  const current = [env.SHEPHRD_SUBDRIVER_ID || "", env.SHEPHRD_SUBDRIVER_GENERATION || "", env.SHEPHRD_SUBDRIVER_TOKEN || ""];
  const legacy = [env.SHEPHRD_COORDINATOR_ID || "", env.SHEPHRD_COORDINATOR_GENERATION || "", env.SHEPHRD_COORDINATOR_TOKEN || ""];
  if (current.some(Boolean) && legacy.some(Boolean) && current.some((value, i) => value !== legacy[i])) throw new Error("Conflicting sub-driver environment identities");
  const identity = current.some(Boolean) ? current : legacy;
  if (!identity.some(Boolean)) return false;
  const generation = Number(identity[1]);
  if (!identity.every(Boolean) || !Number.isSafeInteger(generation) || generation < 1 || env.SHEPHRD_WORKER === "1") throw new Error("Invalid sub-driver environment identity");
  if (identity[0] !== env.SHEPHRD_BRIDGE_ATTEMPT_ID || generation !== Number(env.SHEPHRD_BRIDGE_RUN_GENERATION)) throw new Error("Sub-driver identity conflicts with bridge session");
  return true;
}

function repairPrompt(response: BridgeResponse, subdriver: boolean): string {
  const diagnostic = response.diagnostic!;
  const lines = [
    "SHEPHRD_PROTOCOL_REPAIR",
    "The prior Shephrd event candidate was rejected and was not accepted.",
    `Code: ${diagnostic.code}`,
    `Diagnostic: ${diagnostic.message}`,
    `Repair ID: ${response.repair_id}`,
  ];
  if (diagnostic.field) lines.push(`Rejected field: ${diagnostic.field}`);
  lines.push("Do not use tools, modify files, rerun checks, or repeat project work.");
  lines.push("Emit exactly one valid current checkpoint followed by one corrected terminal envelope in one assistant message, with no prose. Preserve the report's facts; do not drop rejected fields.");
  lines.push('decisions entries must be objects {"decision":"choice made","reason":"why"}; checks entries must be objects {"command":"test command","result":"observed result"}, never strings.');
  if (subdriver) {
    lines.push("Sub-driver session terminal must be done without artifact. Do not dispatch, return, acknowledge, or repeat previous actions.");
  }
  lines.push("Allowed terminal fields: type, payload, artifact.");
  lines.push("Allowed checkpoint fields: schema_version, summary, completed, next_steps, decisions, changed_paths, checks, blockers.");
  if (diagnostic.code === "EVENT_SCHEMA_UNKNOWN_FIELD" && diagnostic.field === "question") {
    lines.push("For a question, put the complete prompt, options, consequences, and recommendation in payload.");
  }
  lines.push("Deadline: 120 seconds. This is correction attempt 1 of 1.");
  return lines.join("\n");
}

export default function (pi: ExtensionAPI) {
  const socketPath = process.env.SHEPHRD_BRIDGE_SOCKET || "";
  const token = process.env.SHEPHRD_BRIDGE_TOKEN || "";
  const attemptID = process.env.SHEPHRD_BRIDGE_ATTEMPT_ID || "";
  const runGeneration = Number(process.env.SHEPHRD_BRIDGE_RUN_GENERATION || "0");
  let socket: Socket | undefined;
  let connection: Promise<Socket> | undefined;
  let responseBuffer = "";
  let seq = 0;
  let started = false;
  let subdriver = false;
  let terminal = false;
  let repairPending = false;
  let bridgeFailed = false;
  const pending = new Map<number, PendingResponse>();

  const failPending = (error: Error) => {
    if (bridgeFailed) return;
    bridgeFailed = true;
    for (const entry of pending.values()) {
      clearTimeout(entry.timer);
      entry.reject(error);
    }
    pending.clear();
  };

  const consumeResponses = (chunk: Buffer) => {
    responseBuffer += chunk.toString();
    if (Buffer.byteLength(responseBuffer) >= maxFrameBytes) {
      failPending(new Error("oversized Shephrd bridge response"));
      socket?.destroy();
      return;
    }
    const lines = responseBuffer.split("\n");
    responseBuffer = lines.pop() || "";
    for (const line of lines) {
      if (!line) continue;
      const response = parseResponse(line, token, attemptID, runGeneration);
      const entry = response ? pending.get(response.ack_seq) : undefined;
      if (!response || !entry) {
        failPending(new Error("invalid or uncorrelated Shephrd bridge response"));
        socket?.destroy();
        return;
      }
      pending.delete(response.ack_seq);
      clearTimeout(entry.timer);
      entry.resolve(response);
    }
  };

  const connect = () => {
    if (connection) return connection;
    connection = new Promise<Socket>((resolve, reject) => {
      if (!socketPath || !token || !attemptID || !Number.isSafeInteger(runGeneration) || runGeneration < 1) {
        reject(new Error("invalid Shephrd bridge environment"));
        return;
      }
      const candidate = createConnection(socketPath);
      const fail = (error: Error) => reject(error);
      candidate.once("error", fail);
      candidate.once("connect", () => {
        candidate.off("error", fail);
        candidate.on("data", consumeResponses);
        candidate.on("error", (error) => failPending(error));
        candidate.on("end", () => failPending(new Error("Shephrd bridge closed")));
        socket = candidate;
        resolve(candidate);
      });
    });
    return connection;
  };

  const send = async (kind: BridgeFrame["kind"], sessionID?: string, envelope?: string): Promise<BridgeResponse> => {
    const target = await connect();
    if (bridgeFailed) throw new Error("Shephrd bridge failed");
    const nextSequence = seq + 1;
    const frame: BridgeFrame = {
      schema_version: schemaVersion,
      token,
      attempt_id: attemptID,
      run_generation: runGeneration,
      seq: nextSequence,
      kind,
      ...(sessionID ? { session_id: sessionID } : {}),
      ...(envelope ? { envelope } : {}),
    };
    const line = JSON.stringify(frame) + "\n";
    if (Buffer.byteLength(line) >= maxFrameBytes) throw new Error("oversized Shephrd bridge frame");
    seq = nextSequence;
    const response = new Promise<BridgeResponse>((resolve, reject) => {
      const timer = setTimeout(() => {
        pending.delete(nextSequence);
        reject(new Error("Shephrd bridge response timed out"));
      }, responseTimeoutMilliseconds);
      pending.set(nextSequence, { resolve, reject, timer });
    });
    try {
      await new Promise<void>((resolve, reject) => target.write(line, (error) => error ? reject(error) : resolve()));
    } catch (error) {
      const entry = pending.get(nextSequence);
      if (entry) clearTimeout(entry.timer);
      pending.delete(nextSequence);
      throw error;
    }
    return response;
  };

  const sendControl = async (kind: "settled" | "invalid" | "repair_exhausted", ctx: { shutdown(): void }) => {
    try {
      await send(kind);
    } finally {
      bridgeFailed = true;
      ctx.shutdown();
    }
  };

  pi.on("session_start", async (_event, ctx) => {
    if (ctx.mode !== "tui" || started) {
      await sendControl("invalid", ctx);
      return;
    }
    started = true;
    try {
      subdriver = subdriverSession(process.env);
      const response = await send("session", ctx.sessionManager.getSessionId());
      if (response.result !== "accepted_nonterminal") ctx.shutdown();
    } catch {
      bridgeFailed = true;
      ctx.shutdown();
    }
  });

  pi.on("message_end", async (event, ctx) => {
    if (!started || terminal || bridgeFailed) return;
    if (event.message.role !== "assistant" || !["stop", "length", "toolUse", "deferred"].includes(event.message.stopReason)) return;
    const text = assistantTexts(event.message).join("\n");
    if (!/<shephrd-event>\s*\{/.test(text)) return;
    let response: BridgeResponse;
    try {
      response = await send("event_candidate", undefined, text);
    } catch {
      bridgeFailed = true;
      ctx.shutdown();
      return;
    }
    if (response.result === "accepted_nonterminal") return;
    if (response.result === "accepted_terminal") {
      terminal = true;
      ctx.shutdown();
      return;
    }
    if (response.result === "repair" && !repairPending) {
      repairPending = true;
      pi.sendMessage({ customType: "shephrd-protocol-repair", content: repairPrompt(response, subdriver), display: true }, { deliverAs: "followUp", triggerTurn: true });
      return;
    }
    bridgeFailed = true;
    ctx.shutdown();
  });

  pi.on("tool_call", async () => {
    if (repairPending) return { block: true, reason: "Shephrd protocol correction turn cannot run tools" };
  });

  pi.on("agent_settled", async (_event, ctx) => {
    if (!started) return;
    if (bridgeFailed) {
      ctx.shutdown();
      return;
    }
    if (terminal) {
      ctx.shutdown();
      return;
    }
    if (repairPending && ctx.hasPendingMessages()) return;
    await sendControl(repairPending ? "repair_exhausted" : "settled", ctx);
  });

  pi.on("session_shutdown", async () => {
    failPending(new Error("Pi session shut down"));
    if (!socket) return;
    if (socket.destroyed) {
      socket = undefined;
      return;
    }
    await new Promise<void>((resolve) => socket?.end(resolve));
    socket = undefined;
  });
}
