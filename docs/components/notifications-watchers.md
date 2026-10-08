# Notifications and watcher delivery

## Purpose

Notifications make worker questions and terminal results durable without keeping a driver turn open. Delivery is owner-routed, claim-leased, and independent from task disposition. The obligations projection gives a read-only restart and portfolio view over the same durable facts.

## Ownership and authority

Each wake-worthy message copies the task's current `driver_id` into `target_driver_id` in the same transaction. Only that owner and the exact driver generation and claim token can renew or acknowledge a claim. The CLI may derive the notification ID and generation from a presented claim token, but it resolves them to the same full fenced store request before mutation. This is cooperative race prevention for one local account, not an authentication boundary.

The optional Pi watcher owns notification consumption while active. Without it, an integration may perform one bounded pass at a normal turn boundary. Notification handling and desktop presentation results never authorize spawn, recovery, verification, landing, release, retry, archive, or discard.

## Inputs and outputs

Accepted current `question`, `done`, `blocked`, and `failed` events create notifications. Runner death and current protocol failure can also publish wake records. When a runner later settles after its accepted terminal notification was already acknowledged or superseded, Shephrd publishes one bounded artifact-free `settled` system notification only if the existing obligations classifier still finds actionable task residue. The attempt and run generation uniquely fence that non-authoritative wake. A pending or claimed terminal notification remains the trigger instead, and a quiescent released task stays silent. Progress and checkpoint events remain quiet. A report `done` record remains unpresentable until the immutable report snapshot exists and every configured [report acceptance lifecycle handler](lifecycle-handlers.md) has a terminal visible outcome. Failed pre-acceptance report verification creates a separate sanitized `report-verification-failed` system notification with no artifact or report bytes.

`wake drain` accepts an owner, optional task, driver generation, batch limit, and claim lease. It returns claimed notifications plus exact owner, generation, token, and expiry data. Inside Pi, an omitted drain owner resolves to `driver:pi:$PI_SESSION_ID`; outside Pi it falls back to `wake.driver_id`.

The routine renewal is `shephrd wake renew --claim-token <token> --json`. The routine acknowledgement after handling settles is `shephrd wake ack --claim-token <token> --json`. Both look up exactly one stored claim, require the inferred or configured current owner to match, derive the notification ID and generation, and submit a full owner/generation/token-fenced request to the store. A first acknowledgement with no `--handling-id` creates a fresh stable handling ID. Human output prints it, and JSON returns it in a schema 1 acknowledgement receipt. Repeating the shorthand after success derives the stored handling ID and is idempotent.

The existing notification positional argument plus `--driver-id`, `--driver-generation`, and `--handling-id` remain advanced compatibility overrides. Every supplied override is checked against the claim and current inferred Pi owner; a conflict fails closed instead of replacing stored identity.

`task obligations` accepts owner, repository, and portfolio filters and returns schema 2 derived buckets, counts, omission instructions, planned readiness, and exact recorded-state recovery commands without external probes. With `--all-drivers`, it also returns exact relevant plan IDs, names, owners, and dispatched/live evidence summaries rather than counts alone.

