package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/fault"
	"shephrd/internal/testkit"
)

func TestMain(m *testing.M) {
	testkit.Main(m)
}

func getenv(key string) string {
	return os.Getenv(key)
}

type fixture struct {
	t   *testing.T
	cfg *config.Config
	pkg string
}

func newFixture(t *testing.T) *fixture {
	pkg, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, pkg: pkg, cfg: &config.Config{
		Path:     filepath.Join(t.TempDir(), "config.toml"),
		DataDir:  t.TempDir(),
		Packages: map[string]config.Package{"tools": {Path: pkg}},
		Plugins:  map[string]config.PluginConfig{},
	}}
}

func (f *fixture) plugin(name, manifest string, order int, options map[string]any) {
	testkit.WritePlugin(f.t, filepath.Join(f.pkg, "plugins", name), name, manifest)
	f.cfg.Plugins[name] = config.PluginConfig{Package: "tools", Order: order, Options: options}
}

func TestGateAllowsBlocksAndFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options map[string]any
		kind    string
	}{
		{"allow", map[string]any{"decision": "allow"}, ""},
		{"block", map[string]any{"decision": "block", "reason": "repository is frozen"}, "plugin_blocked"},
		{"crash", map[string]any{"intercept": "crash"}, "plugin_failed"},
		{"garbage", map[string]any{"intercept": "garbage"}, "plugin_failed"},
		{"unknown decision", map[string]any{"decision": "maybe"}, "plugin_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.plugin("freeze", "[[intercept]]\npoint = \"task.create\"\n", 0, tc.options)
			err := Load(f.cfg, nil).Gate(context.Background(), "task.create", map[string]string{"repo": "api"}, getenv)
			if tc.kind == "" {
				if err != nil {
					t.Fatalf("gate: %v", err)
				}
				return
			}
			if blocked, ok := AsBlocked(err); ok {
				err = blocked.Fault()
				if !strings.Contains(err.Error(), "repository is frozen") {
					t.Fatalf("block reason lost: %v", err)
				}
			}
			if fault.As(err).Kind != tc.kind {
				t.Fatalf("gate: %v, want %s", err, tc.kind)
			}
		})
	}
}

func TestGateTimesOut(t *testing.T) {
	f := newFixture(t)
	f.plugin("slow", "[[intercept]]\npoint = \"task.start\"\n", 0, map[string]any{"intercept": "sleep"})
	saved := Timeouts["intercept"]
	Timeouts["intercept"] = 200 * time.Millisecond
	defer func() { Timeouts["intercept"] = saved }()
	start := time.Now()
	err := Load(f.cfg, nil).Gate(context.Background(), "task.start", nil, getenv)
	if fault.As(err).Kind != "plugin_failed" || time.Since(start) > 10*time.Second {
		t.Fatalf("gate: %v after %s", err, time.Since(start))
	}
}

func TestGatesRunInOrderAndFirstBlockWins(t *testing.T) {
	f := newFixture(t)
	log := filepath.Join(t.TempDir(), "calls")
	f.plugin("second", "[[intercept]]\npoint = \"task.send\"\n", 20, map[string]any{"decision": "block", "reason": "second", "log": log})
	f.plugin("first", "[[intercept]]\npoint = \"task.send\"\n", 10, map[string]any{"decision": "block", "reason": "first", "log": log})
	f.plugin("other", "[[intercept]]\npoint = \"task.cancel\"\n", 0, map[string]any{"decision": "block", "log": log})
	err := Load(f.cfg, nil).Gate(context.Background(), "task.send", nil, getenv)
	blocked, ok := AsBlocked(err)
	if !ok || blocked.Plugin != "first" {
		t.Fatalf("gate: %v", err)
	}
	body, _ := os.ReadFile(log)
	if strings.Count(string(body), "\n") != 1 {
		t.Fatalf("calls after the first block:\n%s", body)
	}
}

func TestUnavailablePluginFailsEveryGateClosed(t *testing.T) {
	f := newFixture(t)
	f.cfg.Plugins["missing"] = config.PluginConfig{Package: "tools"}
	reg := Load(f.cfg, nil)
	if p := reg.Get("missing"); p.Unavailable == "" {
		t.Fatal("missing plugin is available")
	}
	for _, point := range InterceptPoints {
		if err := reg.Gate(context.Background(), point, nil, getenv); fault.As(err).Kind != "plugin_failed" {
			t.Fatalf("%s: %v", point, err)
		}
	}
}

func TestPluginEnvironmentIsClean(t *testing.T) {
	f := newFixture(t)
	log := filepath.Join(t.TempDir(), "calls")
	f.plugin("probe", "[[intercept]]\npoint = \"task.create\"\n", 0, map[string]any{"log": log})
	t.Setenv("SHEPHRD_RUN_TOKEN", "secret")
	if err := Load(f.cfg, nil).Gate(context.Background(), "task.create", nil, getenv); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(log)
	if strings.Contains(string(body), "secret") || !strings.Contains(string(body), "SHEPHRD_PLUGIN=probe") {
		t.Fatalf("plugin environment:\n%s", body)
	}
}

