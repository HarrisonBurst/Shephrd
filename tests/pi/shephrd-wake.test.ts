import assert from "node:assert/strict";
import { execFileSync, spawn as spawnProcess } from "node:child_process";
import { EventEmitter, once } from "node:events";
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PassThrough } from "node:stream";
import test from "node:test";
import { createShephrdWakeExtension, notificationSourceLabel, renewalIntervalMilliseconds, watcherChild, watcherSettings } from "../../.pi/extensions/shephrd-wake.ts";

function fakeChild() {
  const events = new EventEmitter();
  const writes: string[] = [];
  const stdin = new PassThrough();
  stdin.on("data", value => writes.push(String(value)));
  const child = {
    stdin,
    stdout: new PassThrough(),
    stderr: new PassThrough(),
    writes,
    kills: 0,
    kill() {
      child.kills++;
      return true;
    },
    on(name: string, listener: (...args: unknown[]) => void) {
      events.on(name, listener);
      return child;
    },
    emit(name: string, ...args: unknown[]) {
      events.emit(name, ...args);
    },
  };
  return child;
}

function harness(
  mode: "tui" | "json" | "print" | "rpc",
  env: NodeJS.ProcessEnv = { SHEPHRD_PI_WATCHER_ENABLED: "1" },
  confirmInputs = true,
  branch: any[] = [],
  dependencies: Parameters<typeof createShephrdWakeExtension>[0] = {},
) {
  const handlers = new Map<string, Array<(event: any, context: any) => unknown>>();
  const children: ReturnType<typeof fakeChild>[] = [];
  const spawns: Array<{ command: string; args: string[]; options: any }> = [];
  const notices: string[] = [];
  const indicators: Array<{ at: number; text: string }> = [];
  const statuses: Array<{ at: number; key: string; text: string | undefined }> = [];
  let idle = true;
  const entries: unknown[] = [];
  const messages: string[] = [];
  const deliveries: Array<{ at: number; options: unknown }> = [];
  const events: Array<{ name: string; at: number; event: any }> = [];
  const context = {
    mode,
    hasUI: mode === "tui" || mode === "rpc",
    ui: {
      notify(text: string, level?: string) {
        if (level === "info") indicators.push({ at: Date.now(), text });
        else notices.push(text);
      },
      setStatus(key: string, text: string | undefined) { statuses.push({ at: Date.now(), key, text }); },
    },
    isIdle() { return idle; },
    sessionManager: {
      getSessionId() { return "session-owner"; },
      getBranch() { return branch; },
    },
  };
  const emit = async (name: string, event: any = {}) => {
    events.push({ name, at: Date.now(), event });
    const results = [];
    for (const handler of handlers.get(name) || []) results.push(await handler(event, context));
    return results;
  };
  const pi = {
    on(name: string, handler: (event: any, context: any) => unknown) {
      handlers.set(name, [...(handlers.get(name) || []), handler]);
    },
    appendEntry(type: string, data: unknown) {
      entries.push({ type, data });
    },
    sendUserMessage(message: string, options: unknown) {
      messages.push(message);
      deliveries.push({ at: Date.now(), options });
      if (confirmInputs) {
        void emit("input", { text: message, source: "extension" });
        void emit("message_start", { message: { role: "user", content: [{ type: "text", text: message }] } });
      }
    },
  };
  const owners = new Set<string>();
  const extension = createShephrdWakeExtension({
    env,
    execPath: "/node",
    owners,
    randomUUID: (() => `uuid-${spawns.length}`) as typeof crypto.randomUUID,
    readFileSync: (() => "[pi_watcher]\nenabled = true\npoll_min = \"2s\"\npoll_max = \"4s\"\n") as any,
    homedir: () => "/home/test",
    spawn: ((command: string, args: string[], options: any) => {
      const child = fakeChild();
      children.push(child);
      spawns.push({ command, args, options });
      return child;
    }) as any,
    ...dependencies,
  });
  extension(pi as any);
  return {
    children,
    spawns,
    notices,
    indicators,
    statuses,
    setIdle(value: boolean) { idle = value; },
    entries,
    messages,
    deliveries,
    events,
    owners,
    commands(index = children.length - 1) {
      return children[index].writes.flatMap(value => value.trim().split("\n").filter(Boolean).map(line => JSON.parse(line)));
    },
    async childEvent(value: any, index = children.length - 1) {
      if ((value.type === "notification" || value.type === "ack") && !("obligations" in value)) value = { ...value, obligations: obligations() };
      children[index].stdout.write(JSON.stringify(value) + "\n");
      await new Promise(resolve => setTimeout(resolve, 0));
    },
    emit,
  };
}

function obligations(actNow = 0, needsDisposition = 0, resultReady = 0, plannedReady = 0) {
  return {
    schema_version: 2,
    counts: { act_now: actNow, needs_disposition: needsDisposition, result_ready: resultReady, planned_ready: plannedReady },
  };
}

function notification(driverID: string, generation: string, token = "first", claimUntil = Date.now() + 60000) {
  return {
    notification_id: "wake:1",
    task_id: "task_1",
    task_title: "Choose the release option",
    feature_key: "release-choice",
    task_label: "demo/Choose the release option [release-choice]",
    attempt_id: "attempt_1",
    worker_run_generation: 1,
    kind: "question",
    payload: "choose",
    target_driver_id: driverID,
    claim: { consumer_id: driverID, driver_generation: generation, claim_token: token, claim_until: new Date(claimUntil).toISOString() },
  };
}

function identity(runtime: ReturnType<typeof harness>) {
  const args = JSON.parse(runtime.spawns[0].options.env.SHEPHRD_WAKE_ARGS);
  return {
    driverID: args[args.indexOf("--driver-id") + 1],
    generation: args[args.indexOf("--driver-generation") + 1],
  };
}

function subdriverReturn(driverID: string, generation: string, repoName?: string) {
  return { ...notification(driverID, generation), task_id: "", attempt_id: "", worker_run_generation: 0, task_label: "request_one",
    subdriver_id: "coord_one", request_id: "request_one", subdriver_event_id: 42, kind: "subdriver-question", payload: "Which format?",
    ...(repoName ? { subdriver_repo_name: repoName } : {}) };
}

test("subdriver returns preserve request correlation without grandchild supervision", async () => {
  const runtime = harness("tui");
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: subdriverReturn(driverID, generation, "Shephrd") });
  assert.match(runtime.messages[0], /^Shephrd Sub-driver: Shephrd return wake:1 \(sub-driver return\): request request_one, sub-driver coord_one, event 42, kind subdriver-question\./);
  assert.doesNotMatch(runtime.messages[0], /Shephrd subdriver return/);
  assert.match(runtime.messages[0], /subdriver reply request_one --reply-to 42/);
  assert.match(runtime.messages[0], /Do not adopt or independently supervise/);
  assert.match(runtime.messages[0], /subdriver event 42 --json/);
  assert.equal(runtime.commands().filter(command => command.op === "ack").length, 0);
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(command => command.op === "ack").length, 1);
  await runtime.emit("session_shutdown");
});

test("legacy and dual-field returns normalize presentation without changing claim identity", async () => {
  for (const dual of [false, true]) {
    const runtime = harness("tui");
    await runtime.emit("session_start");
    const { driverID, generation } = identity(runtime);
    const canonical = subdriverReturn(driverID, generation, "Shephrd");
    const { subdriver_id, subdriver_event_id, subdriver_repo_name, ...base } = canonical;
    const notice = { ...base, ...(dual ? canonical : {}), coordinator_id: subdriver_id, coordinator_event_id: subdriver_event_id, coordinator_repo_name: subdriver_repo_name, kind: "coordinator-question" };
    await runtime.childEvent({ type: "notification", notification: notice });
    assert.match(runtime.indicators[0].text, /Sub-driver: Shephrd subdriver-question/);
    assert.match(runtime.messages[0], /\(sub-driver return\): request request_one, sub-driver coord_one, event 42, kind subdriver-question/);
    assert.ok(runtime.messages[0].includes(`shephrd ${dual ? "subdriver" : "coordinator"} reply request_one --reply-to 42`));
    await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
    await runtime.emit("agent_settled");
    await runtime.emit("agent_settled");
    const acks = runtime.commands().filter(value => value.op === "ack");
    assert.equal(acks.length, 1);
    assert.equal(acks[0].claim_token, canonical.claim.claim_token);
    assert.equal(acks[0].driver_generation, generation);
    await runtime.emit("session_shutdown");
  }
});

