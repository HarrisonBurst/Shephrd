package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func startTime(pid int) (string, error) {
	state, _, start, err := linuxStat(pid)
	if err != nil {
		return "", err
	}
	if state == "Z" || state == "X" {
		return "", errNoProcess
	}
	return start, nil
}

func linuxStat(pid int) (string, int, string, error) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, "", errNoProcess
	}
	if err != nil {
		return "", 0, "", err
	}
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return "", 0, "", fmt.Errorf("unreadable /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(body)[end+1:])
	if len(fields) < 20 {
		return "", 0, "", fmt.Errorf("unreadable /proc/%d/stat", pid)
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, "", fmt.Errorf("unreadable /proc/%d/stat: %w", pid, err)
	}
	return fields[0], group, fields[19], nil
}
