package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"shephrd/internal/fault"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	*sql.DB
	Path string
}

type Tx struct {
	*sql.Tx
	Now time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	dsn := "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	db.SetMaxOpenConns(8)
	s := &Store{DB: db, Path: path}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("open store: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		db.Close()
		return nil, fmt.Errorf("open store: journal mode is %q, want wal", mode)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("migrate store: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	var current int
	if err := s.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migrate store: %w", err)
	}
	if current > len(names) {
		return fault.New("store_too_new", "store schema is version %d, this release knows %d", current, len(names))
	}
	for _, name := range names[current:] {
		version, err := strconv.Atoi(strings.SplitN(filepath.Base(name), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = s.Write(ctx, func(tx *Tx) error {
			if _, err := tx.Exec(string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, Timestamp(tx.Now))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) Write(ctx context.Context, fn func(*Tx) error) error {
	sqlTx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := &Tx{Tx: sqlTx, Now: time.Now().UTC()}
	if err := fn(tx); err != nil {
		sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
}

func (s *Store) Mutate(ctx context.Context, caller, key, digest string, fn func(*Tx) (any, error)) (json.RawMessage, error) {
	var out json.RawMessage
	err := s.Write(ctx, func(tx *Tx) error {
		var storedDigest, storedResult string
		err := tx.QueryRow(`SELECT digest, result FROM idempotency WHERE caller = ? AND key = ?`, caller, key).Scan(&storedDigest, &storedResult)
		switch {
		case err == nil && storedDigest == digest:
			out = json.RawMessage(storedResult)
			return nil
		case err == nil:
			return fault.New("key_conflict", "key %q was already used for a different request", key)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		result, err := fn(tx)
		if err != nil {
			return err
		}
		if out, err = json.Marshal(result); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO idempotency (caller, key, digest, result, time) VALUES (?, ?, ?, ?, ?)`, caller, key, digest, string(out), Timestamp(tx.Now))
		return err
	})
	return out, err
}

func (tx *Tx) Emit(name, caller string, task, attempt, run int64, data any) (int64, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(`INSERT INTO events (name, task, attempt, run, caller, data, time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, nullable(task), nullable(attempt), nullable(run), caller, string(body), Timestamp(tx.Now))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

type Event struct {
	Seq     int64           `json:"seq"`
	Name    string          `json:"name"`
	Time    string          `json:"time"`
	Task    string          `json:"task,omitempty"`
	Attempt int64           `json:"attempt,omitempty"`
	Run     int64           `json:"run,omitempty"`
	Caller  string          `json:"caller"`
	Data    json.RawMessage `json:"data"`
	TaskID  int64           `json:"-"`
}

func (s *Store) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := s.QueryContext(ctx, `SELECT seq, name, time, COALESCE(task, 0), COALESCE(attempt, 0), COALESCE(run, 0), caller, data
		FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var data string
		if err := rows.Scan(&e.Seq, &e.Name, &e.Time, &e.TaskID, &e.Attempt, &e.Run, &e.Caller, &data); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		if e.TaskID != 0 {
			e.Task = "t_" + strconv.FormatInt(e.TaskID, 10)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func Timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func nullable(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
