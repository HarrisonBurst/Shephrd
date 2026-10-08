package driverdelivery

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"shephrd/internal/config"
	extensionhost "shephrd/internal/extension"
	"shephrd/internal/model"
)

const (
	CapabilityName    = "driver.delivery"
	CapabilityVersion = 1
	DeliverOperation  = "deliver"
	MaxPayloadBytes   = 8 * 1024
	MaxDetailBytes    = 512
	MaxIdentityBytes  = 256
	MaxFieldBytes     = 4 * MaxIdentityBytes
	MaxClaimToken     = 512
	maxCommandArgs    = 16
	maxCommandArg     = 1024

	OutcomeDelivered = "delivered"
	OutcomeRetryable = "retryable"
	OutcomeRejected  = "rejected"
)

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type Request struct {
	Driver       Driver       `json:"driver"`
	Notification Notification `json:"notification"`
	Claim        Claim        `json:"claim"`
	Commands     Commands     `json:"commands"`
	Obligations  *Obligations `json:"obligations,omitempty"`
}

type Driver struct {
	ID         string `json:"id"`
	Generation string `json:"generation"`
}

type Notification struct {
	NotificationID    string    `json:"notification_id"`
	Kind              string    `json:"kind"`
	RequestID         string    `json:"request_id"`
	SubdriverID       string    `json:"subdriver_id"`
	SubdriverRepoName string    `json:"subdriver_repo_name"`
	SubdriverEventID  int64     `json:"subdriver_event_id"`
	TaskID            string    `json:"task_id"`
	TaskTitle         string    `json:"task_title"`
	AttemptID         string    `json:"attempt_id"`
	Artifact          string    `json:"artifact"`
	Payload           string    `json:"payload"`
	PayloadTruncated  bool      `json:"payload_truncated"`
	CreatedAt         time.Time `json:"created_at"`
}

type Claim struct {
	ClaimToken      string    `json:"claim_token"`
	ClaimUntil      time.Time `json:"claim_until"`
	DeliveryAttempt int       `json:"delivery_attempt"`
}

type Commands struct {
	Read    []string `json:"read"`
	Request []string `json:"request,omitempty"`
	Reply   []string `json:"reply"`
	Ack     []string `json:"ack"`
}

type Obligations struct {
	SchemaVersion int              `json:"schema_version"`
	Counts        ObligationCounts `json:"counts"`
}

type ObligationCounts struct {
	ActNow           int `json:"act_now"`
	NeedsDisposition int `json:"needs_disposition"`
	ResultReady      int `json:"result_ready"`
	PlannedReady     int `json:"planned_ready"`
}

