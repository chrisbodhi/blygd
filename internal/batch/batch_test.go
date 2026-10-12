package batch

import (
	"bytes"
	"context"
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
