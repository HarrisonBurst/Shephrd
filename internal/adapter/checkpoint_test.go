package adapter

import (
	"strings"
	"testing"
)

func TestCheckpointEnvelopeIsStrictAndBounded(t *testing.T) {
	text := `<shephrd-event>{"type":"checkpoint","payload":"progress","checkpoint":{"schema_version":1,"summary":"progress","completed":["parser"],"next_steps":["tests"],"decisions":[{"decision":"one latest row","reason":"messages retain audit"}],"changed_paths":["internal/store/store.go"],"checks":[{"command":"go test ./...","result":"passed"}],"blockers":[]}}</shephrd-event>`
	event, found, err := ParseEvent(text)
	if err != nil || !found || event.Type != "checkpoint" || event.Checkpoint == nil {
		t.Fatalf("event = %+v, found = %v, err = %v", event, found, err)
	}
	_, _, err = ParseEvent(`<shephrd-event>{"type":"checkpoint","payload":"x","checkpoint":{"schema_version":1,"summary":"x","completed":[],"next_steps":["x"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[],"extra":true}}</shephrd-event>`)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown checkpoint field error = %v", err)
	}
	_, _, err = ParseEvent(`<shephrd-event>{"type":"checkpoint","payload":"x","checkpoint":{"schema_version":1,"summary":"x\u0000","completed":[],"next_steps":["x"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`)
	if err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("NUL error = %v", err)
	}
}

func TestEmptyNextStepsAreOnlyUsableForAnImmediateDone(t *testing.T) {
	text := `<shephrd-event>{"type":"checkpoint","payload":"finished","checkpoint":{"schema_version":1,"summary":"finished","completed":[],"next_steps":[],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	if _, found, err := ParseEvent(text); err != nil || !found {
		t.Fatalf("empty next_steps should parse for terminal follow-up: found=%v err=%v", found, err)
	}
	if _, _, err := ParseEvent(`<shephrd-event>{"type":"checkpoint","payload":"finished","checkpoint":{"schema_version":1,"summary":"finished","completed":[],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`); err == nil {
		t.Fatal("missing next_steps was accepted")
	}
}
