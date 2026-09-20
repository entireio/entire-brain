package factgitmeta

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdvanceHonorsCancellationBeforeAndDuringLock(t *testing.T) {
	for _, alreadyCancelled := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "waiting"}[alreadyCancelled], func(t *testing.T) {
			b, err := NewLocalBackend(t.TempDir(), "repo", nil)
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := b.repo.lock()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if alreadyCancelled {
				cancel()
			}
			done := make(chan error, 1)
			go func() { _, err := b.Advance(ctx, "repo", "main", "", []byte("facts")); done <- err }()
			if !alreadyCancelled {
				time.Sleep(30 * time.Millisecond)
				cancel()
			}
			select {
			case err := <-done:
				unlock()
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Advance: %v", err)
				}
			case <-time.After(time.Second):
				unlock()
				err := <-done
				t.Errorf("Advance ignored cancellation while lock held, eventually returned %v", err)
			}
			_, _, found, err := b.Current(context.Background(), "repo", "main")
			if err != nil || found {
				t.Errorf("cancelled Advance published: found=%v err=%v", found, err)
			}
		})
	}
}

func TestAdvanceHonorsCancellationBeforePublishing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, err := NewLocalBackend(t.TempDir(), "repo", func() time.Time { cancel(); return time.Now() })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Advance(ctx, "repo", "main", "", []byte("facts")); !errors.Is(err, context.Canceled) {
		t.Errorf("Advance: %v", err)
	}
	_, _, found, err := b.Current(context.Background(), "repo", "main")
	if err != nil || found {
		t.Errorf("cancelled Advance published: found=%v err=%v", found, err)
	}
}
