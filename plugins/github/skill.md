# GitHub pull requests

Repositories with `mode = "pull_request"` and `forge = "github"` land code through pull requests on GitHub.

- `shephrd task deliver <task>` pushes the task's branch and opens its pull request, or updates the open one. With `merge = "shephrd"` it also merges once GitHub reports the pull request mergeable; run it again while checks are pending.
- A task stacked on an unmerged dependency opens its pull request against the dependency's branch. When the dependency merges, Shephrd retargets it to the default branch.
- After someone merges a pull request on GitHub, `shephrd task verify <task>` proves the merge and closes the task.
- Review changes: send the worker a message with `shephrd task send`; its new result pushes to the same pull request on the next `task deliver`.

The plugin uses the `gh` CLI, which must be authenticated on the home host. Set `merge_method` to `merge`, `squash` or `rebase` in the plugin's options; the default is `merge`.
