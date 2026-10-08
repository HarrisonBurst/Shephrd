package terminal

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type providerDouble struct {
	name      string
	key       string
	match     string
	invalid   error
	detectLog *[]ParentContext
}

func (p providerDouble) Backend() string                 { return p.name }
func (p providerDouble) WithSocket(string) RuntimeClient { return p }
func (p providerDouble) Validate() error                 { return nil }
func (p providerDouble) Diagnostics() (Diagnostics, error) {
	return Diagnostics{ProtocolVersion: "1", Capabilities: []string{"test"}}, nil
}
func (p providerDouble) CreateWorkspace(WorkspaceSpec) (Endpoint, error)               { return Endpoint{}, nil }
func (p providerDouble) Start(Endpoint, string) error                                  { return nil }
func (p providerDouble) Inspect(Endpoint) (EndpointState, error)                       { return EndpointState{}, nil }
func (p providerDouble) ProcessInfo(Endpoint) (ProcessInfo, error)                     { return ProcessInfo{}, nil }
func (p providerDouble) Read(Endpoint, int) (string, error)                            { return "", nil }
func (p providerDouble) Focus(Endpoint) error                                          { return nil }
func (p providerDouble) Close(Endpoint) error                                          { return nil }
func (p providerDouble) ReportAgent(Endpoint, AgentReport) error                       { return nil }
func (p providerDouble) ReleaseAgent(Endpoint, string, string, uint64) error           { return nil }
func (p providerDouble) ReportMetadata(Endpoint, string, string, string, string) error { return nil }
func (p providerDouble) ContextKeys() []string                                         { return []string{p.key} }
func (p providerDouble) Detect(context ParentContext) Detection {
	if p.detectLog != nil {
		*p.detectLog = append(*p.detectLog, context)
	}
	if context.Value(p.key) == "" {
		return Detection{Provider: p.name, State: DetectionAbsent}
	}
	if p.invalid != nil {
		return Detection{Provider: p.name, State: DetectionInvalid, Err: p.invalid}
	}
	if context.Value(p.key) == p.match {
		return Detection{Provider: p.name, State: DetectionMatched, Parent: Parent{Backend: p.name, SocketPath: "/" + p.name, WorkspaceID: "workspace", TabID: "tab", PaneID: "pane"}, Diagnostics: Diagnostics{ProtocolVersion: "1", Capabilities: []string{"test"}}}
	}
	return Detection{Provider: p.name, State: DetectionInvalid, Err: errors.New("invalid marker")}
}
func (p providerDouble) OwnedCleanup() bool              { return false }
func (p providerDouble) ValidateEndpoint(Endpoint) error { return nil }

func selectProviderTestContext(providers map[string]Provider) func(Provider) ParentContext {
	return func(provider Provider) ParentContext {
		return SanitizeParentContext("darwin", "/bin", []string{
			"FIRST_PARENT=1", "SECOND_PARENT=1", "SECRET_TOKEN=secret", "UNSAFE_PARENT=x",
		}, provider.ContextKeys())
	}
}

func TestSelectProvidersPrecedenceFallbackAndAmbiguity(t *testing.T) {
	providers := map[string]Provider{
		"first":  providerDouble{name: "first", key: "FIRST_PARENT", match: "1"},
		"second": providerDouble{name: "second", key: "SECOND_PARENT", match: "1"},
	}
	t.Run("explicit headless skips detection", func(t *testing.T) {
		selection, err := SelectProviders("headless", providers, selectProviderTestContext(providers))
		if err != nil || selection.Backend != "headless" {
			t.Fatalf("selection = %+v, err = %v", selection, err)
		}
	})
	t.Run("explicit provider wins", func(t *testing.T) {
		selection, err := SelectProviders("second", providers, selectProviderTestContext(providers))
		if err != nil || selection.Backend != "second" {
			t.Fatalf("selection = %+v, err = %v", selection, err)
		}
	})
	t.Run("auto all matches is typed ambiguity", func(t *testing.T) {
		_, err := SelectProviders("auto", providers, selectProviderTestContext(providers))
		var selectionErr *SelectionError
		if !errors.As(err, &selectionErr) || selectionErr.ErrorKind() != "terminal_provider_ambiguous" || !reflect.DeepEqual(selectionErr.Candidates, []string{"first", "second"}) {
			t.Fatalf("error = %#v", selectionErr)
		}
	})
	t.Run("auto one match", func(t *testing.T) {
		single := map[string]Provider{"first": providers["first"]}
		selection, err := SelectProviders("auto", single, selectProviderTestContext(single))
		if err != nil || selection.Backend != "first" {
			t.Fatalf("selection = %+v, err = %v", selection, err)
		}
	})
	t.Run("auto zero matches falls back headless", func(t *testing.T) {
		selection, err := SelectProviders("auto", providers, func(Provider) ParentContext { return ParentContext{Values: map[string]string{}, InvalidKeys: map[string]bool{}} })
		if err != nil || selection.Backend != "headless" {
			t.Fatalf("selection = %+v, err = %v", selection, err)
		}
	})
	t.Run("explicit invalid context is typed invalid", func(t *testing.T) {
		invalid := map[string]Provider{"first": providerDouble{name: "first", key: "FIRST_PARENT", invalid: errors.New("marker is invalid")}}
		_, err := SelectProviders("first", invalid, func(Provider) ParentContext {
			return ParentContext{Values: map[string]string{"FIRST_PARENT": "wrong"}, InvalidKeys: map[string]bool{}}
		})
		var selectionErr *SelectionError
		if !errors.As(err, &selectionErr) || selectionErr.ErrorKind() != "terminal_provider_invalid" {
			t.Fatalf("error = %#v", selectionErr)
		}
	})
}

