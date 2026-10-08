package control

import (
	"testing"

	"shephrd/internal/terminal"
)

type herdrClientProvider struct {
	terminal.Client
}

func (p herdrClientProvider) Detect(context terminal.ParentContext) terminal.Detection {
	return p.Client.DetectParent(context)
}

func (p herdrClientProvider) OwnedCleanup() bool { return false }

type cmuxClientProvider struct {
	terminal.CmuxClient
}

func (p cmuxClientProvider) Detect(context terminal.ParentContext) terminal.Detection {
	return p.CmuxClient.DetectParent(context)
}

func (p cmuxClientProvider) OwnedCleanup() bool { return true }

func herdrFixtureRunner(t *testing.T, service Service) *lifecycleFixtureRunner {
	t.Helper()
	if len(service.TerminalProviders) != 1 {
		t.Fatalf("expected exactly one terminal provider, got %d", len(service.TerminalProviders))
	}
	provider, ok := service.TerminalProviders[0].(herdrClientProvider)
	if !ok {
		t.Fatal("terminal provider is not a herdr client provider")
	}
	runner, ok := provider.Client.Runner.(*lifecycleFixtureRunner)
	if !ok {
		t.Fatal("herdr client runner is not the lifecycle fixture")
	}
	return runner
}
