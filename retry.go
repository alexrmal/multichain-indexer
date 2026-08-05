package main

import (
	"context"
	"fmt"
	"time"
)

// withRetry retries fn up to attempts times with capped exponential
// backoff, aborting early if ctx is cancelled. It deliberately does not
// classify errors as retryable/fatal — everything gets retried up to the
// cap, then the last error is returned for the caller to log; the outer
// poll loop already treats any tick error as "try again next tick," so a
// persistent failure still surfaces rather than looping forever.
//
// Callers wrap whole units of work (a single RPC call, or a whole DB
// transaction from Begin to Commit) rather than individual statements
// inside an already-open transaction: once one statement in a transaction
// fails, Postgres has already aborted that transaction server-side, so the
// only correct retry is redoing the entire operation from a fresh Begin.
func withRetry(ctx context.Context, attempts int, baseDelay time.Duration, fn func() error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if i < attempts-1 {
			delay := baseDelay * time.Duration(1<<i)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}
