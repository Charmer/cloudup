package queue

import "time"

// backoffDelay returns the wait before retry attempt N+1 has been made,
// i.e. the delay after a failed attempt number `attempt` (1-based):
// baseDelay * 2^(attempt-1), capped at maxDelay.
func backoffDelay(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	if baseDelay <= 0 {
		baseDelay = time.Second
	}
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}

	delay := baseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	return delay
}
