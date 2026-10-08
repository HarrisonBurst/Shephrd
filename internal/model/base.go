package model

import (
	"fmt"
	"strings"
)

const (
	BaseStrategyDefaultBranch = "default_branch"
	BaseStrategyBranch        = "branch"
	BaseStrategyTask          = "task"
	BaseStrategyCommit        = "commit"
)

type BaseSelection struct {
	Strategy string `json:"strategy"`
	Ref      string `json:"ref,omitempty"`
}

func DefaultBaseSelection() BaseSelection {
	return BaseSelection{Strategy: BaseStrategyDefaultBranch}
}

func NormalizeBaseSelection(selection BaseSelection) (BaseSelection, error) {
	selection.Strategy = strings.TrimSpace(selection.Strategy)
	selection.Ref = strings.TrimSpace(selection.Ref)
	if selection.Strategy == "" {
		selection.Strategy = BaseStrategyDefaultBranch
	}
	switch selection.Strategy {
	case BaseStrategyDefaultBranch:
		if selection.Ref != "" {
			return BaseSelection{}, fmt.Errorf("default branch base selection must not include a reference")
		}
	case BaseStrategyBranch, BaseStrategyTask, BaseStrategyCommit:
		if selection.Ref == "" {
			return BaseSelection{}, fmt.Errorf("%s base selection requires a reference", selection.Strategy)
		}
		if len(selection.Ref) > 4096 || strings.IndexByte(selection.Ref, 0) >= 0 {
			return BaseSelection{}, fmt.Errorf("%s base reference is invalid", selection.Strategy)
		}
		if selection.Strategy == BaseStrategyCommit && !ValidCommitID(selection.Ref) {
			return BaseSelection{}, fmt.Errorf("explicit base commit %q must be a full lowercase Git object ID", selection.Ref)
		}
	default:
		return BaseSelection{}, fmt.Errorf("base strategy %q is invalid", selection.Strategy)
	}
	return selection, nil
}

func ValidCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
