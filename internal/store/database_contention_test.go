package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type sqliteCodeError int

func (e sqliteCodeError) Error() string {
	return fmt.Sprintf("sqlite error %d", e)
}

func (e sqliteCodeError) Code() int {
	return int(e)
}

func TestRetrySQLiteContentionRecoversAndRejectsOtherErrors(t *testing.T) {
	attempts := 0
	err := retrySQLiteContention(context.Background(), time.Nanosecond, func(context.Context) error {
		attempts++
		if attempts < 4 {
			return sqliteCodeError(261)
		}
		return nil
	})
	if err != nil || attempts != 4 {
		t.Fatalf("retry result: attempts=%d err=%v", attempts, err)
	}

	sentinel := errors.New("invalid database")
	attempts = 0
	err = retrySQLiteContention(context.Background(), time.Nanosecond, func(context.Context) error {
		attempts++
		return sentinel
	})
	if !errors.Is(err, sentinel) || attempts != 1 {
		t.Fatalf("non-transient result: attempts=%d err=%v", attempts, err)
	}
}

func TestRetrySQLiteContentionExhaustionIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := retrySQLiteContention(ctx, time.Millisecond, func(context.Context) error {
		attempts++
		if attempts == 4 {
			cancel()
		}
		return sqliteCodeError(261)
	})
	if codeErr, ok := err.(sqliteCodeError); !ok || codeErr.Code() != 261 {
		t.Fatalf("exhaustion error = %T %v", err, err)
	}
	if attempts != 4 {
		t.Fatalf("exhaustion attempts = %d", attempts)
	}
}

func TestOpenFailsClosedOnInvalidDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := Open(path); err == nil {
		state.Close()
		t.Fatal("invalid database opened successfully")
	}
}

func TestConcurrentFreshOpensWaitForSetupLockAndMigrateOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	locker, err := sql.Open("sqlite", path+"?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}

	stores := make([]*Store, 2)
	errors := make([]error, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range stores {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			stores[index], errors[index] = Open(path)
		}()
	}
	close(start)
	time.Sleep(100 * time.Millisecond)
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := locker.Close(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	for index, err := range errors {
		if err != nil {
			t.Fatalf("open %d: %v", index, err)
		}
		defer stores[index].Close()
	}
	var migrationCount int
	if err := stores[0].db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=` + strconv.Itoa(baselineVersion)).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration count = %d", migrationCount)
	}
}
