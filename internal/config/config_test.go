package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"shephrd/internal/fault"
)

func load(t *testing.T, body string) (Config, error) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(func(key string) string {
		switch key {
		case "SHEPHRD_CONFIG":
			return path
		case "HOME":
			return home
		}
		return ""
	})
}

func TestDefaults(t *testing.T) {
	cfg, err := load(t, `host = "workhorse"`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxDepth != 3 || filepath.Base(cfg.Store) != "shephrd.db" || !filepath.IsAbs(cfg.DataDir) {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestTimeouts(t *testing.T) {
	cfg, err := load(t, "host = \"workhorse\"\n[timeouts]\nturn_budget = \"1ms\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeouts.TurnBudget.Duration != time.Millisecond || cfg.Timeouts.Inactivity.Duration != 30*time.Minute || cfg.Timeouts.Settle.Duration != 5*time.Second {
		t.Fatalf("timeouts: %+v", cfg.Timeouts)
	}
}

func TestInvalidConfigurationIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":   "host = \"workhorse\"\nwake = true\n",
		"missing host":  `driver = "main"`,
		"bad driver":    "host = \"workhorse\"\ndriver = \"Main Driver\"\n",
		"relative path": "host = \"workhorse\"\nstore = \"state.db\"\n",
		"bad depth":     "host = \"workhorse\"\nmax_depth = -1\n",
		"not toml":      "host = ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, body); fault.As(err).Kind != "invalid_config" {
				t.Fatalf("load: %v", err)
			}
		})
	}
}
