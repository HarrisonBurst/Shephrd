package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"shephrd/internal/claudebridge"
	"shephrd/internal/config"
	"shephrd/internal/control"
	"shephrd/internal/delivery"
	domain "shephrd/internal/model"
	"shephrd/internal/process"
	"shephrd/internal/repository"
	"shephrd/internal/store"
	"shephrd/internal/wakewatch"
)

const standaloneAnnotation = "shephrd.io/standalone"

type application struct {
	config              config.Config
	store               *store.Store
	control             control.Service
	registry            repository.Registry
	json                bool
	legacySubdriverJSON bool
	out                 io.Writer
}

func New() *cobra.Command {
	var app *application
	var jsonOutput, legacySubdriverJSON bool
	root := &cobra.Command{
		Use:               "shephrd",
		Short:             "Local control plane for multi-repo agent orchestration",
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if isCompletionCommand(cmd) {
				if jsonOutput {
					return fmt.Errorf("--json is not supported for shell completion output; omit --json to receive the raw completion script")
				}
				return nil
			}
			if cmd.Annotations[standaloneAnnotation] == "true" {
				return nil
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			state, err := store.Open(cfg.DatabasePath)
			if err != nil {
				return err
			}
			app = &application{config: cfg, store: state, json: jsonOutput, legacySubdriverJSON: legacySubdriverJSON, out: cmd.OutOrStdout()}
			app.control = control.New(cfg, state)
			app.registry = repository.Registry{Config: cfg, Store: state}
			if err := subdriverCommandFence(app, cmd, args); err != nil {
				state.Close()
				return err
			}
			return nil
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if app != nil {
				app.store.Close()
			}
		},
	}
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Print machine-readable JSON where supported")
	root.PersistentFlags().BoolVar(&legacySubdriverJSON, "legacy-coordinator-json", false, "Compatibility-only: emit legacy coordinator-* notification kinds in JSON")
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !jsonOutput {
			defaultHelp(cmd, args)
			return
		}
		var body bytes.Buffer
		output := cmd.OutOrStdout()
		cmd.SetOut(&body)
		defaultHelp(cmd, args)
		cmd.SetOut(output)
		encoder := json.NewEncoder(output)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(helpResponse{SchemaVersion: 1, Command: cmd.CommandPath(), Help: body.String()})
	})
	get := func() *application { return app }
	root.AddCommand(completionCommand(), protocolCommand(), subdriverCommand(get), legacyCommand(get), repoCommand(get), taskCommand(get), planCommand(get), workerCommand(get), wakeCommand(get), workspaceCommand(get), runCommand(get), claudeHookCommand())
	return root
}

type helpResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Help          string `json:"help"`
}

func completionCommand() *cobra.Command {
	command := commandGroup("completion", "Generate shell completion scripts")
	command.Long = "Generate a Cobra completion script for Bash, zsh, or fish."
	command.ValidArgsFunction = cobra.NoFileCompletions
	command.AddCommand(
		completionScriptCommand("bash", func(cmd *cobra.Command, descriptions bool) error {
			return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), descriptions)
		}),
		completionScriptCommand("zsh", func(cmd *cobra.Command, descriptions bool) error {
			if descriptions {
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			}
			return cmd.Root().GenZshCompletionNoDesc(cmd.OutOrStdout())
		}),
		completionScriptCommand("fish", func(cmd *cobra.Command, descriptions bool) error {
			return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), descriptions)
		}),
	)
	return command
}

func completionScriptCommand(shell string, generate func(*cobra.Command, bool) error) *cobra.Command {
	var noDescriptions bool
	command := &cobra.Command{
		Use:                   shell,
		Short:                 "Generate the autocompletion script for " + shell,
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		ValidArgsFunction:     cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return generate(cmd, !noDescriptions)
		},
	}
	command.Flags().BoolVar(&noDescriptions, "no-descriptions", false, "disable completion descriptions")
	return command
}

func repoCommand(get func() *application) *cobra.Command {
	command := commandGroup("repo", "Inspect and manage repository context and registration")
	command.AddCommand(repoContextCommand())
	printRepos := func(repos []domain.Repo) error {
		return get().print(repos, func() {
			for _, repo := range repos {
				context := repo.ContextFile
				if context == "" {
					context = "no context file"
				}
				fmt.Fprintf(get().out, "%s\t%s\t%s\t%s\t%s\n", repo.ID, repo.Name, repo.Path, repo.DefaultBranch, context)
			}
		})
	}
	command.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List registered repositories without scanning or mutation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repos, err := get().store.Repos()
			if err != nil {
				return err
			}
			return printRepos(repos)
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "scan",
		Short: "Discover repository candidates without registering them",
		Long:  "Invoke the explicitly configured repository-discovery capability and return bounded canonical Git repository candidates without mutating the registry.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			candidates, err := get().registry.Scan()
			if err != nil {
				return err
			}
			return get().print(candidates, func() {
				for _, candidate := range candidates {
					fmt.Fprintln(get().out, candidate.Path)
				}
			})
		},
	})
	var addName, addContext, addSetup, addDefaultBranch string
	add := &cobra.Command{
		Use:   "add <path|name>",
		Short: "Register an existing git repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := get().registry.Add(args[0], addName, addContext, addSetup, addDefaultBranch)
			if err != nil {
				return err
			}
			return get().print(repo, func() { fmt.Fprintf(get().out, "registered %s at %s\n", repo.Name, repo.Path) })
		},
	}
	add.Flags().StringVar(&addName, "name", "", "Unique registry alias")
	add.Flags().StringVar(&addContext, "context", "", "Context file path relative to the repo")
	add.Flags().StringVar(&addSetup, "setup-hook", "", "Shell command to run after each worktree acquire")
	add.Flags().StringVar(&addDefaultBranch, "default-branch", "", "Existing local branch accepted as the repository default")
	command.AddCommand(add)
	return command
}