test("conflicting legacy return aliases and stale owners never inject or acknowledge", async () => {
  for (const override of [
    { coordinator_id: "other" }, { coordinator_event_id: 43 }, { coordinator_repo_name: "Other" },
    { subdriver_event_id: undefined }, { target_driver_id: "driver:other" },
  ]) {
    const runtime = harness("tui");
    await runtime.emit("session_start");
    const { driverID, generation } = identity(runtime);
    await runtime.childEvent({ type: "notification", notification: { ...subdriverReturn(driverID, generation, "Shephrd"), ...override } });
    await runtime.childEvent({ type: "notification", notification: subdriverReturn(driverID, "stale", "Shephrd") });
    await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
    await runtime.emit("agent_settled");
    assert.equal(runtime.messages.length, 0);
    assert.equal(runtime.statuses.length, 0);
    assert.equal(runtime.commands().filter(value => value.op === "ack").length, 0);
    await runtime.emit("session_shutdown");
  }
});

test("human names show the registered repository or General for sub-drivers and the task for workers", () => {
  assert.equal(notificationSourceLabel({ request_id: "request_one", subdriver_id: "coord_one", subdriver_repo_name: "Shephrd", task_id: "", task_label: "request_one" }), "Sub-driver: Shephrd");
  assert.equal(notificationSourceLabel({ request_id: "request_one", subdriver_id: "coord_one", task_id: "", task_label: "request_one" }), "Sub-driver: General");
  assert.equal(notificationSourceLabel({ task_id: "task_1", task_label: "demo/Choose the release option [release-choice]" }), "Worker demo/Choose the release option [release-choice]");
  assert.equal(notificationSourceLabel({ task_id: "task_1", task_title: "Choose" }), "Worker Choose");
});

test("accepted receipt is shown immediately while idle and cleared only by the settled acknowledgement", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  assert.equal(runtime.indicators.length, 0);
  assert.equal(runtime.statuses.length, 0);
  await runtime.childEvent({ type: "notification", notification: subdriverReturn(driverID, generation, "Shephrd") });
  assert.equal(runtime.indicators.length, 1);
  assert.equal(runtime.indicators[0].text, "Shephrd received Sub-driver: Shephrd subdriver-question (wake:1); handling turn starting");
  assert.deepEqual(runtime.statuses.map(status => [status.key, status.text]), [["shephrd-wake", "● Sub-driver: Shephrd subdriver-question received"]]);
  assert.ok(runtime.indicators[0].at <= runtime.deliveries[0].at);
  assert.equal(runtime.notices.length, 0);
  await runtime.childEvent({ type: "notification", notification: subdriverReturn(driverID, generation, "Shephrd") });
  assert.equal(runtime.indicators.length, 1);
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 0);
  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[0] }] } });
  assert.equal(runtime.statuses.at(-1)!.text, "● Sub-driver: Shephrd subdriver-question handling");
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 1);
  assert.equal(runtime.statuses.at(-1)!.text, "● Sub-driver: Shephrd subdriver-question handling");
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: true, handling_id: "handling:generated" });
  assert.equal(runtime.statuses.at(-1)!.text, undefined);
  assert.equal(runtime.indicators.length, 1);
  assert.equal(runtime.notices.length, 0);
  await runtime.emit("session_shutdown");
});

test("busy receipts distinguish General sub-drivers from workers and clear on claim loss without acknowledgement", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  runtime.setIdle(false);
  await runtime.childEvent({ type: "notification", notification: subdriverReturn(driverID, generation) });
  assert.equal(runtime.indicators[0].text, "Shephrd received Sub-driver: General subdriver-question (wake:1); queued until the current turn settles");
  assert.equal(runtime.statuses.at(-1)!.text, "● Sub-driver: General subdriver-question received");
  assert.match(runtime.messages[0], /^Shephrd Sub-driver: General return wake:1/);
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 0);
  await runtime.childEvent({ type: "invalidated", notification_id: "wake:1", claim_token: "first", driver_generation: generation, error_kind: "claim_conflict", detail: "superseded" });
  assert.equal(runtime.statuses.at(-1)!.text, undefined);
  assert.equal(runtime.notices.length, 0);
  await runtime.childEvent({ type: "notification", notification: { ...notification(driverID, generation, "second"), notification_id: "wake:2" } });
  assert.equal(runtime.indicators.length, 2);
  assert.equal(runtime.indicators[1].text, "Shephrd received Worker demo/Choose the release option [release-choice] question (wake:2); queued until the current turn settles");
  assert.equal(runtime.statuses.at(-1)!.text, "● Worker demo/Choose the release option [release-choice] question received");
  assert.match(runtime.messages[1], /^Shephrd notification wake:2: task task_1/);
  await runtime.emit("input", { text: runtime.messages[1], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[1] }] } });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  const ack = runtime.commands().find(value => value.op === "ack");
  assert.equal(ack.notification_id, "wake:2");
  assert.equal(ack.claim_token, "second");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 1);
  await runtime.emit("session_shutdown");
  assert.equal(runtime.statuses.at(-1)!.text, undefined);
  assert.equal(runtime.indicators.length, 2);
  assert.equal(runtime.notices.length, 0);
});

test("watcher polling defaults to one second and retains explicit bounds", () => {
  for (const [section, pollMin, pollMax] of [
    ["", 1000, 1000],
    ['poll_max = "15s"', 1000, 15000],
    ['poll_min = "2s"', 2000, 2000],
    ['poll_min = "250ms"\npoll_max = "500ms"', 250, 500],
  ] as const) {
    assert.deepEqual(watcherSettings({
      env: {}, homedir: () => "/home/test",
      readFileSync: (() => `[pi_watcher]\nenabled = true\n${section}\n`) as any,
    }), { enabled: true, pollMin, pollMax });
  }
  assert.deepEqual(watcherSettings({
    env: { SHEPHRD_PI_WATCHER_ENABLED: "1" }, homedir: () => "/home/test",
    readFileSync: (() => { throw new Error("missing config"); }) as any,
  }), { enabled: true, pollMin: 1000, pollMax: 1000 });
});

test("watcher settings enforce valid polling and renewal bounds", () => {
  const value = watcherSettings({
    env: { SHEPHRD_CONFIG: "/config" },
    homedir: () => "/home/test",
    readFileSync: (() => "[pi_watcher]\nenabled = true\npoll_min = \"0s\"\npoll_max = \"1ms\"\n") as any,
  });
  assert.deepEqual(value, { enabled: true, pollMin: 1000, pollMax: 1000 });
  assert.equal(renewalIntervalMilliseconds("2026-01-01T00:00:30.000Z", Date.parse("2026-01-01T00:00:00.000Z")), 15000);
  assert.equal(renewalIntervalMilliseconds("invalid", 0), 1000);
});

test("SHEPHRD_PI_WATCHER_ENABLED=0 overrides enabled config", () => {
  const value = watcherSettings({
    env: { SHEPHRD_PI_WATCHER_ENABLED: "0", SHEPHRD_CONFIG: "/config" },
    homedir: () => "/home/test",
    readFileSync: (() => "[pi_watcher]\nenabled = true\n") as any,
  });
  assert.equal(value.enabled, false);
});

test("JSON and print sessions never start a watcher child", async () => {
  for (const mode of ["json", "print"] as const) {
    const runtime = harness(mode);
    await runtime.emit("session_start");
    assert.equal(runtime.spawns.length, 0);
    const prompt = await runtime.emit("before_agent_start", { systemPrompt: "base" });
    assert.deepEqual(prompt, [undefined]);
  }
});