[Opt-in sub-drivers](subdrivers.md) reuse these claims. Worker notifications target the durable sub-driver owner, while correlated `subdriver-question`, `subdriver-result`, `subdriver-blocker` and `subdriver-handoff` returns target the requesting main driver. Those returns carry request and sub-driver-event IDs instead of task/attempt evidence. The Pi watcher relays them without adopting or supervising descendants. `wake drain` performs a bounded activation pass for already-created owners with pending work. While one main claim remains outstanding, the watcher instead runs `wake pump --driver-id <main-owner> --json` on its serialized `poll_min` cadence, without draining or changing that main claim. Each pass retains the existing two-owner launch bound and process, endpoint and generation fences. Closed requests remain eligible through their pending worker notifications. Pumping never provisions sub-drivers or schedules ordinary worker retries. Empty queues make no model calls. Independent activation requires a matching CLI and watcher; see [mixed-version activation requirements](subdrivers.md#activation-requirements).

## Persisted state

Notification rows store message/task/attempt identity, target owner, worker generation and cursor, kind, payload and artifact, state, claim lease, acknowledgement, handling identity, and supersession data. Report handler annotation, receipt, and failure state remain in the report result and task inspection projection and are included alongside a claimed report notification without rewriting the worker message payload. A delivery log records claim, reclaim, renew, notify, acknowledge, adopt, supersede, and deduplication operations.

The obligations view has no table. It is derived in one read transaction from tasks, attempts, final checkpoints, accepted current-generation worker terminal counts, report recovery attestations, notifications, landing projections, plans, and latest bounded task annotations.

Schema 30 permits sub-driver-return notifications with NULL task/message/attempt IDs. The task notification aggregate selects only task-backed rows; sub-driver-owned worker tasks still count under their durable sub-driver owner, not the requesting main driver. Sub-driver returns remain on the request-correlated notification path and are not task obligations. Zero task counts do not mean those returns were acknowledged or their requests resolved. Mixed-source regression coverage lives in `internal/store/attention_notifications_test.go` and the built-CLI `internal/cli/attention_e2e_test.go`.

When the trusted notification extension is enabled, core checks durable presentation eligibility, prevents a repeated completed presentation, formats one sanitized title/body pair, applies deduplication and rate limits, and invokes version 1 of `notification.presentation` through the provider-neutral extension host. The first-party `shephrd-notification-macos` executable owns the only Notification Center and `osascript` interaction. Its typed result is advisory. Core alone records the delivery attempt and retains notification, claim, acknowledgement, and lifecycle authority.

## Normal flow

```mermaid
stateDiagram-v2
    [*] --> pending: wake event transaction commits
    pending --> claimed: owner drain
    claimed --> claimed: exact renew
    claimed --> acknowledged: settled handling and exact ack
    claimed --> pending: lease expires and owner reclaims
    pending --> superseded: identity invalidated
    claimed --> superseded: retry, stop, adoption rule, or stale identity
```

A drain first sweeps dead recorded runners, including attempts that accepted a terminal event before the runner crashed, reclaims expired owner claims, supersedes stale identities, and then claims a bounded FIFO batch. A dead terminal runner uses the same settlement transaction and idempotency fence as normal process exit. It skips a task with an active claim and returns no more than two notifications per task.

The Pi watcher runs only in a trusted interactive Pi TUI session. It drains one owner, takes one owner-scoped schema 2 `task obligations` count snapshot, injects those bounded counts with one follow-up, confirms matching input persistence, renews near expiry, and acknowledges only after a successful settled assistant turn. As soon as an accepted notification passes the generation, owner, single-claim and duplicate guards, the watcher shows one info notice and a `shephrd-wake` footer status naming the source: `Sub-driver: <registered repository>` or `Sub-driver: General` for sub-driver returns, `Worker <task label>` for worker events. The notice says whether the handling turn starts now or waits until the current turn settles; the status changes to handling once the follow-up is injected and is cleared by the settled acknowledgement or claim loss. This receipt is machine detection evidence only. It does not interrupt an active turn, acknowledge early, or bound the model's summary latency, which remains behind busy-turn injection and provider response time. Immediately before acknowledgement it takes one final count snapshot. A transient acknowledgement retry reuses that snapshot, so a wake turn performs at most two read-only obligations queries. Nonzero residue or a bounded query failure is persisted as counts and count delta only, then surfaced on the next natural turn without triggering another turn or readiness wake. Quiescence persists no obligations context.

The watcher consumes the schema 1 acknowledgement receipt and persists the returned handling ID in its Pi entry. Worker Pi sessions force the watcher off. Shorthand resolution does not cause acknowledgement before the settled-turn boundary, and an obligations command failure does not consume the claimed notification twice.

Its injected guidance is limited to current identity, authorization, held-work safety, and no-polling mechanics. It does not require review, a spawn sequence, successor work, or lifecycle actions before acknowledgement. A `settled` wake and residual counts are evidence only. Handling may report a result or missing decision without starting more work; acknowledgement remains tied to the matching successful settled assistant turn, not workflow completion.

## State transitions

Task adoption retargets pending and claimed notifications, releases old claims, and does not acknowledge them. Retry, stop, stale attempt/run identity, and most releases can supersede notifications. An unhandled `settled` notification survives release only while actionable residue independent of that notification remains; claim, renewal, acknowledgement, and adoption keep their existing fences. A current accepted report `done` notification backed by complete report landing proof remains active after report worktree release, including while a handler invocation is still `pending` or `invoking`, so the result is not lost before recovery and handling. Report result presentation still waits for `succeeded`, `failed`, or `unknown`, which are visible terminal presentation states.

Obligations buckets include action now, needs disposition, result ready, planned ready, underway, queued, planned blocked, and closed. A blocked or failed current report attempt with a dead recorded runner, held workspace, no accepted artifact, current worker checkpoint, and zero accepted worker terminal events in the current generation projects `report_recovery_available` plus the exact attestation command. An ordinary accepted worker blocked or failed event instead retains normal terminal recovery choices. An existing attestation projects `report_recovery_attested` with exact verify and clean-retry commands and never offers same-worktree relaunch. These are inspection evidence only; attestation still revalidates the process, terminal endpoint, worktree, checkpoint, path, and bytes before recording anything. Closure requires terminal status, no active notification or worker, every attempt released or no-workspace, no uncertain workspace, and recognized landing, discard, or no-work outcome. Archive is visibility only and cannot satisfy closure.

## Failure and recovery

Expired, ambiguous, mismatched, wrong-owner, wrong-generation, wrong-token, or conflicting-handling claims fail rather than acknowledging another handler's work. Repeating the exact acknowledgement and handling ID is idempotent; shorthand repetition resolves that stored identity. Identity errors name the missing or conflicting field and return an exact `recovery_command`.

When a watcher turn aborts or fails, its claim remains durable for renewal or expiry. An expired claim's recovery command performs a fresh owner drain. If the current inferred Pi driver differs from the stored owner, the recovery command explicitly adopts the task and drains again; identity inference never performs adoption. Without a watcher, do not loop or sleep after an empty drain. When recovering work under a replacement driver, use `task obligations --all-drivers` and `plan ls --all-drivers` to discover it, then adopt each relevant task and plan explicitly when authorized. Neither disclosure command mutates ownership.

Best-effort macOS hints can fail, time out, be cancelled, deduplicate, or rate-limit without changing durable notification state. Failures remain bounded delivery-log evidence. Default and disabled configurations start no notification extension.

## Safety invariants

- Message transition and notification publication commit together.
- A claim is presentation authority only, not task authority.
- Acknowledgement and desktop presentation record handling evidence, not approval, verification, landing, release, retry, or closure.
- Only the current owner can claim, renew, or acknowledge.
- An active watcher and manual drain must not compete.
- One main claim blocks further main drains, not bounded sub-driver activation; pump-only passes do not consume main notifications.
- Empty drains return immediately and consumers do not poll.
- Obligations is a recorded-state projection and performs no Git, GitHub, process, terminal, file, or workspace mutation.
- Wake-turn obligations reuse that projection, issue at most two owner-scoped read queries, and never schedule or wake on readiness changes.
- A projected report recovery command is not authorization and does not imply that the canonical file currently passes attestation.
- Contradictory or incomplete closure facts surface as attention rather than safe closure.
- Report result notification never precedes its configured handler outcomes.
- Failed report verification has a separate durable notification that never carries unverified report bytes.
- A settlement wake carries no artifact, proves no disposition, and is unique per attempt and run generation.
- Release does not supersede an accepted report notification solely because handler recovery remains pending.

## Commands

- `shephrd wake drain`
- `shephrd wake pump --driver-id <main-owner>` (bounded activation only; no main notification consumption)
- `shephrd wake renew --claim-token <token>`
- `shephrd wake ack --claim-token <token>`
- `shephrd wake renew [notification-id] --claim-token <token> [--driver-id <id>] [--driver-generation <generation>]`
- `shephrd wake ack [notification-id] --claim-token <token> [--driver-id <id>] [--driver-generation <generation>] [--handling-id <id>]`
- `shephrd task obligations`
- `shephrd task adopt <task-id>`

## Interactions with other components

[Worker events](worker-protocol.md) publish notifications. [Report acceptance lifecycle handlers](lifecycle-handlers.md) gate only accepted report result presentation, not worker event authority. [Ownership and adoption](ownership-annotations.md) controls routing transfer. [Review loops](review-feedback.md) often use questions as the feedback boundary. [Delivery](delivery-release.md) and [tasks](tasks.md) remain independent from acknowledgement and obligations projections.

## Design rationale

Durable owner-routed claims let the CLI remain process-bounded and avoid a control daemon. Lease fencing handles crashes without losing records. Keeping handling separate from disposition prevents an injected or acknowledged message from silently authorizing a lifecycle action.
