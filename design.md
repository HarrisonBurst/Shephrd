# Shephrd engineering documentation

Current architecture and behavior are documented by component rather than in one monolithic design document:

- [Documentation overview and command families](docs/README.md)
- [Repository registration](docs/components/repositories.md)
- [Tasks and titles](docs/components/tasks.md)
- [Attempts and isolated worktrees](docs/components/attempts-worktrees.md)
- [Harnesses and runtimes](docs/components/harnesses-runtimes.md)
- [Worker protocol](docs/components/worker-protocol.md)
- [Notifications and watchers](docs/components/notifications-watchers.md)
- [Plans and evidence handoff](docs/components/plan-evidence.md)
- [Ownership and annotations](docs/components/ownership-annotations.md)
- [Optional review feedback](docs/components/review-feedback.md)
- [Artifacts and reports](docs/components/artifacts-reports.md)
- [Delivery and release](docs/components/delivery-release.md)
- [Recovery](docs/components/recovery.md)
- [Configuration and persistence](docs/components/configuration-state.md)
- [Authority map](docs/reference/authority.md)
- [CLI conventions](docs/reference/cli.md)
- [Repository context, memory, and workflow modules (Markdown convention)](docs/reference/workflow-modules.md)

Current code, embedded migrations, CLI help, and tests enforce mechanics. The component guides are their canonical explanation. User-selected workflow preferences and repository worker guidance remain separate from core lifecycle safety as described by the authority map.
