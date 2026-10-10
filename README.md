# Shephrd

Shephrd lets you talk to one main driver that hands repository work, as early as possible, to sub-drivers and isolated workers, and keeps a durable, trustworthy record of what happened. It runs on one machine or across several over SSH.

Licensed under the [MIT License](LICENSE).

## Status

The core is being rebuilt from the approved [system design](docs/design.md), following the [implementation plan](docs/implementation.md). Only the foundation exists so far: `init`, `version`, `repo add` and `repo list`. The previous implementation is documented in [`docs/archive/`](docs/archive/) and remains in git history.

## Requirements

- macOS or Linux
- Go 1.26.5 or newer
- `git`

## Build

```sh
make build
./bin/shephrd init
./bin/shephrd repo add ~/src/my-tool
./bin/shephrd repo list
```

Every command prints one JSON object. Help is the only text output: `shephrd --help`.

## Development

See [AGENTS.md](AGENTS.md) for the repository contract and the checks to run before delivery.
