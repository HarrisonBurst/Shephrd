package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerRuntimeEnvironmentOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("worker_runtime = \"headless\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "herdr")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerRuntime != "herdr" {
		t.Fatalf("runtime = %q", cfg.WorkerRuntime)
	}
}

func TestCmuxWorkerRuntimeIsExplicitlyAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("worker_runtime = \"cmux\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerRuntime != "cmux" {
		t.Fatalf("runtime = %q", cfg.WorkerRuntime)
	}
}

func TestAutoWorkerRuntimeIsOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("worker_runtime = \"auto\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	cfg, err := Load()
	if err != nil || cfg.WorkerRuntime != "auto" {
		t.Fatalf("runtime = %q, err = %v", cfg.WorkerRuntime, err)
	}
}

func TestCmuxExtensionRequiresExplicitAbsoluteCommandAndDigest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	executable := filepath.Join(root, "shephrd-terminal-cmux")
	body := "[terminal_extensions.cmux]\ncommand = [" + fmt.Sprintf("%q", executable) + "]\nsha256 = \"" + strings.Repeat("a", 64) + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TerminalExtensions.Cmux == nil || len(cfg.TerminalExtensions.Cmux.Command) != 1 || cfg.TerminalExtensions.Cmux.Command[0] != executable {
		t.Fatalf("extension = %+v", cfg.TerminalExtensions.Cmux)
	}
}

func TestHerdrExtensionRequiresExplicitAbsoluteCommandAndDigest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	executable := filepath.Join(root, "shephrd-terminal-herdr")
	body := "[terminal_extensions.herdr]\ncommand = [" + fmt.Sprintf("%q", executable) + "]\nsha256 = \"" + strings.Repeat("b", 64) + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TerminalExtensions.Herdr == nil || len(cfg.TerminalExtensions.Herdr.Command) != 1 || cfg.TerminalExtensions.Herdr.Command[0] != executable {
		t.Fatalf("extension = %+v", cfg.TerminalExtensions.Herdr)
	}
}

func TestNotificationExtensionRequiresExplicitEnableAbsoluteCommandAndDigest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	executable := filepath.Join(root, "shephrd-notification-macos")
	body := "[notifications]\nenabled = true\n[notifications.extension]\ncommand = [" + fmt.Sprintf("%q", executable) + "]\nsha256 = \"" + strings.Repeat("c", 64) + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notifications.Enabled || cfg.Notifications.Extension == nil || len(cfg.Notifications.Extension.Command) != 1 || cfg.Notifications.Extension.Command[0] != executable {
		t.Fatalf("notifications = %+v", cfg.Notifications)
	}
}

func TestNotificationExtensionRejectsMissingTrustAndLegacyAdapter(t *testing.T) {
	for name, body := range map[string]string{
		"missing extension":   "[notifications]\nenabled = true\n",
		"relative executable": "[notifications]\nenabled = true\n[notifications.extension]\ncommand = [\"relative\"]\nsha256 = \"" + strings.Repeat("a", 64) + "\"\n",
		"legacy adapter":      "[notifications]\nadapter = \"macos\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SHEPHRD_CONFIG", path)
			t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
			if _, err := Load(); err == nil {
				t.Fatal("invalid notification configuration was accepted")
			}
		})
	}
}

func TestCmuxExtensionRejectsPartialTrustConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[terminal_extensions.cmux]\ncommand = [\"relative\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("error = %v", err)
	}
}

func TestReportAcceptedLifecycleHandlersRequireExplicitTrustedIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	executable := filepath.Join(root, "report-handler")
	body := fmt.Sprintf(`[[lifecycle_handlers.report_accepted]]
name = "memory"
extension_id = "example.report-memory"
command = [%q, %q]
sha256 = %q
environment = ["MEMORY_TOKEN"]
`, executable, filepath.Join(root, "memory"), strings.Repeat("a", 64))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	handlers := cfg.LifecycleHandlers.ReportAccepted
	if len(handlers) != 1 || handlers[0].Name != "memory" || handlers[0].ExtensionID != "example.report-memory" || len(handlers[0].Command) != 2 || len(handlers[0].Environment) != 1 {
		t.Fatalf("handlers = %+v", handlers)
	}
}

func TestReportAcceptedLifecycleHandlersRejectUnsafeOrAmbiguousConfiguration(t *testing.T) {
	for name, body := range map[string]string{
		"relative executable": `[[lifecycle_handlers.report_accepted]]
name="memory"
extension_id="example.memory"
command=["./handler"]
sha256="` + strings.Repeat("a", 64) + `"`,
		"duplicate handler": `[[lifecycle_handlers.report_accepted]]
name="memory"
extension_id="example.memory"
command=["/handler"]
sha256="` + strings.Repeat("a", 64) + `"
[[lifecycle_handlers.report_accepted]]
name="memory"
extension_id="example.other"
command=["/other"]
sha256="` + strings.Repeat("b", 64) + `"`,
		"shephrd environment": `[[lifecycle_handlers.report_accepted]]
name="memory"
extension_id="example.memory"
command=["/handler"]
sha256="` + strings.Repeat("a", 64) + `"
environment=["SHEPHRD_CONFIG"]`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SHEPHRD_CONFIG", path)
			t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "lifecycle_handlers.report_accepted") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestInvalidWorkerRuntimeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("worker_runtime = \"tmux\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHEPHRD_CONFIG", path)
	t.Setenv("SHEPHRD_WORKER_RUNTIME", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "headless, auto, herdr, or cmux") {
		t.Fatalf("error = %v", err)
	}
}
