package proc

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func startTime(pid int) (string, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", errNoProcess
	}
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return "", fmt.Errorf("unreadable /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(body)[end+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("unreadable /proc/%d/stat", pid)
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", errNoProcess
	}
	return fields[19], nil
}
