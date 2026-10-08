package macos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	extensionhost "shephrd/internal/extension"
	"shephrd/internal/notification"
)

func TestDescribeDeclaresTypedNotificationCapability(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"describe"}, strings.NewReader(""), &output, nil); err != nil {
		t.Fatal(err)
	}
	var manifest extensionhost.Manifest
	if err := extensionhost.StrictDecode(output.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := extensionhost.ValidateManifest(manifest, notification.ExtensionID, []extensionhost.Capability{notification.Capability()}); err != nil {
		t.Fatal(err)
	}
}

func TestInvokeDisplaysOnlyExactBoundedPresentation(t *testing.T) {
	request := presentationRequest(t, notification.Presentation{Title: "Shephrd", Body: "demo: question needs driver attention"})
	var displayed notification.Presentation
	var output bytes.Buffer
	if err := run([]string{"invoke"}, requestReader(t, request), &output, func(_ context.Context, presentation notification.Presentation) error {
		displayed = presentation
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var response extensionhost.Response
	if err := extensionhost.StrictDecode(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var result notification.PresentationResult
	if err := extensionhost.StrictDecode(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || !result.Presented || displayed.Title != "Shephrd" || displayed.Body != "demo: question needs driver attention" {
		t.Fatalf("response = %+v, displayed = %+v", response, displayed)
	}
}

func TestInvokeRejectsUnknownOrUnsafePresentationFields(t *testing.T) {
	for name, payload := range map[string]string{
		"unknown":   `{"title":"Shephrd","body":"bounded body","task_id":"task"}`,
		"control":   `{"title":"Shephrd","body":"unsafe\nbody"}`,
		"oversized": `{"title":"Shephrd","body":"` + strings.Repeat("x", notification.MaxPresentationBodyBytes+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := presentationRequest(t, notification.Presentation{Title: "Shephrd", Body: "placeholder"})
			request.Payload = json.RawMessage(payload)
			var calls int
			var output bytes.Buffer
			if err := run([]string{"invoke"}, requestReader(t, request), &output, func(context.Context, notification.Presentation) error {
				calls++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var response extensionhost.Response
			if err := extensionhost.StrictDecode(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != "error" || response.Error == nil || response.Error.Effect != "none" || calls != 0 {
				t.Fatalf("response = %+v, calls = %d", response, calls)
			}
		})
	}
}

func TestInvokeBoundsDisplayWithRequestDeadline(t *testing.T) {
	request := presentationRequest(t, notification.Presentation{Title: "Shephrd", Body: "bounded body"})
	request.DeadlineUnixMS = time.Now().Add(30 * time.Millisecond).UnixMilli()
	var output bytes.Buffer
	started := time.Now()
	if err := run([]string{"invoke"}, requestReader(t, request), &output, func(ctx context.Context, _ notification.Presentation) error {
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	var response extensionhost.Response
	if err := extensionhost.StrictDecode(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "error" || response.Error == nil || time.Since(started) > time.Second {
		t.Fatalf("response = %+v, duration = %s", response, time.Since(started))
	}
}

func TestInvokeRejectsInvalidEnvelopeWithoutDisplay(t *testing.T) {
	request := presentationRequest(t, notification.Presentation{Title: "Shephrd", Body: "bounded body"})
	request.Capability = "notification.other"
	if err := run([]string{"invoke"}, requestReader(t, request), &bytes.Buffer{}, func(context.Context, notification.Presentation) error {
		t.Fatal("display called")
		return nil
	}); err == nil {
		t.Fatal("invalid envelope was accepted")
	}
}

func TestLiveMacOSNotificationIsOptIn(t *testing.T) {
	if os.Getenv("SHEPHRD_REAL_MACOS_NOTIFICATION_E2E") != "1" {
		t.Skip("set SHEPHRD_REAL_MACOS_NOTIFICATION_E2E=1 to present a live macOS notification")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Notification Center is unavailable")
	}
	if _, err := os.Stat("/usr/bin/osascript"); err != nil {
		t.Skip("osascript is unavailable")
	}
	root := projectRoot(t)
	executable := filepath.Join(t.TempDir(), "shephrd-notification-macos")
	command := exec.Command("go", "build", "-o", executable, "./cmd/shephrd-notification-macos")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build notification extension: %s: %v", output, err)
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	presenter := notification.NewExtensionPresenter([]string{executable}, fileDigest(t, executable), os.Environ())
	notifier := notification.New(notification.Options{Presenter: presenter, Timeout: 2 * time.Second})
	result, err := notifier.Notify(context.Background(), notification.Notice{TaskID: "live-test", TaskLabel: "Shephrd live test", Kind: "notification"})
	if err != nil || !result.Presented {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func presentationRequest(t *testing.T, presentation notification.Presentation) extensionhost.Request {
	t.Helper()
	payload, err := json.Marshal(presentation)
	if err != nil {
		t.Fatal(err)
	}
	return extensionhost.Request{
		Wire:      extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor},
		RequestID: "extension_request_000000000000000000000000", Capability: notification.CapabilityName,
		CapabilityVersion: notification.CapabilityVersion, Operation: notification.PresentationOperation,
		DeadlineUnixMS: time.Now().Add(2 * time.Second).UnixMilli(), Payload: payload,
	}
}

func requestReader(t *testing.T, request extensionhost.Request) *bytes.Reader {
	t.Helper()
	frame, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(append(frame, '\n'))
}

func projectRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
