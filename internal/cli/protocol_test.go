package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/adapter"
)

func TestProtocolPreflightBuiltCLIReadOnly(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "shephrd")
	build := exec.Command("go", "build", "-o", binary, "./cmd/shephrd")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"reported","checkpoint":{"schema_version":1,"summary":"reported","completed":[],"next_steps":[],"decisions":[{"decision":"Retain worker","reason":"Already dispatched"}],"changed_paths":[],"checks":[{"command":"inspect","result":"running"}],"blockers":[]}}</shephrd-event>`
	done := `<shephrd-event>{"type":"done","payload":"finished","artifact":"branch:shephrd/task"}</shephrd-event>`
	subdriverDone := `<shephrd-event>{"type":"done","payload":"turn handled"}</shephrd-event>`
	badDecisions := strings.Replace(checkpoint, `[{"decision":"Retain worker","reason":"Already dispatched"}]`, `["Retain worker"]`, 1)
	badChecks := strings.Replace(checkpoint, `[{"command":"inspect","result":"running"}]`, `["running"]`, 1)
	cases := []struct {
		name, role, text, code, field, shape string
	}{
		{name: "worker", role: "worker", text: checkpoint + done},
		{name: "subdriver", role: "subdriver", text: checkpoint + subdriverDone},
		{name: "legacy-role", role: "coordinator", text: checkpoint + subdriverDone},
		{name: "legacy-artifact-forbidden", role: "coordinator", text: checkpoint + done, code: adapter.DiagnosticSemanticInvalid},
		{name: "decisions", role: "subdriver", text: badDecisions + subdriverDone, code: adapter.DiagnosticSchemaTypeMismatch, field: "checkpoint.decisions[]", shape: `{"decision":"choice made","reason":"why"}`},
		{name: "checks", role: "worker", text: badChecks + done, code: adapter.DiagnosticSchemaTypeMismatch, field: "checkpoint.checks[]", shape: `{"command":"test command","result":"observed result"}`},
		{name: "unknown", role: "worker", text: strings.Replace(done, `"type":`, `"unknown":true,"type":`, 1), code: adapter.DiagnosticSchemaUnknownField},
		{name: "trailing", role: "worker", text: strings.Replace(done, `</shephrd-event>`, ` {} </shephrd-event>`, 1), code: adapter.DiagnosticJSONTrailing},
		{name: "missing-close", role: "worker", text: strings.TrimSuffix(done, `</shephrd-event>`), code: adapter.DiagnosticFramingMissingClose},
		{name: "nested", role: "worker", text: `<shephrd-event>{` + done, code: adapter.DiagnosticFramingInvalid},
		{name: "terminal-order", role: "worker", text: done + checkpoint, code: adapter.DiagnosticSequenceInvalid},
		{name: "duplicate-terminal", role: "subdriver", text: checkpoint + subdriverDone + subdriverDone, code: adapter.DiagnosticSequenceInvalid},
		{name: "worker-artifact-required", role: "worker", text: checkpoint + subdriverDone, code: adapter.DiagnosticSemanticInvalid},
		{name: "subdriver-artifact-forbidden", role: "subdriver", text: checkpoint + done, code: adapter.DiagnosticSemanticInvalid},
		{name: "subdriver-bound", role: "subdriver", text: strings.Replace(checkpoint, `"summary":"reported"`, `"summary":"`+strings.Repeat("x", 4000)+`"`, 1) + subdriverDone, code: adapter.DiagnosticSemanticInvalid},
		{name: "absent", role: "worker", text: "ordinary prose <shephrd-event>", code: adapter.DiagnosticFramingInvalid},
		{name: "envelope-bound", role: "worker", text: `<shephrd-event>{` + strings.Repeat("x", 96*1024), code: adapter.DiagnosticFramingInvalid},
		{name: "input-bound", role: "worker", text: strings.Repeat("x", protocolInputMaxBytes+1), code: adapter.DiagnosticFramingInvalid},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "invalid-config.toml")
			if err := os.WriteFile(config, []byte("not valid TOML ["), 0600); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(dir, "authored.txt")
			if err := os.WriteFile(input, []byte(test.text), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadDir(dir)
			for _, file := range []string{"-", input} {
				cmd := exec.Command(binary, "protocol", "validate", "--role", test.role, "--file", file, "--json")
				cmd.Stdin = strings.NewReader(test.text)
				cmd.Env = compoundCLIEnvironment(os.Environ(), map[string]string{
					"HOME": dir, "XDG_CONFIG_HOME": filepath.Join(dir, "config"), "XDG_STATE_HOME": filepath.Join(dir, "state"),
					"SHEPHRD_CONFIG": config, "SHEPHRD_STATE_DIR": filepath.Join(dir, "state"), "SHEPHRD_DATA_DIR": filepath.Join(dir, "data"),
					"SHEPHRD_WORKER": "1", "SHEPHRD_SUBDRIVER_ID": "not-live", "SHEPHRD_SUBDRIVER_GENERATION": "1", "SHEPHRD_SUBDRIVER_TOKEN": "invalid",
				})
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				var response struct {
					Valid      bool                `json:"valid"`
					Scope      string              `json:"scope"`
					Role       string              `json:"role"`
					EventCount int                 `json:"event_count"`
					Diagnostic *adapter.Diagnostic `json:"diagnostic"`
				}
				if decode := json.Unmarshal(stdout.Bytes(), &response); decode != nil {
					t.Fatalf("decode: %s %s %v", stdout.String(), stderr.String(), decode)
				}
				if response.Role != test.role || response.Scope != "format-only" || response.Valid != (test.code == "") || (err == nil) != response.Valid {
					t.Fatalf("response=%+v err=%v stderr=%s", response, err, stderr.String())
				}
				if test.code == "" {
					if response.EventCount != 2 {
						t.Fatalf("response=%+v", response)
					}
				} else if response.EventCount != 0 || response.Diagnostic == nil || response.Diagnostic.Code != test.code || test.field != "" && (response.Diagnostic.Field != test.field || !strings.Contains(response.Diagnostic.Message, test.shape) || !strings.Contains(stderr.String(), test.field)) {
					t.Fatalf("diagnostic=%+v stderr=%s", response.Diagnostic, stderr.String())
				}
			}
			after, _ := os.ReadDir(dir)
			unchanged, _ := os.ReadFile(input)
			configBody, _ := os.ReadFile(config)
			if len(before) != len(after) || string(unchanged) != test.text || string(configBody) != "not valid TOML [" {
				t.Fatal("preflight mutated input, configuration or state")
			}
		})
	}
}
