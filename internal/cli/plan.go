package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"shephrd/internal/model"
)

func planCommand(get func() *application) *cobra.Command {
	var driverID string
	command := commandGroup("plan", "Manage durable driver-owned planned work")
	command.PersistentFlags().StringVar(&driverID, "driver-id", "", "Owning driver identity; inferred from PI_SESSION_ID inside Pi")
	resolveOwner := func(required bool) (string, error) {
		value := strings.TrimSpace(driverID)
		if command.PersistentFlags().Changed("driver-id") && value == "" {
			return "", fmt.Errorf("--driver-id requires a non-empty current owner identity")
		}
		if value == "" {
			if sessionID := strings.TrimSpace(os.Getenv("PI_SESSION_ID")); sessionID != "" {
				value = "driver:pi:" + sessionID
			} else if get() != nil {
				value = strings.TrimSpace(get().config.Wake.DriverID)
			}
		}
		if required && value == "" {
			return "", fmt.Errorf("--driver-id is required outside an active Pi session")
		}
		return value, nil
	}
	owner := func() (string, error) { return resolveOwner(true) }
	resolve := func(ref string) (model.Plan, error) {
		value, err := owner()
		if err != nil {
			return model.Plan{}, err
		}
		return get().control.Plan(ref, value)
	}

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a named durable plan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := owner()
			if err != nil {
				return err
			}
			plan, err := get().store.CreatePlan(args[0], value)
			if err != nil {
				return err
			}
			return get().print(plan, func() { fmt.Fprintf(get().out, "created plan %s (%s)\n", plan.ID, plan.Name) })
		},
	}
	command.AddCommand(create)

	var allDrivers bool
	listCommand := &cobra.Command{
		Use:   "ls",
		Short: "List owned plans or disclose plans across drivers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := resolveOwner(!allDrivers)
			if err != nil {
				return err
			}
			if allDrivers {
				summaries, err := get().store.PlanSummaries(value, true)
				if err != nil {
					return err
				}
				return get().print(summaries, func() { printPlanSummaries(get().out, summaries) })
			}
			plans, err := get().store.Plans(value)
			if err != nil {
				return err
			}
			return get().print(plans, func() {
				for _, plan := range plans {
					fmt.Fprintf(get().out, "%s\t%s\t%s\n", plan.ID, plan.Name, plan.DriverID)
				}
			})
		},
	}
	listCommand.Flags().BoolVar(&allDrivers, "all-drivers", false, "Disclose every owner's plans without adopting or mutating them")
	command.AddCommand(listCommand)

	show := &cobra.Command{
		Use:   "show <plan>",
		Short: "Show ordered planned work, evidence, annotations, and readiness",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			if err := get().store.AttachLatestPlanAnnotations(&plan); err != nil {
				return err
			}
			return get().print(plan, func() { printPlan(get().out, plan) })
		},
	}
	command.AddCommand(show)

	var newDriverID string
	adopt := &cobra.Command{
		Use:   "adopt <plan-id>",
		Short: "Transfer a plan to another driver",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			currentDriverID, err := owner()
			if err != nil {
				return err
			}
			if strings.TrimSpace(newDriverID) == "" {
				return fmt.Errorf("--new-driver-id is required")
			}
			plan, err := get().store.AdoptPlan(args[0], currentDriverID, newDriverID)
			if err != nil {
				return err
			}
			return get().print(plan, func() { fmt.Fprintf(get().out, "adopted plan %s as %s\n", plan.ID, plan.DriverID) })
		},
	}
	adopt.Flags().StringVar(&newDriverID, "new-driver-id", "", "New owning driver identity")
	command.AddCommand(adopt)

	var addTitle, addFeature, addDescription, addAcceptance, addRepo, addDeliverable string
	var addPosition int
	var addRequires, addReports []string
	addItem := &cobra.Command{
		Use:   "add <plan> <objective>",
		Short: "Add an ordered planned item, with its prerequisite and report relations",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			repoID := ""
			if strings.TrimSpace(addRepo) != "" {
				repo, err := get().registry.Resolve(addRepo)
				if err != nil {
					return err
				}
				repoID = repo.ID
			}
			for _, prerequisite := range addReports {
				if !containsString(addRequires, prerequisite) {
					return fmt.Errorf("--report %s also requires the prerequisite relation; pass --requires %s as well", prerequisite, prerequisite)
				}
			}
			item, err := get().store.AddPlanItem(plan.ID, model.PlanItem{Title: model.DeriveTitle(addTitle, args[1], addFeature), FeatureKey: addFeature, Objective: args[1], Description: addDescription,
				AcceptanceCriteria: addAcceptance, RepoID: repoID, Deliverable: addDeliverable}, model.PlanItemRelations{Position: addPosition, Requires: addRequires, Reports: addReports})
			if err != nil {
				return err
			}
			return get().print(item, func() {
				fmt.Fprintf(get().out, "added item %s: %s at position %d\n", item.ID, item.Title, item.Position)
			})
		},
	}
	addItem.Flags().StringVar(&addTitle, "title", "", "Human-readable item title; defaults to a concise title derived from the objective")
	addItem.Flags().StringVar(&addFeature, "feature", "", "Stable feature key required before dispatch")
	addItem.Flags().StringVar(&addDescription, "description", "", "Detailed planned work description")
	addItem.Flags().StringVar(&addAcceptance, "acceptance", "", "Goals or acceptance criteria required before dispatch")
	addItem.Flags().StringVar(&addRepo, "repo", "", "Intended registered repository")
	addItem.Flags().StringVar(&addDeliverable, "deliverable", "code", "Artifact contract: code requires branch/PR; report requires the exact report path")
	addItem.Flags().IntVar(&addPosition, "position", 0, "Ordered position; defaults to append")
	addItem.Flags().StringArrayVar(&addRequires, "requires", nil, "Prerequisite item ID in the same plan; repeat for multiple prerequisites")
	addItem.Flags().StringArrayVar(&addReports, "report", nil, "Prerequisite item ID whose verified report this item consumes; repeat for multiple reports")
	command.AddCommand(addItem)

	var editTitle, editFeature, editObjective, editDescription, editAcceptance, editRepo, editDeliverable string
	var editPosition int
	editItem := &cobra.Command{
		Use:   "edit <plan> <item-id>",
		Short: "Edit planned scope or reorder an item before dispatch",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			update := model.PlanItemUpdate{}
			changed := false
			if cmd.Flags().Changed("title") {
				update.Title, changed = &editTitle, true
			}
			if cmd.Flags().Changed("feature") {
				update.FeatureKey, changed = &editFeature, true
			}
			if cmd.Flags().Changed("objective") {
				update.Objective, changed = &editObjective, true
			}
			if cmd.Flags().Changed("description") {
				update.Description, changed = &editDescription, true
			}
			if cmd.Flags().Changed("acceptance") {
				update.AcceptanceCriteria, changed = &editAcceptance, true
			}
			if cmd.Flags().Changed("deliverable") {
				update.Deliverable, changed = &editDeliverable, true
			}
			if cmd.Flags().Changed("position") {
				update.Position, changed = &editPosition, true
			}
			if cmd.Flags().Changed("repo") {
				repoID := ""
				if strings.TrimSpace(editRepo) != "" {
					repo, err := get().registry.Resolve(editRepo)
					if err != nil {
						return err
					}
					repoID = repo.ID
				}
				update.RepoID, changed = &repoID, true
			}
			if !changed {
				return fmt.Errorf("item edit requires at least one changed field")
			}
			item, err := get().store.UpdatePlanItem(plan.ID, args[1], update)
			if err != nil {
				return err
			}
			return get().print(item, func() { fmt.Fprintf(get().out, "updated item %s\n", item.ID) })
		},
	}
	editItem.Flags().StringVar(&editTitle, "title", "", "Human-readable item title")
	editItem.Flags().StringVar(&editFeature, "feature", "", "Stable feature key; empty clears")
	editItem.Flags().StringVar(&editObjective, "objective", "", "Planned objective")
	editItem.Flags().StringVar(&editDescription, "description", "", "Detailed description; empty clears")
	editItem.Flags().StringVar(&editAcceptance, "acceptance", "", "Goals or acceptance criteria; empty clears")
	editItem.Flags().StringVar(&editRepo, "repo", "", "Registered repository; empty clears")
	editItem.Flags().StringVar(&editDeliverable, "deliverable", "", "Artifact contract: code requires branch/PR; report requires the exact report path")
	editItem.Flags().IntVar(&editPosition, "position", 0, "New ordered position")
	command.AddCommand(editItem)

	removeItem := &cobra.Command{
		Use:   "rm <plan> <item-id>",
		Short: "Remove an undispatched planned item",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			if err := get().store.DeletePlanItem(plan.ID, args[1]); err != nil {
				return err
			}
			return get().print(map[string]any{"plan_id": plan.ID, "item_id": args[1], "removed": true}, func() { fmt.Fprintln(get().out, "item removed") })
		},
	}
	command.AddCommand(removeItem)

	prerequisiteCommand := commandGroup("requires", "Manage explicit prerequisite relations")
	addPrerequisite := &cobra.Command{
		Use:   "add <plan> <item-id> <prerequisite-item-id>",
		Short: "Add a prerequisite to an existing item",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			if err := get().store.AddPlanPrerequisite(plan.ID, args[1], args[2]); err != nil {
				return err
			}
			return get().print(map[string]any{"plan_id": plan.ID, "item_id": args[1], "prerequisite_item_id": args[2]}, func() { fmt.Fprintln(get().out, "prerequisite added") })
		},
	}
	removePrerequisite := &cobra.Command{
		Use:   "rm <plan> <item-id> <prerequisite-item-id>",
		Short: "Remove a prerequisite before dispatch",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			if err := get().store.RemovePlanPrerequisite(plan.ID, args[1], args[2]); err != nil {
				return err
			}
			return get().print(map[string]any{"plan_id": plan.ID, "item_id": args[1], "prerequisite_item_id": args[2], "removed": true}, func() { fmt.Fprintln(get().out, "prerequisite removed") })
		},
	}
	prerequisiteCommand.AddCommand(addPrerequisite, removePrerequisite)
	command.AddCommand(prerequisiteCommand)

	reportCommand := commandGroup("report", "Add, select, reorder, and remove explicit report inputs")
	addReport := &cobra.Command{
		Use:   "add <plan> <item-id> <prerequisite-item-id>",
		Short: "Add an ordered report-input relation for a prerequisite",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			report, err := get().store.AddPlanReport(plan.ID, args[1], args[2])
			if err != nil {
				return err
			}
			return get().print(report, func() {
				fmt.Fprintf(get().out, "report input added at position %d; select its verified report when eligible\n", report.Position)
			})
		},
	}
	selectReport := &cobra.Command{
		Use:   "select <plan> <item-id> <prerequisite-item-id>",
		Short: "Pin the prerequisite's current immutable verified report",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := owner()
			if err != nil {
				return err
			}
			report, err := get().control.SelectPlanReport(args[0], args[1], args[2], value)
			if err != nil {
				return err
			}
			return get().print(report, func() {
				fmt.Fprintf(get().out, "selected artifact %s at input position %d\n", report.ArtifactID, report.Position)
			})
		},
	}
	var reportPosition int
	moveReport := &cobra.Command{
		Use:   "move <plan> <item-id> <prerequisite-item-id>",
		Short: "Reorder selected report inputs",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if reportPosition < 1 {
				return fmt.Errorf("--position must be at least 1")
			}
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			if err := get().store.MovePlanReport(plan.ID, args[1], args[2], reportPosition); err != nil {
				return err
			}
			return get().print(map[string]any{"plan_id": plan.ID, "item_id": args[1], "prerequisite_item_id": args[2], "position": reportPosition}, func() { fmt.Fprintln(get().out, "report input moved") })
		},
	}
	moveReport.Flags().IntVar(&reportPosition, "position", 0, "New input position")
	removeReport := &cobra.Command{
		Use:   "rm <plan> <item-id> <prerequisite-item-id>",
		Short: "Remove a report-input relation before dispatch",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := resolve(args[0])
			if err != nil {
				return err
			}
			if err := get().store.RemovePlanReport(plan.ID, args[1], args[2]); err != nil {
				return err
			}
			return get().print(map[string]any{"plan_id": plan.ID, "item_id": args[1], "prerequisite_item_id": args[2], "removed": true}, func() { fmt.Fprintln(get().out, "report input removed") })
		},
	}
	reportCommand.AddCommand(addReport, selectReport, moveReport, removeReport)
	command.AddCommand(reportCommand)

	var spawn bool
	var spawnHarness, spawnModel, spawnRuntime string
	dispatch := &cobra.Command{
		Use:   "dispatch <plan> <item-id>",
		Short: "Create one ordinary queued task and optionally spawn it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spawnInputs := cmd.Flags().Changed("harness") || cmd.Flags().Changed("model") || cmd.Flags().Changed("runtime")
			if !spawn && spawnInputs {
				return fmt.Errorf("--harness, --model, and --runtime require --spawn")
			}
			for _, input := range []struct{ name, value string }{{"harness", spawnHarness}, {"model", spawnModel}, {"runtime", spawnRuntime}} {
				if cmd.Flags().Changed(input.name) && strings.TrimSpace(input.value) == "" {
					return fmt.Errorf("--%s requires a non-empty value", input.name)
				}
			}
			if spawn && cmd.Flags().Changed("model") && !cmd.Flags().Changed("harness") {
				return fmt.Errorf("--model requires --harness when used with --spawn")
			}
			value, err := owner()
			if err != nil {
				return err
			}
			result, err := get().control.DispatchPlanItem(args[0], args[1], value)
			if err != nil {
				return err
			}
			if spawn {
				spawned, spawnErr := get().control.SpawnWithModelSelection(result.Task.ID, spawnHarness, spawnModel, cmd.Flags().Changed("model"), spawnRuntime)
				if spawnErr != nil {
					current, inspectErr := get().store.Task(result.Task.ID)
					if inspectErr == nil && current.Status == model.TaskStatusQueued && current.CurrentAttemptID == "" {
						return fmt.Errorf("item %s was dispatched as queued task %s, but spawn failed: %w; retry with 'shephrd worker spawn %s'", result.Item.ID, result.Task.ID, spawnErr, result.Task.ID)
					}
					return fmt.Errorf("item %s was dispatched as task %s and its separate spawn transition failed: %w; inspect with 'shephrd task inspect %s' before recovery", result.Item.ID, result.Task.ID, spawnErr, result.Task.ID)
				}
				result.Spawn = &spawned
			}
			return get().print(result, func() {
				if result.Spawn == nil {
					fmt.Fprintf(get().out, "dispatched item %s as queued task %s; no worker was started\n", result.Item.ID, result.Task.ID)
					return
				}
				fmt.Fprintf(get().out, "dispatched item %s as task %s, then spawned attempt %s (%s)\n", result.Item.ID, result.Task.ID, result.Spawn.Attempt.ID, result.Spawn.Label)
			})
		},
	}
	dispatch.Flags().BoolVar(&spawn, "spawn", false, "Spawn the newly dispatched queued task as a separate durable transition")
	dispatch.Flags().StringVar(&spawnHarness, "harness", "", "Worker harness used only with --spawn: claude-code, pi, or codex")
	dispatch.Flags().StringVar(&spawnModel, "model", "", "Harness-specific model identifier used only with --spawn and --harness")
	dispatch.Flags().StringVar(&spawnRuntime, "runtime", "", "Worker runtime used only with --spawn: headless, auto, herdr, or cmux")
	command.AddCommand(dispatch)

	resolveItemScope := func(args []string) (model.AnnotationScope, error) {
		plan, err := resolve(args[0])
		if err != nil {
			return model.AnnotationScope{}, err
		}
		for _, item := range plan.Items {
			if item.ID == args[1] {
				return model.AnnotationScope{PlanID: plan.ID, PlanItemID: item.ID}, nil
			}
		}
		return model.AnnotationScope{}, fmt.Errorf("plan item %q does not exist in plan %s", args[1], plan.ID)
	}

	resolveItemHistoryScope := func(args []string) (model.AnnotationScope, error) {
		plan, err := resolve(args[0])
		if err != nil {
			return model.AnnotationScope{}, err
		}
		return model.AnnotationScope{PlanID: plan.ID, PlanItemID: args[1]}, nil
	}

	var annotateReason, annotateNextAction string
	var annotateRevision int
	annotate := &cobra.Command{
		Use:   "annotate <plan> <item-id> <judgment>",
		Short: "Append one revision-fenced recovery annotation to a plan item",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			currentOwner, err := owner()
			if err != nil {
				return err
			}
			scope, err := resolveItemScope(args)
			if err != nil {
				return err
			}
			return addAnnotation(get(), scope, currentOwner, annotateRevision, model.AnnotationInput{Judgment: args[2], Reason: annotateReason, NextAction: annotateNextAction})
		},
	}
	annotate.Flags().StringVar(&annotateReason, "reason", "", "Bounded reason for the annotation")
	annotate.Flags().StringVar(&annotateNextAction, "next-action", "", "Bounded next explicit driver action")
	annotate.Flags().IntVar(&annotateRevision, "expect-revision", 0, "Expected prior revision, using 0 for the first annotation")
	command.AddCommand(annotate)

	annotations := &cobra.Command{
		Use:   "annotations <plan> <item-id>",
		Short: "List a live or removed plan item annotation scope in revision order",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := owner(); err != nil {
				return err
			}
			scope, err := resolveItemHistoryScope(args)
			if err != nil {
				return err
			}
			return listAnnotations(get(), scope)
		},
	}
	command.AddCommand(annotations)
	return command
}