func taskCommand(get func() *application) *cobra.Command {
	command := commandGroup("task", "Create and inspect tasks")
	var repoRef, feature, title, acceptance, deliverable, createDriverID string
	var reportFrom []string
	create := &cobra.Command{
		Use:   "create [flags] <objective>",
		Short: "Create a queued task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if repoRef == "" {
				return fmt.Errorf("--repo is required; use a registered repo name, ID, or canonical path")
			}
			if strings.TrimSpace(feature) == "" {
				return fmt.Errorf("--feature is required; use a short stable key such as auth-refresh")
			}
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("task objective cannot be empty")
			}
			if deliverable != "code" && deliverable != "report" {
				return fmt.Errorf("--deliverable must be code or report")
			}
			for _, producerTaskID := range reportFrom {
				if strings.TrimSpace(producerTaskID) == "" {
					return fmt.Errorf("--with-report-from requires a producer task ID")
				}
			}
			driverID := strings.TrimSpace(createDriverID)
			if driverID == "" {
				if sessionID := strings.TrimSpace(os.Getenv("PI_SESSION_ID")); sessionID != "" {
					driverID = "driver:pi:" + sessionID
				} else {
					return fmt.Errorf("--driver-id is required outside an active Pi session")
				}
			}
			repo, err := get().registry.Resolve(repoRef)
			if err != nil {
				return err
			}
			task, err := get().control.CreateTaskWithReports(domain.Task{RepoID: repo.ID, FeatureKey: feature, Title: domain.DeriveTitle(title, args[0], feature), DriverID: driverID,
				Objective: args[0], AcceptanceCriteria: acceptance, Deliverable: deliverable}, reportFrom)
			if err != nil {
				return err
			}
			return get().print(task, func() {
				fmt.Fprintf(get().out, "created %s: %s (%s, feature %s)\n", task.ID, task.Title, task.Status, task.FeatureKey)
			})
		},
	}
	create.Flags().StringVar(&repoRef, "repo", "", "Registered repo reference")
	create.Flags().StringVar(&feature, "feature", "", "Stable feature key")
	create.Flags().StringVar(&title, "title", "", "Human-readable task title; defaults to a concise title derived from the objective")
	create.Flags().StringVar(&acceptance, "acceptance", "", "Acceptance criteria")
	create.Flags().StringVar(&deliverable, "deliverable", "code", "Artifact contract: code requires branch/PR; report requires the exact report path")
	create.Flags().StringVar(&createDriverID, "driver-id", "", "Originating driver identity; inferred from PI_SESSION_ID inside Pi")
	create.Flags().StringArrayVar(&reportFrom, "with-report-from", nil, "Attach an explicitly selected verified report producer; repeat to preserve multiple inputs in flag order")
	command.AddCommand(create)
	var statusFilter, repoFilter string
	var includeArchived bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List task inventory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if statusFilter != "" && !domain.ValidTaskStatus(statusFilter) {
				return fmt.Errorf("--status must be queued, starting, working, waiting, done, blocked, failed, or stopped")
			}
			filter := store.TaskFilter{Status: statusFilter, IncludeArchived: includeArchived}
			if repoFilter != "" {
				repo, err := get().registry.Resolve(repoFilter)
				if err != nil {
					return err
				}
				filter.RepoID = repo.ID
			}
			tasks, err := get().store.Tasks(filter)
			if err != nil {
				return err
			}
			return get().print(tasks, func() {
				for _, task := range tasks {
					fmt.Fprintf(get().out, "%s\t%s\t%s\t%s\t%s\n", task.ID, task.Status, task.RepoName, task.FeatureKey, task.Title)
				}
			})
		},
	}
	list.Flags().StringVar(&statusFilter, "status", "", "Filter by task status")
	list.Flags().StringVar(&repoFilter, "repo", "", "Filter by registered repo")
	list.Flags().BoolVar(&includeArchived, "include-archived", false, "Include soft-archived terminal tasks")
	command.AddCommand(list)
	inspect := &cobra.Command{
		Use:   "inspect <task-id>",
		Short: "Inspect one task's stored audit detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			detail, err := get().store.Detail(args[0])
			if err != nil {
				return err
			}
			return get().print(detail, func() {
				archived := ""
				if detail.Task.ArchivedAt != nil {
					archived = " archived=" + detail.Task.ArchivedAt.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(get().out, "%s %s: %s feature=%s owner=%s%s\n%s\n", detail.Task.ID, detail.Task.Status, detail.Task.Title, detail.Task.FeatureKey, detail.Task.DriverID, archived, detail.Task.Objective)
				for _, input := range detail.Inputs {
					fmt.Fprintf(get().out, "input %d %s: report from %s attempt=%s done-message=%d sha256=%s bytes=%d attached-by=%s\n", input.Position, input.ArtifactID, input.ProducerTaskID, input.ProducerAttemptID, input.DoneMessageID, input.SHA256, input.SizeBytes, input.AttachedByDriverID)
				}
				for _, attempt := range detail.Attempts {
					tab, pane := "", ""
					if endpoint := attempt.TerminalEndpoint; endpoint != nil {
						tab, pane = endpoint.TabID, endpoint.PaneID
					}
					fmt.Fprintf(get().out, "attempt %s: %s %s executable=%s model=%s session=%s tab=%s pane=%s\n", attempt.ID, control.WorkerLabel(detail.Task.RepoName, detail.Task.Title, detail.Task.FeatureKey, detail.Task.ID), attempt.RuntimeBackend, attempt.RuntimeExecutable, attempt.Model, attempt.SessionID, tab, pane)
					if endpoint := attempt.TerminalEndpoint; endpoint != nil {
						fmt.Fprintf(get().out, "  terminal endpoint: window=%s workspace=%s tab=%s pane=%s surface=%s\n", endpoint.WindowID, endpoint.WorkspaceID, endpoint.TabID, endpoint.PaneID, endpoint.SurfaceID)
					}
				}
				for _, message := range detail.Messages {
					fmt.Fprintf(get().out, "[%s] %s: %s\n", message.Type, message.Direction, message.Payload)
				}
				for _, annotation := range detail.Annotations {
					fmt.Fprintf(get().out, "[annotation] revision=%d author=%s judgment=%s reason=%s next-action=%s\n", annotation.Revision, annotation.DriverID, annotation.Judgment, annotation.Reason, annotation.NextAction)
				}
				for _, checkpoint := range detail.Checkpoints {
					fmt.Fprintf(get().out, "[checkpoint] attempt=%s revision=%d producer=%s run-generation=%d source-cursor=%d captured=%s\n", checkpoint.AttemptID, checkpoint.Revision, checkpoint.Producer, checkpoint.RunGeneration, checkpoint.SourceCursor, checkpoint.CapturedAt.UTC().Format(time.RFC3339))
					fmt.Fprintf(get().out, "  summary: %s\n", checkpoint.Summary)
					printCheckpointItems(get().out, "completed", checkpoint.Completed)
					printCheckpointItems(get().out, "next steps", checkpoint.NextSteps)
					for _, decision := range checkpoint.Decisions {
						fmt.Fprintf(get().out, "  decision: %s (reason: %s)\n", decision.Decision, decision.Reason)
					}
					printCheckpointItems(get().out, "changed paths", checkpoint.ChangedPaths)
					for _, check := range checkpoint.Checks {
						fmt.Fprintf(get().out, "  check: %s: %s\n", check.Command, check.Result)
					}
					printCheckpointItems(get().out, "blockers", checkpoint.Blockers)
				}
				for _, notification := range detail.Notifications {
					fmt.Fprintf(get().out, "[notification] %s %s owner=%s\n", notification.NotificationID, notification.State, notification.TargetDriverID)
				}
				for _, invocation := range detail.ReportLifecycleInvocations {
					fmt.Fprintf(get().out, "[report.accepted] event=%s handler=%s state=%s attempts=%d", invocation.EventID, invocation.HandlerName, invocation.State, invocation.Attempts)
					if invocation.Annotation != "" {
						fmt.Fprintf(get().out, " annotation=%s", invocation.Annotation)
					}
					if invocation.ReceiptID != "" {
						fmt.Fprintf(get().out, " receipt=%s:%s", invocation.ReceiptSystem, invocation.ReceiptID)
					}
					if invocation.FailureMessage != "" {
						fmt.Fprintf(get().out, " failure=%s:%s", invocation.FailureKind, invocation.FailureMessage)
					}
					fmt.Fprintln(get().out)
					if invocation.RetryCommand != "" {
						fmt.Fprintln(get().out, "  recovery: "+invocation.RetryCommand)
					}
				}
				for _, attestation := range detail.ExternalDeliveryAttestations {
					fmt.Fprintf(get().out, "[external-delivery] original %s unchanged; PR #%d %s attested for sealed commit %s by %s\n", attestation.OriginalArtifactRef, attestation.PRNumber, attestation.PRURL, attestation.SealedCommit, attestation.AttestedByDriverID)
				}
				for _, recovery := range detail.LocalDeliveryRecoveries {
					fmt.Fprintf(get().out, "[local-recovery] original %s unchanged; branch %s attested for sealed commit %s into %s by %s\n", recovery.OriginalArtifactRef, recovery.AttemptBranch, recovery.SealedCommit, recovery.RegisteredDefaultBranch, recovery.AttestedByDriverID)
				}
			})
		},
	}
	command.AddCommand(inspect)
	archive := &cobra.Command{
		Use:   "archive <task-id>",
		Short: "Soft-archive a terminal task without releasing its worktree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, err := get().control.ArchiveTask(args[0])
			if err != nil {
				return err
			}
			return get().print(task, func() {
				fmt.Fprintf(get().out, "archived %s at %s\n", task.ID, task.ArchivedAt.UTC().Format(time.RFC3339))
			})
		},
	}
	command.AddCommand(archive)
	var adoptDriverID, adoptNewDriverID string
	adopt := &cobra.Command{
		Use:   "adopt <task-id>",
		Short: "Transfer a currently owned task and its active notifications to another driver",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			currentDriverID, err := resolveDriverID(adoptDriverID, cmd.Flags().Changed("driver-id"), true)
			if err != nil {
				return err
			}
			if strings.TrimSpace(adoptNewDriverID) == "" {
				return fmt.Errorf("--new-driver-id is required")
			}
			result, err := get().store.AdoptTask(args[0], currentDriverID, adoptNewDriverID)
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintf(get().out, "adopted %s from %s to %s; retargeted %d notifications\n", result.TaskID, result.PreviousDriverID, result.DriverID, result.Retargeted)
			})
		},
	}
	adopt.Flags().StringVar(&adoptDriverID, "driver-id", "", "Current task owner; inferred from PI_SESSION_ID inside Pi")
	adopt.Flags().StringVar(&adoptNewDriverID, "new-driver-id", "", "New stable driver identity")
	command.AddCommand(adopt)
	var attestPR, attestCommit, attestAttempt, attestDriverID string
	attest := &cobra.Command{
		Use:   "attest-delivery <task-id>",
		Short: "Attest a merged external PR for an unchanged branch artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("attempt") && strings.TrimSpace(attestAttempt) == "" {
				return domain.Failure("attempt_not_eligible", "--attempt requires one attempt ID")
			}
			if strings.TrimSpace(attestPR) == "" {
				return domain.Failure("pr_url_invalid", "--pr is required")
			}
			if strings.TrimSpace(attestCommit) == "" {
				return domain.Failure("sealed_commit_mismatch", "--commit is required and must name the full sealed checkpoint commit")
			}
			driverID, err := resolveDriverID(attestDriverID, cmd.Flags().Changed("driver-id"), true)
			if err != nil {
				return err
			}
			result, err := get().control.AttestDelivery(args[0], attestAttempt, driverID, attestPR, attestCommit, cmd.Flags().Changed("attempt"))
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintf(get().out, "attestation %s task=%s attempt=%s checkpoint=%d\n", result.AttestationID, result.TaskID, result.AttemptID, result.CheckpointRevision)
				fmt.Fprintf(get().out, "PR %s head=%s relation=%s merge=%s target=%s\n", result.PRURL, result.PRHeadCommit, result.CommitRelation, result.MergeCommit, result.TargetRef)
				fmt.Fprintf(get().out, "original artifact %s was not replaced; landing_proven=%t release_state=%s\n", result.OriginalArtifactRef, result.LandingProven, result.ReleaseState)
				fmt.Fprintln(get().out, result.NextCommand)
			})
		},
	}
	attest.Flags().StringVar(&attestPR, "pr", "", "Canonical merged GitHub pull request URL")
	attest.Flags().StringVar(&attestCommit, "commit", "", "Full sealed worker checkpoint commit")
	attest.Flags().StringVar(&attestAttempt, "attempt", "", "Accepted done attempt ID, required for a superseded attempt")
	attest.Flags().StringVar(&attestDriverID, "driver-id", "", "Current task owner; inferred from PI_SESSION_ID inside Pi")
	command.AddCommand(attest)
	var reportRecoveryAttempt, reportRecoveryDriverID, reportRecoveryReason string
	var reportRecoveryGeneration, reportRecoveryRevision int
	var reportRecoveryCursor int64
	recoverReport := &cobra.Command{
		Use:   "attest-report-recovery <task-id>",
		Short: "Attest a canonical report after terminal worker handoff failure",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reportRecoveryAttempt) == "" {
				return domain.Failure("attempt_not_eligible", "--attempt is required and must explicitly name the current report attempt")
			}
			if !cmd.Flags().Changed("run-generation") || reportRecoveryGeneration < 1 {
				return domain.Failure("stale_generation", "--run-generation is required and must be at least 1")
			}
			if !cmd.Flags().Changed("checkpoint-revision") || reportRecoveryRevision < 1 {
				return domain.Failure("checkpoint_not_current", "--checkpoint-revision is required and must be at least 1")
			}
			if !cmd.Flags().Changed("checkpoint-cursor") || reportRecoveryCursor < 1 {
				return domain.Failure("checkpoint_not_current", "--checkpoint-cursor is required and must be at least 1")
			}
			driverID, err := resolveDriverID(reportRecoveryDriverID, cmd.Flags().Changed("driver-id"), true)
			if err != nil {
				return err
			}
			result, err := get().control.AttestReportRecovery(args[0], reportRecoveryAttempt, driverID, reportRecoveryReason,
				reportRecoveryGeneration, reportRecoveryRevision, reportRecoveryCursor)
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintf(get().out, "report recovery %s task=%s attempt=%s generation=%d checkpoint=%d cursor=%d\n", result.AttestationID, result.TaskID, result.AttemptID, result.RunGeneration, result.CheckpointRevision, result.CheckpointSourceCursor)
				fmt.Fprintf(get().out, "report %s sha256=%s bytes=%d file=%s\n", result.CanonicalReportPath, result.SHA256, result.SizeBytes, result.FileIdentity)
				fmt.Fprintf(get().out, "driver evidence recorded; landing_proven=%t release_state=%s\n", result.LandingProven, result.ReleaseState)
				fmt.Fprintln(get().out, result.NextCommand)
			})
		},
	}
	recoverReport.Flags().StringVar(&reportRecoveryAttempt, "attempt", "", "Exact current report attempt ID")
	recoverReport.Flags().IntVar(&reportRecoveryGeneration, "run-generation", 0, "Exact current worker run generation")
	recoverReport.Flags().IntVar(&reportRecoveryRevision, "checkpoint-revision", 0, "Exact final worker checkpoint revision")
	recoverReport.Flags().Int64Var(&reportRecoveryCursor, "checkpoint-cursor", 0, "Exact final worker checkpoint source cursor")
	recoverReport.Flags().StringVar(&reportRecoveryReason, "reason", "", "Bounded driver reason for exceptional report recovery")
	recoverReport.Flags().StringVar(&reportRecoveryDriverID, "driver-id", "", "Current task owner; inferred from PI_SESSION_ID inside Pi")
	command.AddCommand(recoverReport)
	var verifyLocal bool
	var verifyAttempt, verifyDriverID string
	verify := &cobra.Command{
		Use:   "verify-delivery <task-id>",
		Short: "Verify delivery evidence and conditionally release the workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("attempt") && strings.TrimSpace(verifyAttempt) == "" {
				return domain.Failure("attempt_not_eligible", "--attempt requires one attempt ID")
			}
			driverID, resolveErr := resolveDriverID(verifyDriverID, cmd.Flags().Changed("driver-id"), false)
			if resolveErr != nil {
				return resolveErr
			}
			var result delivery.Result
			var err error
			if verifyLocal {
				result, err = get().control.VerifyLocal(args[0], verifyAttempt, driverID)
			} else {
				result, err = get().control.VerifyAttempt(args[0], verifyAttempt, driverID)
			}
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintln(get().out, result.Reason)
				printReportLifecycle(get().out, result.LifecycleHandlers, "", true)
			})
		},
	}
	verify.Flags().BoolVar(&verifyLocal, "local", false, "Verify exact ancestry in the registered local default branch without network access")
	verify.Flags().StringVar(&verifyAttempt, "attempt", "", "Accepted done attempt ID, including an externally attested superseded attempt")
	verify.Flags().StringVar(&verifyDriverID, "driver-id", "", "Current task owner for attested verification, including attested local recovery; inferred from PI_SESSION_ID inside Pi")
	command.AddCommand(verify)

	var taskAnnotateReason, taskAnnotateNextAction, taskAnnotateDriverID string
	var taskAnnotateRevision int
	taskAnnotate := &cobra.Command{
		Use:   "annotate <task-id> <judgment>",
		Short: "Append one revision-fenced recovery annotation to a task",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			currentOwner, err := resolveDriverID(taskAnnotateDriverID, cmd.Flags().Changed("driver-id"), true)
			if err != nil {
				return err
			}
			return addAnnotation(get(), domain.AnnotationScope{TaskID: args[0]}, currentOwner, taskAnnotateRevision, domain.AnnotationInput{Judgment: args[1], Reason: taskAnnotateReason, NextAction: taskAnnotateNextAction})
		},
	}
	taskAnnotate.Flags().StringVar(&taskAnnotateDriverID, "driver-id", "", "Current task owner; inferred from PI_SESSION_ID inside Pi")
	taskAnnotate.Flags().StringVar(&taskAnnotateReason, "reason", "", "Bounded reason for the annotation")
	taskAnnotate.Flags().StringVar(&taskAnnotateNextAction, "next-action", "", "Bounded next explicit driver action")
	taskAnnotate.Flags().IntVar(&taskAnnotateRevision, "expect-revision", 0, "Expected prior revision, using 0 for the first annotation")
	command.AddCommand(taskAnnotate)

	taskAnnotations := &cobra.Command{
		Use:   "annotations <task-id>",
		Short: "List a task annotation scope in revision order",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return listAnnotations(get(), domain.AnnotationScope{TaskID: args[0]})
		},
	}
	command.AddCommand(taskAnnotations)

	command.AddCommand(obligationsCommand(get))
	return command
}

