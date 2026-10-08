package notification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	extensionhost "shephrd/internal/extension"
	"shephrd/internal/process"
)

type testPresenter struct {
	presentation Presentation
	calls        int
	present      func(context.Context, Presentation) (PresentationResult, error)
}

func (p *testPresenter) Name() string {
	return "fixture.notification"
}

func (p *testPresenter) Present(ctx context.Context, presentation Presentation) (PresentationResult, error) {
	p.calls++
	p.presentation = presentation
	if p.present != nil {
		return p.present(ctx, presentation)
	}
	return PresentationResult{Presented: true}, nil
}

func TestNotifierUsesSafeBoundedPresentationAndPrivacyDefaults(t *testing.T) {
	presenter := &testPresenter{}
	notifier := New(Options{Presenter: presenter})
	result, err := notifier.Notify(context.Background(), Notice{
		ID: "wake:1", TaskID: "task", TaskLabel: strings.Repeat("界", 200) + "\nlabel", Kind: "question",
		ShortSummary: "secret payload\nwith control", PayloadFingerprint: "fingerprint",
	})
	if err != nil || !result.Presented || presenter.calls != 1 {
		t.Fatalf("result = %+v, calls = %d, err = %v", result, presenter.calls, err)
	}
	if presenter.presentation.Title != "Shephrd" || strings.Contains(presenter.presentation.Body, "secret payload") || strings.ContainsAny(presenter.presentation.Body, "\r\n") {
		t.Fatalf("presentation = %+v", presenter.presentation)
	}
	if err := ValidatePresentation(presenter.presentation); err != nil || len([]byte(presenter.presentation.Body)) > MaxPresentationBodyBytes {
		t.Fatalf("presentation = %+v, err = %v", presenter.presentation, err)
	}
}

func TestNotifierDeduplicatesAndRateLimitsInCore(t *testing.T) {
	current := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	presenter := &testPresenter{}
	notifier := New(Options{Presenter: presenter, Now: func() time.Time { return current }, TaskPerMinute: 2, GlobalPerMinute: 10})
	notice := Notice{TaskID: "task", TaskLabel: "demo", Kind: "question", ShortSummary: "summary", PayloadFingerprint: "same"}
	if _, err := notifier.Notify(context.Background(), notice); err != nil {
		t.Fatal(err)
	}
	if _, err := notifier.Notify(context.Background(), notice); !IsSuppressed(err) {
		t.Fatalf("duplicate error = %v", err)
	}
	current = current.Add(31 * time.Second)
	if _, err := notifier.Notify(context.Background(), Notice{TaskID: "task", TaskLabel: "demo", Kind: "question", PayloadFingerprint: "different"}); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Second)
	if _, err := notifier.Notify(context.Background(), Notice{TaskID: "task", TaskLabel: "demo", Kind: "question", PayloadFingerprint: "third"}); !IsSuppressed(err) {
		t.Fatalf("rate error = %v", err)
	}
	if presenter.calls != 2 {
		t.Fatalf("presentation calls = %d", presenter.calls)
	}
}

func TestNotifierAppliesBoundedTimeout(t *testing.T) {
	presenter := &testPresenter{present: func(ctx context.Context, _ Presentation) (PresentationResult, error) {
		<-ctx.Done()
		return PresentationResult{}, ctx.Err()
	}}
	notifier := New(Options{Presenter: presenter, Timeout: 20 * time.Millisecond})
	started := time.Now()
	_, err := notifier.Notify(context.Background(), Notice{TaskID: "task", TaskLabel: "demo", Kind: "done"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("error = %v, duration = %s", err, time.Since(started))
	}
}

func TestNotificationManifestDeclaresOnlyTypedPresentation(t *testing.T) {
	manifest := Manifest()
	if err := extensionhost.ValidateManifest(manifest, ExtensionID, []extensionhost.Capability{Capability()}); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Capabilities) != 1 || manifest.Capabilities[0].Name != CapabilityName || len(manifest.Capabilities[0].Operations) != 1 || manifest.Capabilities[0].Operations[0] != PresentationOperation {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestConfiguredNotificationExtensionUsesRealSubprocess(t *testing.T) {
	executable := buildPresentationExtension(t)
	logPath := filepath.Join(t.TempDir(), "presentation.json")
	presenter := NewExtensionPresenter([]string{executable, logPath, "success"}, digestFile(t, executable), nil)
	notifier := New(Options{Presenter: presenter, Details: true, Timeout: 10 * time.Second})
	result, err := notifier.Notify(context.Background(), Notice{TaskID: "task", TaskLabel: "demo/task", Kind: "question", ShortSummary: "bounded detail"})
	if err != nil || !result.Presented {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var presentation Presentation
	if err := extensionhost.StrictDecode(payload, &presentation); err != nil {
		t.Fatal(err)
	}
	if presentation.Title != "Shephrd" || !strings.Contains(presentation.Body, "bounded detail") || strings.Contains(string(payload), "task_id") {
		t.Fatalf("presentation = %+v", presentation)
	}
}

func TestNotificationExtensionTimeoutReapsSubprocessTree(t *testing.T) {
	executable := buildPresentationExtension(t)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	presenter := NewExtensionPresenter([]string{executable, pidPath, "hang"}, digestFile(t, executable), nil)
	describeContext, describeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := presenter.host.Describe(describeContext); err != nil {
		describeCancel()
		t.Fatal(err)
	}
	describeCancel()
	notifier := New(Options{Presenter: presenter, Timeout: 300 * time.Millisecond})
	started := time.Now()
	_, err := notifier.Notify(context.Background(), Notice{TaskID: "task", TaskLabel: "demo", Kind: "done"})
	var hostErr *extensionhost.HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != extensionhost.ErrorDeadlineExceeded || time.Since(started) > 7*time.Second {
		t.Fatalf("error = %v, duration = %s", err, time.Since(started))
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for process.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if process.Alive(pid) {
		t.Fatalf("extension descendant %d remains alive", pid)
	}
}

func buildPresentationExtension(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "presentation-extension")
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	command := exec.Command("go", "build", "-o", executable, "./internal/notification/testdata/presentationextension")
	command.Dir = projectRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build presentation extension: %s: %v", output, err)
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	return executable
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
