package execution

import (
	"testing"

	"shephrd/internal/coord"
)

func TestTabLabelUsesTitleForDuplicateTasks(t *testing.T) {
	first := &coord.Task{Ref: "t_1", Role: "worker", Title: "Same title"}
	second := &coord.Task{Ref: "t_2", Role: "driver", Title: "Same title"}
	if got := TabLabel(first); got != "Same title" || TabLabel(second) != got {
		t.Fatalf("duplicate title labels: %q, %q", got, TabLabel(second))
	}
}

func TestTabLabelFallsBackToTaskIDForEmptyTitle(t *testing.T) {
	for _, title := range []string{"", "   "} {
		if got := TabLabel(&coord.Task{Ref: "t_3", Title: title}); got != "t_3" {
			t.Fatalf("title %q label: %q", title, got)
		}
	}
}
