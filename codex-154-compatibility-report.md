# Codex 0.154 compatibility report

## Reproduction

At PR #22 head `0702e1eb40f8c26d765aa9cc7a70bdebc71cdb80`, installed `codex-cli 0.154.0` rejected the interactive launch with exit 2:

```text
error: unexpected argument '--no-daemon' found
```

The flag came from commit `5abf421` to avoid Codex's shared server. Codex 0.154.0 no longer advertises that flag. Shephrd already launches the harness synchronously from `_run` in a dedicated process group and waits for it, so removing the obsolete provider argument preserves Shephrd ownership. The execution spec's process and native-session contract does not change.

## Fix

Removed `--no-daemon` from interactive Codex argv. Headless JSON and native resume argv remain unchanged apart from no longer carrying the unsupported interactive flag. Added `internal/execution/harness_test.go` covering new and resume interactive argv exactly.

## Live validation

Built a scratch binary from this branch with an isolated config, SQLite store, repository, daemon and Herdr 0.9.1 server. The actual installed Codex 0.154.0 ran in uniquely named Herdr workspace `Shephrd t15 Codex 0154001`.

- Run 1 opened a Herdr pane, displayed `OpenAI Codex (v0.154.0)`, accepted a Shephrd progress report, produced a result, and closed its pane. Shephrd recorded `turn_reported=true`, `liveness=exited`, and supervisor exit status `-1` after the intentional post-report termination.
- Run 2 opened a new pane and launched native resume as `codex resume --model gpt-6-astra --dangerously-bypass-approvals-and-sandbox <session> <prompt>`. The prior conversation was intact, progress and result reports were accepted, and the pane closed with `liveness=exited` and status `-1`.
- During both runs the primary Codex process shared the `_run` supervisor's process group and session. Scratch daemons, Herdr server, store, workspace and copied auth were removed. Production Shephrd and Herdr processes and configuration were untouched.

## Verification

- `go test ./internal/execution -count=1`
- `make test`
- `make test-docs`
- `make test-extension`
- `go vet ./...`
- `git diff --check`

All passed. Commit: `87d33db317a3fe120d81e04c5f0ec759d0b8afcd`.