func baseSelectionFromFlags(cmd *cobra.Command, branch, task, commit string) (domain.BaseSelection, bool, error) {
	flags := []struct {
		name     string
		value    string
		strategy string
	}{
		{name: "base-branch", value: branch, strategy: domain.BaseStrategyBranch},
		{name: "base-task", value: task, strategy: domain.BaseStrategyTask},
		{name: "base-commit", value: commit, strategy: domain.BaseStrategyCommit},
	}
	var selected *struct {
		strategy string
		value    string
	}
	for _, flag := range flags {
		if !cmd.Flags().Changed(flag.name) {
			continue
		}
		if selected != nil {
			return domain.BaseSelection{}, false, fmt.Errorf("only one of --base-branch, --base-task, or --base-commit may be specified")
		}
		selected = &struct {
			strategy string
			value    string
		}{strategy: flag.strategy, value: flag.value}
	}
	if selected == nil {
		return domain.DefaultBaseSelection(), false, nil
	}
	selection, err := domain.NormalizeBaseSelection(domain.BaseSelection{Strategy: selected.strategy, Ref: selected.value})
	return selection, true, err
}

func workerCommand(get func() *application) *cobra.Command {
	command := commandGroup("worker", "Manage worker attempts")
	var harness, model, runtime string
	var baseBranch, baseTask, baseCommit string
	spawn := &cobra.Command{
		Use:   "spawn <task-id>",
		Short: "Acquire a worktree and start a fresh worker session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			baseSelection, _, err := baseSelectionFromFlags(cmd, baseBranch, baseTask, baseCommit)
			if err != nil {
				return err
			}
			result, err := get().control.SpawnWithBaseSelection(args[0], harness, model, cmd.Flags().Changed("model"), baseSelection, runtime)
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintf(get().out, "spawned %s (%s) in %s\n", result.Attempt.ID, result.Label, result.Attempt.WorktreePath)
				for _, warning := range result.Warnings {
					fmt.Fprintf(get().out, "warning: %s\n", warning)
				}
			})
		},
	}
	spawn.Flags().StringVar(&harness, "harness", "", "Worker harness: claude-code, pi, or codex")
	spawn.Flags().StringVar(&model, "model", "", "Harness-specific model identifier")
	spawn.Flags().StringVar(&runtime, "runtime", "", "Worker runtime: headless, auto, herdr, or cmux")
	spawn.Flags().StringVar(&baseBranch, "base-branch", "", "Explicit local branch to use as the worker base")
	spawn.Flags().StringVar(&baseTask, "base-task", "", "Explicit task whose current attempt branch is the worker base")
	spawn.Flags().StringVar(&baseCommit, "base-commit", "", "Explicit full immutable Git commit to use as the worker base")
	command.AddCommand(spawn)
	relaunch := &cobra.Command{
		Use:   "relaunch <task-id>",
		Short: "Start a fresh session in the current verified worktree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := get().control.Relaunch(args[0])
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintf(get().out, "relaunched %s in %s\n", result.Attempt.ID, result.Attempt.WorktreePath)
			})
		},
	}
	command.AddCommand(relaunch)
	status := &cobra.Command{
		Use:   "status <task-id>",
		Short: "Show current worker process and session status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, err := get().store.Task(args[0])
			if err != nil {
				return err
			}
			if task.CurrentAttemptID == "" {
				return fmt.Errorf("task %s has no worker attempt", task.ID)
			}
			attempt, err := get().store.Attempt(task.CurrentAttemptID)
			if err != nil {
				return err
			}
			state := "dead"
			if process.Alive(attempt.RunnerPID) {
				state = "alive-busy"
			} else if task.Status == domain.TaskStatusWaiting && attempt.SessionID != "" {
				state = "alive-idle"
			}
			endpointPresent, endpointErr := get().control.EndpointStatus(task.ID)
			if endpointErr != nil && attempt.RuntimeBackend != "headless" {
				return endpointErr
			}
			checkpointRevision, checkpointProducer := 0, ""
			if checkpoint, checkpointErr := get().store.LatestCheckpoint(attempt.ID); checkpointErr == nil {
				checkpointRevision, checkpointProducer = checkpoint.Revision, checkpoint.Producer
			}
			value := map[string]any{"schema_version": 2, "task_id": task.ID, "driver_id": task.DriverID, "attempt_id": attempt.ID, "title": task.Title, "feature_key": task.FeatureKey, "label": control.WorkerLabel(task.RepoName, task.Title, task.FeatureKey, task.ID), "session_id": attempt.SessionID, "model": attempt.Model, "status": state, "pid": attempt.RunnerPID, "runtime_backend": attempt.RuntimeBackend, "runtime_generation": attempt.RuntimeGeneration, "runtime_executable": attempt.RuntimeExecutable, "run_generation": attempt.RunGeneration, "checkpoint_revision": checkpointRevision, "checkpoint_producer": checkpointProducer, "workspace_backend": attempt.WorkspaceBackend, "workspace_state": attempt.WorkspaceState, "worktree_path": attempt.WorktreePath, "terminal_endpoint": attempt.TerminalEndpoint, "terminal_create_intent": attempt.TerminalCreateIntent, "endpoint_present": endpointPresent}
			return get().print(value, func() {
				fmt.Fprintf(get().out, "%s executable=%s workspace=%s/%s\n", state, attempt.RuntimeExecutable, attempt.WorkspaceBackend, attempt.WorkspaceState)
			})
		},
	}
	command.AddCommand(status)
	focus := &cobra.Command{
		Use:   "focus <task-id>",
		Short: "Focus the exact recorded terminal endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := get().control.Focus(args[0]); err != nil {
				return err
			}
			return get().print(map[string]any{"task_id": args[0], "focused": true}, func() { fmt.Fprintln(get().out, "worker focused") })
		},
	}
	command.AddCommand(focus)
	var peekLines int
	peek := &cobra.Command{
		Use:   "peek <task-id>",
		Short: "Read the exact recorded terminal worker endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := get().control.Peek(args[0], peekLines)
			if err != nil {
				return err
			}
			return get().print(result, func() { fmt.Fprint(get().out, result.Output) })
		},
	}
	peek.Flags().IntVar(&peekLines, "lines", 200, "Number of recent pane lines to read")
	command.AddCommand(peek)
	intent := commandGroup("intent", "Manage unresolved terminal create intents")
	intent.AddCommand(&cobra.Command{
		Use:   "clear <task-id>",
		Short: "Clear the unresolved terminal create intent after resolving any orphaned terminal endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			intent, err := get().control.ClearTerminalCreateIntent(args[0])
			if err != nil {
				return err
			}
			return get().print(intent, func() {
				fmt.Fprintf(get().out, "cleared unresolved terminal create intent for run %d (source %s, label %s)\n", intent.RunGeneration, intent.Source, intent.Label)
			})
		},
	})
	command.AddCommand(intent)
	command.AddCommand(sendCommand(get), stopCommand(get), retryCommand(get))
	return command
}

