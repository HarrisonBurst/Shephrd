package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shephrd/internal/extension"
	"shephrd/internal/model"
)

func TestReportAcceptedProtocolIsOneExactVersionedCapability(t *testing.T) {
	capability := ReportAcceptedCapabilityManifest()
	if capability.Name != "lifecycle.report.accepted" || capability.Version != 1 || len(capability.Operations) != 1 || capability.Operations[0] != "handle" {
		t.Fatalf("capability = %+v", capability)
	}
	manifest := extension.Manifest{
		Wire:      extension.WireRange{Major: extension.WireMajor, MinorMin: extension.WireMinor, MinorMax: extension.WireMinor},
		Extension: extension.Identity{ID: "fixture.report-handler", Version: "1.0.0"}, Capabilities: []extension.Capability{capability},
	}
	if err := extension.ValidateManifest(manifest, manifest.Extension.ID, []extension.Capability{capability}); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"before", "after", "subscribe", "wildcard", "worker_event"} {
		candidate := capability
		candidate.Operations = append(candidate.Operations, operation)
		manifest.Capabilities = []extension.Capability{candidate}
		if err := extension.ValidateManifest(manifest, manifest.Extension.ID, []extension.Capability{capability}); err == nil {
			t.Fatalf("operation %q was accepted", operation)
		}
	}
}

func TestReportAcceptedRequestIsBoundedAndNonAuthoritative(t *testing.T) {
	accepted := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	request, err := reportAcceptedRequest(
		model.Task{ID: "task", RepoID: "repo", RepoName: "demo", Title: "Investigation", FeatureKey: "investigate", Deliverable: "report", CompletionProvenance: model.CompletionProvenanceWorkerDone},
		model.Attempt{ID: "attempt", RunGeneration: 2},
		model.VerifiedArtifact{ID: "artifact", Kind: "report", OriginalRef: "report:/data/task/report.md", SHA256: strings.Repeat("a", 64), SizeBytes: 12, SnapshotPath: "/data/artifacts/sha256/a.md", VerifiedAt: accepted, AcceptedEventID: "event", AcceptedEventName: ReportAcceptedEvent, AcceptedEventVersion: 1},
		model.ReportLifecycleInvocation{ID: "invocation", HandlerName: "memory"},
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, forbidden := range []string{"original_ref", "objective", "acceptance_criteria", "driver_id", "database", "worker_event", "task_state", "release", "landing"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("request contains forbidden authority field %q: %s", forbidden, body)
		}
	}
	if request.Event.Name != ReportAcceptedEvent || request.Event.Version != ReportAcceptedEventVersion || request.Report.SnapshotPath == "" || request.Task.RunGeneration != 2 {
		t.Fatalf("request = %+v", request)
	}
}

func TestReportAcceptedResultRequiresBoundedAnnotationOrReceipt(t *testing.T) {
	valid := []ReportAcceptedResult{
		{Annotation: "bounded summary"},
		{Receipt: &Receipt{System: "memory", ID: "receipt-1"}},
		{Annotation: "bounded summary", Receipt: &Receipt{System: "memory", ID: "receipt-1"}},
	}
	for _, result := range valid {
		if err := ValidateReportAcceptedResult(result); err != nil {
			t.Fatalf("valid result %+v: %v", result, err)
		}
	}
	invalid := []ReportAcceptedResult{
		{},
		{Annotation: strings.Repeat("x", 4097)},
		{Annotation: "line\nbreak"},
		{Receipt: &Receipt{System: "", ID: "receipt"}},
		{Receipt: &Receipt{System: "memory", ID: ""}},
	}
	for _, result := range invalid {
		if err := ValidateReportAcceptedResult(result); err == nil {
			t.Fatalf("invalid result accepted: %+v", result)
		}
	}
	var decoded ReportAcceptedResult
	if err := extension.StrictDecode([]byte(`{"annotation":"summary","task_state":"done"}`), &decoded); err == nil {
		t.Fatal("handler result gained task state authority")
	}
}
