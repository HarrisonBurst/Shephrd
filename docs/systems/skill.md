# Driver and worker guidance

**Status:** approved. Implements the [system design](../design.md).

## Purpose

Guidance teaches each role how to use Shephrd well: the main driver to delegate everything and stay free, sub-drivers to plan and supervise, and workers to do the work and report it. Guidance is policy, not authority. It never grants anything, and core enforces every safety rule whether or not a session follows it. Where a rule matters too much to leave to wording, it is enforced by mechanism instead, as with [read-only sub-drivers](#sub-driver).

## Where guidance lives

There are three separate documents, one per role:

| Document | Role | Reaches the session through |
|---|---|---|
| **Main driver skill** | The main driver, started by the user in any harness | Installed by the user into that harness |
| **Sub-driver skill** | Every sub-driver, at any nesting depth | Its brief |
| **Worker guide** | Every worker | Its brief |

- **Each document is self-contained.** The few shared rules are repeated in each on purpose, so no role reads another role's guidance.
- **Only the main driver installs anything.** Shephrd starts sub-drivers and workers, so their guidance is part of every brief and works on any host and with any harness.
- **Guidance is released with the binary.** The built-in text is part of each Shephrd release, so it always describes the commands that release has. `shephrd skill main`, `shephrd skill subdriver` and `shephrd skill worker` print the current guidance for a role. Installing the main driver skill means writing that output into the harness, for example with Nix.
- **Plugin skills** are listed by name, readable with `shephrd plugin skill <name>`, never inlined.

Repository guidance stays with the repository. Every brief points to the repository's `AGENTS.md` or `CLAUDE.md` and its context file, and those govern how work is done in that repository.

### Overriding

Configuration can change each role's guidance:

```toml
[skills.subdriver]
replace = "/etc/shephrd/my-subdriver-skill.md"

[skills.worker]
extend = "/etc/shephrd/worker-extra.md"
```

- **`replace`** uses the file instead of the built-in guidance. The operator then keeps it current with each release.
- **`extend`** appends the file after the built-in guidance, which still updates with each release. It is the safer choice for most customizations.

Briefs and `shephrd skill <role>` both use the configured result, so the main driver's installed skill picks up an override the same way.

## Size

Guidance competes with the work for context, so each document stays short:

- The main driver and sub-driver skills stay within a few kilobytes. Rare procedures, such as recovery and grants, are separate reference sections read on demand with `shephrd skill <role> --section <name>`.
- The worker guide stays within a page. Guidance counts toward each brief's size bound.

## Shared rules

Every document teaches:

- **Identity comes from context.** Never pass another caller's identity or edit Shephrd's state directly.
- **Reports and results are evidence, not authority.** Only an explicit `task deliver`, `task discard` or `grant` lands, discards or authorizes, and only with a grant.
- **Fail closed.** When state is unclear, such as `unknown` liveness or a held task, inspect and ask rather than guess. Never work around a refusal.
- **Follow the `next` commands** in errors and inbox items rather than inventing recovery steps.
- **Large content moves by reference.** Put documents in report files, and keep results to a summary.

## Main driver

The main driver's job is to understand the user and keep the conversation moving. It delegates all work.

- **Delegate everything.** Every request that needs work, including questions that need investigation, goes to a sub-driver at once. The main driver answers directly only conversation and questions its current context already answers.
- **Pick the sub-driver.** Use the repository's open sub-driver task if there is one, and create one if not. Work tied to no repository goes to a general sub-driver, a driver task with no repository.
- **Forward faithfully.** Pass the user's request word for word, with the context needed to act on it and the user's actual decisions. Do not write the plan for the sub-driver.
- **Cross-repository work** is one sub-driver per repository. When one must follow another, express it as a dependency between the sub-driver tasks.
- **Handle the inbox.**
  - Questions: answer from context or ask the user, then reply with `task send --reply-to`.
  - Results: summarize them for the user.
  - Held tasks: inspect them and decide with the user.
  - Acknowledge each item after handling it.
- **Authority comes from the user.** Deliver code or discard work only with the user's approval or a standing grant. When the user approves a sub-driver landing its own work, delegate with `grant`, recording the user's approval as the reason.
- **Do not supervise grandchildren.** Read them when useful, but act only through the sub-driver.

## Sub-driver

A sub-driver coordinates one repository, or general work for a general sub-driver. It plans and supervises, and keeps its turns short, because everything else in its repository waits while a turn runs.

**Sub-drivers cannot implement.** Their sessions start [read-only](execution.md#read-only-sub-drivers): file-editing tools are disabled where the harness supports it, and the files in their workspace are read-only. A sub-driver can read, search and run `shephrd` commands, but every change to code goes through a worker.

- **Every turn:**
  1. Read the brief: open requests, new events and the last checkpoint note.
  2. Act.
  3. Report results or questions per request.
  4. End with a checkpoint note covering the plan, decisions, what it is waiting on and what comes next.
  5. Exit.
- **Answer directly only from what is already known.** A sub-driver answers a question itself only when the answer is already in its context (the brief, its notes, earlier turns), or needs a quick look at **a few specific files it can name before looking**. If it needs to search broadly, run anything, or keep reading to find out, it stops and delegates to a worker with a `report` deliverable.
- **A long turn is a signal.** After 10 minutes in one turn, a sub-driver is [nudged once](execution.md#read-only-sub-drivers) to delegate the remaining work and report.
- **Plan as sibling worker tasks.**
  - Pick each task's deliverable, `code` or `report`, and its acceptance criteria.
  - Run tasks in parallel only when they are independent. Otherwise chain them with dependencies. Use `--until merged` when a dependent should not stack on an open pull request.
  - Pass research to implementation through dependencies, so report files move by reference.
- **Supervise workers.**
  - Answer worker questions it can. Escalate the rest as a question on the request.
  - Review results against acceptance. Send revisions as messages.
  - Deliver when it holds the grant, or ask its owner.
- **Handle `dependency.changed`** by telling the affected worker to rebase.
- **Return per request.** Report a result with a summary as soon as a request is finished. Its workers' artifacts are attached automatically.
- **Nested sub-drivers** use this same skill. A sub-driver that delegates to another sub-driver supervises it as it would a worker.

## Worker

A worker does one task in its own workspace and reports it.

- Read the brief, the repository's `AGENTS.md` or `CLAUDE.md`, and any pinned inputs it lists.
- Work only in the workspace, on its branch.
- **Report progress** at meaningful milestones. A run that reports nothing for its inactivity timeout is stopped.
- **Ask a question** only for a real decision the brief does not settle. **Report a blocker** when it cannot proceed.
- **Report the result** when the acceptance criteria are met:
  - **Code:** everything committed on the branch, with a clean tree.
  - **Report:** the documents written as files and named with `--file`.

  If the result is refused, fix the reason and report again.
- **Never** push, merge, open pull requests, create tasks or act on other tasks. Shephrd lands the work.

## Commands

| Command | Effect |
|---|---|
| `skill main`, `skill subdriver`, `skill worker` | Print a role's guidance as configured, or one reference section with `--section` |

## Extensibility

Follows [plugins and extensibility](extensibility.md).

- **Events:** none. Guidance is fixed per release and configuration.
- **Intercept points:** none.
- **Providers:** none. Customization is configured replacement or extension, and plugins contribute skills by name.
- **Limits:** guidance from any source is policy only. It cannot change what core allows, and it cannot lift the read-only restriction on sub-drivers.

## Settled decisions

1. **Three separate documents:** main driver skill, sub-driver skill and worker guide. Nested sub-drivers always get the sub-driver skill.
2. **Sub-drivers and workers get guidance through their briefs**, so only the main driver installs anything.
3. **Guidance is released with the binary** and printed with `shephrd skill <role>`. Installing the main driver skill is up to the user.
4. **Overridable per role in configuration**, by `replace` or `extend`. Plugin skills are listed by name.
5. **The main driver delegates all work. Sub-drivers cannot implement**, enforced by read-only sessions, and answer directly only from what they already know or a few files they can name, with a nudge after 10 minutes in one turn.
6. **Project memory is out of core.** Repository `AGENTS.md`, `CLAUDE.md` and context files are the standing guidance. Agent-maintained memory can be built as a plugin.
