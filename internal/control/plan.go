package control

import (
	"fmt"
	"strings"

	"shephrd/internal/model"
)

func (s Service) Plan(ref, driverID string) (model.Plan, error) {
	plan, err := s.Store.Plan(ref, driverID)
	if err != nil {
		return model.Plan{}, err
	}
	for itemIndex := range plan.Items {
		item := &plan.Items[itemIndex]
		for reportIndex := range item.Reports {
			report := &item.Reports[reportIndex]
			if report.Artifact == nil || report.Stale {
				continue
			}
			if _, err := s.Verifier.ReadVerifiedReport(*report.Artifact); err != nil {
				report.Stale = true
				report.StaleReason = err.Error()
				message := fmt.Sprintf("report input %d artifact %s failed immutable snapshot verification: %s", report.Position, report.Artifact.ID, err)
				item.Readiness.Reasons = append(item.Readiness.Reasons, model.PlanReason{Code: "report_artifact_invalid", Message: message, PrerequisiteItemID: report.PrerequisiteItemID})
				item.Readiness.RequiredActions = append(item.Readiness.RequiredActions, message)
				item.Readiness.Ready = false
			}
		}
	}
	return plan, nil
}

func (s Service) SelectPlanReport(planRef, itemID, prerequisiteItemID, driverID string) (model.PlanReport, error) {
	plan, err := s.Plan(planRef, driverID)
	if err != nil {
		return model.PlanReport{}, err
	}
	var item *model.PlanItem
	for index := range plan.Items {
		if plan.Items[index].ID == itemID {
			item = &plan.Items[index]
			break
		}
	}
	if item == nil {
		return model.PlanReport{}, fmt.Errorf("plan item %q does not exist in plan %s", itemID, plan.ID)
	}
	var prerequisite *model.PlanPrerequisite
	for index := range item.Prerequisites {
		if item.Prerequisites[index].ItemID == prerequisiteItemID {
			prerequisite = &item.Prerequisites[index]
			break
		}
	}
	if prerequisite == nil {
		return model.PlanReport{}, fmt.Errorf("item %s does not declare prerequisite item %s", itemID, prerequisiteItemID)
	}
	if prerequisite.Task == nil {
		return model.PlanReport{}, fmt.Errorf("prerequisite item %s has not been dispatched", prerequisiteItemID)
	}
	artifact, err := s.Store.EligibleVerifiedReport(prerequisite.Task.ID)
	if err != nil {
		return model.PlanReport{}, err
	}
	if _, err := s.Verifier.ReadVerifiedReport(artifact); err != nil {
		return model.PlanReport{}, err
	}
	return s.Store.SelectPlanReport(plan.ID, itemID, prerequisiteItemID, artifact.ID, driverID)
}

func (s Service) DispatchPlanItem(planRef, itemID, driverID string) (model.PlanDispatch, error) {
	plan, err := s.Plan(planRef, driverID)
	if err != nil {
		return model.PlanDispatch{}, err
	}
	var item *model.PlanItem
	for index := range plan.Items {
		if plan.Items[index].ID == itemID {
			item = &plan.Items[index]
			break
		}
	}
	if item == nil {
		return model.PlanDispatch{}, fmt.Errorf("plan item %q does not exist in plan %s", itemID, plan.ID)
	}
	blocking := make([]string, 0, len(item.Readiness.Reasons))
	for _, reason := range item.Readiness.Reasons {
		if reason.Code != "report_not_selected" {
			blocking = append(blocking, reason.Code+": "+reason.Message)
		}
	}
	if len(blocking) != 0 {
		return model.PlanDispatch{}, fmt.Errorf("plan item %s is not ready: %s", item.ID, strings.Join(blocking, "; "))
	}
	selections := make([]model.PlanReportSelection, 0)
	for _, report := range item.Reports {
		if report.Artifact != nil {
			continue
		}
		var prerequisite *model.PlanPrerequisite
		for index := range item.Prerequisites {
			if item.Prerequisites[index].ItemID == report.PrerequisiteItemID {
				prerequisite = &item.Prerequisites[index]
				break
			}
		}
		if prerequisite == nil || prerequisite.Task == nil {
			return model.PlanDispatch{}, fmt.Errorf("plan item %s report prerequisite %s is not eligible", item.ID, report.PrerequisiteItemID)
		}
		artifact, err := s.Store.EligibleVerifiedReport(prerequisite.Task.ID)
		if err != nil {
			return model.PlanDispatch{}, err
		}
		if _, err := s.Verifier.ReadVerifiedReport(artifact); err != nil {
			return model.PlanDispatch{}, err
		}
		selections = append(selections, model.PlanReportSelection{PrerequisiteItemID: report.PrerequisiteItemID, ProducerTaskID: prerequisite.Task.ID, ArtifactID: artifact.ID})
	}
	task, err := s.Store.DispatchPlanItemWithSelections(plan.ID, item.ID, driverID, selections)
	if err != nil {
		return model.PlanDispatch{}, err
	}
	updated, err := s.Plan(plan.ID, driverID)
	if err != nil {
		return model.PlanDispatch{}, fmt.Errorf("plan dispatch succeeded as task %s, but refreshing the plan projection failed: %w; inspect with 'shephrd task inspect %s'", task.ID, err, task.ID)
	}
	for _, candidate := range updated.Items {
		if candidate.ID == item.ID {
			return model.PlanDispatch{Plan: updated, Item: candidate, Task: task, Readiness: candidate.Readiness}, nil
		}
	}
	return model.PlanDispatch{}, fmt.Errorf("plan dispatch succeeded as task %s, but item %s was missing from the refreshed plan projection; inspect with 'shephrd task inspect %s'", task.ID, item.ID, task.ID)
}
