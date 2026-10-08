package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

type bridgeRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *bridgeRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *bridgeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type fakeBridgeProcess struct {
	recorder *bridgeRecorder
	wait     chan error
}

func (p *fakeBridgeProcess) PID() int { return 123 }

func (p *fakeBridgeProcess) Start() error {
	p.recorder.add("start")
	return nil
}

func (p *fakeBridgeProcess) Wait() error {
	return <-p.wait
}

func (p *fakeBridgeProcess) Kill() error {
	p.recorder.add("kill")
	p.wait <- errors.New("killed")
	return nil
}

func TestBridgeTeardownCannotRegressAcceptedTerminal(t *testing.T) {
	recorder := &bridgeRecorder{}
	process := &fakeBridgeProcess{recorder: recorder, wait: make(chan error, 1)}
	state := bridgeState(recorder)
	grace := make(chan time.Time, 1)
	grace <- time.Time{}
	config := InteractiveConfig{
		Attempt: model.Attempt{ID: "attempt", RunGeneration: 7},
		Log:     io.Discard,
		State:   state,
		Lifecycle: Lifecycle{
			Report:  func(state, _ string) error { recorder.add("lifecycle:" + state); return nil },
			Release: func() error { recorder.add("release"); return nil },
		},
		Name:     "test",
		Finalize: func(Result) (bool, error) { recorder.add("finalize"); return true, nil },
		process:  func(InteractiveConfig) bridgeProcess { return process },
		after:    func(time.Duration) <-chan time.Time { return grace },
	}
	outcome, err := runInteractive(config, func(execution *bridgeExecution) {
		if !execution.ingestEnvelope(`<shephrd-event>{"type":"done","payload":"complete","artifact":"report:/tmp/report.md"}</shephrd-event>`, "missing") {
			t.Fatal("terminal was not accepted")
		}
		execution.fail("late bridge cleanup failure", false)
		execution.addCleanup(func() { recorder.add("cleanup") })
		<-execution.grace()
		execution.stop()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.TerminalAccepted || outcome.Failure != "" {
		t.Fatalf("outcome=%+v", outcome)
	}
	want := []string{"runner", "lifecycle:working", "start", "ingest", "lifecycle:idle", "kill", "finish:", "finalize", "cleanup", "release"}
	if got := recorder.snapshot(); !equalStrings(got, want) {
		t.Fatalf("events=%q want=%q", got, want)
	}
}

func TestClaudeTerminalCandidateWaitsForIdleStopAcceptance(t *testing.T) {
	recorder := &bridgeRecorder{}
	execution := &bridgeExecution{
		config: InteractiveConfig{
			Attempt: model.Attempt{ID: "attempt", RunGeneration: 1},
			State:   bridgeState(recorder),
			Lifecycle: Lifecycle{
				Report: func(state, _ string) error { recorder.add("lifecycle:" + state); return nil },
			},
		},
		cursor: 3,
	}
	terminal := `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:shephrd/task"}</shephrd-event>`
	if !execution.ingestClaudeEnvelope(terminal, "missing") {
		t.Fatal("terminal candidate was rejected")
	}
	if execution.terminal || execution.claudeTerminal != terminal || len(recorder.snapshot()) != 0 {
		t.Fatalf("execution=%+v events=%q", execution, recorder.snapshot())
	}
	if !execution.acceptClaudeTerminal() || !execution.terminal || execution.claudeTerminal != "" {
		t.Fatalf("accepted execution=%+v", execution)
	}
	if got := recorder.snapshot(); !equalStrings(got, []string{"ingest", "lifecycle:idle"}) {
		t.Fatalf("events=%q", got)
	}
}

func TestBridgeFailureTeardownIsLinear(t *testing.T) {
	recorder := &bridgeRecorder{}
	process := &fakeBridgeProcess{recorder: recorder, wait: make(chan error, 1)}
	config := InteractiveConfig{
		Attempt: model.Attempt{ID: "attempt", RunGeneration: 3},
		Log:     io.Discard,
		State:   bridgeState(recorder),
		Lifecycle: Lifecycle{
			Report:  func(state, _ string) error { recorder.add("lifecycle:" + state); return nil },
			Release: func() error { recorder.add("release"); return nil },
		},
		Name:     "test",
		Finalize: func(Result) (bool, error) { recorder.add("finalize"); return true, nil },
		process:  func(InteractiveConfig) bridgeProcess { return process },
	}
	outcome, err := runInteractive(config, func(execution *bridgeExecution) {
		execution.fail("invalid frame", false)
		execution.addCleanup(func() { recorder.add("cleanup") })
		execution.stop()
	})
	if err == nil || err.Error() != "invalid frame" {
		t.Fatalf("error=%v", err)
	}
	if outcome.TerminalAccepted || outcome.Failure != "invalid frame" {
		t.Fatalf("outcome=%+v", outcome)
	}
	want := []string{"runner", "lifecycle:working", "start", "kill", "lifecycle:blocked", "death:invalid frame", "finish:invalid frame", "finalize", "cleanup", "release"}
	if got := recorder.snapshot(); !equalStrings(got, want) {
		t.Fatalf("events=%q want=%q", got, want)
	}
}

func TestClaudeReconciledSuffixUsesStrictTerminalValidation(t *testing.T) {
	checkpoint := `<shephrd-event>{"type":"checkpoint","payload":"progress","checkpoint":{"schema_version":1,"summary":"progress","completed":[],"next_steps":["finish"],"decisions":[],"changed_paths":[],"checks":[],"blockers":[]}}</shephrd-event>`
	done := `<shephrd-event>{"type":"done","payload":"complete","artifact":"branch:shephrd/task"}</shephrd-event>`
	for _, test := range []struct {
		name      string
		envelopes []string
		reason    string
	}{
		{name: "checkpoint only", envelopes: []string{checkpoint}},
		{name: "checkpoint and terminal", envelopes: []string{checkpoint, done}},
		{name: "nested framing", envelopes: []string{`<shephrd-event>{"type":"done","payload":"<shephrd-event>{}","artifact":"branch:shephrd/task"}</shephrd-event>`}, reason: "nested"},
		{name: "semantically invalid", envelopes: []string{`<shephrd-event>{"type":"unknown","payload":"complete"}</shephrd-event>`}, reason: "invalid worker event"},
		{name: "duplicate terminal suffix", envelopes: []string{done, done}, reason: "after a terminal envelope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateClaudeReconciledEnvelopes(test.envelopes, adapter.ParseEvent)
			if test.reason == "" && err != nil {
				t.Fatal(err)
			}
			if test.reason != "" && (err == nil || !strings.Contains(err.Error(), test.reason)) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestBridgeFrameSourceReadsOneOrderedStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inputs := make(chan int)
	stopped := make(chan struct{})
	var active int32
	var maximum int32
	frames := startFrameSource(ctx, func() (int, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			observed := atomic.LoadInt32(&maximum)
			if current <= observed || atomic.CompareAndSwapInt32(&maximum, observed, current) {
				break
			}
		}
		value, ok := <-inputs
		atomic.AddInt32(&active, -1)
		if !ok {
			close(stopped)
			return 0, errors.New("closed")
		}
		return value, nil
	})
	inputs <- 1
	if received := <-frames; received.err != nil || received.frame != 1 {
		t.Fatalf("first=%+v", received)
	}
	inputs <- 2
	if received := <-frames; received.err != nil || received.frame != 2 {
		t.Fatalf("second=%+v", received)
	}
	cancel()
	close(inputs)
	<-stopped
	if atomic.LoadInt32(&active) != 0 {
		t.Fatal("frame source did not stop")
	}
	if atomic.LoadInt32(&maximum) != 1 {
		t.Fatalf("maximum concurrent reads=%d", maximum)
	}
}

func bridgeState(recorder *bridgeRecorder) State {
	return State{
		SetRunner:            func(int) error { recorder.add("runner"); return nil },
		SetSession:           func(string) error { return nil },
		UpdateCursor:         func(int64) error { return nil },
		Ingest:               func(model.Event, int64) error { recorder.add("ingest"); return nil },
		RecordControlFailure: func(reason string) error { recorder.add("control:" + reason); return nil },
		RecordRunnerDeath:    func(reason string) error { recorder.add("death:" + reason); return nil },
		Finish:               func(_ int, failure string) error { recorder.add("finish:" + failure); return nil },
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
