# Getting started

This guide sets Shephrd up on one machine, then across three. The [system design](design.md) and [specs](systems/) explain why it works this way.

## One machine

### Initialize

```sh
shephrd init
```

This writes `~/.config/shephrd/config.toml`, creates the store, and makes this machine the home host. The configuration names your main driver `main` and grants it `land` and `discard`, so it can deliver code and discard work in its own trees.

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

Results, questions and held tasks reach the main driver's inbox. `shephrd inbox` lists them; `shephrd inbox ack <item>` marks one handled. To have them pushed, configure a delivery for the driver:

```toml
[drivers.main.delivery]
provider = "webhook"
url = "https://example.internal/shephrd"
secret_file = "/run/secrets/shephrd-webhook"   # mode 0600
```

Deliveries are signed in the Standard Webhooks style. A main driver in Pi can instead use the [Pi inbox extension](#plugins), which holds `shephrd inbox wait` and turns each item into a follow-up turn.

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

Every command on hermes is now forwarded to workhorse over SSH, with the same inputs and outputs. A dropped connection is retried with the same key; if every attempt fails the command exits 3, meaning the outcome is unknown and is safe to retry with the same `--key`.

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
| `herdr`, `cmux` | Presentation: each task gets a terminal tab, labelled with its ID, role and title, where the harness runs its own interface for every turn. The tab closes when the task closes. Set `presentation = "herdr"` (or `"cmux"`) in the host's configuration. Herdr's options are `workspace`, a workspace label or ID that is created when missing (default `Shephrd tasks`), and `socket` when Herdr is not on its default socket. cmux needs `socket` and `window`, and its socket must accept outside processes: set its socket control to password mode and give the plugin `password`. |
| `macos-notify` | A notification for each new inbox item. |

The Pi extension `plugins/pi/shephrd-inbox.ts` (`share/shephrd/pi/` in the Nix package) is not a Shephrd plugin; install it into Pi for a main driver that runs in Pi. Shephrd's own sessions ignore it.

In a terminal presentation, Claude Code and Codex ask once per repository whether you trust it, and the task's tab waits for the answer. Trust a repository beforehand by opening the harness in it once.

`shephrd plugin list`, `plugin status` and `plugin skill <name>` inspect what is declared. Git packages are fetched with `shephrd plugin sync`.
