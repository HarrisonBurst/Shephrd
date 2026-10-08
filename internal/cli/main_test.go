package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates every test in this package from the user's real Shephrd
// configuration and control database. CLI tests that executed commands
// without an explicit per-test SHEPHRD_CONFIG previously resolved the user's
// default config and opened — and could migrate — the live control database.
// No project test command may ever touch the default database, so the whole
// package runs against a throwaway config/state/data directory; individual
// tests still override these with t.Setenv when they build their own fixture.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shephrd-cli-isolated-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("SHEPHRD_CONFIG", filepath.Join(dir, "config.toml"))
	os.Setenv("SHEPHRD_STATE_DIR", filepath.Join(dir, "state"))
	os.Setenv("SHEPHRD_DATA_DIR", filepath.Join(dir, "data"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
