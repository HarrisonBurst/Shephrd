package control

import (
	"regexp"
	"strings"
	"unicode"

	"shephrd/internal/model"
)

var ansiSequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func WorkerLabel(repoName, title, feature, _ string) string {
	return labelPart(repoName, 28) + "-" + labelPart(title, 48) + "-" + labelPart(feature, 36)
}

func WorkerSessionName(repoName, title, feature, taskID string) string {
	shortID := taskID
	if index := strings.LastIndex(shortID, "_"); index >= 0 {
		shortID = shortID[index+1:]
	}
	return "Shephrd: " + labelPart(repoName, 28) + "/" + labelPart(title, 48) + " [" + labelPart(feature, 36) + ":" + labelPart(shortID, 20) + "]"
}

func labelPart(value string, limit int) string {
	value = ansiSequence.ReplaceAllString(value, "")
	var out strings.Builder
	lastDash := false
	for _, char := range value {
		if out.Len() >= limit {
			break
		}
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '.' || char == '_' || char == '-' {
			out.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(out.String(), "-._")
	if result == "" {
		return "worker"
	}
	return result
}

func workerLabelFor(task model.Task) string {
	return WorkerLabel(task.RepoName, task.Title, task.FeatureKey, task.ID)
}

func workerSessionNameFor(task model.Task) string {
	return WorkerSessionName(task.RepoName, task.Title, task.FeatureKey, task.ID)
}

func SubdriverLabel(repoName string) string {
	name := strings.Join(strings.Fields(strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, ansiSequence.ReplaceAllString(repoName, ""))), " ")
	if name == "" {
		name = "General"
	}
	return "Sub-driver: " + name
}
