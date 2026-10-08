package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"shephrd/internal/model"
)

func obligationsCommand(get func() *application) *cobra.Command {
	var driverID, repoRef string
	var allDrivers, portfolio, includeArchivedClosed, details bool
	var limit int
	command := &cobra.Command{
		Use:   "obligations",
		Short: "Show owner-scoped task obligations and ready planned work without mutation",
		Long: `Derive owner-scoped obligations from recorded task, attempt,
notification, and plan facts in one consistent SQLite read. The default
output shows only work requiring a driver decision plus manually ready planned
work; --portfolio adds underway, queued, planned-blocked, and recently closed
sections. JSON output is the stable integration contract and may itself be
sensitive operational data: piping it to a file changes its privacy boundary.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDriverID(driverID, cmd.Flags().Changed("driver-id"), !allDrivers)
			if err != nil {
				return fmt.Errorf("task obligations needs an owner scope: pass --driver-id or --all-drivers (%w)", err)
			}
			filter := model.AttentionFilter{DriverID: resolved, AllDrivers: allDrivers, Limit: limit,
				Portfolio: portfolio, IncludeArchivedClosed: includeArchivedClosed, Details: details}
			if repoRef != "" {
				repo, err := get().store.Repo(repoRef)
				if err != nil {
					return err
				}
				filter.RepoID = repo.ID
			}
			snapshot, err := get().store.AttentionSnapshot(filter)
			if err != nil {
				return err
			}
			return get().print(snapshot, func() { renderAttention(get().out, snapshot) })
		},
	}
	command.Flags().StringVar(&driverID, "driver-id", "", "Owner scope; inferred from PI_SESSION_ID inside Pi")
	command.Flags().BoolVar(&allDrivers, "all-drivers", false, "Disclose every owner's rows without transferring ownership")
	command.Flags().StringVar(&repoRef, "repo", "", "Exact registered repo filter")
	command.Flags().BoolVar(&portfolio, "portfolio", false, "Render the complete portfolio in mutually exclusive sections")
	command.Flags().IntVar(&limit, "limit", 0, fmt.Sprintf("Rows per section, default %d, maximum %d", model.AttentionDefaultLimit, model.AttentionMaxLimit))
	command.Flags().BoolVar(&details, "details", false, "Include bounded objective and failure-reason text")
	command.Flags().BoolVar(&includeArchivedClosed, "include-archived-closed", false, "Include archived closed rows in the portfolio closed section")
	return command
}

var attentionSectionTitles = map[string]string{
	model.BucketActNow:           "ACT NOW",
	model.BucketNeedsDisposition: "NEEDS DISPOSITION",
	model.BucketResultReady:      "RESULTS READY",
	model.BucketPlannedReady:     "PLANNED READY",
	model.BucketUnderway:         "UNDERWAY",
	model.BucketQueued:           "QUEUED",
	model.BucketPlannedBlocked:   "PLANNED BLOCKED",
	model.BucketClosed:           "RECENTLY CLOSED",
}

func renderAttention(out interface{ Write([]byte) (int, error) }, snapshot model.AttentionSnapshot) {
	scope := snapshot.Scope.DriverID
	if snapshot.Scope.AllDrivers {
		scope = "all drivers"
	}
	fmt.Fprintf(out, "Obligations for %s at %s\n", scope, snapshot.GeneratedAt.UTC().Format(time.RFC3339))
	counts := snapshot.Counts
	fmt.Fprintf(out, "%d act now | %d need disposition | %d ready results | %d planned ready | %d underway | %d queued | %d closed\n",
		counts.ActNow, counts.NeedsDisposition, counts.ResultReady, counts.PlannedReady, counts.Underway, counts.Queued, counts.Closed)
	if snapshot.Scope.AllDrivers && len(snapshot.Plans.Summaries) > 0 {
		fmt.Fprintln(out, "\nPLANS")
		printPlanSummaries(out, snapshot.Plans.Summaries)
	}
	previous := ""
	for _, item := range snapshot.Items {
		if item.Bucket != previous {
			fmt.Fprintf(out, "\n%s\n", attentionSectionTitles[item.Bucket])
			previous = item.Bucket
		}
		renderAttentionItem(out, item)
	}
	fmt.Fprintln(out, "\nQUIET")
	fmt.Fprintf(out, "%d healthy workers are underway.\n", counts.Underway)
	fmt.Fprintf(out, "%d tasks are safely closed; %d more are archived closed.\n", counts.Closed, counts.ArchivedClosed)
	fmt.Fprintf(out, "%d planned items are ready for manual dispatch.\n", counts.PlannedReady)
	if counts.OtherDriverAttention > 0 {
		fmt.Fprintf(out, "%d attention rows belong to other drivers; rerun with --all-drivers to inspect, and adopt explicitly if you take them over.\n", counts.OtherDriverAttention)
	}
	if snapshot.Plans.Status != "available" {
		fmt.Fprintf(out, "planned section unavailable: %s\n", snapshot.Plans.Diagnostic)
	}
	for _, omission := range snapshot.Omitted {
		fmt.Fprintf(out, "%d %s rows omitted; %s\n", omission.Count, omission.Section, omission.Reveal)
	}
}

func renderAttentionItem(out interface{ Write([]byte) (int, error) }, item model.AttentionItem) {
	label := ""
	switch {
	case item.Task != nil:
		label = item.Task.Label
	case item.Planned != nil:
		label = item.Planned.PlanName + "#" + fmt.Sprint(item.Planned.Position) + " " + item.Planned.Title
		if item.Planned.FeatureKey != "" {
			label += " [" + item.Planned.FeatureKey + "]"
		}
	}
	marker := ""
	if item.Visibility != "visible" {
		marker = " [" + item.Visibility + "]"
	}
	fmt.Fprintf(out, "P%d %s %s%s\n", item.Priority, item.AgeBucket, label, marker)
	detail := []string{item.Kind}
	if len(item.ReasonCodes) > 0 {
		detail = append(detail, strings.Join(item.ReasonCodes, ", "))
	}
	if item.Attempts != nil && item.Attempts.HeldCount > 0 {
		detail = append(detail, fmt.Sprintf("%d held attempt(s)", item.Attempts.HeldCount))
	}
	if item.Notifications != nil && item.Notifications.LatestState != "" {
		detail = append(detail, "latest notification "+item.Notifications.LatestKind+"/"+item.Notifications.LatestState)
	}
	fmt.Fprintf(out, "   %s\n", strings.Join(detail, "; "))
	if item.Objective != "" {
		fmt.Fprintf(out, "   objective: %s\n", item.Objective)
	}
	if item.FailureReason != "" {
		fmt.Fprintf(out, "   failure: %s\n", item.FailureReason)
	}
	if item.LatestAnnotation != "" {
		fmt.Fprintf(out, "   latest annotation: %s\n", item.LatestAnnotation)
	}
	if len(item.DriverChoices) > 0 {
		fmt.Fprintf(out, "   decide: %s\n", strings.Join(item.DriverChoices, ", "))
	}
	for _, command := range item.RecoveryCommands {
		fmt.Fprintf(out, "   recovery: %s\n", command)
	}
}
