package gateway

import (
	"encoding/json"
	"net/http"
)

// RoutingStatus is a local observation, never a cross-server capacity lease.
// It deliberately omits worker PIDs, commands and upstream addresses.
type RoutingStatus struct {
	Group           string `json:"group"`
	Eligible        bool   `json:"eligible"`
	Reason          string `json:"reason"`
	ReadyWorkers    int    `json:"ready_workers"`
	Active          int    `json:"active"`
	Queued          int    `json:"queued"`
	FreeConcurrency int    `json:"free_concurrency"`
	FreeQueue       int    `json:"free_queue"`
}

func (g *Gateway) routingStatus(b *group) RoutingStatus {
	a, q := b.gate.snapshot()
	s := RoutingStatus{Group: b.name, ReadyWorkers: b.pool.Ready(), Active: a, Queued: q,
		FreeConcurrency: max(0, b.cfg.MaxConcurrent-a), FreeQueue: max(0, b.cfg.QueueSize-q)}
	switch {
	case g.draining.Load():
		s.Reason = "draining"
	case s.ReadyWorkers == 0:
		s.Reason = "no_ready_worker"
	case b.breaker.Blocked():
		s.Reason = "circuit_open"
	case s.FreeConcurrency == 0 && s.FreeQueue == 0:
		s.Reason = "saturated"
	default:
		s.Eligible, s.Reason = true, "available"
	}
	return s
}

func (g *Gateway) serveRoutingProbe(w http.ResponseWriter, r *http.Request, name string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	for _, b := range g.groups {
		if b.name != name {
			continue
		}
		status := g.routingStatus(b)
		w.Header().Set("Content-Type", "application/json")
		code := http.StatusOK
		if !status.Eligible {
			code = http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(status)
		}
		return
	}
	fail(w, http.StatusNotFound, "unknown_group")
}
