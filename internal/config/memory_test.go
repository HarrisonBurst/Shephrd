package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryConfiguration(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"omitted", "", true},
		{"empty table", "[memory]\n", true},
		{"enabled", "[memory]\nenabled = true\n", true},
		{"disabled", "[memory]\nenabled = false\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			t.Setenv("SHEPHRD_CONFIG", path)
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil || cfg.Memory.Enabled != test.want {
				t.Fatalf("memory = %+v, err = %v", cfg.Memory, err)
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != test.body {
				t.Fatalf("existing config changed: %s, err = %v", body, err)
			}
		})
	}
}

func TestFreshConfigEnablesMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SHEPHRD_CONFIG", path)
	cfg, err := Load()
	if err != nil || !cfg.Memory.Enabled {
		t.Fatalf("memory = %+v, err = %v", cfg.Memory, err)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "[memory]\n  enabled = true") {
		t.Fatalf("default config = %s, err = %v", body, err)
	}
}

func TestInvalidMemoryConfigurationIsRejected(t *testing.T) {
	for _, body := range []string{"[memory]\nenabled = \"false\"\n", "[memory]\nenabeld = false\n"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		t.Setenv("SHEPHRD_CONFIG", path)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("accepted invalid configuration: %s", body)
		}
	}
}
