package client

import (
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	circuitStateClosed   = "closed"
	circuitStateOpen     = "open"
	circuitStateHalfOpen = "half-open"

	circuitBreakerDefaultThreshold = 5
	circuitBreakerDefaultCooldown  = 30 * time.Second
)

type circuitBreaker struct {
	mu                  sync.Mutex
	logger              zerolog.Logger
	threshold           int
	cooldown            time.Duration
	consecutiveFailures int
	state               string
	lastFailureTime     time.Time
	halfOpenAt          time.Time
}

func newCircuitBreaker(threshold int, cooldown time.Duration, logger zerolog.Logger) *circuitBreaker {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown < 0 {
		cooldown = 0
	}

	return &circuitBreaker{
		logger:    logger,
		threshold: threshold,
		cooldown:  cooldown,
		state:     circuitStateClosed,
	}
}

func (c *circuitBreaker) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.state {
	case circuitStateClosed:
		return true
	case circuitStateOpen:
		if time.Since(c.halfOpenAt) < c.cooldown {
			return false
		}
		c.transitionLocked(circuitStateHalfOpen, "circuit breaker half-open")
		return true
	case circuitStateHalfOpen:
		return false
	default:
		return true
	}
}

func (c *circuitBreaker) RecordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.consecutiveFailures = 0
	c.lastFailureTime = time.Time{}
	c.halfOpenAt = time.Time{}
	c.transitionLocked(circuitStateClosed, "circuit breaker closed")
}

func (c *circuitBreaker) RecordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.consecutiveFailures++
	c.lastFailureTime = now

	switch c.state {
	case circuitStateHalfOpen:
		c.halfOpenAt = now
		c.transitionLocked(circuitStateOpen, "circuit breaker reopened")
	case circuitStateOpen:
		// Keep the circuit open without extending the cooldown window.
	default:
		if c.consecutiveFailures >= c.threshold {
			c.halfOpenAt = now
			c.transitionLocked(circuitStateOpen, "circuit breaker opened")
		}
	}
}

func (c *circuitBreaker) stateValue() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *circuitBreaker) failureCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutiveFailures
}

func (c *circuitBreaker) transitionLocked(nextState string, msg string) {
	if c.state == nextState {
		return
	}

	c.state = nextState
	c.logger.Info().
		Str("state", nextState).
		Int("consecutive_failures", c.consecutiveFailures).
		Dur("cooldown", c.cooldown).
		Msg(msg)
}
