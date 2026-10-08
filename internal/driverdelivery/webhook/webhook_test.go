package webhook

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/driverdelivery"
	extensionhost "shephrd/internal/extension"
)

func TestSignatureMatchesStandardWebhooksVector(t *testing.T) {
	key, err := readSecret(writeSecret(t, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if got := Signature(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330", []byte(`{"test": 2432232314}`)); got != "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Fatalf("signature = %s", got)
	}
}

func TestWebhookIDIsClaimScopedDigest(t *testing.T) {
	if got := WebhookID("wake:1", "token-a"); got != "9c7c0659437c8d313496942f308f62d5fb9f9e19a41dc4339dce75fc2831156e" {
		t.Fatalf("webhook id = %s", got)
	}
	if WebhookID("wake:1", "token-a") == WebhookID("wake:1", "token-b") || strings.Contains(WebhookID("wake:1", "token-a"), "token-a") {
		t.Fatal("webhook id must change with the claim and never expose the token")
	}
}

func TestStatusMapping(t *testing.T) {
	for status, want := range map[int]string{
		200: driverdelivery.OutcomeDelivered, 204: driverdelivery.OutcomeDelivered,
		408: driverdelivery.OutcomeRetryable, 429: driverdelivery.OutcomeRetryable, 500: driverdelivery.OutcomeRetryable, 503: driverdelivery.OutcomeRetryable,
		301: driverdelivery.OutcomeRejected, 400: driverdelivery.OutcomeRejected, 401: driverdelivery.OutcomeRejected, 404: driverdelivery.OutcomeRejected, 422: driverdelivery.OutcomeRejected,
	} {
		if got := Outcome(status); got.Outcome != want || driverdelivery.ValidateResult(got) != nil {
			t.Errorf("status %d = %+v, want %s", status, got, want)
		}
	}
}

func TestDescribeDeclaresExactDeliveryCapability(t *testing.T) {
	secret := writeSecret(t, "shared-secret", 0o600)
	var output bytes.Buffer
	if err := Run([]string{"--url", "https://receiver.invalid/hook", "--secret-file", secret, "describe"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	var manifest extensionhost.Manifest
	if err := extensionhost.StrictDecode(output.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := extensionhost.ValidateManifest(manifest, ExtensionID, []extensionhost.Capability{driverdelivery.Capability()}); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsFailClosed(t *testing.T) {
	good := writeSecret(t, "shared-secret", 0o600)
	for name, args := range map[string][]string{
		"plain http":       {"--url", "http://receiver.invalid/hook", "--secret-file", good},
		"credentials":      {"--url", "https://user:pass@receiver.invalid/hook", "--secret-file", good},
		"relative url":     {"--url", "/hook", "--secret-file", good},
		"relative secret":  {"--url", "https://receiver.invalid/hook", "--secret-file", "secret"},
		"readable secret":  {"--url", "https://receiver.invalid/hook", "--secret-file", writeSecret(t, "shared-secret", 0o644)},
		"empty secret":     {"--url", "https://receiver.invalid/hook", "--secret-file", writeSecret(t, " \n", 0o600)},
		"invalid whsec":    {"--url", "https://receiver.invalid/hook", "--secret-file", writeSecret(t, "whsec_???", 0o600)},
		"unknown flag":     {"--url", "https://receiver.invalid/hook", "--secret-file", good, "--insecure"},
		"missing settings": {},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Run(append(args, "describe"), strings.NewReader(""), io.Discard); err == nil {
				t.Fatal("settings were accepted")
			}
		})
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"--url", "https://receiver.invalid/hook", "--secret-file", link, "describe"}, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("symlinked secret was accepted")
	}
	if err := Run([]string{"--url", "http://127.0.0.1:1/hook", "--secret-file", good, "--allow-http", "describe"}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("explicit plain HTTP rejected: %v", err)
	}
}

func TestInvokePostsSignedExactPayload(t *testing.T) {
	type received struct {
		header http.Header
		body   []byte
	}
	var mu sync.Mutex
	var deliveries []received
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		deliveries = append(deliveries, received{header: request.Header.Clone(), body: body})
		mu.Unlock()
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write(bytes.Repeat([]byte("x"), 1<<20))
	}))
	defer server.Close()
	secret := writeSecret(t, "shared-secret", 0o600)
	request := deliveryRequest(t, sampleRequest())
	result, response := invokeExtension(t, []string{"--url", server.URL + "/hook", "--secret-file", secret, "--allow-http", "invoke"}, request)
	if response.Status != "ok" || result.Outcome != driverdelivery.OutcomeDelivered || result.Detail != "HTTP 202" {
		t.Fatalf("response = %+v, result = %+v", response, result)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d", len(deliveries))
	}
	delivered := deliveries[0]
	if !bytes.Equal(delivered.body, request.Payload) {
		t.Fatalf("body = %s, want %s", delivered.body, request.Payload)
	}
	id := delivered.header.Get("webhook-id")
	timestamp := delivered.header.Get("webhook-timestamp")
	if id != WebhookID("wake:1", "token-a") || timestamp == "" || delivered.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %+v", delivered.header)
	}
	if delivered.header.Get("webhook-signature") != Signature([]byte("shared-secret"), id, timestamp, delivered.body) {
		t.Fatalf("signature = %s", delivered.header.Get("webhook-signature"))
	}
	for name, values := range delivered.header {
		for _, value := range values {
			if strings.Contains(value, "token-a") {
				t.Fatalf("header %s exposes the claim token", name)
			}
		}
	}
}

