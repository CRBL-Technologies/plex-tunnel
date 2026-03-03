package client

import (
	"math"
	"math/rand"
	"time"
)

const maxReconnectJitter = 500 * time.Millisecond

func BackoffDelay(attempt int, maxDelay time.Duration) time.Duration {
	return backoffDelayWithRand(attempt, maxDelay, rand.New(rand.NewSource(time.Now().UnixNano())))
}

func backoffDelayWithRand(attempt int, maxDelay time.Duration, rng *rand.Rand) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if maxDelay <= 0 {
		maxDelay = 60 * time.Second
	}

	exponential := math.Pow(2, float64(attempt))
	delay := time.Duration(exponential) * time.Second
	if delay > maxDelay {
		delay = maxDelay
	}

	jitter := time.Duration(rng.Int63n(int64(maxReconnectJitter) + 1))
	return delay + jitter
}
