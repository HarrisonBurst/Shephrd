package delivery

import (
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestVerificationRejectsArtifactsOutsideCreationTimeDeliverableContract(t *testing.T) {
	verifier := New(t.TempDir())
	for _, test := range []struct {
		name        string
		deliverable string
		artifact    string
		want        string
	}{
		{name: "code report", deliverable: "code", artifact: "report:/tmp/report.md", want: "code task artifact"},
		{name: "report branch", deliverable: "report", artifact: "branch:shephrd/task", want: "report task artifact"},
		{name: "report PR", deliverable: "report", artifact: "https://github.com/acme/demo/pull/1", want: "report task artifact"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := verifier.Verify(model.Task{Title: "Test task", ID: "task", Deliverable: test.deliverable, ArtifactRef: test.artifact}, model.Repo{}, model.Attempt{}, model.AttemptCheckpoint{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verification error = %v", err)
			}
		})
	}
}
