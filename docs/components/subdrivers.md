# Repository sub-drivers

## Responsibility, not a permanent conversation

Coordination is opt-in. A main conversational driver forwards the original request and necessary preceding context. A repository sub-driver investigates technical ambiguity, plans, queues ordinary workers, handles their notifications and routine recovery, and returns meaningful results or genuine user questions. The main does not write its worker plans or supervise its grandchildren. Simple questions already answerable in the main conversation stay there.

A sub-driver is not an implementation worker. Its durable `coordinator:<id>` owner is independent of harness sessions. There is one owner per registered repository, shared by concurrent goals and by requests from different main drivers. Workers remain normal tasks with the existing attempts, worktrees, artifacts, checkpoints and lifecycle contracts. They never become sub-drivers. General research uses a new on-demand owner with explicit context, no repository authority and no worker provisioning.

There is no fleet activation, background model daemon, memory migration, automatic repository registration or new approval, merge, publish, discard or release authority. Direct task/worker operation remains available. Ownership is cooperative fencing between local processes, not an OS sandbox or authentication against the local account.

## Main-facing flow

Use an explicitly registered, trusted repository. These examples describe future activation; installing or building the code does not start sub-drivers.

```sh
shephrd subdriver handoff --repo example --key user-request-001 \
  --request-file /path/to/original-request.txt \
  --context "Relevant preceding conversation and actual user decisions, verbatim" \
  --json
```

Outside Pi, supply `--driver-id <main-owner>`. The original request file is retained byte-for-byte as text, including whitespace. An argument can replace `--request-file`. Do not rewrite intake into a detailed implementation plan. Include scoped read-only constraints, selected workflows and memory opt-outs in the forwarded context.

Handoff persists the request before starting a bounded session using the configured global or registered-repository harness/model defaults, then matching driver context if no model is configured, and the ordinary runtime selection mechanisms. Handoff itself does not override or downgrade a model. The resolved selection is retained across automatic turns. `--queue` records intake without starting a session. A repeated key from the original driver returns the same request; changed content under that key is rejected, including after explicit return-route adoption.

```sh
shephrd subdriver inspect <subdriver-id> --json
shephrd subdriver request <request-id> --json
shephrd subdriver event <event-id> --json
shephrd subdriver resume <subdriver-id> --json
```

`inspect` returns pages of 20 request summaries, pending input events and worker links. Follow `next`; full originals/context are read through each request's `read_command`. Long pending events have an exact `read_command` rather than silently truncated content. `request` and `event` are exact-content inspection surfaces. They are not transcripts.

Questions and results arrive through the existing `wake drain` and Pi watcher claim protocol, with `request_id`, `subdriver_id` and `subdriver_event_id`. The watcher relays only sub-driver returns to main; worker events belong to the sub-driver. A delivery acknowledgement is not an answer.

```sh
shephrd subdriver reply <request-id> "The user's actual reply" \
  --reply-to <question-event-id> --key user-reply-001 --json
```

Replies are durable and idempotent. The current main return owner must match, and the referenced event must be a question, blocker or handoff on this exact request. Reply does not itself grant lifecycle authority or launch a worker.

