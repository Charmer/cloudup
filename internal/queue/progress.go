package queue

import "time"

// progressThrottle limits how often progress Events are emitted for a
// single task (no more than ~10/sec) - without this, a fast local-network
// upload can emit thousands of Events per second and flood the UI. The
// final call (sent == total) always passes, so the caller reliably sees
// completion.
type progressThrottle struct {
	minInterval time.Duration
	last        time.Time
}

func newProgressThrottle(minInterval time.Duration) *progressThrottle {
	return &progressThrottle{minInterval: minInterval}
}

func (t *progressThrottle) Allow(sent, total int64) bool {
	if sent >= total {
		return true
	}
	now := time.Now()
	if now.Sub(t.last) < t.minInterval {
		return false
	}
	t.last = now
	return true
}
