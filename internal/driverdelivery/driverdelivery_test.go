package driverdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"shephrd/internal/config"
	extensionhost "shephrd/internal/extension"
	"shephrd/internal/model"
)

func TestNewRequestEmitsExactCanonicalCommands(t *testing.T) {
	until := time.Now().Add(time.Minute)
	subdriver := NewRequest(model.DriverNotification{
		NotificationID: "wake:1", Kind: "subdriver-question", RequestID: "request_1", SubdriverID: "subdriver_1",
		SubdriverRepoName: "demo", SubdriverEventID: 42, TaskTitle: "Sub-driver return", Payload: "Which format?",
		CreatedAt: time.Now(), ClaimOwner: "driver:hermes", ClaimToken: "token", ClaimUntil: &until,
	}, "watch:1", 1, &Obligations{SchemaVersion: model.AttentionSchemaVersion})
	if err := ValidateRequest(subdriver); err != nil {
		t.Fatal(err)
	}
	if subdriver.Notification.TaskTitle != "" || subdriver.Driver != (Driver{ID: "driver:hermes", Generation: "watch:1"}) ||
		!slices.Equal(subdriver.Commands.Read, []string{"shephrd", "subdriver", "event", "42", "--json"}) ||
		!slices.Equal(subdriver.Commands.Request, []string{"shephrd", "subdriver", "request", "request_1", "--json"}) ||
		!slices.Equal(subdriver.Commands.Reply, []string{"shephrd", "subdriver", "reply", "request_1", "<text>", "--reply-to", "42", "--key", "<key>", "--driver-id", "driver:hermes", "--json"}) ||
		!slices.Equal(subdriver.Commands.Ack, []string{"shephrd", "wake", "ack", "--claim-token", "token", "--driver-id", "driver:hermes", "--json"}) {
		t.Fatalf("sub-driver request = %+v", subdriver)
	}
	worker := NewRequest(model.DriverNotification{
		NotificationID: "wake:2", Kind: "question", TaskID: "task_1", TaskTitle: "Fix it", AttemptID: "attempt_1",
		Payload: strings.Repeat("é", MaxPayloadBytes), CreatedAt: time.Now(), ClaimOwner: "driver:hermes", ClaimToken: "token", ClaimUntil: &until,
	}, "watch:1", 1, nil)
	if err := ValidateRequest(worker); err != nil {
		t.Fatal(err)
	}
	if !worker.Notification.PayloadTruncated || len(worker.Notification.Payload) > MaxPayloadBytes || worker.Commands.Request != nil ||
		!slices.Equal(worker.Commands.Read, []string{"shephrd", "task", "inspect", "task_1", "--json"}) ||
		!slices.Equal(worker.Commands.Reply, []string{"shephrd", "worker", "send", "task_1", "<text>", "--json"}) {
		t.Fatalf("worker request = %+v", worker.Commands)
	}
}

func TestNewRequestBoundsLongTitlesAndOmitsOversizedArtifacts(t *testing.T) {
	until := time.Now().Add(time.Minute)
	for name, title := range map[string]string{"ascii": strings.Repeat("t", 1100), "multibyte": strings.Repeat("界", 400), "invalid": strings.Repeat("ab\xff", 600)} {
		t.Run(name, func(t *testing.T) {
			request := NewRequest(model.DriverNotification{
				NotificationID: "wake:1", Kind: "done", TaskID: "task_1", TaskTitle: title, AttemptID: "attempt_1", Artifact: "https://github.com/o/r/pull/1?" + title,
				CreatedAt: time.Now(), ClaimOwner: "driver:hermes", ClaimToken: "token", ClaimUntil: &until,
			}, "watch:1", 1, nil)
			if err := ValidateRequest(request); err != nil {
				t.Fatal(err)
			}
			got := request.Notification.TaskTitle
			if len(got) > MaxFieldBytes || len(got) < MaxFieldBytes-utf8.UTFMax || !utf8.ValidString(got) || !strings.HasPrefix(strings.ToValidUTF8(title, ""), got) {
				t.Fatalf("title = %q (%d bytes)", got, len(got))
			}
			if request.Notification.Artifact != "" || !slices.Equal(request.Commands.Ack, []string{"shephrd", "wake", "ack", "--claim-token", "token", "--driver-id", "driver:hermes", "--json"}) {
				t.Fatalf("oversized artifact or identity changed: %+v", request)
			}
		})
	}
	artifact := "branch:shephrd/task_1"
	request := NewRequest(model.DriverNotification{NotificationID: "wake:1", Kind: "done", TaskID: "task_1", TaskTitle: "Fix it", Artifact: artifact, CreatedAt: time.Now(), ClaimOwner: "driver:hermes", ClaimToken: "token", ClaimUntil: &until}, "watch:1", 1, nil)
	if request.Notification.TaskTitle != "Fix it" || request.Notification.Artifact != artifact {
		t.Fatalf("bounded fields changed: %+v", request.Notification)
	}
}