test("TUI lifecycle owns one child and injects the asynchronous driver instruction", async () => {
  const runtime = harness("tui");
  await runtime.emit("session_start");
  assert.equal(runtime.spawns.length, 1);
  const wakeArgs = JSON.parse(runtime.spawns[0].options.env.SHEPHRD_WAKE_ARGS);
  assert.deepEqual(wakeArgs.slice(0, 4), ["wake", "drain", "--limit", "1"]);
  assert.equal(wakeArgs[wakeArgs.indexOf("--driver-id") + 1], "driver:pi:session-owner");

  const [prompt] = await runtime.emit("before_agent_start", { systemPrompt: "base" });
  assert.match(prompt.systemPrompt, /watcher is active/);
  assert.match(prompt.systemPrompt, /Do not sleep, poll status, manually drain notifications/);

  await runtime.emit("session_start");
  assert.equal(runtime.spawns.length, 2);
  assert.equal(runtime.children[0].kills, 1);
  await runtime.emit("session_shutdown");
  assert.equal(runtime.children[1].kills, 1);
});

test("basic task prompt adds lifecycle safety without workflow mandates", async () => {
  const runtime = harness("tui");
  await runtime.emit("session_start");
  const [prompt] = await runtime.emit("before_agent_start", { prompt: "Change one alias in .zshrc", systemPrompt: "base" });
  const guidance = prompt.systemPrompt;
  assert.ok(guidance.startsWith("base\n\n"));
  assert.ok(Buffer.byteLength(guidance) < 1200);
  assertWorkflowNeutral(guidance);
  assert.match(guidance, /current ownership, task, attempt, workspace, artifact, and process facts and user authorization/);
  assert.match(guidance, /Preserve held work when uncertain/);
  assert.match(guidance, /Do not sleep, poll status, manually drain notifications, or keep the turn open waiting for a worker/);
  assert.match(guidance, /watcher.*owns notification consumption/);
  assert.match(guidance, /each wake and its obligations as read-only evidence/);
  assert.match(guidance, /never lifecycle authority/);
  assert.match(guidance, /acknowledgement does not require further lifecycle actions/);
  await runtime.emit("session_shutdown");
});

function assertWorkflowNeutral(text: string) {
  assert.doesNotMatch(text, /delegat|independent review|re-review|create.*review|create.*\bPR\b|pull request|model.family|Fable|Luna|Sol\b|decompos|successor|run.to.boundary|parallel.batch|single.spawn.end|disposition this work now/i);
}

test("basic completion is acknowledged without review or successor actions", async () => {
  const runtime = harness("tui");
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({
    type: "notification",
    notification: { ...notification(driverID, generation), task_title: "Update dotfile", task_label: "Update dotfile", kind: "done", payload: "Changed one alias; branch:shephrd/task_1" },
    obligations: obligations(0, 1, 1, 1),
  });
  assertWorkflowNeutral(runtime.messages[0]);
  await runtime.emit("before_agent_start", { systemPrompt: "base" });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop", content: "The branch is ready. No landing was requested." } });
  await runtime.emit("agent_settled");
  assert.deepEqual(runtime.commands().map(command => command.op), ["ack"]);
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: true, handling_id: "handling:generated", obligations: obligations(0, 1, 1, 1) });
  const [next] = await runtime.emit("before_agent_start", { systemPrompt: "base" });
  assertWorkflowNeutral(next.systemPrompt + next.message.content);
  assert.match(next.message.content, /not instructions to start more work/);
  assert.equal(runtime.messages.length, 1);
  assert.equal(runtime.spawns.length, 1);
  await runtime.emit("session_shutdown");
});

test("wake-turn start includes bounded owner obligation counts with the claimed notification", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: { ...notification(driverID, generation), kind: "settled" }, obligations: obligations(1, 2, 3, 4) });
  assert.equal(runtime.messages.length, 1);
  assert.match(runtime.messages[0], /Owner obligations at wake-turn start \(schema 2, read-only counts\): 1 act now \| 2 need disposition \| 3 results ready \| 4 planned ready/);
  assert.match(runtime.messages[0], /settled wake is deferred-disposition evidence only/);
  assert.match(runtime.messages[0], /Handle this notification within the user's scope after inspecting current state/);
  assertWorkflowNeutral(runtime.messages[0]);
  await runtime.emit("session_shutdown");
});

test("post-turn obligation residue is preserved once for the next turn", async () => {
  const runtime = harness("tui");
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation), obligations: obligations(2, 2, 1, 1) });
  await runtime.emit("before_agent_start", { systemPrompt: "base" });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: true, handling_id: "handling:generated", obligations: obligations(1, 2, 0, 1) });
  assert.deepEqual(runtime.entries, [{
    type: "shephrd-wake",
    data: {
      notification_id: "wake:1",
      handling_id: "handling:generated",
      obligations: {
        notification_id: "wake:1",
        counts: obligations(1, 2, 0, 1).counts,
        delta: obligations(-1, 0, -1, 0).counts,
      },
    },
  }]);
  const [next] = await runtime.emit("before_agent_start", { systemPrompt: "base" });
  assert.match(next.message.content, /1 act now \| 2 need disposition \| 0 results ready \| 1 planned ready/);
  assert.match(next.message.content, /Change since wake start: -1 act now \| 0 need disposition \| -1 results ready \| 0 planned ready/);
  const [later] = await runtime.emit("before_agent_start", { systemPrompt: "base" });
  assert.equal(later.message, undefined);
  await runtime.emit("session_shutdown");
});

test("persisted residue survives restart without duplicate notification consumption", async () => {
  const residue = {
    notification_id: "wake:1",
    counts: obligations(0, 1, 1, 0).counts,
    delta: obligations(0, -1, 1, 0).counts,
  };
  const branch = [{ type: "custom", customType: "shephrd-wake", data: { notification_id: "wake:1", handling_id: "handling:stored", obligations: residue } }];
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false, branch);
  await runtime.emit("session_start");
  const [next] = await runtime.emit("before_agent_start", { systemPrompt: "base" });
  assert.match(next.message.content, /0 act now \| 1 need disposition \| 1 results ready \| 0 planned ready/);
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation), obligations: obligations(8, 8, 8, 8) });
  assert.equal(runtime.messages.length, 0);
  await runtime.emit("session_shutdown");
});

test("quiescent final obligations add no next-turn context", async () => {
  const runtime = harness("tui");
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation), obligations: obligations(0, 1, 0, 0) });
  await runtime.emit("before_agent_start", { systemPrompt: "base" });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: true, handling_id: "handling:generated", obligations: obligations() });
  assert.deepEqual(runtime.entries, [{ type: "shephrd-wake", data: { notification_id: "wake:1", handling_id: "handling:generated" } }]);
  const [next] = await runtime.emit("before_agent_start", { systemPrompt: "base" });
  assert.equal(next.message, undefined);
  await runtime.emit("session_shutdown");
});

test("obligations command failures stay bounded and do not duplicate notification consumption", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  const notice = { ...notification(driverID, generation), payload: "payload ".repeat(2000) };
  await runtime.childEvent({ type: "notification", notification: notice, obligations: { error: "initial ".repeat(1000) } });
  assert.equal(runtime.messages.length, 1);
  assert.ok(runtime.messages[0].length <= 4096);
  assert.match(runtime.messages[0], /Owner obligations at wake-turn start unavailable/);
  assert.match(runtime.messages[0], /lifecycle authority\.$/);
  assert.ok((runtime.messages[0].match(/initial/g) || []).length < 50);
  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: runtime.messages[0] } });
  await runtime.emit("before_agent_start", { systemPrompt: "base" });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: true, handling_id: "handling:generated", obligations: { error: "final ".repeat(1000) } });
  const [next] = await runtime.emit("before_agent_start", { systemPrompt: "base" });
  assert.ok(next.message.content.length < 512);
  assert.match(next.message.content, /final owner obligations snapshot before acknowledgement was unavailable/);
  await runtime.childEvent({ type: "notification", notification: notice, obligations: obligations(9, 9, 9, 9) });
  assert.equal(runtime.messages.length, 1);
  await runtime.emit("session_shutdown");
});

