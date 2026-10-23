package pool

import "testing"

func TestLeastActiveAndReadiness(t *testing.T) {
	p := New([]string{"http://127.0.0.1:1", "http://127.0.0.1:2"}, false)
	if u, _ := p.Pick(); u != nil {
		t.Fatal("picked unready worker")
	}
	p.Set(0, true, 10, "ready")
	p.Set(1, true, 11, "ready")
	u, a := p.Pick()
	v, b := p.Pick()
	if u == v {
		t.Fatal("did not spread equal load")
	}
	a()
	a() // releases are idempotent
	w, c := p.Pick()
	if w != u {
		t.Fatal("did not select least active")
	}
	b()
	c()
	for _, worker := range p.Snapshot() {
		if worker.Active != 0 {
			t.Fatal("active leak")
		}
	}
}

func TestHealthHysteresis(t *testing.T) {
	p := New([]string{"http://127.0.0.1:1"}, false)
	p.Set(0, false, 10, "starting")
	p.Health(0, true, 3, 2)
	if p.Ready() != 0 {
		t.Fatal("became ready before success threshold")
	}
	p.Health(0, true, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("did not become ready")
	}
	p.Health(0, false, 3, 2)
	p.Health(0, false, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("became unhealthy before failure threshold")
	}
	p.Health(0, true, 3, 2)
	p.Health(0, false, 3, 2)
	p.Health(0, false, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("success did not reset failure streak")
	}
	p.Health(0, false, 3, 2)
	if p.Ready() != 0 {
		t.Fatal("failed threshold did not remove worker")
	}
	p.Health(0, true, 3, 2)
	if p.Ready() != 0 {
		t.Fatal("recovered too early")
	}
	p.Health(0, true, 3, 2)
	if p.Ready() != 1 {
		t.Fatal("did not recover")
	}
}
