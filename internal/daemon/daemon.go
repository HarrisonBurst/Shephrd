package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"shephrd/internal/config"
	"shephrd/internal/coord"
	"shephrd/internal/execution"
	"shephrd/internal/fault"
	"shephrd/internal/notify"
	"shephrd/internal/plugin"
	"shephrd/internal/store"
)

const (
	turnGrace     = 30 * time.Second
	maxDeliveries = 20
	reconcileEach = 5 * time.Minute
)

type Daemon struct {
	Getenv   config.Getenv
	Reserved []string
	Log      io.Writer

	db            *store.Store
	lastReconcile time.Time
}

func lockPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "daemon.lock")
}

// Running reports whether a daemon holds this home host's lock.
func Running(cfg *config.Config) bool {
	file, err := os.Open(lockPath(cfg))
	if err != nil {
		return false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

// Run delivers durable facts until ctx ends: it decides nothing a command
// could not, and every pass can be repeated safely.
func (d *Daemon) Run(ctx context.Context) error {
	cfg, err := config.Load(d.Getenv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath(&cfg), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fault.New("daemon_running", "another daemon already runs on this home host")
	}
	if d.db, err = store.Open(ctx, cfg.Store); err != nil {
		return err
	}
	defer d.db.Close()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var version, seen int64
	var last time.Time
	for {
		if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil && ctx.Err() == nil {
			d.logf("read data version: %v", err)
		}
		if version != seen || time.Since(last) >= 2*time.Second {
			seen, last = version, time.Now()
			d.pass(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (d *Daemon) logf(format string, args ...any) {
	fmt.Fprintf(d.Log, time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
}

func (d *Daemon) pass(ctx context.Context) {
	cfg, err := config.Load(d.Getenv)
	if err != nil {
		d.logf("configuration: %v", err)
		return
	}
	reg := plugin.Load(&cfg, d.Reserved)
	x, err := execution.New(d.db, &cfg, reg, d.Getenv)
	if err != nil {
		d.logf("executor: %v", err)
		return
	}
	if err := d.pluginsChanged(ctx, reg); err != nil {
		d.logf("plugins: %v", err)
	}
	if time.Since(d.lastReconcile) >= reconcileEach {
		d.lastReconcile = time.Now()
		if _, err := x.Reconcile(ctx); err != nil {
			d.logf("reconcile: %v", err)
		}
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"runs", func() error { return d.observe(ctx, x, &cfg) }},
		{"wakes", func() error { return d.wake(ctx, x, reg, &cfg) }},
		{"delivery", func() error { return d.deliver(ctx, reg, &cfg) }},
		{"events", func() error { return d.dispatch(ctx, reg) }},
	} {
		if err := step.run(); err != nil && ctx.Err() == nil {
			d.logf("%s: %v", step.name, err)
		}
	}
}

// observe probes live runs, stops worker processes that outlive their
// reported turn, and interrupts runs inactive past their role's timeout.
func (d *Daemon) observe(ctx context.Context, x *execution.Executor, cfg *config.Config) error {
	runs, err := coord.Runs(d.db, `liveness IN ('starting', 'live')`)
	if err != nil {
		return err
	}
	for _, r := range runs {
		observed, err := x.Probe(ctx, r)
		if err != nil {
			d.logf("probe %s run %d: %v", coord.TaskRef(r.Task), r.Generation, err)
			continue
		}
		if observed != "live" {
			continue
		}
		t, err := coord.Load(d.db, r.Task)
		if err != nil {
			return err
		}
		idle := time.Since(parseTime(r.LastActivity))
		limit := cfg.Timeouts.Inactivity.Duration
		if t.Role == "driver" {
			limit = cfg.Timeouts.DriverInactivity.Duration
		}
		reason := ""
		switch {
		case t.Role == "worker" && r.TurnReported && idle >= turnGrace:
			reason = "turn_ended"
		case idle >= limit:
			reason = "inactive"
		}
		if reason != "" {
			if err := x.Interrupt(ctx, r, reason); err != nil {
				d.logf("interrupt %s: %v", t.Ref, err)
			}
		}
	}
	return nil
}

// wake starts a continuation turn for each task with something new, once
// its settle window has passed and its last run has exited.
func (d *Daemon) wake(ctx context.Context, x *execution.Executor, reg *plugin.Registry, cfg *config.Config) error {
	due := store.Timestamp(time.Now().Add(-cfg.Timeouts.Settle.Duration))
	tasks, err := coord.Query(d.db, `t.wake_at IS NOT NULL AND t.wake_at <= ? AND t.state IN ('running', 'waiting', 'done')`, due)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		current, err := coord.CurrentRun(d.db, t)
		if err != nil {
			return err
		}
		if current != nil && current.Liveness != "exited" {
			continue
		}
		if err := d.gateStart(ctx, reg, t); err != nil {
			continue
		}
		if err := d.db.Write(ctx, func(tx *store.Tx) error {
			_, err := tx.Emit("turn.woken", "system", t.ID, t.Attempt, 0, map[string]any{"task": t.Ref, "reason": "new events"})
			return err
		}); err != nil {
			return err
		}
		if _, err := x.Start(ctx, "system", t.ID, "continue", nil); err != nil {
			d.logf("continue %s: %v", t.Ref, err)
		}
	}
	return nil
}

func (d *Daemon) gateStart(ctx context.Context, reg *plugin.Registry, t *coord.Task) error {
	err := reg.Gate(ctx, "task.start", map[string]any{"caller": "system", "task": t.Ref, "purpose": "continue"}, d.Getenv)
	if err == nil {
		return nil
	}
	d.logf("continue %s refused: %v", t.Ref, err)
	return d.db.Write(ctx, func(tx *store.Tx) error {
		if blocked, ok := plugin.AsBlocked(err); ok {
			if _, err := tx.Emit("plugin.blocked", "system", t.ID, 0, 0, map[string]string{
				"plugin": blocked.Plugin, "point": blocked.Point, "reason": blocked.Reason,
			}); err != nil {
				return err
			}
		}
		t, err := coord.Load(tx, t.ID)
		if err != nil {
			return err
		}
		return coord.Hold(tx, t, "start_blocked")
	})
}

// deliver pushes pending inbox items to each driver's delivery provider.
// Delivery never acknowledges; items fail independently and retry with
// backoff.
func (d *Daemon) deliver(ctx context.Context, reg *plugin.Registry, cfg *config.Config) error {
	now := time.Now()
	items, err := coord.Items(d.db, `i.state = 'pending' AND i.delivery IN ('', 'retrying') AND (i.next_delivery IS NULL OR i.next_delivery <= ?)`, store.Timestamp(now))
	if err != nil {
		return err
	}
	for _, item := range items {
		kind, name, _ := strings.Cut(item.Driver, ":")
		delivery := cfg.Drivers[name].Delivery
		if kind != "driver" || delivery.Provider == "" {
			continue
		}
		id := fmt.Sprintf("%s-inbox-%d", cfg.Host, item.ID)
		body, err := json.Marshal(map[string]any{"id": id, "driver": item.Driver, "item": item})
		if err != nil {
			return err
		}
		outcome, detail := d.push(ctx, reg, delivery, id, body, now)
		err = d.db.Write(ctx, func(tx *store.Tx) error {
			attempts := item.Delivery.Attempts + 1
			state, next := outcome, any(nil)
			if outcome == "retryable" {
				state, next = "retrying", store.Timestamp(now.Add(backoff(attempts, 5*time.Second, 10*time.Minute)))
				if attempts >= maxDeliveries {
					state, next = "failed", nil
				}
			}
			if _, err := tx.Exec(`UPDATE inbox_items SET delivery = ?, delivery_attempts = ?, next_delivery = ?, delivery_detail = ? WHERE id = ?`,
				state, attempts, next, detail, item.ID); err != nil {
				return err
			}
			_, err := tx.Emit("delivery.attempted", "system", item.TaskID, 0, 0, map[string]any{
				"driver": item.Driver, "item": item.ID, "provider": delivery.Provider, "outcome": outcome, "detail": detail,
			})
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) push(ctx context.Context, reg *plugin.Registry, delivery config.Delivery, id string, body []byte, now time.Time) (string, string) {
	if delivery.Provider == "webhook" {
		return notify.Webhook(ctx, delivery, id, body, now)
	}
	provider := reg.Provider("delivery", delivery.Provider)
	if provider == nil {
		return "retryable", fmt.Sprintf("delivery provider %q is not a declared plugin", delivery.Provider)
	}
	out, err := reg.Call(ctx, provider, plugin.Call{Kind: "provide", Type: "delivery", Body: json.RawMessage(body), Getenv: d.Getenv})
	if err != nil {
		return "retryable", err.Error()
	}
	var answer struct {
		Outcome string `json:"outcome"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(out, &answer) != nil || !slices.Contains([]string{"delivered", "retryable", "rejected"}, answer.Outcome) {
		return "retryable", "provider answered invalidly"
	}
	if len(answer.Detail) > 512 {
		answer.Detail = answer.Detail[:512]
	}
	return answer.Outcome, answer.Detail
}

// dispatch delivers subscribed events to each available plugin in order,
// in batches, at least once, advancing its cursor only on success.
func (d *Daemon) dispatch(ctx context.Context, reg *plugin.Registry) error {
	now := time.Now()
	for _, p := range reg.All() {
		if p.Unavailable != "" || len(p.Manifest.Events.Subscribe) == 0 {
			continue
		}
		var cursor int64
		var failures int
		var next sql.NullString
		err := d.db.QueryRowContext(ctx, `SELECT seq, failures, next_attempt FROM plugin_cursors WHERE plugin = ?`, p.Name).Scan(&cursor, &failures, &next)
		if errors.Is(err, sql.ErrNoRows) {
			if err := d.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&cursor); err != nil {
				return err
			}
			if _, err := d.db.ExecContext(ctx, `INSERT INTO plugin_cursors (plugin, seq) VALUES (?, ?)`, p.Name, cursor); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if next.Valid && next.String > store.Timestamp(now) {
			continue
		}
		events, err := d.db.EventsWhere(ctx, `WHERE seq > ? ORDER BY seq LIMIT 500`, cursor)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			continue
		}
		batch, last := []store.Event{}, events[len(events)-1].Seq
		for _, e := range events {
			if !slices.Contains(p.Manifest.Events.Subscribe, e.Name) || !d.inRepos(ctx, p, e) {
				continue
			}
			if len(batch) == 100 {
				last = batch[len(batch)-1].Seq
				break
			}
			batch = append(batch, e)
		}
		if len(batch) > 0 {
			err = d.callEvents(ctx, reg, p, batch)
		}
		if err != nil {
			failures++
			_, err = d.db.ExecContext(ctx, `UPDATE plugin_cursors SET failures = ?, next_attempt = ?, last_error = ? WHERE plugin = ?`,
				failures, store.Timestamp(now.Add(backoff(failures, 5*time.Second, time.Hour))), err.Error(), p.Name)
		} else {
			_, err = d.db.ExecContext(ctx, `UPDATE plugin_cursors SET seq = ?, failures = 0, next_attempt = NULL, last_error = '' WHERE plugin = ?`, last, p.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) callEvents(ctx context.Context, reg *plugin.Registry, p *plugin.Plugin, batch []store.Event) error {
	timeout := plugin.Timeouts["event"]
	token, revoke, err := coord.IssueCallToken(ctx, d.db, p.Name, nil, timeout+time.Minute)
	if err != nil {
		return err
	}
	defer revoke()
	_, err = reg.Call(ctx, p, plugin.Call{Kind: "event", Body: map[string]any{"events": batch}, Token: token, Getenv: d.Getenv})
	return err
}

func (d *Daemon) inRepos(ctx context.Context, p *plugin.Plugin, e store.Event) bool {
	repos := p.Manifest.Events.Repos
	if len(repos) == 0 {
		return true
	}
	var repo string
	d.db.QueryRowContext(ctx, `SELECT COALESCE(r.name, '') FROM tasks t LEFT JOIN repos r ON r.id = t.repo WHERE t.id = ?`, e.TaskID).Scan(&repo)
	return slices.Contains(repos, repo)
}

// pluginsChanged records when the declared set of plugins changes, so
// removing a gate leaves a trace.
func (d *Daemon) pluginsChanged(ctx context.Context, reg *plugin.Registry) error {
	names := []string{}
	for _, p := range reg.All() {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	current, _ := json.Marshal(names)
	var stored string
	err := d.db.QueryRowContext(ctx, `SELECT value FROM daemon_state WHERE key = 'plugins'`).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if stored == string(current) {
		return nil
	}
	var before []string
	json.Unmarshal([]byte(stored), &before)
	added, removed := []string{}, []string{}
	for _, name := range names {
		if !slices.Contains(before, name) {
			added = append(added, name)
		}
	}
	for _, name := range before {
		if !slices.Contains(names, name) {
			removed = append(removed, name)
		}
	}
	return d.db.Write(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO daemon_state (key, value) VALUES ('plugins', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, string(current)); err != nil {
			return err
		}
		_, err := tx.Emit("plugins.changed", "system", 0, 0, 0, map[string]any{"plugins": names, "added": added, "removed": removed})
		return err
	})
}

func backoff(attempt int, base, limit time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000000000Z", s)
	return t
}
