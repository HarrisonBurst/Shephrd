package cli

import (
	"encoding/json"
	"slices"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/fault"
	"shephrd/internal/plugin"
)

var builtinHarnesses = []string{"claude-code", "codex", "pi"}

// route resolves a new task's target: explicit flags win, then the router
// provider or the configured routes, then defaults. Core validates the answer.
func (a *app) route(p *coord.Proposal, explicit config.Target) (config.Target, error) {
	cfg, err := a.config()
	if err != nil {
		return config.Target{}, err
	}
	target := explicit
	if target.Harness == "" {
		answer, err := a.routerAnswer(p)
		if err != nil {
			return config.Target{}, err
		}
		if target.Host == "" {
			target.Host = answer.Host
		}
		target.Harness, target.Model = answer.Harness, firstNonEmpty(target.Model, answer.Model)
	}
	switch {
	case p.RepoID != 0 && target.Host != "" && target.Host != p.Host:
		return config.Target{}, fault.New("invalid_target", "repository %s is on host %s, not %s", p.Repo, p.Host, target.Host)
	case p.RepoID != 0:
		target.Host = p.Host
	case target.Host == "":
		target.Host = firstNonEmpty(cfg.Defaults.Host, cfg.Host)
	}
	if _, ok := cfg.Hosts[target.Host]; target.Host != cfg.Host && !ok {
		return config.Target{}, fault.New("unknown_host", "host %q is not configured", target.Host)
	}
	if target.Harness == "" {
		return config.Target{}, fault.New("no_harness", "no harness for this task; pass --harness or set [defaults] harness")
	}
	if !slices.Contains(builtinHarnesses, target.Harness) {
		reg, err := a.registry()
		if err != nil {
			return config.Target{}, err
		}
		if reg.Provider("harness", target.Harness) == nil {
			return config.Target{}, fault.New("unknown_harness", "harness %q is neither built in nor provided by a declared plugin", target.Harness)
		}
	}
	return target, nil
}

func (a *app) routerAnswer(p *coord.Proposal) (config.Target, error) {
	cfg, err := a.config()
	if err != nil {
		return config.Target{}, err
	}
	if name := cfg.Providers["router"]; name != "" {
		reg, err := a.registry()
		if err != nil {
			return config.Target{}, err
		}
		provider := reg.Provider("router", name)
		if provider == nil {
			return config.Target{}, fault.New("plugin_failed", "router %q is not a declared plugin providing router", name)
		}
		parent := ""
		if p.Parent != nil {
			parent = p.Parent.Ref
		}
		caller, err := a.identity()
		if err != nil {
			return config.Target{}, err
		}
		out, err := reg.Call(a.ctx, provider, plugin.Call{Kind: "provide", Type: "router", Getenv: a.getenv, Body: map[string]any{
			"owner": caller.String(), "parent": parent, "role": p.Role, "repo": p.Repo, "title": p.Title, "objective": p.Objective,
		}})
		if err != nil {
			return config.Target{}, fault.New("plugin_failed", "router %s: %v", name, err)
		}
		var answer config.Target
		if err := json.Unmarshal(out, &answer); err != nil {
			return config.Target{}, fault.New("plugin_failed", "router %s answered invalidly: %v", name, err)
		}
		return answer, nil
	}
	for _, route := range cfg.Routes {
		if (route.Role == "" || route.Role == p.Role) && (route.Repo == "" || route.Repo == p.Repo) {
			return config.Target{Harness: route.Harness, Model: route.Model}, nil
		}
	}
	return cfg.Defaults, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
