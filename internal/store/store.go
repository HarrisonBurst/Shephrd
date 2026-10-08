package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

type Store struct {
	db   *sql.DB
	path string
}

type TaskFilter struct {
	Status          string
	RepoID          string
	IncludeArchived bool
}

const (
	sqliteBusyCode            = 5
	sqliteBusyTimeout         = 5 * time.Second
	sqliteRetryInitialBackoff = 10 * time.Millisecond
	sqliteRetryMaxBackoff     = 250 * time.Millisecond
)

func (s *Store) Close() error {
	return s.db.Close()
}

func retrySQLiteSetup(operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), sqliteBusyTimeout)
	defer cancel()
	return retrySQLiteContention(ctx, sqliteRetryInitialBackoff, operation)
}

func retrySQLiteContention(ctx context.Context, backoff time.Duration, operation func(context.Context) error) error {
	for {
		err := operation(ctx)
		if err == nil || !isSQLiteContention(err) {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		backoff = min(backoff*2, sqliteRetryMaxBackoff)
	}
}

func isSQLiteContention(err error) bool {
	var sqliteErr sqliteError
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteBusyCode
}

func NewID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

func (s *Store) UpsertRepo(repo model.Repo) (model.Repo, error) {
	if existing, err := s.RepoByPath(repo.Path); err == nil {
		repo.ID = existing.ID
		repo.CreatedAt = existing.CreatedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return model.Repo{}, err
	}
	if repo.ID == "" {
		repo.ID = NewID("repo")
	}
	t := time.Now().UTC()
	if repo.CreatedAt.IsZero() {
		repo.CreatedAt = t
	}
	repo.UpdatedAt = t
	_, err := s.db.Exec(`INSERT INTO repos(id, name, path, default_branch, context_file, setup_hook, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET name=excluded.name, default_branch=excluded.default_branch,
		context_file=CASE WHEN excluded.context_file='' THEN repos.context_file ELSE excluded.context_file END,
		setup_hook=CASE WHEN excluded.setup_hook='' THEN repos.setup_hook ELSE excluded.setup_hook END, updated_at=excluded.updated_at`,
		repo.ID, repo.Name, repo.Path, repo.DefaultBranch, repo.ContextFile, repo.SetupHook, stamp(repo.CreatedAt), stamp(repo.UpdatedAt))
	if err != nil {
		if isSQLiteConstraint(err, sqliteConstraintUnique) {
			var conflict int
			if queryErr := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM repos WHERE name=? AND path<>?)`, repo.Name, repo.Path).Scan(&conflict); queryErr == nil && conflict != 0 {
				return model.Repo{}, contractFailure(ErrRepoNameConflict, "repo name %q is already registered; use --name with a unique alias", repo.Name)
			}
		}
		return model.Repo{}, fmt.Errorf("register repo %q: %w", repo.Path, err)
	}
	return s.RepoByPath(repo.Path)
}

func (s *Store) Repos() ([]model.Repo, error) {
	rows, err := s.db.Query(`SELECT id, name, path, default_branch, context_file, setup_hook, created_at, updated_at FROM repos ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer rows.Close()
	repos := make([]model.Repo, 0)
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

func (s *Store) RepoByPath(path string) (model.Repo, error) {
	return scanRepo(s.db.QueryRow(`SELECT id, name, path, default_branch, context_file, setup_hook, created_at, updated_at FROM repos WHERE path=?`, path))
}

func (s *Store) Repo(ref string) (model.Repo, error) {
	repo, err := scanRepo(s.db.QueryRow(`SELECT id, name, path, default_branch, context_file, setup_hook, created_at, updated_at FROM repos WHERE id=? OR name=? OR path=?`, ref, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Repo{}, fmt.Errorf("repo %q is not registered; run 'shephrd repo add <path>' first", ref)
	}
	return repo, err
}

func scanRepo(scanner interface{ Scan(...any) error }) (model.Repo, error) {
	var repo model.Repo
	var created, updated string
	err := scanner.Scan(&repo.ID, &repo.Name, &repo.Path, &repo.DefaultBranch, &repo.ContextFile, &repo.SetupHook, &created, &updated)
	if err != nil {
		return model.Repo{}, err
	}
	repo.CreatedAt = parseTime(created)
	repo.UpdatedAt = parseTime(updated)
	return repo, nil
}

func (s *Store) CreateTask(task model.Task) (model.Task, error) {
	return s.createTask(task, nil)
}

func (s *Store) CreateTaskWithReportInputs(task model.Task, inputs []model.ReportArtifactSelection) (model.Task, error) {
	if len(inputs) > model.MaxTaskReportInputs {
		return model.Task{}, fmt.Errorf("a task can attach at most %d verified reports", model.MaxTaskReportInputs)
	}
	seenProducers := make(map[string]struct{}, len(inputs))
	seenArtifacts := make(map[string]struct{}, len(inputs))
	for index := range inputs {
		input := &inputs[index]
		input.ProducerTaskID = strings.TrimSpace(input.ProducerTaskID)
		input.ArtifactID = strings.TrimSpace(input.ArtifactID)
		if input.ProducerTaskID == "" || input.ArtifactID == "" {
			return model.Task{}, fmt.Errorf("producer task and verified report artifact must not be empty")
		}
		if _, exists := seenProducers[input.ProducerTaskID]; exists {
			return model.Task{}, fmt.Errorf("producer task %s was selected more than once", input.ProducerTaskID)
		}
		if _, exists := seenArtifacts[input.ArtifactID]; exists {
			return model.Task{}, fmt.Errorf("verified report artifact %s was selected more than once", input.ArtifactID)
		}
		seenProducers[input.ProducerTaskID] = struct{}{}
		seenArtifacts[input.ArtifactID] = struct{}{}
	}
	return s.createTask(task, inputs)
}

func (s *Store) createTask(task model.Task, inputs []model.ReportArtifactSelection) (model.Task, error) {
	if model.IsSubdriverOwner(task.DriverID) {
		return model.Task{}, fmt.Errorf("sub-driver workers require correlated subdriver dispatch")
	}
	task.DriverID = strings.TrimSpace(task.DriverID)
	task.Title = strings.TrimSpace(task.Title)
	if task.Title == "" || strings.TrimSpace(task.FeatureKey) == "" || strings.TrimSpace(task.Objective) == "" {
		return model.Task{}, fmt.Errorf("task title, feature key, and objective must not be empty")
	}
	if err := validateDriverID(task.DriverID); err != nil {
		return model.Task{}, err
	}
	if task.Deliverable != "" && task.Deliverable != "code" && task.Deliverable != "report" {
		return model.Task{}, fmt.Errorf("task deliverable must be code or report")
	}
	if task.ID == "" {
		task.ID = NewID("task")
	}
	if task.Deliverable == "" {
		task.Deliverable = "code"
	}
	for _, input := range inputs {
		if task.ID == input.ProducerTaskID {
			return model.Task{}, fmt.Errorf("task %s cannot attach its own report", task.ID)
		}
	}
	task.Status = model.TaskStatusQueued
	t := time.Now().UTC()
	task.CreatedAt, task.UpdatedAt = t, t
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	for _, input := range inputs {
		if err := eligibleVerifiedReportTx(tx, input.ProducerTaskID, input.ArtifactID); err != nil {
			return model.Task{}, err
		}
	}
	if err := insertTaskTx(tx, task, t); err != nil {
		return model.Task{}, err
	}
	for index, input := range inputs {
		if _, err := tx.Exec(`INSERT INTO task_report_inputs(target_task_id, position, artifact_id, attached_by_driver_id, attached_at) VALUES(?, ?, ?, ?, ?)`, task.ID, index+1, input.ArtifactID, task.DriverID, stamp(t)); err != nil {
			return model.Task{}, fmt.Errorf("attach verified report to task: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Task{}, err
	}
	return s.Task(task.ID)
}

func insertTaskTx(tx *sql.Tx, task model.Task, created time.Time) error {
	_, err := tx.Exec(`INSERT INTO tasks(id, repo_id, feature_key, title, driver_id, objective, acceptance_criteria, deliverable, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, task.ID, task.RepoID, task.FeatureKey, task.Title, task.DriverID, task.Objective,
		task.AcceptanceCriteria, task.Deliverable, task.Status, stamp(created), stamp(created))
	if err != nil {
		if isSQLiteConstraint(err, sqliteConstraintUnique) {
			var conflict int
			if queryErr := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE repo_id=? AND feature_key=?)`, task.RepoID, task.FeatureKey).Scan(&conflict); queryErr == nil && conflict != 0 {
				return contractFailure(ErrTaskFeatureConflict, "feature %q already has a task in this repo; choose a different --feature key", task.FeatureKey)
			}
		}
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

func eligibleVerifiedReportIDsTx(tx *sql.Tx, producerTaskID string) ([]string, error) {
	rows, err := tx.Query(`SELECT va.id
		FROM verified_artifacts va
		JOIN tasks p ON p.id=va.producer_task_id
		JOIN task_landing_projections lp ON lp.task_id=p.id
		JOIN attempts a ON a.id=va.producer_attempt_id
		JOIN messages m ON m.id=va.done_message_id
		LEFT JOIN report_recovery_attestations rr ON rr.id=va.report_recovery_id
		WHERE va.producer_task_id=? AND va.kind='report'
		AND p.deliverable='report' AND p.status=? AND lp.landed=1
		AND p.current_attempt_id=va.producer_attempt_id AND p.artifact_ref=va.original_ref
		AND a.task_id=p.id AND a.landed_proven=1
		AND m.task_id=p.id AND m.attempt_id=a.id AND m.stale=0 AND m.artifact_ref=va.original_ref
		AND NOT EXISTS (SELECT 1 FROM report_lifecycle_invocations lifecycle WHERE lifecycle.artifact_id=va.id AND lifecycle.state IN ('pending','invoking'))
		AND ((va.report_recovery_id IS NULL AND p.claimed_done=1 AND p.completion_provenance=? AND m.direction='worker-to-driver' AND m.type='done')
		OR (va.report_recovery_id IS NOT NULL AND p.claimed_done=0 AND p.completion_provenance=? AND m.direction='system' AND m.type='report-recovery'
		AND rr.task_id=p.id AND rr.attempt_id=a.id AND rr.run_generation=a.run_generation AND rr.sha256=va.sha256 AND rr.size_bytes=va.size_bytes))
		ORDER BY va.id`, producerTaskID, model.TaskStatusDone, model.CompletionProvenanceWorkerDone, model.CompletionProvenanceReportRecovery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, 1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func eligibleVerifiedReportTx(tx *sql.Tx, producerTaskID, artifactID string) error {
	var eligible int
	err := tx.QueryRow(`SELECT COUNT(*)
		FROM verified_artifacts va
		JOIN tasks p ON p.id=va.producer_task_id
		JOIN task_landing_projections lp ON lp.task_id=p.id
		JOIN attempts a ON a.id=va.producer_attempt_id
		JOIN messages m ON m.id=va.done_message_id
		LEFT JOIN report_recovery_attestations rr ON rr.id=va.report_recovery_id
		WHERE va.id=? AND va.producer_task_id=? AND va.kind='report'
		AND p.deliverable='report' AND p.status=? AND lp.landed=1
		AND p.current_attempt_id=va.producer_attempt_id AND p.artifact_ref=va.original_ref
		AND a.task_id=p.id AND a.landed_proven=1
		AND m.task_id=p.id AND m.attempt_id=a.id AND m.stale=0 AND m.artifact_ref=va.original_ref
		AND NOT EXISTS (SELECT 1 FROM report_lifecycle_invocations lifecycle WHERE lifecycle.artifact_id=va.id AND lifecycle.state IN ('pending','invoking'))
		AND ((va.report_recovery_id IS NULL AND p.claimed_done=1 AND p.completion_provenance=? AND m.direction='worker-to-driver' AND m.type='done')
		OR (va.report_recovery_id IS NOT NULL AND p.claimed_done=0 AND p.completion_provenance=? AND m.direction='system' AND m.type='report-recovery'
		AND rr.task_id=p.id AND rr.attempt_id=a.id AND rr.run_generation=a.run_generation AND rr.sha256=va.sha256 AND rr.size_bytes=va.size_bytes))`, artifactID, producerTaskID, model.TaskStatusDone, model.CompletionProvenanceWorkerDone, model.CompletionProvenanceReportRecovery).Scan(&eligible)
	if err != nil {
		return err
	}
	if eligible != 1 {
		return fmt.Errorf("producer task %s no longer has the selected verified report; select a current verified report", producerTaskID)
	}
	return nil
}

func (s *Store) Task(id string) (model.Task, error) {
	task, err := scanTask(s.db.QueryRow(s.taskSelect()+` WHERE t.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, fmt.Errorf("task %q does not exist", id)
	}
	return task, err
}

func (s *Store) Tasks(filter TaskFilter) ([]model.Task, error) {
	query := s.taskSelect() + ` WHERE 1=1`
	var args []any
	if !filter.IncludeArchived {
		query += ` AND t.archived_at IS NULL`
	}
	if filter.Status != "" {
		query += ` AND t.status=?`
		args = append(args, filter.Status)
	}
	if filter.RepoID != "" {
		query += ` AND t.repo_id=?`
		args = append(args, filter.RepoID)
	}
	query += ` ORDER BY t.created_at`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]model.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) ArchiveTask(id string) (model.Task, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	var status string
	var processAlive int
	var archived sql.NullString
	if err := tx.QueryRow(`SELECT status, process_alive, archived_at FROM tasks WHERE id=?`, id).Scan(&status, &processAlive, &archived); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Task{}, fmt.Errorf("task %q does not exist", id)
		}
		return model.Task{}, err
	}
	if !model.IsTerminalTaskStatus(status) {
		return model.Task{}, fmt.Errorf("task %s is %s; only terminal tasks can be archived", id, status)
	}
	if processAlive != 0 {
		return model.Task{}, fmt.Errorf("task %s has a live worker; wait for the worker to exit before archiving", id)
	}
	if !archived.Valid {
		timestamp := now()
		if _, err := tx.Exec(`UPDATE tasks SET archived_at=?, updated_at=? WHERE id=?`, timestamp, timestamp, id); err != nil {
			return model.Task{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Task{}, err
	}
	return s.Task(id)
}

const taskSelect = `SELECT t.id, t.repo_id, r.name, r.path, t.feature_key, t.title, t.driver_id, t.objective, t.acceptance_criteria,
	t.deliverable, t.status, COALESCE(t.current_attempt_id,''), t.artifact_ref, t.claimed_done, t.completion_provenance, t.process_alive,
	t.branch_pushed, t.remote_delivery_state, t.pr_state, lp.landed, lp.landed_reason, t.discard_authorized, t.archived_at, t.created_at, t.updated_at
	FROM tasks t JOIN repos r ON r.id=t.repo_id JOIN task_landing_projections lp ON lp.task_id=t.id`

func (s *Store) taskSelect() string {
	return taskSelect
}

func scanTask(scanner interface{ Scan(...any) error }) (model.Task, error) {
	var task model.Task
	var claimed, alive, pushed, landed, discard int
	var archived sql.NullString
	var created, updated string
	err := scanner.Scan(&task.ID, &task.RepoID, &task.RepoName, &task.RepoPath, &task.FeatureKey, &task.Title, &task.DriverID,
		&task.Objective, &task.AcceptanceCriteria, &task.Deliverable, &task.Status, &task.CurrentAttemptID,
		&task.ArtifactRef, &claimed, &task.CompletionProvenance, &alive, &pushed, &task.RemoteDeliveryState, &task.PRState, &landed, &task.LandedReason, &discard, &archived, &created, &updated)
	if err != nil {
		return model.Task{}, err
	}
	task.ClaimedDone, task.ProcessAlive, task.BranchPushed = claimed != 0, alive != 0, pushed != 0
	task.Landed, task.DiscardAuthorized = landed != 0, discard != 0
	if archived.Valid {
		value := parseTime(archived.String)
		task.ArchivedAt = &value
	}
	task.CreatedAt, task.UpdatedAt = parseTime(created), parseTime(updated)
	return task, nil
}

func (s *Store) BeginAttempt(taskID, harness, modelID string) (model.Attempt, error) {
	return s.BeginAttemptWithBaseSelection(taskID, harness, modelID, model.DefaultBaseSelection())
}

func (s *Store) BeginAttemptWithBaseSelection(taskID, harness, modelID string, selection model.BaseSelection) (model.Attempt, error) {
	selection, err := model.NormalizeBaseSelection(selection)
	if err != nil {
		return model.Attempt{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Attempt{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM tasks WHERE id=?`, taskID).Scan(&status); err != nil {
		return model.Attempt{}, err
	}
	if err := model.ValidateTransition(status, model.TaskStatusStarting, false); err != nil {
		return model.Attempt{}, err
	}
	var number int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(number),0)+1 FROM attempts WHERE task_id=?`, taskID).Scan(&number); err != nil {
		return model.Attempt{}, err
	}
	var sourceAttemptID string
	var sourceRevision int
	if err := tx.QueryRow(`SELECT COALESCE(current_attempt_id,'') FROM tasks WHERE id=?`, taskID).Scan(&sourceAttemptID); err != nil {
		return model.Attempt{}, err
	}
	if sourceAttemptID != "" {
		_ = tx.QueryRow(`SELECT revision FROM attempt_checkpoints WHERE attempt_id=?`, sourceAttemptID).Scan(&sourceRevision)
	}
	t := time.Now().UTC()
	attempt := model.Attempt{ID: NewID("attempt"), TaskID: taskID, Number: number, Harness: harness, Model: modelID, RuntimeBackend: "headless", Status: model.AttemptStatusStarting,
		BaseStrategy: selection.Strategy, BaseRef: selection.Ref, ResumeSourceAttemptID: sourceAttemptID, ResumeSourceRevision: sourceRevision, CreatedAt: t, UpdatedAt: t}
	if _, err := tx.Exec(`INSERT INTO attempts(id, task_id, number, harness, model, resume_source_attempt_id, resume_source_revision, status, base_strategy, base_ref, created_at, updated_at) VALUES(?, ?, ?, ?, ?, NULLIF(?,''), ?, ?, ?, ?, ?, ?)`,
		attempt.ID, taskID, number, harness, modelID, sourceAttemptID, sourceRevision, attempt.Status, selection.Strategy, selection.Ref, stamp(t), stamp(t)); err != nil {
		return model.Attempt{}, err
	}
	if _, err := tx.Exec(`UPDATE tasks SET status=?, current_attempt_id=?, process_alive=0, claimed_done=0, completion_provenance='',
		artifact_ref='', branch_pushed=0, remote_delivery_state='unverified', pr_state='', discard_authorized=0, updated_at=? WHERE id=?`, model.TaskStatusStarting, attempt.ID, stamp(t), taskID); err != nil {
		return model.Attempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Attempt{}, err
	}
	return attempt, nil
}

func (s *Store) SetAttemptBaseCommit(id, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("attempt base commit must not be empty")
	}
	result, err := s.db.Exec(`UPDATE attempts SET base_commit=?, updated_at=? WHERE id=? AND (base_commit='' OR base_commit=?)`, commit, now(), id, commit)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %s already has a different base commit", id)
	}
	return nil
}

func validRuntimeBackend(backend string) bool {
	if backend == "headless" {
		return true
	}
	if backend == "auto" || len(backend) == 0 || len(backend) > 32 || backend[0] < 'a' || backend[0] > 'z' {
		return false
	}
	for _, char := range backend[1:] {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
			return false
		}
	}
	return true
}

func validateTerminalDiagnostics(endpoint model.TerminalEndpoint) error {
	for _, value := range []string{endpoint.ProviderVersion, endpoint.ProtocolVersion} {
		if len(value) > 128 || strings.IndexFunc(value, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
			return fmt.Errorf("terminal endpoint version metadata is invalid")
		}
	}
	if len(endpoint.Capabilities) > 64 {
		return fmt.Errorf("terminal endpoint capability metadata is too large")
	}
	seen := make(map[string]bool, len(endpoint.Capabilities))
	for _, capability := range endpoint.Capabilities {
		if capability == "" || len(capability) > 128 || seen[capability] || strings.IndexFunc(capability, func(char rune) bool {
			return !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune(":._-", char))
		}) >= 0 {
			return fmt.Errorf("terminal endpoint capability metadata is invalid")
		}
		seen[capability] = true
	}
	return nil
}

func (s *Store) SetRuntimeBackend(id, backend string) error {
	if !validRuntimeBackend(backend) {
		return fmt.Errorf("worker runtime backend %q is invalid", backend)
	}
	result, err := s.db.Exec(`UPDATE attempts SET runtime_backend=?, updated_at=? WHERE id=?`, backend, now(), id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %q does not exist", id)
	}
	return nil
}

func (s *Store) SetTerminalEndpointForRun(id string, runGeneration int, endpoint model.TerminalEndpoint) (int, error) {
	if !validRuntimeBackend(endpoint.Backend) || endpoint.Backend == "headless" || endpoint.SocketPath == "" || endpoint.WorkspaceID == "" || endpoint.PaneID == "" {
		return 0, fmt.Errorf("terminal endpoint identity must be complete")
	}
	if endpoint.TabID == "" && (endpoint.WindowID == "" || endpoint.SurfaceID == "") || endpoint.TabID != "" && (endpoint.WindowID != "" || endpoint.SurfaceID != "") {
		return 0, fmt.Errorf("terminal endpoint hierarchy must be unambiguous")
	}
	if err := validateTerminalDiagnostics(endpoint); err != nil {
		return 0, err
	}
	capabilitiesJSON, err := json.Marshal(endpoint.Capabilities)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE attempts SET runtime_generation=runtime_generation+1,
		terminal_socket_path=?, terminal_window_id=?, terminal_workspace_id=?, terminal_tab_id=?, terminal_pane_id=?, terminal_surface_id=?,
		terminal_provider_version=?, terminal_protocol_version=?, terminal_capabilities_json=?, updated_at=?
		WHERE id=? AND run_generation=? AND runtime_backend=?`, endpoint.SocketPath, endpoint.WindowID, endpoint.WorkspaceID,
		endpoint.TabID, endpoint.PaneID, endpoint.SurfaceID, endpoint.ProviderVersion, endpoint.ProtocolVersion, string(capabilitiesJSON),
		now(), id, runGeneration, endpoint.Backend)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("%s runtime is not configured for attempt %s", endpoint.Backend, id)
	}
	var generation int
	if err := tx.QueryRow(`SELECT runtime_generation FROM attempts WHERE id=?`, id).Scan(&generation); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return generation, nil
}

var terminalIntentSourcePattern = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,120}$`)

func validTerminalIntentText(value string, limit int) bool {
	return len(value) <= limit && strings.IndexFunc(value, func(char rune) bool { return char < 0x20 || char == 0x7f }) < 0
}

func (s *Store) SetTerminalCreateIntentForRun(id string, runGeneration int, intent model.TerminalCreateIntent) error {
	if !validRuntimeBackend(intent.Backend) || intent.Backend == "headless" || intent.Backend == "auto" {
		return fmt.Errorf("terminal create intent backend is invalid")
	}
	if runGeneration < 1 || intent.Generation < 1 || !terminalIntentSourcePattern.MatchString(intent.Source) || !filepath.IsAbs(intent.CWD) || len(intent.CWD) > 4096 || len(intent.Label) == 0 || !validTerminalIntentText(intent.WindowID, 128) || !validTerminalIntentText(intent.WorkspaceID, 128) || !validTerminalIntentText(intent.CWD, 4096) || !validTerminalIntentText(intent.Label, 4096) {
		return fmt.Errorf("terminal create intent identity is invalid")
	}
	result, err := s.db.Exec(`UPDATE attempts SET terminal_create_state=?, terminal_create_run_generation=?, terminal_create_backend=?, terminal_create_source=?,
		terminal_create_window_id=?, terminal_create_workspace_id=?, terminal_create_cwd=?, terminal_create_label=?, terminal_create_generation=?, updated_at=?
		WHERE id=? AND run_generation=? AND runtime_backend=?`, model.TerminalCreateIntentPending, runGeneration, intent.Backend, intent.Source,
		intent.WindowID, intent.WorkspaceID, intent.CWD, intent.Label, intent.Generation, now(), id, runGeneration, intent.Backend)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("%s runtime is not configured for attempt %s", intent.Backend, id)
	}
	return nil
}

func (s *Store) ResolveTerminalCreateIntentForRun(id string, runGeneration int, state string) error {
	switch state {
	case model.TerminalCreateIntentCommitted, model.TerminalCreateIntentAborted, model.TerminalCreateIntentAbandoned:
	default:
		return fmt.Errorf("terminal create intent resolution state %q is invalid", state)
	}
	result, err := s.db.Exec(`UPDATE attempts SET terminal_create_state=?, updated_at=? WHERE id=? AND terminal_create_state=? AND terminal_create_run_generation=?`,
		state, now(), id, model.TerminalCreateIntentPending, runGeneration)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %s has no pending terminal create intent for run generation %d", id, runGeneration)
	}
	return nil
}

func (s *Store) Attempt(id string) (model.Attempt, error) {
	attempt, err := scanAttempt(s.db.QueryRow(attemptSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Attempt{}, fmt.Errorf("attempt %q does not exist", id)
	}
	if err != nil {
		return model.Attempt{}, err
	}
	return attempt, nil
}

func (s *Store) Attempts(taskID string) ([]model.Attempt, error) {
	rows, err := s.db.Query(attemptSelect+` WHERE task_id=? ORDER BY number`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := make([]model.Attempt, 0)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
}

const attemptSelect = `SELECT id, task_id, number, harness, COALESCE(model,''), runtime_backend, runtime_generation, runtime_executable,
	run_generation, COALESCE(resume_source_attempt_id,''), resume_source_revision,
	session_id, terminal_socket_path, terminal_window_id, terminal_workspace_id, terminal_tab_id, terminal_pane_id, terminal_surface_id,
	terminal_provider_version, terminal_protocol_version, terminal_capabilities_json,
	terminal_create_state, terminal_create_run_generation, terminal_create_backend, terminal_create_source,
	terminal_create_window_id, terminal_create_workspace_id, terminal_create_cwd, terminal_create_label, terminal_create_generation, updated_at AS terminal_updated_at,
	workspace_backend, workspace_state, workspace_state_changed_at, intended_worktree_path,
	worktree_path, worktree_git_dir, worktree_common_dir, lease_id, branch, status,
	runner_pid, cursor, exit_code, failure_reason, base_commit, landed_proven, landing_kind, landed_source_commit,
	landed_target_ref, landed_target_commit, landed_checkpoint_revision, landed_verified_at, landing_reason, landing_quarantine_reason, discard_authorized,
	release_state, release_claimed_at, release_owner_pid, release_reason, released_at, created_at, updated_at, ended_at,
	base_strategy, base_ref,
	COALESCE((SELECT substr(m.artifact_ref,8) FROM messages m WHERE m.attempt_id=attempts.id AND m.run_generation=attempts.run_generation
		AND m.direction='system' AND m.type='report-destination'), '') FROM attempts`

func scanAttempt(scanner interface{ Scan(...any) error }) (model.Attempt, error) {
	var attempt model.Attempt
	var exit sql.NullInt64
	var landed, discard int
	var landedVerified, releaseClaimed, released, ended, workspaceChanged sql.NullString
	var capabilitiesJSON string
	var intent model.TerminalCreateIntent
	var intentUpdatedAt string
	var created, updated string
	var endpoint model.TerminalEndpoint
	err := scanner.Scan(&attempt.ID, &attempt.TaskID, &attempt.Number, &attempt.Harness, &attempt.Model, &attempt.RuntimeBackend,
		&attempt.RuntimeGeneration, &attempt.RuntimeExecutable, &attempt.RunGeneration, &attempt.ResumeSourceAttemptID, &attempt.ResumeSourceRevision,
		&attempt.SessionID,
		&endpoint.SocketPath, &endpoint.WindowID, &endpoint.WorkspaceID,
		&endpoint.TabID, &endpoint.PaneID, &endpoint.SurfaceID,
		&endpoint.ProviderVersion, &endpoint.ProtocolVersion, &capabilitiesJSON,
		&intent.State, &intent.RunGeneration, &intent.Backend, &intent.Source, &intent.WindowID, &intent.WorkspaceID,
		&intent.CWD, &intent.Label, &intent.Generation, &intentUpdatedAt,
		&attempt.WorkspaceBackend, &attempt.WorkspaceState, &workspaceChanged, &attempt.IntendedWorktreePath,
		&attempt.WorktreePath, &attempt.WorktreeGitDir, &attempt.WorktreeCommonDir, &attempt.LeaseID, &attempt.Branch, &attempt.Status,
		&attempt.RunnerPID, &attempt.Cursor,
		&exit, &attempt.FailureReason, &attempt.BaseCommit, &landed, &attempt.LandingKind, &attempt.LandedSourceCommit,
		&attempt.LandedTargetRef, &attempt.LandedTargetCommit, &attempt.LandedCheckpointRevision, &landedVerified, &attempt.LandingReason,
		&attempt.LandingQuarantineReason, &discard, &attempt.ReleaseState, &releaseClaimed, &attempt.ReleaseOwnerPID, &attempt.ReleaseReason, &released, &created, &updated, &ended,
		&attempt.BaseStrategy, &attempt.BaseRef, &attempt.ReportPath)
	if err != nil {
		return model.Attempt{}, err
	}
	if intent.State != "" {
		intent.UpdatedAt = intentUpdatedAt
		attempt.TerminalCreateIntent = &intent
	}
	if err := json.Unmarshal([]byte(capabilitiesJSON), &endpoint.Capabilities); err != nil {
		return model.Attempt{}, fmt.Errorf("attempt %s has malformed terminal capability metadata", attempt.ID)
	}
	if endpoint.SocketPath != "" || endpoint.WindowID != "" || endpoint.WorkspaceID != "" ||
		endpoint.TabID != "" || endpoint.PaneID != "" || endpoint.SurfaceID != "" {
		endpoint.Backend = attempt.RuntimeBackend
		attempt.TerminalEndpoint = &endpoint
	}
	if workspaceChanged.Valid {
		value := parseTime(workspaceChanged.String)
		attempt.WorkspaceStateChangedAt = &value
	}
	if exit.Valid {
		value := int(exit.Int64)
		attempt.ExitCode = &value
	}
	if landedVerified.Valid {
		value := parseTime(landedVerified.String)
		attempt.LandedVerifiedAt = &value
	}
	if releaseClaimed.Valid {
		value := parseTime(releaseClaimed.String)
		attempt.ReleaseClaimedAt = &value
	}
	if released.Valid {
		value := parseTime(released.String)
		attempt.ReleasedAt = &value
	}
	if ended.Valid {
		value := parseTime(ended.String)
		attempt.EndedAt = &value
	}
	attempt.LandedProven, attempt.DiscardAuthorized = landed != 0, discard != 0
	attempt.CreatedAt, attempt.UpdatedAt = parseTime(created), parseTime(updated)
	if _, err := model.NormalizeBaseSelection(model.BaseSelection{Strategy: attempt.BaseStrategy, Ref: attempt.BaseRef}); err != nil {
		return model.Attempt{}, fmt.Errorf("attempt %s has invalid base selection: %w", attempt.ID, err)
	}
	return attempt, nil
}

func (s *Store) Detail(taskID string) (model.TaskDetail, error) {
	task, err := s.Task(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	inputs, err := s.ReportInputs(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	attempts, err := s.Attempts(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	messages, err := s.Messages(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	checkpoints := make([]model.AttemptCheckpoint, 0)
	for _, attempt := range attempts {
		checkpoint, checkpointErr := s.LatestCheckpoint(attempt.ID)
		if checkpointErr == nil {
			checkpoints = append(checkpoints, checkpoint)
		}
	}
	notifications, err := s.Notifications(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	attestations, err := s.ExternalDeliveryAttestations(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	recoveries, err := s.LocalDeliveryRecoveries(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	reportRecoveries, err := s.ReportRecoveryAttestations(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	lifecycleInvocations, err := s.ReportLifecycleInvocations(taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	annotations, err := s.Annotations(model.AnnotationScope{TaskID: taskID})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return model.TaskDetail{Task: task, Inputs: inputs, Attempts: attempts, Messages: messages, Checkpoints: checkpoints, Annotations: annotations,
		Notifications: notifications, ExternalDeliveryAttestations: attestations, LocalDeliveryRecoveries: recoveries,
		ReportRecoveryAttestations: reportRecoveries, ReportLifecycleInvocations: lifecycleInvocations}, nil
}

func (s *Store) ReportInputs(taskID string) ([]model.ReportInput, error) {
	rows, err := s.db.Query(`SELECT i.position, va.id, r.id, r.name, va.producer_task_id, va.producer_attempt_id,
		va.done_message_id, COALESCE(va.report_recovery_id,''), va.kind, va.original_ref, va.sha256, va.size_bytes, va.snapshot_path, va.verified_at,
		i.attached_by_driver_id, i.attached_at
		FROM task_report_inputs i
		JOIN verified_artifacts va ON va.id=i.artifact_id
		JOIN tasks p ON p.id=va.producer_task_id
		JOIN repos r ON r.id=p.repo_id
		WHERE i.target_task_id=? ORDER BY i.position`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	inputs := make([]model.ReportInput, 0)
	for rows.Next() {
		input, err := scanReportInput(rows)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	return inputs, rows.Err()
}

func scanReportInput(scanner interface{ Scan(...any) error }) (model.ReportInput, error) {
	var input model.ReportInput
	var verified, attached string
	err := scanner.Scan(&input.Position, &input.ArtifactID, &input.ProducerRepoID, &input.ProducerRepoName, &input.ProducerTaskID,
		&input.ProducerAttemptID, &input.DoneMessageID, &input.ReportRecoveryID, &input.Kind, &input.OriginalRef, &input.SHA256,
		&input.SizeBytes, &input.SnapshotPath, &verified, &input.AttachedByDriverID, &attached)
	if err != nil {
		return model.ReportInput{}, err
	}
	input.VerifiedAt, input.AttachedAt = parseTime(verified), parseTime(attached)
	return input, nil
}

func (s *Store) AddOutboundForRun(taskID, attemptID string, generation int, typ, payload string) (model.Message, error) {
	return s.addMessage(taskID, attemptID, "driver-to-worker", typ, payload, "", false, false, 0, generation, "")
}

type eventIngestionTarget struct {
	taskID          string
	taskStatus      string
	taskDeliverable string
	attemptBranch   string
	reportRef       string
}

func classifyStaleRunTx(tx *sql.Tx, attemptID string, generation int, cursor int64) (eventIngestionTarget, bool, error) {
	var target eventIngestionTarget
	var current string
	var currentGeneration int
	var released sql.NullString
	if err := tx.QueryRow(`SELECT a.task_id, COALESCE(t.current_attempt_id,''), t.status, t.deliverable,
		a.run_generation, a.released_at, a.branch,
		COALESCE((SELECT m.artifact_ref FROM messages m WHERE m.attempt_id=a.id AND m.run_generation=a.run_generation
			AND m.direction='system' AND m.type='report-destination'), '')
		FROM attempts a JOIN tasks t ON t.id=a.task_id WHERE a.id=?`, attemptID).
		Scan(&target.taskID, &current, &target.taskStatus, &target.taskDeliverable, &currentGeneration, &released, &target.attemptBranch, &target.reportRef); err != nil {
		return eventIngestionTarget{}, false, err
	}
	var latestCursor int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(source_cursor),0) FROM messages WHERE attempt_id=? AND run_generation=?`, attemptID, generation).Scan(&latestCursor); err != nil {
		return eventIngestionTarget{}, false, err
	}
	stale := current != attemptID || released.Valid || currentGeneration != generation || model.IsTerminalTaskStatus(target.taskStatus) || cursor <= latestCursor
	return target, stale, nil
}

func acceptedTerminalTx(tx *sql.Tx, attemptID string, generation int) (bool, error) {
	var accepted int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM messages WHERE attempt_id=? AND run_generation=? AND direction='worker-to-driver' AND type IN ('question','done','blocked','failed') AND stale=0)`, attemptID, generation).Scan(&accepted); err != nil {
		return false, err
	}
	return accepted != 0, nil
}

func checkpointGateTx(tx *sql.Tx, attemptID string, generation int, event model.Event, cursor int64) (string, string, *adapter.Diagnostic, error) {
	var checkpointCursor, previousWakeCursor int64
	var nextStepsJSON, checkpointBranch string
	checkpointErr := tx.QueryRow(`SELECT source_cursor, next_steps_json, branch FROM attempt_checkpoints WHERE attempt_id=? AND run_generation=? AND producer='worker'`, attemptID, generation).Scan(&checkpointCursor, &nextStepsJSON, &checkpointBranch)
	if checkpointErr != nil && !errors.Is(checkpointErr, sql.ErrNoRows) {
		return "", "", nil, checkpointErr
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(source_cursor),0) FROM messages WHERE attempt_id=? AND run_generation=? AND wake=1 AND stale=0`, attemptID, generation).Scan(&previousWakeCursor); err != nil {
		return "", "", nil, err
	}
	protocol := fmt.Sprintf("worker event %q requires a valid checkpoint in run generation %d", event.Type, generation)
	if errors.Is(checkpointErr, sql.ErrNoRows) {
		return checkpointBranch, protocol, &adapter.Diagnostic{Code: adapter.DiagnosticCheckpointRequired, Phase: "store", Message: "a current worker checkpoint is required", RequiresCheckpoint: true}, nil
	}
	if checkpointCursor <= previousWakeCursor || checkpointCursor >= cursor {
		return checkpointBranch, protocol, nil, nil
	}
	var nextSteps []string
	_ = json.Unmarshal([]byte(nextStepsJSON), &nextSteps)
	if len(nextSteps) == 0 && event.Type != "done" {
		return checkpointBranch, protocol, &adapter.Diagnostic{Code: adapter.DiagnosticCheckpointNextSteps, Phase: "store", Message: "the current checkpoint requires non-empty next_steps", RequiresCheckpoint: true}, nil
	}
	return checkpointBranch, "", nil, nil
}

func terminalArtifactContractFence(event model.Event, deliverable string) (string, *adapter.Diagnostic) {
	if event.Type != "done" {
		return "", nil
	}
	if err := model.ValidateDeliverableArtifact(deliverable, event.Artifact); err != nil {
		protocol := "worker done artifact violates the task's creation-time deliverable contract: " + err.Error()
		return protocol, &adapter.Diagnostic{Code: adapter.DiagnosticArtifactContract, Phase: "store", Field: "artifact", Message: "done artifact violates the task deliverable contract"}
	}
	return "", nil
}

func terminalBranchArtifactFence(event model.Event, attemptBranch, checkpointBranch string) (string, *adapter.Diagnostic) {
	if event.Type != "done" || !strings.HasPrefix(event.Artifact, "branch:") ||
		(attemptBranch != "" && event.Artifact == "branch:"+attemptBranch && checkpointBranch == attemptBranch) {
		return "", nil
	}
	protocol := fmt.Sprintf("worker done artifact %q does not name the exact attempt branch; a branch artifact must be branch:%s from the sealed checkpoint branch", event.Artifact, attemptBranch)
	return protocol, &adapter.Diagnostic{Code: adapter.DiagnosticBranchArtifact, Phase: "store", Field: "artifact", Message: "done artifact does not name the exact attempt branch"}
}

type EventRepairRequest struct {
	ID            string
	CandidateHash string
}

func (r EventRepairRequest) validate() error {
	decoded, err := hex.DecodeString(r.CandidateHash)
	validID := strings.HasPrefix(r.ID, "repair_") && len(r.ID) <= 128
	for _, char := range r.ID {
		validID = validID && (char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-')
	}
	if !validID || err != nil || len(decoded) != 32 || r.CandidateHash != strings.ToLower(r.CandidateHash) {
		return fmt.Errorf("event repair request identity is invalid")
	}
	return nil
}

func repairRequestTx(tx *sql.Tx, attemptID string, generation int) (EventRepairRequest, int64, bool, error) {
	rows, err := tx.Query(`SELECT source_cursor, payload FROM messages WHERE attempt_id=? AND run_generation=? AND direction='system' AND type='protocol-repair' ORDER BY id`, attemptID, generation)
	if err != nil {
		return EventRepairRequest{}, 0, false, err
	}
	defer rows.Close()
	var request EventRepairRequest
	var cursor int64
	count := 0
	for rows.Next() {
		var payload string
		if err := rows.Scan(&cursor, &payload); err != nil {
			return EventRepairRequest{}, 0, false, err
		}
		count++
		if count > 1 {
			return EventRepairRequest{}, 0, false, fmt.Errorf("run has multiple protocol repair requests")
		}
		for _, field := range strings.Fields(payload) {
			if value, found := strings.CutPrefix(field, "repair_id="); found {
				request.ID = strings.TrimSuffix(value, ";")
			}
			if value, found := strings.CutPrefix(field, "candidate_sha256="); found {
				request.CandidateHash = strings.TrimSuffix(value, ";")
			}
		}
	}
	if err := rows.Err(); err != nil {
		return EventRepairRequest{}, 0, false, err
	}
	if count == 0 {
		return EventRepairRequest{}, 0, false, nil
	}
	if err := request.validate(); err != nil {
		return EventRepairRequest{}, 0, false, err
	}
	return request, cursor, true, nil
}

func repairExistsTx(tx *sql.Tx, attemptID string, generation int) (bool, error) {
	_, _, exists, err := repairRequestTx(tx, attemptID, generation)
	return exists, err
}

func repairAuditPayload(cursor int64, request EventRepairRequest, diagnostic adapter.Diagnostic) string {
	return fmt.Sprintf("cursor %d rejected %s; correction 1/1 requested; repair_id=%s; candidate_sha256=%s", cursor, diagnostic.Code, request.ID, request.CandidateHash)
}

func recordRepairRequestTx(tx *sql.Tx, target eventIngestionTarget, attemptID string, generation int, event *model.Event, cursor int64, checkpointJSON string, request EventRepairRequest, diagnostic adapter.Diagnostic) (model.Message, error) {
	if event != nil {
		if _, err := insertMessage(tx, target.taskID, attemptID, "worker-to-driver", event.Type, event.Payload, event.Artifact, true, false, cursor, generation, checkpointJSON); err != nil {
			return model.Message{}, err
		}
	}
	message, err := insertMessage(tx, target.taskID, attemptID, "system", "protocol-repair", repairAuditPayload(cursor, request, diagnostic), "", false, false, cursor, generation, "")
	if err != nil {
		return model.Message{}, err
	}
	if _, err := tx.Exec(`UPDATE attempts SET cursor=MAX(cursor,?), updated_at=? WHERE id=? AND task_id=? AND run_generation=? AND status=?`, cursor, now(), attemptID, target.taskID, generation, model.AttemptStatusWorking); err != nil {
		return model.Message{}, err
	}
	return message, nil
}

func recordProtocolViolationTx(tx *sql.Tx, target eventIngestionTarget, attemptID string, generation int, event model.Event, cursor int64, checkpointJSON, protocol string) (model.Message, error) {
	if _, err := insertMessage(tx, target.taskID, attemptID, "worker-to-driver", event.Type, event.Payload, event.Artifact, true, false, cursor, generation, checkpointJSON); err != nil {
		return model.Message{}, err
	}
	protocol = model.WithWorkerRecoveryGuidance(protocol)
	blockedMessage, err := insertMessage(tx, target.taskID, attemptID, "system", "blocked", protocol, "", false, true, cursor, generation, "")
	if err != nil {
		return model.Message{}, err
	}
	if err := publishNotificationTx(tx, blockedMessage); err != nil {
		return model.Message{}, err
	}
	if err := setBlockedTx(tx, target.taskID, attemptID, generation, cursor, protocol); err != nil {
		return model.Message{}, err
	}
	return blockedMessage, nil
}

func transitionEventTx(tx *sql.Tx, target eventIngestionTarget, attemptID string, generation int, event model.Event, cursor int64) error {
	next := target.taskStatus
	attemptStatus := model.AttemptStatusWorking
	ended := any(nil)
	switch event.Type {
	case "question":
		next, attemptStatus = model.TaskStatusWaiting, model.AttemptStatusWaiting
	case "done":
		next, attemptStatus, ended = model.TaskStatusDone, model.AttemptStatusDone, now()
	case "blocked":
		next, attemptStatus, ended = model.TaskStatusBlocked, model.AttemptStatusBlocked, now()
	case "failed":
		next, attemptStatus, ended = model.TaskStatusFailed, model.AttemptStatusFailed, now()
	}
	if next != target.taskStatus {
		if err := model.ValidateTransition(target.taskStatus, next, false); err != nil {
			return err
		}
	}
	claimed := event.Type == "done"
	if _, err := tx.Exec(`UPDATE tasks SET status=?, artifact_ref=CASE WHEN ?='' THEN artifact_ref ELSE ? END,
		claimed_done=CASE WHEN ? THEN 1 ELSE claimed_done END,
		completion_provenance=CASE WHEN ? THEN ? ELSE completion_provenance END, updated_at=? WHERE id=? AND current_attempt_id=?`,
		next, event.Artifact, event.Artifact, claimed, claimed, model.CompletionProvenanceWorkerDone, now(), target.taskID, attemptID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE attempts SET status=?, cursor=?, updated_at=?, ended_at=COALESCE(?, ended_at) WHERE id=? AND task_id=? AND run_generation=?`,
		attemptStatus, cursor, now(), ended, attemptID, target.taskID, generation); err != nil {
		return err
	}
	return nil
}

func (s *Store) AddEventForRun(attemptID string, generation int, event model.Event, cursor int64, facts model.WorkspaceFacts) (model.Message, error) {
	message, _, err := s.addEventForRun(attemptID, generation, event, cursor, facts, nil)
	return message, err
}

func (s *Store) AddEventForRunRepairable(attemptID string, generation int, event model.Event, cursor int64, facts model.WorkspaceFacts, request EventRepairRequest) (model.Message, *adapter.Diagnostic, error) {
	if err := request.validate(); err != nil {
		return model.Message{}, nil, err
	}
	return s.addEventForRun(attemptID, generation, event, cursor, facts, &request)
}

func (s *Store) AddEventCandidateForRun(attemptID string, generation int, sessionID string, events []model.Event, cursor int64, facts model.WorkspaceFacts, request EventRepairRequest, correction bool) ([]model.Message, *adapter.Diagnostic, error) {
	if err := request.validate(); err != nil {
		return nil, nil, err
	}
	if len(events) == 0 {
		return nil, nil, fmt.Errorf("worker event candidate is empty")
	}
	terminal := false
	checkpointJSON := make([]string, len(events))
	for index := range events {
		events[index].Payload = strings.TrimSpace(events[index].Payload)
		if err := model.ValidateEvent(events[index]); err != nil {
			return nil, nil, err
		}
		if events[index].Checkpoint != nil {
			normalized := model.NormalizeCheckpoint(*events[index].Checkpoint)
			events[index].Checkpoint = &normalized
			encoded, err := model.CheckpointJSON(normalized)
			if err != nil {
				return nil, nil, err
			}
			checkpointJSON[index] = encoded
		}
		wake := events[index].Type == "question" || events[index].Type == "done" || events[index].Type == "blocked" || events[index].Type == "failed"
		if terminal || wake && index != len(events)-1 {
			return nil, nil, fmt.Errorf("%w: worker event candidate contains duplicate or post-terminal envelopes", ErrProtocolViolation)
		}
		terminal = wake
	}
	if correction && (len(events) != 2 || events[0].Type != "checkpoint" || !terminal) {
		return nil, nil, fmt.Errorf("%w: protocol correction must contain exactly one checkpoint followed by one terminal envelope", ErrProtocolViolation)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, stale, err := classifyStaleRunTx(tx, attemptID, generation, cursor); err != nil {
		return nil, nil, err
	} else if stale {
		return nil, nil, ErrStaleRun
	}
	storedRequest, repairCursor, repairExists, err := repairRequestTx(tx, attemptID, generation)
	if err != nil {
		return nil, nil, err
	}
	if correction {
		var storedSession string
		var storedCursor int64
		if err := tx.QueryRow(`SELECT session_id, cursor FROM attempts WHERE id=? AND run_generation=?`, attemptID, generation).Scan(&storedSession, &storedCursor); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, ErrStaleRun
			}
			return nil, nil, err
		}
		if !repairExists || storedRequest != request {
			return nil, nil, fmt.Errorf("%w: event repair candidate identity mismatch", ErrProtocolViolation)
		}
		if sessionID == "" || storedSession != sessionID {
			return nil, nil, fmt.Errorf("%w: event repair session is no longer current", ErrStaleRun)
		}
		if cursor <= repairCursor || cursor <= storedCursor {
			return nil, nil, fmt.Errorf("%w: event repair cursor is no longer current", ErrStaleRun)
		}
		accepted, err := acceptedTerminalTx(tx, attemptID, generation)
		if err != nil {
			return nil, nil, err
		}
		if accepted {
			return nil, nil, ErrStaleRun
		}
	} else if repairExists {
		return nil, nil, fmt.Errorf("%w: run already has a pending event repair", ErrProtocolViolation)
	}
	if _, err := tx.Exec(`SAVEPOINT event_candidate`); err != nil {
		return nil, nil, err
	}
	messages := make([]model.Message, 0, len(events))
	for index, event := range events {
		eventCursor := cursor + int64(index)
		target, stale, err := classifyStaleRunTx(tx, attemptID, generation, eventCursor)
		if err != nil {
			return nil, nil, err
		}
		if stale {
			return nil, nil, ErrStaleRun
		}
		wake := event.Type == "question" || event.Type == "done" || event.Type == "blocked" || event.Type == "failed"
		if wake {
			checkpointBranch, protocol, diagnostic, err := checkpointGateTx(tx, attemptID, generation, event, eventCursor)
			if err != nil {
				return nil, nil, err
			}
			if protocol == "" {
				protocol, diagnostic = terminalArtifactContractFence(event, target.taskDeliverable)
			}
			if protocol == "" {
				protocol, diagnostic = terminalReportArtifactFence(event, target.reportRef)
			}
			if protocol == "" {
				protocol, diagnostic = terminalBranchArtifactFence(event, target.attemptBranch, checkpointBranch)
			}
			if protocol != "" {
				if !correction && diagnostic != nil && diagnostic.Repairable() {
					if _, err := tx.Exec(`ROLLBACK TO event_candidate`); err != nil {
						return nil, nil, err
					}
					message, err := recordRepairRequestTx(tx, target, attemptID, generation, nil, eventCursor, "", request, *diagnostic)
					if err != nil {
						return nil, nil, err
					}
					if err := tx.Commit(); err != nil {
						return nil, nil, err
					}
					return []model.Message{message}, diagnostic, nil
				}
				return nil, nil, fmt.Errorf("%w: %s", ErrProtocolViolation, protocol)
			}
		}
		message, err := insertMessage(tx, target.taskID, attemptID, "worker-to-driver", event.Type, event.Payload, event.Artifact, false, wake, eventCursor, generation, checkpointJSON[index])
		if err != nil {
			return nil, nil, err
		}
		if event.Type == "checkpoint" {
			if err := replaceCheckpointTx(tx, attemptID, generation, eventCursor, event, facts, checkpointJSON[index]); err != nil {
				return nil, nil, err
			}
		} else if err := transitionEventTx(tx, target, attemptID, generation, event, eventCursor); err != nil {
			return nil, nil, err
		}
		if err := publishNotificationTx(tx, message); err != nil {
			return nil, nil, err
		}
		messages = append(messages, message)
	}
	lastCursor := cursor + int64(len(events)) - 1
	if _, err := tx.Exec(`UPDATE attempts SET cursor=MAX(cursor,?), updated_at=? WHERE id=? AND task_id=? AND run_generation=?`, lastCursor, now(), attemptID, messages[0].TaskID, generation); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return messages, nil, nil
}

func (s *Store) addEventForRun(attemptID string, generation int, event model.Event, cursor int64, facts model.WorkspaceFacts, repair *EventRepairRequest) (model.Message, *adapter.Diagnostic, error) {
	event.Payload = strings.TrimSpace(event.Payload)
	if err := model.ValidateEvent(event); err != nil {
		return model.Message{}, nil, err
	}
	if event.Checkpoint != nil {
		normalized := model.NormalizeCheckpoint(*event.Checkpoint)
		event.Checkpoint = &normalized
	}
	wake := event.Type == "question" || event.Type == "done" || event.Type == "blocked" || event.Type == "failed"
	checkpointJSON := ""
	var err error
	if event.Checkpoint != nil {
		checkpointJSON, err = model.CheckpointJSON(*event.Checkpoint)
		if err != nil {
			return model.Message{}, nil, err
		}
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	target, stale, err := classifyStaleRunTx(tx, attemptID, generation, cursor)
	if err != nil {
		return model.Message{}, nil, err
	}
	if repair != nil && !stale {
		storedRequest, _, exists, requestErr := repairRequestTx(tx, attemptID, generation)
		if requestErr != nil {
			return model.Message{}, nil, requestErr
		}
		if exists && storedRequest != *repair {
			return model.Message{}, nil, fmt.Errorf("event repair candidate identity mismatch")
		}
		stale, err = acceptedTerminalTx(tx, attemptID, generation)
		if err != nil {
			return model.Message{}, nil, err
		}
	}
	if stale {
		message, err := insertMessage(tx, target.taskID, attemptID, "worker-to-driver", event.Type, event.Payload, event.Artifact, true, false, cursor, generation, checkpointJSON)
		if err != nil {
			return model.Message{}, nil, err
		}
		if err := tx.Commit(); err != nil {
			return model.Message{}, nil, err
		}
		return message, nil, nil
	}
	if wake {
		checkpointBranch, protocol, diagnostic, err := checkpointGateTx(tx, attemptID, generation, event, cursor)
		if err != nil {
			return model.Message{}, nil, err
		}
		if protocol == "" {
			protocol, diagnostic = terminalArtifactContractFence(event, target.taskDeliverable)
		}
		if protocol == "" {
			protocol, diagnostic = terminalReportArtifactFence(event, target.reportRef)
		}
		if protocol == "" {
			protocol, diagnostic = terminalBranchArtifactFence(event, target.attemptBranch, checkpointBranch)
		}
		if protocol != "" {
			if repair != nil && diagnostic != nil {
				exists, err := repairExistsTx(tx, attemptID, generation)
				if err != nil {
					return model.Message{}, nil, err
				}
				if !exists {
					message, err := recordRepairRequestTx(tx, target, attemptID, generation, nil, cursor, "", *repair, *diagnostic)
					if err != nil {
						return model.Message{}, nil, err
					}
					if err := tx.Commit(); err != nil {
						return model.Message{}, nil, err
					}
					return message, diagnostic, nil
				}
			}
			blockedMessage, err := recordProtocolViolationTx(tx, target, attemptID, generation, event, cursor, checkpointJSON, protocol)
			if err != nil {
				return model.Message{}, nil, err
			}
			if err := tx.Commit(); err != nil {
				return model.Message{}, nil, err
			}
			return blockedMessage, nil, fmt.Errorf("%w: %s", ErrProtocolViolation, protocol)
		}
	}
	message, err := insertMessage(tx, target.taskID, attemptID, "worker-to-driver", event.Type, event.Payload, event.Artifact, false, wake, cursor, generation, checkpointJSON)
	if err != nil {
		return model.Message{}, nil, err
	}
	if event.Type == "checkpoint" {
		if err := replaceCheckpointTx(tx, attemptID, generation, cursor, event, facts, checkpointJSON); err != nil {
			return model.Message{}, nil, err
		}
	} else if err := transitionEventTx(tx, target, attemptID, generation, event, cursor); err != nil {
		return model.Message{}, nil, err
	}
	if err := publishNotificationTx(tx, message); err != nil {
		return model.Message{}, nil, err
	}
	if _, err := tx.Exec(`UPDATE attempts SET cursor=MAX(cursor,?), updated_at=? WHERE id=? AND task_id=? AND run_generation=?`, cursor, now(), attemptID, target.taskID, generation); err != nil {
		return model.Message{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return model.Message{}, nil, err
	}
	return message, nil, nil
}

func (s *Store) RecordAdapterRejectionForRun(attemptID string, generation int, cursor int64, request EventRepairRequest, diagnostic adapter.Diagnostic) (model.Message, *adapter.Diagnostic, error) {
	if err := request.validate(); err != nil {
		return model.Message{}, nil, err
	}
	if !diagnostic.Repairable() {
		return model.Message{}, nil, fmt.Errorf("adapter diagnostic %s is not repairable", diagnostic.Code)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	target, stale, err := classifyStaleRunTx(tx, attemptID, generation, cursor)
	if err != nil {
		return model.Message{}, nil, err
	}
	if !stale {
		stale, err = acceptedTerminalTx(tx, attemptID, generation)
		if err != nil {
			return model.Message{}, nil, err
		}
	}
	if stale {
		return model.Message{}, nil, ErrStaleRun
	}
	storedRequest, _, exists, err := repairRequestTx(tx, attemptID, generation)
	if err != nil {
		return model.Message{}, nil, err
	}
	if exists && storedRequest != request {
		return model.Message{}, nil, fmt.Errorf("event repair candidate identity mismatch")
	}
	if !exists {
		message, err := recordRepairRequestTx(tx, target, attemptID, generation, nil, cursor, "", request, diagnostic)
		if err != nil {
			return model.Message{}, nil, err
		}
		if err := tx.Commit(); err != nil {
			return model.Message{}, nil, err
		}
		return message, &diagnostic, nil
	}
	protocol := model.WithWorkerRecoveryGuidance(fmt.Sprintf("second Pi event rejection in run generation %d: %s", generation, diagnostic.Code))
	message, err := insertMessage(tx, target.taskID, attemptID, "system", "blocked", protocol, "", false, true, cursor, generation, "")
	if err != nil {
		return model.Message{}, nil, err
	}
	if err := publishNotificationTx(tx, message); err != nil {
		return model.Message{}, nil, err
	}
	if err := setBlockedTx(tx, target.taskID, attemptID, generation, cursor, protocol); err != nil {
		return model.Message{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return model.Message{}, nil, err
	}
	return message, nil, fmt.Errorf("%w: %s", ErrProtocolViolation, protocol)
}

func (s *Store) addMessage(taskID, attemptID, direction, typ, payload, artifact string, stale, wake bool, cursor int64, generation int, checkpointJSON string) (model.Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return model.Message{}, err
	}
	defer tx.Rollback()
	message, err := insertMessage(tx, taskID, attemptID, direction, typ, payload, artifact, stale, wake, cursor, generation, checkpointJSON)
	if err != nil {
		return model.Message{}, err
	}
	return message, tx.Commit()
}

func insertMessage(tx *sql.Tx, taskID, attemptID, direction, typ, payload, artifact string, stale, wake bool, cursor int64, generation int, checkpointJSON string) (model.Message, error) {
	t := time.Now().UTC()
	result, err := tx.Exec(`INSERT INTO messages(task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake, source_cursor, run_generation, checkpoint_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, taskID, attemptID, direction, typ, payload, artifact, stale, wake, cursor, generation, checkpointJSON, stamp(t))
	if err != nil {
		return model.Message{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Message{}, err
	}
	return model.Message{ID: id, TaskID: taskID, AttemptID: attemptID, Direction: direction, Type: typ, Payload: payload,
		ArtifactRef: artifact, RunGeneration: generation, CheckpointJSON: checkpointJSON, Stale: stale, Wake: wake, SourceCursor: cursor, CreatedAt: t}, nil
}

func (s *Store) Messages(taskID string) ([]model.Message, error) {
	rows, err := s.db.Query(`SELECT id, task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake,
		source_cursor, run_generation, checkpoint_json, created_at FROM messages WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]model.Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func scanMessage(scanner interface{ Scan(...any) error }) (model.Message, error) {
	var message model.Message
	var stale, wake int
	var created string
	err := scanner.Scan(&message.ID, &message.TaskID, &message.AttemptID, &message.Direction, &message.Type,
		&message.Payload, &message.ArtifactRef, &stale, &wake, &message.SourceCursor, &message.RunGeneration, &message.CheckpointJSON, &created)
	if err != nil {
		return model.Message{}, err
	}
	message.Stale, message.Wake = stale != 0, wake != 0
	message.CreatedAt = parseTime(created)
	return message, nil
}

func (s *Store) AcceptedDoneMessage(taskID, attemptID string) (model.Message, error) {
	message, err := scanMessage(s.db.QueryRow(`SELECT id, task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake,
		source_cursor, run_generation, checkpoint_json, created_at FROM messages
		WHERE task_id=? AND attempt_id=? AND direction='worker-to-driver' AND type='done' AND stale=0
		ORDER BY id DESC LIMIT 1`, taskID, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, fmt.Errorf("producer task %s has no accepted done message for attempt %s", taskID, attemptID)
	}
	return message, err
}

const verifiedArtifactSelect = `SELECT id, producer_task_id, producer_attempt_id, done_message_id, COALESCE(report_recovery_id,''), kind, original_ref, sha256, size_bytes, snapshot_path, verified_at, accepted_event_id, accepted_event_name, accepted_event_version FROM verified_artifacts`

func (s *Store) verifiedArtifactSelect() string {
	return verifiedArtifactSelect
}

func (s *Store) VerifiedArtifactForDoneMessage(doneMessageID int64) (*model.VerifiedArtifact, error) {
	artifact, err := scanVerifiedArtifact(s.db.QueryRow(s.verifiedArtifactSelect()+` WHERE done_message_id=?`, doneMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (s *Store) EligibleVerifiedReport(producerTaskID string) (model.VerifiedArtifact, error) {
	task, err := s.Task(producerTaskID)
	if err != nil {
		return model.VerifiedArtifact{}, fmt.Errorf("producer %w", err)
	}
	if task.Deliverable != "report" {
		return model.VerifiedArtifact{}, fmt.Errorf("producer task %s deliverable is %s, not report", task.ID, task.Deliverable)
	}
	if task.Status != model.TaskStatusDone || task.CompletionProvenance != model.CompletionProvenanceWorkerDone && task.CompletionProvenance != model.CompletionProvenanceReportRecovery {
		return model.VerifiedArtifact{}, fmt.Errorf("producer task %s must have an accepted done or verified driver-recovered completion before its report can be attached", task.ID)
	}
	if err := model.ValidateDeliverableArtifact(task.Deliverable, task.ArtifactRef); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if !task.Landed {
		return model.VerifiedArtifact{}, fmt.Errorf("producer task %s has no verified report artifact; run 'shephrd task verify-delivery %s' after it reaches done", task.ID, task.ID)
	}
	if task.CurrentAttemptID == "" {
		return model.VerifiedArtifact{}, fmt.Errorf("producer task %s has no current attempt", task.ID)
	}
	attempt, err := s.Attempt(task.CurrentAttemptID)
	if err != nil {
		return model.VerifiedArtifact{}, err
	}
	if !attempt.LandedProven {
		return model.VerifiedArtifact{}, fmt.Errorf("producer task %s current attempt has no landed proof", task.ID)
	}
	ids, err := func() ([]string, error) {
		tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		return eligibleVerifiedReportIDsTx(tx, task.ID)
	}()
	if err != nil {
		return model.VerifiedArtifact{}, err
	}
	if len(ids) != 1 {
		return model.VerifiedArtifact{}, fmt.Errorf("producer task %s has no unique eligible verified report artifact", task.ID)
	}
	artifact, err := scanVerifiedArtifact(s.db.QueryRow(s.verifiedArtifactSelect()+` WHERE id=?`, ids[0]))
	if err != nil {
		return model.VerifiedArtifact{}, err
	}
	return artifact, nil
}

func scanVerifiedArtifact(scanner interface{ Scan(...any) error }) (model.VerifiedArtifact, error) {
	var artifact model.VerifiedArtifact
	var verified string
	err := scanner.Scan(&artifact.ID, &artifact.ProducerTaskID, &artifact.ProducerAttemptID, &artifact.DoneMessageID, &artifact.ReportRecoveryID,
		&artifact.Kind, &artifact.OriginalRef, &artifact.SHA256, &artifact.SizeBytes, &artifact.SnapshotPath, &verified,
		&artifact.AcceptedEventID, &artifact.AcceptedEventName, &artifact.AcceptedEventVersion)
	if err != nil {
		return model.VerifiedArtifact{}, err
	}
	artifact.VerifiedAt = parseTime(verified)
	return artifact, nil
}

func (s *Store) SetVerifiedReportDelivery(taskID, attemptID string, doneMessageID int64, artifact model.VerifiedArtifact, reason string, handlers ...model.ReportAcceptedHandlerBinding) (model.VerifiedArtifact, error) {
	if artifact.Kind != "report" || artifact.OriginalRef == "" || len(artifact.SHA256) != 64 || strings.ToLower(artifact.SHA256) != artifact.SHA256 || artifact.SizeBytes < 0 || artifact.SnapshotPath == "" {
		return model.VerifiedArtifact{}, fmt.Errorf("verified report artifact metadata is incomplete")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.VerifiedArtifact{}, err
	}
	defer tx.Rollback()
	var taskAttemptID, taskStatus, taskArtifactRef, taskDeliverable string
	var claimed int
	if err := tx.QueryRow(`SELECT COALESCE(current_attempt_id,''), status, artifact_ref, claimed_done, deliverable FROM tasks WHERE id=?`, taskID).
		Scan(&taskAttemptID, &taskStatus, &taskArtifactRef, &claimed, &taskDeliverable); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if taskDeliverable != "report" || model.ValidateDeliverableArtifact(taskDeliverable, taskArtifactRef) != nil || taskAttemptID != attemptID || taskStatus != model.TaskStatusDone || claimed == 0 || taskArtifactRef != artifact.OriginalRef {
		return model.VerifiedArtifact{}, fmt.Errorf("task %s report result changed before verification completed", taskID)
	}
	var attemptTaskID, attemptStatus string
	if err := tx.QueryRow(`SELECT task_id, status FROM attempts WHERE id=?`, attemptID).Scan(&attemptTaskID, &attemptStatus); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if attemptTaskID != taskID || attemptStatus != model.AttemptStatusDone {
		return model.VerifiedArtifact{}, fmt.Errorf("task %s report result changed before verification completed", taskID)
	}
	var messageAttemptID, messageArtifactRef, messageType string
	var stale int
	if err := tx.QueryRow(`SELECT attempt_id, artifact_ref, type, stale FROM messages WHERE id=? AND task_id=?`, doneMessageID, taskID).
		Scan(&messageAttemptID, &messageArtifactRef, &messageType, &stale); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if messageAttemptID != attemptID || messageType != "done" || stale != 0 || messageArtifactRef != artifact.OriginalRef {
		return model.VerifiedArtifact{}, fmt.Errorf("task %s accepted done message changed before verification completed", taskID)
	}
	if artifact.ID == "" {
		artifact.ID = NewID("artifact")
	}
	artifact.ProducerTaskID, artifact.ProducerAttemptID, artifact.DoneMessageID = taskID, attemptID, doneMessageID
	if artifact.VerifiedAt.IsZero() {
		artifact.VerifiedAt = time.Now().UTC()
	}
	existing, existingErr := scanVerifiedArtifact(tx.QueryRow(s.verifiedArtifactSelect()+` WHERE done_message_id=?`, doneMessageID))
	if existingErr == nil {
		if existing.ProducerTaskID != artifact.ProducerTaskID || existing.ProducerAttemptID != artifact.ProducerAttemptID || existing.Kind != artifact.Kind || existing.OriginalRef != artifact.OriginalRef || existing.SHA256 != artifact.SHA256 || existing.SizeBytes != artifact.SizeBytes || existing.SnapshotPath != artifact.SnapshotPath {
			return model.VerifiedArtifact{}, fmt.Errorf("task %s verified report metadata is immutable and does not match the existing snapshot", taskID)
		}
		artifact = existing
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return model.VerifiedArtifact{}, existingErr
	} else {
		prepareReportAcceptedArtifact(&artifact)
		if _, err := tx.Exec(`INSERT INTO verified_artifacts(id, producer_task_id, producer_attempt_id, done_message_id, kind,
			original_ref, sha256, size_bytes, snapshot_path, verified_at, accepted_event_id, accepted_event_name, accepted_event_version)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, artifact.ID, artifact.ProducerTaskID, artifact.ProducerAttemptID,
			artifact.DoneMessageID, artifact.Kind, artifact.OriginalRef, artifact.SHA256, artifact.SizeBytes, artifact.SnapshotPath,
			stamp(artifact.VerifiedAt), artifact.AcceptedEventID, artifact.AcceptedEventName, artifact.AcceptedEventVersion); err != nil {
			return model.VerifiedArtifact{}, err
		}
		if err := insertReportLifecycleInvocationsTx(tx, artifact, handlers); err != nil {
			return model.VerifiedArtifact{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE attempts SET landed_proven=1, landing_kind='report_artifact',
		landed_verified_at=COALESCE(landed_verified_at,?), landing_reason=CASE WHEN landing_reason='' THEN ? ELSE landing_reason END,
		updated_at=? WHERE id=? AND task_id=?`, stamp(artifact.VerifiedAt), reason, now(), attemptID, taskID); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if _, err := tx.Exec(`UPDATE tasks SET branch_pushed=0, remote_delivery_state='unverified', pr_state='', updated_at=? WHERE id=?`, now(), taskID); err != nil {
		return model.VerifiedArtifact{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.VerifiedArtifact{}, err
	}
	return artifact, nil
}

func (s *Store) LatestCheckpoint(attemptID string) (model.AttemptCheckpoint, error) {
	return scanCheckpoint(s.db.QueryRow(`SELECT attempt_id, revision, schema_version, producer, run_generation, source_cursor,
		session_id, branch, head_commit, worktree_dirty, workspace_facts_error, summary, completed_json, next_steps_json,
		decisions_json, changed_paths_json, checks_json, blockers_json, source_attempt_id, source_revision, captured_at
		FROM attempt_checkpoints WHERE attempt_id=?`, attemptID))
}

func (s *Store) RecordSystemCheckpoint(attemptID string, generation int, summary string, nextSteps []string, facts model.WorkspaceFacts) (model.AttemptCheckpoint, error) {
	checkpoint := model.Checkpoint{SchemaVersion: model.CheckpointSchemaVersion, Summary: summary, NextSteps: nextSteps}
	checkpoint = model.NormalizeCheckpoint(checkpoint)
	if err := model.ValidateCheckpoint(checkpoint); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.AttemptCheckpoint{}, err
	}
	defer tx.Rollback()
	var attempt model.Attempt
	var released sql.NullString
	if err := tx.QueryRow(`SELECT id, run_generation, COALESCE(resume_source_attempt_id,''), resume_source_revision,
		session_id, branch, released_at FROM attempts WHERE id=?`, attemptID).Scan(&attempt.ID, &attempt.RunGeneration,
		&attempt.ResumeSourceAttemptID, &attempt.ResumeSourceRevision, &attempt.SessionID, &attempt.Branch, &released); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if released.Valid || attempt.RunGeneration != generation {
		return model.AttemptCheckpoint{}, ErrStaleRun
	}
	if existing, err := scanCheckpoint(tx.QueryRow(`SELECT attempt_id, revision, schema_version, producer, run_generation, source_cursor,
		session_id, branch, head_commit, worktree_dirty, workspace_facts_error, summary, completed_json, next_steps_json,
		decisions_json, changed_paths_json, checks_json, blockers_json, source_attempt_id, source_revision, captured_at
		FROM attempt_checkpoints WHERE attempt_id=?`, attemptID)); err == nil {
		return existing, tx.Commit()
	}
	if attempt.ResumeSourceAttemptID != "" {
		if source, sourceErr := scanCheckpoint(tx.QueryRow(`SELECT attempt_id, revision, schema_version, producer, run_generation, source_cursor,
			session_id, branch, head_commit, worktree_dirty, workspace_facts_error, summary, completed_json, next_steps_json,
			decisions_json, changed_paths_json, checks_json, blockers_json, source_attempt_id, source_revision, captured_at
			FROM attempt_checkpoints WHERE attempt_id=?`, attempt.ResumeSourceAttemptID)); sourceErr == nil && source.Revision == attempt.ResumeSourceRevision {
			checkpoint = model.Checkpoint{SchemaVersion: source.SchemaVersion, Summary: source.Summary, Completed: source.Completed, NextSteps: source.NextSteps,
				Decisions: source.Decisions, ChangedPaths: source.ChangedPaths, Checks: source.Checks, Blockers: source.Blockers}
			checkpoint = model.NormalizeCheckpoint(checkpoint)
			attemptSource := attempt.ResumeSourceAttemptID
			stored, err := insertCheckpointTx(tx, attemptID, 1, "system", generation, 0, attempt.SessionID, attempt.Branch, facts, checkpoint, attemptSource, source.Revision)
			if err != nil {
				return model.AttemptCheckpoint{}, err
			}
			if err := insertSystemCheckpointMessage(tx, attemptID, generation, stored); err != nil {
				return model.AttemptCheckpoint{}, err
			}
			if err := tx.Commit(); err != nil {
				return model.AttemptCheckpoint{}, err
			}
			return stored, nil
		}
	}
	stored, err := insertCheckpointTx(tx, attemptID, 1, "system", generation, 0, attempt.SessionID, attempt.Branch, facts, checkpoint, "", 0)
	if err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := insertSystemCheckpointMessage(tx, attemptID, generation, stored); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	return stored, nil
}

func insertSystemCheckpointMessage(tx *sql.Tx, attemptID string, generation int, checkpoint model.AttemptCheckpoint) error {
	encoded, err := model.CheckpointJSON(model.Checkpoint{SchemaVersion: checkpoint.SchemaVersion, Summary: checkpoint.Summary, Completed: checkpoint.Completed,
		NextSteps: checkpoint.NextSteps, Decisions: checkpoint.Decisions, ChangedPaths: checkpoint.ChangedPaths, Checks: checkpoint.Checks, Blockers: checkpoint.Blockers})
	if err != nil {
		return err
	}
	var taskID string
	if err := tx.QueryRow(`SELECT task_id FROM attempts WHERE id=?`, attemptID).Scan(&taskID); err != nil {
		return err
	}
	_, err = insertMessage(tx, taskID, attemptID, "system", "checkpoint", checkpoint.Summary, "", false, false, checkpoint.SourceCursor, generation, encoded)
	return err
}

func replaceCheckpointTx(tx *sql.Tx, attemptID string, generation int, cursor int64, event model.Event, facts model.WorkspaceFacts, checkpointJSON string) error {
	var sessionID, branch string
	if err := tx.QueryRow(`SELECT session_id, branch FROM attempts WHERE id=? AND run_generation=?`, attemptID, generation).Scan(&sessionID, &branch); err != nil {
		return err
	}
	var checkpoint model.Checkpoint
	if err := json.Unmarshal([]byte(checkpointJSON), &checkpoint); err != nil {
		return err
	}
	_, err := insertCheckpointTx(tx, attemptID, 0, "worker", generation, cursor, sessionID, branch, facts, checkpoint, "", 0)
	return err
}

func insertCheckpointTx(tx *sql.Tx, attemptID string, revision int, producer string, generation int, cursor int64, sessionID, branch string, facts model.WorkspaceFacts, checkpoint model.Checkpoint, sourceAttemptID string, sourceRevision int) (model.AttemptCheckpoint, error) {
	checkpoint = model.NormalizeCheckpoint(checkpoint)
	allowEmptyNextSteps := producer == "worker" || sourceAttemptID != ""
	if err := model.ValidateCheckpointState(checkpoint, allowEmptyNextSteps); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if revision == 0 {
		if err := tx.QueryRow(`SELECT COALESCE(MAX(revision),0)+1 FROM attempt_checkpoints WHERE attempt_id=?`, attemptID).Scan(&revision); err != nil {
			return model.AttemptCheckpoint{}, err
		}
	}
	completed, _ := json.Marshal(checkpoint.Completed)
	nextSteps, _ := json.Marshal(checkpoint.NextSteps)
	decisions, _ := json.Marshal(checkpoint.Decisions)
	changedPaths, _ := json.Marshal(checkpoint.ChangedPaths)
	checks, _ := json.Marshal(checkpoint.Checks)
	blockers, _ := json.Marshal(checkpoint.Blockers)
	captured := time.Now().UTC()
	_, err := tx.Exec(`INSERT INTO attempt_checkpoints(attempt_id, revision, schema_version, producer, run_generation, source_cursor,
		session_id, branch, head_commit, worktree_dirty, workspace_facts_error, summary, completed_json, next_steps_json,
		decisions_json, changed_paths_json, checks_json, blockers_json, source_attempt_id, source_revision, captured_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(attempt_id) DO UPDATE SET revision=excluded.revision, schema_version=excluded.schema_version,
		producer=excluded.producer, run_generation=excluded.run_generation, source_cursor=excluded.source_cursor,
		session_id=excluded.session_id, branch=excluded.branch, head_commit=excluded.head_commit,
		worktree_dirty=excluded.worktree_dirty, workspace_facts_error=excluded.workspace_facts_error,
		summary=excluded.summary, completed_json=excluded.completed_json, next_steps_json=excluded.next_steps_json,
		decisions_json=excluded.decisions_json, changed_paths_json=excluded.changed_paths_json, checks_json=excluded.checks_json,
		blockers_json=excluded.blockers_json, source_attempt_id=excluded.source_attempt_id, source_revision=excluded.source_revision,
		captured_at=excluded.captured_at`, attemptID, revision, checkpoint.SchemaVersion, producer, generation, cursor, sessionID, branch,
		facts.HeadCommit, facts.Dirty, facts.Error, checkpoint.Summary, completed, nextSteps, decisions, changedPaths, checks, blockers,
		sourceAttemptID, sourceRevision, stamp(captured))
	if err != nil {
		return model.AttemptCheckpoint{}, err
	}
	return model.AttemptCheckpoint{AttemptID: attemptID, Revision: revision, SchemaVersion: checkpoint.SchemaVersion, Producer: producer,
		RunGeneration: generation, SourceCursor: cursor, SessionID: sessionID, Branch: branch, HeadCommit: facts.HeadCommit,
		WorktreeDirty: facts.Dirty, WorkspaceFactsError: facts.Error, Summary: checkpoint.Summary, Completed: checkpoint.Completed,
		NextSteps: checkpoint.NextSteps, Decisions: checkpoint.Decisions, ChangedPaths: checkpoint.ChangedPaths, Checks: checkpoint.Checks,
		Blockers: checkpoint.Blockers, SourceAttemptID: sourceAttemptID, SourceRevision: sourceRevision, CapturedAt: captured}, nil
}

func scanCheckpoint(scanner interface{ Scan(...any) error }) (model.AttemptCheckpoint, error) {
	var checkpoint model.AttemptCheckpoint
	var dirty int
	var completed, nextSteps, decisions, changedPaths, checks, blockers, captured string
	if err := scanner.Scan(&checkpoint.AttemptID, &checkpoint.Revision, &checkpoint.SchemaVersion, &checkpoint.Producer, &checkpoint.RunGeneration,
		&checkpoint.SourceCursor, &checkpoint.SessionID, &checkpoint.Branch, &checkpoint.HeadCommit, &dirty, &checkpoint.WorkspaceFactsError,
		&checkpoint.Summary, &completed, &nextSteps, &decisions, &changedPaths, &checks, &blockers, &checkpoint.SourceAttemptID,
		&checkpoint.SourceRevision, &captured); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(completed), &checkpoint.Completed); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(nextSteps), &checkpoint.NextSteps); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(decisions), &checkpoint.Decisions); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(changedPaths), &checkpoint.ChangedPaths); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(checks), &checkpoint.Checks); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	if err := json.Unmarshal([]byte(blockers), &checkpoint.Blockers); err != nil {
		return model.AttemptCheckpoint{}, err
	}
	checkpoint.WorktreeDirty = dirty != 0
	checkpoint.CapturedAt = parseTime(captured)
	return checkpoint, nil
}

func setBlockedTx(tx *sql.Tx, taskID, attemptID string, generation int, cursor int64, reason string) error {
	if _, err := tx.Exec(`UPDATE tasks SET status=?, process_alive=0, updated_at=? WHERE current_attempt_id=? AND id=?`, model.TaskStatusBlocked, now(), attemptID, taskID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE attempts SET status=?, runner_pid=0, cursor=MAX(cursor,?), failure_reason=?, ended_at=COALESCE(ended_at,?), updated_at=? WHERE id=? AND task_id=? AND run_generation=?`, model.AttemptStatusBlocked, cursor, reason, now(), now(), attemptID, taskID, generation)
	return err
}

func (s *Store) RecordControlFailureMessage(attemptID string, generation int, reason string) (model.Message, error) {
	return s.recordControlEventMessage(attemptID, generation, "blocked", reason)
}

func (s *Store) recordControlEventMessage(attemptID string, generation int, typ, payload string) (model.Message, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Message{}, err
	}
	defer tx.Rollback()
	var taskID, current, taskStatus string
	var cursor int64
	var acceptedTerminal int
	if err := tx.QueryRow(`SELECT a.task_id, COALESCE(t.current_attempt_id,''), t.status, a.cursor,
		EXISTS(SELECT 1 FROM messages m WHERE m.attempt_id=a.id AND m.run_generation=a.run_generation
			AND m.direction='worker-to-driver' AND m.type IN ('question','done','blocked','failed') AND m.stale=0)
		FROM attempts a JOIN tasks t ON t.id=a.task_id WHERE a.id=? AND a.run_generation=?`, attemptID, generation).
		Scan(&taskID, &current, &taskStatus, &cursor, &acceptedTerminal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Message{}, ErrStaleRun
		}
		return model.Message{}, err
	}
	stale := current != attemptID || acceptedTerminal != 0 || model.IsTerminalTaskStatus(taskStatus)
	if !stale {
		payload = model.WithWorkerRecoveryGuidance(payload)
	}
	message, err := insertMessage(tx, taskID, attemptID, "system", typ, payload, "", stale, !stale, cursor+1, generation, "")
	if err != nil {
		return model.Message{}, err
	}
	if !stale {
		if err := setBlockedTx(tx, taskID, attemptID, generation, cursor+1, payload); err != nil {
			return model.Message{}, err
		}
		if err := publishNotificationTx(tx, message); err != nil {
			return model.Message{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Message{}, err
	}
	return message, nil
}

func (s *Store) RecordRunnerDeathMessage(attemptID string, generation int, reason string) (model.Message, error) {
	return s.recordControlEventMessage(attemptID, generation, "blocked", reason)
}

func (s *Store) ReserveRunGeneration(attemptID string) (int, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	_, generation, err := reserveRunGenerationTx(tx, attemptID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return generation, nil
}

func (s *Store) ReserveFollowUpRun(taskID, attemptID string, generation int, payload string) (model.Message, int, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Message{}, 0, err
	}
	defer tx.Rollback()
	storedTaskID, nextGeneration, err := reserveRunGenerationTx(tx, attemptID)
	if err != nil {
		return model.Message{}, 0, err
	}
	if storedTaskID != taskID || nextGeneration != generation+1 {
		return model.Message{}, 0, ErrStaleRun
	}
	message, err := insertMessage(tx, taskID, attemptID, "driver-to-worker", "follow-up", payload, "", false, false, 0, generation, "")
	if err != nil {
		return model.Message{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return model.Message{}, 0, err
	}
	return message, nextGeneration, nil
}

func reserveRunGenerationTx(tx *sql.Tx, attemptID string) (string, int, error) {
	if err := guardReportRecoveryContinuationTx(tx, attemptID); err != nil {
		return "", 0, err
	}
	var taskID string
	var generation, runnerPID int
	var attemptStatus string
	var released sql.NullString
	if err := tx.QueryRow(`SELECT task_id, run_generation, runner_pid, status, released_at FROM attempts WHERE id=?`, attemptID).Scan(&taskID, &generation, &runnerPID, &attemptStatus, &released); err != nil {
		return "", 0, err
	}
	if released.Valid {
		return "", 0, fmt.Errorf("attempt %s has been released", attemptID)
	}
	if runnerPID != 0 {
		return "", 0, fmt.Errorf("attempt %s still has a recorded runner; confirm it is dead before reserving a generation", attemptID)
	}
	if generation > 0 && attemptStatus == model.AttemptStatusStarting {
		return "", 0, fmt.Errorf("attempt %s already has a reserved run generation", attemptID)
	}
	if _, err := supersedeNotificationsTx(tx, `d.attempt_id=? AND d.worker_run_generation=?`, []any{attemptID, generation}, "worker run generation replaced"); err != nil {
		return "", 0, err
	}
	generation++
	if _, err := tx.Exec(`UPDATE attempts SET run_generation=?, status=?, runner_pid=0, runtime_executable='', updated_at=? WHERE id=?`, generation, model.AttemptStatusStarting, now(), attemptID); err != nil {
		return "", 0, err
	}
	if _, err := tx.Exec(`UPDATE tasks SET status=CASE WHEN status IN (?,?,?,?) THEN ? ELSE status END, process_alive=0, archived_at=NULL, updated_at=? WHERE current_attempt_id=?`,
		model.TaskStatusWaiting, model.TaskStatusBlocked, model.TaskStatusFailed, model.TaskStatusStopped, model.TaskStatusWorking, now(), attemptID); err != nil {
		return "", 0, err
	}
	return taskID, generation, nil
}

func (s *Store) SetRuntimeExecutableForRun(attemptID string, generation int, executable string) error {
	if strings.TrimSpace(executable) == "" {
		return fmt.Errorf("runtime executable must not be empty")
	}
	result, err := s.db.Exec(`UPDATE attempts SET runtime_executable=?, updated_at=? WHERE id=? AND run_generation=? AND released_at IS NULL`, executable, now(), attemptID, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	return nil
}

func (s *Store) SetRunnerForRun(attemptID string, generation, pid int, alive bool) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	result, err := tx.Exec(`UPDATE attempts SET runner_pid=?, status=CASE WHEN ? THEN ? ELSE status END, updated_at=? WHERE id=? AND run_generation=? AND released_at IS NULL`, pid, alive, model.AttemptStatusWorking, t, attemptID, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	taskResult, err := tx.Exec(`UPDATE tasks SET process_alive=?, status=CASE WHEN ? AND status=? THEN ? ELSE status END, updated_at=? WHERE current_attempt_id=?`, alive, alive, model.TaskStatusWaiting, model.TaskStatusWorking, t, attemptID)
	if err != nil {
		return err
	}
	if n, _ := taskResult.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	return tx.Commit()
}

func (s *Store) ClearRunnerForRun(attemptID string, generation int) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	result, err := tx.Exec(`UPDATE attempts SET runner_pid=0, updated_at=? WHERE id=? AND run_generation=?`, t, attemptID, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	taskResult, err := tx.Exec(`UPDATE tasks SET process_alive=0, updated_at=? WHERE current_attempt_id=?`, t, attemptID)
	if err != nil {
		return err
	}
	if n, _ := taskResult.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	return tx.Commit()
}

func (s *Store) SetSessionForRun(attemptID string, generation int, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	result, err := s.db.Exec(`UPDATE attempts SET session_id=?, updated_at=? WHERE id=? AND run_generation=? AND released_at IS NULL`, sessionID, now(), attemptID, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	return nil
}

func (s *Store) FinishRunnerForRun(attemptID string, generation, exitCode int, reason string) error {
	return s.finishRunnerForRun(attemptID, generation, exitCode, reason, "process_exited")
}

func (s *Store) FinishDeadRunnerForRun(attemptID string, generation int) error {
	return s.finishRunnerForRun(attemptID, generation, -1, "", "swept_dead")
}

func (s *Store) finishRunnerForRun(attemptID string, generation, exitCode int, reason, settlementReason string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireImmediateTransactionLock(tx); err != nil {
		return err
	}
	var runnerPID, processAlive int
	if err := tx.QueryRow(`SELECT a.runner_pid, t.process_alive FROM attempts a JOIN tasks t ON t.current_attempt_id=a.id WHERE a.id=? AND a.run_generation=?`, attemptID, generation).Scan(&runnerPID, &processAlive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStaleRun
		}
		return err
	}
	t := now()
	result, err := tx.Exec(`UPDATE attempts SET runner_pid=0, exit_code=?, failure_reason=CASE WHEN ?='' THEN failure_reason ELSE ? END, updated_at=? WHERE id=? AND run_generation=?`,
		exitCode, reason, reason, t, attemptID, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	taskResult, err := tx.Exec(`UPDATE tasks SET process_alive=0, updated_at=? WHERE current_attempt_id=?`, t, attemptID)
	if err != nil {
		return err
	}
	if n, _ := taskResult.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	if runnerPID != 0 || processAlive != 0 {
		if err := publishSettledNotificationTx(tx, attemptID, generation, settlementReason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateCursorForRun(attemptID string, generation int, cursor int64) error {
	result, err := s.db.Exec(`UPDATE attempts SET cursor=MAX(cursor,?), updated_at=? WHERE id=? AND run_generation=? AND released_at IS NULL`, cursor, now(), attemptID, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrStaleRun
	}
	return nil
}

func (s *Store) PrepareRetry(taskID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, current string
	if err := tx.QueryRow(`SELECT status, COALESCE(current_attempt_id,'') FROM tasks WHERE id=?`, taskID).Scan(&status, &current); err != nil {
		return err
	}
	if status == model.TaskStatusStarting || status == model.TaskStatusWorking {
		return fmt.Errorf("task %s is still %s; stop it before retrying", taskID, status)
	}
	if current == "" {
		return fmt.Errorf("task %s has no attempt to retry; run worker spawn instead", taskID)
	}
	if err := model.ValidateTransition(status, model.TaskStatusQueued, true); err != nil {
		return err
	}
	if current != "" {
		if _, err := supersedeNotificationsTx(tx, `d.attempt_id=?`, []any{current}, "attempt superseded by retry"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE attempts SET status=?, updated_at=? WHERE id=?`, model.AttemptStatusSuperseded, now(), current); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE tasks SET status=?, process_alive=0, claimed_done=0, completion_provenance='', artifact_ref='', branch_pushed=0,
		remote_delivery_state='unverified', pr_state='', discard_authorized=0, archived_at=NULL, updated_at=? WHERE id=?`, model.TaskStatusQueued, now(), taskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Stop(taskID, payload string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, attemptID string
	if err := tx.QueryRow(`SELECT status, COALESCE(current_attempt_id,'') FROM tasks WHERE id=?`, taskID).Scan(&status, &attemptID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("task %q does not exist", taskID)
		}
		return err
	}
	if model.IsTerminalTaskStatus(status) {
		return fmt.Errorf("task %s is already terminal (%s); use worker retry to create a new attempt", taskID, status)
	}
	if err := model.ValidateTransition(status, model.TaskStatusStopped, false); err != nil {
		return err
	}
	t := now()
	if attemptID != "" {
		if _, err := supersedeNotificationsTx(tx, `d.attempt_id=?`, []any{attemptID}, "task stopped by driver"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE tasks SET status=?, process_alive=0, updated_at=? WHERE id=?`, model.TaskStatusStopped, t, taskID); err != nil {
		return err
	}
	if attemptID != "" {
		var generation int
		if err := tx.QueryRow(`SELECT run_generation FROM attempts WHERE id=? AND task_id=?`, attemptID, taskID).Scan(&generation); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE attempts SET status=?, runner_pid=0, ended_at=?, updated_at=? WHERE id=?`, model.AttemptStatusStopped, t, t, attemptID); err != nil {
			return err
		}
		stopPayload := payload + "\nCheckpoint capture: degraded; retained the last valid checkpoint."
		if _, err := insertMessage(tx, taskID, attemptID, "driver-to-worker", "stop", stopPayload, "", false, false, 0, generation, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AcceptedDone(attemptID string, generation int) (model.Message, error) {
	message, err := scanMessage(s.db.QueryRow(`SELECT id, task_id, attempt_id, direction, type, payload, artifact_ref, stale, wake,
		source_cursor, run_generation, checkpoint_json, created_at FROM messages
		WHERE attempt_id=? AND run_generation=? AND type='done' AND stale=0 ORDER BY id DESC LIMIT 1`, attemptID, generation))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, fmt.Errorf("attempt %s has no accepted done event in run generation %d", attemptID, generation)
	}
	return message, err
}

func (s *Store) SetRemoteDelivery(taskID, attemptID, expectedCurrent string, generation int, pushed bool, state, prState string) error {
	if state == "" {
		state = "not_pushed"
	}
	result, err := s.db.Exec(`UPDATE tasks SET
		branch_pushed=CASE WHEN branch_pushed=1 OR ? THEN 1 ELSE 0 END,
		remote_delivery_state=CASE WHEN branch_pushed=1 OR ? THEN 'pushed' ELSE ? END,
		pr_state=CASE WHEN ?='' THEN pr_state ELSE ? END, updated_at=?
		WHERE id=? AND current_attempt_id=? AND current_attempt_id=?
		AND EXISTS(SELECT 1 FROM attempts WHERE id=? AND task_id=? AND run_generation=?)`,
		pushed, pushed, state, prState, prState, now(), taskID, expectedCurrent, attemptID, attemptID, taskID, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %s is stale for task %s", attemptID, taskID)
	}
	return nil
}

func (s *Store) LandingProof(attemptID string) (model.LandingProof, error) {
	return s.CompleteLandingProof(attemptID)
}

// CompleteLandingProof loads and validates the attempt-bound landing/report
// proof. Release authorization must call this instead of trusting the bare
// landed_proven boolean: a legacy or corrupt row that asserts landed_proven
// without complete immutable evidence is rejected here.
func (s *Store) CompleteLandingProof(attemptID string) (model.LandingProof, error) {
	attempt, err := s.Attempt(attemptID)
	if err != nil {
		return model.LandingProof{}, err
	}
	if !attempt.LandedProven {
		if attempt.LandingQuarantineReason != "" {
			return model.LandingProof{}, fmt.Errorf("attempt %s has no trustworthy landing proof: %s", attempt.ID, attempt.LandingQuarantineReason)
		}
		return model.LandingProof{}, fmt.Errorf("attempt %s has no landing proof", attempt.ID)
	}
	if attempt.LandingKind == "" || attempt.LandedVerifiedAt == nil {
		return model.LandingProof{}, fmt.Errorf("attempt %s asserts landed_proven without a complete landing proof (kind %q); refusing to trust the bare boolean", attempt.ID, attempt.LandingKind)
	}
	proof := model.LandingProof{TaskID: attempt.TaskID, AttemptID: attempt.ID, RunGeneration: attempt.RunGeneration,
		Kind: attempt.LandingKind, SourceCommit: attempt.LandedSourceCommit, TargetRef: attempt.LandedTargetRef,
		TargetCommit: attempt.LandedTargetCommit, CheckpointRevision: attempt.LandedCheckpointRevision,
		VerifiedAt: *attempt.LandedVerifiedAt}
	var projected int
	if err := s.db.QueryRow(`SELECT landed FROM attempt_landing_projections WHERE attempt_id=?`, attempt.ID).Scan(&projected); err != nil {
		return model.LandingProof{}, err
	}
	switch attempt.LandingKind {
	case "report_artifact":
		if projected == 0 {
			return model.LandingProof{}, fmt.Errorf("attempt %s report landing proof has no complete immutable verified report artifact provenance bound to it", attempt.ID)
		}
	case "local_default_branch", "github_pr":
		if proof.SourceCommit == "" || proof.TargetRef == "" || proof.TargetCommit == "" || proof.CheckpointRevision < 1 {
			return model.LandingProof{}, fmt.Errorf("attempt %s %s landing proof identity is incomplete", attempt.ID, attempt.LandingKind)
		}
	case "github_pr_attested_ancestry":
		if projected == 0 {
			return model.LandingProof{}, fmt.Errorf("attempt %s %s landing proof has incomplete identity or no matching immutable external delivery attestation evidence", attempt.ID, attempt.LandingKind)
		}
	case model.LandingKindLocalAttestedAncestry:
		if projected == 0 {
			return model.LandingProof{}, fmt.Errorf("attempt %s %s landing proof has incomplete identity or no matching immutable local delivery recovery evidence", attempt.ID, attempt.LandingKind)
		}
	default:
		return model.LandingProof{}, fmt.Errorf("attempt %s has unrecognized landing proof kind %q", attempt.ID, attempt.LandingKind)
	}
	if projected == 0 {
		return model.LandingProof{}, fmt.Errorf("attempt %s %s landing proof is incomplete", attempt.ID, attempt.LandingKind)
	}
	return proof, nil
}

// MarkNoWorkspace classifies an attempt whose workspace acquisition failed
// before any worktree or lease existed. Such attempts own no resource, so
// recording them as held would be a false positive that release and reconcile
// can never resolve.
func (s *Store) MarkNoWorkspace(attemptID, reason string) error {
	timestamp := now()
	result, err := s.db.Exec(`UPDATE attempts SET release_state='no_workspace', workspace_state=?,
		workspace_state_changed_at=?, release_reason=CASE WHEN release_reason='' THEN ? ELSE release_reason END, updated_at=?
		WHERE id=? AND release_state='held' AND workspace_state IN (?, ?)
		AND worktree_path='' AND lease_id='' AND released_at IS NULL`, model.WorkspaceStateNoWorkspace, timestamp, reason, timestamp, attemptID, model.WorkspaceStateUnassigned, model.WorkspaceStateAllocating)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("attempt %s has a recorded workspace or is not held; refusing the no-workspace classification", attemptID)
	}
	return nil
}

func (s *Store) RecordLandingProof(expectedCurrent string, proof model.LandingProof, reason string) (model.LandingProof, error) {
	if proof.TaskID == "" || proof.AttemptID == "" || proof.RunGeneration < 1 || proof.Kind == "" || proof.SourceCommit == "" || proof.TargetRef == "" || proof.TargetCommit == "" || proof.CheckpointRevision < 1 {
		return model.LandingProof{}, fmt.Errorf("landing proof identity is incomplete")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.LandingProof{}, err
	}
	defer tx.Rollback()
	var current, attemptStatus, existingKind, existingSource, existingTargetRef, existingTargetCommit string
	var generation, existingRevision int
	var landed int
	var existingVerified sql.NullString
	if err := tx.QueryRow(`SELECT COALESCE(t.current_attempt_id,''), a.status, a.run_generation, a.landed_proven,
		a.landing_kind, a.landed_source_commit, a.landed_target_ref, a.landed_target_commit,
		a.landed_checkpoint_revision, a.landed_verified_at
		FROM tasks t JOIN attempts a ON a.task_id=t.id WHERE t.id=? AND a.id=?`, proof.TaskID, proof.AttemptID).
		Scan(&current, &attemptStatus, &generation, &landed, &existingKind, &existingSource, &existingTargetRef,
			&existingTargetCommit, &existingRevision, &existingVerified); err != nil {
		return model.LandingProof{}, err
	}
	if current != expectedCurrent || generation != proof.RunGeneration {
		return model.LandingProof{}, fmt.Errorf("attempt %s became stale before landing proof persistence", proof.AttemptID)
	}
	if attemptStatus != model.AttemptStatusDone && attemptStatus != model.AttemptStatusSuperseded {
		return model.LandingProof{}, fmt.Errorf("attempt %s is %s, not accepted done", proof.AttemptID, attemptStatus)
	}
	if landed != 0 || existingSource != "" {
		if existingKind != proof.Kind || existingSource != proof.SourceCommit || existingTargetRef != proof.TargetRef || existingRevision != proof.CheckpointRevision {
			return model.LandingProof{}, fmt.Errorf("attempt %s already has a conflicting immutable landing proof", proof.AttemptID)
		}
		if err := tx.Commit(); err != nil {
			return model.LandingProof{}, err
		}
		verified := time.Time{}
		if existingVerified.Valid {
			verified = parseTime(existingVerified.String)
		}
		return model.LandingProof{TaskID: proof.TaskID, AttemptID: proof.AttemptID, RunGeneration: proof.RunGeneration,
			Kind: existingKind, SourceCommit: existingSource, TargetRef: existingTargetRef, TargetCommit: existingTargetCommit,
			CheckpointRevision: existingRevision, VerifiedAt: verified}, nil
	}
	switch proof.Kind {
	case "local_default_branch", "github_pr":
	case "github_pr_attested_ancestry":
		var evidence int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM external_delivery_attestations
			WHERE task_id=? AND attempt_id=? AND run_generation=? AND sealed_commit=?
			AND 'refs/heads/' || registered_default_branch=? AND merge_commit=? AND checkpoint_revision=?`,
			proof.TaskID, proof.AttemptID, proof.RunGeneration, proof.SourceCommit, proof.TargetRef, proof.TargetCommit, proof.CheckpointRevision).Scan(&evidence); err != nil {
			return model.LandingProof{}, err
		}
		if evidence != 1 {
			return model.LandingProof{}, fmt.Errorf("attempt %s has no matching immutable external delivery attestation evidence", proof.AttemptID)
		}
	case model.LandingKindLocalAttestedAncestry:
		var evidence int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM local_delivery_recoveries
			WHERE task_id=? AND attempt_id=? AND run_generation=? AND sealed_commit=?
			AND 'refs/heads/' || registered_default_branch=? AND checkpoint_revision=?`,
			proof.TaskID, proof.AttemptID, proof.RunGeneration, proof.SourceCommit, proof.TargetRef, proof.CheckpointRevision).Scan(&evidence); err != nil {
			return model.LandingProof{}, err
		}
		if evidence != 1 {
			return model.LandingProof{}, fmt.Errorf("attempt %s has no matching immutable local delivery recovery evidence", proof.AttemptID)
		}
	default:
		return model.LandingProof{}, fmt.Errorf("landing proof kind %q must be recorded through its authoritative evidence path", proof.Kind)
	}
	var checkpointProducer, checkpointHead string
	var checkpointGeneration, checkpointRevision int
	if err := tx.QueryRow(`SELECT producer, run_generation, revision, head_commit FROM attempt_checkpoints WHERE attempt_id=?`, proof.AttemptID).
		Scan(&checkpointProducer, &checkpointGeneration, &checkpointRevision, &checkpointHead); err != nil {
		return model.LandingProof{}, err
	}
	if checkpointProducer != "worker" || checkpointGeneration != proof.RunGeneration || checkpointRevision != proof.CheckpointRevision || checkpointHead != proof.SourceCommit {
		return model.LandingProof{}, fmt.Errorf("attempt %s checkpoint identity changed before landing proof persistence", proof.AttemptID)
	}
	var doneCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE attempt_id=? AND run_generation=? AND type='done' AND stale=0`, proof.AttemptID, proof.RunGeneration).Scan(&doneCount); err != nil {
		return model.LandingProof{}, err
	}
	if doneCount != 1 {
		return model.LandingProof{}, fmt.Errorf("attempt %s no longer has exactly one accepted done event", proof.AttemptID)
	}
	if proof.VerifiedAt.IsZero() {
		proof.VerifiedAt = time.Now().UTC()
	}
	result, err := tx.Exec(`UPDATE attempts SET landed_proven=1, landing_kind=?, landed_source_commit=?,
		landed_target_ref=?, landed_target_commit=?, landed_checkpoint_revision=?, landed_verified_at=?,
		landing_reason=CASE WHEN landing_reason='' THEN ? ELSE landing_reason END, updated_at=?
		WHERE id=? AND task_id=? AND run_generation=? AND landed_source_commit=''
		AND (SELECT COALESCE(current_attempt_id,'') FROM tasks WHERE id=?)=?`, proof.Kind, proof.SourceCommit,
		proof.TargetRef, proof.TargetCommit, proof.CheckpointRevision, stamp(proof.VerifiedAt), reason, now(), proof.AttemptID,
		proof.TaskID, proof.RunGeneration, proof.TaskID, expectedCurrent)
	if err != nil {
		return model.LandingProof{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.LandingProof{}, fmt.Errorf("attempt %s became stale before landing proof persistence", proof.AttemptID)
	}
	if current == proof.AttemptID {
		if _, err := tx.Exec(`UPDATE tasks SET updated_at=? WHERE id=? AND current_attempt_id=?`, now(), proof.TaskID, proof.AttemptID); err != nil {
			return model.LandingProof{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.LandingProof{}, err
	}
	return proof, nil
}

func (s *Store) AuthorizeDiscard(taskID, attemptID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE attempts SET discard_authorized=1, updated_at=? WHERE id=? AND task_id=?`, now(), attemptID, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET discard_authorized=CASE WHEN current_attempt_id=? THEN 1 ELSE discard_authorized END, updated_at=? WHERE id=?`, attemptID, now(), taskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimRelease(attemptID string) (bool, string, error) {
	timestamp := now()
	result, err := s.db.Exec(`UPDATE attempts SET release_state='releasing', workspace_state=?,
		workspace_state_changed_at=?, release_claimed_at=?, release_owner_pid=?, updated_at=?
		WHERE id=? AND release_state='held' AND workspace_state IN (?, ?) AND released_at IS NULL`,
		model.WorkspaceStateReleasing, timestamp, timestamp, os.Getpid(), timestamp, attemptID, model.WorkspaceStateHeld, model.WorkspaceStateUnassigned)
	if err != nil {
		return false, "", err
	}
	if n, _ := result.RowsAffected(); n == 1 {
		return true, model.WorkspaceStateReleasing, nil
	}
	var state, workspaceState string
	if err := s.db.QueryRow(`SELECT release_state, workspace_state FROM attempts WHERE id=?`, attemptID).Scan(&state, &workspaceState); err != nil {
		return false, "", err
	}
	if state == "held" && (workspaceState == model.WorkspaceStateAllocating || workspaceState == model.WorkspaceStateUnknown) {
		return false, workspaceState, nil
	}
	return false, state, nil
}

func (s *Store) ResetReleaseClaim(attemptID string) error {
	timestamp := now()
	_, err := s.db.Exec(`UPDATE attempts SET release_state='held',
		workspace_state=CASE WHEN workspace_state=? THEN ? ELSE workspace_state END,
		workspace_state_changed_at=?, release_claimed_at=NULL, release_owner_pid=0, updated_at=?
		WHERE id=? AND release_state='releasing' AND released_at IS NULL`, model.WorkspaceStateReleasing, model.WorkspaceStateHeld, timestamp, timestamp, attemptID)
	return err
}

func (s *Store) MarkReleasedWithReason(attemptID, reason string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	timestamp := now()
	if _, err := tx.Exec(`UPDATE attempts SET release_state='released', workspace_state=?, workspace_state_changed_at=?,
		release_claimed_at=NULL, release_owner_pid=0,
		release_reason=CASE WHEN release_reason='' THEN ? ELSE release_reason END,
		released_at=COALESCE(released_at,?), updated_at=? WHERE id=? AND release_state IN ('releasing','released')`, model.WorkspaceStateReleased, timestamp, reason, timestamp, timestamp, attemptID); err != nil {
		return err
	}
	if err := supersedeReleasedAttemptNotificationsTx(tx, attemptID, "attempt released"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkWorkspaceUnknown(attemptID, reason string) error {
	timestamp := now()
	_, err := s.db.Exec(`UPDATE attempts SET status=?, failure_reason=?,
		workspace_state=CASE WHEN workspace_state=? THEN workspace_state ELSE ? END,
		workspace_state_changed_at=?, updated_at=? WHERE id=?`, model.AttemptStatusWorkspaceUnknown, reason, model.WorkspaceStateReleased, model.WorkspaceStateUnknown, timestamp, timestamp, attemptID)
	return err
}

func (s *Store) ActiveAttempts() ([]model.Attempt, error) {
	return s.recordedRunnerAttempts(` AND status IN (?,?)`, model.AttemptStatusStarting, model.AttemptStatusWorking)
}

func (s *Store) RecordedRunnerAttempts() ([]model.Attempt, error) {
	return s.recordedRunnerAttempts("")
}

func (s *Store) recordedRunnerAttempts(predicate string, args ...any) ([]model.Attempt, error) {
	rows, err := s.db.Query(attemptSelect+` WHERE runner_pid > 0`+predicate, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := make([]model.Attempt, 0)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
}

func now() string {
	return stamp(time.Now().UTC())
}

func stamp(t time.Time) string {
	return t.Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, value)
	return t
}
