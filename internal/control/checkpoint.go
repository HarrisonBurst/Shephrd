package control

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"shephrd/internal/model"
)

func workspaceFacts(path string) model.WorkspaceFacts {
	facts := model.WorkspaceFacts{}
	var failures []string
	head, headErr := gitFact(path, "rev-parse", "HEAD")
	if headErr != nil {
		failures = append(failures, "HEAD: "+headErr.Error())
	} else {
		facts.HeadCommit = strings.TrimSpace(head)
	}
	status, statusErr := gitFact(path, "status", "--porcelain", "--untracked-files=all")
	if statusErr != nil {
		failures = append(failures, "dirty state: "+statusErr.Error())
	} else {
		facts.Dirty = strings.TrimSpace(status) != ""
	}
	facts.Error = strings.Join(failures, "; ")
	return facts
}

func gitFact(path string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = path
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return "", fmt.Errorf("%s: %w", detail, err)
		}
		return "", err
	}
	return stdout.String(), nil
}
