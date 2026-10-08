package control

import "testing"

func TestWorkerLabelIncludesRepositoryName(t *testing.T) {
	want := "Shephrd-Validate-herdr-executable-resolution"
	if got := WorkerLabel("Shephrd", "Validate", "herdr-executable-resolution", "task_66d46566545f"); got != want {
		t.Fatalf("WorkerLabel() = %q, want %q", got, want)
	}
}

func TestWorkerSessionNameRetainsTaskIdentity(t *testing.T) {
	want := "Shephrd: demo/Human-title [feature:short]"
	if got := WorkerSessionName("demo", "Human title", "feature", "task_short"); got != want {
		t.Fatalf("WorkerSessionName() = %q, want %q", got, want)
	}
}

func TestSubdriverLabelNamesRegisteredRepositoryOrGeneral(t *testing.T) {
	for repoName, want := range map[string]string{"Shephrd": "Sub-driver: Shephrd", "": "Sub-driver: General", " \x1b[31mdemo\x1b[0m\ttab ": "Sub-driver: demo tab"} {
		if got := SubdriverLabel(repoName); got != want {
			t.Fatalf("SubdriverLabel(%q) = %q, want %q", repoName, got, want)
		}
	}
}
