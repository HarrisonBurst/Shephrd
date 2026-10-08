package terminal

import (
	"fmt"

	extensionhost "shephrd/internal/extension"
)

const (
	cmuxExtensionID       = "shephrd.terminal.cmux"
	cmuxCapability        = "terminal.cmux"
	cmuxCapabilityVersion = terminalCapabilityVersion
	cmuxExtensionVersion  = "1.0.0"
)

var cmuxCapabilityOperations = append([]string(nil), terminalCapabilityOperations...)

func cmuxExtensionSpec() terminalExtensionSpec {
	return terminalExtensionSpec{
		backend:            "cmux",
		extensionID:        cmuxExtensionID,
		extensionVersion:   cmuxExtensionVersion,
		capability:         cmuxCapability,
		contextKeys:        []string{"CMUX_SOCKET_PATH", "CMUX_WORKSPACE_ID", "CMUX_SURFACE_ID"},
		allowedEnvironment: []string{"PATH", "HOME", "TMPDIR", "CMUX_SOCKET_PASSWORD", "CMUX_SOCKET_CAPABILITY"},
		ownedCleanup:       true,
		validateEndpoint: func(endpoint Endpoint) error {
			return CmuxClient{}.ValidateEndpoint(endpoint)
		},
		validatePrepared: func(endpoint Endpoint, spec WorkspaceSpec) error {
			if endpoint.WindowID != spec.WindowID {
				return fmt.Errorf("extension returned a cross-parent cmux endpoint")
			}
			return nil
		},
	}
}

func CmuxExtensionCapability() extensionhost.Capability {
	return terminalExtensionCapability(cmuxCapability)
}

func CmuxExtensionManifest() extensionhost.Manifest {
	return terminalExtensionManifest(cmuxExtensionSpec())
}

type CmuxExtensionProvider struct {
	*terminalExtensionProvider
}

func NewCmuxExtensionProvider(command []string, sha256 string, environment []string) *CmuxExtensionProvider {
	return &CmuxExtensionProvider{terminalExtensionProvider: newTerminalExtensionProvider(cmuxExtensionSpec(), command, sha256, environment)}
}
