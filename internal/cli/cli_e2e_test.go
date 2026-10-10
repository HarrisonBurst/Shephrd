package cli_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"shephrd/internal/cli"
	"shephrd/internal/fault"
	"shephrd/internal/testkit"
)

func TestMain(m *testing.M) {
	testkit.Main(m)
}

func initialized(t *testing.T) *testkit.Env {
	t.Helper()
	env := testkit.NewEnv(t)
	out := env.OK("init", "--host", "workhorse")
	if out["created"] != true || out["host"] != "workhorse" {
		t.Fatalf("init: %v", out)
	}
	return env
}

func TestInitCreatesConfigurationAndStoreOnce(t *testing.T) {
	env := testkit.NewEnv(t)
	first := env.OK("init", "--host", "workhorse")
	if first["config"] != env.ConfigPath() || first["created"] != true {
		t.Fatalf("first init: %v", first)
	}
	if _, err := os.Stat(first["store"].(string)); err != nil {
		t.Fatalf("store not created: %v", err)
	}
	body, err := os.ReadFile(env.ConfigPath())
	if err != nil || !strings.Contains(string(body), `driver = "main"`) {
		t.Fatalf("config: %v\n%s", err, body)
	}
	second := env.OK("init")
	if second["created"] != false || second["store"] != first["store"] {
		t.Fatalf("second init: %v", second)
	}
}

func TestVersionNeedsNoConfiguration(t *testing.T) {
	out := testkit.NewEnv(t).OK("version")
	if out["protocol"] != float64(cli.Protocol) || out["version"] == "" {
		t.Fatalf("version: %v", out)
	}
}

func TestCommandsBeforeInitNameTheWayForward(t *testing.T) {
	refusal := testkit.NewEnv(t).Refused("not_initialized", "repo", "list")
	if !reflect.DeepEqual(refusal["next"], []any{[]any{"shephrd", "init"}}) {
		t.Fatalf("next: %v", refusal["next"])
	}
}

func TestRepoAddAndList(t *testing.T) {
	env := initialized(t)
	repo := testkit.GitRepo(t, "Tool")
	added := env.OK("repo", "add", repo)
	want := map[string]any{"id": "r_1", "host": "workhorse", "path": repo, "name": "tool", "default_branch": "main"}
	for key, value := range want {
		if added[key] != value {
			t.Fatalf("repo add %s = %v, want %v: %v", key, added[key], value, added)
		}
	}
	listed := env.OK("repo", "list")["repos"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["path"] != repo {
		t.Fatalf("repo list: %v", listed)
	}
	env.Refused("repo_exists", "repo", "add", repo, "--name", "other")
	env.Refused("repo_exists", "repo", "add", testkit.GitRepo(t, "elsewhere"), "--name", "tool")
}

func TestRepoAddRefusesWhatIsNotARepositoryRoot(t *testing.T) {
	env := initialized(t)
	repo := testkit.GitRepo(t, "tool")
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	env.Refused("not_a_repository", "repo", "add", sub)
	env.Refused("not_a_repository", "repo", "add", t.TempDir())
	env.Refused("invalid_branch", "repo", "add", repo, "--default-branch", "trunk")
}

func TestMutationIsIdempotentByKey(t *testing.T) {
	env := initialized(t)
	repo := testkit.GitRepo(t, "tool")
	first := env.Run("", "repo", "add", repo, "--key", "k1")
	second := env.Run("", "repo", "add", repo, "--key", "k1")
	if first.Code != 0 || second.Code != 0 || first.Stdout != second.Stdout {
		t.Fatalf("repeat with the same key:\n%+v\n%+v", first, second)
	}
	env.Refused("key_conflict", "repo", "add", testkit.GitRepo(t, "other"), "--key", "k1")
}

func TestLongInputComesFromStdinWithinBounds(t *testing.T) {
	env := initialized(t)
	added := env.Run("make setup\n", "repo", "add", testkit.GitRepo(t, "tool"), "--setup", "-")
	if added.Code != 0 || added.JSON(t)["setup"] != "make setup\n" {
		t.Fatalf("setup from stdin: %+v", added)
	}
	env.Run(strings.Repeat("x", 4097), "repo", "add", testkit.GitRepo(t, "big"), "--setup", "-").Refusal(t, "too_large")
}

func TestAsIsRefusedForRunsAndPlugins(t *testing.T) {
	env := initialized(t)
	env.Vars["SHEPHRD_RUN_TOKEN"] = "token"
	env.Refused("as_not_allowed", "repo", "list", "--as", "driver:main")
	delete(env.Vars, "SHEPHRD_RUN_TOKEN")
	env.Vars["SHEPHRD_CALL_TOKEN"] = "token"
	env.Refused("as_not_allowed", "repo", "list", "--as", "driver:main")
}

func TestOperatorMayActAsAnyDriverLocally(t *testing.T) {
	env := initialized(t)
	env.OK("repo", "list", "--as", "driver:other")
	env.Refused("usage", "repo", "list", "--as", "plugin:linear")
}

func TestUnknownCommandsAndFlagsAreUsageRefusals(t *testing.T) {
	env := initialized(t)
	env.Refused("plugin_unknown", "bogus")
	env.Refused("usage", "repo", "list", "--bogus")
	env.Refused("usage", "repo")
}

func TestHelpIsLocalText(t *testing.T) {
	r := testkit.NewEnv(t).Run("", "repo", "--help")
	if r.Code != 0 || !strings.Contains(r.Stdout, "Register and list repositories") {
		t.Fatalf("help: %+v", r)
	}
}

func TestRemoteRequestsAreNeverOperators(t *testing.T) {
	env := initialized(t)
	getenv := func(key string) string {
		if key == "HOME" {
			return env.Home
		}
		return ""
	}
	req := cli.Request{V: cli.Protocol, Argv: []string{"repo", "add", testkit.GitRepo(t, "tool")}, Key: "k"}
	_, err := cli.Dispatch(context.Background(), cli.Origin{As: "driver:main"}, req, getenv, io.Discard)
	if fault.As(err).Kind != "operator_only" {
		t.Fatalf("remote repo add: %v", err)
	}
	req.Argv = []string{"repo", "list", "--as", "driver:main"}
	_, err = cli.Dispatch(context.Background(), cli.Origin{As: "driver:main"}, req, getenv, io.Discard)
	if fault.As(err).Kind != "usage" {
		t.Fatalf("--as in remote argv: %v", err)
	}
	req.V = 2
	_, err = cli.Dispatch(context.Background(), cli.Origin{As: "driver:main"}, req, getenv, io.Discard)
	if fault.As(err).Kind != "version_mismatch" {
		t.Fatalf("protocol 2: %v", err)
	}
}

func TestUnknownOutcomeExitsThree(t *testing.T) {
	code := cli.WriteError(io.Discard, &fault.Error{Kind: "transport_failed", Message: "connection lost", Unknown: true})
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
}