func sendCommand(get func() *application) *cobra.Command {
	return &cobra.Command{
		Use:   "send <task-id> <follow-up>",
		Short: "Resume a waiting worker session with a follow-up",
		Long:  "Resume a waiting worker session with a follow-up. A busy rejection means the follow-up was not queued or delivered and no hold was applied. Gate delivery claims on a successful receipt; annotations and narrative plans are not delivered instructions. Terminal tasks cannot be resumed with send.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[1]) == "" {
				return fmt.Errorf("follow-up text cannot be empty")
			}
			attempt, err := get().control.Send(args[0], args[1])
			if err != nil {
				return err
			}
			return get().print(attempt, func() { fmt.Fprintf(get().out, "resumed %s\n", attempt.ID) })
		},
	}
}

func wakeCommand(get func() *application) *cobra.Command {
	command := commandGroup("wake", "Activate sub-drivers, drain and acknowledge nonblocking driver notifications")
	var pumpDriver string
	pump := &cobra.Command{
		Use:   "pump",
		Short: "Activate a bounded pass of pending sub-drivers without claiming main notifications",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !get().config.Wake.Enabled {
				return fmt.Errorf("wake pump is disabled by wake.enabled")
			}
			driver, err := resolveDriverID(pumpDriver, cmd.Flags().Changed("driver-id"), true)
			if err != nil {
				return err
			}
			release, err := wakewatch.Guard(get().config.DataDir, driver)
			if err != nil {
				return err
			}
			defer release()
			if err := get().control.PumpSubdrivers(driver); err != nil {
				return err
			}
			return get().print(map[string]any{"pumped": true, "driver_id": driver}, func() {
				fmt.Fprintln(get().out, "sub-driver activation pass complete")
			})
		},
	}
	pump.Flags().StringVar(&pumpDriver, "driver-id", "", "Current main return owner")
	command.AddCommand(pump)
	var limit int
	var driverID, driverGeneration string
	var claimTTL time.Duration
	drain := &cobra.Command{
		Use:   "drain [task-id]",
		Short: "Claim a bounded batch of current driver notifications without waiting",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := ""
			if len(args) == 1 {
				if _, err := get().store.Task(args[0]); err != nil {
					return err
				}
				taskID = args[0]
			}
			if !get().config.Wake.Enabled {
				return fmt.Errorf("wake drain is disabled by wake.enabled")
			}
			if driverID == "" {
				if inferred := inferredPIDriverID(); inferred != "" {
					driverID = inferred
				} else {
					driverID = get().config.Wake.DriverID
				}
			}
			if driverID == "" {
				return fmt.Errorf("--driver-id is required when no current Pi driver or wake.driver_id is configured")
			}
			release, err := wakewatch.Guard(get().config.DataDir, driverID)
			if err != nil {
				return err
			}
			defer release()
			if driverGeneration == "" {
				driverGeneration = store.NewID("generation")
			}
			if limit == 0 {
				limit = get().config.Wake.DefaultBatch
			}
			if limit > get().config.Wake.MaxBatch {
				return fmt.Errorf("notification limit must not exceed configured wake.max_batch (%d)", get().config.Wake.MaxBatch)
			}
			if claimTTL == 0 {
				claimTTL = get().config.Wake.ClaimTTL
			}
			if claimTTL < get().config.Wake.ClaimTTLMin || claimTTL > get().config.Wake.ClaimTTLMax {
				return fmt.Errorf("claim TTL must be between %s and %s", get().config.Wake.ClaimTTLMin, get().config.Wake.ClaimTTLMax)
			}
			if err := get().control.SweepDeaths(); err != nil {
				return err
			}
			if err := get().control.PumpSubdrivers(driverID); err != nil {
				return err
			}
			result, err := get().store.DrainNotifications(taskID, driverID, driverGeneration, limit, claimTTL)
			if err != nil {
				return err
			}
			return get().print(result, func() {
				for _, notice := range result.Notifications {
					fmt.Fprintf(get().out, "%s\t%s\t%s\n", notice.NotificationID, notice.TaskID, notice.Kind)
					printReportLifecycle(get().out, notice.ReportLifecycle, "  ", false)
				}
			})
		},
	}
	drain.Flags().IntVar(&limit, "limit", 0, "Maximum number of notifications to claim")
	drain.Flags().StringVar(&driverID, "driver-id", "", "Stable consumer identity")
	drain.Flags().StringVar(&driverGeneration, "driver-generation", "", "Consumer session generation")
	drain.Flags().DurationVar(&claimTTL, "claim-ttl", 0, "Claim lease duration")
	command.AddCommand(drain, watchCommand(get), parkedCommand(get), unparkCommand(get))

	var ackToken, ackDriverID, ackGeneration, handlingID string
	ack := &cobra.Command{
		Use:   "ack [notification-id]",
		Short: "Acknowledge a notification after the driver handled it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			notificationID := ""
			if len(args) == 1 {
				notificationID = args[0]
			}
			notification, err := resolveWakeClaim(get(), "ack", notificationID, ackToken, ackDriverID, ackGeneration, cmd.Flags().Changed("driver-id"), cmd.Flags().Changed("driver-generation"))
			if err != nil {
				return err
			}
			resolvedHandlingID := handlingID
			if cmd.Flags().Changed("handling-id") && strings.TrimSpace(resolvedHandlingID) == "" {
				return wakeClaimFailure("claim_identity_missing", wakeClaimCommand("ack", notification), "handling identity is missing; --handling-id requires a non-empty value")
			}
			if len([]byte(resolvedHandlingID)) > store.MaxNotificationHandlingID {
				return wakeClaimFailure("claim_conflict", wakeClaimCommand("ack", notification), "handling ID must not exceed %d bytes", store.MaxNotificationHandlingID)
			}
			if resolvedHandlingID == "" {
				if notification.State == domain.NotificationAcknowledged && notification.HandlingID != "" {
					resolvedHandlingID = notification.HandlingID
				} else {
					resolvedHandlingID = store.NewID("handling")
				}
			} else if notification.State == domain.NotificationAcknowledged && resolvedHandlingID != notification.HandlingID {
				return wakeClaimFailure("claim_conflict", wakeClaimCommand("ack", notification), "handling ID %q conflicts with stored handling ID %q for notification %s", resolvedHandlingID, notification.HandlingID, notification.NotificationID)
			}
			request := domain.NotificationAckRequest{NotificationID: notification.NotificationID, ClaimToken: notification.ClaimToken, ConsumerID: notification.ClaimOwner, DriverGeneration: notification.DriverGeneration, HandlingID: resolvedHandlingID}
			receipt, err := get().store.AckNotification(request)
			if err != nil {
				return wakeMutationFailure(get(), "ack", notification, err)
			}
			return get().print(receipt, func() {
				fmt.Fprintf(get().out, "acknowledged %s with handling ID %s\n", receipt.NotificationID, receipt.HandlingID)
			})
		},
	}
	ack.Flags().StringVar(&ackToken, "claim-token", "", "Opaque claim token returned by wake drain")
	ack.Flags().StringVar(&ackDriverID, "driver-id", "", "Stable consumer identity override; inferred from PI_SESSION_ID inside Pi")
	ack.Flags().StringVar(&ackGeneration, "driver-generation", "", "Consumer session generation override")
	ack.Flags().StringVar(&handlingID, "handling-id", "", "Idempotency key override; generated when omitted")
	command.AddCommand(ack)

	var renewToken, renewDriverID, renewGeneration string
	var renewTTL time.Duration
	renew := &cobra.Command{
		Use:   "renew [notification-id]",
		Short: "Renew an active notification claim",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			notificationID := ""
			if len(args) == 1 {
				notificationID = args[0]
			}
			notification, err := resolveWakeClaim(get(), "renew", notificationID, renewToken, renewDriverID, renewGeneration, cmd.Flags().Changed("driver-id"), cmd.Flags().Changed("driver-generation"))
			if err != nil {
				return err
			}
			if renewTTL == 0 {
				renewTTL = get().config.Wake.ClaimTTL
			}
			if renewTTL < get().config.Wake.ClaimTTLMin || renewTTL > get().config.Wake.ClaimTTLMax {
				return fmt.Errorf("claim TTL must be between %s and %s", get().config.Wake.ClaimTTLMin, get().config.Wake.ClaimTTLMax)
			}
			request := domain.NotificationRenewRequest{NotificationID: notification.NotificationID, ClaimToken: notification.ClaimToken, ConsumerID: notification.ClaimOwner, DriverGeneration: notification.DriverGeneration}
			notice, err := get().store.RenewNotification(request, renewTTL)
			if err != nil {
				return wakeMutationFailure(get(), "renew", notification, err)
			}
			return get().print(notice, func() {
				until := ""
				if notice.ClaimUntil != nil {
					until = notice.ClaimUntil.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(get().out, "renewed %s until %s\n", notice.NotificationID, until)
			})
		},
	}
	renew.Flags().StringVar(&renewToken, "claim-token", "", "Opaque claim token returned by wake drain")
	renew.Flags().StringVar(&renewDriverID, "driver-id", "", "Stable consumer identity override; inferred from PI_SESSION_ID inside Pi")
	renew.Flags().StringVar(&renewGeneration, "driver-generation", "", "Consumer session generation override")
	renew.Flags().DurationVar(&renewTTL, "claim-ttl", 0, "Lease extension duration")
	command.AddCommand(renew)
	return command
}

