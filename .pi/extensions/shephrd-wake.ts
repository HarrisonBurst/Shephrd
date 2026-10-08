import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

type ClaimErrorKind = "claim_conflict" | "claim_stale" | "claim_expired" | "transient";

type ReportLifecycle = {
  handler_name: string;
  state: string;
  annotation?: string;
  receipt_system?: string;
  receipt_id?: string;
  failure_kind?: string;
  failure_message?: string;
  retry_command?: string;
};

type Notification = {
  coordinator_id?: string;
  coordinator_repo_name?: string;
  coordinator_event_id?: number;
  subdriver_id?: string;
  subdriver_repo_name?: string;
  subdriver_event_id?: number;
  request_id?: string;
  notification_id: string;
  task_id: string;
  task_title?: string;
  feature_key?: string;
  task_label?: string;
  attempt_id: string;
  worker_run_generation: number;
  kind: string;
  payload: string;
  target_driver_id: string;
  report_lifecycle?: ReportLifecycle[];
  claim: {
    consumer_id: string;
    driver_generation: string;
    claim_token: string;
    claim_until: string;
  };
};

type ObligationCounts = {
  act_now: number;
  needs_disposition: number;
  result_ready: number;
  planned_ready: number;
};

type ObligationSnapshot = {
  schema_version?: number;
  counts?: ObligationCounts;
  error?: string;
};

type ObligationResidue = {
  notification_id: string;
  counts?: ObligationCounts;
  delta?: ObligationCounts;
  error?: string;
};

type ChildEvent =
  | { type: "notification"; notification: Notification; obligations: ObligationSnapshot }
  | { type: "ack"; notification_id: string; claim_token: string; driver_generation: string; ok: boolean; handling_id?: string; error_kind?: ClaimErrorKind; detail?: string; obligations?: ObligationSnapshot }
  | { type: "renew"; notification_id: string; claim_token: string; driver_generation: string; ok: boolean; error_kind?: ClaimErrorKind; claim_until?: string; detail?: string }
  | { type: "invalidated"; notification_id: string; claim_token: string; driver_generation: string; error_kind: Exclude<ClaimErrorKind, "transient">; detail?: string }
  | { type: "expired"; notification_id: string; claim_token: string; driver_generation: string }
  | { type: "error"; detail: string };

type ChildCommand =
  | { op: "ack"; notification_id: string; claim_token: string; consumer_id: string; driver_generation: string }
  | { op: "renew"; notification_id: string; claim_token: string; consumer_id: string; driver_generation: string }
  | { op: "stop" };

type Dependencies = {
  spawn: typeof spawn;
  randomUUID: typeof randomUUID;
  readFileSync: typeof readFileSync;
  homedir: typeof homedir;
  env: NodeJS.ProcessEnv;
  execPath: string;
  owners: Set<string>;
};

const processOwners = new Set<string>();

function durationMilliseconds(value: string | undefined, fallback: number): number {
  if (!value) return fallback;
  const match = value.trim().match(/^"?([0-9]+)(ms|s|m)"?$/);
  if (!match) return fallback;
  const multiplier = match[2] === "m" ? 60000 : match[2] === "s" ? 1000 : 1;
  const milliseconds = Number(match[1]) * multiplier;
  return milliseconds > 0 ? milliseconds : fallback;
}

export function renewalIntervalMilliseconds(claimUntil: string, now = Date.now()): number {
  const remaining = Date.parse(claimUntil) - now;
  return Number.isFinite(remaining) && remaining > 0 ? Math.max(1000, Math.min(60000, Math.floor(remaining / 2))) : 1000;
}

function messageText(message: unknown): string {
  if (!message || typeof message !== "object") return "";
  const content = (message as { content?: unknown }).content;
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter(part => part && typeof part === "object" && (part as { type?: unknown }).type === "text").map(part => String((part as { text?: unknown }).text || "")).join("\n");
}

function boundedLine(value: unknown, limit = 256): string {
  return String(value || "").replace(/\s+/g, " ").trim().slice(0, limit);
}

