package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"shephrd/internal/fault"
)

type Config struct {
	Path      string                  `toml:"-"`
	Host      string                  `toml:"host"`
	Driver    string                  `toml:"driver"`
	MaxDepth  int                     `toml:"max_depth"`
	Store     string                  `toml:"store"`
	DataDir   string                  `toml:"data_dir"`
	Defaults  Target                  `toml:"defaults"`
	Routes    []Route                 `toml:"routes"`
	Packages  map[string]Package      `toml:"packages"`
	Plugins   map[string]PluginConfig `toml:"plugins"`
	Providers map[string]string       `toml:"providers"`
	Skills    map[string]Skill        `toml:"skills"`
	Timeouts  Timeouts                `toml:"timeouts"`
	Drivers   map[string]Driver       `toml:"drivers"`
	Repos     map[string]Repo         `toml:"repos"`
	Grants    []Grant                 `toml:"grants"`
}

type Repo struct {
	Landing Landing `toml:"landing"`
}

type Landing struct {
	Mode   string `toml:"mode"`
	Method string `toml:"method"`
	Forge  string `toml:"forge"`
	Merge  string `toml:"merge"`
}

// Grant is standing authority the user declared: to a driver or plugin,
// or to every sub-driver, optionally for one repository.
type Grant struct {
	To      string   `toml:"to"`
	Repo    string   `toml:"repo"`
	Actions []string `toml:"actions"`
}

type Driver struct {
	Delivery Delivery `toml:"delivery"`
}

type Delivery struct {
	Provider   string `toml:"provider"`
	URL        string `toml:"url"`
	SecretFile string `toml:"secret_file"`
}

type Skill struct {
	Replace string `toml:"replace"`
	Extend  string `toml:"extend"`
}

