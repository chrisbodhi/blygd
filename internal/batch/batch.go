// Package batch runs background workers: the mention verifier and sender,
// and the reader's poller. Their batches parse other people's HTML, JSON
// and XML, so a parser bug must cost one batch, not the process: a panic
// in a goroutine kills every request and every other worker with it.
package batch

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Loop runs fn until ctx ends. A batch that did work and succeeded is
// followed at once by the next; otherwise Loop waits idle. A batch that
// panics is logged with its stack and counts as failed, so it waits too.
func Loop(ctx context.Context, idle time.Duration, fn func(context.Context) (int, error), log *slog.Logger) {
	for {
		n, err := run(ctx, fn, log)
		if err != nil && ctx.Err() == nil {
			log.Error("batch failed", "err", err)
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(idle):
		}
	}
}

func run(ctx context.Context, fn func(context.Context) (int, error), log *slog.Logger) (n int, err error) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("batch panicked", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			n, err = 0, fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(ctx)
}
