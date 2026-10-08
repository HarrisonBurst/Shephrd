package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"shephrd/internal/config"
	"shephrd/internal/delivery"
	"shephrd/internal/extension"
	"shephrd/internal/model"
	"shephrd/internal/store"
)

const (
	ReportAcceptedEvent             = model.ReportAcceptedEventName
	ReportAcceptedEventVersion      = model.ReportAcceptedEventVersion
	ReportAcceptedCapability        = "lifecycle.report.accepted"
	ReportAcceptedCapabilityVersion = 1
	ReportAcceptedOperation         = "handle"
)

var reportAcceptedTimeout = 30 * time.Second

func ReportAcceptedCapabilityManifest() extension.Capability {
	return extension.Capability{Name: ReportAcceptedCapability, Version: ReportAcceptedCapabilityVersion, Operations: []string{ReportAcceptedOperation}}
}

type EventIdentity struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Version    int       `json:"version"`
	AcceptedAt time.Time `json:"accepted_at"`
}

type InvocationIdentity struct {
	ID      string `json:"id"`
	Handler string `json:"handler"`
}

type ReportIdentity struct {
	ArtifactID   string `json:"artifact_id"`
	Kind         string `json:"kind"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
	SnapshotPath string `json:"snapshot_path"`
}

type TaskMetadata struct {
	ID                   string `json:"id"`
	AttemptID            string `json:"attempt_id"`
	RunGeneration        int    `json:"run_generation"`
	RepoID               string `json:"repo_id"`
	RepoName             string `json:"repo_name"`
	Title                string `json:"title"`
	FeatureKey           string `json:"feature_key"`
	Deliverable          string `json:"deliverable"`
	CompletionProvenance string `json:"completion_provenance"`
}

type ReportAcceptedRequest struct {
	Event      EventIdentity      `json:"event"`
	Invocation InvocationIdentity `json:"invocation"`
	Report     ReportIdentity     `json:"report"`
	Task       TaskMetadata       `json:"task"`
}

type Receipt struct {
	System string `json:"system"`
	ID     string `json:"id"`
}

type ReportAcceptedResult struct {
	Annotation string   `json:"annotation,omitempty"`
	Receipt    *Receipt `json:"receipt,omitempty"`
}

type configuredHandler struct {
	config config.ReportAcceptedHandler
	hash   string
}

type Dispatcher struct {
	Store       *store.Store
	Verifier    delivery.Verifier
	handlers    []configuredHandler
	environment []string
	Now         func() time.Time
	Timeout     time.Duration
}

func NewDispatcher(state *store.Store, verifier delivery.Verifier, handlers []config.ReportAcceptedHandler, environment []string) *Dispatcher {
	configured := make([]configuredHandler, len(handlers))
	for index, handler := range handlers {
		configured[index] = configuredHandler{config: handler, hash: handlerConfigurationHash(handler)}
	}
	return &Dispatcher{Store: state, Verifier: verifier, handlers: configured, environment: append([]string(nil), environment...)}
}

func (d *Dispatcher) Bindings() []model.ReportAcceptedHandlerBinding {
	if d == nil {
		return nil
	}
	bindings := make([]model.ReportAcceptedHandlerBinding, len(d.handlers))
	for index, handler := range d.handlers {
		bindings[index] = model.ReportAcceptedHandlerBinding{Name: handler.config.Name, ExtensionID: handler.config.ExtensionID, ConfigurationHash: handler.hash}
	}
	return bindings
}

func (d *Dispatcher) Dispatch(artifact model.VerifiedArtifact) ([]model.ReportLifecycleInvocation, error) {
	if d == nil || artifact.AcceptedEventID == "" {
		return []model.ReportLifecycleInvocation{}, nil
	}
	invocations, err := d.Store.ReportLifecycleInvocationsForEvent(artifact.AcceptedEventID)
	if err != nil {
		return nil, err
	}
	configured := make(map[string]configuredHandler, len(d.handlers))
	for _, handler := range d.handlers {
		configured[handler.config.Name] = handler
	}
	for _, invocation := range invocations {
		if invocation.State == "succeeded" || invocation.State == "invoking" && invocation.DeadlineAt != nil && d.now().Before(*invocation.DeadlineAt) {
			continue
		}
		handler, available := configured[invocation.HandlerName]
		if !available || handler.config.ExtensionID != invocation.ExtensionID || handler.hash != invocation.ConfigurationHash {
			kind := "handler_not_configured"
			message := fmt.Sprintf("trusted handler %s is no longer configured for accepted event %s", invocation.HandlerName, invocation.EventID)
			if available {
				kind = "handler_configuration_changed"
				message = fmt.Sprintf("trusted handler %s configuration no longer matches accepted event %s", invocation.HandlerName, invocation.EventID)
			}
			if err := d.recordFailure(invocation, invocation.ConfigurationHash, kind, message, "none"); err != nil {
				return nil, err
			}
			continue
		}
		if err := d.invoke(handler, invocation, artifact); err != nil {
			return nil, err
		}
	}
	return d.Store.ReportLifecycleInvocationsForEvent(artifact.AcceptedEventID)
}

func (d *Dispatcher) invoke(handler configuredHandler, invocation model.ReportLifecycleInvocation, artifact model.VerifiedArtifact) error {
	at := d.now()
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = reportAcceptedTimeout
	}
	claimed, acquired, err := d.Store.ClaimReportLifecycleInvocation(invocation.ID, handler.hash, at, at.Add(timeout))
	if err != nil || !acquired {
		return err
	}
	if err := d.Verifier.ValidateSnapshot(artifact); err != nil {
		_, completeErr := d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "failed", "", "", "", "report_snapshot_invalid", boundedFailure(err.Error()), "none", d.now())
		return completeErr
	}
	task, err := d.Store.Task(invocation.TaskID)
	if err != nil {
		_, completeErr := d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "failed", "", "", "", "task_metadata_unavailable", boundedFailure(err.Error()), "none", d.now())
		return completeErr
	}
	attempt, err := d.Store.Attempt(invocation.AttemptID)
	if err != nil {
		_, completeErr := d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "failed", "", "", "", "attempt_metadata_unavailable", boundedFailure(err.Error()), "none", d.now())
		return completeErr
	}
	request, err := reportAcceptedRequest(task, attempt, artifact, claimed)
	if err != nil {
		_, completeErr := d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "failed", "", "", "", "event_payload_invalid", boundedFailure(err.Error()), "none", d.now())
		return completeErr
	}
	host := extension.NewHost(extension.HostConfig{
		Command: handler.config.Command, SHA256: handler.config.SHA256, ParentEnvironment: d.environment,
		AllowedEnvironment: handler.config.Environment, ExpectedID: handler.config.ExtensionID,
		ExpectedCapabilities: []extension.Capability{ReportAcceptedCapabilityManifest()},
	})
	ctx, cancel := context.WithDeadline(context.Background(), at.Add(timeout))
	defer cancel()
	var response ReportAcceptedResult
	invokeErr := host.Invoke(ctx, ReportAcceptedCapability, ReportAcceptedCapabilityVersion, ReportAcceptedOperation, request, &response)
	if invokeErr != nil {
		state, kind, message, effect := classifyInvocationFailure(invokeErr)
		_, err = d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, state, "", "", "", kind, message, effect, d.now())
		return err
	}
	if err := ValidateReportAcceptedResult(response); err != nil {
		_, completeErr := d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "unknown", "", "", "", "invalid_handler_result", boundedFailure(err.Error()), "unknown", d.now())
		return completeErr
	}
	if err := d.Verifier.ValidateSnapshot(artifact); err != nil {
		_, completeErr := d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "unknown", "", "", "", "report_snapshot_changed", boundedFailure(err.Error()), "unknown", d.now())
		return completeErr
	}
	receiptSystem, receiptID := "", ""
	if response.Receipt != nil {
		receiptSystem, receiptID = response.Receipt.System, response.Receipt.ID
	}
	_, err = d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "succeeded", response.Annotation, receiptSystem, receiptID, "", "", "", d.now())
	return err
}

func (d *Dispatcher) recordFailure(invocation model.ReportLifecycleInvocation, configurationHash, kind, message, effect string) error {
	at := d.now()
	claimed, acquired, err := d.Store.ClaimReportLifecycleInvocation(invocation.ID, configurationHash, at, at.Add(time.Second))
	if err != nil || !acquired {
		return err
	}
	_, err = d.Store.CompleteReportLifecycleInvocation(claimed.ID, claimed.InvocationClaimToken, "failed", "", "", "", kind, boundedFailure(message), effect, d.now())
	return err
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func reportAcceptedRequest(task model.Task, attempt model.Attempt, artifact model.VerifiedArtifact, invocation model.ReportLifecycleInvocation) (ReportAcceptedRequest, error) {
	request := ReportAcceptedRequest{
		Event:      EventIdentity{ID: artifact.AcceptedEventID, Name: artifact.AcceptedEventName, Version: artifact.AcceptedEventVersion, AcceptedAt: artifact.VerifiedAt},
		Invocation: InvocationIdentity{ID: invocation.ID, Handler: invocation.HandlerName},
		Report:     ReportIdentity{ArtifactID: artifact.ID, Kind: artifact.Kind, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes, SnapshotPath: artifact.SnapshotPath},
		Task: TaskMetadata{ID: task.ID, AttemptID: attempt.ID, RunGeneration: attempt.RunGeneration, RepoID: task.RepoID, RepoName: task.RepoName,
			Title: task.Title, FeatureKey: task.FeatureKey, Deliverable: task.Deliverable, CompletionProvenance: task.CompletionProvenance},
	}
	if request.Event.ID == "" || request.Event.Name != ReportAcceptedEvent || request.Event.Version != ReportAcceptedEventVersion || request.Event.AcceptedAt.IsZero() || request.Invocation.ID == "" || request.Invocation.Handler == "" || request.Report.ArtifactID == "" || request.Report.Kind != "report" || len(request.Report.SHA256) != 64 || request.Report.SizeBytes < 0 || invalidProtocolText(request.Report.SnapshotPath, 4096, false) || request.Task.ID == "" || request.Task.AttemptID == "" || request.Task.RunGeneration < 1 || invalidProtocolText(request.Task.RepoID, 256, false) || invalidProtocolText(request.Task.RepoName, 1024, false) || invalidProtocolText(request.Task.Title, 1024, false) || invalidProtocolText(request.Task.FeatureKey, 512, false) || request.Task.Deliverable != "report" {
		return ReportAcceptedRequest{}, fmt.Errorf("report.accepted event metadata is invalid or oversized")
	}
	return request, nil
}

func ValidateReportAcceptedResult(result ReportAcceptedResult) error {
	if invalidProtocolText(result.Annotation, 4096, true) {
		return fmt.Errorf("handler annotation is invalid or oversized")
	}
	if result.Receipt != nil && (invalidProtocolText(result.Receipt.System, 128, false) || invalidProtocolText(result.Receipt.ID, 512, false)) {
		return fmt.Errorf("handler receipt is invalid or oversized")
	}
	if result.Annotation == "" && result.Receipt == nil {
		return fmt.Errorf("handler result requires an annotation or receipt")
	}
	return nil
}

func classifyInvocationFailure(err error) (string, string, string, string) {
	var remote *extension.RemoteError
	if errors.As(err, &remote) {
		state := "failed"
		if remote.Failure.Effect == "unknown" {
			state = "unknown"
		}
		return state, remote.Failure.Code, boundedFailure(remote.Error()), remote.Failure.Effect
	}
	var host *extension.HostError
	if errors.As(err, &host) {
		return "unknown", host.ErrorKind(), boundedFailure(host.Error()), "unknown"
	}
	return "unknown", "handler_invocation_failed", boundedFailure(err.Error()), "unknown"
}

func handlerConfigurationHash(handler config.ReportAcceptedHandler) string {
	encoded, _ := json.Marshal(struct {
		Name        string   `json:"name"`
		ExtensionID string   `json:"extension_id"`
		Command     []string `json:"command"`
		SHA256      string   `json:"sha256"`
		Environment []string `json:"environment"`
	}{handler.Name, handler.ExtensionID, handler.Command, handler.SHA256, handler.Environment})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func invalidProtocolText(value string, limit int, optional bool) bool {
	if value == "" {
		return !optional
	}
	return len(value) > limit || !utf8.ValidString(value) || strings.IndexFunc(value, func(char rune) bool {
		return char < 0x20 || char == 0x7f
	}) >= 0
}

func boundedFailure(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 2048 {
		value = value[:2048]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	if value == "" {
		return "report lifecycle handler failed"
	}
	return value
}
