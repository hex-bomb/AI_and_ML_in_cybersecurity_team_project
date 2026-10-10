package ports

import (
	"context"
	"time"
)

// Clock abstracts time for retry/lease/backoff tests.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns time.Now in UTC to match timestamptz comparisons.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Sleep waits for d or context cancellation.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
