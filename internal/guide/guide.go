package guide

import (
	"embed"
	"fmt"
	"os"
	"slices"
	"strings"

	"shephrd/internal/config"
	"shephrd/internal/fault"
)

//go:embed main.md subdriver.md worker.md
var builtin embed.FS

var Roles = []string{"main", "subdriver", "worker"}

const referencePrefix = "## Reference: "

// Document returns a role's guidance as configured, split into the main
// text and its reference sections, which are read on demand.
func Document(cfg *config.Config, role string) (string, map[string]string, error) {
	if !slices.Contains(Roles, role) {
		return "", nil, fault.New("usage", "role must be main, subdriver or worker, not %q", role)
	}
	override := cfg.Skills[role]
	var text string
	if override.Replace != "" {
		body, err := os.ReadFile(override.Replace)
		if err != nil {
			return "", nil, fault.New("invalid_config", "skills.%s.replace: %v", role, err)
		}
		text = string(body)
	} else {
		body, err := builtin.ReadFile(role + ".md")
		if err != nil {
			return "", nil, err
		}
		text = string(body)
	}
	main, sections := split(text)
	if override.Extend != "" {
		body, err := os.ReadFile(override.Extend)
		if err != nil {
			return "", nil, fault.New("invalid_config", "skills.%s.extend: %v", role, err)
		}
		extra, extraSections := split(string(body))
		main = strings.TrimRight(main, "\n") + "\n\n" + strings.TrimLeft(extra, "\n")
		for name, section := range extraSections {
			sections[name] = section
		}
	}
	if len(sections) > 0 {
		names := make([]string, 0, len(sections))
		for name := range sections {
			names = append(names, name)
		}
		slices.Sort(names)
		main = strings.TrimRight(main, "\n") + fmt.Sprintf("\n\nReference sections, read on demand with `shephrd skill %s --section <name>`: %s.\n", role, strings.Join(names, ", "))
	}
	return main, sections, nil
}

func split(text string) (string, map[string]string) {
	sections := map[string]string{}
	parts := strings.Split(text, "\n"+referencePrefix)
	for _, part := range parts[1:] {
		name, body, _ := strings.Cut(part, "\n")
		sections[strings.TrimSpace(name)] = referencePrefix + name + "\n" + body
	}
	return parts[0], sections
}
