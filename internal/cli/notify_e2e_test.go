package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"shephrd/internal/notify"
	"shephrd/internal/testkit"
)

type webhook struct {
	mu       sync.Mutex
	status   int
	received []map[string]any
	secret   string
	server   *httptest.Server
	bad      []string
}

func newWebhook(t *testing.T, env *testkit.Env, driver string) *webhook {
	w := &webhook{status: 200, secret: "test-secret"}
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		defer w.mu.Unlock()
		want := notify.Signature([]byte(w.secret), r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), body)
		if r.Header.Get("webhook-signature") != want {
			w.bad = append(w.bad, string(body))
		}
		var payload map[string]any
		json.Unmarshal(body, &payload)
		w.received = append(w.received, payload)
		rw.WriteHeader(w.status)
	}))
	t.Cleanup(w.server.Close)
	secret := filepath.Join(env.Home, "webhook-secret")
	os.WriteFile(secret, []byte(w.secret), 0o600)
	env.AppendConfig("[drivers." + driver + ".delivery]\nprovider = \"webhook\"\nurl = \"" + w.server.URL + "\"\nsecret_file = \"" + secret + "\"")
	return w
}

func (w *webhook) items() []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.received...)
}

func withDaemon(t *testing.T) (*testkit.Env, string) {
	env, repo := withHarness(t)
	env.AppendConfig("[timeouts]\nsettle = \"500ms\"")
	return env, repo
}

func TestDaemonNotRunningIsWarned(t *testing.T) {
	env, _ := withHarness(t)
	if r := env.Run("", "repo", "list"); !strings.Contains(r.Stdout, "daemon_not_running") {
		t.Fatalf("no warning without a daemon: %s", r.Stdout)
	}
	env.StartDaemon()
	if r := env.Run("", "repo", "list"); strings.Contains(r.Stdout, "daemon_not_running") {
		t.Fatalf("warning with a daemon: %s", r.Stdout)
	}
	env.Refused("daemon_running", "daemon")
}

