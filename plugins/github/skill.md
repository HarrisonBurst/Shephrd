# GitHub pull requests

Repositories with `mode = "pull_request"` and `forge = "github"` land code through pull requests on GitHub.

- `shephrd task accept <task> [--title <text>] [--body <text|->] [--draft]` pushes the task's branch and opens a pull request. Omitted title and body default to the task reference/title and sealed result text; omitted draft opens ready for review. An existing PR is reused without changing its metadata; explicitly requested metadata must match, or publication is refused. With `merge = "shephrd"` it merges once GitHub reports it mergeable; draft PRs stay pending. Run accept again while checks are pending.
- A task stacked on an unmerged dependency opens its pull request against the dependency's branch. When the dependency merges, Shephrd retargets it to the default branch.
- After someone merges a pull request on GitHub, `shephrd task verify <task>` proves the merge and closes the task.
- Review changes: send the worker a message with `shephrd task send`; its new result pushes to the same pull request on the next `task accept`.

The plugin uses the `gh` CLI, which must be authenticated on the home host. Set `merge_method` to `merge`, `squash` or `rebase` in the plugin's options; the default is `merge`.
