package plugin

import (
	"fmt"
	"slices"
	"sort"

	"shephrd/internal/config"
)

type Plugin struct {
	Name        string         `json:"name"`
	Package     string         `json:"package"`
	Order       int            `json:"order"`
	Dir         string         `json:"dir,omitempty"`
	Manifest    *Manifest      `json:"manifest,omitempty"`
	Unavailable string         `json:"unavailable,omitempty"`
	Options     map[string]any `json:"-"`
	root        string
}

type Registry struct {
	cfg     *config.Config
	plugins []*Plugin
}

func Load(cfg *config.Config, reserved []string) *Registry {
	r := &Registry{cfg: cfg}
	roots := map[string]string{}
	rootErrs := map[string]error{}
	for name, decl := range cfg.Plugins {
		p := &Plugin{Name: name, Package: decl.Package, Order: decl.Order, Options: decl.Options}
		r.plugins = append(r.plugins, p)
		root, seen := roots[decl.Package]
		err := rootErrs[decl.Package]
		if !seen && err == nil {
			root, err = packageRoot(cfg, decl.Package)
			roots[decl.Package], rootErrs[decl.Package] = root, err
		}
		if err != nil {
			p.Unavailable = err.Error()
			continue
		}
		p.root = root
		if err := p.resolve(root, reserved); err != nil {
			p.Unavailable = err.Error()
		}
	}
	sort.Slice(r.plugins, func(i, j int) bool {
		if r.plugins[i].Order != r.plugins[j].Order {
			return r.plugins[i].Order < r.plugins[j].Order
		}
		return r.plugins[i].Name < r.plugins[j].Name
	})
	return r
}

func (p *Plugin) resolve(root string, reserved []string) error {
	dirs, err := discover(root)
	if err != nil {
		return fmt.Errorf("package %s: %w", p.Package, err)
	}
	for _, dir := range dirs {
		m, err := readManifest(dir)
		if err != nil || m.Name != p.Name {
			continue
		}
		if err := m.validate(root, dir, reserved); err != nil {
			return fmt.Errorf("plugin %s: %w", p.Name, err)
		}
		p.Dir, p.Manifest = dir, &m
		return nil
	}
	return fmt.Errorf("package %s has no valid plugin named %s", p.Package, p.Name)
}

func (r *Registry) All() []*Plugin {
	return r.plugins
}

func (r *Registry) Get(name string) *Plugin {
	for _, p := range r.plugins {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// Hooks includes unavailable plugins whose manifest is unknown, so a broken
// plugin fails every gate closed rather than silently skipping its own.
func (r *Registry) Hooks(point string) []*Plugin {
	var hooks []*Plugin
	for _, p := range r.plugins {
		if p.Manifest == nil || slices.Contains(p.Manifest.Intercept, Intercept{Point: point}) {
			hooks = append(hooks, p)
		}
	}
	return hooks
}

func (r *Registry) Provider(kind, name string) *Plugin {
	p := r.Get(name)
	if p == nil || p.Manifest == nil {
		return p
	}
	if slices.Contains(p.Manifest.Provide, Provide{Type: kind}) {
		return p
	}
	return nil
}

func (r *Registry) Command(name string) *Plugin {
	for _, p := range r.plugins {
		if p.Manifest != nil && p.Manifest.Commands[name] != "" {
			return p
		}
	}
	return nil
}
