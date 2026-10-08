package cli

import (
	"testing"

	"github.com/spf13/cobra"

	"shephrd/internal/model"
)

func TestBaseSelectionFlagsAreExplicitAndMutuallyExclusive(t *testing.T) {
	commit := "0123456789012345678901234567890123456789"
	for _, test := range []struct {
		name      string
		args      []string
		want      model.BaseSelection
		provided  bool
		wantError bool
	}{
		{name: "default", want: model.DefaultBaseSelection()},
		{name: "branch", args: []string{"--base-branch", "feature/base"}, want: model.BaseSelection{Strategy: model.BaseStrategyBranch, Ref: "feature/base"}, provided: true},
		{name: "task", args: []string{"--base-task", "task-1"}, want: model.BaseSelection{Strategy: model.BaseStrategyTask, Ref: "task-1"}, provided: true},
		{name: "commit", args: []string{"--base-commit", commit}, want: model.BaseSelection{Strategy: model.BaseStrategyCommit, Ref: commit}, provided: true},
		{name: "multiple", args: []string{"--base-branch", "main", "--base-task", "task-1"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := &cobra.Command{}
			var branch, task, selectedCommit string
			command.Flags().StringVar(&branch, "base-branch", "", "")
			command.Flags().StringVar(&task, "base-task", "", "")
			command.Flags().StringVar(&selectedCommit, "base-commit", "", "")
			if err := command.ParseFlags(test.args); err != nil {
				t.Fatal(err)
			}
			got, provided, err := baseSelectionFromFlags(command, branch, task, selectedCommit)
			if test.wantError {
				if err == nil {
					t.Fatal("multiple base selections were accepted")
				}
				return
			}
			if err != nil || got != test.want || provided != test.provided {
				t.Fatalf("selection = %+v, provided = %t, err = %v", got, provided, err)
			}
		})
	}
}
