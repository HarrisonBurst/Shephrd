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

Requirements: macOS or Linux, `git`, and at least one harness: Claude Code (`claude`), Codex (`codex`) or Pi (`pi`).

With Nix:

```sh
nix profile install github:HarrisonBurst/Shephrd
```

This installs `shephrd` and the first-party plugins under `~/.nix-profile/share/shephrd/plugins`. From source, with Go 1.26.5 or newer:

```sh
make build            # bin/shephrd
make plugins          # first-party plugins under plugins/*/bin
```

Put `shephrd` on your `PATH`. Every command prints one JSON object; help is the only text output (`shephrd --help`).

## Set up

[Getting started](docs/getting-started.md) has the details, and the three-machine setup with a home host, a client host for the main driver and a worker host. On one machine:

1. **Initialize.** `shephrd init` writes `~/.config/shephrd/config.toml` and the store, names your main driver `main` and grants it `land` and `discard`.
2. **Configure.** Add a default harness, a presentation and how each repository lands:

   ```toml
   presentation = "herdr"           # watch each task in its own Herdr tab; omit to run headless

   [defaults]
   harness = "claude-code"          # or "codex" or "pi"
   model = "claude-opus-5-5"

   [packages.firstparty]
   path = "~/.nix-profile/share/shephrd"   # or this checkout, after make plugins

   [plugins.herdr]
   package = "firstparty"

   [plugins.herdr.options]
   workspace = "Shephrd tasks"      # a Herdr workspace label or ID, created when missing

   [repos.my-tool.landing]
   mode = "direct"                  # or "pull_request" with the github plugin
   ```

3. **Register repositories.** `shephrd repo add ~/src/my-tool`.
4. **Run the daemon** as a login service, so sub-drivers are woken and results reach you. On macOS, a launchd agent running `shephrd daemon` with `KeepAlive`; on Linux, a systemd user service with `Restart=always` and lingering enabled (`loginctl enable-linger`). Every command warns `daemon_not_running` while it is down.
5. **Give your main driver the skill.** For Claude Code: `mkdir -p ~/.claude/skills/shephrd && shephrd skill main | jq -r .text > ~/.claude/skills/shephrd/SKILL.md`. For a main driver in Pi, also install `share/shephrd/pi/shephrd-inbox.ts` as a Pi extension so inbox items arrive as turns.
6. **Trust each repository once.** In a terminal presentation the harness runs its own interface, and Claude Code and Codex ask once per repository whether you trust it. Answer in the task's tab (Herdr marks it blocked), or open the harness in the repository once beforehand.

Then ask your main driver for work in `my-tool`. It creates a sub-driver with `shephrd task create --role driver --repo my-tool`, which delegates to workers and reports back. With Herdr, each task appears as a tab labelled with its ID, role and title, showing the harness at work; a task keeps its tab across turns, and the tab closes when the task closes.

## Documentation

- [System design](docs/design.md): goal, principles and boundaries.
- [System specs](docs/systems/): coordination, commands and SSH, plugins, session execution, notifications, artifacts and authority, guidance.
- [Implementation plan](docs/implementation.md): how the core was built, and what remains.
- [First-party plugins](plugins/): the GitHub forge, Herdr and cmux presentations, macOS notifications, and the Pi inbox extension.
- The previous implementation is documented in [`docs/archive/`](docs/archive/) and remains in git history.

## Development

See [AGENTS.md](AGENTS.md) for the repository contract and the checks to run before delivery.
