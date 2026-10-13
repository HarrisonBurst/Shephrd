# Getting started

This guide sets Shephrd up on one machine, then across three. The [system design](design.md) and [specs](systems/) explain why it works this way.

## One machine

### Initialize

```sh
shephrd init
```

This writes `~/.config/shephrd/config.toml`, creates the store, and makes this machine the home host. The configuration names your main driver `main` and grants it `land` and `discard`, so it can accept and land code in its own trees; discarding still needs explicit per-action user approval.

Choose a default harness and model:

```toml
[defaults]
harness = "claude-code"
model = "claude-opus-5-5"
```

Route roles or repositories to other harnesses with `[[routes]]`:

```toml
[[routes]]
role = "worker"
harness = "codex"
```

### Register repositories

```sh
shephrd repo add ~/src/my-tool
shephrd repo add ~/src/api --setup "make deps"
```

Code lands through each repository's landing mode, and there is no default, so nothing is pushed by accident:

```toml
[repos.my-tool.landing]
mode = "direct"            # merge into the default branch and push it

[repos.api.landing]
mode = "pull_request"
forge = "github"
merge = "external"         # someone merges on GitHub; shephrd task verify proves it
```

`pull_request` mode needs the GitHub plugin; see [plugins](#plugins).

### Run the daemon

```sh
shephrd daemon
```

The daemon wakes sub-drivers when their workers report, starts continuation turns, stops inactive runs, pushes inbox items and delivers plugin events. Run it whenever Shephrd is in use, as a launchd agent on macOS or a systemd user service on Linux with lingering enabled, so it outlives your login. Give the service a `PATH` that reaches `git` and your harnesses. Without it every command warns `daemon_not_running`.

### Install the main driver's skill

The main driver is any agent session you talk to. Give it Shephrd's skill:

```sh
mkdir -p ~/.claude/skills/shephrd
shephrd skill main | jq -r .text > ~/.claude/skills/shephrd/SKILL.md
```

Sub-drivers and workers get their guidance in every brief, so nothing else is installed. Reference sections, such as recovery, are read on demand with `shephrd skill main --section recovery`. Adjust any role's guidance in configuration:

```toml
[skills.worker]
extend = "/path/to/house-rules.md"
```

### Receive results

Results, questions and held tasks reach the main driver's durable inbox without delivery configuration. The owner uses `shephrd inbox` or `shephrd inbox wait --after <last-received-item-id>` (start at `0`), reads each real item and any referenced artifacts, shares the findings with the user, then explicitly runs the item's `next.ack` command after handling it. A delivery receipt or a completed turn does not acknowledge an item. A restarted watch rescans pending unacknowledged IDs, including IDs at or below its cursor; deduplicate repeated IDs. Idle watches print nothing. Check `shephrd task show` before calling work complete: a reused sub-driver is done only after all requests are answered. Accept report or answer tasks with `shephrd task accept <task>` without a grant; `task deliver` remains a compatibility alias. A sub-driver accepts its own children, and the main driver accepts only the parent once every child is closed, including done code awaiting merge. Accepting code can land it and needs `land`, and a standing `land` grant is prior authorization. Discarding needs `discard` plus explicit per-action user approval, even with a standing grant. Code in pull-request mode stays done until the pull request merges. In Claude Code, install the main-driver skill above and have that session arm its own long-running background `inbox wait` at session start: each actual stdout item is a turn-level notification, while idle and reconnect are silent. No second terminal, external sender, driver registry or incoming connection on the client host is needed. For optional webhook/plugin delivery, configure a provider:

```toml
[drivers.main.delivery]
provider = "webhook"
url = "https://example.internal/shephrd"
secret_file = "/run/secrets/shephrd-webhook"   # mode 0600
```

Webhook deliveries are signed in the Standard Webhooks style and remain compatible with plugins; they never ack. A main driver in Pi can use the [Pi inbox extension](#plugins), which holds `shephrd inbox wait` and sends real items into follow-up turns for explicit handling and acknowledgement. Codex can use the generic inbox/watch commands; first-party turn delivery for Codex is not included.

## Three machines

The [command spec's example](systems/commands.md#example-three-machines):

| Machine | Role |
|---|---|
| `workhorse` | Home host: store, daemon, repositories, sub-drivers and workers |
| `hermes` | Client host: the main driver |
| `laptop` | Worker host: extra sessions for `workhorse` |

On **workhorse**, initialize as above, then declare the worker host:

```toml
[hosts.laptop]
ssh = "me@laptop"
```

In `~/.ssh/authorized_keys` on workhorse, pin each caller's key to its identity:

```text
command="shephrd serve --as driver:main",restrict ssh-ed25519 AAAA... hermes
command="shephrd serve --host laptop",restrict ssh-ed25519 AAAA... laptop
```

On **hermes**:

```sh
shephrd init --home me@workhorse
```

Every command on hermes, including `inbox wait`, is forwarded outbound to workhorse over SSH as the key's driver identity, with the same inputs and outputs. A dropped connection is retried with the same key for mutations; if every attempt fails the command exits 3, meaning the outcome is unknown and is safe to retry with the same `--key`. Restart a dropped inbox watch with the last received ID; pending items will be rescanned.

On **laptop**:

```sh
shephrd init --host laptop --home me@workhorse
```

and in laptop's `authorized_keys`, pin workhorse's key to the agent:

```text
command="shephrd agent",restrict ssh-ed25519 AAAA... workhorse
```

Register a repository that lives on the laptop from workhorse with `shephrd repo add --host laptop /path/on/laptop`. Its sessions run on the laptop and report back through workhorse. If the laptop sleeps, its runs become `unknown` and nothing that needs them gone proceeds until it returns. All three machines must run the same Shephrd release.

## Plugins

Plugins are declared only in the home host's configuration. A package is a git repository pinned to a commit, or a local path:

```toml
[packages.firstparty]
path = "~/.nix-profile/share/shephrd"   # the Nix package, or this repository after make plugins

[plugins.github]
package = "firstparty"

[plugins.github.options]
merge_method = "squash"
```

| Plugin | Provides |
|---|---|
| `github` | The forge for `pull_request` landing, through an authenticated `gh` CLI. |
| `herdr`, `cmux` | Presentation: each task gets a terminal tab, labelled with its human-readable title, where the harness runs its own interface for every turn. An empty title falls back to the stable task ID; duplicate titles may produce identical labels, while Shephrd keeps task identity in its metadata. A reported turn exits its own terminal process, closing the tab; continuation opens a fresh tab and resumes the same native harness session. A turn without a report or a harness that failed to start shows a task-log hint and holds the terminal until Enter or supersession. Execution never closes a tab by endpoint ID. Set `presentation = "herdr"` (or `"cmux"`) in the host's configuration. Herdr's options are `workspace`, a workspace label or ID that is created when missing (default `Shephrd tasks`), and `socket` when Herdr is not on its default socket. cmux needs `socket` and `window`, and its socket must accept outside processes: set its socket control to password mode and give the plugin `password`. |
| `macos-notify` | A notification for each new inbox item. |

The Pi extension `plugins/pi/shephrd-inbox.ts` (`share/shephrd/pi/` in the Nix package) is not a Shephrd plugin; install it into Pi for a main driver that runs in Pi. Shephrd's own sessions ignore it.

In a terminal presentation, Claude Code and Codex ask once per repository whether you trust it, and the task's tab waits for the answer. Trust a repository beforehand by opening the harness in it once.

`shephrd plugin list`, `plugin status` and `plugin skill <name>` inspect what is declared. Git packages are fetched with `shephrd plugin sync`.
