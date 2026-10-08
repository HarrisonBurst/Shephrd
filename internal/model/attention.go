package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const AttentionSchemaVersion = 2

const (
	AttentionDefaultLimit = 50
	AttentionMaxLimit     = 200
)

// staleReleasingAfter is how old a releasing claim must be before the
// projection reports it as a safety-sensitive stuck release instead of an
// in-progress one. Purely time-based so classification stays deterministic.
const staleReleasingAfter = 15 * time.Minute

const (
	BucketActNow           = "act_now"
	BucketNeedsDisposition = "needs_disposition"
	BucketResultReady      = "result_ready"
	BucketPlannedReady     = "planned_ready"
	BucketUnderway         = "underway"
	BucketQueued           = "queued"
	BucketPlannedBlocked   = "planned_blocked"
	BucketClosed           = "closed"
)

type AttentionFilter struct {
	DriverID              string
	AllDrivers            bool
	RepoID                string
	Limit                 int
	Portfolio             bool
	IncludeArchivedClosed bool
	Details               bool
}

type AttentionScope struct {
	DriverID   string `json:"driver_id"`
	AllDrivers bool   `json:"all_drivers"`
	RepoID     string `json:"repo_id"`
}

type AttentionCounts struct {
	ActNow               int `json:"act_now"`
	NeedsDisposition     int `json:"needs_disposition"`
	ResultReady          int `json:"result_ready"`
	PlannedReady         int `json:"planned_ready"`
	Underway             int `json:"underway"`
	Queued               int `json:"queued"`
	PlannedBlocked       int `json:"planned_blocked"`
	Closed               int `json:"closed"`
	ArchivedClosed       int `json:"archived_closed"`
	OtherDriverAttention int `json:"other_driver_attention"`
}

type AttentionTaskSummary struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	FeatureKey           string `json:"feature_key"`
	Label                string `json:"label"`
	RepoID               string `json:"repo_id"`
	RepoName             string `json:"repo_name"`
	DriverID             string `json:"driver_id"`
	Status               string `json:"status"`
	Deliverable          string `json:"deliverable"`
	ClaimedDone          bool   `json:"claimed_done"`
	CompletionProvenance string `json:"completion_provenance,omitempty"`
	Landed               bool   `json:"landed"`
	DiscardAuthorized    bool   `json:"discard_authorized"`
	Archived             bool   `json:"archived"`
}

type AttentionAttemptSummary struct {
	CurrentAttemptID   string `json:"current_attempt_id"`
	HeldCount          int    `json:"held_count"`
	AllocatingCount    int    `json:"allocating_count"`
	ReleasingCount     int    `json:"releasing_count"`
	ReleasedCount      int    `json:"released_count"`
	NoWorkspaceCount   int    `json:"no_workspace_count"`
	LandingProvenCount int    `json:"landing_proven_count"`
}

type AttentionNotificationSummary struct {
	ActiveCount int    `json:"active_count"`
	LatestKind  string `json:"latest_kind,omitempty"`
	LatestState string `json:"latest_state,omitempty"`
}

