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

// Lease bounds the work on claims leased together. Until is when the
// claims come due again and another worker may take them; ClaimMax is the
// most one claim can take. A claim starts only while ClaimMax still ends
// before Until, and runs under ClaimMax as its deadline, so no claim is
// ever worked after its lease has lapsed, however the batch goes.
type Lease struct {
	Until    time.Time
	ClaimMax time.Duration
	Now      func() time.Time // nil: time.Now
}

// Each runs fn on claims in order, within l. It returns the claims it did
// not start, for the caller to release with their attempt refunded, and
// fn's first error, after which it starts no more.
func Each[T any](ctx context.Context, l Lease, claims []T, fn func(context.Context, T) error) ([]T, error) {
	now := l.Now
	if now == nil {
		now = time.Now
	}
	for i, c := range claims {
		if ctx.Err() != nil || now().Add(l.ClaimMax).After(l.Until) {
			return claims[i:], nil
		}
		cctx, cancel := context.WithTimeout(ctx, l.ClaimMax)
		err := fn(cctx, c)
		cancel()
		if err != nil {
			return claims[i+1:], err
		}
	}
	return nil, nil
}
