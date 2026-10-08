package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shephrd/internal/process"
)

const hostTestRequestID = "extension_request_000000000000000000000000"

func TestHostEnvironmentIsExplicitlyAllowlisted(t *testing.T) {
	filtered := filterEnvironment([]string{"PATH=/bin", "SECRET_TOKEN=credential", "HOME=/home/fixture"}, "PATH", "HOME")
	if strings.Join(filtered, "\n") != "PATH=/bin\nHOME=/home/fixture" {
		t.Fatalf("environment = %q", filtered)
	}
}

func TestHostInvokesStrictCapabilityEnvelope(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke) IFS= read -r request; printf '%s\n' '{"wire":{"major":1,"minor":0},"request_id":"`+hostTestRequestID+`","capability":"fixture.capability","capability_version":1,"operation":"ping","status":"ok","result":{"value":"pong"},"extension":{"id":"fixture.extension","version":"1.0.0"}}' ;;
  *) exit 2 ;;
esac
`)
	host := newTestHost(t, script, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result struct {
		Value string `json:"value"`
	}
	if err := host.Invoke(ctx, "fixture.capability", 1, "ping", map[string]string{"value": "request"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Value != "pong" {
		t.Fatalf("result = %+v", result)
	}
}

func TestHostInvokeRetriesDescribeAfterTransientFailure(t *testing.T) {
	attemptsPath := filepath.Join(t.TempDir(), "describe-attempts")
	script := writeHostExtension(t, `
case "$1" in
  describe)
    printf x >> "$ATTEMPTS_FILE"
    if [ "$(wc -c < "$ATTEMPTS_FILE")" -eq 1 ]; then
      printf 'transient describe failure\n' >&2
      exit 7
    fi
    printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}'
    ;;
  invoke)
    IFS= read -r request
    printf '%s\n' '{"wire":{"major":1,"minor":0},"request_id":"`+hostTestRequestID+`","capability":"fixture.capability","capability_version":1,"operation":"ping","status":"ok","result":{"value":"pong"},"extension":{"id":"fixture.extension","version":"1.0.0"}}'
    ;;
esac
`)
	host := newTestHost(t, script, []string{"ATTEMPTS_FILE=" + attemptsPath})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result struct {
		Value string `json:"value"`
	}
	if err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &result); err == nil || !strings.Contains(err.Error(), "transient describe failure") {
		t.Fatalf("first invoke error = %v", err)
	}
	if err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &result); err != nil || result.Value != "pong" {
		t.Fatalf("second invoke result = %+v, error = %v", result, err)
	}
	if _, err := host.Describe(ctx); err != nil {
		t.Fatalf("cached describe error = %v", err)
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil || string(attempts) != "xx" {
		t.Fatalf("describe attempts = %q, err = %v", attempts, err)
	}
}

func TestHostConcurrentDescribeRetriesUntilOneSuccess(t *testing.T) {
	attemptsPath := filepath.Join(t.TempDir(), "describe-attempts")
	script := writeHostExtension(t, `
case "$1" in
  describe)
    printf x >> "$ATTEMPTS_FILE"
    if [ "$(wc -c < "$ATTEMPTS_FILE")" -eq 1 ]; then exit 7; fi
    printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}'
    ;;
esac
`)
	host := newTestHost(t, script, []string{"ATTEMPTS_FILE=" + attemptsPath})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const callers = 32
	start := make(chan struct{})
	errorsSeen := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := host.Describe(ctx)
			errorsSeen <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsSeen)
	failures := 0
	for err := range errorsSeen {
		if err != nil {
			failures++
		}
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil || string(attempts) != "xx" || failures != 1 {
		t.Fatalf("describe attempts = %q, failures = %d, err = %v", attempts, failures, err)
	}
}

func TestHostRejectsIdentityMismatchAndStdoutContamination(t *testing.T) {
	for _, test := range []struct {
		name      string
		requestID string
		trailing  string
	}{
		{name: "request identity", requestID: "extension_request_111111111111111111111111"},
		{name: "stdout contamination", requestID: hostTestRequestID, trailing: "printf 'noise\\n'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := writeHostExtension(t, fmt.Sprintf(`
case "$1" in
  describe) printf '%%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke) IFS= read -r request; printf '%%s\n' '{"wire":{"major":1,"minor":0},"request_id":"%s","capability":"fixture.capability","capability_version":1,"operation":"ping","status":"ok","result":{},"extension":{"id":"fixture.extension","version":"1.0.0"}}'; %s ;;