export function notificationSourceLabel(notice: Pick<Notification, "subdriver_id" | "subdriver_repo_name" | "request_id" | "task_label" | "task_title" | "task_id">): string {
  if (notice.request_id && notice.subdriver_id) return `Sub-driver: ${boundedLine(notice.subdriver_repo_name, 64) || "General"}`;
  return `Worker ${boundedLine(notice.task_label || notice.task_title || notice.task_id, 96) || "task"}`;
}

function canonicalNotification(notice: Notification): Notification | undefined {
  for (const [current, legacy] of [[notice.subdriver_id, notice.coordinator_id], [notice.subdriver_event_id, notice.coordinator_event_id], [notice.subdriver_repo_name, notice.coordinator_repo_name]]) {
    if (current !== undefined && legacy !== undefined && current !== legacy) return undefined;
  }
  const normalized = {
    ...notice,
    subdriver_id: notice.subdriver_id ?? notice.coordinator_id,
    subdriver_event_id: notice.subdriver_event_id ?? notice.coordinator_event_id,
    subdriver_repo_name: notice.subdriver_repo_name ?? notice.coordinator_repo_name,
  };
  if (normalized.subdriver_id || normalized.subdriver_event_id || normalized.request_id) {
    if (!normalized.subdriver_id || !normalized.subdriver_event_id || !normalized.request_id) return undefined;
    normalized.kind = normalized.kind.replace(/^coordinator-/, "subdriver-");
  }
  return normalized;
}

function obligationCountsText(counts: ObligationCounts): string {
  return `${counts.act_now} act now | ${counts.needs_disposition} need disposition | ${counts.result_ready} results ready | ${counts.planned_ready} planned ready`;
}

function obligationSnapshotText(snapshot: ObligationSnapshot | undefined, boundary: string): string {
  if (snapshot?.counts) return `Owner obligations ${boundary} (schema ${snapshot.schema_version}, read-only counts): ${obligationCountsText(snapshot.counts)}.`;
  return `Owner obligations ${boundary} unavailable: ${boundedLine(snapshot?.error || "invalid obligations response")}.`;
}

function obligationResidue(notificationID: string, initial: ObligationSnapshot | undefined, final: ObligationSnapshot | undefined): ObligationResidue | undefined {
  if (!final?.counts) return { notification_id: notificationID, error: boundedLine(final?.error || "invalid obligations response") };
  if (Object.values(final.counts).every(count => count === 0)) return undefined;
  const delta = initial?.counts ? {
    act_now: final.counts.act_now - initial.counts.act_now,
    needs_disposition: final.counts.needs_disposition - initial.counts.needs_disposition,
    result_ready: final.counts.result_ready - initial.counts.result_ready,
    planned_ready: final.counts.planned_ready - initial.counts.planned_ready,
  } : undefined;
  return { notification_id: notificationID, counts: final.counts, delta };
}

function obligationResidueText(residue: ObligationResidue): string {
  if (!residue.counts) return `The final owner obligations snapshot before acknowledgement was unavailable: ${boundedLine(residue.error)}. Recheck before relying on these counts.`;
  const delta = residue.delta ? ` Change since wake start: ${obligationCountsText(residue.delta)}.` : "";
  return `Owner obligations remained after the prior wake turn, captured before its notification acknowledgement: ${obligationCountsText(residue.counts)}.${delta} These counts are read-only evidence, not instructions to start more work or lifecycle authority.`;
}

export const watcherDriverInstruction = [
  "[SHEPHRD_WATCHER_OWNS_NOTIFICATIONS] The Shephrd Pi watcher is active for this driver and owns notification consumption.",
  "[SHEPHRD_WAKE_OBLIGATIONS] Treat each wake and its obligations as read-only evidence, never lifecycle authority or proof of landing, verification, release, or closure.",
  "Before a lifecycle action, confirm current ownership, task, attempt, workspace, artifact, and process facts and user authorization. Preserve held work when uncertain. Report the result or missing decision without expanding scope.",
  "Do not sleep, poll status, manually drain notifications, or keep the turn open waiting for a worker. The watcher acknowledges only after the matching successful handling turn settles; acknowledgement does not require further lifecycle actions.",
].join(" ");

