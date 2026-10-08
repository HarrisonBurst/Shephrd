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

func (s *Store) CreatePlan(name, driverID string) (model.Plan, error) {
	name = strings.TrimSpace(name)
	driverID = strings.TrimSpace(driverID)
	if name == "" {
		return model.Plan{}, fmt.Errorf("plan name must not be empty")
	}
	if err := validateDriverID(driverID); err != nil {
		return model.Plan{}, err
	}
	t := time.Now().UTC()
	plan := model.Plan{ID: NewID("plan"), Name: name, DriverID: driverID, CreatedAt: t, UpdatedAt: t}
	if _, err := s.db.Exec(`INSERT INTO plans(id, name, driver_id, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`, plan.ID, plan.Name, plan.DriverID, stamp(t), stamp(t)); err != nil {
		if isSQLiteConstraint(err, sqliteConstraintUnique) {
			var conflict int
			if queryErr := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM plans WHERE driver_id=? AND name=?)`, driverID, name).Scan(&conflict); queryErr == nil && conflict != 0 {
				return model.Plan{}, contractFailure(ErrPlanNameConflict, "driver %s already has a plan named %q", driverID, name)
			}
		}
		return model.Plan{}, err
	}
	return plan, nil
}

func (s *Store) Plans(driverID string) ([]model.Plan, error) {
	if err := validateDriverID(strings.TrimSpace(driverID)); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, name, driver_id, created_at, updated_at FROM plans WHERE driver_id=? ORDER BY created_at, id`, strings.TrimSpace(driverID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := make([]model.Plan, 0)
	for rows.Next() {
		plan, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

func (s *Store) PlanSummaries(driverID string, allDrivers bool) ([]model.PlanSummary, error) {
	driverID = strings.TrimSpace(driverID)
	if !allDrivers {
		if err := validateDriverID(driverID); err != nil {
			return nil, err
		}
	} else if driverID != "" {
		if err := validateDriverID(driverID); err != nil {
			return nil, err
		}
	}
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	summaries, err := planSummariesTx(tx, driverID, allDrivers)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return summaries, nil
}

func planSummariesTx(tx *sql.Tx, driverID string, allDrivers bool) ([]model.PlanSummary, error) {
	query := `SELECT l.id, l.name, l.driver_id, COUNT(i.id),
		COALESCE(SUM(CASE WHEN i.id IS NOT NULL AND i.dispatched_task_id IS NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN i.dispatched_task_id IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN t.status IN ('queued','starting','working','waiting') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN lp.landed=1 THEN 1 ELSE 0 END),0), l.created_at, l.updated_at
		FROM plans l LEFT JOIN plan_items i ON i.plan_id=l.id
		LEFT JOIN tasks t ON t.id=i.dispatched_task_id
		LEFT JOIN task_landing_projections lp ON lp.task_id=t.id`
	var args []any
	if !allDrivers {
		query += ` WHERE l.driver_id=?`
		args = append(args, driverID)
	}
	query += ` GROUP BY l.id ORDER BY l.created_at, l.id`
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	summaries := make([]model.PlanSummary, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var summary model.PlanSummary
		var created, updated string
		if err := rows.Scan(&summary.ID, &summary.Name, &summary.DriverID, &summary.ItemCount, &summary.UndispatchedItemCount,
			&summary.DispatchedTaskCount, &summary.LiveTaskCount, &summary.LandedTaskCount, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		summary.Ownership = "unowned"
		if driverID != "" && summary.DriverID == driverID {
			summary.Ownership = "owned"
		}
		summary.DispatchedTasks = make([]model.PlanTaskSummary, 0)
		summary.CreatedAt, summary.UpdatedAt = parseTime(created), parseTime(updated)
		byID[summary.ID] = len(summaries)
		summaries = append(summaries, summary)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	query = `SELECT i.plan_id, t.id, t.title, t.status, lp.landed, COALESCE(t.current_attempt_id,'')
		FROM plan_items i JOIN tasks t ON t.id=i.dispatched_task_id
		JOIN task_landing_projections lp ON lp.task_id=t.id JOIN plans l ON l.id=i.plan_id`
	args = nil
	if !allDrivers {
		query += ` WHERE l.driver_id=?`
		args = append(args, driverID)
	}
	query += ` ORDER BY l.created_at, l.id, i.position`
	rows, err = tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var planID string
		var task model.PlanTaskSummary
		var landed int
		if err := rows.Scan(&planID, &task.ID, &task.Title, &task.Status, &landed, &task.CurrentAttemptID); err != nil {
			return nil, err
		}
		task.Landed = landed != 0
		if index, exists := byID[planID]; exists {
			summaries[index].DispatchedTasks = append(summaries[index].DispatchedTasks, task)
		}
	}
	return summaries, rows.Err()
}

func (s *Store) Plan(ref, driverID string) (model.Plan, error) {
	ref = strings.TrimSpace(ref)
	driverID = strings.TrimSpace(driverID)
	if ref == "" {
		return model.Plan{}, fmt.Errorf("plan reference must not be empty")
	}
	if err := validateDriverID(driverID); err != nil {
		return model.Plan{}, err
	}
	plan, err := scanPlan(s.db.QueryRow(`SELECT id, name, driver_id, created_at, updated_at FROM plans WHERE driver_id=? AND (id=? OR name=?)`, driverID, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Plan{}, fmt.Errorf("plan %q does not exist for driver %s", ref, driverID)
	}
	if err != nil {
		return model.Plan{}, err
	}
	items, err := s.planItems(plan.ID)
	if err != nil {
		return model.Plan{}, err
	}
	plan.Items = items
	return plan, nil
}

func (s *Store) AdoptPlan(id, currentDriverID, newDriverID string) (model.Plan, error) {
	currentDriverID = strings.TrimSpace(currentDriverID)
	newDriverID = strings.TrimSpace(newDriverID)
	if err := validateDriverID(currentDriverID); err != nil {
		return model.Plan{}, err
	}
	if err := validateDriverID(newDriverID); err != nil {
		return model.Plan{}, err
	}
	result, err := s.db.Exec(`UPDATE plans SET driver_id=?, updated_at=? WHERE id=? AND driver_id=?`, newDriverID, now(), id, currentDriverID)
	if err != nil {
		if isSQLiteConstraint(err, sqliteConstraintUnique) {
			var conflict int
			if queryErr := s.db.QueryRow(`SELECT EXISTS(
				SELECT 1 FROM plans existing JOIN plans adopting ON adopting.id=?
				WHERE existing.driver_id=? AND existing.name=adopting.name AND existing.id<>adopting.id)`, id, newDriverID).Scan(&conflict); queryErr == nil && conflict != 0 {
				return model.Plan{}, contractFailure(ErrPlanNameConflict, "driver %s already owns a plan with this name", newDriverID)
			}
		}
		return model.Plan{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var owner string
		if queryErr := s.db.QueryRow(`SELECT driver_id FROM plans WHERE id=?`, id).Scan(&owner); errors.Is(queryErr, sql.ErrNoRows) {
			return model.Plan{}, fmt.Errorf("plan %q does not exist", id)
		} else if queryErr != nil {
			return model.Plan{}, queryErr
		}
		return model.Plan{}, fmt.Errorf("plan %s is owned by %s, not current driver %s", id, owner, currentDriverID)
	}
	return s.Plan(id, newDriverID)
}

func scanPlan(scanner interface{ Scan(...any) error }) (model.Plan, error) {
	var plan model.Plan
	var created, updated string
	if err := scanner.Scan(&plan.ID, &plan.Name, &plan.DriverID, &created, &updated); err != nil {
		return model.Plan{}, err
	}
	plan.CreatedAt, plan.UpdatedAt = parseTime(created), parseTime(updated)
	return plan, nil
}

// AddPlanItem inserts one ordered planned item and, in the same transaction,
// its explicit prerequisite and report-input relations. Relations may only
// reference items in the same plan and are rejected for dispatched scope.
func (s *Store) AddPlanItem(planID string, item model.PlanItem, relations model.PlanItemRelations) (model.PlanItem, error) {
	item.Title = strings.TrimSpace(item.Title)
	item.Objective = strings.TrimSpace(item.Objective)
	item.FeatureKey = strings.TrimSpace(item.FeatureKey)
	if item.Title == "" || item.Objective == "" {
		return model.PlanItem{}, fmt.Errorf("plan item title and objective must not be empty")
	}
	if item.Deliverable == "" {
		item.Deliverable = "code"
	}
	if item.Deliverable != "code" && item.Deliverable != "report" {
		return model.PlanItem{}, fmt.Errorf("plan item deliverable must be code or report")
	}
	if len(relations.Requires) > model.MaxTaskReportInputs || len(relations.Reports) > model.MaxTaskReportInputs {
		return model.PlanItem{}, fmt.Errorf("a plan item can declare at most %d relations", model.MaxTaskReportInputs)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.PlanItem{}, err
	}
	defer tx.Rollback()
	if err := requirePlanTx(tx, planID); err != nil {
		return model.PlanItem{}, err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plan_items WHERE plan_id=?`, planID).Scan(&count); err != nil {
		return model.PlanItem{}, err
	}
	position := 0
	if relations.Position > 0 {
		position = relations.Position
	}
	if position == 0 {
		position = count + 1
	}
	if position < 1 || position > count+1 {
		return model.PlanItem{}, fmt.Errorf("item position must be between 1 and %d", count+1)
	}
	if err := mutateOrderedPositionsTx(tx, planItemPositions, planID, orderedPositionMutation{operation: openOrderedPosition, position: position}); err != nil {
		return model.PlanItem{}, err
	}
	t := time.Now().UTC()
	item.ID = NewID("item")
	item.PlanID = planID
	item.Position = position
	item.CreatedAt, item.UpdatedAt = t, t
	if _, err := tx.Exec(`INSERT INTO plan_items(id, plan_id, position, feature_key, objective, description, acceptance_criteria, repo_id, deliverable, created_at, updated_at, title)
		VALUES(?, ?, ?, ?, ?, ?, ?, NULLIF(?,''), ?, ?, ?, ?)`, item.ID, planID, position, item.FeatureKey, item.Objective, item.Description,
		item.AcceptanceCriteria, item.RepoID, item.Deliverable, stamp(t), stamp(t), item.Title); err != nil {
		return model.PlanItem{}, err
	}
	for _, prerequisiteID := range relations.Requires {
		if prerequisiteID == item.ID {
			return model.PlanItem{}, fmt.Errorf("plan item %s cannot be its own prerequisite", item.ID)
		}
		if err := insertPlanPrerequisiteTx(tx, item.ID, prerequisiteID); err != nil {
			return model.PlanItem{}, err
		}
	}
	for _, prerequisiteID := range relations.Reports {
		if err := insertPlanReportTx(tx, item.ID, prerequisiteID, t); err != nil {
			return model.PlanItem{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, stamp(t), planID); err != nil {
		return model.PlanItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PlanItem{}, err
	}
	return s.PlanItem(planID, item.ID)
}

func insertPlanPrerequisiteTx(tx *sql.Tx, itemID, prerequisiteItemID string) error {
	prerequisiteID := strings.TrimSpace(prerequisiteItemID)
	if prerequisiteID == "" {
		return fmt.Errorf("prerequisite item reference must not be empty")
	}
	if _, err := tx.Exec(`INSERT INTO plan_prerequisites(item_id, prerequisite_item_id, created_at)
		SELECT ?, ?, ? WHERE EXISTS(SELECT 1 FROM plan_items WHERE id=?)`, itemID, prerequisiteID, now(), itemID); err != nil {
		if isSQLiteConstraint(err, sqliteConstraintTrigger) {
			cycle, cycleErr := planPrerequisiteCycleTx(tx, itemID, prerequisiteID)
			if cycleErr != nil {
				return cycleErr
			}
			if cycle {
				return contractFailure(ErrPlanPrerequisiteCycle, "adding prerequisite %s to item %s would create a cycle", prerequisiteID, itemID)
			}
			var sameList int
			if err := tx.QueryRow(`SELECT CASE WHEN
				(SELECT plan_id FROM plan_items WHERE id=?) IS NULL OR
				(SELECT plan_id FROM plan_items WHERE id=?) IS NULL OR
				(SELECT plan_id FROM plan_items WHERE id=?) <> (SELECT plan_id FROM plan_items WHERE id=?)
			THEN 0 ELSE 1 END`, itemID, prerequisiteID, itemID, prerequisiteID).Scan(&sameList); err != nil {
				return err
			}
			if sameList == 0 {
				return fmt.Errorf("prerequisite %s must belong to the same plan", prerequisiteID)
			}
		}
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plan_prerequisites WHERE item_id=? AND prerequisite_item_id=?`, itemID, prerequisiteID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("plan item %q does not exist in the plan", prerequisiteID)
	}
	return nil
}

func insertPlanReportTx(tx *sql.Tx, itemID, prerequisiteItemID string, t time.Time) error {
	prerequisiteID := strings.TrimSpace(prerequisiteItemID)
	if prerequisiteID == "" {
		return fmt.Errorf("report prerequisite reference must not be empty")
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plan_report_inputs WHERE item_id=?`, itemID).Scan(&count); err != nil {
		return err
	}
	if count >= model.MaxTaskReportInputs {
		return fmt.Errorf("a plan item can select at most %d report inputs", model.MaxTaskReportInputs)
	}
	if _, err := tx.Exec(`INSERT INTO plan_report_inputs(item_id, prerequisite_item_id, position, created_at, updated_at)
		SELECT ?, ?, ?, ?, ? WHERE EXISTS(SELECT 1 FROM plan_items WHERE id=?)`, itemID, prerequisiteID, count+1, stamp(t), stamp(t), itemID); err != nil {
		if isSQLiteConstraint(err, sqliteConstraintTrigger) {
			return fmt.Errorf("report input for prerequisite %s requires a prerequisite relation first", prerequisiteID)
		}
		return err
	}
	return nil
}

func (s *Store) PlanItem(planID, itemID string) (model.PlanItem, error) {
	item, err := scanPlanItem(s.db.QueryRow(planItemSelect+` WHERE i.plan_id=? AND i.id=?`, planID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.PlanItem{}, fmt.Errorf("plan item %q does not exist in plan %s", itemID, planID)
	}
	if err != nil {
		return model.PlanItem{}, err
	}
	if err := s.hydratePlanItem(&item); err != nil {
		return model.PlanItem{}, err
	}
	return item, nil
}

func (s *Store) UpdatePlanItem(planID, itemID string, update model.PlanItemUpdate) (model.PlanItem, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.PlanItem{}, err
	}
	defer tx.Rollback()
	item, err := scanPlanItem(tx.QueryRow(planItemSelect+` WHERE i.plan_id=? AND i.id=?`, planID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.PlanItem{}, fmt.Errorf("plan item %q does not exist in plan %s", itemID, planID)
	}
	if err != nil {
		return model.PlanItem{}, err
	}
	if item.DispatchedTaskID != "" {
		return model.PlanItem{}, fmt.Errorf("plan item %s was already dispatched and its scope is immutable", item.ID)
	}
	if update.Title != nil {
		item.Title = strings.TrimSpace(*update.Title)
	}
	if update.FeatureKey != nil {
		item.FeatureKey = strings.TrimSpace(*update.FeatureKey)
	}
	if update.Objective != nil {
		item.Objective = strings.TrimSpace(*update.Objective)
	}
	if update.Description != nil {
		item.Description = strings.TrimSpace(*update.Description)
	}
	if update.AcceptanceCriteria != nil {
		item.AcceptanceCriteria = strings.TrimSpace(*update.AcceptanceCriteria)
	}
	if update.RepoID != nil {
		item.RepoID = strings.TrimSpace(*update.RepoID)
	}
	if update.Deliverable != nil {
		item.Deliverable = strings.TrimSpace(*update.Deliverable)
	}
	if item.Title == "" || item.Objective == "" {
		return model.PlanItem{}, fmt.Errorf("plan item title and objective must not be empty")
	}
	if item.Deliverable != "code" && item.Deliverable != "report" {
		return model.PlanItem{}, fmt.Errorf("plan item deliverable must be code or report")
	}
	if update.Position != nil {
		if err := mutateOrderedPositionsTx(tx, planItemPositions, planID, orderedPositionMutation{operation: moveOrderedPosition, rowID: item.ID, position: *update.Position}); err != nil {
			return model.PlanItem{}, err
		}
	}
	t := now()
	if _, err := tx.Exec(`UPDATE plan_items SET feature_key=?, objective=?, description=?, acceptance_criteria=?, repo_id=NULLIF(?,''), deliverable=?, updated_at=?, title=? WHERE id=?`,
		item.FeatureKey, item.Objective, item.Description, item.AcceptanceCriteria, item.RepoID, item.Deliverable, t, item.Title, item.ID); err != nil {
		return model.PlanItem{}, err
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, t, planID); err != nil {
		return model.PlanItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PlanItem{}, err
	}
	return s.PlanItem(planID, item.ID)
}

func (s *Store) DeletePlanItem(planID, itemID string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var dispatched string
	if err := tx.QueryRow(`SELECT COALESCE(dispatched_task_id,'') FROM plan_items WHERE plan_id=? AND id=?`, planID, itemID).Scan(&dispatched); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("plan item %q does not exist in plan %s", itemID, planID)
		}
		return err
	}
	if dispatched != "" {
		return fmt.Errorf("plan item %s was already dispatched and cannot be deleted", itemID)
	}
	if _, err := tx.Exec(`DELETE FROM plan_report_inputs WHERE item_id=?`, itemID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plan_prerequisites WHERE item_id=?`, itemID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM plan_items WHERE id=?`, itemID); err != nil {
		if isSQLiteConstraint(err, sqliteConstraintForeignKey) {
			return fmt.Errorf("plan item %s is still a prerequisite; remove downstream relations first", itemID)
		}
		return err
	}
	if err := mutateOrderedPositionsTx(tx, planItemPositions, planID, orderedPositionMutation{operation: normalizeOrderedPositions}); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, now(), planID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddPlanPrerequisite(planID, itemID, prerequisiteItemID string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requirePlanTx(tx, planID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plan_items WHERE id=? AND plan_id=?`, itemID, planID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("plan item %q does not exist in plan %s", itemID, planID)
	}
	if err := insertPlanPrerequisiteTx(tx, itemID, prerequisiteItemID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, now(), planID); err != nil {
		return err
	}
	return tx.Commit()
}

func planPrerequisiteCycleTx(tx *sql.Tx, itemID, prerequisiteItemID string) (bool, error) {
	var samePlan, mutable int
	err := tx.QueryRow(`SELECT CASE WHEN item.plan_id=prerequisite.plan_id THEN 1 ELSE 0 END,
		CASE WHEN item.dispatched_task_id IS NULL THEN 1 ELSE 0 END
		FROM plan_items item JOIN plan_items prerequisite ON prerequisite.id=? WHERE item.id=?`, prerequisiteItemID, itemID).Scan(&samePlan, &mutable)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if samePlan == 0 || mutable == 0 {
		return false, nil
	}
	var cycle int
	err = tx.QueryRow(`WITH RECURSIVE reach(id) AS (
		SELECT ?
		UNION
		SELECT prerequisite_item_id FROM plan_prerequisites JOIN reach ON item_id=reach.id
	)
	SELECT EXISTS(SELECT 1 FROM reach WHERE id=?)`, prerequisiteItemID, itemID).Scan(&cycle)
	return cycle != 0, err
}

func (s *Store) RemovePlanPrerequisite(planID, itemID, prerequisiteItemID string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM plan_prerequisites WHERE item_id=? AND prerequisite_item_id=?
		AND EXISTS(SELECT 1 FROM plan_items WHERE id=? AND plan_id=?)`, itemID, prerequisiteItemID, itemID, planID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("prerequisite relation %s -> %s does not exist", itemID, prerequisiteItemID)
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, now(), planID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddPlanReport(planID, itemID, prerequisiteItemID string) (model.PlanReport, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.PlanReport{}, err
	}
	defer tx.Rollback()
	if err := requirePlanTx(tx, planID); err != nil {
		return model.PlanReport{}, err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plan_items WHERE id=? AND plan_id=?`, itemID, planID).Scan(&count); err != nil {
		return model.PlanReport{}, err
	}
	if count == 0 {
		return model.PlanReport{}, fmt.Errorf("plan item %q does not exist in plan %s", itemID, planID)
	}
	t := time.Now().UTC()
	if err := insertPlanReportTx(tx, itemID, prerequisiteItemID, t); err != nil {
		return model.PlanReport{}, err
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, stamp(t), planID); err != nil {
		return model.PlanReport{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PlanReport{}, err
	}
	item, err := s.PlanItem(planID, itemID)
	if err != nil {
		return model.PlanReport{}, err
	}
	for _, report := range item.Reports {
		if report.PrerequisiteItemID == prerequisiteItemID {
			return report, nil
		}
	}
	return model.PlanReport{}, fmt.Errorf("plan report was not created")
}

func (s *Store) SelectPlanReport(planID, itemID, prerequisiteItemID, artifactID, driverID string) (model.PlanReport, error) {
	t := now()
	if err := setPlanReportSelection(s.db, planID, itemID, prerequisiteItemID, artifactID, driverID, t); err != nil {
		return model.PlanReport{}, err
	}
	_, _ = s.db.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, t, planID)
	item, err := s.PlanItem(planID, itemID)
	if err != nil {
		return model.PlanReport{}, err
	}
	for _, report := range item.Reports {
		if report.PrerequisiteItemID == prerequisiteItemID {
			return report, nil
		}
	}
	return model.PlanReport{}, fmt.Errorf("selected plan report disappeared")
}

type planReportExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

func setPlanReportSelection(execer planReportExecer, planID, itemID, prerequisiteItemID, artifactID, driverID, selectedAt string) error {
	result, err := execer.Exec(`UPDATE plan_report_inputs SET artifact_id=?, selected_by_driver_id=?, selected_at=?, updated_at=?
		WHERE item_id=? AND prerequisite_item_id=? AND EXISTS(SELECT 1 FROM plan_items WHERE id=? AND plan_id=?)`,
		artifactID, driverID, selectedAt, selectedAt, itemID, prerequisiteItemID, itemID, planID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("report relation from item %s to prerequisite %s does not exist; add it first", itemID, prerequisiteItemID)
	}
	return nil
}

func (s *Store) MovePlanReport(planID, itemID, prerequisiteItemID string, position int) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := mutateOrderedPositionsTx(tx, planReportPositions, itemID, orderedPositionMutation{operation: moveOrderedPosition, rowID: prerequisiteItemID, position: position}); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, now(), planID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemovePlanReport(planID, itemID, prerequisiteItemID string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM plan_report_inputs WHERE item_id=? AND prerequisite_item_id=?`, itemID, prerequisiteItemID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("report relation from item %s to prerequisite %s does not exist", itemID, prerequisiteItemID)
	}
	if err := mutateOrderedPositionsTx(tx, planReportPositions, itemID, orderedPositionMutation{operation: normalizeOrderedPositions}); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, now(), planID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) VerifiedArtifact(id string) (model.VerifiedArtifact, error) {
	artifact, err := scanVerifiedArtifact(s.db.QueryRow(s.verifiedArtifactSelect()+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.VerifiedArtifact{}, fmt.Errorf("verified artifact %q does not exist", id)
	}
	return artifact, err
}

func (s *Store) DispatchPlanItem(planID, itemID, driverID string) (model.Task, error) {
	return s.DispatchPlanItemWithSelections(planID, itemID, driverID, nil)
}

func (s *Store) DispatchPlanItemWithSelections(planID, itemID, driverID string, selections []model.PlanReportSelection) (model.Task, error) {
	strictSelections := selections != nil
	expected := make(map[string]model.PlanReportSelection, len(selections))
	for _, selection := range selections {
		selection.PrerequisiteItemID = strings.TrimSpace(selection.PrerequisiteItemID)
		selection.ProducerTaskID = strings.TrimSpace(selection.ProducerTaskID)
		selection.ArtifactID = strings.TrimSpace(selection.ArtifactID)
		if selection.PrerequisiteItemID == "" || selection.ProducerTaskID == "" || selection.ArtifactID == "" {
			return model.Task{}, fmt.Errorf("automatic plan report selection is incomplete")
		}
		if _, exists := expected[selection.PrerequisiteItemID]; exists {
			return model.Task{}, fmt.Errorf("plan report for prerequisite %s was selected more than once", selection.PrerequisiteItemID)
		}
		expected[selection.PrerequisiteItemID] = selection
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	var item model.PlanItem
	var planDriver, dispatched string
	if err := tx.QueryRow(`SELECT l.driver_id, i.feature_key, i.title, i.objective, i.description, i.acceptance_criteria,
		COALESCE(i.repo_id,''), i.deliverable, COALESCE(i.dispatched_task_id,'')
		FROM plan_items i JOIN plans l ON l.id=i.plan_id WHERE i.plan_id=? AND i.id=?`, planID, itemID).
		Scan(&planDriver, &item.FeatureKey, &item.Title, &item.Objective, &item.Description, &item.AcceptanceCriteria, &item.RepoID, &item.Deliverable, &dispatched); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Task{}, fmt.Errorf("plan item %q does not exist in plan %s", itemID, planID)
		}
		return model.Task{}, err
	}
	if planDriver != driverID {
		return model.Task{}, fmt.Errorf("plan %s is owned by %s, not %s", planID, planDriver, driverID)
	}
	if dispatched != "" {
		return model.Task{}, fmt.Errorf("plan item %s was already dispatched as task %s", itemID, dispatched)
	}
	if item.Title == "" || item.FeatureKey == "" || item.Objective == "" || item.AcceptanceCriteria == "" || item.RepoID == "" {
		return model.Task{}, fmt.Errorf("plan item %s is not ready; inspect its readiness reasons", itemID)
	}
	var inadequate int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plan_prerequisites p
		JOIN plan_items prerequisite ON prerequisite.id=p.prerequisite_item_id
		LEFT JOIN tasks t ON t.id=prerequisite.dispatched_task_id
		LEFT JOIN task_landing_projections lp ON lp.task_id=t.id
		WHERE p.item_id=? AND (prerequisite.dispatched_task_id IS NULL OR t.status<>? OR COALESCE(lp.landed,0)<>1)`, itemID, model.TaskStatusDone).Scan(&inadequate); err != nil {
		return model.Task{}, err
	}
	if inadequate != 0 {
		return model.Task{}, fmt.Errorf("plan item %s has %d inadequate prerequisites; inspect its readiness reasons", itemID, inadequate)
	}
	t := time.Now().UTC()
	rows, err := tx.Query(`SELECT input.prerequisite_item_id, COALESCE(prerequisite.dispatched_task_id,''),
		COALESCE(input.artifact_id,''), COALESCE(va.producer_task_id,'')
		FROM plan_report_inputs input
		JOIN plan_items prerequisite ON prerequisite.id=input.prerequisite_item_id
		LEFT JOIN verified_artifacts va ON va.id=input.artifact_id
		WHERE input.item_id=? ORDER BY input.position`, itemID)
	if err != nil {
		return model.Task{}, err
	}
	type dispatchReport struct {
		prerequisiteItemID string
		producerTaskID     string
		artifactID         string
		selectedProducerID string
	}
	pending := make([]dispatchReport, 0)
	for rows.Next() {
		var input dispatchReport
		if err := rows.Scan(&input.prerequisiteItemID, &input.producerTaskID, &input.artifactID, &input.selectedProducerID); err != nil {
			rows.Close()
			return model.Task{}, err
		}
		pending = append(pending, input)
	}
	if err := rows.Close(); err != nil {
		return model.Task{}, err
	}
	if len(pending) > model.MaxTaskReportInputs {
		return model.Task{}, fmt.Errorf("plan item %s selects more than %d report inputs", itemID, model.MaxTaskReportInputs)
	}
	inputs := make([]model.ReportArtifactSelection, 0, len(pending))
	for _, input := range pending {
		if input.artifactID != "" {
			if input.selectedProducerID == "" || input.selectedProducerID != input.producerTaskID {
				return model.Task{}, fmt.Errorf("plan item %s has a stale report input for prerequisite %s", itemID, input.prerequisiteItemID)
			}
			if err := eligibleVerifiedReportTx(tx, input.selectedProducerID, input.artifactID); err != nil {
				return model.Task{}, err
			}
			inputs = append(inputs, model.ReportArtifactSelection{ProducerTaskID: input.selectedProducerID, ArtifactID: input.artifactID})
			continue
		}
		if input.producerTaskID == "" {
			return model.Task{}, fmt.Errorf("plan item %s report prerequisite %s has no dispatched task", itemID, input.prerequisiteItemID)
		}
		artifactIDs, err := eligibleVerifiedReportIDsTx(tx, input.producerTaskID)
		if err != nil {
			return model.Task{}, err
		}
		if len(artifactIDs) != 1 {
			return model.Task{}, fmt.Errorf("plan item %s report prerequisite %s has %d eligible verified reports; exactly one is required", itemID, input.prerequisiteItemID, len(artifactIDs))
		}
		artifactID := artifactIDs[0]
		if strictSelections {
			selection, exists := expected[input.prerequisiteItemID]
			if !exists || selection.ProducerTaskID != input.producerTaskID || selection.ArtifactID != artifactID {
				return model.Task{}, fmt.Errorf("plan item %s report input identity changed before dispatch", itemID)
			}
			delete(expected, input.prerequisiteItemID)
		}
		if err := setPlanReportSelection(tx, planID, itemID, input.prerequisiteItemID, artifactID, driverID, stamp(t)); err != nil {
			return model.Task{}, err
		}
		inputs = append(inputs, model.ReportArtifactSelection{ProducerTaskID: input.producerTaskID, ArtifactID: artifactID})
	}
	if len(expected) != 0 {
		return model.Task{}, fmt.Errorf("plan item %s report relations changed before dispatch", itemID)
	}
	objective := item.Objective
	if item.Description != "" {
		objective += "\n\n" + item.Description
	}
	task := model.Task{ID: NewID("task"), RepoID: item.RepoID, FeatureKey: item.FeatureKey, Title: item.Title, DriverID: driverID, Objective: objective,
		AcceptanceCriteria: item.AcceptanceCriteria, Deliverable: item.Deliverable, Status: model.TaskStatusQueued, CreatedAt: t, UpdatedAt: t}
	if err := insertTaskTx(tx, task, t); err != nil {
		return model.Task{}, err
	}
	for index, input := range inputs {
		if _, err := tx.Exec(`INSERT INTO task_report_inputs(target_task_id, position, artifact_id, attached_by_driver_id, attached_at) VALUES(?, ?, ?, ?, ?)`,
			task.ID, index+1, input.ArtifactID, driverID, stamp(t)); err != nil {
			return model.Task{}, err
		}
	}
	result, err := tx.Exec(`UPDATE plan_items SET dispatched_task_id=?, updated_at=? WHERE id=? AND dispatched_task_id IS NULL`, task.ID, stamp(t), itemID)
	if err != nil {
		return model.Task{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return model.Task{}, fmt.Errorf("plan item %s was dispatched concurrently", itemID)
	}
	if _, err := tx.Exec(`UPDATE plans SET updated_at=? WHERE id=?`, stamp(t), planID); err != nil {
		return model.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Task{}, err
	}
	return s.Task(task.ID)
}

const planItemSelect = `SELECT i.id, i.plan_id, i.position, i.feature_key, i.title, i.objective, i.description,
	i.acceptance_criteria, COALESCE(i.repo_id,''), COALESCE(r.name,''), COALESCE(r.path,''), i.deliverable,
	COALESCE(i.dispatched_task_id,''), i.created_at, i.updated_at
	FROM plan_items i LEFT JOIN repos r ON r.id=i.repo_id`

func (s *Store) planItems(planID string) ([]model.PlanItem, error) {
	rows, err := s.db.Query(planItemSelect+` WHERE i.plan_id=? ORDER BY i.position`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.PlanItem, 0)
	for rows.Next() {
		item, err := scanPlanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range items {
		if err := s.hydratePlanItem(&items[index]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func scanPlanItem(scanner interface{ Scan(...any) error }) (model.PlanItem, error) {
	var item model.PlanItem
	var created, updated string
	if err := scanner.Scan(&item.ID, &item.PlanID, &item.Position, &item.FeatureKey, &item.Title, &item.Objective, &item.Description,
		&item.AcceptanceCriteria, &item.RepoID, &item.RepoName, &item.RepoPath, &item.Deliverable, &item.DispatchedTaskID,
		&created, &updated); err != nil {
		return model.PlanItem{}, err
	}
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	return item, nil
}

func (s *Store) hydratePlanItem(item *model.PlanItem) error {
	if item.DispatchedTaskID != "" {
		task, err := s.Task(item.DispatchedTaskID)
		if err != nil {
			return err
		}
		item.DispatchedTask = &task
	}
	rows, err := s.db.Query(`SELECT prerequisite.id, prerequisite.title, prerequisite.objective, prerequisite.position, COALESCE(prerequisite.dispatched_task_id,'')
		FROM plan_prerequisites relation
		JOIN plan_items prerequisite ON prerequisite.id=relation.prerequisite_item_id
		WHERE relation.item_id=? ORDER BY prerequisite.position, prerequisite.id`, item.ID)
	if err != nil {
		return err
	}
	item.Prerequisites = make([]model.PlanPrerequisite, 0)
	for rows.Next() {
		var prerequisite model.PlanPrerequisite
		if err := rows.Scan(&prerequisite.ItemID, &prerequisite.Title, &prerequisite.Objective, &prerequisite.Position, &prerequisite.DispatchedTaskID); err != nil {
			rows.Close()
			return err
		}
		if prerequisite.DispatchedTaskID != "" {
			task, err := s.Task(prerequisite.DispatchedTaskID)
			if err != nil {
				rows.Close()
				return err
			}
			prerequisite.Task = &task
		}
		item.Prerequisites = append(item.Prerequisites, prerequisite)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = s.db.Query(`SELECT input.prerequisite_item_id, input.position, COALESCE(input.artifact_id,''),
		input.selected_by_driver_id, input.selected_at
		FROM plan_report_inputs input WHERE input.item_id=? ORDER BY input.position`, item.ID)
	if err != nil {
		return err
	}
	item.Reports = make([]model.PlanReport, 0)
	for rows.Next() {
		var input model.PlanReport
		var selected sql.NullString
		if err := rows.Scan(&input.PrerequisiteItemID, &input.Position, &input.ArtifactID, &input.SelectedByDriverID, &selected); err != nil {
			rows.Close()
			return err
		}
		if selected.Valid {
			value := parseTime(selected.String)
			input.SelectedAt = &value
		}
		if input.ArtifactID != "" {
			artifact, err := s.VerifiedArtifact(input.ArtifactID)
			if err != nil {
				rows.Close()
				return err
			}
			input.Artifact = &artifact
		}
		item.Reports = append(item.Reports, input)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return s.projectPlanReadiness(item)
}

func (s *Store) projectPlanReadiness(item *model.PlanItem) error {
	readiness := model.PlanReadiness{Reasons: make([]model.PlanReason, 0), RequiredActions: make([]string, 0), AvailableEvidence: make([]model.TaskEvidence, 0)}
	add := func(code, message, prerequisiteID string, action bool) {
		readiness.Reasons = append(readiness.Reasons, model.PlanReason{Code: code, Message: message, PrerequisiteItemID: prerequisiteID})
		if action {
			readiness.RequiredActions = append(readiness.RequiredActions, message)
		}
	}
	if item.DispatchedTaskID != "" {
		add("already_dispatched", fmt.Sprintf("item was already dispatched as task %s", item.DispatchedTaskID), "", false)
	}
	if item.RepoID == "" {
		add("repo_required", "select an intended registered repository", "", true)
	}
	if strings.TrimSpace(item.FeatureKey) == "" {
		add("feature_required", "choose a stable feature key", "", true)
	}
	if strings.TrimSpace(item.AcceptanceCriteria) == "" {
		add("acceptance_required", "define goals or acceptance criteria", "", true)
	}
	if item.DispatchedTaskID == "" && item.RepoID != "" && item.FeatureKey != "" {
		var existing string
		err := s.db.QueryRow(`SELECT id FROM tasks WHERE repo_id=? AND feature_key=?`, item.RepoID, item.FeatureKey).Scan(&existing)
		if err == nil {
			add("feature_conflict", fmt.Sprintf("feature key %q is already used by task %s", item.FeatureKey, existing), "", true)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	prerequisites := make(map[string]model.PlanPrerequisite, len(item.Prerequisites))
	for index := range item.Prerequisites {
		prerequisite := &item.Prerequisites[index]
		prerequisites[prerequisite.ItemID] = *prerequisite
		if prerequisite.DispatchedTaskID == "" || prerequisite.Task == nil {
			add("prerequisite_not_dispatched", fmt.Sprintf("prerequisite item %s has not been manually dispatched", prerequisite.ItemID), prerequisite.ItemID, false)
			continue
		}
		evidence := model.TaskEvidence{TaskID: prerequisite.Task.ID, Title: prerequisite.Task.Title, FeatureKey: prerequisite.Task.FeatureKey,
			Status: prerequisite.Task.Status, Landed: prerequisite.Task.Landed, ArtifactRef: prerequisite.Task.ArtifactRef, CurrentAttemptID: prerequisite.Task.CurrentAttemptID}
		if prerequisite.Task.Status != model.TaskStatusDone {
			add("prerequisite_not_done", fmt.Sprintf("prerequisite task %s is %s, not done; decide whether to wait, retry cleanly, or relaunch its verified worktree", prerequisite.Task.ID, prerequisite.Task.Status), prerequisite.ItemID, true)
		} else if !prerequisite.Task.Landed {
			add("prerequisite_not_verified", fmt.Sprintf("prerequisite task %s is done but lacks verified landing or report evidence; decide whether to verify or rerun it", prerequisite.Task.ID), prerequisite.ItemID, true)
		}
		if artifact, err := s.EligibleVerifiedReport(prerequisite.Task.ID); err == nil {
			evidence.VerifiedReport = &artifact
		}
		prerequisite.Evidence = &evidence
		readiness.AvailableEvidence = append(readiness.AvailableEvidence, evidence)
	}
	if len(item.Reports) > model.MaxTaskReportInputs {
		add("too_many_reports", fmt.Sprintf("remove report inputs until at most %d remain", model.MaxTaskReportInputs), "", true)
	}
	for index := range item.Reports {
		report := &item.Reports[index]
		prerequisite, exists := prerequisites[report.PrerequisiteItemID]
		if !exists {
			add("report_prerequisite_missing", fmt.Sprintf("report input %d references missing prerequisite item %s", report.Position, report.PrerequisiteItemID), report.PrerequisiteItemID, true)
			continue
		}
		if report.Artifact == nil {
			add("report_not_selected", fmt.Sprintf("select a verified report for report input %d from prerequisite item %s", report.Position, report.PrerequisiteItemID), report.PrerequisiteItemID, true)
			continue
		}
		if prerequisite.Task == nil {
			report.Stale = true
			report.StaleReason = "prerequisite has no dispatched task"
		} else {
			current, err := s.EligibleVerifiedReport(prerequisite.Task.ID)
			if err != nil || current.ID != report.Artifact.ID {
				report.Stale = true
				if err != nil {
					report.StaleReason = err.Error()
				} else {
					report.StaleReason = fmt.Sprintf("selected artifact %s was replaced by current artifact %s", report.Artifact.ID, current.ID)
				}
			}
		}
		if report.Stale {
			add("report_selection_stale", fmt.Sprintf("report input %d must be reselected: %s", report.Position, report.StaleReason), report.PrerequisiteItemID, true)
		}
	}
	readiness.Ready = len(readiness.Reasons) == 0
	item.Readiness = readiness
	return nil
}

func requirePlanTx(tx *sql.Tx, planID string) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plans WHERE id=?`, planID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("plan %q does not exist", planID)
	}
	return nil
}
