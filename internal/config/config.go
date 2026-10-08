package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
)

type Config struct {
	DefaultHarness      string                     `toml:"default_harness" json:"default_harness"`
	DefaultModel        string                     `toml:"default_model,omitempty" json:"default_model,omitempty"`
	RepositoryModels    map[string]ModelSelection  `toml:"repository_models,omitempty" json:"repository_models,omitempty"`
	RepositoryDiscovery *RepositoryDiscoveryConfig `toml:"repository_discovery,omitempty" json:"repository_discovery,omitempty"`
	GitHubObservation   *ExtensionConfig           `toml:"github_observation,omitempty" json:"github_observation,omitempty"`
	WorkerRuntime       string                     `toml:"worker_runtime" json:"worker_runtime"`
	DatabasePath        string                     `toml:"database_path" json:"database_path"`
	DataDir             string                     `toml:"data_dir" json:"data_dir"`
	WorktreeRoot        string                     `toml:"worktree_root" json:"worktree_root"`
	Wake                WakeConfig                 `toml:"wake" json:"wake"`
	Notifications       NotificationConfig         `toml:"notifications" json:"notifications"`
	PiWatcher           PiWatcherConfig            `toml:"pi_watcher" json:"pi_watcher"`
	WakeWatch           WakeWatchConfig            `toml:"wake_watch" json:"wake_watch"`
	Memory              MemoryConfig               `toml:"memory" json:"memory"`
	TerminalExtensions  TerminalExtensions         `toml:"terminal_extensions,omitempty" json:"terminal_extensions,omitempty"`
	LifecycleHandlers   LifecycleHandlers          `toml:"lifecycle_handlers,omitempty" json:"lifecycle_handlers,omitempty"`
}

type ModelSelection struct {
	Harness string `toml:"harness" json:"harness"`
	Model   string `toml:"model" json:"model"`
}

type TerminalExtensions struct {
	Herdr *ExtensionConfig `toml:"herdr,omitempty" json:"herdr,omitempty"`
	Cmux  *ExtensionConfig `toml:"cmux,omitempty" json:"cmux,omitempty"`
}

type ExtensionConfig struct {
	Command []string `toml:"command" json:"command"`
	SHA256  string   `toml:"sha256" json:"sha256"`
}

type RepositoryDiscoveryConfig struct {
	Roots   []string `toml:"roots" json:"roots"`
	Command []string `toml:"command" json:"command"`
	SHA256  string   `toml:"sha256" json:"sha256"`
}

type LifecycleHandlers struct {
	ReportAccepted []ReportAcceptedHandler `toml:"report_accepted,omitempty" json:"report_accepted,omitempty"`
}

type ReportAcceptedHandler struct {
	Name        string   `toml:"name" json:"name"`
	ExtensionID string   `toml:"extension_id" json:"extension_id"`
	Command     []string `toml:"command" json:"command"`
	SHA256      string   `toml:"sha256" json:"sha256"`
	Environment []string `toml:"environment,omitempty" json:"environment,omitempty"`
}

type WakeConfig struct {
	Enabled      bool          `toml:"enabled" json:"enabled"`
	DefaultBatch int           `toml:"default_batch" json:"default_batch"`
	MaxBatch     int           `toml:"max_batch" json:"max_batch"`
	ClaimTTL     time.Duration `toml:"claim_ttl" json:"claim_ttl"`
	ClaimTTLMin  time.Duration `toml:"claim_ttl_min" json:"claim_ttl_min"`
	ClaimTTLMax  time.Duration `toml:"claim_ttl_max" json:"claim_ttl_max"`
	DriverID     string        `toml:"driver_id" json:"driver_id"`
}

type NotificationConfig struct {
	Enabled         bool             `toml:"enabled" json:"enabled"`
	Details         bool             `toml:"details" json:"details"`
	TaskPerMinute   int              `toml:"task_per_minute" json:"task_per_minute"`
	GlobalPerMinute int              `toml:"global_per_minute" json:"global_per_minute"`
	Extension       *ExtensionConfig `toml:"extension,omitempty" json:"extension,omitempty"`
}

type MemoryConfig struct {
	Enabled bool `toml:"enabled" json:"enabled"`
}

type PiWatcherConfig struct {
	Enabled bool          `toml:"enabled" json:"enabled"`
	PollMin time.Duration `toml:"poll_min" json:"poll_min"`
	PollMax time.Duration `toml:"poll_max" json:"poll_max"`
}

type WakeWatchConfig struct {
	PollMin      time.Duration            `toml:"poll_min" json:"poll_min"`
	PollMax      time.Duration            `toml:"poll_max" json:"poll_max"`
	RenewHorizon time.Duration            `toml:"renew_horizon" json:"renew_horizon"`
	Delivery     *DeliveryExtensionConfig `toml:"delivery,omitempty" json:"delivery,omitempty"`
}

