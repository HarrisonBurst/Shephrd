package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"shephrd/internal/config"
	"shephrd/internal/model"
)

type Invocation struct {
	Command     string
	Args        []string
	Dir         string
	Environment map[string]string
}

type EventAuthority uint8

const (
	EventAuthorityNone EventAuthority = iota
	EventAuthorityAssignedWorker
	EventAuthorityNestedWorker
)

type Parsed struct {
	Acknowledged   bool
	SessionID      string
	Text           string
	EventAuthority EventAuthority
	Terminal       bool
	Failure        string
}

func Validate(harness string) error {
	if !config.ValidHarness(harness) {
		return fmt.Errorf("harness must be claude-code, pi, or codex, got %q", harness)
	}
	switch harness {
	case "claude-code":
		return require("claude")
	case "pi":
		return require("pi")
	case "codex":
		return require("codex")
	}
	return nil
}

func NewSessionID(harness string) string {
	if harness == "codex" {
		return "pending:" + uuid.NewString()
	}
	return uuid.NewString()
}

func BuildNamed(harness string, attempt model.Attempt, prompt string, resume bool, displayName string) (Invocation, error) {
	return BuildNamedForRuntime(harness, attempt, prompt, resume, displayName, "headless", "")
}

func BuildNamedForRuntime(harness string, attempt model.Attempt, prompt string, resume bool, displayName, runtime, extensionPath string) (Invocation, error) {
	if err := Validate(harness); err != nil {
		return Invocation{}, err
	}
	switch harness {
	case "claude-code":
		args := []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
		if runtime != "headless" {
			if extensionPath == "" {
				return Invocation{}, fmt.Errorf("interactive Claude Code terminal invocation requires bridge settings")
			}
			args = []string{"--dangerously-skip-permissions", "--settings", extensionPath}
		}
		if attempt.Model != "" {
			args = append(args, "--model", attempt.Model)
		}
		if resume {
			args = append(args, "--resume", attempt.SessionID)
		} else {
			args = append(args, "--session-id", attempt.SessionID)
			if displayName != "" {
				args = append(args, "--name", displayName)
			}
		}
		args = append(args, prompt)
		return Invocation{Command: "claude", Args: args, Dir: attempt.WorktreePath}, nil
	case "pi":
		args := []string{"--mode", "json", "--print", "--approve"}
		if runtime != "headless" {
			if extensionPath == "" {
				return Invocation{}, fmt.Errorf("interactive Pi terminal invocation requires a bridge extension")
			}
			args = []string{"--approve", "--extension", extensionPath}
		}
		if attempt.Model != "" {
			args = append(args, "--model", attempt.Model)
		}
		if resume {
			args = append(args, "--session", attempt.SessionID)
		} else {
			args = append(args, "--session-id", attempt.SessionID)
			if displayName != "" {
				args = append(args, "--name", displayName)
			}
		}
		args = append(args, prompt)
		return Invocation{Command: "pi", Args: args, Dir: attempt.WorktreePath, Environment: map[string]string{"SHEPHRD_PI_WATCHER_ENABLED": "0"}}, nil
	case "codex":
		if resume {
			args := []string{"exec", "resume", attempt.SessionID, prompt}
			if attempt.Model != "" {
				args = append(args, "--model", attempt.Model)
			}
			args = append(args, "--json", "--dangerously-bypass-approvals-and-sandbox")
			return Invocation{Command: "codex", Args: args, Dir: attempt.WorktreePath}, nil
		}
		args := []string{"exec"}
		if attempt.Model != "" {
			args = append(args, "--model", attempt.Model)
		}
		args = append(args, "--json", "--dangerously-bypass-approvals-and-sandbox", "-C", attempt.WorktreePath, prompt)
		return Invocation{Command: "codex", Args: args, Dir: attempt.WorktreePath}, nil
	}
	return Invocation{}, fmt.Errorf("unsupported harness %q", harness)
}