type AttentionPlanItem struct {
	PlanID      string    `json:"plan_id"`
	PlanName    string    `json:"plan_name"`
	ItemID      string    `json:"item_id"`
	Position    int       `json:"position"`
	Title       string    `json:"title"`
	FeatureKey  string    `json:"feature_key,omitempty"`
	Objective   string    `json:"objective"`
	RepoName    string    `json:"repo_name,omitempty"`
	DriverID    string    `json:"driver_id"`
	Ready       bool      `json:"ready"`
	ReasonCodes []string  `json:"reason_codes"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AttentionItem struct {
	AttentionKey         string                        `json:"attention_key"`
	Priority             int                           `json:"priority"`
	Bucket               string                        `json:"bucket"`
	Kind                 string                        `json:"kind"`
	AttentionSince       time.Time                     `json:"attention_since"`
	AttentionSinceSource string                        `json:"attention_since_source"`
	AgeSeconds           int64                         `json:"age_seconds"`
	AgeBucket            string                        `json:"age_bucket"`
	Visibility           string                        `json:"visibility"`
	Task                 *AttentionTaskSummary         `json:"task,omitempty"`
	Attempts             *AttentionAttemptSummary      `json:"attempts,omitempty"`
	Notifications        *AttentionNotificationSummary `json:"notifications,omitempty"`
	Planned              *AttentionPlanItem            `json:"planned,omitempty"`
	ReasonCodes          []string                      `json:"reason_codes"`
	DriverChoices        []string                      `json:"driver_choices"`
	RecoveryCommands     []string                      `json:"recovery_commands"`
	Objective            string                        `json:"objective,omitempty"`
	FailureReason        string                        `json:"failure_reason,omitempty"`
	LatestAnnotation     string                        `json:"latest_annotation,omitempty"`
}

type AttentionPlans struct {
	Status       string        `json:"status"`
	Diagnostic   string        `json:"diagnostic,omitempty"`
	ReadyCount   int           `json:"ready_count"`
	BlockedCount int           `json:"blocked_count"`
	Summaries    []PlanSummary `json:"summaries"`
}

type AttentionOmission struct {
	Section string `json:"section"`
	Count   int    `json:"count"`
	Reveal  string `json:"reveal"`
}

type AttentionSnapshot struct {
	SchemaVersion         int                 `json:"schema_version"`
	GeneratedAt           time.Time           `json:"generated_at"`
	DatabaseSchemaVersion int                 `json:"database_schema_version"`
	Scope                 AttentionScope      `json:"scope"`
	Counts                AttentionCounts     `json:"counts"`
	Items                 []AttentionItem     `json:"items"`
	Plans                 AttentionPlans      `json:"plans"`
	Omitted               []AttentionOmission `json:"omitted"`
}

// AttentionTaskFacts is the complete recorded input the pure classifier reads
// for one task. It carries no payload bodies, claim tokens, or paths.
type AttentionTaskFacts struct {
	Task                    Task
	Attempts                []Attempt
	ActiveNotifications     int
	LatestNotificationKind  string
	LatestNotificationState string
	// LatestQuestionAt is the created time of the newest accepted question
	// wake message; LatestTerminalAt of the newest accepted done, blocked, or
	// failed wake message.
	LatestQuestionAt                 *time.Time
	LatestTerminalAt                 *time.Time
	LatestAnnotation                 string
	LatestCheckpoint                 *AttemptCheckpoint
	CurrentAcceptedWorkerTerminals   int
	CurrentReportRecoveryAttestation bool
	Now                              time.Time
}

func ProjectAttention(facts []AttentionTaskFacts, plans AttentionPlans, planned []AttentionPlanItem, filter AttentionFilter, now time.Time) (AttentionCounts, AttentionPlans, []AttentionItem, []AttentionOmission) {
	classified := make([]AttentionItem, 0, len(facts))
	counts := AttentionCounts{}
	for _, fact := range facts {
		item := ClassifyAttention(fact)
		if filter.Details {
			item.Objective = boundText(fact.Task.Objective, 200)
			item.FailureReason = boundText(currentFailureReason(fact), 200)
		}
		classified = append(classified, item)
	}
	if !filter.AllDrivers {
		for _, item := range classified {
			if item.Task != nil && item.Task.DriverID != filter.DriverID && attentionActionable(item.Bucket) {
				counts.OtherDriverAttention++
			}
		}
		scoped := classified[:0]
		for _, item := range classified {
			if item.Task != nil && item.Task.DriverID == filter.DriverID {
				scoped = append(scoped, item)
			}
		}
		classified = scoped
	}
	for _, planned := range planned {
		if planned.Ready {
			plans.ReadyCount++
			classified = append(classified, plannedAttentionItem(planned, now))
		} else {
			plans.BlockedCount++
			classified = append(classified, plannedBlockedAttentionItem(planned, now))
		}
	}
	counts = countAttention(classified, counts)
	items, omitted := sectionAttention(classified, filter)
	return counts, plans, items, omitted
}

func attentionActionable(bucket string) bool {
	return bucket == BucketActNow || bucket == BucketNeedsDisposition || bucket == BucketResultReady
}

func currentFailureReason(fact AttentionTaskFacts) string {
	for _, attempt := range fact.Attempts {
		if attempt.ID == fact.Task.CurrentAttemptID {
			return attempt.FailureReason
		}
	}
	return ""
}

func boundText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// ClassifyAttention is the pure, deterministic projection from recorded task
// facts to one attention row. It performs no I/O and never suggests an
// automatic action; every driver choice is an explicit command family.
func ClassifyAttention(fact AttentionTaskFacts) AttentionItem {
	task := fact.Task
	aggregate := AttentionAttemptSummary{CurrentAttemptID: task.CurrentAttemptID}
	var heldSuperseded, unknownLease, staleReleasing, staleAllocating, unrecognized, recoveredLanding, liveRecordedRunner, incompleteProof bool
	var unrecognizedRaw []string
	for _, attempt := range fact.Attempts {
		switch attempt.ReleaseState {
		case "held":
			if attempt.ReleasedAt == nil {
				aggregate.HeldCount++
				if attempt.ID != task.CurrentAttemptID {
					heldSuperseded = true
				}
			}
		case "releasing":
			aggregate.ReleasingCount++
			if attempt.ReleaseClaimedAt != nil && fact.Now.Sub(*attempt.ReleaseClaimedAt) >= staleReleasingAfter {
				staleReleasing = true
			}
		case "released":
			aggregate.ReleasedCount++
		case "no_workspace":
			aggregate.NoWorkspaceCount++
		default:
			unrecognized = true
			unrecognizedRaw = append(unrecognizedRaw, "release_state:"+attempt.ReleaseState)
		}
		if !ValidAttemptStatus(attempt.Status) {
			unrecognized = true
			unrecognizedRaw = append(unrecognizedRaw, "attempt_status:"+attempt.Status)
		} else if attempt.Status == AttemptStatusWorkspaceUnknown {
			unknownLease = true
		}
		if !ValidWorkspaceState(attempt.WorkspaceState) {
			unrecognized = true
			unrecognizedRaw = append(unrecognizedRaw, "workspace_state:"+attempt.WorkspaceState)
		} else {
			switch attempt.WorkspaceState {
			case WorkspaceStateAllocating:
				aggregate.AllocatingCount++
				// A live spawn holds allocating only briefly; an old allocating
				// row is an interrupted allocation that reconcile must classify.
				if attempt.WorkspaceStateChangedAt == nil || fact.Now.Sub(*attempt.WorkspaceStateChangedAt) >= staleReleasingAfter {
					staleAllocating = true
				}
			case WorkspaceStateUnknown:
				unknownLease = true
			}
		}
		if attempt.LandedProven {
			// Mirror the schema invariant: landed_proven without a complete
			// proof identity is never trusted as a validated landing.
			if attempt.LandingKind == "" || attempt.LandedVerifiedAt == nil {
				incompleteProof = true
			} else {
				aggregate.LandingProvenCount++
				if attempt.LandingKind == LandingKindLocalAttestedAncestry {
					recoveredLanding = true
				}
			}
		}
		if attempt.RunnerPID > 0 && attempt.ReleasedAt == nil && IsTerminalTaskStatus(task.Status) {
			liveRecordedRunner = true
		}
	}
	if !ValidTaskStatus(task.Status) {
		unrecognized = true
		unrecognizedRaw = append(unrecognizedRaw, "task_status:"+task.Status)
	}
	discardAuthorized := task.DiscardAuthorized
	for _, attempt := range fact.Attempts {
		if attempt.DiscardAuthorized {
			discardAuthorized = true
		}
	}

	item := AttentionItem{Visibility: "visible"}
	item.Task = &AttentionTaskSummary{ID: task.ID, Title: task.Title, FeatureKey: task.FeatureKey,
		Label: task.RepoName + "/" + task.Title + " [" + task.FeatureKey + "]", RepoID: task.RepoID,
		RepoName: task.RepoName, DriverID: task.DriverID, Status: task.Status, Deliverable: task.Deliverable,
		ClaimedDone: task.ClaimedDone, CompletionProvenance: task.CompletionProvenance, Landed: task.Landed, DiscardAuthorized: discardAuthorized, Archived: task.ArchivedAt != nil}
	item.Attempts = &aggregate
	item.Notifications = &AttentionNotificationSummary{ActiveCount: fact.ActiveNotifications,
		LatestKind: fact.LatestNotificationKind, LatestState: fact.LatestNotificationState}
	reasons := make([]string, 0, 4)
	addReason := func(code string) {
		for _, existing := range reasons {
			if existing == code {
				return
			}
		}
		reasons = append(reasons, code)
	}

	closed, closedKind := attentionClosed(task, aggregate, fact.ActiveNotifications, unknownLease, unrecognized)

	anchor := task.UpdatedAt
	anchorSource := "task_updated_at"
	terminalAnchor := func() {
		if fact.LatestTerminalAt != nil {
			anchor, anchorSource = *fact.LatestTerminalAt, "accepted_terminal_message"
			return
		}
		for _, attempt := range fact.Attempts {
			if attempt.ID == task.CurrentAttemptID && attempt.EndedAt != nil {
				anchor, anchorSource = *attempt.EndedAt, "attempt_ended_at"
				return
			}
		}
	}

	switch {
	case unrecognized:
		item.Kind, item.Bucket, item.Priority = "state_unrecognized", BucketActNow, 400
		for _, raw := range unrecognizedRaw {
			addReason("unrecognized_" + raw)
		}
		item.DriverChoices = []string{"task_show"}
	case unknownLease:
		item.Kind, item.Bucket, item.Priority = "state_inconsistent", BucketActNow, 400
		addReason("unknown_lease_identity")
		item.DriverChoices = []string{"worker_status", "reconcile"}
	case staleAllocating:
		item.Kind, item.Bucket, item.Priority = "state_inconsistent", BucketActNow, 400
		addReason("stale_allocating")
		item.DriverChoices = []string{"worker_status", "reconcile"}
	case staleReleasing:
		item.Kind, item.Bucket, item.Priority = "state_inconsistent", BucketActNow, 400
		addReason("stale_releasing")
		for _, attempt := range fact.Attempts {
			if attempt.ReleaseState == "releasing" && attempt.ReleaseClaimedAt != nil {
				anchor, anchorSource = *attempt.ReleaseClaimedAt, "release_claim"
			}
		}
		item.DriverChoices = []string{"worker_status", "reconcile"}
	case liveRecordedRunner || (IsTerminalTaskStatus(task.Status) && task.ProcessAlive):
		item.Kind, item.Bucket, item.Priority = "state_inconsistent", BucketActNow, 400
		addReason("terminal_with_recorded_runner")
		terminalAnchor()
		item.DriverChoices = []string{"worker_status", "reconcile"}
	case incompleteProof:
		item.Kind, item.Bucket, item.Priority = "state_inconsistent", BucketActNow, 400
		addReason("incomplete_landing_proof")
		terminalAnchor()
		item.DriverChoices = []string{"task_show", "reconcile"}
	case closed:
		item.Kind, item.Bucket, item.Priority = closedKind, BucketClosed, 0
		if task.ArchivedAt != nil {
			item.Visibility = "archived_closed"
		} else {
			item.DriverChoices = []string{"task_archive"}
		}
		if recoveredLanding {
			addReason("artifact_mismatch_recovered")
		}
		if task.CompletionProvenance == CompletionProvenanceReportRecovery {
			addReason("report_recovered_by_driver")
		}
	case task.Status == TaskStatusWaiting:
		item.Kind, item.Bucket, item.Priority = "question_waiting", BucketActNow, 360
		addReason("awaiting_driver_answer")
		if fact.LatestQuestionAt != nil {
			anchor, anchorSource = *fact.LatestQuestionAt, "accepted_question"
		}
		item.DriverChoices = []string{"task_send", "worker_stop"}
	case task.Status == TaskStatusQueued:
		item.Kind, item.Bucket, item.Priority = "queued_unstarted", BucketQueued, 200
		anchor, anchorSource = task.CreatedAt, "task_created_at"
		item.DriverChoices = []string{"worker_spawn"}
	case task.Status == TaskStatusStarting || task.Status == TaskStatusWorking:
		item.Kind, item.Bucket, item.Priority = "healthy_working", BucketUnderway, 100
		item.DriverChoices = []string{}
	default:
		// Terminal, not closed.
		terminalAnchor()
		switch {
		case (task.Landed || discardAuthorized) && (aggregate.HeldCount > 0 || aggregate.ReleasingCount > 0):
			item.Kind, item.Bucket, item.Priority = "release_needed", BucketNeedsDisposition, 340
			if task.Landed {
				addReason("landed_with_held_attempt")
			}
			if discardAuthorized {
				addReason("discard_authorized")
			}
			if aggregate.ReleasingCount > 0 {
				addReason("release_in_progress")
			}
			item.DriverChoices = []string{"worker_release"}
		case task.Status == TaskStatusDone && task.ClaimedDone && !task.Landed:
			item.Kind, item.Bucket, item.Priority = "artifact_unlanded", BucketResultReady, 330
			addReason("accepted_artifact_without_landing")
			item.DriverChoices = []string{"task_verify", "worker_relaunch", "worker_release_discard"}
		case task.ClaimedDone && !task.Landed:
			item.Kind, item.Bucket, item.Priority = "terminal_held", BucketNeedsDisposition, 330
			addReason("accepted_artifact_without_landing")
			item.DriverChoices = []string{"worker_relaunch", "task_verify", "worker_release_discard"}
		case aggregate.HeldCount > 0 && heldSuperseded && !currentAttemptHeld(fact):
			item.Kind, item.Bucket, item.Priority = "held_superseded_attempt", BucketNeedsDisposition, 320
			addReason("held_superseded_attempt")
			item.DriverChoices = []string{"worker_release", "worker_release_discard"}
		case aggregate.HeldCount > 0:
			item.Kind, item.Bucket, item.Priority = "terminal_held", BucketNeedsDisposition, 320
			item.DriverChoices = []string{"worker_relaunch", "task_retry", "worker_release_discard"}
			if fact.CurrentReportRecoveryAttestation {
				item.Kind = "report_recovery_attested"
				addReason("immutable_report_recovery_requires_verification_or_clean_retry")
				item.DriverChoices = []string{"task_verify", "task_retry"}
				verifyCommand, retryCommand := ReportRecoveryContinuationCommands(task.ID, task.CurrentAttemptID, task.DriverID)
				item.RecoveryCommands = []string{verifyCommand, retryCommand}
			} else if command := reportRecoveryAttentionCommand(fact); command != "" {
				item.Kind = "report_recovery_available"
				addReason("canonical_report_recovery_requires_attestation")
				item.DriverChoices = []string{"task_attest_report_recovery", "worker_relaunch", "task_retry"}
				item.RecoveryCommands = []string{command}
			}
		case aggregate.ReleasingCount > 0:
			item.Kind, item.Bucket, item.Priority = "release_needed", BucketNeedsDisposition, 320
			addReason("release_in_progress")
			item.DriverChoices = []string{"worker_status"}
		case fact.ActiveNotifications > 0:
			item.Kind, item.Bucket, item.Priority = "unhandled_notification", BucketNeedsDisposition, 320
			addReason("active_notification")
			item.DriverChoices = []string{"wake_drain"}
		default:
			item.Kind, item.Bucket, item.Priority = "terminal_unresolved", BucketNeedsDisposition, 320
			addReason("no_explicit_outcome")
			item.DriverChoices = []string{"task_retry"}
		}
		if aggregate.HeldCount > 0 {
			addReason("held_attempt")
		}
		if heldSuperseded {
			addReason("held_superseded_attempt")
		}
		if recoveredLanding {
			addReason("artifact_mismatch_recovered")
		}
		if task.CompletionProvenance == CompletionProvenanceReportRecovery {
			addReason("report_recovered_by_driver")
		}
	}

	if task.ArchivedAt != nil && !closed {
		if item.Priority < 400 {
			item.Priority = 400
		}
		item.Bucket = BucketActNow
		item.Visibility = "archived_unresolved"
		addReason("archived_unresolved")
	}

	item.AttentionSince = anchor.UTC()
	item.AttentionSinceSource = anchorSource
	item.AgeSeconds, item.AgeBucket = attentionAge(fact.Now, item.AttentionSince)
	if item.Priority > 0 {
		item.Priority += ageWeight(item.AgeBucket)
	}
	item.AttentionKey = "task:" + task.ID + ":" + item.Kind
	item.ReasonCodes = reasons
	if (item.Bucket == BucketNeedsDisposition || item.Bucket == BucketResultReady) && strings.TrimSpace(fact.LatestAnnotation) != "" {
		item.LatestAnnotation = boundText(fact.LatestAnnotation, 160)
	}
	if item.DriverChoices == nil {
		item.DriverChoices = []string{}
	}
	if item.RecoveryCommands == nil {
		item.RecoveryCommands = []string{}
	}
	return item
}

func reportRecoveryAttentionCommand(fact AttentionTaskFacts) string {
	if fact.Task.Deliverable != "report" || (fact.Task.Status != TaskStatusBlocked && fact.Task.Status != TaskStatusFailed) || fact.Task.ClaimedDone || fact.Task.ArtifactRef != "" || fact.Task.CompletionProvenance != "" || fact.LatestCheckpoint == nil || fact.CurrentAcceptedWorkerTerminals != 0 {
		return ""
	}
	var attempt *Attempt
	for index := range fact.Attempts {
		if fact.Attempts[index].ID == fact.Task.CurrentAttemptID {
			attempt = &fact.Attempts[index]
			break
		}
	}
	checkpoint := fact.LatestCheckpoint
	if attempt == nil || attempt.RunnerPID != 0 || attempt.ReleaseState != WorkspaceStateHeld || attempt.WorkspaceState != WorkspaceStateHeld ||
		(attempt.Status != AttemptStatusBlocked && attempt.Status != AttemptStatusFailed) || checkpoint.Producer != "worker" ||
		checkpoint.RunGeneration != attempt.RunGeneration || checkpoint.AttemptID != attempt.ID || checkpoint.SourceCursor < 1 ||
		checkpoint.SessionID != attempt.SessionID || checkpoint.Branch != attempt.Branch || checkpoint.HeadCommit == "" || checkpoint.WorkspaceFactsError != "" {
		return ""
	}
	return fmt.Sprintf("shephrd task attest-report-recovery %s --attempt %s --run-generation %d --checkpoint-revision %d --checkpoint-cursor %d --reason %s --driver-id %s --json",
		fact.Task.ID, attempt.ID, attempt.RunGeneration, checkpoint.Revision, checkpoint.SourceCursor,
		ShellQuote("terminal report handoff failed after canonical report write"), ShellQuote(fact.Task.DriverID))
}

func currentAttemptHeld(fact AttentionTaskFacts) bool {
	for _, attempt := range fact.Attempts {
		if attempt.ID == fact.Task.CurrentAttemptID {
			return attempt.ReleaseState == "held" && attempt.ReleasedAt == nil
		}
	}
	return false
}

// attentionClosed applies the closure rules: a task is quiet only when no
// worker is recorded alive, no notification is pending or claimed, every
// attempt with a workspace is released (a superseded held attempt prevents
// closure), nothing is mid-release or unknown, and exactly one explicit
// outcome holds. Archive is never part of closure.
func attentionClosed(task Task, aggregate AttentionAttemptSummary, activeNotifications int, unknownLease, unrecognized bool) (bool, string) {
	if unrecognized || unknownLease || !IsTerminalTaskStatus(task.Status) {
		return false, ""
	}
	if task.ProcessAlive || activeNotifications > 0 {
		return false, ""
	}
	if aggregate.HeldCount > 0 || aggregate.ReleasingCount > 0 {
		return false, ""
	}
	switch {
	case task.Status == TaskStatusDone && task.Landed && aggregate.LandingProvenCount > 0 &&
		(task.ClaimedDone && (task.CompletionProvenance == "" || task.CompletionProvenance == CompletionProvenanceWorkerDone) || !task.ClaimedDone && task.CompletionProvenance == CompletionProvenanceReportRecovery):
		return true, "closed_landed"
	case task.DiscardAuthorized && !task.Landed:
		return true, "closed_discarded"
	case task.Status == TaskStatusStopped && !task.ClaimedDone && !task.Landed && task.ArtifactRef == "":
		return true, "closed_no_work"
	default:
		return false, ""
	}
}

func attentionAge(now, since time.Time) (int64, string) {
	age := now.Sub(since)
	if age < 0 {
		age = 0
	}
	seconds := int64(age / time.Second)
	switch {
	case age < 15*time.Minute:
		return seconds, "fresh"
	case age < 2*time.Hour:
		return seconds, "aging"
	case age < 24*time.Hour:
		return seconds, "stale"
	default:
		return seconds, "overdue"
	}
}

func ageWeight(bucket string) int {
	switch bucket {
	case "aging":
		return 10
	case "stale":
		return 20
	case "overdue":
		return 30
	default:
		return 0
	}
}

func plannedAttentionItem(planned AttentionPlanItem, now time.Time) AttentionItem {
	item := AttentionItem{Kind: "planned_ready", Bucket: BucketPlannedReady, Priority: 220, Visibility: "visible"}
	value := planned
	item.Planned = &value
	item.AttentionSince = planned.UpdatedAt.UTC()
	item.AttentionSinceSource = "plan_item_updated_at"
	item.AgeSeconds, item.AgeBucket = attentionAge(now, item.AttentionSince)
	item.Priority += ageWeight(item.AgeBucket)
	item.AttentionKey = "planned:" + planned.ItemID + ":planned_ready"
	item.ReasonCodes = []string{}
	item.DriverChoices = []string{"plan_dispatch"}
	return item
}

func plannedBlockedAttentionItem(planned AttentionPlanItem, now time.Time) AttentionItem {
	item := AttentionItem{Kind: "planned_blocked", Bucket: BucketPlannedBlocked, Priority: 40, Visibility: "visible"}
	value := planned
	item.Planned = &value
	item.AttentionSince = planned.UpdatedAt.UTC()
	item.AttentionSinceSource = "plan_item_updated_at"
	item.AgeSeconds, item.AgeBucket = attentionAge(now, item.AttentionSince)
	item.Priority += ageWeight(item.AgeBucket)
	item.AttentionKey = "planned:" + planned.ItemID + ":planned_blocked"
	item.ReasonCodes = append([]string{}, planned.ReasonCodes...)
	item.DriverChoices = []string{"plan_edit"}
	return item
}

func countAttention(items []AttentionItem, counts AttentionCounts) AttentionCounts {
	for _, item := range items {
		if item.Bucket == BucketClosed && item.Task != nil && item.Task.Archived {
			counts.ArchivedClosed++
			continue
		}
		switch item.Bucket {
		case BucketActNow:
			counts.ActNow++
		case BucketNeedsDisposition:
			counts.NeedsDisposition++
		case BucketResultReady:
			counts.ResultReady++
		case BucketPlannedReady:
			counts.PlannedReady++
		case BucketUnderway:
			counts.Underway++
		case BucketQueued:
			counts.Queued++
		case BucketPlannedBlocked:
			counts.PlannedBlocked++
		case BucketClosed:
			counts.Closed++
		}
	}
	return counts
}

// sectionAttention assembles the visible item list in deterministic order and
// records an exact omission entry for every bounded or hidden section, so a
// truncated section can never look empty.
func sectionAttention(items []AttentionItem, filter AttentionFilter) ([]AttentionItem, []AttentionOmission) {
	order := []string{BucketActNow, BucketNeedsDisposition, BucketResultReady, BucketPlannedReady,
		BucketUnderway, BucketQueued, BucketPlannedBlocked, BucketClosed}
	byBucket := make(map[string][]AttentionItem)
	for _, item := range items {
		byBucket[item.Bucket] = append(byBucket[item.Bucket], item)
	}
	for bucket, rows := range byBucket {
		rows := rows
		if bucket == BucketClosed {
			sort.SliceStable(rows, func(i, j int) bool {
				if !rows[i].AttentionSince.Equal(rows[j].AttentionSince) {
					return rows[i].AttentionSince.After(rows[j].AttentionSince)
				}
				return rows[i].AttentionKey < rows[j].AttentionKey
			})
		} else {
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].Priority != rows[j].Priority {
					return rows[i].Priority > rows[j].Priority
				}
				if !rows[i].AttentionSince.Equal(rows[j].AttentionSince) {
					return rows[i].AttentionSince.Before(rows[j].AttentionSince)
				}
				return rows[i].AttentionKey < rows[j].AttentionKey
			})
		}
		byBucket[bucket] = rows
	}
	visible := make([]AttentionItem, 0, len(items))
	omitted := make([]AttentionOmission, 0)
	for _, bucket := range order {
		rows := byBucket[bucket]
		sectionVisible := filter.Portfolio || bucket == BucketActNow || bucket == BucketNeedsDisposition ||
			bucket == BucketResultReady || bucket == BucketPlannedReady
		if bucket == BucketClosed {
			kept := rows[:0]
			archivedClosed := 0
			for _, row := range rows {
				if row.Task != nil && row.Task.Archived {
					archivedClosed++
					if !filter.IncludeArchivedClosed {
						continue
					}
				}
				kept = append(kept, row)
			}
			if archivedClosed > 0 && !filter.IncludeArchivedClosed {
				omitted = append(omitted, AttentionOmission{Section: "archived_closed", Count: archivedClosed,
					Reveal: "rerun with --portfolio --include-archived-closed"})
			}
			rows = kept
		}
		if len(rows) == 0 {
			continue
		}
		if !sectionVisible {
			omitted = append(omitted, AttentionOmission{Section: bucket, Count: len(rows), Reveal: "rerun with --portfolio"})
			continue
		}
		if len(rows) > filter.Limit {
			omitted = append(omitted, AttentionOmission{Section: bucket, Count: len(rows) - filter.Limit,
				Reveal: fmt.Sprintf("rerun with --limit %d", minInt(len(rows), AttentionMaxLimit))})
			rows = rows[:filter.Limit]
		}
		visible = append(visible, rows...)
	}
	return visible, omitted
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