type Timeouts struct {
	Inactivity       Duration `toml:"inactivity"`
	DriverInactivity Duration `toml:"driver_inactivity"`
	TurnBudget       Duration `toml:"turn_budget"`
	Settle           Duration `toml:"settle"`
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

type Target struct {
	Host    string `toml:"host" json:"host"`
	Harness string `toml:"harness" json:"harness"`
	Model   string `toml:"model" json:"model,omitempty"`
}

type Route struct {
	Role    string `toml:"role"`
	Repo    string `toml:"repo"`
	Harness string `toml:"harness"`
	Model   string `toml:"model"`
}

type Package struct {
	Git  string `toml:"git"`
	Rev  string `toml:"rev"`
	Path string `toml:"path"`
}

type PluginConfig struct {
	Package string         `toml:"package"`
	Order   int            `toml:"order"`
	Options map[string]any `toml:"options"`
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Getenv func(string) string

var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func Path(getenv Getenv) (string, error) {
	if path := getenv("SHEPHRD_CONFIG"); path != "" {
		return expand(getenv, path)
	}
	dir, err := xdgDir(getenv, "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shephrd", "config.toml"), nil
}

func Load(getenv Getenv) (Config, error) {
	path, err := Path(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Path: path}
	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fault.New("not_initialized", "no configuration at %s", path).WithNext("init")
	}
	if err != nil {
		return Config{}, fault.New("invalid_config", "%s: %v", path, err)
	}
	var keys []string
	for _, key := range md.Undecoded() {
		if len(key) > 2 && key[0] == "plugins" && key[2] == "options" {
			continue
		}
		keys = append(keys, key.String())
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		return Config{}, fault.New("invalid_config", "%s: unknown keys %s", path, strings.Join(keys, ", "))
	}
	if err := cfg.resolve(getenv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) resolve(getenv Getenv) error {
	if !NamePattern.MatchString(c.Host) {
		return fault.New("invalid_config", "%s: host %q must match %s", c.Path, c.Host, NamePattern)
	}
	if c.Driver != "" && !NamePattern.MatchString(c.Driver) {
		return fault.New("invalid_config", "%s: driver %q must match %s", c.Path, c.Driver, NamePattern)
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = 3
	}
	for _, d := range []struct {
		value    *Duration
		fallback time.Duration
	}{
		{&c.Timeouts.Inactivity, 30 * time.Minute},
		{&c.Timeouts.DriverInactivity, 30 * time.Minute},
		{&c.Timeouts.TurnBudget, 10 * time.Minute},
		{&c.Timeouts.Settle, 5 * time.Second},
	} {
		if d.value.Duration == 0 {
			d.value.Duration = d.fallback
		}
	}
	for role, skill := range c.Skills {
		if role != "main" && role != "subdriver" && role != "worker" {
			return fault.New("invalid_config", "%s: skills.%s: the roles are main, subdriver and worker", c.Path, role)
		}
		for _, path := range []*string{&skill.Replace, &skill.Extend} {
			if *path == "" {
				continue
			}
			expanded, err := expand(getenv, *path)
			if err != nil {
				return err
			}
			*path = expanded
		}
		c.Skills[role] = skill
	}
	if c.MaxDepth < 1 {
		return fault.New("invalid_config", "%s: max_depth must be at least 1", c.Path)
	}
	for name, repo := range c.Repos {
		l := repo.Landing
		switch {
		case l.Mode == "direct" && l.Method == "":
			l.Method = "fast-forward"
		case l.Mode == "pull_request" && l.Merge == "":
			return fault.New("invalid_config", "%s: repos.%s.landing: pull_request mode needs merge = \"shephrd\" or \"external\"", c.Path, name)
		}
		valid := l.Mode == "" ||
			l.Mode == "direct" && (l.Method == "fast-forward" || l.Method == "merge") ||
			l.Mode == "pull_request" && l.Forge != "" && (l.Merge == "shephrd" || l.Merge == "external")
		if !valid {
			return fault.New("invalid_config", "%s: repos.%s.landing: mode is direct (method fast-forward or merge) or pull_request (with forge and merge)", c.Path, name)
		}
		repo.Landing = l
		c.Repos[name] = repo
	}
	for i, g := range c.Grants {
		kind, name, _ := strings.Cut(g.To, ":")
		if g.To != "subdriver" && !((kind == "driver" || kind == "plugin") && NamePattern.MatchString(name)) {
			return fault.New("invalid_config", "%s: grants[%d].to is driver:<name>, plugin:<name> or subdriver", c.Path, i)
		}
		for _, action := range g.Actions {
			if action != "land" && action != "discard" {
				return fault.New("invalid_config", "%s: grants[%d] action %q is not land or discard", c.Path, i, action)
			}
		}
	}
	for name, pkg := range c.Packages {
		if !NamePattern.MatchString(name) {
			return fault.New("invalid_config", "%s: package name %q must match %s", c.Path, name, NamePattern)
		}
		switch {
		case pkg.Git != "" && pkg.Path != "":
			return fault.New("invalid_config", "%s: package %s sets both git and path", c.Path, name)
		case pkg.Git != "" && !commitPattern.MatchString(pkg.Rev):
			return fault.New("invalid_config", "%s: package %s must pin rev to a full commit", c.Path, name)
		case pkg.Path != "":
			path, err := expand(getenv, pkg.Path)
			if err != nil {
				return err
			}
			pkg.Path = path
			c.Packages[name] = pkg
		case pkg.Git == "":
			return fault.New("invalid_config", "%s: package %s needs git or path", c.Path, name)
		}
	}
	for name, plugin := range c.Plugins {
		if !NamePattern.MatchString(name) {
			return fault.New("invalid_config", "%s: plugin name %q must match %s", c.Path, name, NamePattern)
		}
		if _, ok := c.Packages[plugin.Package]; !ok {
			return fault.New("invalid_config", "%s: plugin %s names undeclared package %q", c.Path, name, plugin.Package)
		}
	}
	var err error
	if c.Store == "" {
		dir, err := xdgDir(getenv, "XDG_STATE_HOME", filepath.Join(".local", "state"))
		if err != nil {
			return err
		}
		c.Store = filepath.Join(dir, "shephrd", "shephrd.db")
	} else if c.Store, err = expand(getenv, c.Store); err != nil {
		return err
	}
	if c.DataDir == "" {
		dir, err := xdgDir(getenv, "XDG_DATA_HOME", filepath.Join(".local", "share"))
		if err != nil {
			return err
		}
		c.DataDir = filepath.Join(dir, "shephrd")
	} else if c.DataDir, err = expand(getenv, c.DataDir); err != nil {
		return err
	}
	return nil
}

func Write(path, host, driver string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	body := fmt.Sprintf("host = %q\ndriver = %q\n\n[[grants]]\nto = \"driver:%s\"\nactions = [\"land\", \"discard\"]\n", host, driver, driver)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	if _, err := file.WriteString(body); err != nil {
		file.Close()
		return fmt.Errorf("write configuration: %w", err)
	}
	return file.Close()
}

func xdgDir(getenv Getenv, name, fallback string) (string, error) {
	if dir := getenv(name); dir != "" {
		return dir, nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", fault.New("invalid_environment", "HOME is not set")
	}
	return filepath.Join(home, fallback), nil
}

func expand(getenv Getenv, path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home := getenv("HOME")
		if home == "" {
			return "", fault.New("invalid_environment", "HOME is not set")
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		return "", fault.New("invalid_config", "path %q must be absolute", path)
	}
	return filepath.Clean(path), nil
}
