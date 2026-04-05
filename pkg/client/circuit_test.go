package client

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestCircuitBreaker_ClosedAfterInit(t *testing.T) {
	breaker := newCircuitBreaker(5, 30*time.Second, zerolog.Nop())

	if breaker.stateValue() != circuitStateClosed {
		t.Fatalf("state = %q, want %q", breaker.stateValue(), circuitStateClosed)
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	breaker := newCircuitBreaker(3, 30*time.Second, zerolog.Nop())

	breaker.RecordFailure()
	breaker.RecordFailure()
	if breaker.stateValue() != circuitStateClosed {
		t.Fatalf("state before threshold = %q, want %q", breaker.stateValue(), circuitStateClosed)
	}

	breaker.RecordFailure()
	if breaker.stateValue() != circuitStateOpen {
		t.Fatalf("state after threshold = %q, want %q", breaker.stateValue(), circuitStateOpen)
	}
}

func TestCircuitBreaker_RejectsWhenOpen(t *testing.T) {
	breaker := newCircuitBreaker(1, 30*time.Second, zerolog.Nop())
	breaker.RecordFailure()

	if breaker.Allow() {
		t.Fatal("Allow() = true, want false while open")
	}
}

func TestCircuitBreaker_HalfOpenAfterCooldown(t *testing.T) {
	breaker := newCircuitBreaker(1, 20*time.Millisecond, zerolog.Nop())
	breaker.RecordFailure()

	time.Sleep(30 * time.Millisecond)

	if !breaker.Allow() {
		t.Fatal("Allow() = false, want true after cooldown")
	}
	if breaker.stateValue() != circuitStateHalfOpen {
		t.Fatalf("state = %q, want %q", breaker.stateValue(), circuitStateHalfOpen)
	}
	if breaker.Allow() {
		t.Fatal("Allow() = true, want false for additional half-open requests")
	}
}

func TestCircuitBreaker_ResetsOnSuccess(t *testing.T) {
	breaker := newCircuitBreaker(2, 20*time.Millisecond, zerolog.Nop())
	breaker.RecordFailure()
	breaker.RecordFailure()

	time.Sleep(30 * time.Millisecond)

	if !breaker.Allow() {
		t.Fatal("Allow() = false, want true after cooldown")
	}

	breaker.RecordSuccess()

	if breaker.stateValue() != circuitStateClosed {
		t.Fatalf("state = %q, want %q", breaker.stateValue(), circuitStateClosed)
	}
	if breaker.failureCount() != 0 {
		t.Fatalf("failureCount = %d, want 0", breaker.failureCount())
	}
}
