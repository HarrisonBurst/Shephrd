package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"shephrd/internal/cli"
	"shephrd/internal/testkit"
)

func surfaceClient(t *testing.T, home *testkit.Env, name string) *testkit.Env {
	t.Helper()
	client := testkit.NewEnv(t)
	client.OK("init", "--home", "me@workhorse")
	client.InstallSSH(map[string]testkit.SSHTarget{
		"me@workhorse": {Command: []string{home.Bin, "serve", "--as", "surface:" + name}, Home: home.Home},
	})
	return client
}

func TestSurfaceSSHIdentityIsolationAndManualMode(t *testing.T) {
	m := threeMachines(t)
	larry := surfaceClient(t, m.home, "larry")
	phone := surfaceClient(t, m.home, "phone")
	larry.Refused("surface_only", "task", "list")
	larry.Refused("surface_only", "task", "create", "--objective", "forged")
	larry.Refused("surface_only", "inbox")
	larry.Refused("surface_only", "inbox", "ack", "1")
	larry.Refused("surface_only", "events")
	larry.Refused("surface_only", "repo", "list")
	larry.Refused("surface_only", "grant", "land", "--to", "t_1")
	larry.Refused("surface_only", "arbitrary-plugin-command")
	larry.Refused("surface_only", "main", "pending")
	larry.Refused("as_not_allowed", "surface", "replies", "--thread", "room", "--as", "driver:main")
	m.client.Refused("surface_only", "surface", "submit", "--thread", "room", "hi")
	m.client.Refused("operator_only", "main", "pending")
	first := larry.Run("hello from Matrix", "surface", "submit", "--thread", "room", "-", "--key", "event-1")
	if first.Code != 0 || first.JSON(t)["surface"] != "larry" || first.JSON(t)["id"] != float64(1) {
		t.Fatalf("submit: %+v", first)
	}
	if retry := larry.Run("hello from Matrix", "surface", "submit", "--thread", "room", "-", "--key", "event-1"); retry.Code != 0 || retry.Stdout != first.Stdout {
		t.Fatalf("retry: %+v", retry)
	}
	larry.Refused("key_conflict", "surface", "submit", "--thread", "room", "different", "--key", "event-1")
	phone.OK("surface", "submit", "--thread", "room", "independent", "--key", "event-1")
	phone.OK("surface", "submit", "--thread", "other", "separate")
	inputs := m.home.OK("main", "pending")["inputs"].([]any)
	if len(inputs) != 3 || inputs[0].(map[string]any)["body"] != "hello from Matrix" || inputs[1].(map[string]any)["surface"] != "phone" {
		t.Fatalf("ordered input: %v", inputs)
	}
	if tasks := m.home.OK("task", "list")["tasks"].([]any); len(tasks) != 0 {
		t.Fatalf("surface modified tasks: %v", tasks)
	}
	m.home.OK("main", "reply", "1", "received", "--key", "reply-1")
	m.home.OK("main", "reply", "2", "private", "--key", "reply-2")
	m.home.OK("main", "reply", "3", "other thread", "--key", "reply-3")
	m.home.Refused("already_answered", "main", "reply", "1", "another")
	larryReplies := larry.OK("surface", "replies", "--thread", "room")["replies"].([]any)
	if len(larryReplies) != 1 || larryReplies[0].(map[string]any)["body"] != "received" {
		t.Fatalf("larry replies: %v", larryReplies)
	}
	if got := larry.OK("surface", "replies", "--thread", "room")["replies"].([]any); !reflect.DeepEqual(got, larryReplies) {
		t.Fatalf("reply lost across reconnect: %v", got)
	}
	if got := larry.OK("surface", "replies", "--thread", "other")["replies"].([]any); len(got) != 0 {
		t.Fatalf("foreign thread: %v", got)
	}
	if got := phone.OK("surface", "replies", "--thread", "room")["replies"].([]any); len(got) != 1 || got[0].(map[string]any)["body"] != "private" {
		t.Fatalf("phone replies: %v", got)
	}
	larry.OK("surface", "submit", "--thread", "room", "again")
	m.home.OK("main", "reply", "4", "next")
	got := larry.OK("surface", "replies", "--thread", "room", "--after", "1")["replies"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["input"] != float64(4) {
		t.Fatalf("reply cursor: %v", got)
	}
	if got := larry.OK("surface", "replies", "--thread", "room", "--after", "4")["replies"].([]any); len(got) != 0 {
		t.Fatalf("advanced cursor: %v", got)
	}
}

func TestSurfaceForcedCommandRejectsIdentityTokens(t *testing.T) {
	for _, token := range []string{`"run_token":"forged"`, `"call_token":"forged"`} {
		var out, stderr bytes.Buffer
		request := `{"v":` + fmt.Sprint(cli.Protocol) + `,"argv":["surface","submit","--thread","room","hi"],"key":"key",` + token + `}`
		code := cli.Main(context.Background(), []string{"serve", "--as", "surface:larry"}, strings.NewReader(""), &out, &stderr, func(key string) string {
			if key == "SSH_ORIGINAL_COMMAND" {
				return request
			}
			return ""
		})
		if code != 1 || !strings.Contains(stderr.String(), `"kind":"invalid_token"`) || out.Len() != 0 {
			t.Fatalf("token accepted: %d %s %s", code, out.String(), stderr.String())
		}
	}
}

func TestSurfaceDroppedSSHRetryAndInputBounds(t *testing.T) {
	home := initialized(t)
	client := surfaceClient(t, home, "larry")
	client.SSHDropOnce("me@workhorse")
	out := client.Run("once", "surface", "submit", "--thread", "room", "-", "--key", "redelivery")
	if out.Code != 0 || out.JSON(t)["id"] != float64(1) {
		t.Fatalf("lost response retry: %+v", out)
	}
	if inputs := home.OK("main", "pending")["inputs"].([]any); len(inputs) != 1 {
		t.Fatalf("duplicate input after SSH retry: %v", inputs)
	}
	client.Refused("too_large", "surface", "submit", "--thread", "room", strings.Repeat("a", 8193))
	client.Refused("usage", "surface", "submit", "--thread", "room", " ")
	client.Refused("usage", "surface", "submit", "--thread", "UPPER", "hi")
	client.Refused("usage", "surface", "replies", "--thread", "room", "--after", "-1")
	home.Refused("too_large", "main", "reply", "1", strings.Repeat("b", 8193))
	for i := 0; i < 105; i++ {
		client.OK("surface", "submit", "--thread", "room", fmt.Sprint(i))
	}
	if inputs := home.OK("main", "pending")["inputs"].([]any); len(inputs) != 100 {
		t.Fatalf("unbounded page: %d", len(inputs))
	}
	if inputs := home.OK("main", "pending", "--after", "100")["inputs"].([]any); len(inputs) != 6 {
		t.Fatalf("next page: %d", len(inputs))
	}
}
