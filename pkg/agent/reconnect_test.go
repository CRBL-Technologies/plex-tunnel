package agent

import (
	"math/rand"
	"testing"
	"time"
)

func TestBackoffDelayIncreasesAndCaps(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	maxDelay := 10 * time.Second

	d0 := backoffDelayWithRand(0, maxDelay, rng)
	d4 := backoffDelayWithRand(4, maxDelay, rng)
	d20 := backoffDelayWithRand(20, maxDelay, rng)

	if d4 <= d0 {
		t.Fatalf("expected d4 > d0, got d4=%v d0=%v", d4, d0)
	}

	if d20 < maxDelay || d20 > maxDelay+maxReconnectJitter {
		t.Fatalf("expected capped delay with jitter in [%v,%v], got %v", maxDelay, maxDelay+maxReconnectJitter, d20)
	}
}
