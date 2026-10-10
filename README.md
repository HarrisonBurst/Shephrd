# Shephrd

Shephrd lets you talk to one main driver that hands repository work, as early as possible, to sub-drivers and isolated workers, and keeps a durable, trustworthy record of what happened. It runs on one machine or across several over SSH.

- **One main driver, recursive delegation.** The agent you talk to delegates each request to a sub-driver for its repository, which plans the work as worker tasks and supervises them. Results flow back up to whoever delegated them.
- **Isolated workspaces.** Every attempt gets its own git worktree. Sub-drivers are read-only; workers make every change.
- **Durable and fail closed.** Tasks, runs, reports and artifacts live in one SQLite store with an append-only event log. When ownership, liveness or lineage is unclear, Shephrd refuses and keeps recoverable state.
- **Explicit authority.** Landing or discarding work needs a grant you gave. Reports, results and acknowledgements are evidence, never approval.
- **SSH parity.** Every command behaves the same locally and over SSH. Sessions can run on worker hosts.
- **Plugins.** Events, intercept hooks, providers and commands, declared in configuration, in any language.

Licensed under the [MIT License](LICENSE).

## Install

Requirements: macOS or Linux, Go 1.26.5 or newer, `git`, and at least one harness: Claude Code (`claude`), Codex (`codex`) or Pi (`pi`).

```sh
make build            # bin/shephrd
make plugins          # first-party plugins under plugins/*/bin
```

Put `bin/shephrd` on your `PATH`. Every command prints one JSON object; help is the only text output (`shephrd --help`).

## Get started

[Getting started](docs/getting-started.md) walks through one machine, then the three-machine setup with a home host, a client host for the main driver, and a worker host.

The short version on one machine:

```sh
shephrd init
shephrd repo add ~/src/my-tool
shephrd daemon &                       # wakes sub-drivers and pushes results
shephrd skill main | jq -r .text > ~/.claude/skills/shephrd/SKILL.md
```

Then ask your main driver for work in `my-tool`. It creates a sub-driver with `shephrd task create --role driver --repo my-tool`, which delegates to workers and reports back.

## Documentation

- [System design](docs/design.md): goal, principles and boundaries.
- [System specs](docs/systems/): coordination, commands and SSH, plugins, session execution, notifications, artifacts and authority, guidance.
- [Implementation plan](docs/implementation.md): how the core was built, and what remains.
- [First-party plugins](plugins/): the GitHub forge, Herdr and cmux presentations, macOS notifications, and the Pi inbox extension.
- The previous implementation is documented in [`docs/archive/`](docs/archive/) and remains in git history.

## Development

See [AGENTS.md](AGENTS.md) for the repository contract and the checks to run before delivery.
