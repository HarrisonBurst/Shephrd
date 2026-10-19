package coord

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"shephrd/internal/store"
)

func TestSurfaceMailboxConcurrentOrderAndDurableReplies(t *testing.T) {
	f := newFixture(t)
	const count = 24
	var wg sync.WaitGroup
	ids := make(chan int64, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := f.write(func(tx *store.Tx) error {
				item, err := SubmitSurface(tx, fmt.Sprintf("s%d", i%2), "room", fmt.Sprint(i))
				if err == nil {
					ids <- item.ID
				}
				return err
			})
			if err != nil {
				t.Errorf("submit: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
	items, err := PendingSurface(f.db, 0)
	if err != nil || len(items) != count {
		t.Fatalf("pending: %d, %v", len(items), err)
	}
	for i, item := range items {
		if item.ID != int64(i+1) {
			t.Fatalf("out of order: %v", items)
		}
		if err := f.write(func(tx *store.Tx) error {
			_, err := ReplySurface(tx, item.ID, "reply "+item.Body)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if items, err := PendingSurface(f.db, 0); err != nil || len(items) != 0 {
		t.Fatalf("answered input remains: %v %v", items, err)
	}
	f.db.Close()
	db, err := store.Open(context.Background(), f.db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, surface := range []string{"s0", "s1"} {
		replies, err := SurfaceReplies(db, surface, "room", 0)
		if err != nil || len(replies) != count/2 {
			t.Fatalf("%s replies: %v %v", surface, replies, err)
		}
		for i, reply := range replies {
			if i > 0 && replies[i-1].ID >= reply.ID {
				t.Fatalf("reply cursor not ordered: %v", replies)
			}
			if items[reply.Input-1].Surface != surface {
				t.Fatalf("foreign reply: %v", reply)
			}
		}
		later, err := SurfaceReplies(db, surface, "room", replies[0].ID)
		if err != nil || len(later) != len(replies)-1 || later[0].ID != replies[1].ID {
			t.Fatalf("cursor: %v %v", later, err)
		}
	}
	if replies, err := SurfaceReplies(db, "s0", "other", 0); err != nil || len(replies) != 0 {
		t.Fatalf("cross-thread replies: %v %v", replies, err)
	}
}

func TestSurfaceMailboxCapacityAndSingleReply(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 1024; i++ {
		if err := f.write(func(tx *store.Tx) error {
			_, err := SubmitSurface(tx, "s", "room", "body")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.write(func(tx *store.Tx) error {
		_, err := SubmitSurface(tx, "s", "room", "body")
		return err
	}); kind(err) != "mailbox_full" {
		t.Fatalf("over capacity: %v", err)
	}
	items, err := PendingSurface(f.db, 0)
	if err != nil || len(items) != 100 {
		t.Fatalf("bounded page: %v %v", len(items), err)
	}
	if err := f.write(func(tx *store.Tx) error {
		_, err := ReplySurface(tx, 1, "done")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.write(func(tx *store.Tx) error {
		_, err := ReplySurface(tx, 1, "again")
		return err
	}); kind(err) != "already_answered" {
		t.Fatalf("duplicate reply: %v", err)
	}
	if err := f.write(func(tx *store.Tx) error {
		_, err := SubmitSurface(tx, "s", "room", "next")
		return err
	}); err != nil {
		t.Fatalf("reclaimed capacity: %v", err)
	}
	for id := int64(2); id <= 1024; id++ {
		if err := f.write(func(tx *store.Tx) error {
			_, err := ReplySurface(tx, id, "done")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	replies, err := SurfaceReplies(f.db, "s", "room", 0)
	if err != nil || len(replies) != 100 {
		t.Fatalf("bounded reply page: %d %v", len(replies), err)
	}
	if err := f.write(func(tx *store.Tx) error {
		_, err := ReplySurface(tx, 1025, "no space")
		return err
	}); kind(err) != "mailbox_full" {
		t.Fatalf("over reply capacity: %v", err)
	}
	items, err = PendingSurface(f.db, 1024)
	if err != nil || len(items) != 1 || items[0].ID != 1025 {
		t.Fatalf("full reply mailbox consumed input: %v %v", items, err)
	}
}