// The first full loop: a sub-driver plans two workers, is woken once when
// both report, reports its request's result, and the main driver receives
// it by push and acknowledges it.
func TestSubDriverLoopEndsInTheMainDriversInbox(t *testing.T) {
	env, _ := withDaemon(t)
	hook := newWebhook(t, env, "main")
	env.Script(map[string][][]action{
		"t_1": {
			{
				{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Build part one"}},
				{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Build part two"}},
				{"shephrd": []string{"task", "start", "t_2"}},
				{"shephrd": []string{"task", "start", "t_3"}},
				{"report": []string{"note", "Plan: t_2 and t_3 in parallel. Waiting on both."}},
			},
			{
				{"report": []string{"result", "Both parts are built."}},
				{"report": []string{"note", "Request done."}},
			},
		},
		"t_2": {{{"work": "Part one"}, {"report": []string{"result", "Part one built"}}}},
		"t_3": {{{"work": "Part two"}, {"report": []string{"result", "Part two built"}}}},
	})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Build the feature")
	env.OK("task", "start", "t_1")
	env.WaitState("t_2", "done")
	env.WaitState("t_3", "done")
	if errs := commandErrors(env, "t_1"); len(errs) != 0 {
		t.Fatalf("sub-driver commands failed: %v", errs)
	}

	env.StartDaemon()
	env.WaitState("t_1", "done")
	second := callsFor(env, "t_1", 1)
	if len(second) != 1 || !strings.Contains(second[0]["brief"].(string), "Part one built") || !strings.Contains(second[0]["brief"].(string), "Part two built") {
		t.Fatalf("continuation brief: %v", second)
	}
	if len(callsFor(env, "t_1", 2)) != 0 {
		t.Fatal("the sub-driver was woken more than once")
	}

	var items []any
	env.Eventually("the result in the inbox", func() bool {
		items = env.OK("inbox")["items"].([]any)
		return len(items) == 1
	})
	item := items[0].(map[string]any)
	if item["task"] != "t_1" || item["event"].(map[string]any)["name"] != "task.result" {
		t.Fatalf("inbox item: %v", item)
	}
	env.Eventually("the push", func() bool { return len(hook.items()) == 1 })
	if len(hook.bad) != 0 {
		t.Fatalf("unsigned deliveries: %v", hook.bad)
	}
	pushed := hook.items()[0]["item"].(map[string]any)
	if pushed["id"] != item["id"] || pushed["next"].(map[string]any)["ack"] == nil {
		t.Fatalf("pushed: %v", pushed)
	}
	env.OK("inbox", "ack", "1")
	env.OK("inbox", "ack", "1")
	if items := env.OK("inbox")["items"].([]any); len(items) != 0 {
		t.Fatalf("after ack: %v", items)
	}
	if items := env.OK("inbox", "--all")["items"].([]any); items[0].(map[string]any)["delivery"].(map[string]any)["state"] != "delivered" {
		t.Fatalf("delivery state: %v", items)
	}
}

func TestInterimNotifyReachesOwnerWithoutFinishingWork(t *testing.T) {
	env, _ := withDaemon(t)
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"progress", "Still working"}}, {"report": []string{"notify", "Preview is ready; final review is pending"}}, {"shephrd": []string{"_turn"}}, {"report": []string{"notify", "Invalid target", "--request", "1"}}, {"wait": "finish"}, {"work": "Finished"}, {"report": []string{"result", "Final review complete"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Review")
	env.OK("task", "start", "t_1")
	env.Eventually("interim owner item", func() bool { return len(env.OK("inbox")["items"].([]any)) == 1 })
	if task := env.Task("t_1"); task["state"] != "running" {
		t.Fatalf("interim report finished worker: %v", task)
	}
	if brief := callsFor(env, "t_1", 0)[0]["brief"].(string); !strings.Contains(brief, "report notify -") {
		t.Fatalf("worker brief omitted interim guidance: %s", brief)
	}
	os.WriteFile(filepath.Join(env.Home, ".fakeharness", "finish"), nil, 0o600)
	env.WaitState("t_1", "done")
	if errs := commandErrors(env, "t_1"); len(errs) != 1 || !strings.Contains(errs[0], `"kind":"usage"`) {
		t.Fatalf("worker request target should be refused: %v", errs)
	}
	var turn map[string]any
	for _, call := range env.Calls() {
		if cmd, ok := call["command"].([]any); ok && len(cmd) > 1 && cmd[1] == "_turn" {
			turn = call
		}
	}
	if turn == nil || !strings.Contains(turn["stdout"].(string), `"ended":false`) {
		t.Fatalf("interim report ended the turn: %v", turn)
	}
	items := env.OK("inbox")["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["event"].(map[string]any)["name"] != "task.notify" ||
		items[1].(map[string]any)["event"].(map[string]any)["name"] != "task.result" {
		t.Fatalf("progress should stay quiet; interim report and final result should notify: %v", items)
	}
}

func TestDriverNotifiesWithSeveralOpenRequests(t *testing.T) {
	env, _ := withDaemon(t)
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Supervise")
	first := strconv.Itoa(int(env.OK("task", "show", "t_1")["requests"].([]any)[0].(map[string]any)["seq"].(float64)))
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Foreign")
	foreign := strconv.Itoa(int(env.OK("task", "show", "t_2")["requests"].([]any)[0].(map[string]any)["seq"].(float64)))
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"result", "Initial answer"}}, {"report": []string{"note", "Waiting for more work"}}}}})
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	second := strconv.Itoa(int(env.OK("task", "send", "t_1", "Second request")["seq"].(float64)))
	env.OK("task", "send", "t_1", "Third request")
	env.Script(map[string][][]action{"t_1": {
		{},
		{
			{"report": []string{"progress", "Working on both"}},
			{"report": []string{"notify", "Shared preview ready"}},
			{"report": []string{"notify", "Second request preview ready", "--request", second}},
			{"report": []string{"notify", "First answer FYI", "--request", first}},
			{"report": []string{"notify", "Wrong owner", "--request", foreign}},
			{"report": []string{"notify", "Unknown", "--request", "999999"}},
			{"report": []string{"note", "Both requests still open"}},
		},
	}})
	env.StartDaemon()
	waitStateFromDaemon(t, env, "t_1", "running")
	if errs := commandErrors(env, "t_1"); len(errs) != 2 || !strings.Contains(errs[0], `"kind":"invalid_request"`) || !strings.Contains(errs[1], `"kind":"invalid_request"`) {
		t.Fatalf("foreign and unknown targets should be refused: %v", errs)
	}
	shown := env.OK("task", "show", "t_1")
	if shown["task"].(map[string]any)["state"] != "running" || len(shown["requests"].([]any)) != 3 {
		t.Fatalf("notification changed task or request count: %v", shown)
	}
	for i, req := range shown["requests"].([]any) {
		state := req.(map[string]any)["state"]
		if i == 0 && state != "answered" || i != 0 && state != "open" {
			t.Fatalf("notification changed request %d: %v", i, req)
		}
	}
	items := env.OK("inbox")["items"].([]any)
	for i, want := range []string{"", second, first} {
		event := items[i+1].(map[string]any)["event"].(map[string]any)
		data := event["data"].(map[string]any)
		if event["name"] != "task.notify" || (want == "" && data["request"] != nil) || (want != "" && (data["request"] == nil || strconv.Itoa(int(data["request"].(float64))) != want)) ||
			items[i+1].(map[string]any)["next"].(map[string]any)["reply"] != nil {
			t.Fatalf("notify %d: %v", i, items[i+1])
		}
	}
}