func inferredPIDriverID() string {
	if f, err := control.SubdriverEnvironmentFence(); err == nil && f.ID != "" {
		return (domain.Subdriver{ID: f.ID}).DriverID()
	}
	if sessionID := strings.TrimSpace(os.Getenv("PI_SESSION_ID")); sessionID != "" {
		return "driver:pi:" + sessionID
	}
	return ""
}

func resolveWakeClaim(app *application, operation, notificationID, claimToken, explicitDriverID, explicitGeneration string, driverChanged, generationChanged bool) (domain.DriverNotification, error) {
	if strings.TrimSpace(claimToken) == "" {
		recovery := "shephrd wake drain --json"
		if notificationID != "" {
			if notification, err := app.store.Notification(notificationID); err == nil {
				recovery = wakeIdentityRecoveryCommand(operation, notification)
			}
		}
		return domain.DriverNotification{}, wakeClaimFailure("claim_identity_missing", recovery, "claim token is missing; --claim-token is required to identify the durable notification claim")
	}
	notification, err := app.store.NotificationByClaimToken(claimToken)
	if err != nil {
		recovery := "shephrd task obligations --all-drivers --json"
		if notificationID != "" {
			if stored, storedErr := app.store.Notification(notificationID); storedErr == nil {
				recovery = wakeIdentityRecoveryCommand(operation, stored)
				return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", recovery, "claim token conflicts with the stored claim for notification %s", stored.NotificationID)
			}
		}
		return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", recovery, "%v", err)
	}
	if notificationID != "" && notificationID != notification.NotificationID {
		return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", wakeIdentityRecoveryCommand(operation, notification), "notification ID %q conflicts with claim token bound to notification %s", notificationID, notification.NotificationID)
	}
	f, err := control.SubdriverEnvironmentFence()
	if err != nil {
		return domain.DriverNotification{}, err
	}
	if f.ID != "" && notification.DriverGeneration != fmt.Sprintf("coordinator:%d", f.Generation) {
		return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", "shephrd subdriver inspect "+f.ID+" --json", "notification belongs to a different sub-driver session generation")
	}
	inferredDriverID := inferredPIDriverID()
	if driverChanged && strings.TrimSpace(explicitDriverID) == "" {
		return domain.DriverNotification{}, wakeClaimFailure("claim_identity_missing", wakeIdentityRecoveryCommand(operation, notification), "driver identity is missing; --driver-id requires a non-empty value")
	}
	if driverChanged && inferredDriverID != "" && explicitDriverID != inferredDriverID {
		recovery := wakeClaimCommand(operation, notification)
		if notification.ClaimOwner != inferredDriverID {
			recovery = wakeAdoptionCommand(notification, inferredDriverID)
		}
		return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", recovery, "explicit driver ID %q conflicts with current inferred driver %q", explicitDriverID, inferredDriverID)
	}
	consumerID := explicitDriverID
	if consumerID == "" {
		consumerID = inferredDriverID
	}
	if consumerID == "" {
		consumerID = app.config.Wake.DriverID
	}
	if notification.ClaimOwner == "" || notification.TargetDriverID == "" || notification.ClaimOwner != notification.TargetDriverID {
		return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", "shephrd task inspect "+domain.ShellQuote(notification.TaskID)+" --json", "stored claim owner identity is missing or conflicts with notification routing for notification %s", notification.NotificationID)
	}
	if strings.TrimSpace(consumerID) == "" {
		return domain.DriverNotification{}, wakeClaimFailure("claim_identity_missing", wakeClaimCommand(operation, notification), "driver identity is missing; pass --driver-id or run inside the owning Pi session")
	}
	if consumerID != notification.ClaimOwner {
		recovery := wakeClaimCommand(operation, notification)
		if inferredDriverID != "" {
			recovery = wakeAdoptionCommand(notification, inferredDriverID)
		}
		return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", recovery, "driver ID %q conflicts with stored claim owner %q for notification %s", consumerID, notification.ClaimOwner, notification.NotificationID)
	}
	if generationChanged && strings.TrimSpace(explicitGeneration) == "" {
		return domain.DriverNotification{}, wakeClaimFailure("claim_identity_missing", wakeClaimCommand(operation, notification), "driver generation is missing; --driver-generation requires a non-empty value")
	}
	if generationChanged && explicitGeneration != notification.DriverGeneration {
		return domain.DriverNotification{}, wakeClaimFailure("claim_conflict", wakeClaimCommand(operation, notification), "driver generation %q conflicts with stored claim generation %q for notification %s", explicitGeneration, notification.DriverGeneration, notification.NotificationID)
	}
	return notification, nil
}

