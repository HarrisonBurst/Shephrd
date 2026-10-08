package discovery

import (
	"fmt"
	"runtime/debug"

	extensionhost "shephrd/internal/extension"
)

const (
	ExtensionID       = "shephrd.repository-scanner"
	ExtensionVersion  = "1.0.0"
	CapabilityName    = "repository.discovery"
	CapabilityVersion = 1
	DiscoverOperation = "discover"
	MaxRoots          = 16
	MaxCandidates     = 1024
	MaxPathBytes      = 4096
)

type Candidate struct {
	Path string `json:"path"`
}

type Request struct {
	Roots []string `json:"roots"`
}

type Result struct {
	Candidates []Candidate `json:"candidates"`
}

func Capability() extensionhost.Capability {
	return extensionhost.Capability{Name: CapabilityName, Version: CapabilityVersion, Operations: []string{DiscoverOperation}}
}

func Manifest() extensionhost.Manifest {
	return extensionhost.Manifest{
		Wire:         extensionhost.WireRange{Major: extensionhost.WireMajor, MinorMin: extensionhost.WireMinor, MinorMax: extensionhost.WireMinor},
		Extension:    extensionhost.Identity{ID: ExtensionID, Version: ExtensionVersion, BuildCommit: buildCommit()},
		Capabilities: []extensionhost.Capability{Capability()},
	}
}

func ValidateRequest(request Request) error {
	if len(request.Roots) == 0 || len(request.Roots) > MaxRoots {
		return fmt.Errorf("repository discovery roots are invalid")
	}
	seen := make(map[string]bool, len(request.Roots))
	for _, root := range request.Roots {
		if !validPath(root) || seen[root] {
			return fmt.Errorf("repository discovery roots are invalid")
		}
		seen[root] = true
	}
	return nil
}

func ValidCandidate(candidate Candidate) bool {
	return validPath(candidate.Path)
}

func validPath(path string) bool {
	return path != "" && len(path) <= MaxPathBytes
}

func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) <= 128 {
			return setting.Value
		}
	}
	return ""
}