func TestNotifyOutcomeAfterOwnerReplyDoesNotAnswerRequest(t *testing.T) {
	env, _ := withDaemon(t)
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Complete the review")
	request := env.OK("task", "show", "t_1")["requests"].([]any)[0].(map[string]any)["seq"]
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"question", "May I share the interim review?"}}, {"report": []string{"note", "Waiting for approval"}}}}})
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "waiting")
	question := questionSeq(env, "t_1")
	reply := env.OK("task", "send", "t_1", "Approved; continue the review", "--reply-to", question)["seq"]
	replySeq := strconv.Itoa(int(reply.(float64)))
	env.Script(map[string][][]action{"t_1": {
		{},
		{
			{"report": []string{"notify", "Approval received; interim findings shared", "--request", replySeq}},
			{"report": []string{"notify", "Approval received; interim findings shared"}},
			{"report": []string{"note", "Review still in progress"}},
		},
	}})
	env.StartDaemon()
	env.Eventually("outcome in owner's inbox", func() bool { return len(env.OK("inbox")["items"].([]any)) == 2 })
	if errs := commandErrors(env, "t_1"); len(errs) != 1 || !strings.Contains(errs[0], `"kind":"invalid_request"`) {
		t.Fatalf("reply sequence accepted as a request: %v", errs)
	}
	shown := env.OK("task", "show", "t_1")
	requests := shown["requests"].([]any)
	if shown["task"].(map[string]any)["state"] != "running" || len(requests) != 1 || requests[0].(map[string]any)["seq"] != request || requests[0].(map[string]any)["state"] != "open" {
		t.Fatalf("outcome changed request or task state: %v", shown)
	}
	items := env.OK("inbox")["items"].([]any)
	event := items[1].(map[string]any)["event"].(map[string]any)
	data := event["data"].(map[string]any)
	if event["name"] != "task.notify" || data["request"] != nil || data["body"] != "Approval received; interim findings shared" {
		t.Fatalf("unscoped outcome not delivered: %v", items[1])
	}
}

