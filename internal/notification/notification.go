package notification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	extensionhost "shephrd/internal/extension"
)

const (
	ExtensionID               = "shephrd.notification.macos"
	ExtensionVersion          = "1.0.0"
	CapabilityName            = "notification.presentation"
	CapabilityVersion         = 1
	PresentationOperation     = "present"
	MaxPresentationTitleBytes = 64
	MaxPresentationBodyBytes  = 256
)

type Presenter interface {
	Name() string
	Present(context.Context, Presentation) (PresentationResult, error)
}

type Presentation struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type PresentationResult struct {
	Presented bool `json:"presented"`
}

type Notice struct {
	ID                 string
	TaskID             string
	TaskLabel          string
	Kind               string
	ShortSummary       string
	CreatedAt          time.Time
	PayloadFingerprint string
}

type SuppressedError struct {
	Reason string
}

func (e *SuppressedError) Error() string {
	return "notification suppressed: " + e.Reason
}

func IsSuppressed(err error) bool {
	var suppressed *SuppressedError
	return errors.As(err, &suppressed)
}

type Options struct {
	Details         bool
	TaskPerMinute   int
	GlobalPerMinute int
	Timeout         time.Duration
	Now             func() time.Time
	Presenter       Presenter
}

type Notifier struct {
	details         bool
	taskPerMinute   int
	globalPerMinute int
	timeout         time.Duration
	now             func() time.Time
	presenter       Presenter
	mu              sync.Mutex
	dedupe          map[string]time.Time
	tasks           map[string][]time.Time
	global          []time.Time
}

func New(options Options) *Notifier {
	if options.TaskPerMinute == 0 {
		options.TaskPerMinute = 2
	}
	if options.GlobalPerMinute == 0 {
		options.GlobalPerMinute = 10
	}
	if options.Timeout <= 0 {
		options.Timeout = 750 * time.Millisecond
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Notifier{
		details: options.Details, taskPerMinute: options.TaskPerMinute, globalPerMinute: options.GlobalPerMinute,
		timeout: options.Timeout, now: options.Now, presenter: options.Presenter,
		dedupe: make(map[string]time.Time), tasks: make(map[string][]time.Time),
	}
}

func (n *Notifier) Name() string {
	return n.presenter.Name()
}

func (n *Notifier) Notify(ctx context.Context, notice Notice) (PresentationResult, error) {
	now := n.now().UTC()
	fingerprint := notice.PayloadFingerprint
	if fingerprint == "" {
		fingerprint = Fingerprint(notice.ShortSummary)
	}
	key := notice.TaskID + "\x00" + notice.Kind + "\x00" + fingerprint
	n.mu.Lock()
	if last, ok := n.dedupe[key]; ok && now.Sub(last) < 30*time.Second {
		n.mu.Unlock()
		return PresentationResult{}, &SuppressedError{Reason: "duplicate within 30s"}
	}
	n.global = recent(n.global, now, time.Minute)
	for key, last := range n.dedupe {
		if now.Sub(last) >= 30*time.Second {
			delete(n.dedupe, key)
		}
	}
	n.tasks[notice.TaskID] = recent(n.tasks[notice.TaskID], now, time.Minute)
	if (n.taskPerMinute > 0 && len(n.tasks[notice.TaskID]) >= n.taskPerMinute) || (n.globalPerMinute > 0 && len(n.global) >= n.globalPerMinute) {
		n.mu.Unlock()
		return PresentationResult{}, &SuppressedError{Reason: "rate limit"}
	}
	n.dedupe[key] = now
	n.tasks[notice.TaskID] = append(n.tasks[notice.TaskID], now)
	n.global = append(n.global, now)
	n.mu.Unlock()

	presentation := Presentation{Title: "Shephrd", Body: formatBody(notice, n.details)}
	presentContext, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	return n.presenter.Present(presentContext, presentation)
}

type ExtensionPresenter struct {
	host *extensionhost.Host
}

func NewExtensionPresenter(command []string, sha256 string, environment []string) *ExtensionPresenter {
	return &ExtensionPresenter{host: extensionhost.NewHost(extensionhost.HostConfig{
		Command: command, SHA256: sha256, ParentEnvironment: environment,
		AllowedEnvironment: []string{"HOME", "TMPDIR", "USER", "LOGNAME"},
		ExpectedID:         ExtensionID, ExpectedCapabilities: []extensionhost.Capability{Capability()},
	})}
}

func (p *ExtensionPresenter) Name() string {
	return ExtensionID
}

func (p *ExtensionPresenter) Present(ctx context.Context, presentation Presentation) (PresentationResult, error) {
	if err := ValidatePresentation(presentation); err != nil {
		return PresentationResult{}, err
	}
	var result PresentationResult
	if err := p.host.Invoke(ctx, CapabilityName, CapabilityVersion, PresentationOperation, presentation, &result); err != nil {
		return PresentationResult{}, err
	}
	if !result.Presented {
		return PresentationResult{}, &extensionhost.HostError{Kind: extensionhost.ErrorProtocolViolation, Operation: PresentationOperation, Cause: fmt.Errorf("extension did not confirm presentation")}
	}
	return result, nil
}

func Capability() extensionhost.Capability {
	return extensionhost.Capability{Name: CapabilityName, Version: CapabilityVersion, Operations: []string{PresentationOperation}}
}

func Manifest() extensionhost.Manifest {
	return extensionhost.Manifest{
		Wire:         extensionhost.WireRange{Major: extensionhost.WireMajor, MinorMin: extensionhost.WireMinor, MinorMax: extensionhost.WireMinor},
		Extension:    extensionhost.Identity{ID: ExtensionID, Version: ExtensionVersion, BuildCommit: buildCommit()},
		Capabilities: []extensionhost.Capability{Capability()},
	}
}

func ValidatePresentation(presentation Presentation) error {
	if presentation.Title == "" || len([]byte(presentation.Title)) > MaxPresentationTitleBytes || !utf8.ValidString(presentation.Title) || sanitize(presentation.Title) != presentation.Title {
		return fmt.Errorf("notification title is invalid")
	}
	if presentation.Body == "" || len([]byte(presentation.Body)) > MaxPresentationBodyBytes || !utf8.ValidString(presentation.Body) || sanitize(presentation.Body) != presentation.Body {
		return fmt.Errorf("notification body is invalid")
	}
	return nil
}

func Fingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func formatBody(notice Notice, details bool) string {
	label := sanitize(notice.TaskLabel)
	if label == "" {
		label = "task"
	}
	kind := sanitize(notice.Kind)
	if kind == "" {
		kind = "event"
	}
	body := fmt.Sprintf("%s: %s needs driver attention", label, kind)
	if details {
		summary := truncateUTF8(sanitize(notice.ShortSummary), 160)
		if summary != "" {
			body += " - " + summary
		}
	}
	return truncateUTF8(body, MaxPresentationBodyBytes)
}

func sanitize(value string) string {
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func truncateUTF8(value string, limit int) string {
	if len([]byte(value)) <= limit {
		return value
	}
	value = string([]byte(value)[:limit])
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

func recent(values []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	first := 0
	for first < len(values) && values[first].Before(cutoff) {
		first++
	}
	return values[first:]
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
