// Package breaker provides a bounded rolling-window circuit breaker.
package breaker

import (
	"sync"
	"time"

	"isolatex/internal/config"
)

type Result int

const (
	Ignored Result = iota // no upstream attempt, local rejection, or client cancellation
	Success
	Failure
)

type bucket struct {
	epoch         int64
	total, failed uint64
}

// One hundred time buckets bound memory regardless of request rate. Window
// boundaries have at most one bucket (1% of Window) of quantization error.
type Breaker struct {
	mu          sync.Mutex
	cfg         *config.CircuitBreaker
	now         func() time.Time
	state       string
	generation  uint64
	openUntil   time.Time
	probeBusy   bool
	buckets     [100]bucket
	transitions uint64
}

type Snapshot struct {
	Enabled       bool      `json:"enabled"`
	State         string    `json:"state"`
	Requests      uint64    `json:"window_requests"`
	Failures      uint64    `json:"window_failures"`
	ProbeInFlight bool      `json:"probe_in_flight"`
	OpenUntil     time.Time `json:"open_until,omitempty"`
	Transitions   uint64    `json:"transitions"`
}

func New(c *config.CircuitBreaker) *Breaker { return &Breaker{cfg: c, now: time.Now, state: "closed"} }

// Allow returns a completion for this admission generation. The caller must
// finish even when local admission fails; an abandoned Half-open probe must not
// block recovery. Finish is idempotent, and stale in-flight results are ignored.
func (b *Breaker) Allow() (finish func(Result), ok bool) {
	if b.cfg == nil {
		return func(Result) {}, true
	}
	b.mu.Lock()
	now := b.now()
	if b.state == "open" && !now.Before(b.openUntil) {
		b.transition("half_open")
		b.probeBusy = false
	}
	if b.state == "open" || (b.state == "half_open" && b.probeBusy) {
		b.mu.Unlock()
		return nil, false
	}
	if b.state == "half_open" {
		b.probeBusy = true
	}
	generation := b.generation
	b.mu.Unlock()
	var once sync.Once
	return func(result Result) { once.Do(func() { b.complete(generation, result) }) }, true
}

func (b *Breaker) complete(generation uint64, result Result) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if generation != b.generation {
		return
	}
	if b.state == "half_open" {
		b.probeBusy = false
		switch result {
		case Success:
			b.transition("closed")
			b.buckets = [100]bucket{}
			b.openUntil = time.Time{}
		case Failure:
			b.open(b.now())
		}
		return
	}
	if result == Ignored {
		return
	}
	now := b.now()
	epoch := now.UnixNano() / int64(b.cfg.Window/100)
	slot := &b.buckets[epoch%100]
	if slot.epoch != epoch {
		*slot = bucket{epoch: epoch}
	}
	slot.total++
	if result == Failure {
		slot.failed++
	}
	total, failed := b.counts(now)
	if total >= uint64(b.cfg.MinRequests) && float64(failed)/float64(total) >= b.cfg.FailureRatio {
		b.open(now)
	}
}

func (b *Breaker) counts(now time.Time) (total, failed uint64) {
	epoch := now.UnixNano() / int64(b.cfg.Window/100)
	for _, slot := range b.buckets {
		if slot.epoch > epoch-100 && slot.epoch <= epoch {
			total += slot.total
			failed += slot.failed
		}
	}
	return
}
func (b *Breaker) transition(state string) { b.state = state; b.generation++; b.transitions++ }
func (b *Breaker) open(now time.Time) {
	b.transition("open")
	b.openUntil = now.Add(b.cfg.OpenTimeout)
	b.probeBusy = false
}

// Blocked is a fast pre-queue check. Allow is still required at dispatch: the
// circuit can open while an admitted request waits for a concurrency slot.
func (b *Breaker) Blocked() bool {
	if b.cfg == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return (b.state == "open" && b.now().Before(b.openUntil)) || (b.state == "half_open" && b.probeBusy)
}

func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{Enabled: b.cfg != nil, State: b.state, ProbeInFlight: b.probeBusy, OpenUntil: b.openUntil, Transitions: b.transitions}
	if b.cfg != nil {
		s.Requests, s.Failures = b.counts(b.now())
	}
	return s
}