func TestInvokeMapsReceiverFailuresWithoutRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	statuses := map[string]int{"/unavailable": 503, "/limited": 429, "/forbidden": 403}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redirect":
			http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
		case "/slow":
			time.Sleep(3 * time.Second)
		default:
			writer.WriteHeader(statuses[request.URL.Path])
		}
	}))
	defer server.Close()
	secret := writeSecret(t, "shared-secret", 0o600)
	for path, want := range map[string]string{"/unavailable": "retryable", "/limited": "retryable", "/forbidden": "rejected", "/redirect": "rejected", "/slow": "retryable"} {
		t.Run(path, func(t *testing.T) {
			request := deliveryRequest(t, sampleRequest())
			request.DeadlineUnixMS = time.Now().Add(1500 * time.Millisecond).UnixMilli()
			started := time.Now()
			result, response := invokeExtension(t, []string{"--url", server.URL + path, "--secret-file", secret, "--allow-http", "invoke"}, request)
			if response.Status != "ok" || result.Outcome != want || time.Since(started) > 2*time.Second {
				t.Fatalf("response = %+v, result = %+v, duration = %s", response, result, time.Since(started))
			}
		})
	}
	listener := httptest.NewServer(http.NotFoundHandler())
	closedURL := listener.URL
	listener.Close()
	result, _ := invokeExtension(t, []string{"--url", closedURL, "--secret-file", secret, "--allow-http", "invoke"}, deliveryRequest(t, sampleRequest()))
	if result.Outcome != driverdelivery.OutcomeRetryable || result.Detail != "receiver connection failed" {
		t.Fatalf("connection failure = %+v", result)
	}
	if redirected {
		t.Fatal("redirect was followed")
	}
}