func TestValidateRequestRejectsConflictingOrUnboundedFields(t *testing.T) {
	until := time.Now().Add(time.Minute)
	valid := func() Request {
		return NewRequest(model.DriverNotification{NotificationID: "wake:1", Kind: "subdriver-result", RequestID: "request_1", SubdriverID: "subdriver_1", SubdriverEventID: 1, CreatedAt: time.Now(), ClaimOwner: "driver:hermes", ClaimToken: "token", ClaimUntil: &until}, "watch:1", 1, nil)
	}
	for name, mutate := range map[string]func(*Request){
		"legacy kind":        func(r *Request) { r.Notification.Kind = "coordinator-result" },
		"sub-driver owner":   func(r *Request) { r.Driver.ID = "coordinator:subdriver_1" },
		"mixed identity":     func(r *Request) { r.Notification.TaskID = "task_1" },
		"missing event":      func(r *Request) { r.Notification.SubdriverEventID = 0 },
		"oversized payload":  func(r *Request) { r.Notification.Payload = strings.Repeat("x", MaxPayloadBytes+1) },
		"oversized title":    func(r *Request) { r.Notification.TaskTitle = strings.Repeat("x", MaxFieldBytes+1) },
		"oversized artifact": func(r *Request) { r.Notification.Artifact = strings.Repeat("x", MaxFieldBytes+1) },
		"missing ack":        func(r *Request) { r.Commands.Ack = nil },
		"foreign executable": func(r *Request) { r.Commands.Read[0] = "/bin/sh" },
		"control argument":   func(r *Request) { r.Commands.Reply[4] = "a\nb" },
		"missing claim":      func(r *Request) { r.Claim.ClaimToken = "" },
		"zero attempt":       func(r *Request) { r.Claim.DeliveryAttempt = 0 },
		"bad obligations":    func(r *Request) { r.Obligations = &Obligations{SchemaVersion: 1} },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid()
			mutate(&request)
			if ValidateRequest(request) == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	for _, result := range []Result{{Outcome: "acknowledged"}, {Outcome: OutcomeDelivered, Detail: strings.Repeat("x", MaxDetailBytes+1)}, {Outcome: OutcomeRejected, Detail: "a\nb"}} {
		if ValidateResult(result) == nil {
			t.Fatalf("invalid result accepted: %+v", result)
		}
	}
}

func TestExtensionDelivererEnforcesManifestPinAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.delivery","version":"1.0.0"},"capabilities":[{"name":"driver.delivery","version":1,"operations":["deliver"]}]}`
	respond := `id=$(printf '%s' "$line" | sed -n 's/^{"wire":{[^}]*},"request_id":"\([^"]*\)".*/\1/p')
printf '{"wire":{"major":1,"minor":0},"request_id":"%s","capability":"driver.delivery","capability_version":1,"operation":"deliver","status":"ok","result":{"outcome":"delivered","detail":"%s/%s"},"extension":{"id":"fixture.delivery","version":"1.0.0"}}\n' "$id" "${ALLOWED_VALUE:-unset}" "${HIDDEN_VALUE:-unset}"`
	script := "#!/bin/sh\ncase \"$1\" in\ndescribe) printf '%s\\n' '" + manifest + "' ;;\ninvoke) read -r line\n" + respond + " ;;\nesac\n"
	executable := writeScript(t, dir, "delivery", script)
	environment := []string{"ALLOWED_VALUE=visible", "HIDDEN_VALUE=hidden"}
	deliverer := NewExtensionDeliverer(config.DeliveryExtensionConfig{ExtensionID: "fixture.delivery", Command: []string{executable}, SHA256: digest(t, executable), Environment: []string{"ALLOWED_VALUE"}}, environment)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := deliverer.Describe(ctx); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	request := NewRequest(model.DriverNotification{NotificationID: "wake:1", Kind: "done", TaskID: "task_1", CreatedAt: time.Now(), ClaimOwner: "driver:hermes", ClaimToken: "token", ClaimUntil: &until}, "watch:1", 1, nil)
	result, err := deliverer.Deliver(ctx, request)
	if err != nil || result != (Result{Outcome: OutcomeDelivered, Detail: "visible/unset"}) {
		t.Fatalf("result = %+v, err = %v", result, err)
	}

	wrongID := NewExtensionDeliverer(config.DeliveryExtensionConfig{ExtensionID: "other.delivery", Command: []string{executable}, SHA256: digest(t, executable)}, environment)
	if err := wrongID.Describe(ctx); !protocolViolation(err) {
		t.Fatalf("wrong extension identity = %v", err)
	}
	broad := writeScript(t, dir, "broad", "#!/bin/sh\nprintf '%s\\n' '"+strings.Replace(manifest, `"operations":["deliver"]`, `"operations":["deliver","ack"]`, 1)+"'\n")
	if err := NewExtensionDeliverer(config.DeliveryExtensionConfig{ExtensionID: "fixture.delivery", Command: []string{broad}, SHA256: digest(t, broad)}, nil).Describe(ctx); !protocolViolation(err) {
		t.Fatalf("broader manifest = %v", err)
	}
	unpinned := NewExtensionDeliverer(config.DeliveryExtensionConfig{ExtensionID: "fixture.delivery", Command: []string{executable}, SHA256: strings.Repeat("0", 64)}, environment)
	if err := unpinned.Describe(ctx); err == nil || !strings.Contains(err.Error(), "SHA-256 does not match") {
		t.Fatalf("digest mismatch = %v", err)
	}
	invalid := request
	invalid.Notification.Kind = "coordinator-question"
	if _, err := deliverer.Deliver(ctx, invalid); err == nil {
		t.Fatal("invalid request was delivered")
	}
	badResult := writeScript(t, dir, "bad-result", strings.Replace(script, `"outcome":"delivered"`, `"outcome":"acknowledged"`, 1))
	if _, err := NewExtensionDeliverer(config.DeliveryExtensionConfig{ExtensionID: "fixture.delivery", Command: []string{badResult}, SHA256: digest(t, badResult)}, nil).Deliver(ctx, request); !protocolViolation(err) {
		t.Fatalf("invalid outcome = %v", err)
	}
}

func protocolViolation(err error) bool {
	var hostErr *extensionhost.HostError
	return errors.As(err, &hostErr) && hostErr.Kind == extensionhost.ErrorProtocolViolation
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func digest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
