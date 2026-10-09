# Shephrd engineering documentation

Current architecture and behavior are documented by component rather than in one monolithic design document:

- [Documentation overview and command families](docs/archive/README.md)
- [Repository registration](docs/archive/components/repositories.md)
- [Tasks and titles](docs/archive/components/tasks.md)
- [Attempts and isolated worktrees](docs/archive/components/attempts-worktrees.md)
- [Harnesses and runtimes](docs/archive/components/harnesses-runtimes.md)
- [Worker protocol](docs/archive/components/worker-protocol.md)
- [Notifications and watchers](docs/archive/components/notifications-watchers.md)
- [Plans and evidence handoff](docs/archive/components/plan-evidence.md)
- [Ownership and annotations](docs/archive/components/ownership-annotations.md)
- [Optional review feedback](docs/archive/components/review-feedback.md)
- [Artifacts and reports](docs/archive/components/artifacts-reports.md)
- [Delivery and release](docs/archive/components/delivery-release.md)
- [Recovery](docs/archive/components/recovery.md)
- [Configuration and persistence](docs/archive/components/configuration-state.md)
- [Authority map](docs/archive/reference/authority.md)
- [CLI conventions](docs/archive/reference/cli.md)
- [Repository context, memory, and workflow modules (Markdown convention)](docs/archive/reference/workflow-modules.md)

Current code, embedded migrations, CLI help, and tests enforce mechanics. The component guides are their canonical explanation. User-selected workflow preferences and repository worker guidance remain separate from core lifecycle safety as described by the authority map.
