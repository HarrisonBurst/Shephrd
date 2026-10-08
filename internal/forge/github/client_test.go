package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	extensionhost "shephrd/internal/extension"
)

var (
	observerBinary string
	observerSHA    string
	observerOnce   sync.Once
	observerBuild  error
)

func buildObserver(t *testing.T) {
	t.Helper()
	observerOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			observerBuild = fmt.Errorf("cannot locate test source")
			return
		}
		projectRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
		root, err := os.MkdirTemp("", "github-observer-test")
		if err != nil {
			observerBuild = err
			return
		}
		binary := filepath.Join(root, "shephrd-github-observer")
		build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd-github-observer")
		build.Dir = projectRoot
		if output, err := build.CombinedOutput(); err != nil {
			observerBuild = fmt.Errorf("%s: %w", output, err)
			return
		}
		body, err := os.ReadFile(binary)
		if err != nil {
			observerBuild = err
			return
		}
		sum := sha256.Sum256(body)
		observerBinary = binary
		observerSHA = hex.EncodeToString(sum[:])
	})
	if observerBuild != nil {
		t.Fatal(observerBuild)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if observerBinary != "" {
		_ = os.RemoveAll(filepath.Dir(observerBinary))
	}
	os.Exit(code)
}

func fakeGHPath(t *testing.T, script string) string {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func testEnvironment(extra string) []string {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		environment[key] = value
	}
	for _, entry := range strings.Split(extra, "\n") {
		if entry == "" {
			continue
		}
		key, value, _ := strings.Cut(entry, "=")
		environment[key] = value
	}
	result := make([]string, 0, len(environment))
	for key, value := range environment {
		result = append(result, key+"="+value)
	}
	return result
}

func validObserverScript(defaultHead, head, merge, sealed string) string {
	return fmt.Sprintf(`#!/bin/bash
set -eu
args="$*"
if [[ "$args" == *"graphql"* ]]; then
cat <<'JSON'
[{"data":{"repository":{"id":"repo-node","nameWithOwner":"acme/demo","url":"https://github.com/acme/demo","defaultBranchRef":{"name":"main","target":{"oid":"%s"}},"pullRequest":{"id":"pr-node","url":"https://github.com/acme/demo/pull/42","state":"MERGED","mergedAt":"2026-08-13T07:25:38Z","baseRefName":"main","headRefName":"review","headRefOid":"%s","baseRepository":{"nameWithOwner":"acme/demo"},"mergeCommit":{"oid":"%s"},"body":"SENSITIVE_PR_BODY","commits":{"nodes":[{"commit":{"oid":"%s"}},{"commit":{"oid":"%s"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}]
JSON
elif [[ "$args" == *"%s...%s"* ]]; then
printf '{"status":"ahead","merge_base_commit":{"sha":"%s"}}\n'
elif [[ "$args" == *"%s...%s"* ]]; then
printf '{"status":"ahead","merge_base_commit":{"sha":"%s"}}\n'
else
exit 2
fi
`, defaultHead, head, merge, sealed, head, sealed, head, sealed, merge, defaultHead, merge)
}

func TestClientObserveRoundTrip(t *testing.T) {
	buildObserver(t)
	binDir := fakeGHPath(t, validObserverScript(testDefault, testHead, testMerge, testSealed))
	client := NewClient([]string{observerBinary}, observerSHA, testEnvironment("PATH="+binDir+":"+os.Getenv("PATH")))
	client.timeout = 10 * time.Second
	observation, err := client.Observe(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if observation.PullRequest.Number != 42 || observation.PullRequest.HeadRefOID != testHead || observation.MergeCompare.MergeBaseSHA != testMerge || len(observation.Commits) != 2 {
		t.Fatalf("observation = %+v", observation)
	}
	if strings.Contains(fmt.Sprintf("%+v", observation), "SENSITIVE_PR_BODY") {
		t.Fatal("observation carried private PR body")
	}
}

func TestClientRedactsProviderSecrets(t *testing.T) {
	buildObserver(t)
	secret := "super-secret-gh-token"
	script := fmt.Sprintf("#!/bin/bash\necho \"gh: auth failed with %s\" >&2\nexit 1\n", secret)
	binDir := fakeGHPath(t, script)
	client := NewClient([]string{observerBinary}, observerSHA, testEnvironment("PATH="+binDir+":"+os.Getenv("PATH")+"\nGH_TOKEN="+secret))
	client.timeout = 10 * time.Second
	_, err := client.Observe(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected provider failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error did not redact the secret: %v", err)
	}
}

func TestClientDeadlineIsFailClosed(t *testing.T) {
	buildObserver(t)
	binDir := fakeGHPath(t, "#!/bin/bash\nsleep 5\nexit 0\n")
	client := NewClient([]string{observerBinary}, observerSHA, testEnvironment("PATH="+binDir+":"+os.Getenv("PATH")))
	client.timeout = 300 * time.Millisecond
	_, err := client.Observe(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected timeout")
	}
	hostTimedOut := false
	var hostError *extensionhost.HostError
	if errors.As(err, &hostError) && hostError.Kind == extensionhost.ErrorDeadlineExceeded {
		hostTimedOut = true
	}
	remoteTimedOut := false
	var remote *extensionhost.RemoteError
	if errors.As(err, &remote) && remote.Failure.Class == "unavailable" && remote.Failure.Code == "provider_timeout" {
		remoteTimedOut = true
	}
	if !hostTimedOut && !remoteTimedOut {
		t.Fatalf("error = %v", err)
	}
}

func TestClientRefusesTamperedExecutable(t *testing.T) {
	buildObserver(t)
	body, err := os.ReadFile(observerBinary)
	if err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "tampered-observer")
	if err := os.WriteFile(tampered, append(body, 0), 0o700); err != nil {
		t.Fatal(err)
	}
	client := NewClient([]string{tampered}, observerSHA, testEnvironment(""))
	if _, err := client.Observe(context.Background(), testRequest()); err == nil {
		t.Fatal("tampered executable accepted")
	}
}

func TestClientRefusesLyingProcess(t *testing.T) {
	buildObserver(t)
	lying := filepath.Join(t.TempDir(), "lying-observer")
	script := fmt.Sprintf("#!/bin/bash\nif [[ \"${1:-}\" == describe ]]; then\nexec %q describe\nfi\nexit 0\n", observerBinary)
	if err := os.WriteFile(lying, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(lying)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	client := NewClient([]string{lying}, hex.EncodeToString(sum[:]), testEnvironment(""))
	client.timeout = 2 * time.Second
	_, err = client.Observe(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected protocol failure")
	}
	var hostError *extensionhost.HostError
	if !errors.As(err, &hostError) {
		t.Fatalf("error = %v", err)
	}
}