func addAnnotation(app *application, scope model.AnnotationScope, currentOwner string, expectedRevision int, input model.AnnotationInput) error {
	record, err := app.store.AddAnnotation(scope, currentOwner, expectedRevision, input)
	if err != nil {
		return err
	}
	return app.print(record, func() {
		fmt.Fprintf(app.out, "recorded annotation %s revision %d\n", record.ID, record.Revision)
	})
}

func listAnnotations(app *application, scope model.AnnotationScope) error {
	records, err := app.store.Annotations(scope)
	if err != nil {
		return err
	}
	return app.print(records, func() {
		for _, record := range records {
			fmt.Fprintf(app.out, "%d\t%s\t%s\t%s\n", record.Revision, record.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), record.DriverID, record.Judgment)
		}
	})
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func printPlanSummaries(out io.Writer, summaries []model.PlanSummary) {
	for _, summary := range summaries {
		fmt.Fprintf(out, "%s\t%s\towner=%s\t[%s]\tdispatched=%d live=%d landed=%d undispatched=%d\n", summary.ID, summary.Name,
			summary.DriverID, summary.Ownership, summary.DispatchedTaskCount, summary.LiveTaskCount, summary.LandedTaskCount, summary.UndispatchedItemCount)
		for _, task := range summary.DispatchedTasks {
			fmt.Fprintf(out, "  task=%s title=%s status=%s landed=%t attempt=%s\n", task.ID, task.Title, task.Status, task.Landed, task.CurrentAttemptID)
		}
	}
}

func printPlan(out io.Writer, plan model.Plan) {
	fmt.Fprintf(out, "%s %s owner=%s\n", plan.ID, plan.Name, plan.DriverID)
	for _, item := range plan.Items {
		state := "not ready"
		if item.Readiness.Ready {
			state = "ready for manual dispatch"
		}
		fmt.Fprintf(out, "%d. %s %s (feature %s) [%s]\n", item.Position, item.ID, item.Title, item.FeatureKey, state)
		fmt.Fprintf(out, "   objective: %s\n", item.Objective)
		for _, reason := range item.Readiness.Reasons {
			fmt.Fprintf(out, "   reason %s: %s\n", reason.Code, reason.Message)
		}
		for _, evidence := range item.Readiness.AvailableEvidence {
			fmt.Fprintf(out, "   evidence task=%s status=%s landed=%t artifact=%s\n", evidence.TaskID, evidence.Status, evidence.Landed, evidence.ArtifactRef)
		}
		if item.LatestAnnotation != nil {
			fmt.Fprintf(out, "   latest annotation r%d by %s: %s; next action: %s\n", item.LatestAnnotation.Revision, item.LatestAnnotation.DriverID, item.LatestAnnotation.Judgment, item.LatestAnnotation.NextAction)
		}
	}
}
