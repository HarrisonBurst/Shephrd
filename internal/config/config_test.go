package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiWatcherPollingConfiguration(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		min  time.Duration
		max  time.Duration
	}{
		{"fresh", "", time.Second, time.Second},
		{"omitted", "[pi_watcher]\nenabled = true\n", time.Second, time.Second},
		{"explicit maximum", "[pi_watcher]\npoll_max = \"15s\"\n", time.Second, 15 * time.Second},
		{"explicit minimum", "[pi_watcher]\npoll_min = \"2s\"\n", 2 * time.Second, 2 * time.Second},
		{"explicit bounds", "[pi_watcher]\npoll_min = \"250ms\"\npoll_max = \"500ms\"\n", 250 * time.Millisecond, 500 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.toml")
			t.Setenv("SHEPHRD_CONFIG", path)
			t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(root, "state"))
			t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(root, "data"))
			if test.body != "" {
				if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				cfg, err := Load()
				if err != nil {
					t.Fatal(err)
				}
				if cfg.PiWatcher.PollMin != test.min || cfg.PiWatcher.PollMax != test.max {
					t.Fatalf("polling = %+v", cfg.PiWatcher)
				}
			}
		})
	}
}

func TestModelDefaultsConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, body, wantError string
	}{
		{"valid", "default_harness = \"pi\"\ndefault_model = \"openai-codex/gpt-6-sol:xhigh\"\n[repository_models.repo_887acd1c4f50]\nharness = \"claude-code\"\nmodel = \"claude-opus-5-5\"\n", ""},
		{"unknown field", "[repository_models.repo_887acd1c4f50]\nharness = \"pi\"\nmodel = \"opaque\"\neffort = \"high\"\n", "unknown config key"},
		{"invalid ID", "[repository_models.glide]\nharness = \"pi\"\nmodel = \"opaque\"\n", "repository_models key"},
		{"missing harness", "[repository_models.repo_887acd1c4f50]\nmodel = \"opaque\"\n", "requires a valid harness"},
		{"missing model", "[repository_models.repo_887acd1c4f50]\nharness = \"pi\"\n", "nonempty model"},
		{"invalid harness", "[repository_models.repo_887acd1c4f50]\nharness = \"current\"\nmodel = \"opaque\"\n", "requires a valid harness"},
		{"ambiguous global model", "default_harness = \"current\"\ndefault_model = \"opaque\"\n", "requires a static default_harness"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			t.Setenv("SHEPHRD_CONFIG", path)
			if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DefaultModel != "openai-codex/gpt-6-sol:xhigh" || cfg.RepositoryModels["repo_887acd1c4f50"] != (ModelSelection{Harness: "claude-code", Model: "claude-opus-5-5"}) {
				t.Fatalf("configuration = %+v", cfg)
			}
		})
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("SHEPHRD_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, filepath.Join("shephrd", "config.toml")) {
		t.Fatalf("path = %s", path)
	}
}