func TestInvokeRejectsInvalidRequestsWithoutPosting(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posts++ }))
	defer server.Close()
	secret := writeSecret(t, "shared-secret", 0o600)
	args := []string{"--url", server.URL, "--secret-file", secret, "--allow-http", "invoke"}
	legacy := sampleRequest()
	legacy.Notification.Kind = "coordinator-question"
	oversized := sampleRequest()
	oversized.Notification.Payload = strings.Repeat("x", driverdelivery.MaxPayloadBytes+1)
	for name, payload := range map[string]json.RawMessage{
		"unknown field": json.RawMessage(`{"driver":{"id":"driver:hermes","generation":"watch:1"},"extra":true}`),
		"legacy kind":   deliveryRequest(t, legacy).Payload,
		"oversized":     deliveryRequest(t, oversized).Payload,
	} {
		t.Run(name, func(t *testing.T) {
			request := deliveryRequest(t, sampleRequest())
			request.Payload = payload
			var output bytes.Buffer
			if err := Run(args, requestReader(t, request), &output); err != nil {
				t.Fatal(err)
			}
			var response extensionhost.Response
			if err := extensionhost.StrictDecode(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != "error" || response.Error == nil || response.Error.Effect != "none" {
				t.Fatalf("response = %+v", response)
			}
		})
	}
	envelope := deliveryRequest(t, sampleRequest())
	envelope.Capability = "notification.presentation"
	if err := Run(args, requestReader(t, envelope), io.Discard); err == nil {
		t.Fatal("foreign capability envelope was accepted")
	}
	expired := deliveryRequest(t, sampleRequest())
	expired.DeadlineUnixMS = time.Now().Add(-time.Second).UnixMilli()
	if err := Run(args, requestReader(t, expired), io.Discard); err == nil {
		t.Fatal("expired envelope was accepted")
	}
	if posts != 0 {
		t.Fatalf("posts = %d", posts)
	}
}

func sampleRequest() driverdelivery.Request {
	return driverdelivery.Request{
		Driver: driverdelivery.Driver{ID: "driver:hermes", Generation: "watch:1"},
		Notification: driverdelivery.Notification{
			NotificationID: "wake:1", Kind: "subdriver-question", RequestID: "request_1", SubdriverID: "subdriver_1",
			SubdriverRepoName: "demo", SubdriverEventID: 7, Payload: "Which format?", CreatedAt: time.Unix(1700000000, 0).UTC(),
		},
		Claim: driverdelivery.Claim{ClaimToken: "token-a", ClaimUntil: time.Unix(1700000300, 0).UTC(), DeliveryAttempt: 1},
		Commands: driverdelivery.Commands{
			Read:    []string{"shephrd", "subdriver", "event", "7", "--json"},
			Request: []string{"shephrd", "subdriver", "request", "request_1", "--json"},
			Reply:   []string{"shephrd", "subdriver", "reply", "request_1", "<text>", "--reply-to", "7", "--key", "<key>", "--driver-id", "driver:hermes", "--json"},
			Ack:     []string{"shephrd", "wake", "ack", "--claim-token", "token-a", "--driver-id", "driver:hermes", "--json"},
		},
	}
}

func deliveryRequest(t *testing.T, delivery driverdelivery.Request) extensionhost.Request {
	t.Helper()
	payload, err := json.Marshal(delivery)
	if err != nil {
		t.Fatal(err)
	}
	return extensionhost.Request{
		Wire:      extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor},
		RequestID: "extension_request_000000000000000000000000", Capability: driverdelivery.CapabilityName,
		CapabilityVersion: driverdelivery.CapabilityVersion, Operation: driverdelivery.DeliverOperation,
		DeadlineUnixMS: time.Now().Add(5 * time.Second).UnixMilli(), Payload: payload,
	}
}

func invokeExtension(t *testing.T, args []string, request extensionhost.Request) (driverdelivery.Result, extensionhost.Response) {
	t.Helper()
	var output bytes.Buffer
	if err := Run(args, requestReader(t, request), &output); err != nil {
		t.Fatal(err)
	}
	var response extensionhost.Response
	if err := extensionhost.StrictDecode(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var result driverdelivery.Result
	if response.Status == "ok" {
		if err := extensionhost.StrictDecode(response.Result, &result); err != nil {
			t.Fatal(err)
		}
	}
	return result, response
}

func requestReader(t *testing.T, request extensionhost.Request) *bytes.Reader {
	t.Helper()
	frame, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(append(frame, '\n'))
}

func writeSecret(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}
