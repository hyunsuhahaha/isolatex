package pool

import (
	"net/url"
	"sync"
	"time"
)

type Worker struct {
	URL                  *url.URL
	Ready                bool
	Active               int
	PID                  int
	State                string
	Restarts             int
	HealthChecks         uint64
	HealthFailures       uint64
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	LastHealthCheck      time.Time
	generation           uint64
}

type Pool struct {
	mu      sync.Mutex
	workers []Worker
	next    int
}

func New(origins []string, ready bool) *Pool {
	p := &Pool{}
	for _, origin := range origins {
		u, _ := url.Parse(origin)
		state := "stopped"
		if ready {
			state = "ready"
		}
		p.workers = append(p.workers, Worker{URL: u, Ready: ready, State: state})
	}
	return p
}

func (p *Pool) Set(i int, ready bool, pid int, state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := &p.workers[i]
	w.Ready, w.PID, w.State = ready, pid, state
	if state == "starting" {
		// In-flight requests to an exited process may finish after its
		// replacement becomes ready on the same port. Keep load accounting
		// local to a process generation, rather than to the reusable slot.
		w.generation++
		w.Active = 0
		w.ConsecutiveFailures = 0
		w.ConsecutiveSuccesses = 0
	}
}

// Health changes routing eligibility without killing the process or disturbing
// requests already in flight. Recovery requires consecutive successful checks.
func (p *Pool) Health(i int, ok bool, failures, successes int) (before, after bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := &p.workers[i]
	before = w.Ready
	w.HealthChecks++
	w.LastHealthCheck = time.Now()
	if ok {
		w.ConsecutiveFailures = 0
		w.ConsecutiveSuccesses++
		if w.ConsecutiveSuccesses >= successes {
			w.Ready = true
			w.State = "ready"
		}
	} else {
		w.HealthFailures++
		w.ConsecutiveSuccesses = 0
		w.ConsecutiveFailures++
		if w.ConsecutiveFailures >= failures {
			w.Ready = false
			if w.State != "starting" {
				w.State = "unhealthy"
			}
		}
	}
	return before, w.Ready
}

func (p *Pool) Restart(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workers[i].Restarts++
}

func (p *Pool) Ready() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, w := range p.workers {
		if w.Ready {
			n++
		}
	}
	return n
}

func (p *Pool) Pick() (*url.URL, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	best := -1
	for j := 0; j < len(p.workers); j++ {
		i := (p.next + j) % len(p.workers)
		w := p.workers[i]
		if w.Ready && (best == -1 || w.Active < p.workers[best].Active) {
			best = i
		}
	}
	if best == -1 {
		return nil, nil
	}
	p.workers[best].Active++
	generation := p.workers[best].generation
	p.next = (best + 1) % len(p.workers)
	var once sync.Once
	return p.workers[best].URL, func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.workers[best].generation == generation {
				p.workers[best].Active--
			}
		})
	}
}

func (p *Pool) Snapshot() []Worker {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Worker(nil), p.workers...)
}
