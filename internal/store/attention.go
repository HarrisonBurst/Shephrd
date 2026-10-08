package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shephrd/internal/model"
)

func (s *Store) AttentionSnapshot(filter model.AttentionFilter) (model.AttentionSnapshot, error) {
	if filter.Limit == 0 {
		filter.Limit = model.AttentionDefaultLimit
	}
	if filter.Limit < 1 || filter.Limit > model.AttentionMaxLimit {
		return model.AttentionSnapshot{}, fmt.Errorf("attention limit must be between 1 and %d", model.AttentionMaxLimit)
	}
	if !filter.AllDrivers {
		if err := validateDriverID(filter.DriverID); err != nil {
			return model.AttentionSnapshot{}, err
		}
	}
	generatedAt := time.Now().UTC()
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.AttentionSnapshot{}, err
	}
	defer tx.Rollback()
	snapshot := model.AttentionSnapshot{
		SchemaVersion: model.AttentionSchemaVersion,
		GeneratedAt:   generatedAt,
		Scope:         model.AttentionScope{DriverID: filter.DriverID, AllDrivers: filter.AllDrivers, RepoID: filter.RepoID},
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&snapshot.DatabaseSchemaVersion); err != nil {
		return model.AttentionSnapshot{}, err
	}
	facts, err := attentionTaskFactsTx(tx, filter, generatedAt)
	if err != nil {
		return model.AttentionSnapshot{}, err
	}
	plans, plannedItems := attentionPlansTx(tx, filter)
	snapshot.Plans = plans
	if err := tx.Commit(); err != nil {
		return model.AttentionSnapshot{}, err
	}
	snapshot.Counts, snapshot.Plans, snapshot.Items, snapshot.Omitted = model.ProjectAttention(facts, snapshot.Plans, plannedItems, filter, generatedAt)
	return snapshot, nil
}

func boundText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