func wakeClaimCommand(operation string, notification domain.DriverNotification) string {
	return fmt.Sprintf("shephrd wake %s --claim-token %s --driver-id %s --json", operation, domain.ShellQuote(notification.ClaimToken), domain.ShellQuote(notification.ClaimOwner))
}

func wakeIdentityRecoveryCommand(operation string, notification domain.DriverNotification) string {
	if inferredDriverID := inferredPIDriverID(); inferredDriverID != "" && notification.TargetDriverID != inferredDriverID {
		return wakeAdoptionCommand(notification, inferredDriverID)
	}
	if notification.ClaimToken != "" {
		return wakeClaimCommand(operation, notification)
	}
	return wakeDrainCommand(notification)
}

func wakeAdoptionCommand(notification domain.DriverNotification, newDriverID string) string {
	if notification.RequestID != "" {
		return fmt.Sprintf("shephrd subdriver adopt-request %s --from-driver %s --driver-id %s --json && shephrd wake drain --driver-id %s --json", domain.ShellQuote(notification.RequestID), domain.ShellQuote(notification.TargetDriverID), domain.ShellQuote(newDriverID), domain.ShellQuote(newDriverID))
	}
	if owner, ok := strings.CutPrefix(notification.TargetDriverID, "coordinator:"); ok {
		return "shephrd subdriver inspect " + domain.ShellQuote(owner) + " --json"
	}
	return fmt.Sprintf("shephrd task adopt %s --driver-id %s --new-driver-id %s --json && shephrd wake drain %s --driver-id %s --json",
		domain.ShellQuote(notification.TaskID), domain.ShellQuote(notification.TargetDriverID), domain.ShellQuote(newDriverID), domain.ShellQuote(notification.TaskID), domain.ShellQuote(newDriverID))
}

