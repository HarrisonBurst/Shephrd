package terminal

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	DetectionAbsent  = "absent"
	DetectionMatched = "matched"
	DetectionInvalid = "invalid"
)

type ParentContext struct {
	Platform    string            `json:"platform"`
	Path        string            `json:"path"`
	Values      map[string]string `json:"values"`
	InvalidKeys map[string]bool   `json:"invalid_keys"`
}

func (c ParentContext) Value(key string) string {
	return c.Values[key]
}

type Diagnostics struct {
	ProviderVersion string   `json:"provider_version"`
	ProtocolVersion string   `json:"protocol_version"`
	Capabilities    []string `json:"capabilities"`
}

type Detection struct {
	Provider    string
	State       string
	Parent      Parent
	Diagnostics Diagnostics
	Err         error
}

type Provider interface {
	RuntimeClient
	ContextKeys() []string
	Detect(ParentContext) Detection
	OwnedCleanup() bool
	ValidateEndpoint(Endpoint) error
}

type Selection struct {
	Backend     string
	Provider    Provider
	Parent      Parent
	Diagnostics Diagnostics
	Detections  []Detection
}

type SelectionError struct {
	Kind       string
	Requested  string
	Candidates []string
	Cause      error
}

func (e *SelectionError) Error() string {
	switch e.Kind {
	case "terminal_provider_ambiguous":
		return fmt.Sprintf("terminal provider auto-selection is ambiguous across %s; select one explicitly", strings.Join(e.Candidates, ", "))
	case "terminal_provider_unavailable":
		return fmt.Sprintf("terminal provider %q is not available in the sanitized parent context", e.Requested)
	case "terminal_provider_invalid":
		if e.Cause != nil {
			return fmt.Sprintf("terminal provider %q failed validation: %v", e.Requested, e.Cause)
		}
		return fmt.Sprintf("terminal provider %q failed validation", e.Requested)
	case "terminal_extension_not_configured":
		return fmt.Sprintf("terminal runtime %q requires an explicitly configured first-party extension; headless remains the built-in default", e.Requested)
	default:
		return fmt.Sprintf("terminal provider %q is unsupported", e.Requested)
	}
}

func (e *SelectionError) Unwrap() error {
	return e.Cause
}

func (e *SelectionError) ErrorKind() string {
	return e.Kind
}

func KnownTerminalBackend(backend string) bool {
	return backend == "herdr" || backend == "cmux"
}

var contextKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// SanitizeParentContext bounds the parent environment to the exact context
// keys requested by the configured providers.
func SanitizeParentContext(platform, path string, environment []string, contextKeys ...[]string) ParentContext {
	allowed := make(map[string]bool)
	for _, keys := range contextKeys {
		for _, key := range keys {
			if contextKeyPattern.MatchString(key) {
				allowed[key] = true
			}
		}
	}
	context := ParentContext{Platform: boundedContextValue(platform, 32), Path: boundedContextValue(path, 8192), Values: make(map[string]string), InvalidKeys: make(map[string]bool)}
	total := 0
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || !allowed[key] {
			continue
		}
		previous := len(context.Values[key])
		if len(value) > 4096 || total-previous+len(value) > 16384 {
			context.InvalidKeys[key] = true
			continue
		}
		total -= previous
		context.Values[key] = value
		total += len(value)
	}
	return context
}