func ParseLine(harness string, line []byte) Parsed {
	var value map[string]any
	if json.Unmarshal(line, &value) != nil {
		return Parsed{}
	}
	typeName, _ := value["type"].(string)
	switch harness {
	case "claude-code":
		parsed := Parsed{SessionID: stringField(value, "session_id")}
		if typeName == "system" && stringField(value, "subtype") == "init" {
			parsed.Acknowledged = true
		}
		if typeName == "assistant" {
			parsed.EventAuthority = claudeEventAuthority(value)
			parsed.Text = messageText(value["message"])
		}
		if typeName == "result" {
			parsed.Terminal = true
			if booleanField(value, "is_error") || stringField(value, "subtype") == "error" {
				parsed.Failure = firstNonempty(stringField(value, "result"), stringField(value, "terminal_reason"), "Claude Code failed")
			}
		}
		return parsed
	case "pi":
		parsed := Parsed{}
		if typeName == "session" {
			parsed.Acknowledged = true
			parsed.SessionID = stringField(value, "id")
		}
		if typeName == "message_end" {
			if message, ok := value["message"].(map[string]any); ok && stringField(message, "role") == "assistant" {
				switch stringField(message, "stopReason") {
				case "stop", "length", "toolUse", "deferred":
					parsed.EventAuthority = EventAuthorityAssignedWorker
					parsed.Text = messageText(message)
				case "error":
					parsed.Failure = firstNonempty(stringField(message, "errorMessage"), "Pi failed")
				case "aborted":
					parsed.Failure = firstNonempty(stringField(message, "errorMessage"), "Pi aborted")
				default:
					parsed.Failure = "Pi assistant message ended without a completion reason"
				}
			}
		}
		if typeName == "auto_retry_end" && !booleanField(value, "success") {
			parsed.Failure = firstNonempty(stringField(value, "finalError"), "Pi retry failed")
		}
		if typeName == "agent_settled" {
			parsed.Terminal = true
		}
		return parsed
	case "codex":
		parsed := Parsed{}
		if typeName == "thread.started" {
			parsed.Acknowledged = true
			parsed.SessionID = stringField(value, "thread_id")
		}
		if typeName == "item.completed" {
			if item, ok := value["item"].(map[string]any); ok && stringField(item, "type") == "agent_message" {
				parsed.EventAuthority = EventAuthorityAssignedWorker
				parsed.Text = stringField(item, "text")
			}
		}
		if typeName == "turn.completed" {
			parsed.Terminal = true
		}
		if typeName == "turn.failed" || typeName == "error" {
			parsed.Terminal = true
			parsed.Failure = firstNonempty(stringField(value, "message"), nestedString(value, "error", "message"), "Codex failed")
		}
		return parsed
	}
	return Parsed{}
}

type Diagnostic struct {
	Code               string `json:"code"`
	Phase              string `json:"phase"`
	Field              string `json:"field,omitempty"`
	Offset             int64  `json:"offset,omitempty"`
	Message            string `json:"message"`
	RequiresCheckpoint bool   `json:"requires_checkpoint"`
	detail             string
}

const (
	DiagnosticJSONSyntax           = "EVENT_JSON_SYNTAX"
	DiagnosticJSONTrailing         = "EVENT_JSON_TRAILING"
	DiagnosticSchemaUnknownField   = "EVENT_SCHEMA_UNKNOWN_FIELD"
	DiagnosticSchemaTypeMismatch   = "EVENT_SCHEMA_TYPE_MISMATCH"
	DiagnosticSemanticInvalid      = "EVENT_SEMANTIC_INVALID"
	DiagnosticCheckpointRequired   = "CHECKPOINT_REQUIRED"
	DiagnosticCheckpointNotCurrent = "CHECKPOINT_NOT_CURRENT"
	DiagnosticCheckpointNextSteps  = "CHECKPOINT_NEXT_STEPS_REQUIRED"
	DiagnosticArtifactContract     = "DONE_ARTIFACT_CONTRACT_MISMATCH"
	DiagnosticBranchArtifact       = "DONE_BRANCH_ARTIFACT_MISMATCH"
	DiagnosticFramingMissingClose  = "EVENT_FRAMING_MISSING_CLOSE"
	DiagnosticFramingInvalid       = "EVENT_FRAMING_INVALID"
	DiagnosticSequenceInvalid      = "EVENT_SEQUENCE_INVALID"
)

