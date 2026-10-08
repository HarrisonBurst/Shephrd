package model

import (
	"fmt"
	"path/filepath"
)

func RunReportPath(dataDir, taskID, attemptID string, generation int) (string, error) {
	return filepath.Abs(filepath.Join(dataDir, taskID, attemptID, fmt.Sprintf("run-%d", generation), "report.md"))
}
