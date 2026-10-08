package runner

import (
	"strings"
	"testing"

	"shephrd/internal/adapter"
)

func TestCandidateEndCursorPrefersPersistedRepairCursorAndFallsBackToEnvelopeCount(t *testing.T) {
	for _, test := range []struct {
		name                          string
		start, count, persisted, want int64
	}{
		{name: "persisted index zero", start: 2, count: 1, persisted: 2, want: 2},
		{name: "persisted index one", start: 2, count: 2, persisted: 3, want: 3},
		{name: "persisted index two", start: 2, count: 3, persisted: 4, want: 4},
		{name: "fallback", start: 2, count: 3, want: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := candidateEndCursor(test.start, int(test.count), test.persisted); got != test.want {
				t.Fatalf("cursor=%d want=%d", got, test.want)
			}
		})
	}
}

func TestHeadlessRepairPromptIsBoundedAndDeterministic(t *testing.T) {
	request := newRepairRequest("candidate")
	diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticSchemaUnknownField, Phase: "adapter", Field: strings.Repeat("x", 300), Message: strings.Repeat("m", 900)}
	first := headlessRepairPrompt(request, diagnostic)
	second := headlessRepairPrompt(request, diagnostic)
	if first != second || len(first) > 2048 || !strings.Contains(first, "correction attempt 1 of 1") || !strings.Contains(first, request.CandidateHash) {
		t.Fatalf("prompt length=%d deterministic=%t prompt=%q", len(first), first == second, first)
	}
}
