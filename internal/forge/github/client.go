package github

import (
	"context"
	"fmt"
	"time"

	extensionhost "shephrd/internal/extension"
)

var observerEnvironment = []string{"PATH", "HOME", "TMPDIR", "USER", "LOGNAME", "TERM", "LANG", "LC_ALL", "SSH_AUTH_SOCK", "GH_TOKEN", "GITHUB_TOKEN"}

type Client struct {
	host    *extensionhost.Host
	timeout time.Duration
}

func NewClient(command []string, sha256 string, environment []string) *Client {
	return &Client{
		host: extensionhost.NewHost(extensionhost.HostConfig{
			Command:              command,
			SHA256:               sha256,
			ExpectedID:           ExtensionID,
			ExpectedCapabilities: []extensionhost.Capability{Capability()},
			ParentEnvironment:    environment,
			AllowedEnvironment:   observerEnvironment,
		}),
		timeout: 30 * time.Second,
	}
}

func (c *Client) Observe(ctx context.Context, request Request) (Observation, error) {
	if c == nil || c.host == nil {
		return Observation{}, fmt.Errorf("GitHub observation capability is not configured")
	}
	if err := ValidateRequest(request); err != nil {
		return Observation{}, err
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var result Result
	if err := c.host.Invoke(ctx, CapabilityName, CapabilityVersion, ObserveOperation, request, &result); err != nil {
		return Observation{}, err
	}
	if err := ValidateObservation(result.Observation); err != nil {
		return Observation{}, fmt.Errorf("GitHub observation is invalid: %w", err)
	}
	return result.Observation, nil
}