test("notification acknowledgement waits for confirmed Pi input and the matching turn", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation) });
  assert.equal(runtime.messages.length, 1);
  assert.match(runtime.messages[0], /demo\/Choose the release option \[release-choice\], feature release-choice/);

  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 0);

  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[0] }] } });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  const acknowledgement = runtime.commands().find(value => value.op === "ack");
  assert.ok(acknowledgement);
  assert.equal("handling_id" in acknowledgement, false);
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 1);
  await runtime.emit("session_shutdown");
});

test("report notification presents lifecycle failure and explicit recovery alongside the worker payload", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  const notice = {
    ...notification(driverID, generation),
    kind: "done",
    payload: "report complete",
    report_lifecycle: [{
      handler_name: "memory",
      state: "unknown",
      failure_kind: "extension_exit",
      failure_message: "response was interrupted",
      retry_command: "shephrd task verify-delivery task_1 --json",
    }],
  };
  await runtime.childEvent({ type: "notification", notification: notice });
  assert.equal(runtime.messages.length, 1);
  assert.match(runtime.messages[0], /Report lifecycle outcomes/);
  assert.match(runtime.messages[0], /report\.accepted handler memory: unknown - response was interrupted/);
  assert.match(runtime.messages[0], /shephrd task verify-delivery task_1 --json/);
  assert.match(runtime.messages[0], /report complete/);
  await runtime.emit("session_shutdown");
});

test("superseded acknowledgement clears local delivery without a user notice", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation) });
  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[0] }] } });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  await runtime.childEvent({ type: "invalidated", notification_id: "wake:1", claim_token: "first", driver_generation: generation, error_kind: "claim_conflict", detail: "superseded" });
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 1);
  assert.equal(runtime.notices.length, 0);
  await runtime.emit("session_shutdown");
});

test("stale terminal invalidation clears local delivery without a user notice", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation) });
  await runtime.childEvent({ type: "invalidated", notification_id: "wake:1", claim_token: "first", driver_generation: generation, error_kind: "claim_stale", detail: "stale" });
  assert.equal(runtime.messages.length, 1);
  assert.equal(runtime.notices.length, 0);
  await runtime.emit("session_shutdown");
});

test("terminal renewal failure clears the parent claim without a user notice", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation, "first", Date.now() + 2500) });
  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.childEvent({ type: "invalidated", notification_id: "wake:1", claim_token: "first", driver_generation: generation, error_kind: "claim_expired", detail: "expired" });
  await new Promise(resolve => setTimeout(resolve, 1100));
  assert.equal(runtime.commands().filter(value => value.op === "renew").length, 0);
  assert.equal(runtime.notices.length, 0);
  await runtime.emit("session_shutdown");
});

test("agent_settled during transient acknowledgement backoff does not overlap retry streams", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation) });
  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[0] }] } });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: false, error_kind: "transient", detail: "temporary" });
  assert.equal(runtime.notices.some(value => value.includes("acknowledgement failed")), true);
  await runtime.emit("agent_settled");
  await new Promise(resolve => setTimeout(resolve, 1100));
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 2);
  await runtime.emit("session_shutdown");
});

test("transient acknowledgement failure retries through the bounded scheduler", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation) });
  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[0] }] } });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: false, error_kind: "transient", detail: "temporary" });
  await new Promise(resolve => setTimeout(resolve, 1100));
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 2);
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: true, handling_id: "handling:generated" });
  assert.deepEqual(runtime.entries, [{ type: "shephrd-wake", data: { notification_id: "wake:1", handling_id: "handling:generated" } }]);
  await runtime.emit("session_shutdown");
});

test("aborted notification turns remain unacknowledged", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation) });
  await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[0] }] } });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "aborted" } });
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 0);
  await runtime.emit("session_shutdown");
});

test("watcher child exit disables ownership and active instructions", async () => {
  const runtime = harness("tui");
  await runtime.emit("session_start");
  assert.equal(runtime.owners.size, 1);
  runtime.children[0].emit("exit");
  assert.equal(runtime.owners.size, 0);
  const [prompt] = await runtime.emit("before_agent_start", { prompt: "next", systemPrompt: "base" });
  assert.equal(prompt, undefined);
});

test("unconfirmed injection expires without duplicate delivery and can be retried", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  const first = notification(driverID, generation);
  await runtime.childEvent({ type: "notification", notification: first });
  await runtime.emit("agent_settled");
  assert.equal(runtime.commands().filter(value => value.op === "ack").length, 0);

  await runtime.childEvent({ type: "expired", notification_id: "wake:1", claim_token: "first", driver_generation: generation });
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation, "second") });
  assert.equal(runtime.messages.length, 2);
  await runtime.emit("session_shutdown");
});

test("stale expiry and acknowledgement events do not clear a newer delivery", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation, "first") });
  await runtime.childEvent({ type: "expired", notification_id: "wake:1", claim_token: "first", driver_generation: generation });
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation, "second") });
  await runtime.childEvent({ type: "expired", notification_id: "wake:1", claim_token: "first", driver_generation: generation });
  await runtime.childEvent({ type: "ack", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: true });
  await runtime.emit("input", { text: runtime.messages[1], source: "extension" });
  await runtime.emit("message_start", { message: { role: "user", content: [{ type: "text", text: runtime.messages[1] }] } });
  await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
  await runtime.emit("agent_settled");
  const ack = runtime.commands().findLast(value => value.op === "ack");
  assert.equal(ack.claim_token, "second");
  await runtime.emit("session_shutdown");
});

test("stale renewal events are ignored", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "1" }, false);
  await runtime.emit("session_start");
  const { driverID, generation } = identity(runtime);
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation, "first") });
  await runtime.childEvent({ type: "expired", notification_id: "wake:1", claim_token: "first", driver_generation: generation });
  await runtime.childEvent({ type: "notification", notification: notification(driverID, generation, "second") });
  await runtime.childEvent({ type: "renew", notification_id: "wake:1", claim_token: "first", driver_generation: generation, ok: false, detail: "stale" });
  assert.equal(runtime.notices.some(value => value.includes("renewal failed")), false);
  await runtime.emit("session_shutdown");
});

