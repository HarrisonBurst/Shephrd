package delivery

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestReportRecoveryHashFailsClosedOnByteAndPathRaces(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(string)
	}{
		{name: "bytes", change: func(path string) {
			if err := os.WriteFile(path, []byte("changed bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "replacement", change: func(path string) {
			replacement := path + ".replacement"
			if err := os.WriteFile(replacement, []byte("initial bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.md")
			if err := os.WriteFile(path, []byte("initial bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := hashStableReport(path, nil, func() { test.change(path) }); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("race error = %v", err)
			}
		})
	}
}

func TestReportRecoveryRejectsSymlinkAndDefersInputLimits(t *testing.T) {
	dataDir := t.TempDir()
	verifier := New(dataDir)
	taskID := "task_report"
	path, err := verifier.CanonicalReportPath(taskID, model.Attempt{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dataDir, "target.md")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.ValidateReportRecovery(taskID, model.Attempt{}); err == nil || !strings.Contains(err.Error(), "pinned regular") {
		t.Fatalf("symlink error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0xff}, int(MaxReportInputBytes+1))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := verifier.ValidateReportRecovery(taskID, model.Attempt{})
	if err != nil || evidence.SizeBytes != int64(len(body)) {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}