func (d Diagnostic) Error() string {
	if d.detail != "" {
		return d.detail
	}
	return d.Message
}

func (d Diagnostic) Bounded() Diagnostic {
	d.Code = boundDiagnosticText(d.Code, 64)
	d.Phase = boundDiagnosticText(d.Phase, 32)
	d.Field = boundDiagnosticText(d.Field, 128)
	d.Message = boundDiagnosticText(d.Message, 512)
	d.detail = boundDiagnosticText(d.detail, 512)
	return d
}

func (d Diagnostic) Repairable() bool {
	switch d.Code {
	case DiagnosticJSONSyntax, DiagnosticJSONTrailing, DiagnosticSchemaUnknownField,
		DiagnosticSchemaTypeMismatch, DiagnosticSemanticInvalid, DiagnosticCheckpointRequired,
		DiagnosticCheckpointNotCurrent, DiagnosticCheckpointNextSteps, DiagnosticArtifactContract,
		DiagnosticBranchArtifact, DiagnosticFramingMissingClose:
		return true
	default:
		return false
	}
}

const (
	eventOpenTag          = "<shephrd-event>"
	eventCloseTag         = "</shephrd-event>"
	maxEventEnvelopeBytes = 96 * 1024
)

func ParseEvents(text string) ([]model.Event, bool, error) {
	events, found, diagnostic := parseEventCandidates(text)
	if diagnostic != nil {
		message := diagnostic.detail
		if message == "" {
			message = diagnostic.Message
		}
		return nil, found, errors.New(message)
	}
	return events, found, nil
}

func ParseEvent(text string) (model.Event, bool, error) {
	events, found, err := ParseEvents(text)
	if err != nil || !found {
		return model.Event{}, found, err
	}
	if len(events) != 1 {
		return model.Event{}, true, fmt.Errorf("worker assistant text contains %d event envelopes; use ParseEvents", len(events))
	}
	return events[0], true, nil
}

func ParseEventCandidate(text string) (model.Event, bool, *Diagnostic) {
	return singleCandidate(ParseEventCandidateSet(text))
}

func ParseSubdriverCandidate(text string) (model.Event, bool, *Diagnostic) {
	return singleCandidate(ParseSubdriverCandidateSet(text))
}

func singleCandidate(events []model.Event, found bool, diagnostic *Diagnostic) (model.Event, bool, *Diagnostic) {
	if diagnostic != nil || !found {
		return model.Event{}, found, diagnostic
	}
	if len(events) != 1 {
		return model.Event{}, true, framingDiagnostic(DiagnosticFramingInvalid, fmt.Sprintf("worker assistant text contains %d event envelopes", len(events)))
	}
	return events[0], true, nil
}

func ParseEventCandidateSet(text string) ([]model.Event, bool, *Diagnostic) {
	return parseCandidateSet(text, model.ValidateEvent)
}

func ParseSubdriverCandidateSet(text string) ([]model.Event, bool, *Diagnostic) {
	return parseCandidateSet(text, model.ValidateSubdriverEvent)
}

func parseCandidateSet(text string, validate func(model.Event) error) ([]model.Event, bool, *Diagnostic) {
	events, found, diagnostic := parseEventCandidatesValidated(text, validate)
	if diagnostic != nil || !found {
		return events, found, diagnostic
	}
	terminal := false
	for index, event := range events {
		wake := event.Type == "question" || event.Type == "done" || event.Type == "blocked" || event.Type == "failed"
		if terminal || wake && index != len(events)-1 {
			return nil, true, &Diagnostic{Code: DiagnosticSequenceInvalid, Phase: "adapter", Message: "event candidate ordering is invalid", detail: "worker event candidate contains duplicate or post-terminal envelopes"}
		}
		terminal = wake
	}
	return events, true, nil
}