esac
`, test.requestID, test.trailing))
			host := newTestHost(t, script, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &struct{}{})
			if err == nil {
				t.Fatal("invalid extension output was accepted")
			}
		})
	}
}

func TestHostValidatesTrailingStdoutAfterInputEOF(t *testing.T) {
	for _, test := range []struct {
		name     string
		trailing string
		reject   bool
	}{
		{name: "empty", trailing: ":"},
		{name: "whitespace", trailing: "printf ' \\t\\n'"},
		{name: "whitespace at bound", trailing: fmt.Sprintf("printf '%%%ds' ''", MaxFrameBytes)},
		{name: "noise", trailing: "printf 'noise\\n'", reject: true},
		{name: "whitespace beyond bound", trailing: fmt.Sprintf("printf '%%%ds' ''", MaxFrameBytes+1), reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			phasePath := filepath.Join(t.TempDir(), "phases")
			script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke)
    IFS= read -r request
    printf '%s\n' '{"wire":{"major":1,"minor":0},"request_id":"`+hostTestRequestID+`","capability":"fixture.capability","capability_version":1,"operation":"ping","status":"ok","result":{"value":"pong"},"extension":{"id":"fixture.extension","version":"1.0.0"}}'
    printf 'response-written\n' >> "$PHASE_FILE"
    if IFS= read -r extra; then exit 9; fi
    printf 'stdin-eof\n' >> "$PHASE_FILE"
    `+test.trailing+`
    printf 'trailing-written\n' >> "$PHASE_FILE"
    ;;
esac
`)
			host := newTestHost(t, script, []string{"PHASE_FILE=" + phasePath})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var result struct {
				Value string `json:"value"`
			}
			err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &result)
			phases, phaseErr := os.ReadFile(phasePath)
			t.Logf("result=%+v error=%v context_error=%v shell_phases=%q phase_error=%v", result, err, ctx.Err(), phases, phaseErr)
			if phaseErr != nil || string(phases) != "response-written\nstdin-eof\ntrailing-written\n" || result.Value != "pong" || ctx.Err() != nil {
				t.Fatal("fixture did not reach trailing validation after a valid response")
			}
			if test.reject {
				var hostErr *HostError
				if !errors.As(err, &hostErr) || hostErr.Operation != "ping" || !strings.Contains(err.Error(), "unexpected protocol output") {
					t.Fatalf("invalid trailing output error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid output rejected: %v", err)
			}
		})
	}
}