test("watcher child accepts acknowledgement while idle", async () => {
  const directory = mkdtempSync(join(tmpdir(), "shephrd-watcher-"));
  const executable = join(directory, "shephrd");
  const log = join(directory, "calls.jsonl");
  writeFileSync(executable, `#!/bin/sh\nprintf '%s\\n' "$*" >> "$SHEPHRD_TEST_LOG"\ncase "$1 $2" in\n  'wake drain') printf '%s\\n' "$SHEPHRD_TEST_NOTIFICATION" ;;\n  'wake ack') printf '%s\\n' '{"schema_version":1,"notification_id":"wake:1","consumer_id":"driver:test","driver_generation":"generation:test","handling_id":"handling:generated"}' ;;\n  'wake renew') printf '%s\\n' '{"claim_until":"2099-01-01T00:00:00Z"}' ;;\nesac\n`);
  chmodSync(executable, 0o755);
  const value = notification("driver:test", "generation:test", "token:test", Date.now() + 600000);
  const child = spawnProcess(process.execPath, ["-e", watcherChild], {
    env: {
      ...process.env,
      SHEPHRD_EXECUTABLE: executable,
      SHEPHRD_TEST_LOG: log,
      SHEPHRD_TEST_NOTIFICATION: JSON.stringify({ notifications: [value] }).replaceAll("'", "'\\''"),
      SHEPHRD_WAKE_ARGS: JSON.stringify(["wake", "drain", "--driver-id", "driver:test", "--driver-generation", "generation:test", "--json"]),
      SHEPHRD_PI_POLL_MIN: "1",
      SHEPHRD_PI_POLL_MAX: "2",
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  try {
    let output = "";
    const lines: string[] = [];
    const waiters: Array<(line: string) => void> = [];
    child.stdout.on("data", chunk => {
      output += String(chunk);
      const parts = output.split("\n");
      output = parts.pop() || "";
      for (const line of parts) {
        const waiter = waiters.shift();
        if (waiter) waiter(line);
        else lines.push(line);
      }
    });
    const nextLine = () => lines.length > 0
      ? Promise.resolve(lines.shift()!)
      : Promise.race([
          new Promise<string>(resolve => waiters.push(resolve)),
          new Promise<never>((_, reject) => setTimeout(() => reject(new Error("watcher event timed out")), 5000)),
        ]);
    assert.equal(JSON.parse(await nextLine()).type, "notification");
    child.stdin.write(JSON.stringify({ op: "renew", notification_id: "wake:1", claim_token: "token:test", consumer_id: "driver:test", driver_generation: "generation:test" }) + "\n");
    assert.equal(JSON.parse(await nextLine()).type, "renew");
    child.stdin.write(JSON.stringify({ op: "ack", notification_id: "wake:1", claim_token: "token:test", consumer_id: "driver:test", driver_generation: "generation:test" }) + "\n");
    const acknowledgement = JSON.parse(await nextLine());
    assert.equal(acknowledgement.type, "ack");
    assert.equal(acknowledgement.handling_id, "handling:generated");
    const calls = readFileSync(log, "utf8").trim().split("\n");
    assert.equal(calls.some(call => call === "wake renew --claim-token token:test --driver-id driver:test --json"), true);
    assert.equal(calls.some(call => call === "wake ack --claim-token token:test --driver-id driver:test --json"), true);
  } finally {
    child.stdin.write(JSON.stringify({ op: "stop" }) + "\n");
    await Promise.race([once(child, "exit"), new Promise(resolve => setTimeout(resolve, 1000))]);
    child.kill();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("watcher child takes two bounded owner obligation snapshots and caches the final across ack retry", async () => {
  const directory = mkdtempSync(join(tmpdir(), "shephrd-watcher-"));
  const executable = join(directory, "shephrd");
  const log = join(directory, "calls.jsonl");
  const obligationCount = join(directory, "obligation-count");
  const ackCount = join(directory, "ack-count");
  const value = notification("driver:test", "generation:test", "token:test", Date.now() + 600000);
  writeFileSync(executable, `#!/bin/sh
printf '%s\n' "$*" >> "$SHEPHRD_TEST_LOG"
case "$1 $2" in
  'wake drain') printf '%s\n' "$SHEPHRD_TEST_NOTIFICATION" ;;
  'task obligations')
    count=$(cat "$SHEPHRD_TEST_OBLIGATION_COUNT" 2>/dev/null || echo 0)
    count=$((count + 1))
    printf '%s' "$count" > "$SHEPHRD_TEST_OBLIGATION_COUNT"
    if [ "$count" -eq 1 ]; then
      printf '%s\n' '{"schema_version":2,"scope":{"driver_id":"driver:test","all_drivers":false},"counts":{"act_now":1,"needs_disposition":2,"result_ready":3,"planned_ready":4}}'
    else
      head -c 4096 /dev/zero | tr '\\0' x >&2
      exit 1
    fi ;;
  'wake ack')
    count=$(cat "$SHEPHRD_TEST_ACK_COUNT" 2>/dev/null || echo 0)
    count=$((count + 1))
    printf '%s' "$count" > "$SHEPHRD_TEST_ACK_COUNT"
    if [ "$count" -eq 1 ]; then
      printf '%s\n' '{"error_kind":"transient"}' >&2
      exit 1
    fi
    printf '%s\n' '{"schema_version":1,"notification_id":"wake:1","consumer_id":"driver:test","driver_generation":"generation:test","handling_id":"handling:generated"}' ;;
esac
`);
  chmodSync(executable, 0o755);
  const child = spawnProcess(process.execPath, ["-e", watcherChild], {
    env: {
      ...process.env,
      SHEPHRD_EXECUTABLE: executable,
      SHEPHRD_TEST_LOG: log,
      SHEPHRD_TEST_OBLIGATION_COUNT: obligationCount,
      SHEPHRD_TEST_ACK_COUNT: ackCount,
      SHEPHRD_TEST_NOTIFICATION: JSON.stringify({ notifications: [value] }),
      SHEPHRD_WAKE_ARGS: JSON.stringify(["wake", "drain", "--driver-id", "driver:test", "--driver-generation", "generation:test", "--json"]),
      SHEPHRD_PI_POLL_MIN: "1",
      SHEPHRD_PI_POLL_MAX: "2",
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  try {
    let output = "";
    const lines: string[] = [];
    const waiters: Array<(line: string) => void> = [];
    child.stdout.on("data", chunk => {
      output += String(chunk);
      const parts = output.split("\n");
      output = parts.pop() || "";
      for (const line of parts) {
        const waiter = waiters.shift();
        if (waiter) waiter(line);
        else lines.push(line);
      }
    });
    const nextLine = () => lines.length > 0
      ? Promise.resolve(lines.shift()!)
      : Promise.race([
          new Promise<string>(resolve => waiters.push(resolve)),
          new Promise<never>((_, reject) => setTimeout(() => reject(new Error("watcher event timed out")), 5000)),
        ]);
    const first = JSON.parse(await nextLine());
    assert.equal(first.type, "notification");
    assert.deepEqual(first.obligations, obligations(1, 2, 3, 4));
    const acknowledgement = { op: "ack", notification_id: "wake:1", claim_token: "token:test", consumer_id: "driver:test", driver_generation: "generation:test" };
    child.stdin.write(JSON.stringify(acknowledgement) + "\n");
    const failed = JSON.parse(await nextLine());
    assert.equal(failed.type, "ack");
    assert.equal(failed.ok, false);
    assert.equal(failed.obligations.error.length, 256);
    child.stdin.write(JSON.stringify(acknowledgement) + "\n");
    const succeeded = JSON.parse(await nextLine());
    assert.equal(succeeded.type, "ack");
    assert.equal(succeeded.ok, true);
    assert.deepEqual(succeeded.obligations, failed.obligations);
    child.stdin.write(JSON.stringify({ op: "stop" }) + "\n");
    await Promise.race([once(child, "exit"), new Promise(resolve => setTimeout(resolve, 1000))]);
    const calls = readFileSync(log, "utf8").trim().split("\n");
    assert.deepEqual(calls.filter(call => !call.startsWith("wake pump ")).slice(0, 4), [
      "wake drain --driver-id driver:test --driver-generation generation:test --json",
      "task obligations --driver-id driver:test --limit 1 --json",
      "task obligations --driver-id driver:test --limit 1 --json",
      "wake ack --claim-token token:test --driver-id driver:test --json",
    ]);
    assert.equal(calls.filter(call => call.startsWith("task obligations ")).length, 2);
    assert.equal(calls.filter(call => call.startsWith("wake ack ")).length, 2);
  } finally {
    child.kill();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("terminal child claim loss drains the next notification immediately", async () => {
  const directory = mkdtempSync(join(tmpdir(), "shephrd-watcher-"));
  const executable = join(directory, "shephrd");
  const count = join(directory, "count");
  const first = notification("driver:test", "generation:test", "first", Date.now() + 600000);
  const second = notification("driver:test", "generation:test", "second", Date.now() + 600000);
  writeFileSync(executable, `#!/bin/sh
count=$(cat "$SHEPHRD_TEST_COUNT" 2>/dev/null || echo 0)
count=$((count + 1))
printf '%s' "$count" > "$SHEPHRD_TEST_COUNT"
case "$1 $2" in
  'wake drain') if [ "$count" -eq 1 ]; then printf '%s\\n' "$SHEPHRD_TEST_FIRST"; else printf '%s\\n' "$SHEPHRD_TEST_SECOND"; fi ;;
  'wake ack') printf '%s\\n' '{"error":"notification superseded","error_kind":"claim_conflict"}' >&2; exit 1 ;;
esac
`);
  chmodSync(executable, 0o755);
  const child = spawnProcess(process.execPath, ["-e", watcherChild], {
    env: {
      ...process.env,
      SHEPHRD_EXECUTABLE: executable,
      SHEPHRD_TEST_COUNT: count,
      SHEPHRD_TEST_FIRST: JSON.stringify({ notifications: [first] }),
      SHEPHRD_TEST_SECOND: JSON.stringify({ notifications: [second] }),
      SHEPHRD_WAKE_ARGS: JSON.stringify(["wake", "drain", "--driver-id", "driver:test", "--driver-generation", "generation:test", "--json"]),
      SHEPHRD_PI_POLL_MIN: "1",
      SHEPHRD_PI_POLL_MAX: "2",
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  try {
    let output = "";
    const lines: string[] = [];
    const waiters: Array<(line: string) => void> = [];
    child.stdout.on("data", chunk => {
      output += String(chunk);
      const parts = output.split("\n");
      output = parts.pop() || "";
      for (const line of parts) {
        const waiter = waiters.shift();
        if (waiter) waiter(line);
        else lines.push(line);
      }
    });
    const nextLine = () => lines.length > 0
      ? Promise.resolve(lines.shift()!)
      : Promise.race([
          new Promise<string>(resolve => waiters.push(resolve)),
          new Promise<never>((_, reject) => setTimeout(() => reject(new Error("watcher event timed out")), 5000)),
        ]);
    assert.equal(JSON.parse(await nextLine()).type, "notification");
    child.stdin.write(JSON.stringify({ op: "ack", notification_id: "wake:1", claim_token: "first", consumer_id: "driver:test", driver_generation: "generation:test" }) + "\n");
    const invalidated = JSON.parse(await nextLine());
    assert.equal(invalidated.type, "invalidated");
    assert.equal(invalidated.error_kind, "claim_conflict");
    const next = JSON.parse(await nextLine());
    assert.equal(next.type, "notification");
    assert.equal(next.notification.claim.claim_token, "second");
  } finally {
    child.stdin.write(JSON.stringify({ op: "stop" }) + "\n");
    await new Promise(resolve => setTimeout(resolve, 20));
    child.kill();
    rmSync(directory, { recursive: true, force: true });
  }
});

for (const mode of ["supported", "legacy", "legacy-command", "transient"]) {
  test(`watcher pump is serial and preserves claim delivery: ${mode}`, { timeout: 10000 }, async () => {
    const directory = mkdtempSync(join(tmpdir(), "shephrd-pump-"));
    const executable = join(directory, "shephrd");
    const log = join(directory, "calls");
    const value = notification("driver:test", "generation:test", "token:test", Date.now() + 600000);
    writeFileSync(log, "");
    writeFileSync(executable, `#!${process.execPath}
const { appendFileSync } = require("node:fs");
const op = process.argv[3];
const log = value => appendFileSync(${JSON.stringify(log)}, value + "\\n");
log(op);
if (op === "drain") console.log(${JSON.stringify(JSON.stringify({ notifications: [value] }))});
if (op === "pump") {
  if (${JSON.stringify(mode)}.startsWith("legacy")) {
    console.error(JSON.stringify({ error: ${JSON.stringify(mode)} === "legacy" ? "unknown flag: --driver-id" : 'unknown command "pump" for "shephrd wake"' }));
    process.exit(1);
  }
  setTimeout(() => {
    log("pump-end");
    if (${JSON.stringify(mode)} === "transient") {
      console.error(JSON.stringify({ error: "database is locked" }));
      process.exitCode = 1;
    } else console.log('{}');
  }, 100);
}
if (op === "renew") console.log('{"claim_until":"2099-01-01T00:00:00Z"}');
if (op === "ack") console.log('{"schema_version":1,"notification_id":"wake:1","consumer_id":"driver:test","driver_generation":"generation:test","handling_id":"handling:generated"}');
`);
    chmodSync(executable, 0o700);
    const child = spawnProcess(process.execPath, ["-e", watcherChild], {
      env: { ...process.env, SHEPHRD_EXECUTABLE: executable,
        SHEPHRD_WAKE_ARGS: JSON.stringify(["wake", "drain", "--driver-id", "driver:test", "--driver-generation", "generation:test", "--json"]),
        SHEPHRD_PI_POLL_MIN: "10", SHEPHRD_PI_POLL_MAX: "20" },
      stdio: ["pipe", "pipe", "pipe"],
    });
    const events: any[] = [];
    let buffer = "";
    child.stdout.on("data", chunk => {
      buffer += String(chunk);
      const lines = buffer.split("\n");
      buffer = lines.pop() || "";
      for (const line of lines) events.push(JSON.parse(line));
    });
    const calls = () => readFileSync(log, "utf8").trim().split("\n");
    const waitFor = async (predicate: () => boolean) => {
      const deadline = Date.now() + 5000;
      while (!predicate()) {
        assert.ok(Date.now() < deadline, JSON.stringify({ events, calls: calls() }));
        await new Promise(resolve => setTimeout(resolve, 10));
      }
    };
    const send = (op: string) => child.stdin.write(JSON.stringify({ op, notification_id: value.notification_id, claim_token: value.claim.claim_token, consumer_id: "driver:test", driver_generation: "generation:test" }) + "\n");
    try {
      await waitFor(() => events.some(event => event.type === "notification"));
      if (mode.startsWith("legacy")) {
        await waitFor(() => events.some(event => event.type === "error"));
        assert.match(events.find(event => event.type === "error").detail, /lacks wake pump.*drain-coupled/);
        await new Promise(resolve => setTimeout(resolve, 300));
        assert.equal(calls().filter(call => call === "pump").length, 1);
        assert.equal(events.filter(event => event.type === "error").length, 1);
      } else {
        await waitFor(() => calls().filter(call => call === "pump-end").length >= 3);
        const pumps = calls().filter(call => call.startsWith("pump"));
        pumps.forEach((call, index) => assert.equal(call, index % 2 === 0 ? "pump" : "pump-end"));
        if (mode === "transient") assert.ok(events.filter(event => event.type === "error").length >= 2);
        else assert.equal(events.filter(event => event.type === "error").length, 0);
      }
      assert.equal(calls().filter(call => call === "drain").length, 1);
      assert.equal(calls().filter(call => call === "ack").length, 0);
      assert.equal(events.filter(event => event.type === "notification").length, 1);
      send("renew");
      await waitFor(() => events.some(event => event.type === "renew" && event.ok));
      send("ack");
      await waitFor(() => events.some(event => event.type === "ack" && event.ok));
      assert.equal(calls().filter(call => call === "ack").length, 1);
    } finally {
      const exit = once(child, "exit");
      send("stop");
      child.stdin.end();
      await exit;
      rmSync(directory, { recursive: true, force: true });
    }
  });
}

for (const op of ["drain", "pump"]) {
  test(`stop never reports a killed in-flight ${op} whose orphan still completes`, { timeout: 10000 }, async () => {
    const directory = mkdtempSync(join(tmpdir(), "shephrd-stop-"));
    const executable = join(directory, "shephrd");
    const inner = join(directory, "inner");
    const started = join(directory, "started");
    const release = join(directory, "release");
    const value = notification("driver:test", "generation:test", "token:test", Date.now() + 600000);
    writeFileSync(inner, `#!${process.execPath}
const { existsSync, writeFileSync } = require("node:fs");
const op = process.argv[3];
if (op === "drain" && ${JSON.stringify(op)} === "pump") return console.log(${JSON.stringify(JSON.stringify({ notifications: [value] }))});
if (op !== ${JSON.stringify(op)}) return;
writeFileSync(${JSON.stringify(started)}, "");
const poll = setInterval(() => {
  if (!existsSync(${JSON.stringify(release)})) return;
  clearInterval(poll);
  console.log(op === "drain" ? '{"notifications":[]}' : "{}");
}, 5);
`);
    writeFileSync(executable, `#!/bin/sh\n${JSON.stringify(inner)} "$@"\nexit "$?"\n`);
    chmodSync(inner, 0o700);
    chmodSync(executable, 0o700);
    const child = spawnProcess(process.execPath, ["-e", watcherChild], {
      env: { ...process.env, SHEPHRD_EXECUTABLE: executable,
        SHEPHRD_WAKE_ARGS: JSON.stringify(["wake", "drain", "--driver-id", "driver:test", "--driver-generation", "generation:test", "--json"]),
        SHEPHRD_PI_POLL_MIN: "1", SHEPHRD_PI_POLL_MAX: "1" },
      stdio: ["pipe", "pipe", "pipe"],
    });
    let output = "";
    child.stdout.on("data", chunk => { output += String(chunk); });
    child.stderr.on("data", chunk => { output += String(chunk); });
    try {
      const deadline = Date.now() + 5000;
      while (!existsSync(started)) {
        assert.ok(Date.now() < deadline, output);
        await new Promise(resolve => setTimeout(resolve, 5));
      }
      const exit = once(child, "exit");
      child.stdin.write(JSON.stringify({ op: "stop" }) + "\n");
      child.stdin.end();
      await new Promise(resolve => setTimeout(resolve, 50));
      writeFileSync(release, "");
      await exit;
      assert.deepEqual(output.trim().split("\n").filter(Boolean).map(line => JSON.parse(line).type), op === "pump" ? ["notification"] : []);
    } finally {
      child.kill();
      rmSync(directory, { recursive: true, force: true });
    }
  });
}

test("built CLI watcher timing", { skip: !process.env.SHEPHRD_TEST_EXECUTABLE, timeout: 90000 }, async t => {
  const root = process.env.SHEPHRD_TEST_TIMING_ROOT!;
  const executable = process.env.SHEPHRD_TEST_EXECUTABLE!;
  const request = process.env.SHEPHRD_TEST_REQUEST!;
  const busy = process.env.SHEPHRD_TEST_BUSY === "true";
  const log = join(root, "calls.jsonl");
  const wrapper = join(root, "observe-cli");
  writeFileSync(log, "");
  writeFileSync(wrapper, `#!${process.execPath}
const { spawnSync } = require("node:child_process");
const { appendFileSync } = require("node:fs");
const args = process.argv.slice(2);
const result = spawnSync(${JSON.stringify(executable)}, args, { encoding: "utf8" });
appendFileSync(${JSON.stringify(log)}, JSON.stringify({ args, at: Date.now(), status: result.status, stdout: result.stdout }) + "\\n");
process.stdout.write(result.stdout || "");
process.stderr.write(result.stderr || "");
process.exit(result.status ?? 1);
`);
  chmodSync(wrapper, 0o700);
  const calls = () => readFileSync(log, "utf8").trim().split("\n").filter(Boolean).map(line => JSON.parse(line));
  const waitFor = async (predicate: () => boolean) => {
    const deadline = Date.now() + 40000;
    while (!predicate()) {
      assert.ok(Date.now() < deadline, "timing fixture timed out");
      await new Promise(resolve => setTimeout(resolve, 10));
    }
  };
  const env = { ...process.env, SHEPHRD_EXECUTABLE: wrapper };
  const settings = watcherSettings({ env, readFileSync, homedir: () => root });
  assert.equal(settings.pollMax, process.env.SHEPHRD_TEST_POLL_MAX === "15s" ? 15000 : 1000);
  const children: ReturnType<typeof spawnProcess>[] = [];
  const receipts: Array<{ at: number; value: any }> = [];
  const commands: any[] = [];
  const runtime = harness("tui", env, !busy, [], {
    readFileSync,
    execPath: process.execPath,
    spawn: ((command, args, options) => {
      const child = spawnProcess(command, args, options);
      children.push(child);
      let buffer = "";
      child.stdout!.on("data", chunk => {
        buffer += String(chunk);
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";
        for (const line of lines) receipts.push({ at: Date.now(), value: JSON.parse(line) });
      });
      const write = child.stdin!.write.bind(child.stdin);
      child.stdin!.write = ((value: string) => {
        commands.push(JSON.parse(value));
        return write(value);
      }) as any;
      return child;
    }) as typeof spawnProcess,
  });
  const publish = (key: string) => JSON.parse(execFileSync(executable,
    ["subdriver", "return", request, key, "--kind", "question", "--key", key, "--json"],
    { env: { ...env, ...JSON.parse(process.env.SHEPHRD_TEST_FENCE!) }, encoding: "utf8" }));
  try {
    runtime.setIdle(!busy);
    await runtime.emit("session_start");
    await waitFor(() => calls().filter(call => call.args[0] === "wake" && call.args[1] === "drain").length >= 4);
    assert.equal(runtime.messages.length, 0);
    assert.equal(runtime.indicators.length, 0);
    assert.equal(calls().every(call => call.status === 0 && JSON.parse(call.stdout).notifications.length === 0), true);
    const first = publish("first");
    const second = publish("second");
    await waitFor(() => runtime.messages.length === 1);
    const receipt = receipts.find(item => item.value.type === "notification")!;
    const notice = receipt.value.notification;
    assert.equal(notice.subdriver_event_id, first.id);
    assert.equal(notice.request_id, request);
    assert.equal(notice.subdriver_repo_name || "", process.env.SHEPHRD_TEST_REPO_NAME);
    assert.equal(notice.task_id, "");
    assert.deepEqual(runtime.deliveries[0].options, { deliverAs: "followUp" });
    const expectedSource = process.env.SHEPHRD_TEST_REPO_NAME ? `Sub-driver: ${process.env.SHEPHRD_TEST_REPO_NAME}` : "Sub-driver: General";
    assert.equal(runtime.indicators.length, 1);
    assert.equal(runtime.indicators[0].text, `Shephrd received ${expectedSource} subdriver-question (${notice.notification_id}); ${busy ? "queued until the current turn settles" : "handling turn starting"}`);
    assert.equal(runtime.statuses[0].text, `● ${expectedSource} subdriver-question received`);
    assert.equal(runtime.statuses.at(-1)!.text, `● ${expectedSource} subdriver-question ${busy ? "received" : "handling"}`);
    assert.match(runtime.messages[0], new RegExp(`^Shephrd ${expectedSource.replace("-", "\\-")} return `));
    const created = Date.parse(first.created_at);
    const claimed = Date.parse(notice.claimed_at);
    assert.ok(claimed >= created);
    assert.ok(receipt.at - created < settings.pollMax + 2000, `receipt delay ${receipt.at - created}ms, cap ${settings.pollMax}ms`);
    if (settings.pollMax === 15000) assert.ok(claimed - created > 12000, "baseline must exercise backed-off polling");
    await runtime.emit("agent_settled");
    assert.equal(commands.filter(command => command.op === "ack").length, 0);
    if (busy) {
      await runtime.emit("input", { text: runtime.messages[0], source: "extension" });
      await new Promise(resolve => setTimeout(resolve, 1500));
      await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
      await runtime.emit("agent_settled");
      assert.equal(commands.filter(command => command.op === "ack").length, 0);
      assert.equal(runtime.messages.length, 1);
      await runtime.emit("message_start", { message: { role: "user", content: runtime.messages[0] } });
    }
    const injected = runtime.events.find(event => event.name === "message_start")!.at;
    assert.ok(runtime.indicators[0].at <= injected);
    await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
    assert.equal(commands.filter(command => command.op === "ack").length, 0);
    await runtime.emit("agent_settled");
    await waitFor(() => runtime.entries.length === 1 && runtime.messages.length === 2);
    assert.equal(runtime.indicators.length, 2);
    assert.equal(runtime.statuses.some(status => status.text === undefined), true);
    assert.equal(receipts.filter(item => item.value.type === "notification")[1].value.notification.subdriver_event_id, second.id);
    if (busy) {
      await runtime.emit("input", { text: runtime.messages[1], source: "extension" });
      await runtime.emit("message_start", { message: { role: "user", content: runtime.messages[1] } });
    }
    await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
    await runtime.emit("agent_settled");
    await waitFor(() => runtime.entries.length === 2);
    const drains = calls().filter(call => call.args[0] === "wake" && call.args[1] === "drain").length;
    await waitFor(() => calls().filter(call => call.args[0] === "wake" && call.args[1] === "drain").length > drains);
    assert.equal(runtime.messages.length, 2);
    assert.equal(children.length, 1);
    assert.equal(runtime.notices.length, 0);
    assert.equal(runtime.indicators.length, 2);
    assert.equal(runtime.statuses.at(-1)!.text, undefined);
    assert.equal(commands.filter(command => command.op === "ack").length, 2);
    assert.equal(calls().filter(call => call.args[0] === "task" && call.args[1] === "obligations").length, 4);
    t.diagnostic(JSON.stringify({ busy, pollMax: settings.pollMax, created: first.created_at, claimed: notice.claimed_at,
      received: new Date(receipt.at).toISOString(), indicated: new Date(runtime.indicators[0].at).toISOString(), queued: new Date(runtime.deliveries[0].at).toISOString(), injected: new Date(injected).toISOString(),
      claimMs: claimed - created, receiptMs: receipt.at - created, receiptUiMs: runtime.indicators[0].at - created, injectionMs: injected - created }));
  } finally {
    const exits = children.map(child => once(child, "exit"));
    await runtime.emit("session_shutdown");
    await Promise.all(exits);
  }
});

test("built CLI outstanding main claim activation", { skip: !process.env.SHEPHRD_TEST_WAKE_ROOT, timeout: 45000 }, async t => {
  const root = process.env.SHEPHRD_TEST_WAKE_ROOT!;
  const executable = process.env.SHEPHRD_TEST_EXECUTABLE!;
  const subdriver = process.env.SHEPHRD_TEST_SUBDRIVER!;
  const worker = process.env.SHEPHRD_TEST_WORKER!;
  const log = join(root, "watcher-calls.jsonl");
  const wrapper = join(root, "observe-cli");
  writeFileSync(log, "");
  writeFileSync(wrapper, `#!${process.execPath}
const { spawnSync } = require("node:child_process");
const { appendFileSync } = require("node:fs");
const args = process.argv.slice(2);
const result = spawnSync(${JSON.stringify(executable)}, args, { encoding: "utf8", env: { ...process.env, SHEPHRD_EXECUTABLE: ${JSON.stringify(executable)} } });
appendFileSync(${JSON.stringify(log)}, JSON.stringify({ args, at: Date.now(), status: result.status, stdout: result.stdout }) + "\\n");
process.stdout.write(result.stdout || "");
process.stderr.write(result.stderr || "");
process.exit(result.status ?? 1);
`);
  chmodSync(wrapper, 0o700);
  const calls = () => readFileSync(log, "utf8").trim().split("\n").filter(Boolean).map(line => JSON.parse(line));
  const cli = (...args: string[]) => JSON.parse(execFileSync(executable, [...args, "--json"], { env: process.env, encoding: "utf8" }));
  const page = () => cli("subdriver", "inspect", subdriver);
  const waitFor = async (predicate: () => boolean, detail: string, timeout = 12000) => {
    const deadline = Date.now() + timeout;
    while (!predicate()) {
      assert.ok(Date.now() < deadline, detail);
      await new Promise(resolve => setTimeout(resolve, 50));
    }
  };
  const children: ReturnType<typeof spawnProcess>[] = [];
  const receipts: any[] = [];
  const runtime = harness("tui", { ...process.env, SHEPHRD_EXECUTABLE: wrapper }, true, [], {
    readFileSync,
    execPath: process.execPath,
    spawn: ((command, args, options) => {
      const child = spawnProcess(command, args, options);
      children.push(child);
      let buffer = "";
      child.stdout!.on("data", chunk => {
        buffer += String(chunk);
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";
        for (const line of lines) receipts.push(JSON.parse(line));
      });
      return child;
    }) as typeof spawnProcess,
  });
  try {
    const before = page();
    assert.equal(before.subdriver.state, "idle");
    assert.equal(before.subdriver.generation, 1);
    assert.equal(before.events.length, 0);
    assert.ok(before.requests.every((request: any) => request.state === "done"));
    await runtime.emit("session_start");
    await waitFor(() => runtime.messages.length === 1, "main notification missing");
    const first = receipts.find(value => value.type === "notification").notification;
    assert.equal(runtime.entries.length, 0);
    writeFileSync(join(root, "release-worker"), "");
    await waitFor(() => cli("task", "inspect", worker).task.status === "done", "child worker did not complete");
    await waitFor(() => receipts.some(value => value.type === "renew" && value.ok), "main claim did not renew", 20000);
    const claim = () => cli("subdriver", "notification", first.notification_id);
    const renewed = claim();
    assert.equal(renewed.state, "claimed");
    assert.equal(renewed.claim_token, first.claim.claim_token);
    assert.equal(renewed.driver_generation, first.claim.driver_generation);
    assert.ok(Date.parse(renewed.claim_until) > Date.parse(first.claim.claim_until));
    t.diagnostic(JSON.stringify({ boundary: "renewed outstanding main claim", generation: page().subdriver.generation, deliveries: runtime.messages.length, drains: calls().filter(call => call.args[1] === "drain").length, main: renewed }));
    await waitFor(() => {
      const current = page().subdriver;
      return current.generation === 2 && current.state === "idle";
    }, "child-owner activation blocked by the outstanding renewed main claim", 5000);
    const pumps = calls().filter(call => call.args[1] === "pump").length;
    await waitFor(() => calls().filter(call => call.args[1] === "pump").length >= pumps + 2, "further pump opportunities missing");
    const held = claim();
    assert.equal(held.state, "claimed");
    assert.equal(held.handling_id, undefined);
    assert.equal(held.delivery_attempts, 1);
    assert.equal(held.claim_token, first.claim.claim_token);
    assert.equal(held.claim_owner, first.claim.consumer_id);
    assert.equal(held.driver_generation, first.claim.driver_generation);
    assert.equal(runtime.messages.length, 1);
    assert.equal(runtime.entries.length, 0);
    assert.equal(calls().filter(call => call.args[1] === "drain").length, 1);
    assert.equal(calls().filter(call => call.args[1] === "ack").length, 0);
    const after = page();
    assert.equal(after.subdriver.generation, 2);
    assert.equal(after.events.length, 0);
    assert.ok(after.requests.every((request: any) => request.state === "done"));
    const childNotices = cli("task", "inspect", worker).notifications;
    assert.equal(childNotices.length, 1);
    assert.equal(childNotices[0].state, "acknowledged");
    assert.equal(childNotices[0].delivery_attempts, 1);
    assert.equal(childNotices[0].target_driver_id, `coordinator:${subdriver}`);
    assert.ok(childNotices[0].handling_id);
    t.diagnostic(JSON.stringify({ boundary: "child handled while main outstanding", main: held, child: childNotices[0], generation: after.subdriver.generation, pumps: calls().filter(call => call.args[1] === "pump").length }));
    for (let i = 0; i < 4; i++) {
      await waitFor(() => runtime.messages.length === i + 1, "serialized return missing");
      await runtime.emit("message_end", { message: { role: "assistant", stopReason: "stop" } });
      await runtime.emit("agent_settled");
      await runtime.emit("agent_settled");
      await waitFor(() => runtime.entries.length === i + 1, "settled acknowledgement missing");
    }
    assert.equal(claim().state, "acknowledged");
    assert.equal(receipts.filter(value => value.type === "notification").length, 4);
    assert.equal(calls().filter(call => call.args[1] === "ack").length, 4);
    assert.equal(children.length, 1);
    assert.deepEqual(runtime.notices, []);
  } finally {
    const exits = children.map(child => once(child, "exit"));
    await runtime.emit("session_shutdown");
    await Promise.all(exits);
  }
});

test("explicit disable prevents TUI startup", async () => {
  const runtime = harness("tui", { SHEPHRD_PI_WATCHER_ENABLED: "0" });
  await runtime.emit("session_start");
  assert.equal(runtime.spawns.length, 0);
});
