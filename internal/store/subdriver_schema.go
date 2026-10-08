package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
)

const subdriverSchema = `
CREATE TABLE coordinators (
 id TEXT PRIMARY KEY, repo_id TEXT UNIQUE REFERENCES repos(id), context TEXT NOT NULL,
 generation INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'idle' CHECK(state IN ('idle','starting','running','held')),
 token TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', runner_pid INTEGER NOT NULL DEFAULT 0,
 harness_pid INTEGER NOT NULL DEFAULT 0, harness TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', runtime TEXT NOT NULL DEFAULT '',
 checkpoint TEXT NOT NULL DEFAULT '', failure TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL, endpoint_json TEXT NOT NULL DEFAULT ''
);
CREATE TABLE coordinator_requests (
 id TEXT PRIMARY KEY, coordinator_id TEXT NOT NULL REFERENCES coordinators(id), driver_id TEXT NOT NULL, origin_driver_id TEXT NOT NULL,
 request_key TEXT NOT NULL, original TEXT NOT NULL, context TEXT NOT NULL, lead_request_id TEXT REFERENCES coordinator_requests(id),
 state TEXT NOT NULL DEFAULT 'open' CHECK(state IN ('open','waiting','done')), created_at TEXT NOT NULL,
 UNIQUE(origin_driver_id,request_key)
);
CREATE TABLE coordinator_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL REFERENCES coordinator_requests(id),
 event_key TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('request','reply','question','result','blocker','handoff','recovery')),
 payload TEXT NOT NULL, reply_to INTEGER REFERENCES coordinator_events(id), handled INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL, UNIQUE(request_id,event_key)
);
CREATE TABLE coordinator_workers (
 request_id TEXT NOT NULL REFERENCES coordinator_requests(id), dispatch_key TEXT NOT NULL,
 task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id), specification TEXT NOT NULL,
 PRIMARY KEY(request_id,dispatch_key)
);
CREATE INDEX coordinator_requests_owner_idx ON coordinator_requests(coordinator_id,state);
CREATE INDEX coordinator_events_request_idx ON coordinator_events(request_id,id);
`

var subdriverColumns = map[string][]string{
	"coordinators":         {"id", "repo_id", "context", "generation", "state", "token", "session_id", "runner_pid", "harness_pid", "harness", "model", "runtime", "checkpoint", "failure", "updated_at", "endpoint_json"},
	"coordinator_requests": {"id", "coordinator_id", "driver_id", "origin_driver_id", "request_key", "original", "context", "lead_request_id", "state", "created_at"},
	"coordinator_events":   {"id", "request_id", "event_key", "kind", "payload", "reply_to", "handled", "created_at"},
	"coordinator_workers":  {"request_id", "dispatch_key", "task_id", "specification"},
}

func subdriverChecksum() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(subdriverSchema+"nullable-notification-source-v1")))
}

func (s *Store) upgradeSubdrivers() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "PRAGMA foreign_keys=ON")
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	var version int
	if err = conn.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version == 30 {
		var baseName, baseSum string
		var count int
		if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
			return err
		}
		if err = conn.QueryRowContext(ctx, "SELECT name,checksum FROM schema_migrations WHERE version=29").Scan(&baseName, &baseSum); err != nil {
			return err
		}
		if count != 2 || baseName != baselineName || baseSum != baselineChecksum() {
			return fmt.Errorf("incompatible baseline identity under sub-driver migration")
		}
		var name, checksum string
		if err = conn.QueryRowContext(ctx, "SELECT name,checksum FROM schema_migrations WHERE version=30").Scan(&name, &checksum); err != nil {
			return err
		}
		if name != "030_coordinators" || checksum != subdriverChecksum() {
			return fmt.Errorf("incompatible sub-driver schema identity")
		}
		_, err = conn.ExecContext(ctx, "COMMIT")
		return err
	}
	if version != 29 {
		return fmt.Errorf("sub-driver migration requires schema 29")
	}
	if err = validateBaselineShapeOn(conn); err != nil {
		return err
	}
	var ddl string
	if err = conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE name='driver_notifications'").Scan(&ddl); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, "SELECT sql FROM sqlite_schema WHERE type='index' AND tbl_name='driver_notifications' AND sql IS NOT NULL")
	if err != nil {
		return err
	}
	var indexes []string
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			rows.Close()
			return err
		}
		indexes = append(indexes, body)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, subdriverSchema); err != nil {
		return err
	}
	ddl = strings.Replace(ddl, "CREATE TABLE driver_notifications", "CREATE TABLE new_notifications", 1)
	for _, field := range []string{"message_id INTEGER", "task_id TEXT", "attempt_id TEXT"} {
		ddl = strings.Replace(ddl, field+" NOT NULL", field, 1)
	}
	ddl = strings.TrimSuffix(strings.TrimSpace(ddl), ")") + `, coordinator_event_id INTEGER UNIQUE REFERENCES coordinator_events(id),
 CHECK((coordinator_event_id IS NULL AND message_id IS NOT NULL AND task_id IS NOT NULL AND attempt_id IS NOT NULL) OR
 (coordinator_event_id IS NOT NULL AND message_id IS NULL AND task_id IS NULL AND attempt_id IS NULL)))`
	if _, err = conn.ExecContext(ctx, ddl); err != nil {
		return err
	}
	columns := strings.Join(baselineObjectContract["driver_notifications"], ",")
	if _, err = conn.ExecContext(ctx, "INSERT INTO new_notifications("+columns+") SELECT "+columns+" FROM driver_notifications; DROP TABLE driver_notifications; ALTER TABLE new_notifications RENAME TO driver_notifications;"); err != nil {
		return err
	}
	for _, body := range indexes {
		if _, err = conn.ExecContext(ctx, body); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(30,'030_coordinators',?,?)", subdriverChecksum(), now()); err != nil {
		return err
	}
	var violation string
	rows, err = conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	if rows.Next() {
		rows.Close()
		return fmt.Errorf("sub-driver migration foreign key violation %s", violation)
	}
	rows.Close()
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}
