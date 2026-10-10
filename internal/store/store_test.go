package store

import (
	"context"
	"path/filepath"
	"testing"

	"shephrd/internal/fault"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "shephrd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReopeningAppliesNoMigrationTwice(t *testing.T) {
	s := open(t)
	again, err := Open(context.Background(), s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var count int
	if err := again.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migrations recorded %d, %v", count, err)
	}
}

func TestStoreNewerThanReleaseIsRefused(t *testing.T) {
	s := open(t)
	if _, err := s.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (99, 'now')`); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), s.Path)
	if fault.As(err).Kind != "store_too_new" {
		t.Fatalf("open: %v", err)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	s := open(t)
	err := s.Write(context.Background(), func(tx *Tx) error {
		_, err := tx.Emit("repo.added", "driver:main", 0, 0, 0, map[string]string{"repo": "r_1"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`UPDATE events SET name = 'x'`); err == nil {
		t.Fatal("update succeeded")
	}
	if _, err := s.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("delete succeeded")
	}
}

func TestMutateReplaysByKeyAndRefusesConflicts(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	calls := 0
	mutate := func(caller, digest string) (string, error) {
		out, err := s.Mutate(ctx, caller, "k", digest, func(*Tx) (any, error) {
			calls++
			return map[string]int{"call": calls}, nil
		})
		return string(out), err
	}
	first, err := mutate("driver:main", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := mutate("driver:main", "d1"); err != nil || again != first || calls != 1 {
		t.Fatalf("replay %q, %v, calls %d", again, err, calls)
	}
	if _, err := mutate("driver:main", "d2"); fault.As(err).Kind != "key_conflict" {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := mutate("driver:other", "d2"); err != nil || calls != 2 {
		t.Fatalf("keys are per caller: %v, calls %d", err, calls)
	}
}

func TestRefusedMutationRecordsNothing(t *testing.T) {
	s := open(t)
	_, err := s.Mutate(context.Background(), "driver:main", "k", "d", func(tx *Tx) (any, error) {
		if _, err := tx.Emit("repo.added", "driver:main", 0, 0, 0, nil); err != nil {
			return nil, err
		}
		return nil, fault.New("refused", "no")
	})
	if fault.As(err).Kind != "refused" {
		t.Fatalf("mutate: %v", err)
	}
	var events, keys int
	s.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events)
	s.QueryRow(`SELECT COUNT(*) FROM idempotency`).Scan(&keys)
	if events != 0 || keys != 0 {
		t.Fatalf("events %d, keys %d after refusal", events, keys)
	}
}
