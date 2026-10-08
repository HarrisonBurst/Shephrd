package runner

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

func TestHeadlessPiFailureRequiresSettledSuccessfulRecovery(t *testing.T) {
	message := func(reason, text string) map[string]any {
		return map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": reason, "errorMessage": "WebSocket error", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	success := message("stop", "Recovered response")
	settled := map[string]any{"type": "agent_settled"}
	end := map[string]any{"type": "agent_end", "willRetry": false}
	for _, test := range []struct {
		name    string
		events  []map[string]any
		failure string
	}{
		{"exhausted", []map[string]any{end, {"type": "auto_retry_end", "success": false, "finalError": "Exhausted"}, settled}, "Exhausted"},
		{"cancelled", []map[string]any{{"type": "auto_retry_end", "success": false, "finalError": "Retry cancelled"}, settled}, "Retry cancelled"},
		{"no settlement", []map[string]any{success, end}, "WebSocket error"},
		{"retry end is not recovery", []map[string]any{{"type": "auto_retry_end", "success": true}, end, settled}, "WebSocket error"},
		{"abort is not recovery", []map[string]any{message("aborted", ""), {"type": "auto_retry_end", "success": true}, settled}, "WebSocket error"},
		{"later failure wins", []map[string]any{success, end, message("error", ""), settled}, "WebSocket error"},
		{"settled recovery", []map[string]any{success, {"type": "auto_retry_end", "success": true}, end, settled}, ""},
		{"empty successful recovery", []map[string]any{message("stop", ""), end, settled}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			encoder := json.NewEncoder(file)
			for _, event := range append([]map[string]any{message("toolUse", "Completed tool evidence"), message("error", `<shephrd-event>{"type":"done"`)}, test.events...) {
				if err := encoder.Encode(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			attempt := model.Attempt{Harness: "pi", SessionID: "session"}
			cursor := int64(0)
			acknowledged := false
			var progress []string
			turn := headlessTurn{attempt: &attempt, cursor: &cursor, acknowledged: &acknowledged, output: io.Discard, config: HeadlessConfig{State: State{
				Ingest:       func(event model.Event, _ int64) error { progress = append(progress, event.Payload); return nil },
				UpdateCursor: func(int64) error { return nil },
			}}}
			turn.run(adapter.Invocation{Command: "cat", Args: []string{path}})
			if turn.adapterFailure != test.failure || turn.failure != "" || turn.diagnostic != nil || turn.waitErr != nil || len(progress) < 1 || progress[0] != "Completed tool evidence" {
				t.Fatalf("failure=%q control=%q diagnostic=%+v wait=%v progress=%q", turn.adapterFailure, turn.failure, turn.diagnostic, turn.waitErr, progress)
			}
		})
	}
}