func parseEventCandidates(text string) ([]model.Event, bool, *Diagnostic) {
	return parseEventCandidatesValidated(text, model.ValidateEvent)
}

func parseEventCandidatesValidated(text string, validate func(model.Event) error) ([]model.Event, bool, *Diagnostic) {
	bodies, found, diagnostic := extractEventBodies(text)
	if diagnostic != nil || !found {
		return nil, found, diagnostic
	}
	events := make([]model.Event, 0, len(bodies))
	for _, body := range bodies {
		event, diagnostic := parseEventBody(body, validate)
		if diagnostic != nil {
			return nil, true, diagnostic
		}
		events = append(events, event)
	}
	return events, true, nil
}

func extractEventBodies(text string) ([]string, bool, *Diagnostic) {
	var bodies []string
	offset := 0
	for {
		start, bodyStart := candidateOpen(text, offset)
		if start < 0 {
			return bodies, len(bodies) > 0, nil
		}
		closeOffset := strings.Index(text[bodyStart:], eventCloseTag)
		if closeOffset < 0 {
			if len(text)-start >= maxEventEnvelopeBytes {
				return nil, true, framingDiagnostic(DiagnosticFramingInvalid, fmt.Sprintf("worker event exceeds %d bytes", maxEventEnvelopeBytes))
			}
			return nil, true, framingDiagnostic(DiagnosticFramingMissingClose, "worker event is missing "+eventCloseTag)
		}
		bodyEnd := bodyStart + closeOffset
		if nested, _ := candidateOpen(text, bodyStart); nested >= 0 && nested < bodyEnd {
			return nil, true, framingDiagnostic(DiagnosticFramingInvalid, "worker event contains a nested "+eventOpenTag)
		}
		end := bodyEnd + len(eventCloseTag)
		if end-start >= maxEventEnvelopeBytes {
			return nil, true, framingDiagnostic(DiagnosticFramingInvalid, fmt.Sprintf("worker event exceeds %d bytes", maxEventEnvelopeBytes))
		}
		bodies = append(bodies, strings.TrimSpace(text[start+len(eventOpenTag):bodyEnd]))
		offset = end
	}
}

func candidateOpen(text string, offset int) (int, int) {
	for offset < len(text) {
		relative := strings.Index(text[offset:], eventOpenTag)
		if relative < 0 {
			return -1, -1
		}
		start := offset + relative
		body := start + len(eventOpenTag)
		for body < len(text) && (text[body] == ' ' || text[body] == '\t' || text[body] == '\r' || text[body] == '\n') {
			body++
		}
		if body < len(text) && text[body] == '{' {
			return start, body
		}
		offset = start + len(eventOpenTag)
	}
	return -1, -1
}

func parseEventBody(body string, validate func(model.Event) error) (model.Event, *Diagnostic) {
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()
	var event model.Event
	if err := decoder.Decode(&event); err != nil {
		diagnostic := classifyJSONDiagnostic(body, err)
		diagnostic.RequiresCheckpoint = diagnostic.Code == DiagnosticJSONSyntax || candidateIsCheckpoint(body)
		return model.Event{}, &diagnostic
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return model.Event{}, &Diagnostic{Code: DiagnosticJSONTrailing, Phase: "adapter", Message: "event contains trailing JSON", RequiresCheckpoint: event.Type == "checkpoint", detail: "worker event contains trailing JSON"}
	}
	event.Payload = strings.TrimSpace(event.Payload)
	if err := validate(event); err != nil {
		return model.Event{}, &Diagnostic{Code: DiagnosticSemanticInvalid, Phase: "adapter", Message: boundDiagnosticText(err.Error(), 512), RequiresCheckpoint: event.Type == "checkpoint", detail: "invalid worker event: " + err.Error()}
	}
	if event.Checkpoint != nil {
		normalized := model.NormalizeCheckpoint(*event.Checkpoint)
		event.Checkpoint = &normalized
	}
	return event, nil
}

