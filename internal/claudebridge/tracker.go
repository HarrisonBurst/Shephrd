package claudebridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	openTag  = "<shephrd-event>"
	closeTag = "</shephrd-event>"
)

type BackgroundTask struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	Server      string `json:"server,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Name        string `json:"name,omitempty"`
}

type HookInput struct {
	SessionID            string
	HookEventName        string
	AgentID              string
	Source               string
	MessageID            string
	Index                int
	Final                bool
	Delta                string
	Error                string
	Reason               string
	LastAssistantMessage string
	BackgroundTasks      []BackgroundTask
}

type Outcome struct {
	Acknowledged     bool
	Envelopes        []string
	Settled          bool
	Reconciled       bool
	BackgroundActive bool
	BackgroundTasks  []BackgroundTask
	Failure          string
}

type Tracker struct {
	sessionID   string
	startSource string
	sessionSeen bool
	messages    map[string]*messageState
}

type messageState struct {
	nextIndex int
	extractor envelopeExtractor
	emitted   []string
}

type envelopeExtractor struct {
	buffer string
}

func NewTracker(sessionID string, resume bool) *Tracker {
	source := "startup"
	if resume {
		source = "resume"
	}
	return &Tracker{sessionID: sessionID, startSource: source, messages: make(map[string]*messageState)}
}

func ParseHookInput(raw json.RawMessage) (HookInput, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return HookInput{}, fmt.Errorf("invalid Claude hook input: %w", err)
	}
	input := HookInput{}
	var err error
	if input.SessionID, err = requiredString(fields, "session_id"); err != nil {
		return HookInput{}, err
	}
	if input.HookEventName, err = requiredString(fields, "hook_event_name"); err != nil {
		return HookInput{}, err
	}
	input.AgentID, _ = optionalString(fields, "agent_id")
	switch input.HookEventName {
	case "SessionStart":
		input.Source, err = requiredString(fields, "source")
	case "MessageDisplay":
		if input.MessageID, err = requiredString(fields, "message_id"); err != nil {
			break
		}
		if input.Index, err = requiredInt(fields, "index"); err != nil {
			break
		}
		if input.Index < 0 {
			err = fmt.Errorf("Claude hook field %q is invalid", "index")
			break
		}
		if input.Final, err = requiredBool(fields, "final"); err != nil {
			break
		}
		input.Delta, err = requiredStringValue(fields, "delta")
	case "Stop":
		input.LastAssistantMessage, err = requiredStringValue(fields, "last_assistant_message")
		if err == nil {
			input.BackgroundTasks, err = optionalBackgroundTasks(fields)
		}
	case "StopFailure":
		input.Error, err = requiredString(fields, "error")
	case "SessionEnd":
		input.Reason, err = requiredString(fields, "reason")
	default:
		err = fmt.Errorf("Claude hook event %q is unsupported", input.HookEventName)
	}
	if err != nil {
		return HookInput{}, err
	}
	return input, nil
}

func (t *Tracker) Handle(input HookInput) Outcome {
	if input.AgentID != "" {
		return Outcome{}
	}
	if input.SessionID != t.sessionID {
		return failed("Claude bridge reported an unexpected native session identity")
	}
	switch input.HookEventName {
	case "SessionStart":
		if !t.sessionSeen {
			if input.Source != t.startSource {
				return failed("Claude bridge reported an unexpected session start source")
			}
			t.sessionSeen = true
			return Outcome{Acknowledged: true}
		}
		if input.Source == "compact" {
			return Outcome{}
		}
		return failed("Claude bridge reported an unexpected repeated session start")
	case "MessageDisplay":
		if !t.sessionSeen {
			return failed("Claude bridge displayed output before session acknowledgment")
		}
		message := t.messages[input.MessageID]
		if message == nil {
			if input.Index != 0 {
				return failed("Claude bridge message sequence is invalid")
			}
			message = &messageState{}
			t.messages[input.MessageID] = message
		}
		if input.Index != message.nextIndex {
			return failed("Claude bridge message sequence is invalid")
		}
		message.nextIndex++
		envelopes, err := message.extractor.Feed(input.Delta, input.Final)
		if err != nil {
			return failed(err.Error())
		}
		message.emitted = append(message.emitted, envelopes...)
		if input.Final {
			delete(t.messages, input.MessageID)
		}
		return Outcome{Envelopes: envelopes}
	case "Stop":
		if !t.sessionSeen {
			return failed("Claude bridge stopped with incomplete structured output")
		}
		outcome := t.reconcileStop(input.LastAssistantMessage)
		outcome.BackgroundTasks = append([]BackgroundTask(nil), input.BackgroundTasks...)
		if len(input.BackgroundTasks) > 0 {
			outcome.Settled = false
			outcome.BackgroundActive = true
		}
		return outcome
	case "StopFailure":
		return failed("Claude Code stopped with " + input.Error)
	case "SessionEnd":
		return failed("Claude Code session ended before a terminal envelope: " + input.Reason)
	default:
		return failed("Claude bridge event is unsupported")
	}
}

func (t *Tracker) reconcileStop(completed string) Outcome {
	if len(t.messages) == 0 {
		return Outcome{Settled: true}
	}
	if len(t.messages) != 1 {
		return failed("Claude bridge Stop reconciliation conflicts with streamed structured output")
	}
	var messageID string
	var message *messageState
	for messageID, message = range t.messages {
	}
	extractor := envelopeExtractor{}
	envelopes, err := extractor.Feed(completed, true)
	if err != nil {
		return failed(err.Error())
	}
	if len(envelopes) < len(message.emitted) {
		return failed("Claude bridge Stop reconciliation conflicts with streamed structured output")
	}
	for index := range message.emitted {
		if envelopes[index] != message.emitted[index] {
			return failed("Claude bridge Stop reconciliation conflicts with streamed structured output")
		}
	}
	delete(t.messages, messageID)
	return Outcome{Envelopes: envelopes[len(message.emitted):], Settled: true, Reconciled: true}
}

func (e *envelopeExtractor) Feed(delta string, final bool) ([]string, error) {
	e.buffer += delta
	var envelopes []string
	for {
		start := strings.Index(e.buffer, openTag)
		if start < 0 {
			if final {
				e.buffer = ""
				return envelopes, nil
			}
			if len(e.buffer) >= len(openTag) {
				e.buffer = e.buffer[len(e.buffer)-len(openTag)+1:]
			}
			return envelopes, nil
		}
		e.buffer = e.buffer[start:]
		end := strings.Index(e.buffer[len(openTag):], closeTag)
		if end < 0 {
			if len(e.buffer) >= MaxFrameBytes {
				return nil, fmt.Errorf("Claude bridge structured output is oversized")
			}
			if final {
				return nil, fmt.Errorf("Claude bridge structured output is malformed")
			}
			return envelopes, nil
		}
		end += len(openTag)
		frameEnd := end + len(closeTag)
		envelope := e.buffer[:frameEnd]
		if len(envelope) >= MaxFrameBytes {
			return nil, fmt.Errorf("Claude bridge structured output is oversized")
		}
		envelopes = append(envelopes, envelope)
		e.buffer = e.buffer[frameEnd:]
	}
}

func failed(reason string) Outcome {
	return Outcome{Failure: reason}
}

func optionalBackgroundTasks(fields map[string]json.RawMessage) ([]BackgroundTask, error) {
	raw, ok := fields["background_tasks"]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var tasks []BackgroundTask
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return nil, fmt.Errorf("Claude hook field %q is invalid", "background_tasks")
	}
	for _, task := range tasks {
		if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Type) == "" || strings.TrimSpace(task.Status) == "" {
			return nil, fmt.Errorf("Claude hook field %q is invalid", "background_tasks")
		}
	}
	return tasks, nil
}

func requiredString(fields map[string]json.RawMessage, name string) (string, error) {
	value, err := requiredStringValue(fields, name)
	if err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("Claude hook field %q is invalid", name)
	}
	return value, nil
}

func requiredStringValue(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("Claude hook field %q is missing", name)
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("Claude hook field %q is invalid", name)
	}
	return value, nil
}

func optionalString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("Claude hook field %q is invalid", name)
	}
	return value, nil
}

func requiredInt(fields map[string]json.RawMessage, name string) (int, error) {
	raw, ok := fields[name]
	if !ok {
		return 0, fmt.Errorf("Claude hook field %q is missing", name)
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return 0, fmt.Errorf("Claude hook field %q is invalid", name)
	}
	return value, nil
}

func requiredBool(fields map[string]json.RawMessage, name string) (bool, error) {
	raw, ok := fields[name]
	if !ok {
		return false, fmt.Errorf("Claude hook field %q is missing", name)
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, fmt.Errorf("Claude hook field %q is invalid", name)
	}
	return value, nil
}