func wakeDrainCommand(notification domain.DriverNotification) string {
	if notification.RequestID != "" {
		return fmt.Sprintf("shephrd wake drain --driver-id %s --json", domain.ShellQuote(notification.TargetDriverID))
	}
	if owner, ok := strings.CutPrefix(notification.TargetDriverID, "coordinator:"); ok {
		return "shephrd subdriver inspect " + domain.ShellQuote(owner) + " --json"
	}
	return fmt.Sprintf("shephrd wake drain %s --driver-id %s --json", domain.ShellQuote(notification.TaskID), domain.ShellQuote(notification.TargetDriverID))
}

func wakeClaimFailure(kind, recoveryCommand, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return domain.EvidenceFailure(kind, map[string]string{"recovery_command": recoveryCommand}, "%s; recover with `%s`", message, recoveryCommand)
}

func wakeMutationFailure(app *application, operation string, notification domain.DriverNotification, err error) error {
	kind := ErrorKind(err)
	if kind == "" {
		return err
	}
	if current, currentErr := app.store.Notification(notification.NotificationID); currentErr == nil {
		notification = current
	}
	recovery := "shephrd task inspect " + domain.ShellQuote(notification.TaskID) + " --json"
	switch {
	case operation == "ack" && notification.State == domain.NotificationAcknowledged:
		recovery = wakeClaimCommand(operation, notification)
	case notification.State == domain.NotificationClaimed && kind == "claim_expired":
		recovery = wakeDrainCommand(notification)
	case notification.State == domain.NotificationClaimed:
		recovery = wakeClaimCommand(operation, notification)
	case notification.State == domain.NotificationPending:
		recovery = wakeDrainCommand(notification)
	}
	return wakeClaimFailure(kind, recovery, "%v", err)
}

type workerStopResponse struct {
	SchemaVersion int    `json:"schema_version"`
	TaskID        string `json:"task_id"`
	Stopped       bool   `json:"stopped"`
	Discarded     bool   `json:"discarded"`
	NoWorkspace   bool   `json:"no_workspace"`
}

func stopCommand(get func() *application) *cobra.Command {
	var discard bool
	var reason string
	command := &cobra.Command{
		Use:   "stop <task-id>",
		Short: "Stop the current worker attempt",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			task, err := get().store.Task(args[0])
			if err != nil {
				return err
			}
			if err := get().control.Stop(args[0], reason, discard); err != nil {
				return err
			}
			noWorkspace := task.CurrentAttemptID == ""
			value := workerStopResponse{SchemaVersion: 1, TaskID: args[0], Stopped: true, Discarded: discard && !noWorkspace, NoWorkspace: noWorkspace}
			return get().print(value, func() {
				if noWorkspace {
					fmt.Fprintln(get().out, "worker stopped; no workspace existed")
					return
				}
				fmt.Fprintln(get().out, "worker stopped")
			})
		},
	}
	command.Flags().BoolVar(&discard, "discard", false, "Authorize discard and release the worktree")
	command.Flags().StringVar(&reason, "reason", "", "Stop reason")
	return command
}