// SelectProviders resolves a requested terminal runtime against the
// explicitly configured providers. headless never probes a provider; an
// explicit provider must be configured and detected; auto falls back to
// headless unless exactly one configured provider is detected.
func SelectProviders(requested string, providers map[string]Provider, contextFor func(Provider) ParentContext) (Selection, error) {
	if requested == "headless" {
		return Selection{Backend: "headless"}, nil
	}
	if requested != "auto" {
		provider, exists := providers[requested]
		if !exists {
			if KnownTerminalBackend(requested) {
				return Selection{}, &SelectionError{Kind: "terminal_extension_not_configured", Requested: requested}
			}
			return Selection{}, &SelectionError{Kind: "terminal_provider_unsupported", Requested: requested}
		}
		detection := detectProvider(provider, contextFor(provider))
		switch detection.State {
		case DetectionMatched:
			return Selection{Backend: requested, Provider: provider, Parent: detection.Parent, Diagnostics: detection.Diagnostics, Detections: []Detection{detection}}, nil
		case DetectionAbsent:
			return Selection{}, &SelectionError{Kind: "terminal_provider_unavailable", Requested: requested}
		default:
			return Selection{}, &SelectionError{Kind: "terminal_provider_invalid", Requested: requested, Cause: detection.Err}
		}
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	detections := make([]Detection, 0, len(names))
	matches := make([]Detection, 0, len(names))
	invalid := make([]Detection, 0)
	for _, name := range names {
		detection := detectProvider(providers[name], contextFor(providers[name]))
		detections = append(detections, detection)
		switch detection.State {
		case DetectionMatched:
			matches = append(matches, detection)
		case DetectionInvalid:
			invalid = append(invalid, detection)
		}
	}
	if len(matches) > 1 {
		candidates := make([]string, 0, len(matches))
		for _, match := range matches {
			candidates = append(candidates, match.Provider)
		}
		return Selection{}, &SelectionError{Kind: "terminal_provider_ambiguous", Requested: "auto", Candidates: candidates}
	}
	if len(matches) == 1 {
		match := matches[0]
		return Selection{Backend: match.Provider, Provider: providers[match.Provider], Parent: match.Parent, Diagnostics: match.Diagnostics, Detections: detections}, nil
	}
	if len(invalid) > 0 {
		return Selection{}, &SelectionError{Kind: "terminal_provider_invalid", Requested: invalid[0].Provider, Cause: invalid[0].Err}
	}
	return Selection{Backend: "headless", Detections: detections}, nil
}

func detectProvider(provider Provider, context ParentContext) Detection {
	detection := provider.Detect(context)
	if detection.Provider != provider.Backend() {
		return Detection{Provider: provider.Backend(), State: DetectionInvalid, Err: fmt.Errorf("provider returned mismatched detection identity")}
	}
	switch detection.State {
	case DetectionAbsent, DetectionInvalid:
		return detection
	case DetectionMatched:
		if detection.Parent.Backend != provider.Backend() || detection.Parent.SocketPath == "" || detection.Parent.WorkspaceID == "" || detection.Parent.PaneID == "" {
			return Detection{Provider: provider.Backend(), State: DetectionInvalid, Err: fmt.Errorf("provider returned incomplete parent identity")}
		}
		if err := validateDiagnostics(detection.Diagnostics); err != nil {
			return Detection{Provider: provider.Backend(), State: DetectionInvalid, Err: err}
		}
		return detection
	default:
		return Detection{Provider: provider.Backend(), State: DetectionInvalid, Err: fmt.Errorf("provider returned invalid detection state")}
	}
}

func validateDiagnostics(diagnostics Diagnostics) error {
	for _, value := range []string{diagnostics.ProviderVersion, diagnostics.ProtocolVersion} {
		if len(value) > 128 || strings.IndexFunc(value, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0 {
			return fmt.Errorf("provider returned invalid version diagnostics")
		}
	}
	if len(diagnostics.Capabilities) > 64 {
		return fmt.Errorf("provider returned too many capability diagnostics")
	}
	seen := make(map[string]bool, len(diagnostics.Capabilities))
	for _, capability := range diagnostics.Capabilities {
		if capability == "" || len(capability) > 128 || seen[capability] || strings.IndexFunc(capability, func(char rune) bool {
			return !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune(":._-", char))
		}) >= 0 {
			return fmt.Errorf("provider returned invalid capability diagnostics")
		}
		seen[capability] = true
	}
	return nil
}

func boundedContextValue(value string, limit int) string {
	if len(value) > limit {
		return ""
	}
	return value
}