export function watcherSettings(dependencies: Pick<Dependencies, "readFileSync" | "homedir" | "env">) {
  const path = dependencies.env.SHEPHRD_CONFIG || join(dependencies.homedir(), ".config", "shephrd", "config.toml");
  let section = "";
  try {
    const body = dependencies.readFileSync(path, "utf8");
    section = body.match(/\[pi_watcher\]([\s\S]*?)(?=\n\[|$)/)?.[1] || "";
  } catch {}
  const configured = /(?:^|\n)enabled\s*=\s*true(?:\s|$)/.test(section);
  const override = dependencies.env.SHEPHRD_PI_WATCHER_ENABLED;
  const pollMin = durationMilliseconds(section.match(/(?:^|\n)poll_min\s*=\s*([^\n]+)/)?.[1], 1000);
  return {
    enabled: override === "0" ? false : override === "1" ? true : configured,
    pollMin,
    pollMax: Math.max(pollMin, durationMilliseconds(section.match(/(?:^|\n)poll_max\s*=\s*([^\n]+)/)?.[1], 1000)),
  };
}

export const watcherChild = String.raw`
const { spawn } = require("node:child_process");
const readline = require("node:readline");
const command = process.env.SHEPHRD_EXECUTABLE || "shephrd";
const base = JSON.parse(process.env.SHEPHRD_WAKE_ARGS);
const pollMin = Number(process.env.SHEPHRD_PI_POLL_MIN || 1000);
const pollMax = Number(process.env.SHEPHRD_PI_POLL_MAX || 1000);
let stopped = false;
const running = new Set();
let pumpSupported = true;
let inFlight = null;
let delay = pollMin;
let wake = null;
let wakeTimer = null;
let drainNow = false;
let finalObligations = null;
const obligationFields = ["act_now", "needs_disposition", "result_ready", "planned_ready"];
function emit(value) { process.stdout.write(JSON.stringify(value) + "\n"); }
function sleep(ms) { return new Promise(resolve => setTimeout(resolve, ms)); }
function claimErrorKind(result) {
  for (const output of [result.stderr, result.stdout]) {
    try {
      const value = JSON.parse(output.trim());
      if (value && (value.error_kind === "claim_conflict" || value.error_kind === "claim_stale" || value.error_kind === "claim_expired")) return value.error_kind;
    } catch {}
  }
  return "transient";
}
function terminalClaimError(kind) { return kind === "claim_conflict" || kind === "claim_stale" || kind === "claim_expired"; }
function wakeClaim() {
  if (wakeTimer) clearTimeout(wakeTimer);
  wakeTimer = null;
  const resolve = wake;
  wake = null;
  if (resolve) resolve();
}
function clearInFlight() {
  inFlight = null;
  finalObligations = null;
}
function invalidate(notification, kind, detail) {
  if (!inFlight || inFlight.notification_id !== notification.notification_id || inFlight.claim.claim_token !== notification.claim.claim_token) return;
  clearInFlight();
  drainNow = true;
  wakeClaim();
  emit({ type: "invalidated", notification_id: notification.notification_id, claim_token: notification.claim.claim_token, driver_generation: notification.claim.driver_generation, error_kind: kind, detail });
}
function run(args) {
  return new Promise(resolve => {
    const child = spawn(command, args, { stdio: ["ignore", "pipe", "pipe"] });
    running.add(child);
    let stdout = "";
    let stderr = "";
    const append = (current, value) => current.length >= 65536 ? current : (current + String(value)).slice(0, 65536);
    const timer = setTimeout(() => child.kill(), 10000);
    child.stdout.on("data", value => { stdout = append(stdout, value); });
    child.stderr.on("data", value => { stderr = append(stderr, value); });
    child.on("close", code => {
      clearTimeout(timer);
      running.delete(child);
      resolve({ code, stdout, stderr });
    });
    child.on("error", error => {
      clearTimeout(timer);
      running.delete(child);
      resolve({ code: -1, stdout, stderr: String(error) });
    });
  });
}
function obligationFailure(detail) {
  return { error: String(detail || "obligations query failed").replace(/\s+/g, " ").trim().slice(0, 256) };
}
async function readObligations(owner) {
  const result = await run(["task", "obligations", "--driver-id", owner, "--limit", "1", "--json"]);
  if (result.code !== 0) return obligationFailure(result.stderr || result.stdout);
  let value;
  try { value = JSON.parse(result.stdout.trim()); } catch { return obligationFailure("obligations output was invalid"); }
  if (value?.schema_version !== 2 || value?.scope?.driver_id !== owner || value?.scope?.all_drivers !== false || !value?.counts) return obligationFailure("obligations scope or schema was invalid");
  const counts = {};
  for (const field of obligationFields) {
    if (!Number.isInteger(value.counts[field]) || value.counts[field] < 0) return obligationFailure("obligations counts were invalid");
    counts[field] = value.counts[field];
  }
  return { schema_version: value.schema_version, counts };
}
function waitForClaim() {
  return new Promise(resolve => {
    wake = resolve;
    const check = () => {
      if (stopped || !inFlight) return wakeClaim();
      const remaining = Date.parse(inFlight.claim.claim_until) - Date.now();
      if (remaining <= 0) {
        const expired = inFlight;
        clearInFlight();
        wakeClaim();
        emit({ type: "expired", notification_id: expired.notification_id, claim_token: expired.claim.claim_token, driver_generation: expired.claim.driver_generation });
        return;
      }
      wakeTimer = setTimeout(check, Math.min(1000, remaining));
    };
    check();
  });
}
async function drain() {
  const result = await run(base);
  if (result.code !== 0) {
    emit({ type: "error", detail: (result.stderr || result.stdout || "wake drain failed").slice(0, 512) });
    delay = Math.min(delay * 2, pollMax);
    return;
  }
  let value;
  try { value = JSON.parse(result.stdout.trim() || "{}"); } catch (error) {
    emit({ type: "error", detail: String(error).slice(0, 512) });
    delay = Math.min(delay * 2, pollMax);
    return;
  }
  if (!Array.isArray(value.notifications) || value.notifications.length === 0) {
    delay = Math.min(delay * 2, pollMax);
    return;
  }
  delay = pollMin;
  inFlight = value.notifications[0];
  finalObligations = null;
  const obligations = await readObligations(inFlight.target_driver_id);
  emit({ type: "notification", notification: inFlight, obligations });
  void waitForClaim();
}
async function pump() {
  if (!pumpSupported) return;
  const result = await run(["wake", "pump", "--driver-id", base[base.indexOf("--driver-id") + 1], "--json"]);
  if (result.code === 0) return;
  let error;
  try { error = JSON.parse(result.stderr.trim()).error; } catch {}
  if (error === 'unknown command "pump" for "shephrd wake"' || error === "unknown flag: --driver-id") {
    pumpSupported = false;
    emit({ type: "error", detail: "This Shephrd CLI lacks wake pump; sub-driver activation remains drain-coupled until the tested CLI and watcher are installed together. Notification delivery is unchanged." });
    return;
  }
  emit({ type: "error", detail: (result.stderr || result.stdout || "sub-driver activation failed").slice(0, 512) });
}
async function loop() {
  while (!stopped) {
    if (!drainNow) await sleep(delay);
    drainNow = false;
    if (stopped) break;
    if (inFlight) await pump();
    else await drain();
  }
}
async function handleCommand(value) {
  if (value.op === "ack" && inFlight && value.notification_id === inFlight.notification_id && value.claim_token === inFlight.claim.claim_token) {
    if (!finalObligations) finalObligations = await readObligations(inFlight.target_driver_id);
    const obligations = finalObligations;
    const args = ["wake", "ack", "--claim-token", value.claim_token, "--driver-id", value.consumer_id, "--json"];
    const result = await run(args);
    let handlingID;
    try {
      const receipt = JSON.parse(result.stdout);
      if (receipt.schema_version === 1 && receipt.notification_id === value.notification_id && receipt.consumer_id === value.consumer_id && receipt.driver_generation === value.driver_generation && typeof receipt.handling_id === "string" && receipt.handling_id) handlingID = receipt.handling_id;
    } catch {}
    const ok = result.code === 0 && Boolean(handlingID);
    const errorKind = ok ? undefined : claimErrorKind(result);
    const detail = result.code === 0 && !handlingID ? "wake ack returned no schema-versioned handling ID" : (result.stderr || result.stdout).slice(0, 512);
    if (!ok && terminalClaimError(errorKind)) {
      invalidate(inFlight, errorKind, detail);
    } else {
      emit({ type: "ack", notification_id: value.notification_id, claim_token: value.claim_token, driver_generation: value.driver_generation, ok, handling_id: handlingID, error_kind: errorKind, detail: ok ? "" : detail, obligations });
      if (ok && inFlight?.notification_id === value.notification_id && inFlight.claim.claim_token === value.claim_token) {
        clearInFlight();
        wakeClaim();
      }
    }
  }
  if (value.op === "renew" && inFlight && value.notification_id === inFlight.notification_id && value.claim_token === inFlight.claim.claim_token) {
    const args = ["wake", "renew", "--claim-token", value.claim_token, "--driver-id", value.consumer_id, "--json"];
    const result = await run(args);
    let until;
    try { until = JSON.parse(result.stdout).claim_until; } catch {}
    const ok = result.code === 0;
    const errorKind = ok ? undefined : claimErrorKind(result);
    if (!ok && terminalClaimError(errorKind)) {
      invalidate(inFlight, errorKind, (result.stderr || result.stdout).slice(0, 512));
    } else {
      emit({ type: "renew", notification_id: value.notification_id, claim_token: value.claim_token, driver_generation: value.driver_generation, ok, error_kind: errorKind, claim_until: until, detail: ok ? "" : (result.stderr || result.stdout).slice(0, 512) });
      if (ok && until && inFlight?.notification_id === value.notification_id && inFlight.claim.claim_token === value.claim_token) inFlight.claim.claim_until = until;
    }
  }
}
let commandQueue = Promise.resolve();
const input = readline.createInterface({ input: process.stdin });
input.on("line", line => {
  let value;
  try { value = JSON.parse(line); } catch { return; }
  if (value.op === "stop") {
    stopped = true;
    if (wake) wake();
    if (wakeTimer) clearTimeout(wakeTimer);
    for (const child of running) child.kill();
    input.close();
    return;
  }
  commandQueue = commandQueue.then(() => handleCommand(value)).catch(error => emit({ type: "error", detail: String(error).slice(0, 512) }));
});
process.on("SIGTERM", () => {
  stopped = true;
  for (const child of running) child.kill();
  process.exit(0);
});
loop();
`;

export function createShephrdWakeExtension(overrides: Partial<Dependencies> = {}) {
  const dependencies: Dependencies = {
    spawn,
    randomUUID,
    readFileSync,
    homedir,
    env: process.env,
    execPath: process.execPath,
    owners: processOwners,
    ...overrides,
  };

  return function shephrdWakeExtension(pi: ExtensionAPI) {
    let generation = "";
    let consumerID = "";
    let ownsConsumer = false;
    let child: ChildProcessWithoutNullStreams | undefined;
    let current: Notification | undefined;
    let pendingContent = "";
    let initialObligations: ObligationSnapshot | undefined;
    let nextTurnObligations: ObligationResidue | undefined;
    let inputConfirmed = false;
    let injected = false;
    let responseCompleted = false;
    let acknowledgementPending = false;
    let renewTimer: ReturnType<typeof setInterval> | undefined;
    let acknowledgementTimer: ReturnType<typeof setTimeout> | undefined;
    let renewDeadline = 0;
    let active = false;
    let buffer = "";
    let lifecycle = 0;
    const handled = new Set<string>();

    const notify = (ctx: ExtensionContext, text: string, level: "info" | "warning" | "error" = "warning") => {
      if (ctx.hasUI) ctx.ui.notify(text.slice(0, 512), level);
    };

    const setStatus = (ctx: ExtensionContext, text: string | undefined) => {
      if (ctx.hasUI) ctx.ui.setStatus("shephrd-wake", text);
    };

    const send = (command: ChildCommand) => {
      if (!child?.stdin.writable) return false;
      child.stdin.write(JSON.stringify(command) + "\n");
      return true;
    };

    const clearTimers = () => {
      if (renewTimer) clearInterval(renewTimer);
      if (acknowledgementTimer) clearTimeout(acknowledgementTimer);
      renewTimer = undefined;
      acknowledgementTimer = undefined;
      renewDeadline = 0;
    };

    const stop = (ctx: ExtensionContext) => {
      lifecycle++;
      clearTimers();
      active = false;
      if (child) {
        send({ op: "stop" });
        child.kill();
      }
      child = undefined;
      if (current) setStatus(ctx, undefined);
      current = undefined;
      pendingContent = "";
      initialObligations = undefined;
      nextTurnObligations = undefined;
      inputConfirmed = false;
      injected = false;
      responseCompleted = false;
      acknowledgementPending = false;
      buffer = "";
      if (ownsConsumer) dependencies.owners.delete(consumerID);
      ownsConsumer = false;
    };

    const clearCurrent = (ctx: ExtensionContext) => {
      setStatus(ctx, undefined);
      current = undefined;
      pendingContent = "";
      initialObligations = undefined;
      inputConfirmed = false;
      injected = false;
      responseCompleted = false;
      acknowledgementPending = false;
      clearTimers();
    };

    const startRenewal = (ctx: ExtensionContext) => {
      if (!current || renewTimer) return;
      renewDeadline = Date.now() + 30 * 60 * 1000;
      renewTimer = setInterval(() => {
        if (!current) return;
        if (Date.now() >= renewDeadline) {
          clearTimers();
          notify(ctx, `Shephrd wake claim renewal horizon reached for ${current.notification_id}; it may be redelivered`);
          return;
        }
        send({ op: "renew", notification_id: current.notification_id, claim_token: current.claim.claim_token, consumer_id: consumerID, driver_generation: generation });
      }, renewalIntervalMilliseconds(current.claim.claim_until));
    };

    const acknowledge = (ctx: ExtensionContext) => {
      if (!current || !injected || !responseCompleted || acknowledgementPending || acknowledgementTimer || generation !== current.claim.driver_generation) return;
      if (Date.parse(current.claim.claim_until) <= Date.now()) {
        clearCurrent(ctx);
        return;
      }
      acknowledgementPending = send({ op: "ack", notification_id: current.notification_id, claim_token: current.claim.claim_token, consumer_id: consumerID, driver_generation: generation });
      if (!acknowledgementPending) notify(ctx, "Shephrd wake acknowledgement could not reach the watcher child");
    };

    const handle = async (value: ChildEvent, ctx: ExtensionContext, childLifecycle: number) => {
      if (!active || childLifecycle !== lifecycle) return;
      if (value.type === "error") {
        notify(ctx, `Shephrd wake watcher: ${value.detail}`);
        return;
      }
      if (value.type === "renew") {
        if (current?.notification_id !== value.notification_id || current.claim.claim_token !== value.claim_token || current.claim.driver_generation !== value.driver_generation) return;
        if (!value.ok) notify(ctx, `Shephrd wake claim renewal failed: ${value.detail || "claim may expire"}`);
        return;
      }
      if (value.type === "invalidated") {
        if (current?.notification_id !== value.notification_id || current.claim.claim_token !== value.claim_token || current.claim.driver_generation !== value.driver_generation) return;
        clearCurrent(ctx);
        return;
      }
      if (value.type === "expired") {
        if (current?.notification_id === value.notification_id && current.claim.claim_token === value.claim_token && current.claim.driver_generation === value.driver_generation) clearCurrent(ctx);
        return;
      }
      if (value.type === "ack") {
        if (current?.notification_id !== value.notification_id || current.claim.claim_token !== value.claim_token || current.claim.driver_generation !== value.driver_generation) return;
        acknowledgementPending = false;
        if (!value.ok) {
          if (value.error_kind === "claim_conflict" || value.error_kind === "claim_stale" || value.error_kind === "claim_expired") {
            clearCurrent(ctx);
            return;
          }
          if (Date.parse(current.claim.claim_until) <= Date.now()) {
            clearCurrent(ctx);
            return;
          }
          notify(ctx, `Shephrd wake acknowledgement failed: ${value.detail || "claim remains durable"}`);
          if (!acknowledgementTimer) {
            acknowledgementTimer = setTimeout(() => {
              acknowledgementTimer = undefined;
              acknowledge(ctx);
            }, 1000);
          }
          return;
        }
        const residue = obligationResidue(value.notification_id, initialObligations, value.obligations);
        handled.add(value.notification_id);
        clearCurrent(ctx);
        nextTurnObligations = residue;
        pi.appendEntry("shephrd-wake", { notification_id: value.notification_id, handling_id: value.handling_id, ...(residue ? { obligations: residue } : {}) });
        return;
      }
      if (generation !== value.notification.claim.driver_generation || value.notification.target_driver_id !== consumerID || current || handled.has(value.notification.notification_id)) return;
      const notice = canonicalNotification(value.notification);
      if (!notice) {
        notify(ctx, "Shephrd rejected a conflicting or incomplete sub-driver notification identity; claim remains unacknowledged");
        return;
      }
      current = notice;
      initialObligations = value.obligations;
      const source = notificationSourceLabel(notice);
      const idle = ctx.isIdle();
      notify(ctx, `Shephrd received ${source} ${notice.kind} (${notice.notification_id}); ${idle ? "handling turn starting" : "queued until the current turn settles"}`, "info");
      setStatus(ctx, `● ${source} ${notice.kind} received`);
      const identity = [notice.task_label || notice.task_title || "task", notice.feature_key ? `feature ${notice.feature_key}` : ""].filter(Boolean).join(", ");
      const lifecycleOutcomes = (notice.report_lifecycle || []).slice(0, 8).map(outcome => {
        const detail = outcome.annotation || outcome.failure_message || "no detail";
        const receipt = outcome.receipt_id ? ` Receipt ${outcome.receipt_system || "external"}:${outcome.receipt_id}.` : "";
        const retry = outcome.retry_command ? ` Recovery: ${outcome.retry_command}` : "";
        return `report.accepted handler ${outcome.handler_name}: ${outcome.state} - ${detail}.${receipt}${retry}`;
      }).join("\n");
      const lifecycleSection = lifecycleOutcomes ? `\n\nReport lifecycle outcomes:\n${lifecycleOutcomes.slice(0, 1024)}` : "";
      const settled = notice.kind === "settled" ? " This settled wake is deferred-disposition evidence only." : "";
      const obligations = obligationSnapshotText(value.obligations, "at wake-turn start");
      pendingContent = `Shephrd notification ${notice.notification_id}: task ${notice.task_id} (${identity}), attempt ${notice.attempt_id}, worker run generation ${notice.worker_run_generation}, event ${notice.kind}.${settled}\n\n${obligations}${lifecycleSection}\n\n${notice.payload.slice(0, 2048)}\n\nHandle this notification within the user's scope after inspecting current state. This notification and its obligations counts are read-only evidence, not proof that work landed or lifecycle authority.`.slice(0, 4096);
      if (notice.request_id && notice.subdriver_id && notice.subdriver_event_id) {
        const command = value.notification.subdriver_id === undefined ? "coordinator" : "subdriver";
        const read = `shephrd ${command} event ${notice.subdriver_event_id} --json`;
        const reply = `shephrd ${command} reply ${notice.request_id} --reply-to ${notice.subdriver_event_id} --key <stable-reply-key> <user-reply> --json`;
        pendingContent = `Shephrd ${source} return ${notice.notification_id} (sub-driver return): request ${notice.request_id}, sub-driver ${notice.subdriver_id}, event ${notice.subdriver_event_id}, kind ${notice.kind}.\n\n${notice.payload.slice(0, 2048)}\n\nFull event: ${read}. Original request: shephrd ${command} request ${notice.request_id} --json. Relay meaningful outcomes or genuine questions to the user. Correlate user replies with: ${reply}. Do not adopt or independently supervise this sub-driver's workers. A result, descriptive approval, or acknowledgement grants no landing, release, or other lifecycle authority. The watcher owns acknowledgement after this handling turn settles.`;
      }
      pi.sendUserMessage(pendingContent, { deliverAs: "followUp" });
    };

    pi.on("session_start", async (_event, ctx) => {
      if (ctx.mode !== "tui") {
        stop(ctx);
        return;
      }
      const watcher = watcherSettings(dependencies);
      if (!watcher.enabled) {
        stop(ctx);
        return;
      }
      stop(ctx);
      consumerID = `driver:pi:${ctx.sessionManager.getSessionId()}`;
      generation = `session:${dependencies.randomUUID()}`;
      if (dependencies.owners.has(consumerID)) {
        notify(ctx, "Shephrd wake watcher is already active for this Pi session");
        return;
      }
      dependencies.owners.add(consumerID);
      ownsConsumer = true;
      active = true;
      const childLifecycle = lifecycle;
      for (const entry of ctx.sessionManager.getBranch()) {
        if (entry.type === "custom" && entry.customType === "shephrd-wake") {
          const data = entry.data as { notification_id?: string; obligations?: ObligationResidue };
          if (data.notification_id) handled.add(data.notification_id);
          if (data.obligations) nextTurnObligations = data.obligations;
        }
        if (entry.type === "custom_message" && entry.customType === "shephrd-wake-obligations") {
          const details = entry.details as { notification_id?: string } | undefined;
          if (details?.notification_id === nextTurnObligations?.notification_id) nextTurnObligations = undefined;
        }
      }
      const args = ["wake", "drain", "--limit", "1", "--driver-id", consumerID, "--driver-generation", generation, "--json"];
      const startedChild = dependencies.spawn(dependencies.execPath, ["-e", watcherChild], {
        env: { ...dependencies.env, SHEPHRD_WAKE_ARGS: JSON.stringify(args), SHEPHRD_PI_POLL_MIN: String(watcher.pollMin), SHEPHRD_PI_POLL_MAX: String(watcher.pollMax) },
        stdio: ["pipe", "pipe", "pipe"],
      });
      child = startedChild;
      startedChild.stdout.on("data", chunk => {
        if (childLifecycle !== lifecycle || child !== startedChild) return;
        buffer += String(chunk);
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";
        for (const line of lines) {
          if (!line.trim()) continue;
          try {
            const value = JSON.parse(line) as ChildEvent;
            void handle(value, ctx, childLifecycle).catch(error => notify(ctx, `Shephrd wake watcher event failed: ${String(error)}`));
          } catch (error) {
            notify(ctx, `Shephrd wake watcher output was invalid: ${String(error)}`);
          }
        }
      });
      startedChild.stderr.on("data", chunk => {
        if (childLifecycle === lifecycle && child === startedChild) notify(ctx, `Shephrd wake watcher: ${String(chunk)}`);
      });
      startedChild.on("exit", () => {
        if (childLifecycle !== lifecycle || child !== startedChild) return;
        child = undefined;
        active = false;
        clearCurrent(ctx);
        if (ownsConsumer) dependencies.owners.delete(consumerID);
        ownsConsumer = false;
        notify(ctx, "Shephrd wake watcher stopped; durable claims remain recoverable");
      });
    });

    pi.on("input", async (event, ctx) => {
      if (!current || event.source !== "extension" || event.text !== pendingContent) return;
      inputConfirmed = true;
      startRenewal(ctx);
    });

    pi.on("before_agent_start", async event => {
      if (!active) return;
      const residue = nextTurnObligations;
      nextTurnObligations = undefined;
      return {
        message: residue ? { customType: "shephrd-wake-obligations", content: obligationResidueText(residue), display: true, details: { notification_id: residue.notification_id } } : undefined,
        systemPrompt: `${event.systemPrompt}\n\n${watcherDriverInstruction}`,
      };
    });

    pi.on("message_start", async (event, ctx) => {
      if (!current || !inputConfirmed || injected || event.message.role !== "user" || messageText(event.message) !== pendingContent) return;
      injected = true;
      setStatus(ctx, `● ${notificationSourceLabel(current)} ${current.kind} handling`);
    });

    pi.on("message_end", async event => {
      if (!current || !injected || event.message.role !== "assistant") return;
      responseCompleted = event.message.stopReason !== "aborted" && event.message.stopReason !== "error";
    });

    pi.on("agent_settled", async (_event, ctx) => {
      if (current && injected && !responseCompleted && renewTimer) {
        clearInterval(renewTimer);
        renewTimer = undefined;
      }
      acknowledge(ctx);
    });

    pi.on("session_shutdown", async (_event, ctx) => {
      stop(ctx);
      generation = "";
      consumerID = "";
    });
  };
}

export default createShephrdWakeExtension();