func TestInvocationFinishTerminatesUnfinishedOutput(t *testing.T) {
	for _, test := range []struct {
		name     string
		deadline time.Duration
		kind     string
	}{
		{name: "deadline", deadline: 300 * time.Millisecond, kind: ErrorDeadlineExceeded},
		{name: "cancel", deadline: time.Minute, kind: ErrorCancelled},
		{name: "finish bound", deadline: time.Minute, kind: ErrorExtensionExit},
	} {
		t.Run(test.name, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "child.pid")
			script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke)
    IFS= read -r request
    /bin/sleep 60 & child=$!; printf '%s' "$child" > "$PID_FILE"
    printf '%s\n' '{"wire":{"major":1,"minor":0},"request_id":"`+hostTestRequestID+`","capability":"fixture.capability","capability_version":1,"operation":"ping","status":"ok","result":{},"extension":{"id":"fixture.extension","version":"1.0.0"}}'
    if IFS= read -r extra; then exit 9; fi
    wait
    ;;
esac
`)
			host := newTestHost(t, script, []string{"PID_FILE=" + pidPath})
			describeContext, describeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if _, err := host.Describe(describeContext); err != nil {
				describeCancel()
				t.Fatal(err)
			}
			describeCancel()
			ctx, cancel := context.WithTimeout(context.Background(), test.deadline)
			defer cancel()
			invocation, err := host.Begin(ctx, "fixture.capability", 1, "ping", struct{}{})
			if err != nil {
				t.Fatal(err)
			}
			defer invocation.Cancel()
			if _, err := invocation.Receive(&struct{}{}, "ok"); err != nil {
				t.Fatal(err)
			}
			t.Log("valid response received before finish")
			if test.name == "cancel" {
				cancel()
			}
			started := time.Now()
			err = invocation.Finish()
			var hostErr *HostError
			if !errors.As(err, &hostErr) || hostErr.Kind != test.kind || hostErr.Operation != "ping" || time.Since(started) > 7*time.Second {
				t.Fatalf("finish error = %v, duration = %s", err, time.Since(started))
			}
			if test.name == "finish bound" && !strings.Contains(err.Error(), "did not finish within its bound") {
				t.Fatalf("finish bound error = %v", err)
			}
			data, readErr := os.ReadFile(pidPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var pid int
			if _, err := fmt.Sscan(string(data), &pid); err != nil {
				t.Fatal(err)
			}
			if process.Alive(pid) || process.Alive(invocation.session.cmd.Process.Pid) {
				t.Fatal("unfinished extension process tree remains alive")
			}
			t.Logf("finish error=%v; extension process and descendant %d reaped", err, pid)
		})
	}
}

func TestHostBoundsAndRedactsExtensionDiagnostics(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke) IFS= read -r request; printf '%s\n' "TEST_TOKEN=$TEST_TOKEN and $TEST_TOKEN" >&2; exit 7 ;;
esac
`)
	host := newTestHost(t, script, []string{"TEST_TOKEN=credential-value"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &struct{}{})
	if err == nil || strings.Contains(err.Error(), "credential-value") || strings.Contains(err.Error(), "TEST_TOKEN=") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostRedactsBoundedRemoteFailure(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke) IFS= read -r request; printf '%s\n' '{"wire":{"major":1,"minor":0},"request_id":"`+hostTestRequestID+`","capability":"fixture.capability","capability_version":1,"operation":"ping","status":"error","error":{"class":"unavailable","code":"fixture_failed","message":"TEST_TOKEN=credential-value","effect":"none"},"extension":{"id":"fixture.extension","version":"1.0.0"}}' ;;
esac
`)
	host := newTestHost(t, script, []string{"TEST_TOKEN=credential-value"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &struct{}{})
	var remote *RemoteError
	if !errors.As(err, &remote) || strings.Contains(err.Error(), "credential-value") || strings.Contains(err.Error(), "TEST_TOKEN=") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostDeadlineTerminatesAndReapsExtensionProcessTree(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke) IFS= read -r request; /bin/sleep 60 & child=$!; printf '%s' "$child" > "$PID_FILE"; wait ;;
esac
`)
	host := newTestHost(t, script, []string{"PID_FILE=" + pidPath})
	describeContext, describeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := host.Describe(describeContext); err != nil {
		describeCancel()
		t.Fatal(err)
	}
	describeCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &struct{}{})
	var hostErr *HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != ErrorDeadlineExceeded || time.Since(started) > 7*time.Second {
		t.Fatalf("error = %v, duration = %s", err, time.Since(started))
	}
	data, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil {
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

func TestHostCancellationTerminatesInvocation(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["ping"]}]}' ;;
  invoke) IFS= read -r request; /bin/sleep 60 ;;
