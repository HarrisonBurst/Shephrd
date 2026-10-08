package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	extensionhost "shephrd/internal/extension"
)

const testRequestID = "extension_request_000000000000000000000000"

func TestClientRejectsMissingMalformedAndUnboundedResults(t *testing.T) {
	root := canonicalTempDir(t)
	for _, test := range []struct {
		name   string
		result string
		want   string
	}{
		{name: "missing result", result: `{}`, want: "candidate count"},
		{name: "malformed result", result: `{"candidates":[],"authority":"register"}`, want: "invalid JSON"},
		{name: "unbounded result", result: candidateResult(repeatedCandidates(MaxCandidates + 1)), want: "candidate count"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := faultClient(t, root, test.result, "")
			_, err := client.Discover(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	client := NewClient([]string{filepath.Join(t.TempDir(), "missing-scanner")}, strings.Repeat("0", 64), []string{root})
	if _, err := client.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing extension error = %v", err)
	}
}

func TestClientRejectsDishonestDuplicateEscapingAndNonGitCandidates(t *testing.T) {
	root := canonicalTempDir(t)
	repository := filepath.Join(root, "repository")
	initGitRepository(t, repository)
	outside := filepath.Join(canonicalTempDir(t), "outside")
	initGitRepository(t, outside)
	nonGit := filepath.Join(root, "plain")
	if err := os.Mkdir(nonGit, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "dishonest")
	if err := os.Symlink(repository, symlink); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		candidates []Candidate
		want       string
	}{
		{name: "dishonest non-canonical path", candidates: []Candidate{{Path: symlink}}, want: "non-canonical"},
		{name: "duplicate", candidates: []Candidate{{Path: repository}, {Path: repository}}, want: "duplicate"},
		{name: "escaping", candidates: []Candidate{{Path: outside}}, want: "escapes"},
		{name: "non-Git", candidates: []Candidate{{Path: nonGit}}, want: "not a Git repository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := faultClient(t, root, candidateResult(test.candidates), "")
			_, err := client.Discover(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestClientRejectsCandidateIdentityChange(t *testing.T) {
	root := canonicalTempDir(t)
	repository := filepath.Join(root, "repository")
	initGitRepository(t, repository)
	client := &Client{gitRoot: func(_ context.Context, path string) (string, error) {
		moved := path + "-moved"
		if err := os.Rename(path, moved); err != nil {
			return "", err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return "", err
		}
		return path, nil
	}}
	err := client.validateCandidate(context.Background(), []string{root}, Candidate{Path: repository})
	if err == nil || !strings.Contains(err.Error(), "changed identity") {
		t.Fatalf("error = %v", err)
	}
}

func faultClient(t *testing.T, root, result, body string) *Client {
	t.Helper()
	if body == "" {
		body = "printf '%s\\n' '" + response(result) + "'"
	}
	script := filepath.Join(t.TempDir(), "scanner")
	source := `#!/bin/sh
set -eu
case "$1" in
  describe) printf '%s\n' '{"wire":{"major":1,"minor_min":0,"minor_max":0},"extension":{"id":"shephrd.repository-scanner","version":"1.0.0"},"capabilities":[{"name":"repository.discovery","version":1,"operations":["discover"]}]}' ;;
  invoke) IFS= read -r request; ` + body + ` ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(script, []byte(source), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(source))
	host := extensionhost.NewHost(extensionhost.HostConfig{
		Command: []string{script}, SHA256: hex.EncodeToString(digest[:]), ExpectedID: ExtensionID,
		ExpectedCapabilities: []extensionhost.Capability{Capability()}, RequestID: func() (string, error) { return testRequestID, nil },
	})
	return &Client{host: host, roots: []string{root}, timeout: 5 * time.Second, gitRoot: resolveGitRoot}
}

func response(result string) string {
	if result == "" {
		result = `{"candidates":[]}`
	}
	return `{"wire":{"major":1,"minor":0},"request_id":"` + testRequestID + `","capability":"repository.discovery","capability_version":1,"operation":"discover","status":"ok","result":` + result + `,"extension":{"id":"shephrd.repository-scanner","version":"1.0.0"}}`
}

func candidateResult(candidates []Candidate) string {
	body, err := json.Marshal(Result{Candidates: candidates})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func repeatedCandidates(count int) []Candidate {
	result := make([]Candidate, count)
	for index := range result {
		result[index] = Candidate{Path: fmt.Sprintf("/candidate/%04d", index)}
	}
	return result
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func initGitRepository(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "-q", "-b", "main", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", output, err)
	}
}

func TestClientTimeoutClassifiesHostError(t *testing.T) {
	root := canonicalTempDir(t)
	client := faultClient(t, root, "", `/bin/sleep 60`)
	client.timeout = 100 * time.Millisecond
	_, err := client.Discover(context.Background())
	var hostErr *extensionhost.HostError
	if !errors.As(err, &hostErr) || hostErr.Kind != extensionhost.ErrorDeadlineExceeded {
		t.Fatalf("error = %v", err)
	}
}