func TestChildNotifyWakesSubDriverWithoutFinishingChild(t *testing.T) {
	env, _ := withDaemon(t)
	env.Script(map[string][][]action{
		"t_1": {
			{{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Investigate"}}, {"shephrd": []string{"task", "start", "t_2"}}, {"report": []string{"note", "Waiting for child"}}},
			{{"report": []string{"note", "Preview received; waiting for final result"}}},
			{{"report": []string{"result", "Final investigation received"}}, {"report": []string{"note", "Done"}}},
		},
		"t_2": {{{"report": []string{"progress", "Internal milestone"}}, {"report": []string{"notify", "Interim findings ready"}}, {"wait": "child-finish"}, {"work": "Investigated"}, {"report": []string{"result", "Final findings"}}}},
	})
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Supervise")
	env.OK("task", "start", "t_1")
	env.Eventually("child interim event", func() bool {
		shown := env.Run("", "task", "show", "t_2")
		if shown.Code != 0 {
			return false
		}
		for _, entry := range shown.JSON(t)["events"].([]any) {
			if entry.(map[string]any)["name"] == "task.notify" {
				return true
			}
		}
		return false
	})
	env.StartDaemon()
	env.Eventually("sub-driver continuation", func() bool { return len(callsFor(env, "t_1", 1)) == 1 })
	brief := callsFor(env, "t_1", 1)[0]["brief"].(string)
	if !strings.Contains(brief, "report notify -") || !strings.Contains(brief, "task.notify on t_2") || !strings.Contains(brief, "Interim findings ready") || strings.Contains(brief, "Internal milestone") {
		t.Fatalf("continuation brief: %s", brief)
	}
	if task := env.Task("t_2"); task["state"] != "running" {
		t.Fatalf("interim report finished child: %v", task)
	}
	os.WriteFile(filepath.Join(env.Home, ".fakeharness", "child-finish"), nil, 0o600)
	env.WaitState("t_2", "done")
	env.WaitState("t_1", "done")
	if errs := commandErrors(env, "t_1"); len(errs) != 0 {
		t.Fatalf("sub-driver continuation failed: %v", errs)
	}
}

func TestAnsweredDriverRequestCanAskAgain(t *testing.T) {
	env, _ := withDaemon(t)
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Supervise")
	request := strconv.Itoa(int(env.OK("task", "show", "t_1")["requests"].([]any)[0].(map[string]any)["seq"].(float64)))
	env.Script(map[string][][]action{
		"t_1": {
			{
				{"shephrd": []string{"task", "create", "--repo", "api", "--objective", "Investigate", "--deliverable", "report"}},
				{"shephrd": []string{"task", "start", "t_2"}},
				{"report": []string{"result", "Initial answer"}},
				{"report": []string{"note", "Waiting on child"}},
			},
			{
				{"report": []string{"question", "Which behavior?"}},
				{"report": []string{"question", "Which behavior?", "--request", request}},
				{"report": []string{"note", "Waiting for owner"}},
			},
			{
				{"report": []string{"result", "Final answer"}},
				{"report": []string{"note", "Finished"}},
			},
		},
		"t_2": {{{"report": []string{"question", "Which behavior?"}}}, {{"report": []string{"result", "Investigated"}}}},
	})
	env.OK("task", "start", "t_1")
	env.WaitState("t_2", "waiting")
	env.StartDaemon()
	env.WaitState("t_1", "waiting")
	if errs := commandErrors(env, "t_1"); len(errs) != 1 || !strings.Contains(errs[0], `"kind":"request_required"`) {
		t.Fatalf("implicit follow-up should fail, explicit should succeed: %v", errs)
	}
	items := env.OK("inbox")["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected initial result and follow-up question: %v", items)
	}
	question := items[1].(map[string]any)
	event := question["event"].(map[string]any)
	if event["name"] != "task.question" || event["data"].(map[string]any)["request"] != float64(2) || question["task"] != "t_1" {
		t.Fatalf("follow-up question: %v", question)
	}
	shown := env.OK("task", "show", "t_1")
	requests := shown["requests"].([]any)
	var original map[string]any
	for _, entry := range shown["events"].([]any) {
		event := entry.(map[string]any)
		if event["name"] == "task.result" {
			original = event
		}
	}
	if original == nil {
		t.Fatal("initial result event missing")
	}
	oldArtifact := original["data"].(map[string]any)["artifact"].(string)
	if req := requests[0].(map[string]any); req["state"] != "asked" || req["result"] != nil {
		t.Fatalf("question did not clear current answer: %v", req)
	}
	env.Refused("not_owner", "task", "send", "t_2", "Do this")
	reply := toStrings(question["next"].(map[string]any)["reply"])
	if out := env.Run("Use v2", reply[1:]...); out.Code != 0 {
		t.Fatalf("owner reply: %+v", out)
	}
	env.Eventually("reply to reopen the request", func() bool {
		req := env.OK("task", "show", "t_1")["requests"].([]any)[0].(map[string]any)
		return req["state"] == "open"
	})
	env.WaitState("t_1", "done")
	if errs := commandErrors(env, "t_1"); len(errs) != 1 {
		t.Fatalf("follow-up result failed: %v", errs)
	}
	shown = env.OK("task", "show", "t_1")
	var results []map[string]any
	for _, entry := range shown["events"].([]any) {
		if event := entry.(map[string]any); event["name"] == "task.result" {
			results = append(results, event)
		}
	}
	if len(results) != 2 || results[0]["seq"] != original["seq"] || results[1]["data"].(map[string]any)["artifact"] == oldArtifact ||
		shown["requests"].([]any)[0].(map[string]any)["result"] != results[1]["seq"] {
		t.Fatalf("result history/current answer: %v", shown)
	}
	if env.OK("artifact", "show", oldArtifact)["id"] != oldArtifact {
		t.Fatalf("historical artifact %s missing", oldArtifact)
	}
	if items := env.OK("inbox")["items"].([]any); len(items) != 3 || items[2].(map[string]any)["event"].(map[string]any)["name"] != "task.result" {
		t.Fatalf("result did not get a separate inbox item: %v", items)
	}
	if task := env.Task("t_2"); task["state"] != "waiting" {
		t.Fatalf("child was automatically closed: %v", task)
	}
}

func TestAnsweredDriverRequestCanReviseResult(t *testing.T) {
	env, _ := withDaemon(t)
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Supervise")
	first := strconv.Itoa(int(env.OK("task", "show", "t_1")["requests"].([]any)[0].(map[string]any)["seq"].(float64)))
	env.OK("task", "create", "--role", "driver", "--repo", "api", "--objective", "Other driver")
	foreign := strconv.Itoa(int(env.OK("task", "show", "t_2")["requests"].([]any)[0].(map[string]any)["seq"].(float64)))
	env.Script(map[string][][]action{"t_1": {
		{{"report": []string{"result", "Initial answer"}}, {"report": []string{"note", "Waiting"}}},
		{
			{"report": []string{"progress", "Working on revision"}},
			{"report": []string{"result", "Another answer"}},
			{"report": []string{"result", "Implicit follow-up"}},
			{"report": []string{"result", "Unknown request", "--request", "999999"}},
			{"report": []string{"question", "Foreign question", "--request", foreign}},
			{"report": []string{"result", "Foreign result", "--request", foreign}},
			{"report": []string{"result", "Revised answer", "--request", first}},
			{"report": []string{"note", "Finished"}},
		},
	}})
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	shown := env.OK("task", "show", "t_1")
	initial := shown["requests"].([]any)[0].(map[string]any)["result"]
	oldArtifact := shown["artifact"].(map[string]any)["id"].(string)
	sent := env.OK("task", "send", "t_1", "Another request")
	second := sent["seq"]
	env.StartDaemon()
	env.Eventually("revision turn", func() bool { return len(callsFor(env, "t_1", 1)) == 1 })
	env.WaitState("t_1", "done")
	errs := commandErrors(env, "t_1")
	if len(errs) != 4 || !strings.Contains(errs[0], `"kind":"request_required"`) ||
		!strings.Contains(errs[1], `"kind":"invalid_request"`) || !strings.Contains(errs[2], `"kind":"invalid_request"`) ||
		!strings.Contains(errs[3], `"kind":"invalid_request"`) {
		t.Fatalf("invalid requests were not refused: %v", errs)
	}
	shown = env.OK("task", "show", "t_1")
	requests := shown["requests"].([]any)
	if len(requests) != 2 || requests[0].(map[string]any)["result"] == initial || requests[0].(map[string]any)["state"] != "answered" ||
		requests[1].(map[string]any)["seq"] != second || requests[1].(map[string]any)["state"] != "answered" {
		t.Fatalf("request answers: %v", requests)
	}
	var results []map[string]any
	for _, entry := range shown["events"].([]any) {
		if event := entry.(map[string]any); event["name"] == "task.result" {
			results = append(results, event)
		}
	}
	if len(results) != 3 || results[0]["seq"] != initial || results[1]["seq"] != requests[1].(map[string]any)["result"] ||
		results[2]["seq"] != requests[0].(map[string]any)["result"] || results[2]["data"].(map[string]any)["artifact"] == oldArtifact {
		t.Fatalf("result lineage: %v", results)
	}
	if env.OK("artifact", "show", oldArtifact)["id"] != oldArtifact {
		t.Fatalf("historical artifact %s missing", oldArtifact)
	}
	if items := env.OK("inbox")["items"].([]any); len(items) != 3 {
		t.Fatalf("only result reports should reach the inbox: %v", items)
	} else {
		for _, entry := range items {
			item := entry.(map[string]any)
			if item["task"] != "t_1" || item["event"].(map[string]any)["name"] != "task.result" {
				t.Fatalf("unexpected inbox item: %v", item)
			}
		}
	}
	if shown["task"].(map[string]any)["state"] != "done" {
		t.Fatalf("result accepted the task: %v", shown["task"])
	}
	env.Vars["SHEPHRD_RUN_TOKEN"] = envOf(callsFor(env, "t_1", 1)[0])["SHEPHRD_RUN_TOKEN"]
	env.Refused("stale_run", "report", "result", "Too late", "--request", first)
}

func TestReplyWakesTheWorker(t *testing.T) {
	env, _ := withDaemon(t)
	env.Script(map[string][][]action{"t_1": {
		{{"report": []string{"question", "Use v1 or v2?"}}},
		{{"work": "Migrate"}, {"report": []string{"result", "Used v2"}}},
	}})
	env.StartDaemon()
	env.OK("task", "create", "--repo", "api", "--objective", "Migrate")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "waiting")
	var item map[string]any
	env.Eventually("the question in the inbox", func() bool {
		items := env.OK("inbox")["items"].([]any)
		if len(items) == 1 {
			item = items[0].(map[string]any)
		}
		return item != nil
	})
	reply := toStrings(item["next"].(map[string]any)["reply"])
	if r := env.Run("Use v2", reply[1:]...); r.Code != 0 {
		t.Fatalf("reply: %+v", r)
	}
	env.WaitState("t_1", "done")
	if brief := callsFor(env, "t_1", 1)[0]["brief"].(string); !strings.Contains(brief, "Use v2") {
		t.Fatalf("continuation brief lacks the reply:\n%s", brief)
	}
}

