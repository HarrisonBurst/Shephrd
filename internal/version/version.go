package version

import "runtime/debug"

// Protocol is the request and host-operation protocol every host speaks.
const Protocol = 1

// String is this binary's release: its module version, or the commit it
// was built from. All hosts in an installation must run the same release.
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["vcs.revision"] == "" {
		return "(devel)"
	}
	if settings["vcs.modified"] == "true" {
		return settings["vcs.revision"] + "+dirty"
	}
	return settings["vcs.revision"]
}