type DeliveryExtensionConfig struct {
	ExtensionID string   `toml:"extension_id" json:"extension_id"`
	Command     []string `toml:"command" json:"command"`
	SHA256      string   `toml:"sha256" json:"sha256"`
	Environment []string `toml:"environment,omitempty" json:"environment,omitempty"`
}

func Path() (string, error) {
	if path := os.Getenv("SHEPHRD_CONFIG"); path != "" {
		return expand(path)
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "shephrd", "config.toml"), nil
}

func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	cfg, err := defaults()
	if err != nil {
		return Config{}, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeDefault(path, cfg); err != nil {
			return Config{}, err
		}
	} else if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	} else {
		metadata, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("decode config %s: %w", path, err)
		}
		if !metadata.IsDefined("pi_watcher", "poll_max") {
			cfg.PiWatcher.PollMax = max(cfg.PiWatcher.PollMin, cfg.PiWatcher.PollMax)
		}
		if !metadata.IsDefined("wake_watch", "poll_max") {
			cfg.WakeWatch.PollMax = max(cfg.WakeWatch.PollMin, cfg.WakeWatch.PollMax)
		}
		if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
			if undecoded[0].String() == "worktree_backend" {
				return Config{}, fmt.Errorf("worktree_backend is retired and no longer accepted in %s; remove the key (the worktree backend is fixed to the native Git worktree)", path)
			}
			return Config{}, fmt.Errorf("unknown config key %q in %s", undecoded[0].String(), path)
		}
	}
	if cfg.DatabasePath == "" || cfg.DataDir == "" {
		return Config{}, fmt.Errorf("database_path and data_dir must not be empty")
	}
	if err := validateRepositoryDiscovery(cfg.RepositoryDiscovery); err != nil {
		return Config{}, fmt.Errorf("repository_discovery: %w", err)
	}
	if cfg.GitHubObservation != nil {
		if err := validateExtensionTrust(&cfg.GitHubObservation.Command, cfg.GitHubObservation.SHA256); err != nil {
			return Config{}, fmt.Errorf("github_observation: %w", err)
		}
	}
	cfg.DatabasePath, err = expand(cfg.DatabasePath)
	if err != nil {
		return Config{}, fmt.Errorf("database path: %w", err)
	}
	cfg.DataDir, err = expand(cfg.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("data directory: %w", err)
	}
	if strings.TrimSpace(cfg.WorktreeRoot) == "" {
		return Config{}, fmt.Errorf("worktree_root must not be empty")
	}
	cfg.WorktreeRoot, err = expand(cfg.WorktreeRoot)
	if err != nil {
		return Config{}, fmt.Errorf("worktree root: %w", err)
	}
	if runtime := os.Getenv("SHEPHRD_WORKER_RUNTIME"); runtime != "" {
		cfg.WorkerRuntime = runtime
	}
	if cfg.DefaultHarness != "current" && !ValidHarness(cfg.DefaultHarness) {
		return Config{}, fmt.Errorf("default_harness must be current, claude-code, pi, or codex, got %q", cfg.DefaultHarness)
	}
	if cfg.DefaultModel != "" && cfg.DefaultHarness == "current" {
		return Config{}, fmt.Errorf("default_model requires a static default_harness")
	}
	for repoID, selection := range cfg.RepositoryModels {
		if !strings.HasPrefix(repoID, "repo_") || len(repoID) != 17 || strings.IndexFunc(repoID[5:], func(char rune) bool {
			return !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f')
		}) >= 0 {
			return Config{}, fmt.Errorf("repository_models key %q must be a registered repository ID", repoID)
		}
		if !ValidHarness(selection.Harness) || selection.Model == "" {
			return Config{}, fmt.Errorf("repository_models.%s requires a valid harness and nonempty model", repoID)
		}
	}
	if err := ValidateRuntime(cfg.WorkerRuntime); err != nil {
		return Config{}, err
	}
	if err := validateTerminalExtension(cfg.TerminalExtensions.Herdr); err != nil {
		return Config{}, fmt.Errorf("terminal_extensions.herdr: %w", err)
	}
	if err := validateTerminalExtension(cfg.TerminalExtensions.Cmux); err != nil {
		return Config{}, fmt.Errorf("terminal_extensions.cmux: %w", err)
	}
	if err := validateReportAcceptedHandlers(cfg.LifecycleHandlers.ReportAccepted); err != nil {
		return Config{}, fmt.Errorf("lifecycle_handlers.report_accepted: %w", err)
	}
	if err := validateNotificationConfig(&cfg); err != nil {
		return Config{}, err
	}
	if err := validateWakeWatch(&cfg.WakeWatch); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ValidHarness(harness string) bool {
	switch harness {
	case "claude-code", "pi", "codex":
		return true
	default:
		return false
	}
}

