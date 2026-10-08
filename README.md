# Shephrd

Shephrd is a local CLI control plane for delegating repository work from a user-facing driver to isolated Claude Code, Pi, or Codex workers. It records tasks and attempts in SQLite, creates attempt-owned native Git worktrees, routes durable worker events to the owning driver, and verifies delivery evidence before cleanup.

Licensed under the [MIT License](LICENSE).

Start with the [documentation overview](docs/README.md) for the end-to-end lifecycle, component map, and command-family guide. Detailed behavior is organized by component under [`docs/components/`](docs/components/), with the authority split in the [authority map](docs/reference/authority.md).

## Supported platforms

Shephrd supports macOS and Linux and requires a POSIX environment.

## Requirements

- Go 1.26.5 or newer
- `git`
- At least one worker harness: `claude`, `pi`, or `codex`
- Authenticated `gh` for GitHub PR verification and the optional GitHub observer extension
- Herdr protocol 17 or newer for the Herdr runtime
- macOS and compatible cmux socket protocol 2 for the cmux runtime

## Build and install

```sh
make build
./bin/shephrd --help
```

This also builds `bin/shephrd-terminal-herdr`, `bin/shephrd-terminal-cmux`, the opt-in `bin/shephrd-notification-macos` notification presentation extension, the opt-in `bin/shephrd-repository-scanner` repository-discovery extension, and the opt-in `bin/shephrd-github-observer` GitHub observation extension. Headless execution is the default and does not launch terminal or notification extensions; discovery, GitHub observation, notifications, Herdr, and cmux are enabled only by their explicit pinned configuration.

Or install through Go:

```sh
go install ./cmd/shephrd ./cmd/shephrd-terminal-herdr ./cmd/shephrd-terminal-cmux ./cmd/shephrd-notification-macos ./cmd/shephrd-repository-scanner ./cmd/shephrd-github-observer
```

A build run directly in a working tree that has uncommitted changes embeds a dirty VCS stamp, so the executable identifies itself as `<commit>+dirty` and cannot be traced to a committed revision. `make build-clean` and `make build-clean-all` clone the current commit into a temporary directory, build there, verify each stamp names that exact revision without a dirty marker, and only then install into `bin/`. The `cli` target installs `bin/shephrd` alone, which is the ordinary case when the deployed control plane must track a commit without disturbing anything else.

`make build-clean-all` also installs the terminal extension, whose bytes are pinned in the live `~/.config/shephrd/config.toml` under `terminal_extensions.herdr` and verified on every launch. When the rebuilt extension differs from that pin, the default remains to print both the built and pinned hashes and install nothing. Choosing a new pin is an explicit decision. To install the rebuilt binaries and repin the live configuration in the same run:

```sh
SHEPHRD_BUILD_CLEAN_REPIN=1 make build-clean-all
```

The opt-in prepares the updated configuration before installation, installs the Herdr binary, then immediately atomically replaces the live configuration with the new pin. This reduces the mismatch window to the adjacent file replacements; it does not bypass the launch-time hash guard. It prints the new hash and the remaining operator steps: the installed extension, live pinned value, flake pin and configuration activated by `darwin-rebuild switch` must agree. The target does not edit the flake or run the switch. A later switch with a stale flake pin would restore the mismatch.

A pin move creates a fail-closed launch window: a sub-driver session that launches the extension while the binary and live pin disagree is held.

Home-manager activation can also open a window when `config.toml` is a real file rather than the home-manager symlink: activation backs up the file before linking the new one, briefly leaving the path absent. A session launching Herdr in that gap fails closed with `terminal runtime herdr requires an explicitly configured first-party extension` and the owner remains held even when the pin and binary hashes agree again. Such a hold emits one correlated blocker per open request on that owner, so one short window can produce many identical notifications.

For either window, after restoring the configuration and binary/pin agreement, inspect the held owner and verify that its recorded runner and harness are absent and its endpoint pane is gone. Recover at the exact inspected generation, then resume:

```sh
shephrd subdriver inspect <subdriver-id> --json
shephrd subdriver recover <subdriver-id> --generation <inspected-generation> --json
shephrd subdriver resume <subdriver-id> --json
```

Uncertain process or endpoint absence is not permission to recover. See [sub-driver recovery](docs/components/subdrivers.md#restart-and-recovery) for the exact identity and absence requirements.

## Minimal flow

Task creation always persists a nonblank title and queues work without starting it. Outside an active Pi session, pass an explicit `--driver-id`. `--title` is optional; when omitted, Shephrd derives a concise title from the objective:

```sh
shephrd repo add ~/Projects/example --json
shephrd task create \
  --repo example \
  --feature improve-search \
  --driver-id driver:example \
  --acceptance "Tests pass and the exact artifact is reviewable" \
  "Improve repository search" \
  --json
shephrd worker spawn <task-id> --json
```

For explicitly requested [repository coordination](docs/components/subdrivers.md), forward original intake with `shephrd subdriver handoff --repo example --key request-001 --request-file request.txt --json`. A durable scoped owner, shown in terminals and watcher notices as `Sub-driver: <registered repository>` or `Sub-driver: General`, plans and supervises ordinary workers in bounded, replaceable sessions. Coordination is opt-in; existing direct-driver commands remain available.

Shephrd never automatically merges, retries or relaunches workers, dispatches planned work, escalates a model, or discards unlanded work. Git worktrees isolate Git state but do not sandbox worker processes. Register only trusted repositories and setup hooks.

## Development

```sh
make test-docs
make test
make test-extension
go vet ./...
git diff --check
```

Contributions are welcome via issues and pull requests. Run these checks before submitting; live provider and notification tests are opt-in and are not part of CI.

On macOS, `make test-notification-macos-live` explicitly opts into one bounded live Notification Center presentation.

Developers can explicitly build the standalone executable diagnostic without adding it to the ordinary build or install:

```sh
make build-freshness
./bin/shephrd-freshness --json ./bin/shephrd
```

The diagnostic only inspects the selected executable and its containing Git checkout. It does not load Shephrd configuration or state, rebuild Shephrd, or run lifecycle services.

Repository contributors and workers follow [AGENTS.md](AGENTS.md). The portable [Shephrd skill](.agents/skills/shephrd/SKILL.md) covers basic execution and lifecycle safety, with [driver guidance](.agents/skills/shephrd/references/driver-policy.md) and detailed mechanics available on demand. Drivers delegate substantive repository work by default, including small tasks, with proportionate investigation and answers. Conversation, answers supported by available context, coordination and lifecycle operations, and explicitly requested direct work can remain direct. Review, PR creation, model choices, decomposition, successor work, and recursive delegation are not automatic. See [repository context, memory, and workflow modules](docs/reference/workflow-modules.md) for the optional Markdown organization and reusable playbooks. The [repository context](.shephrd/context.md) provides a lean overview; optional project memory is local and excluded from this public snapshot. `shephrd repo context <path> --json` discovers this entry point without changing AGENTS.md. Agent-maintained memory is enabled by default; set `[memory] enabled = false` in Shephrd configuration to disable automatic recall and writes. No background indexer or workflow engine is involved.