func TestSelectProvidersRequiresConfiguredFirstPartyExtensions(t *testing.T) {
	t.Run("unconfigured herdr is a typed refusal", func(t *testing.T) {
		_, err := SelectProviders("herdr", map[string]Provider{}, func(Provider) ParentContext { return ParentContext{} })
		var selectionErr *SelectionError
		if !errors.As(err, &selectionErr) || selectionErr.ErrorKind() != "terminal_extension_not_configured" {
			t.Fatalf("error = %#v", selectionErr)
		}
		if !strings.Contains(err.Error(), "requires an explicitly configured first-party extension") {
			t.Fatalf("error text = %v", err)
		}
	})
	t.Run("unconfigured cmux is a typed refusal", func(t *testing.T) {
		_, err := SelectProviders("cmux", map[string]Provider{}, func(Provider) ParentContext { return ParentContext{} })
		var selectionErr *SelectionError
		if !errors.As(err, &selectionErr) || selectionErr.ErrorKind() != "terminal_extension_not_configured" {
			t.Fatalf("error = %#v", selectionErr)
		}
	})
	t.Run("unknown backend stays unsupported", func(t *testing.T) {
		_, err := SelectProviders("fixture", map[string]Provider{}, func(Provider) ParentContext { return ParentContext{} })
		var selectionErr *SelectionError
		if !errors.As(err, &selectionErr) || selectionErr.ErrorKind() != "terminal_provider_unsupported" {
			t.Fatalf("error = %#v", selectionErr)
		}
	})
	t.Run("auto without configured providers stays headless", func(t *testing.T) {
		selection, err := SelectProviders("auto", map[string]Provider{}, func(Provider) ParentContext { return ParentContext{} })
		if err != nil || selection.Backend != "headless" {
			t.Fatalf("selection = %+v, err = %v", selection, err)
		}
	})
}

func TestSanitizedParentContextIsBoundedAndAllowlisted(t *testing.T) {
	var detections []ParentContext
	provider := providerDouble{name: "only", key: "SAFE_PARENT", match: "1", detectLog: &detections}
	selection, err := SelectProviders("only", map[string]Provider{"only": provider}, func(provider Provider) ParentContext {
		return SanitizeParentContext("darwin", "/bin", []string{"SAFE_PARENT=1", "SECRET_TOKEN=secret", "CMUX_SOCKET_CAPABILITY=credential"}, provider.ContextKeys())
	})
	if err != nil || selection.Backend != "only" {
		t.Fatalf("selection = %+v, err = %v", selection, err)
	}
	if len(detections) != 1 || detections[0].Value("SECRET_TOKEN") != "" || detections[0].Value("CMUX_SOCKET_CAPABILITY") != "" || detections[0].Value("SAFE_PARENT") != "1" {
		t.Fatalf("context = %+v", detections)
	}
	oversized := SanitizeParentContext("darwin", "/bin", []string{"SAFE_PARENT=" + string(make([]byte, 4097))}, []string{"SAFE_PARENT"})
	if !oversized.InvalidKeys["SAFE_PARENT"] || oversized.Value("SAFE_PARENT") != "" {
		t.Fatalf("oversized context = %+v", oversized)
	}
	if SanitizeParentContext("darwin", "/bin", []string{"SAFE_PARENT=1"}, []string{"UNSAFE_PARENT"}).Value("SAFE_PARENT") != "" {
		t.Fatal("context key outside the allowlist was forwarded")
	}
}
