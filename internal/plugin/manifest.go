package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"shephrd/internal/config"
)

const Protocol = 1

var InterceptPoints = []string{
	"task.create", "task.start", "task.send", "task.adopt", "task.cancel",
	"report.result",
	"task.deliver", "task.discard", "grant.add",
}

var ProviderTypes = []string{"router", "harness", "presentation", "delivery", "forge"}

type Manifest struct {
	Name      string            `toml:"name" json:"name"`
	Version   string            `toml:"version" json:"version"`
	Protocol  int               `toml:"protocol" json:"protocol"`
	Exec      []string          `toml:"exec" json:"exec"`
	Skills    []string          `toml:"skills" json:"skills,omitempty"`
	Events    Events            `toml:"events" json:"events"`
	Commands  map[string]string `toml:"commands" json:"commands,omitempty"`
	Intercept []Intercept       `toml:"intercept" json:"intercept,omitempty"`
	Provide   []Provide         `toml:"provide" json:"provide,omitempty"`
}

type Intercept struct {
	Point string `toml:"point" json:"point"`
}

type Provide struct {
	Type string `toml:"type" json:"type"`
}

type Events struct {
	Subscribe []string `toml:"subscribe" json:"subscribe,omitempty"`
	Repos     []string `toml:"repos" json:"repos,omitempty"`
}

func readManifest(dir string) (Manifest, error) {
	var m Manifest
	md, err := toml.DecodeFile(filepath.Join(dir, "plugin.toml"), &m)
	if err != nil {
		return Manifest{}, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Manifest{}, fmt.Errorf("plugin.toml: unknown key %s", undecoded[0])
	}
	return m, nil
}

func (m Manifest) validate(root, dir string, reserved []string) error {
	if !config.NamePattern.MatchString(m.Name) {
		return fmt.Errorf("name %q must match %s", m.Name, config.NamePattern)
	}
	if m.Protocol != Protocol {
		return fmt.Errorf("protocol %d is not supported; this release speaks %d", m.Protocol, Protocol)
	}
	if len(m.Exec) == 0 {
		return fmt.Errorf("exec is empty")
	}
	if _, err := inside(root, dir, m.Exec[0]); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	for _, skill := range m.Skills {
		if _, err := inside(root, dir, skill); err != nil {
			return fmt.Errorf("skill: %w", err)
		}
	}
	for name := range m.Commands {
		if name != m.Name {
			return fmt.Errorf("command %q must be the plugin's name", name)
		}
		if slices.Contains(reserved, name) {
			return fmt.Errorf("command %q collides with a core command", name)
		}
	}
	for _, i := range m.Intercept {
		if !slices.Contains(InterceptPoints, i.Point) {
			return fmt.Errorf("unknown intercept point %q", i.Point)
		}
	}
	for _, p := range m.Provide {
		if !slices.Contains(ProviderTypes, p.Type) {
			return fmt.Errorf("unknown provider type %q", p.Type)
		}
	}
	return nil
}

func inside(root, dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s must be relative to the plugin directory", rel)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(dir, rel))
	if err != nil {
		return "", fmt.Errorf("%s: %w", rel, err)
	}
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%s resolves outside its package", rel)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", rel)
	}
	return path, nil
}
