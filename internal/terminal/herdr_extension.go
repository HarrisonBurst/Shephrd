package terminal

import (
	"fmt"

	extensionhost "shephrd/internal/extension"
)

const (
	herdrExtensionID      = "shephrd.terminal.herdr"
	herdrCapability       = "terminal.herdr"
	herdrExtensionVersion = "1.0.0"
)

func herdrExtensionSpec() terminalExtensionSpec {
	return terminalExtensionSpec{
		backend:            "herdr",
		extensionID:        herdrExtensionID,
		extensionVersion:   herdrExtensionVersion,
		capability:         herdrCapability,
		contextKeys:        []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_WORKSPACE_ID", "HERDR_PANE_ID"},
		allowedEnvironment: []string{"PATH", "HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME"},
		validateEndpoint: func(endpoint Endpoint) error {
			return Client{}.ValidateEndpoint(endpoint)
		},
		validatePrepared: func(endpoint Endpoint, spec WorkspaceSpec) error {
			if endpoint.WorkspaceID != spec.WorkspaceID {
				return fmt.Errorf("extension returned a cross-parent Herdr endpoint")
			}
			return nil
		},
	}
}

func HerdrExtensionCapability() extensionhost.Capability {
	return terminalExtensionCapability(herdrCapability)
}

func HerdrExtensionManifest() extensionhost.Manifest {
	return terminalExtensionManifest(herdrExtensionSpec())
}

type HerdrExtensionProvider struct {
	*terminalExtensionProvider
}

func NewHerdrExtensionProvider(command []string, sha256 string, environment []string) *HerdrExtensionProvider {
	return &HerdrExtensionProvider{terminalExtensionProvider: newTerminalExtensionProvider(herdrExtensionSpec(), command, sha256, environment)}
}