func TestDefaultHarnessValidation(t *testing.T) {
	for _, harness := range []string{"current", "claude-code", "pi", "codex"} {
		t.Run(harness, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			t.Setenv("SHEPHRD_CONFIG", path)
			if err := os.WriteFile(path, []byte("default_harness = \""+harness+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DefaultHarness != harness {
				t.Fatalf("default harness = %q", cfg.DefaultHarness)
			}
		})
	}
}

func TestInvalidDefaultHarnessIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SHEPHRD_CONFIG", path)
	if err := os.WriteFile(path, []byte("default_harness = \"cursor\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "current, claude-code, pi, or codex") {
		t.Fatalf("error = %v", err)
	}
}

func TestUnknownConfigKeyIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SHEPHRD_CONFIG", path)
	if err := os.WriteFile(path, []byte("unknown = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepositoryDiscoveryRequiresExplicitPinnedConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	t.Setenv("SHEPHRD_CONFIG", path)
	body := "[repository_discovery]\nroots = [\"~/Projects\"]\ncommand = [\"/absolute/scanner\"]\nsha256 = \"" + strings.Repeat("a", 64) + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepositoryDiscovery == nil || len(cfg.RepositoryDiscovery.Roots) != 1 || !filepath.IsAbs(cfg.RepositoryDiscovery.Roots[0]) || cfg.RepositoryDiscovery.Command[0] != "/absolute/scanner" {
		t.Fatalf("repository discovery = %+v", cfg.RepositoryDiscovery)
	}
}

func TestObsoleteWorkspaceRootsAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SHEPHRD_CONFIG", path)
	if err := os.WriteFile(path, []byte("workspace_roots = [\"/tmp\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("error = %v", err)
	}
}

func TestIncompleteRepositoryDiscoveryIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SHEPHRD_CONFIG", path)
	if err := os.WriteFile(path, []byte("[repository_discovery]\nroots = [\"/tmp\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("error = %v", err)
	}
}

func TestFreshConfigUsesOnlyNativeWorktrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SHEPHRD_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreeRoot == "" || !filepath.IsAbs(cfg.WorktreeRoot) {
		t.Fatalf("default worktree root = %q", cfg.WorktreeRoot)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "worktree_backend") {
		t.Fatalf("fresh config retained the retired backend setting:\n%s", body)
	}
}

// TestWorktreeBackendKeyIsRetired proves the retired worktree_backend key is
// rejected for every historical value with removal guidance, now that the
// worktree backend is fixed to the native Git worktree.
func TestWorktreeBackendKeyIsRetired(t *testing.T) {
	for _, value := range []string{"native_git_worktree", "treehouse", "clone"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			t.Setenv("SHEPHRD_CONFIG", path)
			if err := os.WriteFile(path, []byte("worktree_backend = \""+value+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "worktree_backend is retired and no longer accepted") || !strings.Contains(err.Error(), "remove the key") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestWakeWatchConfiguration(t *testing.T) {
	digest := strings.Repeat("a", 64)
	delivery := "[wake_watch.delivery]\nextension_id = \"shephrd.delivery-webhook\"\ncommand = [\"/opt/shephrd-delivery-webhook\", \"--url\", \"https://receiver.example/hook\"]\nsha256 = \"" + digest + "\"\n"
	for _, test := range []struct {
		name    string
		body    string
		min     time.Duration
		max     time.Duration
		horizon time.Duration
		valid   bool
	}{
		{name: "fresh", min: 2 * time.Second, max: 30 * time.Second, horizon: 30 * time.Minute, valid: true},
		{name: "explicit minimum", body: "[wake_watch]\npoll_min = \"45s\"\n", min: 45 * time.Second, max: 45 * time.Second, horizon: 30 * time.Minute, valid: true},
		{name: "delivery", body: "[wake_watch]\nrenew_horizon = \"5m\"\n" + delivery + "environment = [\"HOME\"]\n", min: 2 * time.Second, max: 30 * time.Second, horizon: 5 * time.Minute, valid: true},
		{name: "inverted bounds", body: "[wake_watch]\npoll_min = \"5s\"\npoll_max = \"1s\"\n"},
		{name: "zero horizon", body: "[wake_watch]\nrenew_horizon = \"0s\"\n"},
		{name: "relative command", body: strings.Replace(delivery, "/opt/shephrd-delivery-webhook", "shephrd-delivery-webhook", 1)},
		{name: "invalid digest", body: strings.Replace(delivery, digest, "ABC", 1)},
		{name: "invalid identity", body: strings.Replace(delivery, "shephrd.delivery-webhook", "Delivery", 1)},
		{name: "shephrd environment", body: delivery + "environment = [\"SHEPHRD_CONFIG\"]\n"},
		{name: "unknown key", body: delivery + "url = \"https://receiver.example\"\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.toml")
			t.Setenv("SHEPHRD_CONFIG", path)
			t.Setenv("SHEPHRD_STATE_DIR", filepath.Join(root, "state"))
			t.Setenv("SHEPHRD_DATA_DIR", filepath.Join(root, "data"))
			if test.body != "" {
				if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				cfg, err := Load()
				if !test.valid {
					if err == nil {
						t.Fatalf("invalid configuration accepted: %+v", cfg.WakeWatch)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if cfg.WakeWatch.PollMin != test.min || cfg.WakeWatch.PollMax != test.max || cfg.WakeWatch.RenewHorizon != test.horizon || (cfg.WakeWatch.Delivery != nil) != strings.Contains(test.body, "delivery") {
					t.Fatalf("wake_watch = %+v", cfg.WakeWatch)
				}
			}
		})
	}
}