func retryCommand(get func() *application) *cobra.Command {
	var harness, model, runtime string
	var baseBranch, baseTask, baseCommit string
	command := &cobra.Command{
		Use:   "retry <task-id>",
		Short: "Create a fresh attempt, lease, and worker session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			baseSelection, baseProvided, err := baseSelectionFromFlags(cmd, baseBranch, baseTask, baseCommit)
			if err != nil {
				return err
			}
			result, err := get().control.RetryWithBaseSelection(args[0], harness, model, cmd.Flags().Changed("model"), baseSelection, baseProvided, runtime)
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintf(get().out, "retry started as %s\n", result.Attempt.ID)
			})
		},
	}
	command.Flags().StringVar(&harness, "harness", "", "Override worker harness")
	command.Flags().StringVar(&model, "model", "", "Override with a harness-specific model identifier")
	command.Flags().StringVar(&runtime, "runtime", "", "Worker runtime: headless, auto, herdr, or cmux")
	command.Flags().StringVar(&baseBranch, "base-branch", "", "Explicit local branch to use as the worker base")
	command.Flags().StringVar(&baseTask, "base-task", "", "Explicit task whose current attempt branch is the worker base")
	command.Flags().StringVar(&baseCommit, "base-commit", "", "Explicit full immutable Git commit to use as the worker base")
	return command
}

type workspaceReleaseResponse struct {
	SchemaVersion int    `json:"schema_version"`
	TaskID        string `json:"task_id"`
	AttemptID     string `json:"attempt_id"`
	Released      bool   `json:"released"`
	Discarded     bool   `json:"discarded"`
}

func workspaceCommand(get func() *application) *cobra.Command {
	command := commandGroup("workspace", "Release workspaces and mutate recorded recovery state")
	var discard bool
	var attemptID string
	release := &cobra.Command{
		Use:   "release <task-id>",
		Short: "Release a landed or explicitly discarded workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			attempt, err := get().control.ResolveWorkspaceAttempt(args[0], attemptID)
			if err != nil {
				return err
			}
			if err := get().control.Release(args[0], attempt.ID, discard); err != nil {
				return err
			}
			value := workspaceReleaseResponse{SchemaVersion: 1, TaskID: args[0], AttemptID: attempt.ID, Released: true, Discarded: discard}
			return get().print(value, func() { fmt.Fprintf(get().out, "workspace for attempt %s released\n", attempt.ID) })
		},
	}
	release.Flags().BoolVar(&discard, "discard", false, "Authorize discarding unlanded work")
	release.Flags().StringVar(&attemptID, "attempt", "", "Attempt ID, including a superseded attempt")
	command.AddCommand(release)
	command.AddCommand(&cobra.Command{
		Use:   "reconcile",
		Short: "Mutate recovery state by reconciling dead workers and recorded workspaces",
		Long:  "Sweep dead runners, classify recorded workspace identity, and mutate recovery state, including completing an already-authorized interrupted release when exact evidence permits it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := get().control.Reconcile()
			if err != nil {
				return err
			}
			return get().print(result, func() {
				fmt.Fprintf(get().out, "recovery state mutated: %d verified, %d unknown, %d classifications; %d pending notifications\n", len(result.Verified), len(result.Unknown), len(result.Classifications), result.NotificationCounts.Pending)
			})
		},
	})
	return command
}

func runCommand(get func() *application) *cobra.Command {
	var input string
	var resume bool
	var runGeneration int
	command := &cobra.Command{
		Use:    "_run <attempt-id>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return fmt.Errorf("--input is required")
			}
			if !cmd.Flags().Changed("run-generation") || runGeneration < 1 {
				return fmt.Errorf("--run-generation must be at least 1")
			}
			return get().control.RunAttempt(args[0], input, resume, cmd.OutOrStdout(), runGeneration)
		},
	}
	command.Flags().StringVar(&input, "input", "", "Worker input file")
	command.Flags().BoolVar(&resume, "resume", false, "Resume the harness session")
	command.Flags().IntVar(&runGeneration, "run-generation", 0, "Fenced worker run generation")
	return command
}

func claudeHookCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "_claude-hook",
		Hidden:      true,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{standaloneAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return claudebridge.RunHook(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func commandGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
}

func printReportLifecycle(out io.Writer, invocations []domain.ReportLifecycleInvocation, prefix string, retry bool) {
	for _, invocation := range invocations {
		fmt.Fprintf(out, "%sreport.accepted handler %s: %s", prefix, invocation.HandlerName, invocation.State)
		if invocation.Annotation != "" {
			fmt.Fprintf(out, " - %s", invocation.Annotation)
		}
		if invocation.ReceiptID != "" {
			fmt.Fprintf(out, " receipt %s:%s", invocation.ReceiptSystem, invocation.ReceiptID)
		}
		if invocation.FailureMessage != "" {
			fmt.Fprintf(out, " - %s", invocation.FailureMessage)
		}
		fmt.Fprintln(out)
		if retry && invocation.RetryCommand != "" {
			fmt.Fprintln(out, invocation.RetryCommand)
		}
	}
}

func printCheckpointItems(out io.Writer, label string, values []string) {
	for _, value := range values {
		fmt.Fprintf(out, "  %s: %s\n", label, value)
	}
}

func isCompletionCommand(cmd *cobra.Command) bool {
	for current := cmd; current != nil; current = current.Parent() {
		if current.Name() == "completion" {
			return true
		}
	}
	return false
}

func (a *application) print(value any, text func()) error {
	if !a.json {
		text()
		return nil
	}
	if a.legacySubdriverJSON {
		legacy := func(n domain.DriverNotification) domain.DriverNotification {
			if n.SubdriverEventID != 0 {
				n.Kind = strings.Replace(n.Kind, "subdriver-", "coordinator-", 1)
			}
			return n
		}
		switch v := value.(type) {
		case domain.DriverNotification:
			value = legacy(v)
		case domain.NotificationDrain:
			v.Notifications = append([]domain.DriverNotification{}, v.Notifications...)
			for i := range v.Notifications {
				v.Notifications[i] = legacy(v.Notifications[i])
			}
			value = v
		}
	}
	encoder := json.NewEncoder(a.out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func resolveDriverID(explicit string, changed, required bool) (string, error) {
	f, err := control.SubdriverEnvironmentFence()
	if err != nil {
		return "", err
	}
	if f.ID != "" {
		owner := (domain.Subdriver{ID: f.ID}).DriverID()
		if changed && explicit != owner {
			return "", fmt.Errorf("sub-driver owner override conflicts")
		}
		return owner, nil
	}
	driverID := strings.TrimSpace(explicit)
	if changed && driverID == "" {
		return "", domain.Failure("driver_context_required", "--driver-id requires a non-empty driver identity")
	}
	if driverID == "" {
		if sessionID := strings.TrimSpace(os.Getenv("PI_SESSION_ID")); sessionID != "" {
			driverID = "driver:pi:" + sessionID
		}
	}
	if required && driverID == "" {
		return "", domain.Failure("driver_context_required", "--driver-id is required outside an active Pi session")
	}
	return driverID, nil
}

func ErrorKind(err error) string {
	type classified interface {
		ErrorKind() string
	}
	var typed classified
	if errors.As(err, &typed) {
		return typed.ErrorKind()
	}
	switch {
	case errors.Is(err, store.ErrNotificationConflict):
		return "claim_conflict"
	case errors.Is(err, store.ErrNotificationStale):
		return "claim_stale"
	case errors.Is(err, store.ErrNotificationExpired):
		return "claim_expired"
	default:
		return ""
	}
}

func ErrorEvidence(err error) map[string]string {
	type evidenced interface {
		ErrorEvidence() map[string]string
	}
	var typed evidenced
	if errors.As(err, &typed) {
		return typed.ErrorEvidence()
	}
	return nil
}

func IsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			return true
		}
	}
	return false
}