esac
`)
	host := newTestHost(t, script, nil)
	describeContext, describeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := host.Describe(describeContext); err != nil {
		describeCancel()
		t.Fatal(err)
	}
	describeCancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	cancel()
	err := host.Invoke(ctx, "fixture.capability", 1, "ping", struct{}{}, &struct{}{})
	var hostErr *HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != ErrorCancelled {
		t.Fatalf("error = %v", err)
	}
}

func TestHostRejectsUntrustedExecutableBeforeInvocation(t *testing.T) {
	t.Run("digest", func(t *testing.T) {
		script := writeHostExtension(t, "exit 0")
		host := newTestHost(t, script, nil)
		host.config.SHA256 = strings.Repeat("0", 64)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := host.Describe(ctx); err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("writable mode", func(t *testing.T) {
		script := writeHostExtension(t, "exit 0")
		if err := os.Chmod(script, 0o722); err != nil {
			t.Fatal(err)
		}
		host := newTestHost(t, script, nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := host.Describe(ctx); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := writeHostExtension(t, "exit 0")
		link := filepath.Join(t.TempDir(), "extension")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		host := newTestHost(t, target, nil)
		host.config.Command[0] = link
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := host.Describe(ctx); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("error = %v", err)
		}
	})
}

func newTestHost(t *testing.T, executable string, environment []string) *Host {
	t.Helper()
	allowed := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		allowed = append(allowed, key)
	}
	return NewHost(HostConfig{
		Command:              []string{executable},
		SHA256:               fileSHA256(t, executable),
		ParentEnvironment:    environment,
		AllowedEnvironment:   allowed,
		ExpectedID:           "fixture.extension",
		ExpectedCapabilities: []Capability{{Name: "fixture.capability", Version: 1, Operations: []string{"ping"}}},
		RequestID:            func() (string, error) { return hostTestRequestID, nil },
		describeTimeout:      5 * time.Second,
	})
}

func writeHostExtension(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extension")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

const hostTestCreateManifest = `{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"fixture.extension","version":"1.0.0"},"capabilities":[{"name":"fixture.capability","version":1,"operations":["create"]}]}`

type hostTestPreparedResult struct {
	Endpoint struct {
		ID string `json:"id"`
	} `json:"endpoint"`
}

func hostTestCreateResponse(status string, body string) string {
	return fmt.Sprintf(`{"wire":{"major":1,"minor":0},"request_id":"%s","capability":"fixture.capability","capability_version":1,"operation":"create","status":%q,"result":%s,"extension":{"id":"fixture.extension","version":"1.0.0"}}`, hostTestRequestID, status, body)
}

func newCreateTestHost(t *testing.T, executable string, environment []string, abortTimeout time.Duration) *Host {
	t.Helper()
	allowed := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		allowed = append(allowed, key)
	}
	host := NewHost(HostConfig{
		Command:              []string{executable},
		SHA256:               fileSHA256(t, executable),
		ParentEnvironment:    environment,
		AllowedEnvironment:   allowed,
		ExpectedID:           "fixture.extension",
		ExpectedCapabilities: []Capability{{Name: "fixture.capability", Version: 1, Operations: []string{"create"}}},
		RequestID:            func() (string, error) { return hostTestRequestID, nil },
		describeTimeout:      5 * time.Second,
		AbortTimeout:         abortTimeout,
	})
	return host
}

func TestInvocationAbortProvesCleanupWithinBound(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '`+hostTestCreateManifest+`' ;;
  invoke)
    IFS= read -r request
    printf '%s\n' '`+hostTestCreateResponse("prepared", `{"endpoint":{"id":"fixture"}}`)+`'
    IFS= read -r control
    case "$control" in
      *'"action":"abort"'*) printf '%s\n' '`+hostTestCreateResponse("aborted", `{"ok":true}`)+`' ;;
      *) exit 9 ;;
    esac
    ;;
  *) exit 2 ;;
esac
`)
	host := newCreateTestHost(t, script, nil, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invocation, err := host.Begin(ctx, "fixture.capability", 1, "create", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var prepared hostTestPreparedResult
	if _, err := invocation.Receive(&prepared, "prepared"); err != nil {
		t.Fatal(err)
	}
	if err := invocation.Abort(); err != nil {
		t.Fatalf("abort error = %v", err)
	}
}

func TestInvocationAbortSurfacesUncertaintyWhenExtensionIgnoresAbort(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '`+hostTestCreateManifest+`' ;;
  invoke)
    IFS= read -r request
    printf '%s\n' '`+hostTestCreateResponse("prepared", `{"endpoint":{"id":"fixture"}}`)+`'
    IFS= read -r _
    /bin/sleep 60 & child=$!; printf '%s' "$child" > "$PID_FILE"; wait ;;
  *) exit 2 ;;
esac
`)
	host := newCreateTestHost(t, script, []string{"PID_FILE=" + pidPath}, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invocation, err := host.Begin(ctx, "fixture.capability", 1, "create", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var prepared hostTestPreparedResult
	if _, err := invocation.Receive(&prepared, "prepared"); err != nil {
		t.Fatal(err)
	}
	abortErr := invocation.Abort()
	var hostErr *HostError
	if !errors.As(abortErr, &hostErr) || hostErr.Kind != ErrorAbortUncertain {
		t.Fatalf("abort error = %v", abortErr)
	}
	data, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil {
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

func TestInvocationAbortUncertainAfterCreateDeadline(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '`+hostTestCreateManifest+`' ;;
  invoke)
    IFS= read -r request
    /bin/sleep 60 & child=$!; printf '%s' "$child" > "$PID_FILE"; wait
    printf '%s\n' '`+hostTestCreateResponse("prepared", `{"endpoint":{"id":"fixture"}}`)+`' ;;
  *) exit 2 ;;
esac
`)
	host := newCreateTestHost(t, script, []string{"PID_FILE=" + pidPath}, 300*time.Millisecond)
	describeContext, describeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := host.Describe(describeContext); err != nil {
		describeCancel()
		t.Fatal(err)
	}
	describeCancel()
	t.Log("describe completed before starting the create deadline")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	invocation, err := host.Begin(ctx, "fixture.capability", 1, "create", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var prepared hostTestPreparedResult
	status, createErr := invocation.Receive(&prepared, "prepared")
	var hostErr *HostError
	if !errors.As(createErr, &hostErr) || hostErr.Kind != ErrorDeadlineExceeded || hostErr.Operation != "create" || status != "" || prepared.Endpoint.ID != "" {
		t.Fatalf("create status = %q, prepared = %+v, error = %v", status, prepared, createErr)
	}
	t.Logf("create phase: status=%q endpoint=%q error=%v", status, prepared.Endpoint.ID, createErr)
	abortErr := invocation.Abort()
	if !errors.As(abortErr, &hostErr) || hostErr.Kind != ErrorAbortUncertain || hostErr.Operation != "create" {
		t.Fatalf("abort error = %v", abortErr)
	}
	t.Logf("abort phase: %v", abortErr)
	data, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for process.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if process.Alive(pid) {
		t.Fatalf("extension descendant %d remains alive", pid)
	}
	t.Logf("extension descendant %d reaped; cleanup still unproven", pid)
}