func ValidateRuntime(runtime string) error {
	switch runtime {
	case "headless", "auto", "herdr", "cmux":
		return nil
	default:
		return fmt.Errorf("worker_runtime must be headless, auto, herdr, or cmux, got %q", runtime)
	}
}

func Require(commands ...string) error {
	for _, command := range commands {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("required dependency %q is not installed or not on PATH", command)
		}
	}
	return nil
}

func defaults() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}
	state := os.Getenv("SHEPHRD_STATE_DIR")
	if state == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "state")
		}
		state = filepath.Join(base, "shephrd")
	}
	data := os.Getenv("SHEPHRD_DATA_DIR")
	if data == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "share")
		}
		data = filepath.Join(base, "shephrd", "data")
	}
	return Config{
		DefaultHarness: "claude-code",
		WorkerRuntime:  "headless",
		DatabasePath:   filepath.Join(state, "shephrd.db"),
		DataDir:        data,
		WorktreeRoot:   filepath.Join(filepath.Dir(data), "worktrees"),
		Memory:         MemoryConfig{Enabled: true},
		Wake: WakeConfig{Enabled: true, DefaultBatch: 10, MaxBatch: 20, ClaimTTL: 5 * time.Minute,
			ClaimTTLMin: 30 * time.Second, ClaimTTLMax: 30 * time.Minute, DriverID: "driver:" + uuid.NewString()},
		Notifications: NotificationConfig{Enabled: false, Details: false, TaskPerMinute: 2, GlobalPerMinute: 10},
		PiWatcher:     PiWatcherConfig{Enabled: false, PollMin: time.Second, PollMax: time.Second},
		WakeWatch:     WakeWatchConfig{PollMin: 2 * time.Second, PollMax: 30 * time.Second, RenewHorizon: 30 * time.Minute},
	}, nil
}

func validateRepositoryDiscovery(configured *RepositoryDiscoveryConfig) error {
	if configured == nil {
		return nil
	}
	if len(configured.Roots) == 0 || len(configured.Roots) > 16 {
		return fmt.Errorf("roots must contain between 1 and 16 paths")
	}
	seen := make(map[string]bool, len(configured.Roots))
	for index, root := range configured.Roots {
		if root == "" {
			return fmt.Errorf("roots must not contain an empty path")
		}
		expanded, err := expand(root)
		if err != nil {
			return fmt.Errorf("root %q: %w", root, err)
		}
		expanded = filepath.Clean(expanded)
		if seen[expanded] {
			return fmt.Errorf("roots must not contain duplicate paths")
		}
		seen[expanded] = true
		configured.Roots[index] = expanded
	}
	return validateExtensionTrust(&configured.Command, configured.SHA256)
}

func validateTerminalExtension(configured *ExtensionConfig) error {
	if configured == nil {
		return nil
	}
	return validateExtensionTrust(&configured.Command, configured.SHA256)
}

func validateReportAcceptedHandlers(handlers []ReportAcceptedHandler) error {
	if len(handlers) > 8 {
		return fmt.Errorf("at most 8 handlers may be configured")
	}
	seen := make(map[string]bool, len(handlers))
	for index := range handlers {
		handler := &handlers[index]
		if !validHandlerIdentity(handler.Name) || !validExtensionIdentity(handler.ExtensionID) || seen[handler.Name] {
			return fmt.Errorf("handler %d has an invalid or duplicate name or extension_id", index+1)
		}
		seen[handler.Name] = true
		if err := validateExtensionTrust(&handler.Command, handler.SHA256); err != nil {
			return fmt.Errorf("handler %s: %w", handler.Name, err)
		}
		if err := validateEnvironmentAllowlist(handler.Environment); err != nil {
			return fmt.Errorf("handler %s %w", handler.Name, err)
		}
	}
	return nil
}

func validateEnvironmentAllowlist(names []string) error {
	if len(names) > 16 {
		return fmt.Errorf("may allow at most 16 environment variables")
	}
	environment := make(map[string]bool, len(names))
	for _, name := range names {
		if !validEnvironmentName(name) || strings.HasPrefix(name, "SHEPHRD_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || environment[name] {
			return fmt.Errorf("has an invalid or duplicate environment variable")
		}
		environment[name] = true
	}
	return nil
}