The existing watcher runs serialized activation passes independently of main notification settlement. With no outstanding main claim, `wake drain` performs the bounded pass before claiming one notification. While that claim remains outstanding, the same watcher cadence invokes only `wake pump --driver-id <main-owner> --json`, waiting `poll_min` after each completed pass; it does not drain again. Renewal and successful-settled acknowledgement keep their existing paths. Empty drains still back off to `poll_max`; neither interval bounds CLI execution or model latency. For a non-Pi main driver, [`wake watch`](notifications-watchers.md#driver-agnostic-watcher) runs the same activation in-process: the bounded pass before each one-notification drain, and a pump-only pass every `wake_watch.poll_min` while its delivered claim is outstanding.

Each pass can start up to two already-created owners with pending intake, replies or eligible worker notifications. Closed intake requests do not disqualify pending worker notifications. Pumping cannot claim, acknowledge, renew, expire or release main notifications, inject child notifications into main, or bypass idle-state, process-absence, endpoint-settlement and atomic generation-reservation checks. Activation passes do not overlap in one watcher. `wake pump` requires a main owner (explicit outside Pi), honors `wake.enabled`, is unavailable to ordinary workers and sub-driver sessions, and fails closed while a `wake watch` holds that owner's lock. `wake watch` has the same session fences. Without a watcher, explicitly resume the owner or use one normal-turn wake pass; do not poll or create another consumer. An empty queue starts no model process. Ordinary worker retry/relaunch remains explicit, not scheduled.

## Human-readable name

Canonical CLI commands, protocol role and code identifiers use `subdriver`; readable role labels use sub-driver/Sub-driver. A repository owner is shown as `Sub-driver: <registered repository name>` (for example `Sub-driver: Shephrd`) and a general research owner as `Sub-driver: General`. The name is derived from `Subdriver.RepoID` and the registered repository name; nothing new is stored. It labels Herdr and cmux tabs, the native Pi/Claude `--name`, watcher receipts, footers and injected returns. Repository and General display naming is unchanged.

Canonical JSON uses `subdrivers`, `subdriver`, `subdriver_id`, `subdriver_event_id` and `subdriver_repo_name`. New notifications store `subdriver-question`, `subdriver-result`, `subdriver-blocker` and `subdriver-handoff`; reads also project historical kinds into those canonical values without changing stored evidence. This is not a presentation-only rename. The only alternate command spelling is the legacy `coordinator` alias, not `sub-driver`. See the [compatibility inventory](#compatibility-exception-inventory) before updating a consumer or activating this artifact.

## Sub-driver mechanics

The runtime supplies a generation-fenced sub-driver identity to the harness. Ordinary workers have that identity removed and their watcher disabled. Sub-driver sessions cannot hand off to new sub-drivers, adopt tasks, provision repositories, consume another scope's claims or use unrestricted direct task creation.

```sh
shephrd subdriver dispatch <request-id> "Bounded worker objective" \
  --key stable-plan-step --feature unique-worker-feature \
  --deliverable code --acceptance "Requested acceptance criteria" --json
shephrd worker spawn <returned-task-id> --json
shephrd subdriver handled <intake-event-id> --json
```

Dispatch transactionally creates one ordinary task and its request association. Replaying the same specification/key returns that task, including after a crash. Changed specifications conflict. Dispatch only queues; inspect the task before spawning, sending, retrying or relaunching it. Existing task inspection, annotations, worker follow-up/recovery, delivery attestations and verification remain the operational records, not a duplicate checkpoint database. These commands retain their existing evidence, ownership and process checks. A sub-driver may release only through existing proven-delivery paths; explicit discard stays with main. A result or user reply never substitutes for those checks.

The runtime claims at most five worker notifications for a turn with a 30-minute lease. The sub-driver handles them and uses ordinary `wake ack --claim-token ...`; manual sub-driver drains are rejected. Claims are fenced to the current sub-driver generation. A failed turn does not implicitly acknowledge anything. Proven-absent session recovery releases old claims without acknowledging them and queues a recovery input for unfinished requests. This also recovers a crash after intake was marked handled but before a queued worker was started.

```sh
shephrd subdriver return <request-id> "Concise outcome or real question" \
  --key stable-return-key --kind result --json
```

Kinds are `question`, `result`, `blocker` and `handoff`. Results are conversational completion of that request, never worker artifacts, verification, landing or release proof. Request/event keys, generation checks and existing worker attempt guards make replay safe without interpreting checkpoint prose as commands. A session ends with a small structured checkpoint and an artifact-free `done` envelope. Only the sub-driver runner accepts this session-ending form; ordinary worker `done` still requires its artifact contract.

## Checkpoint preflight and reporting repair

Before emitting a report, run `shephrd protocol validate --role subdriver --file authored-output.txt --json` or pass the exact authored text on stdin. This is read-only format validation, not a return, input acknowledgement, artifact acceptance or lifecycle authorization. `decisions` entries are objects with string `decision` and `reason` fields; `checks` entries are objects with string `command` and `result` fields. Neither accepts string shorthand. The actual sub-driver brief supplies typed examples and preflight instructions. See the [canonical protocol](worker-protocol.md#read-only-format-preflight) for bounds and examples.

Pi sub-drivers can correct one rejected report before terminal hold using the existing 120-second reporting-only repair, within the overall turn deadline. Headless correction resumes only the current native session with tools disabled; interactive Pi blocks tools during its correction turn. No dispatch, return, acknowledgement or prior action is replayed. The entire corrected checkpoint-plus-artifact-free-done output must pass strict validation and current generation, session, cursor, repair identity/hash and terminal fences before replacing the checkpoint. Exhausted, stale, ambiguous or nonrepairable corrections remain held with prior durable actions and recoverable state intact. Claude/Codex sub-driver failures still hold rather than attempt an unestablished tool-free correction. This is not automatic worker retry or another full work turn.

## Bounded fresh sessions

Each turn starts a new harness session, never a transcript resume. Defaults are:

- At most 48 KiB of Shephrd startup context, with room reserved for worker notices.
- At most 20 pending input summaries and five claimed worker notices per turn.
- A 4 KiB operational checkpoint, containing continuation hints and evidence pointers only.
- A 20-minute turn deadline, shorter than the notification lease.
- Original requests up to 256 KiB, preceding context up to 64 KiB, and individual replies/returns up to 8 KiB. Oversize writes fail, never silently truncate.

Repository instructions and the lean context entry point are mandatory reading pointers. The existing memory guidance selects relevant topics, not all notes. `memory.enabled` is reloaded at session start; disabled memory is excluded from automatic recall and writes at both start and recovery. Scoped repository/request opt-outs still apply. No workflow is selected by discovery. The Shephrd budget does not override the harness's own system instructions or context settings.

Omitted operational records always retain inspection commands and paging. Read full originals, authority boundaries and any omitted event before acting. Runner diagnostics and headless output are logged under `<data_dir>/<subdriver-id>/session-<generation>.log`; they are never automatically rehydrated. Native terminal contents stay on the harness interface rather than being captured as a transcript.

Headless is the existing default and retains bounded stream-mode invocation. Explicitly configured Herdr and cmux runtimes use the same pinned provider selection, durable endpoint identity and process inspection facilities as workers. Pi and Claude Code sub-driver turns show their native interactive TUIs through the existing trusted worker bridges; Codex keeps the worker stream presentation. This changes visibility, not session lifetime or authority: every turn starts fresh, retains its selected model, records its harness PID and ends after an accepted checkpoint plus artifact-free done or the 20-minute deadline. Bounded interactive turns own a foreground process group so deadline shutdown also stops harness descendants. Ordinary workers still require their artifact contracts.

The endpoint is disposable, not a persistent conversation. Completed endpoint identity remains recorded; an outside wake/resume pass proves process absence and closes that exact endpoint if the provider has not already removed it. Rotation never replaces an unresolved endpoint. No keepalive, polling consumer or live terminal rollout is implied by fixture coverage.

## Restart and recovery

```sh
shephrd subdriver ls --all-drivers --json
shephrd subdriver inspect <subdriver-id> --json
shephrd subdriver adopt-request <request-id> \
  --from-driver <old-main> --driver-id <new-main> --json
```

Discovery is read-only. Adoption changes only a request's main return route and releases its outstanding main claims. Original intake identity, sub-driver ownership, worker ownership, active attempts and acknowledged historical returns remain unchanged. Adopt each relevant request explicitly. Do not adopt sub-driver-owned worker tasks or independently supervise them.

A failed turn, missing consumption of its supplied batch, expired runtime deadline or observed dead runner holds the owner and publishes correlated blocker notifications. Pending work and worker processes remain intact. No automatic retry is attempted.

A wake-pump failure observation is conditional on the same sub-driver ID, generation, state and `updated_at` snapshot still matching under the store write lock. All sub-driver session, process and endpoint updates advance that timestamp. Validation, hold and blocker publication share one transaction, for both drain-coupled and independent pump passes. If a bounded runner has already finalized to idle, its earlier running snapshot cannot overwrite that completion or enqueue blockers. A stale observation is a no-op, not recovery or acknowledgement; current missing-runner, launch-timeout and idle-endpoint failures still hold. Generation-only fencing is insufficient because normal finalization retains the generation.

```sh
shephrd subdriver recover <subdriver-id> --generation <inspected-generation> --json
shephrd subdriver resume <subdriver-id> --json
```

Recovery requires the recorded runner and harness to be absent and any recorded terminal endpoint to be provably absent. Live/reused PIDs, an existing endpoint and failed process/terminal probes fail closed. Close only an independently verified owned endpoint, never an unrelated process. A starting session with incomplete process or endpoint identity remains held. If an operator has independently inspected launch logs and the exact `shephrd:coordinator:<id>:<generation>` source and established that unrecorded launch effects are absent, `recover --launch-absent "evidence and reason"` records that explicit confirmation. It cannot override a live recorded PID or a present/uncertain recorded endpoint. Never use it to guess past uncertain state.

### Correcting a retained model

`subdriver resume --model <exact-harness-model-id> --generation <inspected-generation>` explicitly replaces the model for the next pending turn and subsequent turns. It keeps the owner, requests, worker links, checkpoint, harness and runtime. Omit `--model` to retain the stored selection, even when empty; `--model=""` explicitly selects the harness-native default. This is not a harness switch or an automatic model escalation. On a never-started queued owner, inspect generation 0 and use it to select the initial model; the harness still comes from the registered-repository override, global default, or driver context in that order.

An override requires the exact inspected generation. Reservation atomically compares that generation and idle state before persisting the new selection and incrementing the generation. A stale request, held/starting/running owner, live or uncertain recorded process, or unresolved terminal endpoint cannot be bypassed by selecting a model. With no pending work, resume starts no session and does not change selection.

For a held owner, first verify exact identity, original intake, generation, process absence and endpoint absence. Keep automatic activation for this owner quiescent between recovery and explicit resume: a concurrent handoff, resume or wake-driven activation could otherwise start the old selection after recovery. A generation conflict requires reinspection, never an unconditional retry.

```sh
shephrd subdriver inspect <subdriver-id> --json
shephrd subdriver request <request-id> --json
shephrd subdriver recover <subdriver-id> --generation <inspected-generation> --json
shephrd subdriver resume <subdriver-id> --generation <inspected-generation> \
  --model <exact-harness-model-id> --json
shephrd subdriver inspect <subdriver-id> --json
```

Recovery retains the generation; successful pending-turn reservation increments it. Do not recover a held owner merely because a replacement model was chosen. The recovery prerequisites above still apply, and `--launch-absent` is only for independently proven absent unrecorded launch effects. Updating environment variables, editing ledger rows, changing global Pi settings, or handing the original request to a new owner is not a substitute for exact-owner recovery.

`resume --foreground` is available for configured headless execution and deterministic fixtures. It is synchronous; normal handoffs and watcher activation launch bounded owned runner processes asynchronously.

Schema 30 is a forward migration from the immutable schema-29 baseline. It preserves existing task, attempt, artifact and notification facts and adds sub-driver routing plus an alternative correlated notification source. Schedule an explicitly chosen future installation after existing workers and consumers have settled and stopped, and back up state first. This is not a live migration recipe. Older binaries refuse schema 30. Do not edit ledger rows, copy a live database into a worktree, migrate memory, or provision owners merely to upgrade. No live activation was performed by the implementation fixtures.

## Compatibility exception inventory

These are intentional exceptions, not canonical usage. Source filenames, Go model/store/control/parser identifiers and current documentation links now use `subdriver`/`subdrivers`. No literal-zero claim is made. The inventory covers remaining case-insensitive `coordinator` source occurrences by contract:

| Contract | Concrete symbols and paths | Why it remains / interoperability |
| --- | --- | --- |
| Legacy CLI entry point and protocol role | `subdriverCommand` in `internal/cli/subdriver.go`: `Aliases: coordinator`; `protocolCommand` in `internal/cli/protocol.go`: `--role coordinator` | Old briefs, scripts and queued `_run` commands still execute. Cobra resolves both spellings to the same canonical command path and scope fence. Both roles use `ParseSubdriverCandidateSet`; validation echoes the requested role (`coordinator` only for the explicit legacy input) and never grants worker artifact or lifecycle authority. Help, arguments, completion, generated read/recovery commands and new briefs use the canonical name. |
| JSON field aliases | `SubdriverRequest.MarshalJSON`, `SubdriverPage.MarshalJSON`, `DriverNotification.MarshalJSON` in `internal/model/subdriver.go`; list response in `internal/cli/subdriver.go` | Canonical fields are emitted alongside equal deprecated `coordinator_id`, `coordinator_event_id`, `coordinator_repo_name`, `coordinator`, `coordinators`. These are output aliases of one value, not independently writable identities. Internal Go types are not a public API and have no old type aliases. |
| Legacy notification consumers and kinds | `scanDriverNotification` in `internal/store/notifications.go`; `--legacy-coordinator-json` in `internal/cli/root.go`; `canonicalNotification` and command selection in `.pi/extensions/shephrd-wake.ts` | Historical `coordinator-*` rows are projected, never updated by reads. The compatibility-only flag with `--json` restores old kinds for `wake drain`, `wake renew` and `subdriver notification`; canonical and legacy correlation fields remain equal. Consumers matching old kind strings must use this flag or update before rollout. The existing old watcher accepts canonical kinds and old correlation aliases. A new watcher accepts old, new and equal dual-field returns, normalizes receipts/footers, rejects conflicting or incomplete correlation before injection/ack, and uses legacy executable commands only for an old producer's legacy-only notification. |
| Environment/session fences | `SubdriverEnvironmentFence` and `subdriverEnvironment` in `internal/control/subdriver.go`; `workerBoundaryEnvironment`/worker shell launch in `internal/control/runtime.go`; `subdriverSession` in `internal/pibridge/shephrd-herdr-bridge.ts` | New launches emit `SHEPHRD_SUBDRIVER_ID`, `_GENERATION`, `_TOKEN` and equal `SHEPHRD_COORDINATOR_*` aliases for already-running/older CLI and bridge consumers. Either complete triplet works alone. If both are present they must match exactly; partial, invalid, conflicting, or worker-plus-role identities fail closed. They cannot be assembled from fragments of two triplets. Both prefixes are removed from ordinary workers. The bridge also checks owner/generation against its independent bridge identity; CLI private runner arguments must match the environment. Standalone read-only protocol preflight intentionally requires no environment authority. |
| Opaque durable owner and claim identities | `Subdriver.DriverID`/`IsSubdriverOwner` in `internal/model/subdriver.go`; `subdriverConsumerTx`, `SubdriverTaskFence`, pending/recovery queries in `internal/store/subdriver.go`; inference/claim/recovery checks in `internal/cli/root.go` and scope checks in `internal/cli/subdriver.go` | `coordinator:<id>` remains the exact persisted owner namespace, including newly linked workers under that owner, so old/new processes agree on routing, adoption protection and claims. `coordinator:<generation>` remains the exact claim-generation value, not a public role name. Tokens, owner IDs and generations are never translated. `subdriver:<id>` is reserved and rejected as an alternate owner, not normalized into a second privilege path. Store task creation/adoption and CLI scope checks use `IsSubdriverOwner`; stale, renamed and cross-owner claims cannot acknowledge or renew. |
| Terminal endpoint and held-notification identity | `shephrd:coordinator:<id>:<generation>` source and recovery hint in `internal/control/subdriver.go`; `coordinator-held:<event-id>` in `internal/store/subdriver.go` | Keep exact endpoint discovery/recovery and deterministic notification identity compatible with running sessions and old recovery code. Human labels and new held payloads use Sub-driver; new held kinds use `subdriver-blocker`. No endpoint, notification ID or claim is renamed. |
| Immutable schema-30 evidence and SQL storage | `internal/store/subdriver_schema.go`: `subdriverSchema`, `subdriverColumns`, `subdriverChecksum`, `upgradeSubdrivers`; `internal/store/baseline.go`; SQL in `internal/store/subdriver.go`, `subdriver_session.go` (`HoldSubdriverObservation` compares the unchanged SQL identity/state/timestamp), `notifications.go`; shape fixture `schema_test.go` | Keep `coordinators`, `coordinator_requests`, `coordinator_events`, `coordinator_workers`, `coordinator_id`, `coordinator_event_id`, `coordinator_requests_owner_idx`, `coordinator_events_request_idx` and ledger name `030_coordinators`. The schema checksum remains `63922f9eeee838cac070634ec4617b2646b8bdfd771caa185a7c4fdc58fad31c`. Renamed Go symbols/files do not change DDL bytes, ledger identity or schema version. There is no new migration. |
| Existing IDs and authored/historical evidence | `NewID("coord")` in `internal/store/subdriver.go`, stored intake/context, event payloads, checkpoints, logs, dispatch specifications, accepted artifacts and ownership facts | Existing IDs are opaque, including `coord_...`; the allocator stays compatible. Exact originals, old generated payloads and saved prompts can still say coordinator. They are not rewritten, relabelled in-place or interpreted as new authority. Artifacts, migrations, archived evidence and sealed reports are not grep-cleanup targets. |
| Compatibility tests and documentation references | `internal/cli/subdriver_compatibility_e2e_test.go`, `subdriver_e2e_test.go`, `subdriver_terminal_e2e_test.go`, `wake_pump_e2e_test.go`, `subdriver_hold_e2e_test.go` (clears both environment prefixes), `subdriver_test.go`, `protocol_test.go`; `internal/control/subdriver_test.go`; `internal/store/subdriver_compatibility_test.go`, `subdriver_test.go`, `subdriver_hold_test.go` (retains exact durable owner/claim generation), `schema_test.go`; `internal/pibridge/shephrd-herdr-bridge.test.mjs`; `tests/pi/shephrd-wake.test.ts`; this page, `ownership-annotations.md`, the skill reference, notification memory and `../reference/subdriver-activation.md` | Legacy examples are deliberate tests or explanations of the above contracts, not recommended new usage. Tests prove old/new permission equivalence, persisted schema/claims, conflict and stale rejection, exactly-once handling, child ownership and unchanged lifecycle fences. The built-CLI compatibility fixture explicitly rejects canonical kinds in a strict old-kind consumer, accepts the legacy projection, and checks that this selection leaves the same outstanding claim unacknowledged. |

`AGENTS.md`, `CLAUDE.md`, `CHANGELOG.md` and files marked generated are not edited by this rename. The inspected tracked versions contain no additional coordinator terminology exceptions. The sealed source catalogue and Git history are historical evidence outside the rename diff. No other worktree or shared memory is updated.

### Activation requirements

The [paired activation procedure](../reference/subdriver-activation.md) covers the integrated rename, independent pump and atomic stale-observation repair, including loaded-consumer evidence and reload gates. A committed artifact is not installed consumer readiness.

Deliver and install only the tested committed artifact under explicit deployment authority. This implementation does not install a binary/extension, replace a running process/session, rewrite a repository root, migrate a live database or acknowledge live notifications. No global build/install or fleet activation is part of the rename.

Schema-30 state requires no migration or owner transfer for this rename. Existing schema-30 binaries can read the same schema, owners, tokens and requests. A pre-schema-30 database still needs the previously documented, separately authorized schema-30 upgrade. Keep backups and preserve exact record identities; never alter ledger checksums to activate a naming change.

Before an authorized rollout, identify consumers with strict JSON key sets or notification-kind enums. Additive legacy field aliases support old field-based consumers, not closed-schema decoders. Update strict consumers to accept the canonical fields; old kind-only consumers may temporarily pass `--legacy-coordinator-json`. New watcher plus old CLI retains notification delivery, and old watcher plus new CLI retains its existing behavior. Independent activation during an outstanding main claim requires both the new watcher and a CLI supporting `wake pump`. An old CLI's exact unknown-command or unsupported `--driver-id` flag response disables pump-only attempts for that watcher child and emits one explicit warning; ordinary drain/renew/settled-ack handling continues, with activation still drain-coupled. Other pump errors remain visible and do not disable future scheduled passes. No version/capability framework or fallback extra drain is introduced. Old/new command aliases and old/new complete environment triplets remain supported. Do not remove legacy aliases while old sessions/clients depend on them. Old binaries cannot detect conflicting dual environment values, so never manufacture divergent identities; new launches emit identical values and new code rejects conflicts. A coordinated future installation after sessions and consumers settle is the safe activation boundary, not permission to interrupt them now.

## Cross-repository and general work

The main explicitly designates a lead by routing the initial request there. The lead returns a `handoff` proposal describing a scoped sibling assignment. Main forwards it with `--lead-request <lead-request-id>` to the sibling owner. The referenced lead must have the same current main return owner. Sibling results retain their own request correlation; main relays them as a reply to the lead's handoff event. This is one sub-driver layer with main-mediated routing, not peer ownership or implicit cross-repository authority.

```sh
shephrd subdriver handoff "Substantive general research question" \
  --general-context "Explicit domain, relevant facts and requested scope" \
  --key research-001 --json
```

A general sub-driver researches directly and cannot create repository workers. Its records survive; its conversation is discarded after every bounded turn. Empty owners are healthy and require no retirement model call.

## Evidence

`internal/cli/subdriver_hold_e2e_test.go` runs the real watcher child and built CLI with isolated configuration, database, repository and processes. A test-only build overlay pauses the pump after its running snapshot; the ordinary runner then accepts a bounded checkpoint/done and exits before the actual PID probe. Headless and interactive Pi bridge fixtures check that completion stays idle across subsequent watcher passes without blockers, recovery input or a new session. Deliberate loss of the fixture runner before finalization still holds and delivers a correlated blocker without acknowledging it or changing worker ownership. Store/control tests cover changed state, generation, session, harness and endpoint observations, launch-timeout races, current failures and retained worker claims. This deterministic scheduling demonstrates the mechanism, not any historical session's interleaving, and uses no authenticated model or live terminal service.

`TestPiWatcherOutstandingClaimActivationE2E` in `internal/cli/subdriver_e2e_test.go` reuses the built CLI, isolated native worktrees and deterministic Pi fixture with the real watcher child. Before the scheduling fix, a successfully renewed main claim blocked generation-2 child handling even with all intake requests done. The regression now proves one child activation/delivery despite further pump opportunities, the exact unacknowledged main claim and single main delivery retained throughout, no new intake or worker restart, and later acknowledgement through simulated successful Pi settled events. Watcher fixtures cover serial pump passes, transient failures and explicit old-CLI fallback; built-CLI and existing store/control fixtures cover permissions, empty/held/live/cross-owner state, endpoint and stale-generation fences. These are deterministic fixture results, not live activation or latency guarantees.

`internal/cli/subdriver_e2e_test.go` uses a built CLI, isolated configuration/database/repository, deterministic fake Pi and ordinary native worktrees. It covers original request fidelity, multiple goals sharing an owner, idempotent dispatch and questions, correlated user replies, worker return while dormant, wake-driven fresh sessions, configured model retention, held-turn recovery, general context and main replacement without stolen worker supervision. `internal/cli/subdriver_selection_e2e_test.go` reproduces `PI_MODEL`-only native fallback and retained empty selection, then covers explicit correction, stale/held rejection, initial generation-0 selection, later retention and explicit native defaults without authenticated model calls. Store and control tests also cover stale reservation after a completed intervening turn and live-process refusal. `internal/cli/subdriver_terminal_e2e_test.go` drives the built private CLI runner with isolated state, deterministic Pi/Claude harnesses and pinned fake Herdr/cmux extensions. It checks terminal output forwarding, native invocation and bridge paths, fresh sessions, model propagation, correlated returns, disabled watcher consumption, strict checkpoint bounds and stale bridge rejection without authenticated model calls. The same built-CLI fixture also reproduces string-valued decisions/checks after dispatch and covers corrected, exhausted, duplicate, artifact-bearing, prose-bearing, checkpoint-only and changed-session corrections over headless and native Pi bridges. It checks retained worker linkage and exactly-once prior dispatch/return/input handling. `internal/cli/protocol_test.go` covers read-only format preflight, including invalid configuration and synthetic owner identities; control tests cover correction fences and ordinary worker artifact requirements. Runner tests cover deadlines before and after bridge acknowledgment and owned process-group shutdown. These provider fixtures are not evidence of a live cmux run. `internal/cli/live_herdr_presentation_e2e_test.go` is the opt-in live Herdr fixture (`make test-herdr-e2e`): it creates and closes owned tabs labelled `Sub-driver: Shephrd` and `Sub-driver: General`, then runs one isolated real Pi TUI with the wake extension against an isolated database to observe the receipt notice, footer status, injected return and settled acknowledgement for a repository return while idle and a general return while busy. Its model turns use the live provider; it is not cmux coverage. Store, control, adapter, runner and watcher tests cover the associated fences and bounds. Existing direct-driver and landing/release tests remain authoritative for their unchanged lifecycle contracts.