func TestInactiveRunIsStoppedAndNudged(t *testing.T) {
	env, _ := withDaemon(t)
	env.AppendConfig("inactivity = \"1s\"")
	env.Script(map[string][][]action{"t_1": {
		{{"sleep": "60s"}},
		{{"work": "Work"}, {"report": []string{"result", "Finished after the nudge"}}},
	}})
	env.StartDaemon()
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	nudge := callsFor(env, "t_1", 1)
	if len(nudge) != 1 || !strings.Contains(nudge[0]["brief"].(string), "inactive") {
		t.Fatalf("nudge: %v", nudge)
	}
}

func TestFailingPushStaysPendingAndRetries(t *testing.T) {
	env, _ := withDaemon(t)
	hook := newWebhook(t, env, "main")
	hook.status = 503
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"blocker", "No access"}}}}})
	env.StartDaemon()
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "held")
	var item map[string]any
	env.Eventually("a failed delivery attempt", func() bool {
		items := env.OK("inbox")["items"].([]any)
		if len(items) == 1 {
			item = items[0].(map[string]any)
		}
		return item != nil && item["delivery"].(map[string]any)["state"] == "retrying"
	})
	if item["state"] != "pending" {
		t.Fatalf("failed delivery acknowledged the item: %v", item)
	}
}