func classifyTaskAttentionTx(tx *sql.Tx, taskID, ignoredNotificationID string, generatedAt time.Time) (model.AttentionItem, error) {
	task, err := scanTask(tx.QueryRow(taskSelectTx+` WHERE t.id=?`, taskID))
	if err != nil {
		return model.AttentionItem{}, err
	}
	rows, err := tx.Query(attemptSelect+` WHERE task_id=? ORDER BY number`, taskID)
	if err != nil {
		return model.AttentionItem{}, err
	}
	attempts := make([]model.Attempt, 0)
	for rows.Next() {
		attempt, scanErr := scanAttempt(rows)
		if scanErr != nil {
			rows.Close()
			return model.AttentionItem{}, scanErr
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Close(); err != nil {
		return model.AttentionItem{}, err
	}
	if err := rows.Err(); err != nil {
		return model.AttentionItem{}, err
	}
	var activeNotifications int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM driver_notifications WHERE task_id=? AND notification_id<>? AND state IN ('pending','claimed')`, taskID, ignoredNotificationID).Scan(&activeNotifications); err != nil {
		return model.AttentionItem{}, err
	}
	return model.ClassifyAttention(model.AttentionTaskFacts{Task: task, Attempts: attempts, ActiveNotifications: activeNotifications, Now: generatedAt}), nil
}

func actionableTaskAttention(attention model.AttentionItem) bool {
	switch attention.Bucket {
	case model.BucketActNow, model.BucketNeedsDisposition, model.BucketResultReady:
		return true
	default:
		return false
	}
}

func attentionTaskFactsTx(tx *sql.Tx, filter model.AttentionFilter, generatedAt time.Time) ([]model.AttentionTaskFacts, error) {
	query := taskSelectTx + ` WHERE 1=1`
	var args []any
	if filter.RepoID != "" {
		query += ` AND t.repo_id=?`
		args = append(args, filter.RepoID)
	}
	query += ` ORDER BY t.created_at, t.id`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("attention task read: %w", err)
	}
	tasks := make([]model.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	attemptsByTask, err := attentionAttemptsTx(tx)
	if err != nil {
		return nil, err
	}
	notificationsByTask, err := attentionNotificationsTx(tx)
	if err != nil {
		return nil, err
	}
	questionAt, terminalAt, err := attentionAnchorsTx(tx)
	if err != nil {
		return nil, err
	}
	latestAnnotations, err := attentionLatestAnnotationsTx(tx)
	if err != nil {
		return nil, err
	}
	checkpoints, err := attentionCheckpointsTx(tx)
	if err != nil {
		return nil, err
	}
	acceptedWorkerTerminals, err := attentionAcceptedWorkerTerminalsTx(tx)
	if err != nil {
		return nil, err
	}
	reportRecoveryAttestations, err := attentionReportRecoveryAttestationsTx(tx)
	if err != nil {
		return nil, err
	}
	facts := make([]model.AttentionTaskFacts, 0, len(tasks))
	for _, task := range tasks {
		fact := model.AttentionTaskFacts{Task: task, Attempts: attemptsByTask[task.ID], Now: generatedAt}
		if aggregate, exists := notificationsByTask[task.ID]; exists {
			fact.ActiveNotifications = aggregate.active
			fact.LatestNotificationKind = aggregate.latestKind
			fact.LatestNotificationState = aggregate.latestState
		}
		if at, exists := questionAt[task.ID]; exists {
			value := at
			fact.LatestQuestionAt = &value
		}
		if at, exists := terminalAt[task.ID]; exists {
			value := at
			fact.LatestTerminalAt = &value
		}
		fact.LatestAnnotation = latestAnnotations[task.ID]
		if checkpoint, exists := checkpoints[task.CurrentAttemptID]; exists {
			value := checkpoint
			fact.LatestCheckpoint = &value
		}
		fact.CurrentAcceptedWorkerTerminals = acceptedWorkerTerminals[task.CurrentAttemptID]
		fact.CurrentReportRecoveryAttestation = reportRecoveryAttestations[task.CurrentAttemptID]
		facts = append(facts, fact)
	}
	return facts, nil
}

const taskSelectTx = `SELECT t.id, t.repo_id, r.name, r.path, t.feature_key, t.title, t.driver_id, t.objective, t.acceptance_criteria,
	t.deliverable, t.status, COALESCE(t.current_attempt_id,''), t.artifact_ref, t.claimed_done, t.completion_provenance, t.process_alive,
	t.branch_pushed, t.remote_delivery_state, t.pr_state, lp.landed, lp.landed_reason, t.discard_authorized, t.archived_at, t.created_at, t.updated_at
	FROM tasks t JOIN repos r ON r.id=t.repo_id JOIN task_landing_projections lp ON lp.task_id=t.id`

func attentionAttemptsTx(tx *sql.Tx) (map[string][]model.Attempt, error) {
	rows, err := tx.Query(attemptSelect + ` ORDER BY task_id, number`)
	if err != nil {
		return nil, fmt.Errorf("attention attempt read: %w", err)
	}
	defer rows.Close()
	byTask := make(map[string][]model.Attempt)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		byTask[attempt.TaskID] = append(byTask[attempt.TaskID], attempt)
	}
	return byTask, rows.Err()
}

type attentionNotificationAggregate struct {
	active      int
	latestKind  string
	latestState string
}

func attentionNotificationsTx(tx *sql.Tx) (map[string]attentionNotificationAggregate, error) {
	rows, err := tx.Query(`SELECT d.task_id,
		SUM(CASE WHEN d.state IN ('pending','claimed') THEN 1 ELSE 0 END),
		(SELECT kind FROM driver_notifications latest WHERE latest.task_id=d.task_id ORDER BY latest.message_id DESC LIMIT 1),
		(SELECT state FROM driver_notifications latest WHERE latest.task_id=d.task_id ORDER BY latest.message_id DESC LIMIT 1)
		FROM driver_notifications d WHERE d.task_id IS NOT NULL GROUP BY d.task_id`)
	if err != nil {
		return nil, fmt.Errorf("attention notification read: %w", err)
	}
	defer rows.Close()
	byTask := make(map[string]attentionNotificationAggregate)
	for rows.Next() {
		var taskID string
		var aggregate attentionNotificationAggregate
		if err := rows.Scan(&taskID, &aggregate.active, &aggregate.latestKind, &aggregate.latestState); err != nil {
			return nil, err
		}
		byTask[taskID] = aggregate
	}
	return byTask, rows.Err()
}

func attentionCheckpointsTx(tx *sql.Tx) (map[string]model.AttemptCheckpoint, error) {
	rows, err := tx.Query(`SELECT attempt_id, revision, schema_version, producer, run_generation, source_cursor,
		session_id, branch, head_commit, worktree_dirty, workspace_facts_error, summary, completed_json, next_steps_json,
		decisions_json, changed_paths_json, checks_json, blockers_json, source_attempt_id, source_revision, captured_at FROM attempt_checkpoints`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]model.AttemptCheckpoint)
	for rows.Next() {
		checkpoint, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		result[checkpoint.AttemptID] = checkpoint
	}
	return result, rows.Err()
}

func attentionAcceptedWorkerTerminalsTx(tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.Query(`SELECT a.id, COUNT(m.id) FROM attempts a JOIN messages m
		ON m.attempt_id=a.id AND m.run_generation=a.run_generation
		WHERE m.direction='worker-to-driver' AND m.type IN ('question','done','blocked','failed') AND m.stale=0
		GROUP BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("attention accepted worker terminal read: %w", err)
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var attemptID string
		var count int
		if err := rows.Scan(&attemptID, &count); err != nil {
			return nil, err
		}
		result[attemptID] = count
	}
	return result, rows.Err()
}

func attentionReportRecoveryAttestationsTx(tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT attempt_id FROM report_recovery_attestations`)
	if err != nil {
		return nil, fmt.Errorf("attention report recovery attestation read: %w", err)
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			return nil, err
		}
		result[attemptID] = true
	}
	return result, rows.Err()
}

func attentionLatestAnnotationsTx(tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.Query(`SELECT a.task_id, a.judgment FROM annotations a
		WHERE a.task_id IS NOT NULL AND a.revision=(SELECT MAX(latest.revision) FROM annotations latest WHERE latest.task_id=a.task_id)`)
	if err != nil {
		return nil, fmt.Errorf("attention annotation read: %w", err)
	}
	defer rows.Close()
	latest := make(map[string]string)
	for rows.Next() {
		var taskID, judgment string
		if err := rows.Scan(&taskID, &judgment); err != nil {
			return nil, err
		}
		latest[taskID] = boundText(judgment, 160)
	}
	return latest, rows.Err()
}

func attentionAnchorsTx(tx *sql.Tx) (map[string]time.Time, map[string]time.Time, error) {
	rows, err := tx.Query(`SELECT task_id, type, MAX(created_at) FROM messages
		WHERE direction='worker-to-driver' AND stale=0 AND wake=1 AND type IN ('question','done','blocked','failed')
		GROUP BY task_id, type`)
	if err != nil {
		return nil, nil, fmt.Errorf("attention anchor read: %w", err)
	}
	defer rows.Close()
	questionAt := make(map[string]time.Time)
	terminalAt := make(map[string]time.Time)
	for rows.Next() {
		var taskID, typ, created string
		if err := rows.Scan(&taskID, &typ, &created); err != nil {
			return nil, nil, err
		}
		at := parseTime(created)
		if typ == "question" {
			if existing, exists := questionAt[taskID]; !exists || at.After(existing) {
				questionAt[taskID] = at
			}
			continue
		}
		if existing, exists := terminalAt[taskID]; !exists || at.After(existing) {
			terminalAt[taskID] = at
		}
	}
	return questionAt, terminalAt, rows.Err()
}

// attentionPlansTx projects planned work inside the same read
// transaction. A failure never aborts the ordinary task projection: the
// section reports unavailable with a bounded diagnostic instead.
func attentionPlansTx(tx *sql.Tx, filter model.AttentionFilter) (model.AttentionPlans, []model.AttentionPlanItem) {
	unavailable := func(err error) (model.AttentionPlans, []model.AttentionPlanItem) {
		return model.AttentionPlans{Status: "unavailable", Diagnostic: boundText(err.Error(), 300), Summaries: make([]model.PlanSummary, 0)}, make([]model.AttentionPlanItem, 0)
	}
	lists, err := planSummariesTx(tx, filter.DriverID, filter.AllDrivers)
	if err != nil {
		return unavailable(err)
	}
	items, err := plannedItemsTx(tx, filter)
	if err != nil {
		return unavailable(err)
	}
	return model.AttentionPlans{Status: "available", Summaries: lists}, items
}

type plannedItemRow struct {
	item               model.AttentionPlanItem
	repoID             string
	featureKey         string
	acceptanceCriteria string
}

func plannedItemsTx(tx *sql.Tx, filter model.AttentionFilter) ([]model.AttentionPlanItem, error) {
	query := `SELECT l.id, l.name, l.driver_id, i.id, i.position, i.title, i.objective, COALESCE(i.repo_id,''), COALESCE(r.name,''),
		i.feature_key, i.acceptance_criteria, i.updated_at
		FROM plans l JOIN plan_items i ON i.plan_id=l.id
		LEFT JOIN repos r ON r.id=i.repo_id
		WHERE i.dispatched_task_id IS NULL`
	var args []any
	if !filter.AllDrivers {
		query += ` AND l.driver_id=?`
		args = append(args, filter.DriverID)
	}
	if filter.RepoID != "" {
		query += ` AND i.repo_id=?`
		args = append(args, filter.RepoID)
	}
	query += ` ORDER BY l.created_at, l.id, i.position`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("plan item read: %w", err)
	}
	planned := make([]plannedItemRow, 0)
	for rows.Next() {
		var row plannedItemRow
		var updated string
		if err := rows.Scan(&row.item.PlanID, &row.item.PlanName, &row.item.DriverID, &row.item.ItemID, &row.item.Position,
			&row.item.Title, &row.item.Objective, &row.repoID, &row.item.RepoName, &row.featureKey, &row.acceptanceCriteria, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		row.item.Title = boundText(row.item.Title, 160)
		row.item.FeatureKey = row.featureKey
		row.item.Objective = boundText(row.item.Objective, 160)
		row.item.UpdatedAt = parseTime(updated)
		planned = append(planned, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]model.AttentionPlanItem, 0, len(planned))
	for _, row := range planned {
		codes, err := plannedReadinessTx(tx, row)
		if err != nil {
			return nil, err
		}
		row.item.ReasonCodes = codes
		row.item.Ready = len(codes) == 0
		result = append(result, row.item)
	}
	return result, nil
}

// plannedReadinessTx mirrors the plan readiness projection with reason
// codes only, computed against the snapshot transaction so planned rows are
// consistent with the task rows in the same read.
func plannedReadinessTx(tx *sql.Tx, row plannedItemRow) ([]string, error) {
	codes := make([]string, 0)
	add := func(code string) {
		for _, existing := range codes {
			if existing == code {
				return
			}
		}
		codes = append(codes, code)
	}
	if row.repoID == "" {
		add("repo_required")
	}
	if strings.TrimSpace(row.featureKey) == "" {
		add("feature_required")
	}
	if strings.TrimSpace(row.acceptanceCriteria) == "" {
		add("acceptance_required")
	}
	if row.repoID != "" && strings.TrimSpace(row.featureKey) != "" {
		var existing string
		err := tx.QueryRow(`SELECT id FROM tasks WHERE repo_id=? AND feature_key=?`, row.repoID, row.featureKey).Scan(&existing)
		if err == nil {
			add("feature_conflict")
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	prerequisiteRows, err := tx.Query(`SELECT prerequisite.id, COALESCE(prerequisite.dispatched_task_id,''), COALESCE(t.status,''), COALESCE(lp.landed,0)
		FROM plan_prerequisites relation
		JOIN plan_items prerequisite ON prerequisite.id=relation.prerequisite_item_id
		LEFT JOIN tasks t ON t.id=prerequisite.dispatched_task_id
		LEFT JOIN task_landing_projections lp ON lp.task_id=t.id
		WHERE relation.item_id=?`, row.item.ItemID)
	if err != nil {
		return nil, err
	}
	prerequisiteItems := make(map[string]bool)
	for prerequisiteRows.Next() {
		var prerequisiteID, dispatched, status string
		var landed int
		if err := prerequisiteRows.Scan(&prerequisiteID, &dispatched, &status, &landed); err != nil {
			prerequisiteRows.Close()
			return nil, err
		}
		prerequisiteItems[prerequisiteID] = true
		switch {
		case dispatched == "":
			add("prerequisite_not_dispatched")
		case status != model.TaskStatusDone:
			add("prerequisite_not_done")
		case landed == 0:
			add("prerequisite_not_verified")
		}
	}
	if err := prerequisiteRows.Close(); err != nil {
		return nil, err
	}
	inputRows, err := tx.Query(`SELECT input.prerequisite_item_id, COALESCE(input.artifact_id,''), COALESCE(va.producer_task_id,'')
		FROM plan_report_inputs input LEFT JOIN verified_artifacts va ON va.id=input.artifact_id
		WHERE input.item_id=? ORDER BY input.position`, row.item.ItemID)
	if err != nil {
		return nil, err
	}
	type selectedInput struct{ prerequisiteID, artifactID, producerTaskID string }
	inputs := make([]selectedInput, 0)
	for inputRows.Next() {
		var input selectedInput
		if err := inputRows.Scan(&input.prerequisiteID, &input.artifactID, &input.producerTaskID); err != nil {
			inputRows.Close()
			return nil, err
		}
		inputs = append(inputs, input)
	}
	if err := inputRows.Close(); err != nil {
		return nil, err
	}
	if len(inputs) > model.MaxTaskReportInputs {
		add("too_many_reports")
	}
	for _, input := range inputs {
		if !prerequisiteItems[input.prerequisiteID] {
			add("report_prerequisite_missing")
			continue
		}
		if input.artifactID == "" {
			add("report_not_selected")
			continue
		}
		if input.producerTaskID == "" {
			add("report_selection_stale")
			continue
		}
		if err := eligibleVerifiedReportTx(tx, input.producerTaskID, input.artifactID); err != nil {
			add("report_selection_stale")
		}
	}
	return codes, nil
}
