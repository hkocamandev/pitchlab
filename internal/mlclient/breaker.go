package mlclient

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen reports that the breaker refused the call without attempting it.
var ErrCircuitOpen = errors.New("ml circuit breaker is open")

// State is the breaker's position.
type State int

const (
	// StateClosed passes calls through. Normal operation.
	StateClosed State = iota
	// StateOpen rejects calls immediately, without attempting them.
	StateOpen
	// StateHalfOpen lets a single probe through to test recovery.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// BreakerConfig tunes the breaker.
type BreakerConfig struct {
	// FailureThreshold is how many consecutive failures trip the breaker.
	FailureThreshold int
	// OpenDuration is how long it stays open before probing.
	OpenDuration time.Duration
	// HalfOpenSuccesses is how many probes must succeed before closing.
	HalfOpenSuccesses int

	now func() time.Time // injectable for tests
}

// DefaultBreakerConfig returns sensible defaults.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		FailureThreshold:  5,
		OpenDuration:      30 * time.Second,
		HalfOpenSuccesses: 2,
	}
}

// Breaker stops a failed dependency from turning into a queue of retries.
//
// Without one, an inference service that is down costs every message its full
// retry budget: with six partitions, five attempts and exponential backoff,
// the consumer spends its time waiting rather than draining. The breaker
// converts that into immediate, cheap failure, and the pipeline keeps moving
// with predictions marked as failed rather than stalling.
//
// The point is not to hide the outage. It is to keep the outage from spreading
// into the parts of the system that still work.
type Breaker struct {
	cfg BreakerConfig

	mu                sync.Mutex
	state             State
	consecutiveFails  int
	halfOpenSuccesses int
	openedAt          time.Time

	// onStateChange is called outside the lock so a logger cannot deadlock it.
	onStateChange func(from, to State)
}

// NewBreaker builds a breaker.
func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 30 * time.Second
	}
	if cfg.HalfOpenSuccesses <= 0 {
		cfg.HalfOpenSuccesses = 2
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Breaker{cfg: cfg, state: StateClosed}
}

// OnStateChange registers a callback for transitions.
func (b *Breaker) OnStateChange(fn func(from, to State)) { b.onStateChange = fn }

// State returns the current state, advancing out of Open if the cooldown has
// elapsed. Reading the state is what triggers the probe, so a breaker that is
// never called never spins.
func (b *Breaker) State() State {
	b.mu.Lock()
	// `from, to := b.state, b.advanceLocked()` looks equivalent and is not:
	// Go does not guarantee the plain read happens before the call, so `from`
	// could observe the already-advanced state and the transition would go
	// unreported. Sequencing it explicitly is the fix.
	from := b.state
	to := b.advanceLocked()
	b.mu.Unlock()
	b.notify(from, to)
	return to
}

func (b *Breaker) advanceLocked() State {
	if b.state == StateOpen && b.cfg.now().Sub(b.openedAt) >= b.cfg.OpenDuration {
		b.state = StateHalfOpen
		b.halfOpenSuccesses = 0
	}
	return b.state
}

// Allow reports whether a call may proceed.
func (b *Breaker) Allow() bool {
	return b.State() != StateOpen
}

// Do runs fn if the breaker allows it, recording the outcome.
func (b *Breaker) Do(fn func() error) error {
	if !b.Allow() {
		return ErrCircuitOpen
	}
	err := fn()
	if err != nil {
		b.RecordFailure()
		return err
	}
	b.RecordSuccess()
	return nil
}

// RecordSuccess reports a successful call.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	from := b.state
	b.consecutiveFails = 0

	if b.state == StateHalfOpen {
		b.halfOpenSuccesses++
		if b.halfOpenSuccesses >= b.cfg.HalfOpenSuccesses {
			b.state = StateClosed
			b.halfOpenSuccesses = 0
		}
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
}

// RecordFailure reports a failed call.
//
// A failure while half-open reopens immediately: the probe was the test, and
// it failed, so there is nothing to be gained by sending more traffic.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	from := b.state

	switch b.state {
	case StateHalfOpen:
		b.state = StateOpen
		b.openedAt = b.cfg.now()
		b.halfOpenSuccesses = 0
	case StateClosed:
		b.consecutiveFails++
		if b.consecutiveFails >= b.cfg.FailureThreshold {
			b.state = StateOpen
			b.openedAt = b.cfg.now()
		}
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
}

func (b *Breaker) notify(from, to State) {
	if from != to && b.onStateChange != nil {
		b.onStateChange(from, to)
	}
}