func framingDiagnostic(code, detail string) *Diagnostic {
	return &Diagnostic{Code: code, Phase: "framing", Message: "event framing is invalid", detail: detail}
}

func classifyJSONDiagnostic(body string, err error) Diagnostic {
	detail := "invalid worker event JSON: " + err.Error()
	if field, found := strings.CutPrefix(err.Error(), "json: unknown field \""); found {
		field = strings.TrimSuffix(field, "\"")
		return Diagnostic{Code: DiagnosticSchemaUnknownField, Phase: "adapter", Field: boundDiagnosticField(field), Message: "event contains an unknown field", detail: detail}
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		field := mismatch.Field
		message := "event field has the wrong JSON type"
		var shape any
		switch mismatch.Type {
		case reflect.TypeFor[model.Decision]():
			shape = model.Decision{Decision: "choice made", Reason: "why"}
		case reflect.TypeFor[model.Check]():
			shape = model.Check{Command: "test command", Result: "observed result"}
		}
		if shape != nil {
			field += "[]"
			encoded, _ := json.Marshal(shape)
			message = field + " must contain objects with string fields, expected " + string(encoded) + "; not strings"
			detail = message
		}
		return Diagnostic{Code: DiagnosticSchemaTypeMismatch, Phase: "adapter", Field: boundDiagnosticField(field), Offset: mismatch.Offset, Message: message, detail: detail}
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return Diagnostic{Code: DiagnosticJSONSyntax, Phase: "adapter", Offset: syntax.Offset, Message: "event JSON syntax is invalid", detail: detail}
	}
	if !json.Valid([]byte(body)) {
		return Diagnostic{Code: DiagnosticJSONSyntax, Phase: "adapter", Message: "event JSON syntax is invalid", detail: detail}
	}
	return Diagnostic{Code: DiagnosticSchemaTypeMismatch, Phase: "adapter", Message: "event JSON does not match the schema", detail: detail}
}

func candidateIsCheckpoint(body string) bool {
	var candidate struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(body), &candidate) == nil && candidate.Type == "checkpoint"
}

func boundDiagnosticField(field string) string {
	return boundDiagnosticText(strings.TrimSpace(field), 128)
}

func boundDiagnosticText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := 0
	for index := range text {
		if index > limit {
			break
		}
		cut = index
	}
	return text[:cut]
}

func claudeEventAuthority(value map[string]any) EventAuthority {
	for _, key := range []string{"subagent_type", "agent_type", "agent_id", "is_sidechain", "isSidechain"} {
		if marker, ok := value[key]; ok && marker != nil && marker != "" && marker != false {
			return EventAuthorityNestedWorker
		}
	}
	parent, present := value["parent_tool_use_id"]
	if !present {
		return EventAuthorityNone
	}
	if text, ok := parent.(string); ok && text != "" {
		return EventAuthorityNestedWorker
	}
	if parent != nil {
		return EventAuthorityNone
	}
	return EventAuthorityAssignedWorker
}

func messageText(raw any) string {
	message, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	content, ok := message["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if ok && stringField(part, "type") == "text" {
			parts = append(parts, stringField(part, "text"))
		}
	}
	return strings.Join(parts, "\n")
}

func stringField(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return text
}

func booleanField(value map[string]any, key string) bool {
	flag, _ := value[key].(bool)
	return flag
}

func nestedString(value map[string]any, parent, key string) string {
	nested, _ := value[parent].(map[string]any)
	return stringField(nested, key)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func require(command string) error {
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("worker harness %q is not installed or not on PATH", command)
	}
	return nil
}
