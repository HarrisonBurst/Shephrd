package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"shephrd/internal/model"
)

const (
	MaxAnnotationJudgment   = 4096
	MaxAnnotationReason     = 1024
	MaxAnnotationNextAction = 1024
)

func (s *Store) AddAnnotation(scope model.AnnotationScope, driverID string, expectedPriorRevision int, input model.AnnotationInput) (model.Annotation, error) {
	driverID = strings.TrimSpace(driverID)
	if err := validateAnnotation(scope, driverID, expectedPriorRevision, input); err != nil {
		return model.Annotation{}, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Annotation{}, err
	}
	defer tx.Rollback()
	owner, prior, err := annotationScopeStateTx(tx, scope)
	if err != nil {
		return model.Annotation{}, err
	}
	if owner != driverID {
		return model.Annotation{}, fmt.Errorf("annotation scope is owned by %s, not %s", owner, driverID)
	}
	if prior != expectedPriorRevision {
		return model.Annotation{}, annotationRevisionConflict(scope, expectedPriorRevision, prior)
	}
	created := time.Now().UTC()
	annotation := model.Annotation{
		ID:         NewID("annotation"),
		DriverID:   driverID,
		TaskID:     scope.TaskID,
		PlanID:     scope.PlanID,
		PlanItemID: scope.PlanItemID,
		Revision:   prior + 1,
		Judgment:   input.Judgment,
		Reason:     input.Reason,
		NextAction: input.NextAction,
		CreatedAt:  created,
	}
	_, err = tx.Exec(`INSERT INTO annotations(id, driver_id, task_id, plan_id, plan_item_id, revision, judgment, reason, next_action, created_at)
		VALUES(?, ?, NULLIF(?,''), NULLIF(?,''), NULLIF(?,''), ?, ?, ?, ?, ?)`, annotation.ID, annotation.DriverID, annotation.TaskID,
		annotation.PlanID, annotation.PlanItemID, annotation.Revision, annotation.Judgment, annotation.Reason, annotation.NextAction, stamp(created))
	if err != nil {
		if isSQLiteConstraint(err, sqliteConstraintUnique) || isSQLiteConstraint(err, sqliteConstraintTrigger) {
			_, actual, stateErr := annotationScopeStateTx(tx, scope)
			if stateErr == nil {
				return model.Annotation{}, annotationRevisionConflict(scope, expectedPriorRevision, actual)
			}
		}
		return model.Annotation{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Annotation{}, err
	}
	return annotation, nil
}

func (s *Store) Annotations(scope model.AnnotationScope) ([]model.Annotation, error) {
	if err := validateAnnotationScope(scope); err != nil {
		return nil, err
	}
	if err := annotationReadScopeExistsDB(s.db, scope); err != nil {
		return nil, err
	}
	query := `SELECT id, driver_id, COALESCE(task_id,''), COALESCE(plan_id,''), COALESCE(plan_item_id,''), revision, judgment, reason, next_action, created_at
		FROM annotations WHERE task_id=? ORDER BY revision`
	arguments := []any{scope.TaskID}
	if scope.PlanItemID != "" {
		query = `SELECT id, driver_id, COALESCE(task_id,''), COALESCE(plan_id,''), COALESCE(plan_item_id,''), revision, judgment, reason, next_action, created_at
			FROM annotations WHERE plan_id=? AND plan_item_id=? ORDER BY revision`
		arguments = []any{scope.PlanID, scope.PlanItemID}
	}
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	annotations := make([]model.Annotation, 0)
	for rows.Next() {
		annotation, err := scanAnnotation(rows)
		if err != nil {
			return nil, err
		}
		annotations = append(annotations, annotation)
	}
	return annotations, rows.Err()
}

func (s *Store) LatestAnnotation(scope model.AnnotationScope) (*model.Annotation, error) {
	if err := validateAnnotationScope(scope); err != nil {
		return nil, err
	}
	query := `SELECT id, driver_id, COALESCE(task_id,''), COALESCE(plan_id,''), COALESCE(plan_item_id,''), revision, judgment, reason, next_action, created_at
		FROM annotations WHERE task_id=? ORDER BY revision DESC LIMIT 1`
	arguments := []any{scope.TaskID}
	if scope.PlanItemID != "" {
		query = `SELECT id, driver_id, COALESCE(task_id,''), COALESCE(plan_id,''), COALESCE(plan_item_id,''), revision, judgment, reason, next_action, created_at
			FROM annotations WHERE plan_id=? AND plan_item_id=? ORDER BY revision DESC LIMIT 1`
		arguments = []any{scope.PlanID, scope.PlanItemID}
	}
	annotation, err := scanAnnotation(s.db.QueryRow(query, arguments...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &annotation, nil
}

func (s *Store) AttachLatestPlanAnnotations(plan *model.Plan) error {
	for index := range plan.Items {
		annotation, err := s.LatestAnnotation(model.AnnotationScope{PlanID: plan.ID, PlanItemID: plan.Items[index].ID})
		if err != nil {
			return err
		}
		plan.Items[index].LatestAnnotation = annotation
	}
	return nil
}

func validateAnnotation(scope model.AnnotationScope, driverID string, expectedPriorRevision int, input model.AnnotationInput) error {
	if err := validateAnnotationScope(scope); err != nil {
		return err
	}
	if err := validateDriverID(driverID); err != nil {
		return err
	}
	if strings.ContainsRune(driverID, '\x00') || driverID != strings.TrimSpace(driverID) {
		return fmt.Errorf("driver ID must be trimmed and NUL-free")
	}
	if expectedPriorRevision < 0 {
		return fmt.Errorf("expected prior revision must not be negative")
	}
	if err := validateAnnotationText("judgment", input.Judgment, MaxAnnotationJudgment, true); err != nil {
		return err
	}
	if err := validateAnnotationText("reason", input.Reason, MaxAnnotationReason, false); err != nil {
		return err
	}
	return validateAnnotationText("next action", input.NextAction, MaxAnnotationNextAction, false)
}

func validateAnnotationScope(scope model.AnnotationScope) error {
	taskID := strings.TrimSpace(scope.TaskID)
	planID := strings.TrimSpace(scope.PlanID)
	itemID := strings.TrimSpace(scope.PlanItemID)
	taskScope := taskID != "" && planID == "" && itemID == ""
	itemScope := taskID == "" && planID != "" && itemID != ""
	if !taskScope && !itemScope {
		return fmt.Errorf("annotation must be scoped to exactly one task or plan item identity")
	}
	if taskID != scope.TaskID || planID != scope.PlanID || itemID != scope.PlanItemID || strings.ContainsRune(taskID+planID+itemID, '\x00') {
		return fmt.Errorf("annotation scope identity must be trimmed and NUL-free")
	}
	return nil
}

func validateAnnotationText(label, value string, limit int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("annotation judgment must not be empty")
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("annotation %s must be trimmed", label)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("annotation %s contains NUL", label)
	}
	if len([]byte(value)) > limit {
		return fmt.Errorf("annotation %s exceeds %d bytes", label, limit)
	}
	return nil
}

func annotationRevisionConflict(scope model.AnnotationScope, expected, actual int) error {
	identity := scope.TaskID
	if scope.PlanItemID != "" {
		identity = scope.PlanID + "/" + scope.PlanItemID
	}
	return model.Failure("annotation_revision_conflict", "annotation scope %s is at revision %d, not expected revision %d; list annotations and retry with the current revision", identity, actual, expected)
}

func annotationScopeStateTx(tx *sql.Tx, scope model.AnnotationScope) (string, int, error) {
	return annotationScopeStateDB(tx, scope)
}

type annotationScopeQueryer interface {
	QueryRow(string, ...any) *sql.Row
}

func annotationScopeStateDB(queryer annotationScopeQueryer, scope model.AnnotationScope) (string, int, error) {
	var owner string
	var revision int
	var err error
	if scope.TaskID != "" {
		err = queryer.QueryRow(`SELECT t.driver_id, COALESCE((SELECT MAX(a.revision) FROM annotations a WHERE a.task_id=t.id),0)
			FROM tasks t WHERE t.id=?`, scope.TaskID).Scan(&owner, &revision)
	} else {
		err = queryer.QueryRow(`SELECT l.driver_id, COALESCE((SELECT MAX(a.revision) FROM annotations a WHERE a.plan_id=i.plan_id AND a.plan_item_id=i.id),0)
			FROM plan_items i JOIN plans l ON l.id=i.plan_id WHERE i.plan_id=? AND i.id=?`, scope.PlanID, scope.PlanItemID).Scan(&owner, &revision)
	}
	if errors.Is(err, sql.ErrNoRows) {
		identity := scope.TaskID
		kind := "task"
		if scope.PlanItemID != "" {
			identity = scope.PlanItemID
			kind = "plan item"
		}
		return "", 0, fmt.Errorf("%s %q does not exist", kind, identity)
	}
	return owner, revision, err
}

func annotationReadScopeExistsDB(queryer annotationScopeQueryer, scope model.AnnotationScope) error {
	var exists int
	var err error
	if scope.TaskID != "" {
		err = queryer.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`, scope.TaskID).Scan(&exists)
	} else {
		err = queryer.QueryRow(`SELECT EXISTS(
			SELECT 1 FROM plan_items WHERE plan_id=? AND id=?
			UNION ALL
			SELECT 1 FROM annotations WHERE plan_id=? AND plan_item_id=?
		)`, scope.PlanID, scope.PlanItemID, scope.PlanID, scope.PlanItemID).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if exists == 0 {
		identity := scope.TaskID
		kind := "task"
		if scope.PlanID != "" {
			identity = scope.PlanID + "/" + scope.PlanItemID
			kind = "plan item identity"
		}
		return fmt.Errorf("%s %q does not exist", kind, identity)
	}
	return nil
}

func scanAnnotation(scanner interface{ Scan(...any) error }) (model.Annotation, error) {
	var annotation model.Annotation
	var created string
	if err := scanner.Scan(&annotation.ID, &annotation.DriverID, &annotation.TaskID, &annotation.PlanID, &annotation.PlanItemID, &annotation.Revision,
		&annotation.Judgment, &annotation.Reason, &annotation.NextAction, &created); err != nil {
		return model.Annotation{}, err
	}
	annotation.CreatedAt = parseTime(created)
	return annotation, nil
}