type Result struct {
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

func Capability() extensionhost.Capability {
	return extensionhost.Capability{Name: CapabilityName, Version: CapabilityVersion, Operations: []string{DeliverOperation}}
}

func NewRequest(notification model.DriverNotification, generation string, attempt int, obligations *Obligations) Request {
	payload, truncated := truncateUTF8(strings.ToValidUTF8(notification.Payload, ""), MaxPayloadBytes)
	repoName, _ := truncateUTF8(strings.ToValidUTF8(notification.SubdriverRepoName, ""), MaxFieldBytes)
	artifact := notification.Artifact
	if len(artifact) > MaxFieldBytes || !utf8.ValidString(artifact) {
		artifact = ""
	}
	request := Request{
		Driver: Driver{ID: notification.ClaimOwner, Generation: generation},
		Notification: Notification{
			NotificationID: notification.NotificationID, Kind: notification.Kind, RequestID: notification.RequestID,
			SubdriverID: notification.SubdriverID, SubdriverRepoName: repoName, SubdriverEventID: notification.SubdriverEventID,
			TaskID: notification.TaskID, AttemptID: notification.AttemptID, Artifact: artifact,
			Payload: payload, PayloadTruncated: truncated, CreatedAt: notification.CreatedAt.UTC(),
		},
		Claim:       Claim{ClaimToken: notification.ClaimToken, ClaimUntil: notification.ClaimUntil.UTC(), DeliveryAttempt: attempt},
		Obligations: obligations,
	}
	ack := []string{"shephrd", "wake", "ack", "--claim-token", notification.ClaimToken, "--driver-id", notification.ClaimOwner, "--json"}
	if notification.SubdriverEventID != 0 {
		event := strconv.FormatInt(notification.SubdriverEventID, 10)
		request.Commands = Commands{
			Read:    []string{"shephrd", "subdriver", "event", event, "--json"},
			Request: []string{"shephrd", "subdriver", "request", notification.RequestID, "--json"},
			Reply:   []string{"shephrd", "subdriver", "reply", notification.RequestID, "<text>", "--reply-to", event, "--key", "<key>", "--driver-id", notification.ClaimOwner, "--json"},
			Ack:     ack,
		}
		return request
	}
	request.Notification.TaskTitle, _ = truncateUTF8(strings.ToValidUTF8(notification.TaskTitle, ""), MaxFieldBytes)
	request.Commands = Commands{
		Read:  []string{"shephrd", "task", "inspect", notification.TaskID, "--json"},
		Reply: []string{"shephrd", "worker", "send", notification.TaskID, "<text>", "--json"},
		Ack:   ack,
	}
	return request
}

func ValidateRequest(request Request) error {
	if invalidIdentity(request.Driver.ID) || invalidIdentity(request.Driver.Generation) || model.IsSubdriverOwner(request.Driver.ID) {
		return fmt.Errorf("delivery driver identity is invalid")
	}
	notice := request.Notification
	if invalidIdentity(notice.NotificationID) || !kindPattern.MatchString(notice.Kind) || strings.HasPrefix(notice.Kind, "coordinator-") || notice.CreatedAt.IsZero() {
		return fmt.Errorf("delivery notification identity is invalid")
	}
	if len(notice.Payload) > MaxPayloadBytes || !utf8.ValidString(notice.Payload) {
		return fmt.Errorf("delivery notification payload is invalid or oversized")
	}
	for _, value := range []string{notice.RequestID, notice.SubdriverID, notice.SubdriverRepoName, notice.TaskID, notice.TaskTitle, notice.AttemptID, notice.Artifact} {
		if len(value) > MaxFieldBytes || !utf8.ValidString(value) {
			return fmt.Errorf("delivery notification field is invalid or oversized")
		}
	}
	subdriver := notice.RequestID != "" || notice.SubdriverID != "" || notice.SubdriverEventID != 0
	if subdriver {
		if notice.RequestID == "" || notice.SubdriverID == "" || notice.SubdriverEventID <= 0 || notice.TaskID != "" || !strings.HasPrefix(notice.Kind, "subdriver-") || len(request.Commands.Request) == 0 {
			return fmt.Errorf("delivery sub-driver return identity is incomplete or conflicting")
		}
	} else if notice.TaskID == "" || len(request.Commands.Request) != 0 {
		return fmt.Errorf("delivery worker notification identity is incomplete or conflicting")
	}
	if request.Claim.ClaimToken == "" || len(request.Claim.ClaimToken) > MaxClaimToken || request.Claim.ClaimUntil.IsZero() || request.Claim.DeliveryAttempt < 1 {
		return fmt.Errorf("delivery claim is invalid")
	}
	for _, command := range [][]string{request.Commands.Read, request.Commands.Reply, request.Commands.Ack} {
		if len(command) == 0 {
			return fmt.Errorf("delivery commands are incomplete")
		}
	}
	for _, command := range [][]string{request.Commands.Read, request.Commands.Request, request.Commands.Reply, request.Commands.Ack} {
		if len(command) == 0 {
			continue
		}
		if len(command) > maxCommandArgs || command[0] != "shephrd" {
			return fmt.Errorf("delivery command vector is invalid")
		}
		for _, argument := range command {
			if argument == "" || len(argument) > maxCommandArg || strings.IndexFunc(argument, isControl) >= 0 {
				return fmt.Errorf("delivery command argument is invalid")
			}
		}
	}
	if request.Obligations != nil {
		counts := request.Obligations.Counts
		if request.Obligations.SchemaVersion != model.AttentionSchemaVersion || counts.ActNow < 0 || counts.NeedsDisposition < 0 || counts.ResultReady < 0 || counts.PlannedReady < 0 {
			return fmt.Errorf("delivery obligations snapshot is invalid")
		}
	}
	return nil
}

func ValidateResult(result Result) error {
	switch result.Outcome {
	case OutcomeDelivered, OutcomeRetryable, OutcomeRejected:
	default:
		return fmt.Errorf("delivery outcome is invalid")
	}
	if len(result.Detail) > MaxDetailBytes || !utf8.ValidString(result.Detail) || strings.IndexFunc(result.Detail, isControl) >= 0 {
		return fmt.Errorf("delivery detail is invalid or oversized")
	}
	return nil
}

type ExtensionDeliverer struct {
	host *extensionhost.Host
}

func NewExtensionDeliverer(configured config.DeliveryExtensionConfig, environment []string) *ExtensionDeliverer {
	return &ExtensionDeliverer{host: extensionhost.NewHost(extensionhost.HostConfig{
		Command: configured.Command, SHA256: configured.SHA256, ParentEnvironment: environment,
		AllowedEnvironment: configured.Environment, ExpectedID: configured.ExtensionID,
		ExpectedCapabilities: []extensionhost.Capability{Capability()},
	})}
}

func (d *ExtensionDeliverer) Describe(ctx context.Context) error {
	_, err := d.host.Describe(ctx)
	return err
}

func (d *ExtensionDeliverer) Deliver(ctx context.Context, request Request) (Result, error) {
	if err := ValidateRequest(request); err != nil {
		return Result{}, err
	}
	var result Result
	if err := d.host.Invoke(ctx, CapabilityName, CapabilityVersion, DeliverOperation, request, &result); err != nil {
		return Result{}, err
	}
	if err := ValidateResult(result); err != nil {
		return Result{}, &extensionhost.HostError{Kind: extensionhost.ErrorProtocolViolation, Operation: DeliverOperation, Cause: err}
	}
	return result, nil
}

func invalidIdentity(value string) bool {
	return strings.TrimSpace(value) == "" || len(value) > MaxIdentityBytes || strings.IndexFunc(value, isControl) >= 0
}

func isControl(char rune) bool {
	return char < 0x20 || char == 0x7f
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}
