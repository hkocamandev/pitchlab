package mlclient

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestBreaker(clock *fakeClock) *Breaker {
	return NewBreaker(BreakerConfig{
		FailureThreshold:  3,
		OpenDuration:      30 * time.Second,
		HalfOpenSuccesses: 2,
		now:               clock.Now,
	})
}

var errBoom = errors.New("boom")

func TestBreakerStartsClosed(t *testing.T) {
	b := newTestBreaker(&fakeClock{now: time.Now()})
	if b.State() != StateClosed || !b.Allow() {
		t.Fatal("a fresh breaker must pass calls through")
	}
}

func TestBreakerTripsOnConsecutiveFailures(t *testing.T) {
	b := newTestBreaker(&fakeClock{now: time.Now()})

	for i := 0; i < 2; i++ {
		b.RecordFailure()
		if b.State() != StateClosed {
			t.Fatalf("tripped after %d failures; the threshold is 3", i+1)
		}
	}

	b.RecordFailure()
	if b.State() != StateOpen {
		t.Fatal("expected the breaker to trip on the third failure")
	}
	if b.Allow() {
		t.Fatal("an open breaker must refuse calls")
	}
}

func TestSuccessResetsTheFailureRun(t *testing.T) {
	// The threshold counts *consecutive* failures. A service that fails
	// intermittently is degraded, not down, and tripping on scattered
	// failures would take away capacity that still works.
	b := newTestBreaker(&fakeClock{now: time.Now()})

	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()

	if b.State() != StateClosed {
		t.Fatal("an intervening success should have reset the run")
	}
}

func TestOpenBreakerRejectsWithoutCalling(t *testing.T) {
	// The entire point: when the dependency is down, fail immediately rather
	// than spending the retry budget discovering it again per message.
	b := newTestBreaker(&fakeClock{now: time.Now()})
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}

	called := false
	err := b.Do(func() error {
		called = true
		return nil
	})

	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v, want ErrCircuitOpen", err)
	}
	if called {
		t.Fatal("an open breaker must not invoke the call")
	}
}

func TestBreakerProbesAfterTheCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	b := newTestBreaker(clock)
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}

	clock.Advance(29 * time.Second)
	if b.State() != StateOpen {
		t.Fatal("the breaker reopened early")
	}

	clock.Advance(2 * time.Second)
	if b.State() != StateHalfOpen {
		t.Fatal("expected a probe after the cooldown")
	}
	if !b.Allow() {
		t.Fatal("half-open must let a probe through")
	}
}

func TestRecoveryRequiresRepeatedSuccess(t *testing.T) {
	// One success could be luck. Closing on it would send full traffic back
	// at a service that has not actually recovered.
	clock := &fakeClock{now: time.Now()}
	b := newTestBreaker(clock)
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	clock.Advance(31 * time.Second)

	if b.State() != StateHalfOpen {
		t.Fatal("expected half-open")
	}
	b.RecordSuccess()
	if b.State() != StateHalfOpen {
		t.Fatal("one success should not be enough to close")
	}
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Fatal("two successes should close the breaker")
	}
}

func TestFailureWhileProbingReopensImmediately(t *testing.T) {
	// The probe was the test. It failed, so there is nothing to learn from
	// sending more traffic.
	clock := &fakeClock{now: time.Now()}
	b := newTestBreaker(clock)
	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	clock.Advance(31 * time.Second)
	if b.State() != StateHalfOpen {
		t.Fatal("expected half-open")
	}

	b.RecordFailure()
	if b.State() != StateOpen {
		t.Fatal("a failed probe must reopen the breaker")
	}

	clock.Advance(29 * time.Second)
	if b.State() != StateOpen {
		t.Fatal("the cooldown should restart from the reopen")
	}
}

func TestBreakerReportsTransitions(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	b := newTestBreaker(clock)

	var transitions []string
	b.OnStateChange(func(from, to State) {
		transitions = append(transitions, from.String()+"->"+to.String())
	})

	for i := 0; i < 3; i++ {
		b.RecordFailure()
	}
	clock.Advance(31 * time.Second)
	b.State()
	b.RecordSuccess()
	b.RecordSuccess()

	want := []string{"closed->open", "open->half_open", "half_open->closed"}
	if len(transitions) != len(want) {
		t.Fatalf("got %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transition %d: got %q, want %q", i, transitions[i], want[i])
		}
	}
}

func TestDoRecordsOutcomes(t *testing.T) {
	b := newTestBreaker(&fakeClock{now: time.Now()})

	for i := 0; i < 3; i++ {
		if err := b.Do(func() error { return errBoom }); !errors.Is(err, errBoom) {
			t.Fatalf("the underlying error should surface, got %v", err)
		}
	}
	if b.State() != StateOpen {
		t.Fatal("Do should have recorded the failures")
	}
}

func TestBreakerIsSafeUnderConcurrency(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	b := newTestBreaker(clock)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				b.RecordFailure()
			} else {
				b.RecordSuccess()
			}
			b.State()
			b.Allow()
		}(i)
	}
	wg.Wait()
}