func TestContinuationBlockedByAGateIsHeld(t *testing.T) {
	env, _ := withDaemon(t)
	pkg, _ := filepath.EvalSymlinks(t.TempDir())
	testkit.WritePlugin(t, filepath.Join(pkg, "plugins", "pause"), "pause", "[[intercept]]\npoint = \"task.start\"\n")
	env.Script(map[string][][]action{"t_1": {{{"report": []string{"question", "Which?"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "waiting")
	env.AppendConfig(`[packages.tools]
path = "` + pkg + `"

[plugins.pause]
package = "tools"

[plugins.pause.options]
decision = "block"
reason = "paused for the release"`)
	env.StartDaemon()
	env.OK("task", "send", "t_1", "This one", "--reply-to", questionSeq(env, "t_1"))
	env.Eventually("the task to be held", func() bool { return env.Task("t_1")["reason"] == "start_blocked" })
}

func TestPluginsReceiveSubscribedEvents(t *testing.T) {
	env, _ := withDaemon(t)
	pkg, _ := filepath.EvalSymlinks(t.TempDir())
	log := filepath.Join(env.Home, "events.log")
	testkit.WritePlugin(t, filepath.Join(pkg, "plugins", "audit"), "audit", "[events]\nsubscribe = [\"task.created\"]\n")
	env.AppendConfig(`[packages.tools]
path = "` + pkg + `"

[plugins.audit]
package = "tools"

[plugins.audit.options]
log = "` + log + `"`)
	env.StartDaemon()
	env.Eventually("plugins.changed", func() bool {
		for _, e := range env.OK("events")["events"].([]any) {
			if e.(map[string]any)["name"] == "plugins.changed" {
				return true
			}
		}
		return false
	})
	env.OK("task", "create", "--repo", "api", "--objective", "Audited")
	env.Eventually("the event delivered", func() bool {
		body, _ := os.ReadFile(log)
		return strings.Contains(string(body), `"task.created"`) && strings.Contains(string(body), `"token":true`)
	})
	body, _ := os.ReadFile(log)
	if strings.Contains(string(body), `"repo.added"`) {
		t.Fatalf("delivered unsubscribed or historical events:\n%s", body)
	}
}

func TestOnlyDriversHaveInboxes(t *testing.T) {
	env, _ := withHarness(t)
	env.Script(map[string][][]action{"t_1": {{{"shephrd": []string{"inbox"}}, {"work": "Work"}, {"report": []string{"result", "done"}}}}})
	env.OK("task", "create", "--repo", "api", "--objective", "Work")
	env.OK("task", "start", "t_1")
	env.WaitState("t_1", "done")
	if errs := commandErrors(env, "t_1"); len(errs) != 1 || !strings.Contains(errs[0], "no_inbox") {
		t.Fatalf("inbox from a run: %v", errs)
	}
}

func questionSeq(env *testkit.Env, task string) string {
	for _, e := range env.OK("task", "show", task)["events"].([]any) {
		if e.(map[string]any)["name"] == "task.question" {
			return strconv.Itoa(int(e.(map[string]any)["seq"].(float64)))
		}
	}
	return ""
}