func validateWakeWatch(watch *WakeWatchConfig) error {
	if watch.PollMin <= 0 || watch.PollMax < watch.PollMin || watch.RenewHorizon <= 0 || watch.RenewHorizon > 24*time.Hour {
		return fmt.Errorf("wake_watch poll bounds or renew_horizon are invalid")
	}
	if watch.Delivery == nil {
		return nil
	}
	if !validExtensionIdentity(watch.Delivery.ExtensionID) {
		return fmt.Errorf("wake_watch.delivery has an invalid extension_id")
	}
	if err := validateExtensionTrust(&watch.Delivery.Command, watch.Delivery.SHA256); err != nil {
		return fmt.Errorf("wake_watch.delivery: %w", err)
	}
	if err := validateEnvironmentAllowlist(watch.Delivery.Environment); err != nil {
		return fmt.Errorf("wake_watch.delivery %w", err)
	}
	return nil
}

func validateExtensionTrust(command *[]string, digest string) error {
	if len(*command) == 0 || len(*command) > 16 {
		return fmt.Errorf("command must contain between 1 and 16 arguments")
	}
	for index, argument := range *command {
		if argument == "" || len(argument) > 8192 || strings.ContainsRune(argument, 0) {
			return fmt.Errorf("command contains an invalid argument")
		}
		if index == 0 {
			if !filepath.IsAbs(argument) {
				return fmt.Errorf("command executable must be an absolute path")
			}
			expanded, err := expand(argument)
			if err != nil {
				return fmt.Errorf("expand executable: %w", err)
			}
			(*command)[index] = expanded
		}
	}
	if len(digest) != 64 || strings.ToLower(digest) != digest || strings.IndexFunc(digest, func(char rune) bool {
		return !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f')
	}) >= 0 {
		return fmt.Errorf("sha256 must be 64 lowercase hexadecimal digits")
	}
	return nil
}

func validHandlerIdentity(value string) bool {
	if value == "" || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	return strings.IndexFunc(value, func(char rune) bool {
		return !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_')
	}) < 0
}

func validExtensionIdentity(value string) bool {
	if value == "" || len(value) > 128 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	separator := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z' || char >= '0' && char <= '9':
			separator = false
		case char == '-' || char == '.':
			if separator {
				return false
			}
			separator = true
		default:
			return false
		}
	}
	return !separator
}

func validEnvironmentName(value string) bool {
	if value == "" || len(value) > 128 || !(value[0] >= 'A' && value[0] <= 'Z' || value[0] == '_') {
		return false
	}
	return strings.IndexFunc(value, func(char rune) bool {
		return !(char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_')
	}) < 0
}

func validateNotificationConfig(cfg *Config) error {
	if cfg.Wake.DefaultBatch < 1 || cfg.Wake.DefaultBatch > 20 {
		return fmt.Errorf("wake.default_batch must be between 1 and 20")
	}
	if cfg.Wake.MaxBatch < cfg.Wake.DefaultBatch || cfg.Wake.MaxBatch > 20 {
		return fmt.Errorf("wake.max_batch must be between wake.default_batch and 20")
	}
	if cfg.Wake.ClaimTTLMin <= 0 || cfg.Wake.ClaimTTLMin > cfg.Wake.ClaimTTLMax || cfg.Wake.ClaimTTL < cfg.Wake.ClaimTTLMin || cfg.Wake.ClaimTTLMax < cfg.Wake.ClaimTTL {
		return fmt.Errorf("wake claim TTL values are invalid")
	}
	if cfg.Wake.ClaimTTLMax > 30*time.Minute || cfg.Wake.ClaimTTLMin < 30*time.Second {
		return fmt.Errorf("wake claim TTL bounds must stay within 30s and 30m")
	}
	if strings.TrimSpace(cfg.Wake.DriverID) == "" {
		cfg.Wake.DriverID = "driver:" + uuid.NewString()
	}
	if cfg.Notifications.Extension != nil {
		if err := validateExtensionTrust(&cfg.Notifications.Extension.Command, cfg.Notifications.Extension.SHA256); err != nil {
			return fmt.Errorf("notifications.extension: %w", err)
		}
	}
	if cfg.Notifications.Enabled && cfg.Notifications.Extension == nil {
		return fmt.Errorf("notifications.extension is required when notifications are enabled")
	}
	if cfg.Notifications.TaskPerMinute < 0 || cfg.Notifications.GlobalPerMinute < 0 {
		return fmt.Errorf("notification rate limits must not be negative")
	}
	if cfg.PiWatcher.PollMin <= 0 || cfg.PiWatcher.PollMax < cfg.PiWatcher.PollMin {
		return fmt.Errorf("pi_watcher poll bounds are invalid")
	}
	return nil
}

func writeDefault(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	var out strings.Builder
	if err := toml.NewEncoder(&out).Encode(cfg); err != nil {
		return fmt.Errorf("encode default config: %w", err)
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		return fmt.Errorf("write default config %s: %w", path, err)
	}
	return nil
}

func expand(path string) (string, error) {
	path = os.ExpandEnv(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}