func TestInvocationAbortAfterProtocolViolationUsesExplicitCleanup(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '`+hostTestCreateManifest+`' ;;
  invoke)
    IFS= read -r request
    printf '%s\n' '`+hostTestCreateResponse("bogus", `{}`)+`'
    IFS= read -r control
    case "$control" in
      *'"action":"abort"'*) printf '%s\n' '`+hostTestCreateResponse("aborted", `{"ok":true}`)+`' ;;
      *) exit 9 ;;
    esac
    ;;
  *) exit 2 ;;
esac
`)
	host := newCreateTestHost(t, script, nil, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invocation, err := host.Begin(ctx, "fixture.capability", 1, "create", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var prepared hostTestPreparedResult
	if _, err := invocation.Receive(&prepared, "prepared"); err == nil {
		t.Fatal("invalid status should be rejected")
	}
	if err := invocation.Abort(); err != nil {
		t.Fatalf("abort error = %v", err)
	}
}

func TestInvocationAbortAcceptsErrorEffectNone(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '`+hostTestCreateManifest+`' ;;
  invoke)
    IFS= read -r request
    printf '%s\n' '`+hostTestCreateResponse("bogus", `{}`)+`'
    IFS= read -r control
    case "$control" in
      *'"action":"abort"'*) printf '%s\n' '{"wire":{"major":1,"minor":0},"request_id":"`+hostTestRequestID+`","capability":"fixture.capability","capability_version":1,"operation":"create","status":"error","error":{"class":"endpoint_absent","code":"endpoint_absent","message":"no side effect","effect":"none"},"extension":{"id":"fixture.extension","version":"1.0.0"}}' ;;
      *) exit 9 ;;
    esac
    ;;
  *) exit 2 ;;
esac
`)
	host := newCreateTestHost(t, script, nil, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invocation, err := host.Begin(ctx, "fixture.capability", 1, "create", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var prepared hostTestPreparedResult
	if _, err := invocation.Receive(&prepared, "prepared"); err == nil {
		t.Fatal("invalid status should be rejected")
	}
	if err := invocation.Abort(); err != nil {
		t.Fatalf("abort with effect none should be proven, got %v", err)
	}
}

func TestInvocationAbortSurfacesErrorEffectUnknown(t *testing.T) {
	script := writeHostExtension(t, `
case "$1" in
  describe) printf '%s\n' '`+hostTestCreateManifest+`' ;;
  invoke)
    IFS= read -r request
    printf '%s\n' '`+hostTestCreateResponse("bogus", `{}`)+`'
    IFS= read -r control
    case "$control" in
      *'"action":"abort"'*) printf '%s\n' '{"wire":{"major":1,"minor":0},"request_id":"`+hostTestRequestID+`","capability":"fixture.capability","capability_version":1,"operation":"create","status":"error","error":{"class":"unavailable","code":"unavailable","message":"cleanup failed","effect":"unknown"},"extension":{"id":"fixture.extension","version":"1.0.0"}}' ;;
      *) exit 9 ;;
    esac
    ;;
  *) exit 2 ;;
esac
`)
	host := newCreateTestHost(t, script, nil, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invocation, err := host.Begin(ctx, "fixture.capability", 1, "create", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var prepared hostTestPreparedResult
	if _, err := invocation.Receive(&prepared, "prepared"); err == nil {
		t.Fatal("invalid status should be rejected")
	}
	abortErr := invocation.Abort()
	var remote *RemoteError
	if !errors.As(abortErr, &remote) || remote.Failure.Effect != "unknown" {
		t.Fatalf("abort error = %v", abortErr)
	}
}