func TestManifestValidation(t *testing.T) {
	for name, manifest := range map[string]string{
		"core command":     "[commands]\nrepo = \"shadow\"\n",
		"foreign command":  "[commands]\nother = \"x\"\n",
		"unknown point":    "[[intercept]]\npoint = \"task.explode\"\n",
		"unknown provider": "[[provide]]\ntype = \"teleport\"\n",
		"escaping skill":   "skills = [\"../../outside.md\"]\n",
		"unknown key":      "colour = \"blue\"\n",
		"absolute skill":   "skills = [\"/etc/passwd\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			pluginName := "repo"
			if name != "core command" {
				pluginName = "probe"
			}
			f.plugin(pluginName, manifest, 0, nil)
			if p := Load(f.cfg, []string{"repo"}).Get(pluginName); p.Unavailable == "" {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}

func TestUnsupportedProtocolIsUnavailable(t *testing.T) {
	f := newFixture(t)
	f.plugin("future", "", 0, nil)
	path := filepath.Join(f.pkg, "plugins", "future", "plugin.toml")
	body, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(body), "protocol = 1", "protocol = 2", 1)), 0o644)
	if p := Load(f.cfg, nil).Get("future"); !strings.Contains(p.Unavailable, "protocol 2") {
		t.Fatalf("unavailable: %q", p.Unavailable)
	}
}

func TestPackageDiscovery(t *testing.T) {
	f := newFixture(t)
	testkit.WritePlugin(t, filepath.Join(f.pkg, "extra", "a"), "alpha", "")
	testkit.WritePlugin(t, filepath.Join(f.pkg, "plugins", "b"), "beta", "")
	f.cfg.Plugins["alpha"] = config.PluginConfig{Package: "tools"}
	f.cfg.Plugins["beta"] = config.PluginConfig{Package: "tools"}
	reg := Load(f.cfg, nil)
	if reg.Get("alpha").Unavailable == "" || reg.Get("beta").Unavailable != "" {
		t.Fatalf("convention discovery: alpha %q, beta %q", reg.Get("alpha").Unavailable, reg.Get("beta").Unavailable)
	}
	os.WriteFile(filepath.Join(f.pkg, "shephrd-package.toml"), []byte("plugins = [\"extra/*\"]\n"), 0o644)
	reg = Load(f.cfg, nil)
	if reg.Get("alpha").Unavailable != "" || reg.Get("beta").Unavailable == "" {
		t.Fatalf("explicit discovery: alpha %q, beta %q", reg.Get("alpha").Unavailable, reg.Get("beta").Unavailable)
	}
}

func TestGitPackageSyncAndPinning(t *testing.T) {
	source := testkit.GitRepo(t, "plugins")
	testkit.WritePlugin(t, filepath.Join(source, "plugins", "linear"), "linear", "")
	testkit.Git(t, source, "add", ".")
	testkit.Git(t, source, "commit", "-q", "-m", "Add plugin")
	rev := testkit.Git(t, source, "rev-parse", "HEAD")
	cfg := &config.Config{
		Path:     filepath.Join(t.TempDir(), "config.toml"),
		DataDir:  t.TempDir(),
		Packages: map[string]config.Package{"acme": {Git: source, Rev: rev}},
		Plugins:  map[string]config.PluginConfig{"linear": {Package: "acme"}},
	}
	if p := Load(cfg, nil).Get("linear"); !strings.Contains(p.Unavailable, "plugin sync") {
		t.Fatalf("before sync: %q", p.Unavailable)
	}
	stale := filepath.Join(cfg.DataDir, "packages", "stale")
	os.MkdirAll(stale, 0o700)
	statuses, err := Sync(cfg)
	if err != nil || len(statuses) != 2 || statuses[0].State != "synced" || statuses[1].State != "removed" {
		t.Fatalf("sync: %+v, %v", statuses, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("undeclared package kept")
	}
	if p := Load(cfg, nil).Get("linear"); p.Unavailable != "" {
		t.Fatalf("after sync: %q", p.Unavailable)
	}
	checkout := filepath.Join(cfg.DataDir, "packages", "acme")
	os.WriteFile(filepath.Join(checkout, "plugins", "linear", "plugin.toml"), []byte("tampered"), 0o644)
	if p := Load(cfg, nil).Get("linear"); !strings.Contains(p.Unavailable, "local changes") {
		t.Fatalf("after tampering: %q", p.Unavailable)
	}
	if statuses, err := Sync(cfg); err != nil || statuses[0].State != "synced" {
		t.Fatalf("resync: %+v, %v", statuses, err)
	}
	if p := Load(cfg, nil).Get("linear"); p.Unavailable != "" {
		t.Fatalf("after resync: %q", p.Unavailable)
	}
}
