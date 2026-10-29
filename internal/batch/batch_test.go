package batch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A batch that panics is logged and the loop carries on.
func TestLoopSurvivesPanic(t *testing.T) {
	logs := &lockedBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan int, 10)
	n := 0
	done := make(chan struct{})
	go func() {
		Loop(ctx, time.Millisecond, func(context.Context) (int, error) {
			n++
			calls <- n
			if n == 1 {
				panic("hostile feed")
			}
			return 0, nil
		}, slog.New(slog.NewTextHandler(logs, nil)))
		close(done)
	}()
	for want := 1; want <= 2; want++ {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("call %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no batch %d: the loop died with the panic", want)
		}
	}
	cancel()
	<-done
	if out := logs.String(); !strings.Contains(out, "batch panicked") || !strings.Contains(out, "hostile feed") || !strings.Contains(out, "batch_test.go") {
		t.Errorf("panic not logged with its value and stack:\n%s", out)
	}
}

// A claim starts only if its worst case ends before the lease lapses; the
// rest are handed back unstarted, and so is everything after an error.
func TestEachKeepsWithinTheLease(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	clock := t0
	l := Lease{Until: t0.Add(100 * time.Second), ClaimMax: 30 * time.Second, Now: func() time.Time { return clock }}
	claims := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	var ran []int
	rest, err := Each(context.Background(), l, claims, func(ctx context.Context, c int) error {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > l.ClaimMax {
			t.Errorf("claim %d runs without its ClaimMax deadline", c)
		}
		ran = append(ran, c)
		clock = clock.Add(10 * time.Second)
		return nil
	})
	// Starts at 0, 10, … 70 s: 70 + 30 = 100 still ends by Until; 80 would not.
	if err != nil || len(ran) != 8 || len(rest) != 2 || rest[0] != 8 {
		t.Fatalf("ran %v, rest %v, %v", ran, rest, err)
	}

	clock, ran = t0, nil
	boom := errors.New("boom")
	rest, err = Each(context.Background(), l, claims, func(_ context.Context, c int) error {
		ran = append(ran, c)
		if c == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || len(ran) != 3 || len(rest) != 7 || rest[0] != 3 {
		t.Fatalf("after an error: ran %v, rest %v, %v", ran, rest, err)
	}
}

// A claim the caller's context ends under is handed back with those not
// started; a claim that fails on its own is not.
func TestEachHandsBackTheClaimAShutdownInterrupts(t *testing.T) {
	l := Lease{Until: time.Now().Add(time.Hour), ClaimMax: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	rest, err := Each(ctx, l, []int{0, 1, 2}, func(ctx context.Context, c int) error {
		if c == 1 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	if err == nil || len(rest) != 2 || rest[0] != 1 {
		t.Errorf("interrupted at 1: handed back %v (%v), want [1 2]", rest, err)
	}
	rest, err = Each(context.Background(), l, []int{0, 1, 2}, func(ctx context.Context, c int) error {
		if c == 1 {
			return errors.New("source refused")
		}
		return nil
	})
	if err == nil || len(rest) != 1 || rest[0] != 2 {
		t.Errorf("failed at 1: handed back %v (%v), want [2]", rest, err)
	}
}
