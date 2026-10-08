package model

import "testing"

func TestNormalizeBaseSelection(t *testing.T) {
	commit := "0123456789012345678901234567890123456789"
	for _, test := range []struct {
		name      string
		selection BaseSelection
		want      BaseSelection
		valid     bool
	}{
		{name: "default", selection: BaseSelection{}, want: DefaultBaseSelection(), valid: true},
		{name: "branch", selection: BaseSelection{Strategy: BaseStrategyBranch, Ref: "feature/base"}, want: BaseSelection{Strategy: BaseStrategyBranch, Ref: "feature/base"}, valid: true},
		{name: "task", selection: BaseSelection{Strategy: BaseStrategyTask, Ref: "task-1"}, want: BaseSelection{Strategy: BaseStrategyTask, Ref: "task-1"}, valid: true},
		{name: "commit", selection: BaseSelection{Strategy: BaseStrategyCommit, Ref: commit}, want: BaseSelection{Strategy: BaseStrategyCommit, Ref: commit}, valid: true},
		{name: "abbreviated commit", selection: BaseSelection{Strategy: BaseStrategyCommit, Ref: commit[:12]}},
		{name: "default reference", selection: BaseSelection{Strategy: BaseStrategyDefaultBranch, Ref: "main"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeBaseSelection(test.selection)
			if test.valid {
				if err != nil || got != test.want {
					t.Fatalf("selection = %+v, err = %v, want %+v", got, err, test.want)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid base selection was accepted")
			}
		})
	}
}
